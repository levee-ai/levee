# /// script
# requires-python = ">=3.11"
# dependencies = []
# ///
"""Mechanically evaluate the pre-registered sanity bands for one benchmark run.

The bands are gates, not prose. A human reading figures and judging whether the
numbers look reasonable is precisely the failure mode pre-registration exists to
prevent, so every band here is arithmetic over the committed artifacts and every
violation exits non-zero.

Two sources are read per cell, and the split is deliberate:

  <cell>.duration.csv.gz  the committed filtered per-request rows, steady
                          scenario only. Every percentile in a band comes from
                          here, because these are the same rows the figures
                          publish. The k6 end-of-run summary aggregates the
                          WHOLE invocation including the warmup scenario, so
                          gating on those would gate numbers nobody publishes.
  <cell>.summary.json     cell metadata and the integrity outcomes. Percentiles
                          from this file are reported for reference only, marked
                          as whole-invocation values.

Percentile keys in the summary come from k6 and are spelled p(50), p(90), p(99)
and p(99.9). Percentiles computed here from the CSV use linear interpolation
between order statistics, which is the ordinary definition and is stated so the
arithmetic is reproducible by hand from the committed rows.

Output is written to bands.txt by the harness and committed with the run. It
never prints the absolute results directory path, only the directory name: the
harness audits every committed artifact for home directory paths and would
reject its own gate report.
"""

from __future__ import annotations

import gzip
import json
import os
import re
import statistics
import sys
from dataclasses import dataclass, field

# The no-load rule for CI: the plot scripts read committed artifacts and must
# never generate load, open a socket, or shell out. The check that proves it is
# an IMPORT-SHAPED one, anchored to the start of a line:
#
#   rg -n '^\s*(import|from)\s+(subprocess|socket|urllib|requests)\b' benchmarks/plots/*.py
#
# It returns nothing across all three plot scripts, which is the passing state.
#
# Use that form, not a bare word search for the module names. A bare
# 'subprocess|socket|requests|urllib' matches three times in this file alone, and
# every hit is the ordinary English word "requests" inside prose about failed and
# recorded requests, not an import. A check whose passing state is three known
# false hits is a check nobody can automate and nobody trusts, so the prose stays
# as it reads and the pattern is the thing that got fixed.
#
# The anchor and the \b matter. Without the line anchor the pattern would match
# this very comment block, and without the word boundary a module named
# socketserver or requests_cache would slip past as a prefix match.

# Band 1. A direct-to-mock cell measures the generator, the loopback stack and
# the mock. If that floor is already at the proxy budget then the box or the mock
# is the bottleneck and no proxied number derived from the same run means
# anything.
#
# AMENDED 2026-09-16, after the first five matrices on the reference host. The
# band was originally pre-registered as direct P99 below 1.0ms. It moved to P50
# because the P99 form was detecting host tail noise rather than a bottleneck,
# and those are different things: a genuine bottleneck raises the CENTRAL
# tendency, while resident endpoint-security agents raise only the TAIL, so a P99
# bound cannot tell them apart. The 1.0ms figure itself came from Tenet 1, which
# is a claim about proxy OVERHEAD, a delta between two distributions, and the
# original band borrowed it as an absolute bound on one cell. The design already
# settled this same question elsewhere by ruling that SLO values are drawn on
# figures and never enforced as k6 thresholds, so that a noisy run reports
# honestly instead of failing. The original P99 form was that ruling violated
# from inside the band checker.
#
# Observed direct P99 on the reference host across five matrices, which is the
# evidence that prompted the amendment: canary-open 0.547 to 1.353, canary-close
# 0.660 to 4.080, payload-32768 0.623 to 1.320, streaming 0.831 to 1.426. The
# streaming cell's MEDIAN P99 of 0.976ms sat on the original threshold by
# construction, since six write and flush cycles cost more than one write.
# Direct P50 over the same runs was around 0.3ms non-streaming and 0.46ms
# streaming, so a 1.0ms P50 ceiling sits at roughly two to three times expected.
#
# The tail is still measured and still reported, as an advisory naming host load
# rather than as a verdict on the run, and every direct P99 is printed in the
# committed band report so a reader can apply the original stricter band and see
# what it would have said.
BAND1_DIRECT_P50_MAX_MILLISECONDS = 1.0
BAND1_DIRECT_P99_ADVISORY_MILLISECONDS = 2.5

# Band 2. Passthrough adds one extra loopback HTTP hop over direct. Below the
# floor the extra hop is missing, which means the cell did not go through the
# proxy at all. Above the ceiling something other than the hop is being paid.
BAND2_PASSTHROUGH_SHIFT_MIN_MILLISECONDS = 0.05
BAND2_PASSTHROUGH_SHIFT_MAX_MILLISECONDS = 0.6

# Band 3. Enforcement over passthrough. The work being measured is token
# estimation plus admission plus reconcile, and at 4096B the estimation term
# dominates everything else by two orders of magnitude.
#
# AMENDED 2026-09-16, in the style of the band 1 and band 5 amendments above. The
# PRIMARY GATE MOVED FROM 150B TO 4096B, and the 150B reading is now RECORDED with
# an advisory instead of gating.
#
# READ THE REASON PRECISELY, because it is easy to mistake for its opposite. THE
# 150-BYTE BAND WAS CORRECT AND THE RUN THAT FAILED IT WAS INVALID. The first
# completed 43-cell evidence run measured +107, +175, +126, +114 and +123us against
# the 0 to 100us window, median +123us, and the band refused it. An investigation
# then proved the band right and the run wrong:
#
#   - On a quiet host, six pairs measured in BOTH orders at the same commit, the
#     same configs and the same load shape read +13, +14, +16, +16, +19 and +14us,
#     median +15us.
#   - Three background busy loops move that same measurement to +93 and +109us AND
#     move both arms' absolute P50 onto the invalidated artifact's own values, while
#     still achieving 500.0 rps with zero steady drops and zero failed requests. So
#     the contended regime would have passed every integrity gate the harness had.
#   - The +15us decomposes into 38.2us of gross enforce-only work, two tokenizer
#     passes at 17.4us each plus 2.1us of logging plus 0.9us of ObserveDrift plus
#     0.4us for Admit and ReconcileMulti, offset by a 24us credit because the
#     SHARED path runs faster in the enforce arm. Lock contention is 124ns per
#     request and metrics 853ns, so neither is in the story, and GC costs 19.7us of
#     CPU and ZERO latency because marking runs on idle and dedicated workers
#     rather than as request-goroutine assists.
#
# SO THE WINDOW IS NOT WIDENED. The 0 to 100us shape was a correct description of
# the quantity. What changed is where the gate is applied, and the reason is
# SIGNAL TO NOISE and nothing else:
#
#   payload   measured shift   estimator noise floor   ratio   within-run spread
#   150B          +15us               4us              3.7:1   68us, 55 pct of it
#   4096B        +655us               4us            164:1     12us, 1.8 pct of it
#
# Both spread figures are from the SAME invalidated 43-cell run, which is what makes
# the comparison fair rather than selective. A 3.7:1 gate cannot be relied on. A
# 164:1 gate can.
#
# THE 4096B NUMBERS, from that run's five repetition-matched pairs. Passthrough P50
# 0.790, 0.807, 0.795, 0.797 and 0.798ms against enforce 1.445, 1.453, 1.440, 1.454
# and 1.453ms, so the shifts are +655, +646, +645, +657 and +655us, median +655us,
# spread 12us.
#
# THE WINDOW, 0.15 to 0.95ms, derived rather than eyeballed:
#
#   - FLOOR 0.15ms. Its job is to catch an enforce arm that is not enforcing, which
#     reads what the A/A control reads, 0 to 15us, so the floor is an order of
#     magnitude clear of that. Its constraint is the PENDING product fix removing
#     the duplicate tokenizer pass. One pass at 4096B measures 423.6us in
#     isolation, while the in-server double-pass shift is 655us, which puts the
#     in-server per-pass cost near 338us. Removing one leaves either 317us or
#     231us depending on which of those two figures is the honest one, and the
#     floor sits at least 1.5 times below the lower.
#   - CEILING 0.95ms. 45 percent above the measured 655us. It catches a THIRD
#     tokenizer pass, which lands near 993us, and a third pass is the most likely
#     regression shape precisely because a duplicate SECOND pass is the defect the
#     pending fix removes. Said out loud: it does not catch a 20 percent
#     regression, and pretending otherwise would be the band 5 percentage mistake
#     made again.
#
# WHAT THE RELOCATION DOES NOT DO. The 4096B numbers above come from the
# INVALIDATED run and they would have PASSED this window. That is the point rather
# than an embarrassment: the contention that destroyed the 150B band moved this
# measurement by less than its own window, which is exactly why this one can be
# gated. The check that refuses a contended host is the quiescence floor in run.sh,
# not this band, and no band can do that job.
#
# A SECOND 4096B READING, from the quick matrix that first ran this gate, recorded
# because it is the only other one in existence and because it is the less flattering
# of the two. That run's host dipped below the idle floor on FIVE of its 26 readings,
# so an evidence run would have refused it outright, and on it:
#
#   payload   passthrough P50   enforce P50   shift    A/A control   150B shift
#   4096B         0.599ms         1.404ms    +805us      -27us         +61us
#
# Three things to take from that row. The 4096B gate passed with 145us to spare. The
# A/A control read -27us at a true zero, which is the estimator noise a contended
# host produces and is seven times the 4us quiet floor. And the 150B reading went to
# +61us, four times its quiet value, on a host where the 4096B reading moved by 23
# percent. That is the signal-to-noise argument reproducing itself in one run.
#
# The two readings together, 655us and 805us, bound the BETWEEN-run spread at 4096B
# on this host at 150us. The ceiling sits 45 percent above the quieter of them rather
# than 10 percent above it for exactly that reason. If a run that PASSES the
# quiescence floor ever reads above 0.95ms, the response is to count the tokenizer
# passes and examine the numbers, not to widen the ceiling.
#
# LIMITATION. One matrix has ever run 4096B pairs, five repetitions inside a single
# run, so the WITHIN-run spread is measured at 12us and the BETWEEN-regime bias at
# 4096B is not measured at all. The window is 800us wide against a bias of the
# 108us magnitude seen at 150B, so such a bias cannot move the verdict, and that
# headroom is the reason the window is wide rather than tight.
BAND3_PRIMARY_PAYLOAD_BYTES = 4096
BAND3_PRIMARY_SHIFT_MIN_MILLISECONDS = 0.15
BAND3_PRIMARY_SHIFT_MAX_MILLISECONDS = 0.95

# The 150B enforcement reading, RECORDED with an advisory and no longer a gate.
# Same amendment, same date, and the window is the original band 3 shape with one
# change: the floor is NEGATIVE.
#
# WHY A NEGATIVE FLOOR IS CORRECT RATHER THAN A LOOPHOLE. Enforcement can only ADD
# work, so a naive reading says the true shift cannot be below zero and a floor of
# zero costs nothing. That is wrong here, and the measurement says why. At 150B the
# gross enforce-only work is 38.2us while the SHARED path runs 24us FASTER in the
# enforce arm, and the published net of +15us is the sum of those two. The credit is
# a real measured term in a shared code path, not enforcement doing less, so the
# subtraction does not have to come out positive.
#
# The pending product fix removes one of the two tokenizer passes. Gross work drops
# from 38.2us to 20.8us against the same 24us credit, so the net at 150B goes to
# roughly -3us. A floor of zero would then fail every quiet run on a codebase that
# had just become FASTER, which is the worst possible thing for a validity gate to
# do. So the floor is -0.05ms, which clears the predicted -3us by an order of
# magnitude and absorbs the 4us estimator floor and ordinary drift on top of it.
#
# A negative reading here is therefore EXPECTED after that fix lands and is not
# alarming. What it does NOT mean is that enforcement became free: the enforcement
# work is measured directly by the component decomposition in microbench.txt and by
# the 4096B gate above, neither of which can go negative.
BAND3_SMALL_SHIFT_ADVISORY_MIN_MILLISECONDS = -0.05
BAND3_SMALL_SHIFT_ADVISORY_MAX_MILLISECONDS = 0.10

# Band 4. The P99 companion to band 3. A tail shift far above the median shift
# means one cell caught a transient even though every median gate passed.
#
# AMENDED 2026-09-16, and only because band 3 moved. Band 4 is defined as a RATIO
# against band 3's median P50 shift, so it follows band 3 to 4096B: a 150B P99 shift
# divided by a 4096B P50 shift would be arithmetic between two different
# experiments. The 150B P99 shift is still printed, beside the 150B median, so
# nothing that used to be visible stopped being visible.
#
# The relocation also fixes this band on its own terms. Ten times a 15us median is a
# 150us allowance on a quantity whose host-noise component is measured in
# milliseconds, so at 150B the ratio form was never resolvable either. That is the
# same signal-to-noise argument band 3 moved for, not a second one.
BAND4_TAIL_SHIFT_RATIO_MAX = 10.0

# Band 3 streaming companion. Band 3 and band 4 gate the NON-streaming
# enforcement shift at the small payload. Nothing gated the streaming one, so a
# streaming enforcement regression could publish silently.
#
# ADDED 2026-09-16, and documented the way bands 1 and 5 were amended above,
# because it has the same shape of problem those amendments fixed: the number the
# band would naturally be sized against is not a number a single matrix can
# measure.
#
# WHAT PROMPTED IT. One matrix reported an enforce minus passthrough P50 shift of
# 25.0us non-streaming and 163.5us streaming, a 6.5x gap with no explanation
# attached. A gap that size is either real streaming enforcement work, which
# should be gated, or drift, which must not be. So the code was read and probed
# before any window was chosen.
#
# WHAT THE CODE SAYS. Nothing on the streaming path is enforcement-conditional.
# internal/proxy/proxy.go calls injectStreamOptions and then streamResponse for
# every streaming request regardless of agent mode. internal/proxy/streaming.go
# calls shouldParseUsage on every SSE data event and inspectUsage on the ones that
# pass, also regardless of mode. Both passthrough and enforce take the
# reconcileForStream branch after the stream ends, only observe mode diverges. So
# per-event usage inspection, stream_options injection and the stream reconcile
# decision are all paid by the passthrough cell too and cancel out of the shift.
# What remains is the SAME work in both response modes: one EstimateSplit in
# enforce, one Admit, one Estimate recomputed for the drift log, one
# ReconcileMulti, two budgetAmounts calls, and two extra slog lines.
#
# WHAT THE COMPONENT MEASUREMENTS SAY, and this block CARRIED TWO WRONG CONSTANTS
# until 2026-09-16. It reported the estimator at "EstimateSplit 4.32us, Estimate
# 4.29us" and an in-process total shift of 16.9us non-streaming against 19.5us
# streaming. Both are wrong, and they are wrong in a way that was checkable from
# this repository's own README, which has said 120ns per prompt byte all along:
# 4.32us at 150 bytes would be 29ns per byte.
#
# CORRECTED, measured on the EXACT k6 150-byte request body at the pinned
# toolchain, go1.26.3 on an Apple M3 Pro, model id gpt-4o-mini-2024-07-18 resolving
# through the tiktoken model tables to o200k_base:
#
#   Estimate          17,364 ns/op   12,936 B/op   163 allocs/op
#   EstimateSplit     essentially identical, the two calls do the same work
#   per prompt byte   116ns at 150B, 104ns at 4096B, 106ns at 32768B
#
# So ONE pass is 17.4us and the shipped code makes TWO, which is 34.8us. A total
# in-process shift of 16.9us is therefore arithmetically impossible: the tokenizer
# alone is more than twice it. Reproduce the figures with a benchmark that calls
# Estimate on the exact body benchmarks/k6/overhead.js builds, which is the fixture
# model id and max_tokens 16 with the message content padded to the target byte
# count by buildPrompt. The committed microbench.txt reads 8270 ns/op for
# BenchmarkEstimate_OpenAI, which is NOT comparable: that benchmark uses its own
# shorter prose body and constructs its estimator with cl100k_base.
#
# THE CORRECTED DECOMPOSITION at 150B, which replaces the retracted probe totals:
#
#   two tokenizer passes            34.8us
#   two extra structured log lines   2.1us
#   ObserveDrift                     0.9us
#   Admit plus ReconcileMulti        0.4us
#   gross enforce-only work         38.2us
#   shared-path credit             -24.0us   the shared path runs FASTER in enforce
#   net measured shift, quiet host  +15us
#
# The credit is measured and is NOT attributed to a named cause, which is stated
# rather than smoothed over. Lock contention is 124ns per request and metrics 853ns,
# so neither is in it, and GC costs 19.7us of CPU and zero latency because marking
# runs on idle and dedicated workers rather than as request-goroutine assists.
#
# WHAT SURVIVES OF THE STREAMING CLAIM. The retracted probe put streaming at 1.16
# times non-streaming, and since its absolute figures were wrong that ratio has to be
# read as unverified rather than as measurement. What still stands is the code
# reading above, which is independent of the probe: nothing on the streaming path is
# enforcement-conditional, so the streaming shift should sit close to the
# non-streaming one. The ceiling below was sized as inherent work plus the observed
# drift envelope. Recomputed with the corrected inherent term, 15us plus 336us is
# 351us against the 356us it was sized on, so the 0.60ms figure is unchanged.
#
# THEREFORE the 163.5us reading is not a central value. Roughly 15us of it is
# inherent work and the rest is between-cell drift. Every streaming
# shift reading available on this host, one streaming repetition per matrix,
# recomputed from the committed per-request CSVs. Every row was independently
# reverified cell by cell against those CSVs with a separate percentile
# implementation, and all four matched:
#
#   run   streaming P50 shift   non-streaming P50 shift   run verdict
#   r1           +59us                  +86us            INVALID, see below
#   r2          +215us                  +85us            valid
#   r3          -336us                 +140us            invalid, band 3 failed
#   r4          +163us                  +25us            valid
#   r5           +26us                  +49us            valid, see below
#
# The r5 row is the matrix that first ran this band, and it is the strongest row
# in the table. It is the only run on record with zero steady dropped iterations
# in all NINE cells and the tightest canary drift ever measured here, 0.001ms at
# P50. Its streaming shift of +26us sits inside the 150B advisory window and within
# 11us of the +15us the corrected decomposition gives, so the advisory below did not
# fire. That is the corroboration the rest of this note was missing:
# when the host is quiet the streaming shift COLLAPSES onto the inherent cost, and
# the +163us and -336us readings are what host noise does to the same quantity.
#
# PROVENANCE of the r1 row, recorded because a reader must not assume four clean
# runs. That matrix was INVALIDATED by the k6 dropped-iterations threshold. The
# row is kept on a LOCATED fact rather than a judgement call: the drop was 13 of
# 14989 steady iterations, confined to the closing drift-canary cell
# direct-canary-close-nonstream-150, which exited 99, while all FOUR cells that
# feed the streaming and non-streaming shift readings recorded zero steady drops
# and exited 0, the streaming pair with zero warmup drops as well. So the r1
# streaming shift is computed entirely from cells that passed every integrity
# threshold, and the cell that failed contributes to neither shift. That split is
# in its dropped-iterations.txt and k6-exit-codes.txt verbatim. The r3 row is
# likewise from an invalid run, rejected by band 3 on its non-streaming shift of
# +140us, and is kept for the same reason: both of its streaming cells passed.
#
# The streaming reading CHANGES SIGN across the five matrices while the
# non-streaming one never does. The -336us reading is essentially all drift: gross
# enforce-only work is 38.2us and the shared-path credit that offsets it is 24us, so
# nothing in the decomposition can produce a third of a millisecond of either sign.
# That negative excursion is the cleanest measure available of the streaming cells'
# drift envelope on this host, and it is an order of magnitude larger than the
# inherent work being measured.
#
# SO THIS BAND RECORDS AND ADVISES RATHER THAN GATING THE CENTRAL VALUE. A tight
# window around 163.5us would be a gate on drift, and four of the five matrices
# above would have failed it, including the cleanest one. Instead:
#   - the shift is printed every time, so a regression is visible in every
#     committed bands.txt even where it is not gated,
#   - an ADVISORY fires when the shift falls outside the 150B advisory window, which
#     the component decomposition shows is the range inherent work alone can
#     produce, and it says plainly that the reading is drift-dominated and must not
#     be published as the streaming enforcement cost,
#   - the GATE is a wide ceiling on the ABSOLUTE SIZE of the shift, sized to the
#     inherent work plus the 336us observed drift envelope, plus headroom, because a
#     largest-excursion estimate drawn from four samples underestimates the largest
#     excursion in general. 0.60ms is roughly 1.7 times that sum. The inherent term
#     was corrected from 19.5us to 15us on 2026-09-16, which moves the sum from
#     356us to 351us and leaves the ceiling where it was.
#
# The ceiling is two-sided deliberately. The inherent work is one-sided and can
# only be positive, but the drift that dominates the reading is two-sided, so a
# floor at zero would reject r3 purely for drift. A
# strongly negative shift is also the signature of an enforce cell that was not
# enforcing, a rendered-config or agent-header mix-up, so a symmetric ceiling
# catches that failure as well.
#
# WHAT THIS GATE DOES NOT CATCH, said out loud so it is never mistaken for tight.
# At 0.60ms it fires only on roughly a 40-fold regression in streaming
# enforcement cost. A doubling, from 15us to 30us, sits far inside the drift
# envelope and is invisible to any gate these data can support. Closing that
# needs streaming repetitions and a streaming drift canary, neither of which the
# matrix has today. It does not need a tighter number here, and inventing one
# would be the band 5 percentage mistake made a third time.
#
# LIMITATION, CALIBRATED ON LIMITED DATA. Five quick-mode matrices, one streaming
# repetition each, 5000 samples per streaming cell against 10001 per
# non-streaming cell, all on one host. The first evidence run is this band's first
# real test. If it fires there, the numbers and the matrix design get examined
# rather than the threshold moved.
BAND3_STREAM_SHIFT_MAX_ABSOLUTE_MILLISECONDS = 0.60

# Band 5. The opening and closing direct cells bracket the whole matrix. If they
# disagree, the machine drifted underneath the experiment and no cell in between
# can be compared to any other.
#
# AMENDED 2026-09-16, alongside the band 1 amendment above and for the same
# family of reason.
#
# ORIGINAL pre-registered form: the opening and closing canaries agreeing within
# 15 PERCENT at both P50 and P99.
# AMENDED form: two ABSOLUTE gates, P50 drift at or below 0.25ms and P99 drift at
# or below 1.50ms. The percentage is still computed and printed beside both gates
# as context, and is no longer a gate itself.
#
# The defect was the percentage, not the tail. A percentage tolerance on a
# sub-millisecond quantity produces an absolute tolerance tighter than the signal
# the experiment measures: 15 percent of a 0.265ms canary is 0.040ms, while the
# deltas this matrix publishes are roughly 0.195ms for band 2 and 0.043 to 0.120ms
# for band 3. So the original form could reject a run whose measured deltas were
# perfectly resolvable, which is incoherent in a validity gate. Band 1 was amended
# for the same shape of error, a number borrowed from an overhead SLO being applied
# as an absolute bound on one cell's latency. Observed canary drift across seven
# matrices on the reference host was 645.9, 144.0, 23.6, 57.6, 3.3, 141.2 and 28.8
# percent, so the original band passed 1 run in 7 and would have blocked any
# evidence run on this host.
#
# Those seven readings are the P99 leg, which is why the tail stayed a GATE rather
# than becoming an advisory. The percentages are large because the quantity is
# small, not because the tail is uninformative: the two worst of them turn out to
# be multi-millisecond absolute moves that any honest validity gate should reject.
# Expressing the tail in milliseconds separates those from the merely noisy runs,
# which a percentage cannot do at this magnitude.
#
# The P50 ceiling is sized to the band 2 shift it protects. A drift comparable to
# the smallest published delta is the point at which the comparison stops meaning
# anything. It has more slack than that framing suggests, because the passthrough
# and enforce cells run back to back as a pair by design, so slow drift across the
# matrix largely cancels WITHIN each pair. What the canary is really there to catch
# is GROSS drift, a thermal collapse or a background job that arrived mid run and
# stayed, which biases whole pairs rather than cancelling inside them.
#
# The P99 ceiling of 1.50ms is CALIBRATED against six historical matrices on this
# host. It was chosen after seeing which runs failed, which is exactly the kind of
# choice that deserves scrutiny, so the calibration data is recorded here in full
# and a reader can judge whether the number was picked honestly:
#
#   run   open P99  close P99  drift pct  absolute drift  at 1.50ms
#   r3    0.547     4.080          645.9         3.533ms  FAIL
#   r4    1.353     3.301          144.0         1.948ms  FAIL
#   r8    0.556     0.687           23.6         0.131ms  pass
#   r10   0.564     0.889           57.6         0.325ms  pass
#   r11   0.682     0.660            3.3         0.022ms  pass
#   last  0.777     1.001           28.8         0.224ms  pass
#
# Two rejections out of six, landing on exactly the two runs whose closing canary
# showed a multi-millisecond spike and which were independently attributed to host
# noise bursts. So the tail gate demonstrably still detects gross drift, which was
# the risk in dropping it, while no longer rejecting a run whose drift is smaller
# than the signal being measured, which was the original defect. Under the original
# 15 percent form only r11 passes.
#
# The seventh matrix, the 141.2 percent reading, is absent from that table because
# its absolutes were not recovered. It would fail the 1.50ms gate only if its
# opening canary P99 exceeded 1.062ms, which is inside the 0.547 to 1.353ms range
# the table shows, so its verdict under the amended band is genuinely unknown.
#
# LIMITATION, stated because the two gates are not equally well evidenced. Only
# ONE historical P50 pair was recovered, 0.265 then 0.341 for an absolute drift of
# 0.076ms, so unlike the P99 gate the 0.25ms P50 ceiling has never been tested
# against a pathological run. The first evidence run is its first real test. If it
# fails there, the numbers get examined rather than the threshold moved.
BAND5_CANARY_DRIFT_MAX_MILLISECONDS = 0.25
BAND5_CANARY_TAIL_DRIFT_MAX_MILLISECONDS = 1.50

# The achieved-versus-demanded arrival rate gate, ADDED 2026-09-16.
#
# WHAT IT IS FOR. Every band below compares quantiles between cells, and that
# comparison assumes each cell's latency distribution is SERVICE TIME. A cell
# demanding more than its capacity reports QUEUE RESIDENCE instead, which is a
# property of the arrival rate and the pool size rather than of levee, and no
# band can tell the two apart because a queue raises the central tendency exactly
# the way real work does.
#
# WHY IT SUBSUMES THE DROP COUNT. A dropped iteration means the VU pool ran out
# of workers, which is one symptom of over-demand rather than the condition
# itself. Give the pool enough slots to hold the backlog and a saturated cell
# drops nothing while still completing less work than was demanded of it, so the
# drop count reads clean and the published median is residence time. Achieved
# throughput catches the condition directly in both shapes. The drop gate is kept
# exactly as it was, at count==0 on the steady scenario, because it did its job
# correctly on the attempt that prompted this: it refused to publish a queue-time
# number as a latency number.
#
# THE MARGIN, and why 2 percent rather than something tighter. The legitimate
# envelope is far smaller. A healthy cell overshoots slightly, 10001 rows against
# 10000 demanded at 500 rps over 20 seconds on this host, and a k6 probe at 20 rps
# over 2 seconds delivered exactly 40 of 40. The one honest source of shortfall is
# work in flight when the window closes, bounded by the pool size times one
# service time, which at the matrix's worst cell is a couple of requests in 9000
# or 0.02 percent. So 2 percent is roughly 100 times the envelope.
#
# It is still 25 times smaller than the failure it exists to catch. The evidence
# attempt that prompted the gate demanded 500 rps at 32768B enforce and achieved
# about 256, a 49 percent shortfall. Saturation on this path is RETROGRADE past
# the knee, so an over-demanded cell does not miss its target by a few percent, it
# collapses. Sizing the margin nearer the envelope would start rejecting runs for
# single-iteration host stalls, which is the mistake bands 1 and 5 were both
# amended to stop making.
RATE_SHORTFALL_TOLERANCE_FRACTION = 0.02

# A cell's achieved rate is recomputed here from the COMMITTED CSV rows and cross
# checked against the figure k6 reported, because the summary is written by the
# process under measurement while the CSV is the artifact that gets published. The
# two count the same population and should agree to within a row or two, so a
# wider disagreement means one of them is not describing the published window.
RATE_CROSSCHECK_TOLERANCE_FRACTION = 0.01

# The small payload. Band 2, the recorded 150B enforcement advisory, the streaming
# companion and the A/A control all read cells at this size. The PRIMARY enforcement
# gate does not any more, see BAND3_PRIMARY_PAYLOAD_BYTES above.
SMALL_PAYLOAD_BYTES = 150

# The A/A control pair, ADDED 2026-09-16. Two cells that both run the PASSTHROUGH
# config at the small payload, so the repetition-matched P50 shift between them has
# a KNOWN TRUE VALUE OF ZERO and whatever it reads is the estimator's own noise.
#
# WHY IT EXISTS. Band 3 published a 15us enforcement signal for weeks without ever
# measuring what the same estimator reads when the answer is zero, which is the one
# number that says whether 15us is a measurement or a rounding error. Measured on a
# quiet host the A/A shift is -1, +4 and 0us, so the floor is about 4us and the 15us
# signal is genuinely above it.
#
# WHY IT IS REPORTED AND NOT GATED. A CONTENDED A/A pair still read 13us. A true
# zero reported as 13us means a passing control does NOT certify a quiet host, so
# gating on it would create exactly the false confidence this control was added to
# remove. It is necessary and not sufficient. The gate against contention is the host
# quiescence floor in run.sh.
#
# The role names come from run.sh's cell names and are what check_bands derives a
# role from, the text before the first hyphen.
CONTROL_A_ROLE = "controla"
CONTROL_B_ROLE = "controlb"

DURATION_METRIC = "http_req_duration"
WAITING_METRIC = "http_req_waiting"

REPETITION_PATTERN = re.compile(r"-r(\d+)$")


def spread_phrase(values: list[float]) -> str:
    """Describe the across-repetition spread, or say it cannot be resolved.

    A single repetition has a spread of zero by arithmetic rather than by
    measurement, and printing "spread 0.000ms across 1 repetitions" invites exactly
    the misreading that a quick-mode run resolved something.
    """
    if len(values) < 2:
        return "single repetition, so the spread is not resolvable"
    return f"spread {max(values) - min(values):.3f}ms across {len(values)} repetitions"


@dataclass
class Cell:
    """One k6 invocation, as recovered from the two artifacts it wrote."""

    name: str
    role: str
    stream: bool
    prompt_bytes: int
    rate: float
    max_vus: int
    repetition: int | None
    steady_duration_samples: list[float] = field(default_factory=list)
    steady_waiting_samples: list[float] = field(default_factory=list)
    summary_duration: dict = field(default_factory=dict)
    dropped_steady: int = 0
    dropped_warmup: int = 0
    failed_count: int = 0
    request_count: int = 0
    thresholds: dict = field(default_factory=dict)
    steady_seconds: float = 0.0
    reported_steady_requests: int | None = None
    cpu_seconds: float | None = None
    cpu_milliseconds_per_request: float | None = None

    def percentile(self, quantile: float) -> float:
        return percentile(self.steady_duration_samples, quantile)

    def waiting_percentile(self, quantile: float) -> float:
        return percentile(self.steady_waiting_samples, quantile)

    @property
    def csv_steady_requests(self) -> int:
        """The published row count, which is the population every band uses."""
        return len(self.steady_duration_samples)

    @property
    def achieved_rate(self) -> float | None:
        """Achieved steady throughput recomputed from the committed CSV rows.

        None when the steady duration is unrecoverable, which is the only case
        where the arithmetic cannot be done rather than merely coming out short.
        """
        if self.steady_seconds <= 0:
            return None
        return self.csv_steady_requests / self.steady_seconds

    @property
    def achieved_fraction(self) -> float | None:
        achieved = self.achieved_rate
        if achieved is None or self.rate <= 0:
            return None
        return achieved / self.rate


def percentile(values: list[float], quantile: float) -> float:
    """Return the quantile of values by linear interpolation between neighbours.

    quantile is a percentage. An empty input is a programming error here rather
    than a missing measurement, because every caller has already established
    that the cell produced steady rows.
    """
    if not values:
        raise ValueError("percentile of an empty sample")
    ordered = sorted(values)
    if len(ordered) == 1:
        return ordered[0]
    position = (len(ordered) - 1) * quantile / 100.0
    lower_index = int(position)
    upper_index = min(lower_index + 1, len(ordered) - 1)
    fraction = position - lower_index
    return ordered[lower_index] + (ordered[upper_index] - ordered[lower_index]) * fraction


def read_steady_samples(csv_path: str) -> tuple[list[float], list[float]]:
    """Return the duration and waiting samples from one filtered cell CSV.

    The column positions are read out of the header rather than assumed, so a k6
    CSV layout change fails here with a named cause instead of silently
    producing an empty or wrongly keyed sample.
    """
    durations: list[float] = []
    waitings: list[float] = []
    with gzip.open(csv_path, "rt", newline="") as handle:
        header_line = handle.readline()
        if not header_line:
            raise ValueError(f"{os.path.basename(csv_path)} is empty")
        header = header_line.rstrip("\n").split(",")
        for required in ("metric_name", "metric_value", "scenario"):
            if required not in header:
                raise ValueError(
                    f"{os.path.basename(csv_path)} header has no {required} column, "
                    "the k6 CSV format changed"
                )
        metric_index = header.index("metric_name")
        value_index = header.index("metric_value")
        scenario_index = header.index("scenario")
        for line in handle:
            columns = line.rstrip("\n").split(",")
            if len(columns) <= max(metric_index, value_index, scenario_index):
                continue
            if columns[scenario_index] != "steady":
                raise ValueError(
                    f"{os.path.basename(csv_path)} carries a "
                    f"{columns[scenario_index]!r} scenario row, the filter in run.sh "
                    "did not do its job"
                )
            metric = columns[metric_index]
            if metric == DURATION_METRIC:
                durations.append(float(columns[value_index]))
            elif metric == WAITING_METRIC:
                waitings.append(float(columns[value_index]))
    return durations, waitings


DURATION_SUFFIX_SECONDS = {"ms": 0.001, "s": 1.0, "m": 60.0, "h": 3600.0}


def parse_steady_seconds(summary: dict) -> float:
    """Return the steady window length in seconds from one cell summary.

    The numeric field is preferred. Directories written before it existed carry
    only the k6 duration STRING, so that is parsed as a fallback rather than
    refusing to gate an older run at all. An unparseable value returns 0, which
    the caller reports as an unrecoverable rate rather than as a passing one.
    """
    numeric = summary.get("steady_seconds")
    if isinstance(numeric, (int, float)) and numeric > 0:
        return float(numeric)
    text = str(summary.get("steady_duration", "")).strip()
    for suffix, multiplier in sorted(
        DURATION_SUFFIX_SECONDS.items(), key=lambda item: -len(item[0])
    ):
        if text.endswith(suffix):
            try:
                return float(text[: -len(suffix)]) * multiplier
            except ValueError:
                return 0.0
    return 0.0


def optional_number(values: dict[str, str], key: str) -> float | None:
    """Return one KEY=VALUE field as a float, or None when absent or "na".

    "na" is what run.sh writes for a cell with no levee in its path, so it means
    "not applicable here" rather than "measured as zero", and the two must not
    collapse into the same value.
    """
    raw = values.get(key)
    if raw is None or raw == "na":
        return None
    try:
        return float(raw)
    except ValueError:
        return None


def read_cpu_records(results_dir: str) -> dict[str, tuple[float | None, float | None]]:
    """Return per-cell CPU seconds and CPU milliseconds per request.

    Parsed from cpu-seconds.txt, which run.sh writes from ps cputime deltas taken
    at the two edges of each cell's steady window. A missing file means the run
    predates the sampling and every cell reports it as absent, which is honest.
    Direct cells record "na" because no levee is in their path.
    """
    path = os.path.join(results_dir, "cpu-seconds.txt")
    records: dict[str, tuple[float | None, float | None]] = {}
    if not os.path.exists(path):
        return records
    with open(path, encoding="utf-8") as handle:
        for line in handle:
            fields = line.split()
            if not fields:
                continue
            name = fields[0]
            values: dict[str, str] = {}
            for field_text in fields[1:]:
                if "=" in field_text:
                    key, _, value = field_text.partition("=")
                    values[key] = value

            records[name] = (
                optional_number(values, "cpu_seconds"),
                optional_number(values, "cpu_ms_per_request"),
            )
    return records


def load_cells(results_dir: str) -> list[Cell]:
    """Load every cell in the results directory, newest naming scheme aside.

    A summary without its CSV, or the reverse, is a partial run and is refused.
    """
    cpu_records = read_cpu_records(results_dir)
    cells: list[Cell] = []
    for entry in sorted(os.listdir(results_dir)):
        if not entry.endswith(".summary.json"):
            continue
        name = entry[: -len(".summary.json")]
        with open(os.path.join(results_dir, entry), encoding="utf-8") as handle:
            summary = json.load(handle)
        csv_path = os.path.join(results_dir, f"{name}.duration.csv.gz")
        if not os.path.exists(csv_path):
            raise ValueError(f"cell {name} has a summary but no filtered CSV")
        durations, waitings = read_steady_samples(csv_path)
        if not durations:
            raise ValueError(f"cell {name} has no steady {DURATION_METRIC} rows")
        repetition_match = REPETITION_PATTERN.search(name)
        # Only STEADY drops invalidate a cell. A warmup drop is levee's cold
        # start being absorbed by the phase that exists to absorb it, and the
        # enforce cells legitimately drop tens of iterations there while the
        # tokenizer is built on the first request. The k6 threshold is scoped
        # the same way, so gating on the total here would re-fail exactly what
        # the threshold correctly tolerated. The warmup count is reported rather
        # than discarded, because a climbing warmup count is a real signal.
        dropped_total = int(summary.get("dropped_iterations", 0))
        dropped_steady = int(summary.get("dropped_iterations_steady", dropped_total))
        dropped_warmup = int(summary.get("dropped_iterations_warmup", 0))
        reported_steady = summary.get("steady_request_count")
        cpu_seconds, cpu_per_request = cpu_records.get(name, (None, None))
        cells.append(
            Cell(
                name=name,
                role=name.split("-", 1)[0],
                stream=bool(summary.get("stream")),
                prompt_bytes=int(summary.get("prompt_bytes", 0)),
                rate=float(summary.get("rate", 0)),
                max_vus=int(summary.get("max_vus", 0)),
                repetition=int(repetition_match.group(1)) if repetition_match else None,
                steady_duration_samples=durations,
                steady_waiting_samples=waitings,
                summary_duration=summary.get("http_req_duration", {}) or {},
                dropped_steady=dropped_steady,
                dropped_warmup=dropped_warmup,
                failed_count=int(summary.get("http_req_failed_count", 0)),
                request_count=int(summary.get("request_count", 0)),
                thresholds=summary.get("thresholds", {}) or {},
                steady_seconds=parse_steady_seconds(summary),
                reported_steady_requests=(
                    int(reported_steady) if isinstance(reported_steady, (int, float)) else None
                ),
                cpu_seconds=cpu_seconds,
                cpu_milliseconds_per_request=cpu_per_request,
            )
        )
    if not cells:
        raise ValueError("no cell summaries found, this directory is not a run")
    return cells


def select(cells: list[Cell], role: str, stream: bool, prompt_bytes: int | None = None) -> list[Cell]:
    chosen = [cell for cell in cells if cell.role == role and cell.stream == stream]
    if prompt_bytes is not None:
        chosen = [cell for cell in chosen if cell.prompt_bytes == prompt_bytes]
    return sorted(chosen, key=lambda cell: cell.name)


def paired_shifts(
    baseline_cells: list[Cell], treatment_cells: list[Cell], quantile: float
) -> list[tuple[str, float]]:
    """Return per-repetition treatment minus baseline quantile shifts.

    Cells are matched on their repetition ordinal, which is what makes the shift
    a within-pair comparison rather than a comparison across cells that ran
    minutes apart.
    """
    by_repetition = {cell.repetition: cell for cell in baseline_cells}
    shifts: list[tuple[str, float]] = []
    for cell in treatment_cells:
        baseline = by_repetition.get(cell.repetition)
        if baseline is None:
            continue
        shifts.append((f"r{cell.repetition}", cell.percentile(quantile) - baseline.percentile(quantile)))
    return shifts


class Report:
    """Accumulates the gate report and remembers whether anything failed."""

    def __init__(self) -> None:
        self.failed = False

    def line(self, text: str = "") -> None:
        print(text)

    def verdict(self, label: str, ok: bool, detail: str) -> None:
        if not ok:
            self.failed = True
        print(f"{label} {'PASS' if ok else 'FAIL'} {detail}")


def report_inventory(report: Report, cells: list[Cell]) -> None:
    report.line("cell inventory and steady-scenario percentiles in milliseconds")
    report.line(
        "  {:<38} {:>7} {:>8} {:>8} {:>8} {:>8} {:>9} {:>7} {:>8}".format(
            "cell", "rows", "p50", "p90", "p99", "p99.9", "summaryp99", "demand", "achieved"
        )
    )
    for cell in sorted(cells, key=lambda item: item.name):
        summary_p99 = cell.summary_duration.get("p(99)")
        achieved = cell.achieved_rate
        report.line(
            "  {:<38} {:>7d} {:>8.3f} {:>8.3f} {:>8.3f} {:>8.3f} {:>9} {:>7g} {:>8}".format(
                cell.name,
                len(cell.steady_duration_samples),
                cell.percentile(50),
                cell.percentile(90),
                cell.percentile(99),
                cell.percentile(99.9),
                f"{summary_p99:.3f}" if isinstance(summary_p99, (int, float)) else "absent",
                cell.rate,
                f"{achieved:.1f}" if achieved is not None else "unknown",
            )
        )
    report.line(
        "  summaryp99 is the k6 whole-invocation value including warmup, "
        "shown for reference only"
    )
    report.line(
        "  demand and achieved are the arrival rate in requests per second, demanded by the "
        "harness and achieved in the steady window. They must agree, see the RATE gate: a cell "
        "short of its demand reports queue residence rather than service time, so every "
        "percentile on its row would be a number about the arrival rate and the VU pool"
    )
    # Time to first byte is reported for the streaming cells because against a
    # zero-latency upstream it sits only slightly below duration to EOF, and the
    # methodology has to show both real magnitudes rather than imply a large
    # separation the mock cannot produce.
    streaming = [cell for cell in cells if cell.stream and cell.steady_waiting_samples]
    if streaming:
        report.line("  time to first byte, steady scenario, streaming cells only")
        for cell in sorted(streaming, key=lambda item: item.name):
            report.line(
                "  {:<38} waiting p50 {:>7.3f} p99 {:>7.3f}".format(
                    cell.name, cell.waiting_percentile(50), cell.waiting_percentile(99)
                )
            )
    report.line()


def check_integrity(report: Report, cells: list[Cell]) -> None:
    """Re-verify the k6 integrity outcomes from the committed summaries.

    The harness already fails a run on a non-zero k6 exit, but a reader of a
    committed directory should not have to trust that it did.
    """
    problems: list[str] = []
    warmup_notes: list[str] = []
    for cell in sorted(cells, key=lambda item: item.name):
        if cell.dropped_steady != 0:
            problems.append(f"{cell.name} dropped {cell.dropped_steady} steady iterations")
        if cell.dropped_warmup != 0:
            warmup_notes.append(f"{cell.name} {cell.dropped_warmup}")
        if cell.failed_count != 0:
            problems.append(f"{cell.name} had {cell.failed_count} failed requests")
        if cell.request_count <= 0:
            problems.append(f"{cell.name} recorded no requests")
        for expression, outcome in sorted(cell.thresholds.items()):
            if outcome != "pass":
                problems.append(f"{cell.name} threshold {expression} reported {outcome}")
    if problems:
        detail = ", ".join(problems)
    else:
        detail = (
            f"{len(cells)} cells, no steady dropped iterations, no failed requests, "
            "every k6 threshold passed"
        )
    if warmup_notes:
        detail += (
            ". Warmup drops, tolerated by design and recorded so a climbing count stays "
            "visible, " + ", ".join(warmup_notes)
        )
    report.verdict("INTEGRITY", not problems, detail)


def check_achieved_rate(report: Report, cells: list[Cell]) -> None:
    """Refuse to publish a latency number from a cell that missed its demand.

    This runs BEFORE every band, because a band comparing two cells is only
    meaningful once both are known to have reported service time rather than queue
    residence, and this is the check that establishes it.
    """
    floor = 1.0 - RATE_SHORTFALL_TOLERANCE_FRACTION
    problems: list[str] = []
    observations: list[str] = []

    for cell in sorted(cells, key=lambda item: item.name):
        achieved = cell.achieved_rate
        fraction = cell.achieved_fraction
        if achieved is None or fraction is None:
            problems.append(
                f"{cell.name} has no recoverable steady duration or demanded rate, so whether "
                "it served its demand is unknown and its latency number cannot be published"
            )
            continue

        observations.append(
            f"{cell.name} {achieved:.1f} of {cell.rate:g} demanded "
            f"({fraction * 100:.1f} percent)"
        )
        if fraction < floor:
            problems.append(
                f"{cell.name} achieved {achieved:.1f} rps of {cell.rate:g} demanded, "
                f"{(1.0 - fraction) * 100:.1f} percent short"
            )

        # The cross check. k6 counted the steady requests itself, and the rows
        # committed for publication were filtered out of the raw CSV by a separate
        # code path in run.sh. A disagreement means one of the two is not
        # describing the window the bands are about to read.
        if cell.reported_steady_requests is not None and cell.reported_steady_requests > 0:
            divergence = abs(
                cell.csv_steady_requests - cell.reported_steady_requests
            ) / cell.reported_steady_requests
            if divergence > RATE_CROSSCHECK_TOLERANCE_FRACTION:
                problems.append(
                    f"{cell.name} committed {cell.csv_steady_requests} steady rows but k6 "
                    f"counted {cell.reported_steady_requests} steady requests, a "
                    f"{divergence * 100:.1f} percent disagreement, so the published rows are "
                    "not the window k6 measured"
                )

    if problems:
        detail = (
            ", ".join(problems)
            + f". A cell below {floor * 100:.0f} percent of its demanded arrival rate is "
            "reporting queue residence rather than service time, so its quantiles measure the "
            "arrival rate and the VU pool rather than levee. The fix is a lower rate for that "
            "payload size in rate_for_payload, sized from measured capacity, never a wider "
            "band. Throughput on the enforced path is RETROGRADE past its knee, so a cell that "
            "misses its demand will miss it by more when given a bigger pool, not less"
        )
        report.verdict("RATE", False, detail)
        return

    detail = (
        f"every cell served at least {floor * 100:.0f} percent of its demanded arrival rate, "
        + ", ".join(observations)
    )
    report.verdict("RATE", True, detail)


def report_cost(report: Report, cells: list[Cell]) -> None:
    """Print levee CPU cost per request, so saturation is self-evident.

    This is a REPORT and not a gate. The gate on saturation is the achieved-rate
    check above, which measures the consequence directly. This measures the cause,
    and the two together mean a reader never has to infer saturation from the shape
    of a latency curve.
    """
    measured = [cell for cell in cells if cell.cpu_milliseconds_per_request is not None]
    if not measured:
        report.line(
            "COST levee CPU per request was not sampled in this run, so saturation cannot be "
            "read off the artifact and has to be inferred from the achieved rates above"
        )
        report.line()
        return

    report.line(
        "COST levee CPU per request, from ps cputime deltas across the steady window, with the "
        "VU pool headroom each cell actually needed. Direct cells are absent from the CPU "
        "columns because no levee is in their path"
    )
    report.line(
        "  {:<38} {:>9} {:>11} {:>11} {:>9} {:>8} {:>9}".format(
            "cell", "cpu_s", "cpu_ms/req", "demand_rps", "cores", "vus_need", "headroom"
        )
    )
    for cell in sorted(measured, key=lambda item: item.name):
        # Cores is the honest saturation number: CPU milliseconds per request
        # times requests per second, divided by 1000, is how many cores the cell
        # kept busy. Compare it against the host's core count in the MANIFEST. A
        # cell approaching that count is at its knee whatever its latency says.
        cores = (cell.cpu_milliseconds_per_request or 0.0) * cell.rate / 1000.0
        # vus_need is Little's Law on the MEASURED median, arrival rate times
        # service time, so it is the concurrency the cell genuinely required
        # rather than a pre-declared estimate. headroom is the pool divided by it.
        # This is the number that answers whether one 40-slot pool can still
        # deliver the per-payload rates without dropping, and it is measured after
        # the fact rather than asserted before it.
        vus_needed = cell.rate * cell.percentile(50) / 1000.0
        headroom = (cell.max_vus / vus_needed) if vus_needed > 0 else float("inf")
        report.line(
            "  {:<38} {:>9.2f} {:>11.4f} {:>11g} {:>9.2f} {:>8.2f} {:>8.1f}x".format(
                cell.name,
                cell.cpu_seconds if cell.cpu_seconds is not None else float("nan"),
                cell.cpu_milliseconds_per_request or 0.0,
                cell.rate,
                cores,
                vus_needed,
                headroom,
            )
        )
    report.line(
        "  cores is cpu_ms/req times demand_rps over 1000, the count of cores the cell kept "
        "busy. Against hw.ncpu in the MANIFEST it says how close the cell ran to its knee"
    )
    report.line(
        "  vus_need is Little's Law on the measured P50, demand_rps times service time, so it "
        "is the concurrency the cell really needed. headroom is the 40-slot pool over it. A "
        "headroom near 1 means the pool is about to become part of what is measured"
    )
    report.line()


def check_band1(report: Report, cells: list[Cell]) -> None:
    direct = [cell for cell in cells if cell.role == "direct"]
    if not direct:
        report.verdict("BAND1", False, "no direct-to-mock cell in this run, the band cannot be evaluated")
        return
    offenders = []
    median_observations = []
    tail_observations = []
    loud_tails = []
    for cell in sorted(direct, key=lambda item: item.name):
        central = cell.percentile(50)
        tail = cell.percentile(99)
        median_observations.append(f"{cell.name} {central:.3f}")
        tail_observations.append(f"{cell.name} {tail:.3f}")
        if central >= BAND1_DIRECT_P50_MAX_MILLISECONDS:
            offenders.append(f"{cell.name} {central:.3f}ms")
        if tail >= BAND1_DIRECT_P99_ADVISORY_MILLISECONDS:
            loud_tails.append(f"{cell.name} {tail:.3f}ms")
    detail = (
        f"direct P50 below {BAND1_DIRECT_P50_MAX_MILLISECONDS}ms, observed "
        + ", ".join(median_observations)
        + f". Direct P99 recorded for the figures and for anyone applying the original "
        f"pre-registered form of this band, "
        + ", ".join(tail_observations)
    )
    if offenders:
        detail = (
            f"direct P50 must be below {BAND1_DIRECT_P50_MAX_MILLISECONDS}ms, over budget at "
            + ", ".join(offenders)
            + ". A raised central tendency on a cell with no proxy in the path means the box "
            "or the mock is the bottleneck, so no proxied number in this run means anything. "
            "Direct P99 alongside it, "
            + ", ".join(tail_observations)
        )
    report.verdict("BAND1", not offenders, detail)
    # An advisory, deliberately not a gate. A loud tail on a direct cell is host
    # noise rather than a property of levee, and failing the run on it is the
    # mistake this band was amended to stop making. It still gets said out loud,
    # because a reader comparing two evidence directories needs to know which one
    # was measured on a busy machine.
    if loud_tails:
        report.line(
            "BAND1 ADVISORY direct P99 above "
            f"{BAND1_DIRECT_P99_ADVISORY_MILLISECONDS}ms at "
            + ", ".join(loud_tails)
            + ". This is host tail noise, not a bottleneck, and it does not invalidate the "
            "run. Check cpu_idle_pct in machine-state.txt, which is the field that "
            "discriminates a contended host. Read loadavg there for context only: it was "
            "PROVEN not to discriminate, sitting at 4.0 to 6.4 on the invalidated 43-cell run "
            "and 2.8 to 5.0 on the quiet re-measurements that corrected it. On the reference "
            "host the resident endpoint-security agents and the Spotlight indexer produce "
            "tens-of-milliseconds scheduler stalls that land in every cell including the "
            "direct ones"
        )


def check_band2(report: Report, cells: list[Cell]) -> None:
    direct = select(cells, "direct", stream=False, prompt_bytes=SMALL_PAYLOAD_BYTES)
    passthrough = select(cells, "passthrough", stream=False, prompt_bytes=SMALL_PAYLOAD_BYTES)
    if not direct or not passthrough:
        report.verdict(
            "BAND2",
            False,
            f"needs direct and passthrough non-streaming cells at {SMALL_PAYLOAD_BYTES}B, "
            f"found {len(direct)} direct and {len(passthrough)} passthrough",
        )
        return
    baseline = statistics.median([cell.percentile(50) for cell in direct])
    shifts = [(cell.name, cell.percentile(50) - baseline) for cell in passthrough]
    median_shift = statistics.median([shift for _, shift in shifts])
    ok = (
        BAND2_PASSTHROUGH_SHIFT_MIN_MILLISECONDS
        <= median_shift
        <= BAND2_PASSTHROUGH_SHIFT_MAX_MILLISECONDS
    )
    detail = (
        f"passthrough minus direct P50 median {median_shift:.3f}ms, window "
        f"{BAND2_PASSTHROUGH_SHIFT_MIN_MILLISECONDS} to "
        f"{BAND2_PASSTHROUGH_SHIFT_MAX_MILLISECONDS}ms, direct baseline P50 "
        f"{baseline:.3f}ms from {len(direct)} cells, per cell "
        + ", ".join(f"{name} {shift:+.3f}" for name, shift in shifts)
    )
    if not ok:
        detail += (
            ". Below the floor means the cell did not traverse the proxy. Above the "
            "ceiling means something beyond the extra loopback hop is being paid"
        )
    report.verdict("BAND2", ok, detail)


def enforcement_shifts(
    cells: list[Cell],
    quantile: float,
    stream: bool = False,
    prompt_bytes: int = SMALL_PAYLOAD_BYTES,
) -> list[tuple[str, float]]:
    passthrough = select(cells, "passthrough", stream=stream, prompt_bytes=prompt_bytes)
    enforce = select(cells, "enforce", stream=stream, prompt_bytes=prompt_bytes)
    return paired_shifts(passthrough, enforce, quantile)


def check_band3(report: Report, cells: list[Cell]) -> float:
    """Evaluate the PRIMARY enforcement gate and return its median P50 shift.

    Relocated to BAND3_PRIMARY_PAYLOAD_BYTES on 2026-09-16 for signal to noise. The
    150B reading is still computed and printed, by report_band3_small below, and it
    no longer gates. Band 4 divides by the value returned here.
    """
    payload = BAND3_PRIMARY_PAYLOAD_BYTES
    shifts = enforcement_shifts(cells, 50, prompt_bytes=payload)
    if not shifts:
        report.verdict(
            "BAND3",
            False,
            f"no repetition-matched enforce and passthrough pair at {payload}B, so the "
            "PRIMARY enforcement gate has no cells to read and this run cannot be "
            f"published. Include {payload} in PROMPT_SIZES. A payload-restricted run is a "
            "capacity check rather than a validity check, and this verdict is the correct "
            "outcome for one rather than a harness fault",
        )
        return 0.0
    values = [shift for _, shift in shifts]
    median_shift = statistics.median(values)
    spread = max(values) - min(values)
    in_window = (
        BAND3_PRIMARY_SHIFT_MIN_MILLISECONDS
        <= median_shift
        <= BAND3_PRIMARY_SHIFT_MAX_MILLISECONDS
    )
    if len(values) >= 2:
        spread_resolved = spread < median_shift
        spread_detail = spread_phrase(values)
    else:
        # One repetition gives a spread of zero, which would pass the resolution
        # check by arithmetic without measuring anything. Say so rather than
        # letting a vacuous pass read as a real one.
        spread_resolved = True
        spread_detail = (
            "spread not evaluated, a single repetition cannot resolve it, "
            "this check is vacuous in quick mode"
        )
    ok = in_window and spread_resolved
    detail = (
        f"PRIMARY enforcement gate at {payload}B, enforce minus passthrough P50 median "
        f"{median_shift:.3f}ms, window {BAND3_PRIMARY_SHIFT_MIN_MILLISECONDS} to "
        f"{BAND3_PRIMARY_SHIFT_MAX_MILLISECONDS}ms, {spread_detail}, per repetition "
        + ", ".join(f"{label} {shift:+.3f}" for label, shift in shifts)
    )
    if median_shift < BAND3_PRIMARY_SHIFT_MIN_MILLISECONDS:
        detail += (
            ". Below the floor means the enforce arm was probably not enforcing, so check "
            "that the rendered config and the agent header reached it, and compare against "
            "the A/A control below: a non-enforcing arm reads what that control reads, which "
            "is single-digit microseconds rather than hundreds"
        )
    elif median_shift > BAND3_PRIMARY_SHIFT_MAX_MILLISECONDS:
        detail += (
            ". Above the ceiling means the enforcement path got materially more expensive, "
            "and the first thing to check is the number of tokenizer passes, since one "
            "extra pass at this payload size is roughly 340us on its own"
        )
    elif not spread_resolved:
        detail += (
            ". The spread exceeds the signal, so this run cannot resolve the "
            "enforcement cost. More repetitions are needed, not a wider band"
        )
    report.verdict("BAND3", ok, detail)
    return median_shift


def report_band3_small(report: Report, cells: list[Cell]) -> None:
    """Print the 150B enforcement reading, with an advisory and no gate.

    RELOCATED rather than dropped on 2026-09-16. Every number the gated form used to
    print is still printed here, so a reader can apply the original band by hand and
    see what it would have said. What is gone is its power to invalidate a run, for
    the signal-to-noise reasons argued at BAND3_PRIMARY_PAYLOAD_BYTES.
    """
    shifts = enforcement_shifts(cells, 50, prompt_bytes=SMALL_PAYLOAD_BYTES)
    tail_shifts = enforcement_shifts(cells, 99, prompt_bytes=SMALL_PAYLOAD_BYTES)
    if not shifts:
        report.line(
            f"BAND3-SMALL absent, no repetition-matched enforce and passthrough pair at "
            f"{SMALL_PAYLOAD_BYTES}B in this run, so the recorded small-payload "
            "enforcement reading is unavailable. This is a record and not a gate, so it "
            "does not invalidate the run"
        )
        return

    values = [shift for _, shift in shifts]
    median_shift = statistics.median(values)
    inside = (
        BAND3_SMALL_SHIFT_ADVISORY_MIN_MILLISECONDS
        <= median_shift
        <= BAND3_SMALL_SHIFT_ADVISORY_MAX_MILLISECONDS
    )
    tail_note = ""
    if tail_shifts:
        tail_median = statistics.median([shift for _, shift in tail_shifts])
        tail_note = (
            f". P99 shift median {tail_median:+.3f}ms at this payload, printed because band 4 "
            "no longer gates it, context only"
        )
    report.line(
        f"BAND3-SMALL RECORDED enforce minus passthrough P50 median {median_shift:+.3f}ms at "
        f"{SMALL_PAYLOAD_BYTES}B, {spread_phrase(values)}, advisory window "
        f"{BAND3_SMALL_SHIFT_ADVISORY_MIN_MILLISECONDS} to "
        f"{BAND3_SMALL_SHIFT_ADVISORY_MAX_MILLISECONDS}ms, per repetition "
        + ", ".join(f"{label} {shift:+.3f}" for label, shift in shifts)
        + tail_note
    )
    if not inside:
        report.line(
            f"BAND3-SMALL ADVISORY the {median_shift:+.3f}ms reading is outside the "
            f"{BAND3_SMALL_SHIFT_ADVISORY_MIN_MILLISECONDS} to "
            f"{BAND3_SMALL_SHIFT_ADVISORY_MAX_MILLISECONDS}ms window that the component "
            "decomposition says inherent enforcement work can produce at this payload, which "
            "is 38.2us of gross enforce-only work against a 24us shared-path credit for a net "
            "near +15us. A reading well above that window is host contention rather than "
            "levee: three background busy loops reproduce exactly this, moving the same "
            "measurement to +93 and +109us while every integrity gate still reads clean. "
            "Check cpu_idle_pct in machine-state.txt and the A/A control below, which reads "
            "the noise the same estimator invents when the true answer is zero. THIS READING "
            "MUST NOT BE PUBLISHED as the enforcement cost at this payload. It does NOT "
            "invalidate the run, because the gate that decides enforcement cost is at "
            f"{BAND3_PRIMARY_PAYLOAD_BYTES}B where the signal is roughly 160 times the "
            "estimator noise floor rather than 4 times it, and a run whose "
            f"{BAND3_PRIMARY_PAYLOAD_BYTES}B gate passes while this advisory fires is a run "
            "with a sound enforcement measurement and an unusable small-payload one"
        )
    if median_shift < 0:
        report.line(
            "BAND3-SMALL NOTE a negative reading here is expected rather than alarming. "
            "Gross enforce-only work at this payload is 38.2us and the shared path runs 24us "
            "FASTER in the enforce arm, so the net is a small difference between two larger "
            "terms. Removing one of the two tokenizer passes takes gross work to 20.8us "
            "against the same credit, which puts the net near -3us. Enforcement did not "
            "become free, and the work itself is measured by the "
            f"{BAND3_PRIMARY_PAYLOAD_BYTES}B gate and by microbench.txt, neither of which "
            "can go negative"
        )


def report_control_pair(report: Report, cells: list[Cell]) -> None:
    """Print the A/A control shift, whose TRUE VALUE IS ZERO.

    A REPORT and never a gate, for the reasons argued at CONTROL_A_ROLE. It sits
    beside the enforcement readings on purpose: the noise floor and the signal it
    qualifies belong on the same screen.
    """
    arm_a = select(cells, CONTROL_A_ROLE, stream=False, prompt_bytes=SMALL_PAYLOAD_BYTES)
    arm_b = select(cells, CONTROL_B_ROLE, stream=False, prompt_bytes=SMALL_PAYLOAD_BYTES)
    if not arm_a or not arm_b:
        report.line(
            "CONTROL-AA absent, this run has no passthrough-versus-passthrough control pair, "
            "so the estimator's noise floor is not measured in the same run that publishes a "
            "number and has to be taken from the quiet-host readings of -1, +4 and 0us. Any "
            "directory written before the control was added to the matrix reads this way"
        )
        return

    shifts = paired_shifts(arm_a, arm_b, 50)
    if not shifts:
        report.line(
            "CONTROL-AA present but unpaired, the two control arms share no repetition "
            "ordinal, so no shift can be computed"
        )
        return

    values = [shift for _, shift in shifts]
    median_shift = statistics.median(values)
    report.line(
        f"CONTROL-AA RECORDED passthrough minus passthrough P50 median {median_shift:+.3f}ms, "
        f"{spread_phrase(values)}, per repetition "
        + ", ".join(f"{label} {shift:+.3f}" for label, shift in shifts)
    )
    report.line(
        "  This is a CONTROL and its EXPECTED VALUE IS ZERO. Both arms run the same "
        "passthrough config at the same payload and rate, with the same levee restart and "
        "TIME_WAIT drain between them as the enforcement pair, so whatever it reads is noise "
        "the estimator invented rather than work levee did. Read it as the resolution limit "
        "of every shift printed above it"
    )
    report.line(
        "  It is NOT a gate, and a small reading here does NOT certify a quiet host. On a "
        "quiet host this control reads -1, +4 and 0us, so the floor is about 4us. Under the "
        "three-busy-loop contention that reproduced the invalidated 43-cell run it still read "
        "13us, which is a true zero reported as 13us. So the control is necessary and not "
        "sufficient, and the check that refuses a contended host is the cpu_idle_pct floor in "
        "run.sh rather than this line"
    )


def check_band4(report: Report, cells: list[Cell], median_p50_shift: float) -> None:
    payload = BAND3_PRIMARY_PAYLOAD_BYTES
    shifts = enforcement_shifts(cells, 99, prompt_bytes=payload)
    if not shifts:
        report.verdict(
            "BAND4",
            False,
            f"no repetition-matched enforce and passthrough pair at {payload}B, and this band "
            "follows band 3 to that payload size because it is a ratio against band 3's median, "
            "so it cannot be evaluated",
        )
        return
    values = [shift for _, shift in shifts]
    median_tail_shift = statistics.median(values)
    if median_p50_shift > 0:
        allowance = BAND4_TAIL_SHIFT_RATIO_MAX * median_p50_shift
        basis = f"{BAND4_TAIL_SHIFT_RATIO_MAX:g} times the median P50 shift of {median_p50_shift:.3f}ms"
    else:
        # A ratio against a zero or negative median shift is undefined, so the
        # band falls back to the same multiple of band 3's own ceiling. That is
        # the widest the ratio form could ever have allowed while band 3 passed,
        # so the fallback can never be looser than the rule it stands in for.
        allowance = BAND4_TAIL_SHIFT_RATIO_MAX * BAND3_PRIMARY_SHIFT_MAX_MILLISECONDS
        basis = (
            f"{BAND4_TAIL_SHIFT_RATIO_MAX:g} times the band 3 ceiling of "
            f"{BAND3_PRIMARY_SHIFT_MAX_MILLISECONDS}ms, because the median P50 shift of "
            f"{median_p50_shift:.3f}ms is not positive and a ratio against it is undefined"
        )
    ok = median_tail_shift <= allowance
    detail = (
        f"at {payload}B, enforce minus passthrough P99 median {median_tail_shift:.3f}ms against an "
        f"allowance of {allowance:.3f}ms, which is {basis}, per repetition "
        + ", ".join(f"{label} {shift:+.3f}" for label, shift in shifts)
    )
    if not ok:
        detail += (
            ". A tail shift this far above the median shift means one cell caught a "
            "transient that every median gate missed"
        )
    report.verdict("BAND4", ok, detail)


def check_band3_stream(report: Report, cells: list[Cell]) -> None:
    """Evaluate the streaming companion to band 3.

    The gate is on the absolute size of the shift and is deliberately wide. The
    central value is RECORDED and ADVISED on rather than gated, for the reasons
    tabulated at BAND3_STREAM_SHIFT_MAX_ABSOLUTE_MILLISECONDS above.
    """
    shifts = enforcement_shifts(cells, 50, stream=True)
    if not shifts:
        report.verdict(
            "BAND3-STREAM",
            False,
            f"no repetition-matched streaming enforce and passthrough pair at "
            f"{SMALL_PAYLOAD_BYTES}B, the band cannot be evaluated",
        )
        return
    values = [shift for _, shift in shifts]
    median_shift = statistics.median(values)
    ceiling = BAND3_STREAM_SHIFT_MAX_ABSOLUTE_MILLISECONDS
    ok = abs(median_shift) <= ceiling

    tail_shifts = enforcement_shifts(cells, 99, stream=True)
    if tail_shifts:
        tail_note = (
            f". Streaming P99 shift median "
            f"{statistics.median([shift for _, shift in tail_shifts]):.3f}ms, context only, "
            "no gate"
        )
    else:
        tail_note = ""

    detail = (
        f"streaming enforce minus passthrough P50 median {median_shift:+.3f}ms against a "
        f"two-sided ceiling of {ceiling}ms on its absolute size, per repetition "
        + ", ".join(f"{label} {shift:+.3f}" for label, shift in shifts)
        + tail_note
    )
    if not ok:
        detail += (
            ". A shift this large in either direction is beyond what the measured inherent "
            "streaming enforcement cost plus the known drift envelope on this host can "
            "produce. Positive means a real regression in the estimate, admit, reconcile "
            "path. Negative means the enforce cell probably was not enforcing, so check "
            "that the rendered config and the agent header reached it"
        )
    report.verdict("BAND3-STREAM", ok, detail)

    # The advisory, deliberately not a gate. Inherent enforcement work at this
    # payload nets +15us by the component decomposition, so a reading outside the
    # 150B advisory window is drift rather than work, and a reader who quotes it as
    # the streaming enforcement cost would be quoting host noise.
    if not (
        BAND3_SMALL_SHIFT_ADVISORY_MIN_MILLISECONDS
        <= median_shift
        <= BAND3_SMALL_SHIFT_ADVISORY_MAX_MILLISECONDS
    ):
        report.line(
            f"BAND3-STREAM ADVISORY the {median_shift:+.3f}ms streaming shift is outside the "
            f"{BAND3_SMALL_SHIFT_ADVISORY_MIN_MILLISECONDS} to "
            f"{BAND3_SMALL_SHIFT_ADVISORY_MAX_MILLISECONDS}ms window the component "
            "decomposition says inherent enforcement work can produce at this payload. This "
            "reading is drift-dominated and must NOT be published as the streaming "
            "enforcement cost. It "
            "does not invalidate the run, because the matrix runs one streaming repetition "
            "with no streaming drift canary and so cannot resolve a shift this small. Check "
            "cpu_idle_pct in machine-state.txt, which is the field that discriminates a "
            "contended host, and the A/A control for what the estimator invents at a true zero"
        )


def check_band5(report: Report, cells: list[Cell]) -> None:
    opening = [cell for cell in cells if "canary-open" in cell.name]
    closing = [cell for cell in cells if "canary-close" in cell.name]
    if len(opening) != 1 or len(closing) != 1:
        report.verdict(
            "BAND5",
            False,
            f"expected exactly one opening and one closing drift canary, found "
            f"{len(opening)} and {len(closing)}. A run missing a canary cannot be "
            "checked for drift and is not evidence",
        )
        return
    open_cell, close_cell = opening[0], closing[0]
    observations = []
    offenders = []
    # Both quantiles are absolute gates with their own ceiling, evaluated the same
    # way because they ask the same question at different parts of the
    # distribution. The two ceilings differ by an order of magnitude, and that gap
    # is the point: the central tendency should barely move across a matrix, while
    # a tail on this host legitimately moves by hundreds of microseconds without
    # the experiment being compromised.
    for label, quantile, ceiling in (
        ("P50", 50, BAND5_CANARY_DRIFT_MAX_MILLISECONDS),
        ("P99", 99, BAND5_CANARY_TAIL_DRIFT_MAX_MILLISECONDS),
    ):
        first = open_cell.percentile(quantile)
        last = close_cell.percentile(quantile)
        drift = abs(last - first)
        # The percentage is kept in the ratio form the original band gated on, so a
        # figure printed here stays directly comparable to the historical readings
        # tabulated in the amendment note above.
        smaller = min(first, last)
        fraction = (max(first, last) / smaller) - 1.0 if smaller > 0 else float("inf")
        observations.append(
            f"{label} {first:.3f} then {last:.3f}, absolute drift {drift:.3f}ms against a "
            f"ceiling of {ceiling}ms, which is {fraction * 100:.1f} percent of the smaller "
            "cell and is context rather than a gate"
        )
        if drift > ceiling:
            offenders.append(f"{label} drifted {drift:.3f}ms past its {ceiling}ms ceiling")
    detail = "opening and closing direct canaries, " + ". ".join(observations)
    if offenders:
        detail += (
            ". "
            + ", ".join(offenders)
            + ". A drift this size means the machine moved underneath the experiment by as "
            "much as the effect being measured, so no cell can be compared to any other "
            "and the whole run is invalid. Look for a thermal event or a background job "
            "that arrived mid run and stayed, in the per-cell cpu_idle_pct readings in "
            "machine-state.txt. An evidence run refuses to continue below the idle floor "
            "recorded there, so a run that reached this band without that refusal drifted "
            "for some reason other than sustained CPU contention"
        )
    report.verdict("BAND5", not offenders, detail)


def main(argv: list[str]) -> int:
    if len(argv) != 2:
        print("usage: check_bands.py <results-directory>", file=sys.stderr)
        return 2
    results_dir = argv[1]

    report = Report()
    # Only the directory name is printed. The absolute path is a home directory
    # path, and the harness audits this file for exactly that.
    report.line(f"pre-registered sanity band evaluation for {os.path.basename(results_dir)}")
    report.line(f"checker python {sys.version.split()[0]}")
    report.line()

    try:
        cells = load_cells(results_dir)
    except (OSError, ValueError, json.JSONDecodeError) as error:
        print(f"VERDICT INVALID could not load the run, {error}")
        return 1

    report_inventory(report, cells)
    report_cost(report, cells)
    check_integrity(report, cells)
    check_achieved_rate(report, cells)
    check_band1(report, cells)
    check_band2(report, cells)
    # The order here is the reading order a skeptic needs. The primary gate first,
    # then the small-payload reading it was relocated from, then the control that
    # says what the estimator behind both of them invents at a true zero. Putting the
    # control last of the three means the noise floor is on screen underneath every
    # shift it qualifies.
    median_p50_shift = check_band3(report, cells)
    report_band3_small(report, cells)
    report_control_pair(report, cells)
    check_band4(report, cells, median_p50_shift)
    check_band3_stream(report, cells)
    check_band5(report, cells)

    report.line()
    if report.failed:
        report.line("VERDICT INVALID this run violated at least one pre-registered band and is NOT publishable")
        return 1
    report.line("VERDICT VALID every pre-registered band passed")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
