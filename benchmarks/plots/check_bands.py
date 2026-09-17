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
#
# AMENDED AGAIN 2026-09-17. The single P50 ceiling becomes a PER-MODE PAIR and the
# P99 advisory splits with it. A non-streaming direct cell keeps 1.0ms at P50 and
# 2.5ms at the tail, unchanged in value and in behaviour. A streaming direct cell
# gets 2.0ms and 5.0ms.
#
# THIS IS NOT A FRESH INSIGHT AND THE RECORD SHOULD NOT READ AS IF IT WERE. When
# band 1 was amended above, scoping it by stream mode was explicitly offered as the
# alternative and was REJECTED in favour of moving the quantile from P99 to P50. Two
# independent defects were bundled into one choice and only one of them got fixed.
# The quantile move was right and stands: a P99 bound cannot tell a bottleneck from
# tail noise. It says nothing whatever about a ceiling calibrated on one response
# shape being applied to a different one, which is the defect that was left standing,
# and it took a completed evidence run to collect on it. That exchange is not in the
# git history, so this paragraph is the only record of it.
#
# WHAT FAILED. 2026-09-17-4d3f224-m3pro-macos-evidence-r1 COMPLETED all 53 cells and
# was refused by this band and by nothing else. Its three direct streaming cells read
# P50 1.341, 1.230 and 0.951ms against the 1.0ms ceiling. Every non-streaming direct
# cell in the same run passed with room: canary-open 0.432, canary-close 0.506,
# payload-4096 0.546, payload-32768 0.836.
#
# NOT A CONTENTION FAILURE, and that was checked rather than assumed. The host idle
# readings at those three cells were 61.8, 60.57 and 70.84 percent against the 60
# percent floor, and the two lowest-idle cells produced the two highest medians, so
# noise is certainly in the reading. But the run's contended-cells.txt names exactly
# ONE breaching cell, direct-canary-close-nonstream-150 at 56.39 percent idle, and it
# is none of these three. Noise contributed. It is not the cause.
#
# THE CAUSE IS STRUCTURAL. A streaming response replays six SSE events with a write
# and a flush each, which is six TCP segments and six loopback round trips worth of
# scheduling. A non-streaming response is one write. Duration to last byte is
# therefore a DIFFERENT QUANTITY in the two modes rather than the same quantity
# measured twice, and one ceiling cannot serve both. At the only payload where both
# modes exist, 150 bytes, the streaming P50 median on this host is 0.648ms against
# 0.293ms non-streaming, a mode ratio of 2.21. The marginal cost of each extra SSE
# segment is 0.648 minus 0.293 over 5, about 71us.
#
# THE EVIDENCE, every direct cell reading in this results tree, recomputed from the
# committed steady-only CSVs. Six early directories are aborted runs that never wrote
# a filtered CSV and are absent for that reason:
#
#   group                  n   P50 median   P50 min   P50 max   P99 median   P99 max
#   non-streaming   150B   22       0.293     0.255     0.506        0.720     1.611
#   non-streaming  4096B    8       0.334     0.307     0.546        0.855     1.419
#   non-streaming 32768B   11       0.485     0.278     0.899        1.193     2.835
#   streaming       150B   13       0.648     0.540     1.341        1.816     2.606
#
# The thirteen streaming P50 readings in full, sorted, because the ceiling is derived
# from them: 0.540, 0.555, 0.584, 0.597, 0.600, 0.643, 0.648, 0.649, 0.743, 0.791,
# 0.951, 1.230, 1.341. The last three are the failing run's three repetitions.
#
# HOW THE ERROR SURVIVED THE FIRST AMENDMENT, visible by comparing that amendment's
# own cited figures with the table above. It put direct P50 at "around 0.3ms
# non-streaming and 0.46ms streaming" and concluded that 1.0ms therefore sat at two to
# three times expected. The non-streaming figure holds up, 0.293ms measured. The
# streaming one does NOT: 0.46ms is below EVERY streaming reading now in this tree,
# whose minimum is 0.540ms and whose median is 0.648ms. At 0.46ms the single ceiling
# genuinely would have been 2.2 times expected for streaming and the split would have
# looked unnecessary. At the measured 0.648ms it is 1.54 times, which is not a band at
# all. So the conclusion was arithmetically sound on a streaming central value that was
# roughly 30 percent too low. The five matrices it drew on predate the currently
# loadable directories, so its figures cannot be recomputed and are left as written.
# The lesson is narrow and worth keeping: an amendment that justifies a threshold by a
# multiple of an expected value has to name where that expected value was measured, or
# the multiple cannot be rechecked when the data grows.
#
# A PROVENANCE TRAP worth naming, because it produced two slightly different sets of
# numbers for the same cells during this amendment. The failing run's streaming P50
# values are 1.341, 1.230 and 0.951 in the committed CSVs and 1.339, 1.238 and 0.947
# in the summary JSON p(50) fields. This band gates the CSV values, for the reason
# the module docstring gives: the summary aggregates the WHOLE invocation including
# warmup, so gating on it would gate numbers nobody publishes. Anyone re-deriving
# these thresholds has to read the CSV column.
#
# THE DERIVATION, one rule covering both thresholds. Each streaming threshold is the
# non-streaming one multiplied by the MEASURED mode ratio, then rounded DOWN to a
# round figure so the streaming arm stays relatively stricter than the non-streaming
# arm it derives from. At P50 that is 1.0 times 2.21, which is 2.21ms, rounded down
# to 2.0ms. At P99 it is 2.5 times 2.52, which is 6.30ms, rounded down to 5.0ms. So
# both streaming thresholds are exactly double their non-streaming counterparts while
# both measured mode ratios exceed two, and the rounding direction is the conservative
# one by construction rather than by taste.
#
#   quantity                                   non-streaming     streaming
#   P50 ceiling                                        1.0ms         2.0ms
#   payload-matched 150B P50 median                  0.293ms       0.648ms
#   ceiling as a multiple of that median                3.41x         3.09x
#   ceiling over the largest 150B reading               1.98x         1.49x
#
# Both rows are payload-matched at 150 bytes, which is the only payload where both
# modes exist, and the non-streaming column is therefore NOT that arm's worst case.
# Against direct-payload-32768, whose largest reading in this tree is 0.899ms, the
# unchanged non-streaming ceiling has only 1.11-fold margin. That is the tightest
# margin anywhere in band 1 after this amendment and it is recorded again below.
#
# So the two arms now refuse a run at almost the same severity, 3.41 against 3.09, a
# 10 percent disagreement. Under the single 1.0ms ceiling they disagreed by a factor
# of 2.2, the non-streaming arm refusing at 3.41 times its central value while the
# streaming arm refused at 1.54 times its own. THAT is the defect stated as a number,
# and it is why the unsplit ceiling cuts through the middle of a distribution whose
# observed span is 0.540 to 1.341ms.
#
# IT STILL DETECTS A BOTTLENECK, which is this band's whole purpose and the test any
# widening has to survive. Two failure shapes, both still caught:
#
#   - THE BOX SLOWS DOWN. A uniform 3.09-fold slowdown trips the streaming gate and a
#     3.41-fold one trips the non-streaming gate, so a genuinely saturated host now
#     fails band 1 on both arms at comparable severity. The old single ceiling did not
#     have that property, and a host slow enough to matter would have been caught on
#     the streaming arm alone at 1.54-fold, which reads as a streaming problem rather
#     than as the host problem it is.
#   - THE MOCK BECOMES THE BOTTLENECK ON ITS STREAMING PATH, which is the one failure
#     only this cell can see. Its per-segment cost would have to rise from 71us to
#     2.0 minus 0.293 over 5, about 341us, a 4.8-fold rise in the term that is unique
#     to streaming.
#
# Neither is a subtle regression, and that is said out loud rather than implied. This
# band has never been a sensitivity instrument. It answers one question, whether the
# floor is so high that no proxied number in the run means anything, and 2.0ms answers
# it for the streaming arm at the same relative strictness that 1.0ms answers it for
# the non-streaming arm.
#
# WHY AN INFLATED STREAMING FLOOR IS NOT BY ITSELF DISQUALIFYING, which is what
# licenses sizing this ceiling on relative rather than absolute grounds. The direct
# streaming cell has exactly two consumers: this band, and the baseline that
# overhead_figure.py pairs the streaming arms against. Both published streaming
# quantities are DIFFERENCES. The figure draws P99 proxied minus P99 direct with the
# same-mode direct cell as its baseline, and BAND3-STREAM is enforce minus passthrough
# and never reads the direct cell at all. A floor inflated uniformly by host state
# therefore largely cancels out of both. What would NOT cancel is a bottleneck
# confined to the direct arm, and that shows up as a shrinking or negative
# passthrough minus direct shift rather than as a raised absolute floor.
#
# THE RESIDUAL GAP, recorded rather than closed. Band 2 gates passthrough minus direct
# at 150B with a floor of 0.05ms, so a direct arm inflated on its own is caught for
# the NON-streaming mode. There is no streaming equivalent, so a streaming-only direct
# inflation is visible on the figure and gated nowhere. Closing that needs a streaming
# band 2, which is a matrix change rather than a threshold change.
#
# THE P99 ADVISORY SPLITS TOO, and its reasoning is deliberately recorded as weaker
# than the gate's, because it is. 5.0ms is 2.75 times the streaming P99 median of
# 1.816ms while 2.5ms is 3.47 times the non-streaming median of 0.720ms, so the same
# relative-strictness ordering holds at the tail.
#
# THE CLAIM THAT PROMPTED THAT SPLIT DOES NOT SURVIVE MEASUREMENT, and it is recorded
# that way rather than quietly repaired. The split was proposed on the ground that an
# unsplit 2.5ms advisory fires on most runs and so becomes noise a reader learns to
# ignore. It does not. Across every reading in this tree it fires on 1 of 13 streaming
# cells and 1 of 41 non-streaming ones. What IS true is narrower and is the real
# reason to split it: the largest streaming P99 on any run other than the failing one
# is 2.422ms, which is 97 percent of the 2.5ms threshold, so on the streaming arm the
# advisory sits one ordinary noise burst below firing and carries almost no
# discriminating power when it does fire. At 5.0ms it fires on the class of burst it
# exists to name. The band 5 calibration table below records non-streaming closing
# canaries reaching 4.080 and 3.301ms, which are 5.7 and 4.6 times the non-streaming
# P99 median, and a burst of that relative size on a streaming cell reads 10.3 and
# 8.3ms.
#
# WHAT THIS SPLIT DOES NOT FIX, found while deriving it and left alone on purpose. The
# advisory is miscalibrated ACROSS PAYLOADS as well as across modes, and worse in that
# dimension. The 32768-byte non-streaming cell has a P99 median of 1.193ms against the
# same 2.5ms advisory, 2.10 times, the tightest relative advisory of any group, and it
# is the only non-streaming cell that has ever fired the advisory, at 2.835ms. A
# per-payload split would be this same argument in a third dimension. It is not made
# here, because this is an advisory that never blocks publication and because
# bundling it in would repeat the exact bundling mistake the second paragraph of this
# amendment is about.
#
# LIMITATION, AND IT ARGUES FOR A WIDER NUMBER THAN THE ONE CHOSEN. The mode ratio
# above is pooled across 11 runs, of which 10 are quick-mode. Split by run mode it is
# 2.14 on quick data and 2.62 on the one evidence run, because that run's streaming
# cells were elevated 1.98-fold over quick while its non-streaming 150B cells were
# elevated only 1.61-fold, which is what six segments of exposure to scheduler stalls
# looks like. Calibrating on the evidence cut would give 2.6ms rather than 2.0ms. It
# was NOT used, for two reasons: it rests on 3 streaming and 2 non-streaming readings
# from a single run, and that run is the one that failed, so sizing the ceiling to it
# is fitting a band to the run it has to judge. The consequence is stated plainly
# instead: at 2.0ms a fifth evidence run as loaded as the fourth has 1.49-fold margin
# on this arm. If a run that PASSES the quiescence floor ever reads above 2.0ms here,
# the response is to examine the mock's per-segment cost and the host, not to widen
# the ceiling.
#
# THE TIGHTEST GATE LEFT IN BAND 1 IS NOT THE ONE THIS AMENDMENT TOUCHED, and it is
# recorded rather than fixed. direct-payload-32768 has read as high as 0.899ms against
# its unchanged 1.0ms non-streaming ceiling, 1.11-fold margin, and it read 0.836ms on
# the fourth evidence attempt. Every other direct cell has at least 1.49-fold. Note
# also that band 1 is the ONLY band that does not honour contended-cells.txt: it
# evaluates every direct cell whatever its host state, deliberately, because a floor
# measured on a busy machine is still the floor that run's proxied numbers sit on. So
# no repetition-dropping rule can rescue that cell on a loaded run. Widening it would
# need a calibration this tree cannot supply, eleven readings from one host of which
# the two highest both come from runs whose other cells were also elevated.
#
# NOT RETROACTIVE. 2026-09-17-4d3f224-m3pro-macos-evidence-r1 would PASS the amended
# band and it stays INVALID. Its committed bands.txt keeps the FAIL line and the
# INVALID verdict it was judged under. A band amended after seeing a run and then
# applied backwards to that run is not a pre-registered band at all. The amended form
# binds the NEXT run.
#
# AMENDED A THIRD TIME 2026-09-17. The per-mode PAIR becomes a PER-GROUP TABLE, one
# calibrated P50 ceiling and one calibrated P99 advisory for every direct-cell group in
# the matrix, where a group is a response mode paired with a payload size.
#
# THIS IS BAND 1'S THIRD AMENDMENT AND THE SECOND IN TWO DAYS, and a reader who sees
# two amendments to one band inside two days deserves to be told at once that they are
# not repeated tuning. THEY ARE THE SAME CATEGORY ERROR FOUND ON TWO DIFFERENT AXES. The
# error is a threshold calibrated on one quantity and applied to a different quantity
# that happens to be reported in the same unit. The amendment above found it on the
# RESPONSE MODE axis, where a six-write streaming response was judged by a ceiling
# derived from one-write non-streaming cells. This one finds it on the PAYLOAD SIZE
# axis, where a 32768-byte response was judged by a ceiling derived from 150-byte cells.
# Nothing about the second fix generalised it, because it was written as a special case,
# and the third paragraph of that amendment even names the payload dimension as
# unaddressed. That is the shape this amendment removes: it stops patching band 1 one
# dimension at a time and states ONE RULE that covers every dimension at once.
#
# THE RULE, and it is deliberately restateable without reference to any individual run.
# Each direct-cell group's P50 ceiling is that group's OWN median P50 multiplied by a
# single multiplier held constant across every group, then rounded DOWN to the nearest
# 0.5ms. The P99 advisory is the same rule on the same group's own median P99 with its
# own single multiplier. Two multipliers in total, one per quantile, and no group has a
# multiplier of its own.
#
# THE MULTIPLIER IS NOT A FREE PARAMETER, which is what keeps this from being a
# widening. It is fixed by the one band 1 threshold this harness has always had, the
# 1.0ms non-streaming 150-byte P50 ceiling, divided by that group's own median. So the
# anchor group's product is 1.0ms exactly, the round-down is a no-op on it, its ceiling
# is unchanged by construction, and every other group is set at the same relative
# strictness that the original ceiling expressed. The advisory multiplier is fixed the
# same way from the 2.5ms non-streaming advisory.
#
#   P50 multiplier   1.0 / 0.288 = 3.47
#   P99 multiplier   2.5 / 0.752 = 3.32
#
# The two were set independently, amendments apart, and they land within 5 percent of
# each other. That is a coherence check rather than an argument, and it is recorded as
# one.
#
# ONE ADDITION TO THE ROUNDING RULE, forced by measurement during this amendment and
# recorded as forced rather than presented as foresight. Where a product straddles a
# 0.5ms boundary across the data cuts available, the threshold takes the LOWEST bin any
# cut produces. Without that clause the streaming P99 advisory would have been 6.0ms on
# the cut this amendment was derived from and 5.5ms on the cut that existed forty
# minutes later, after its own verification run added five readings. A threshold that
# depends on which hour the data was pooled is not a calibration, and resolving the
# straddle downward is the only direction that cannot be mistaken for fitting a
# threshold to make something pass.
#
# THIS RULE SUBSUMES THE AMENDMENT ABOVE RATHER THAN CONTRADICTING IT. That one set the
# streaming P50 ceiling to the non-streaming ceiling times the measured MODE RATIO,
# rounded down. Substitute GROUP RATIO for mode ratio and the two rules are the same
# rule, because a group ratio is that group's median over the anchor's median and
# 1.0 times that ratio is exactly the group's median times the multiplier. Applied to
# the streaming group it reproduces 2.0ms to three decimal places. So the streaming P50
# gate set yesterday is CONFIRMED by the general rule rather than replaced by it, which
# is the strongest evidence available that the generalisation is the right one.
#
# THE CALIBRATION CUT IS FROZEN AT 13 LOADABLE DIRECTORIES and named here, which is the
# whole point of the provenance rule this amendment also writes down. A later run is
# judged AGAINST the frozen cut and its readings are recorded, never silently folded back
# in to move the thresholds, because a threshold that re-derives itself on every run is
# not pre-registered. The amendment above did not freeze its cut and went stale within
# minutes of its own verification run.
#
# THE EVIDENCE, recomputed from the committed steady-only CSVs across the frozen cut. Six
# early directories are aborted runs that never wrote a filtered CSV and are absent for
# that reason:
#
#   group                  n   P50 median   P50 min   P50 max   P99 median   P99 max
#   non-streaming   150B   26       0.288     0.255     0.506        0.752     1.611
#   non-streaming  4096B   10       0.332     0.307     0.546        0.875     1.419
#   non-streaming 32768B   13       0.481     0.278     0.899        1.130     2.835
#   streaming       150B   15       0.643     0.523     1.341        1.804     2.606
#
# THAT TABLE IS TWO DIRECTORIES LARGER THAN THE ONE IN THE AMENDMENT ABOVE, and the
# difference is named rather than left for a reader to trip over, because it is the same
# staleness trap that amendment fell into. It pooled 11 directories and read n of 22, 8,
# 11 and 13. The twelfth is 2026-09-17-4d3f224-dirty-m3pro-macos-quick-r1, that
# amendment's OWN verification run, written minutes after its table was computed and
# therefore absent from it. The thirteenth is
# 2026-09-17-16883b9-dirty-m3pro-macos-quick-r1, THIS amendment's verification run. Five
# direct cells each, which is the whole difference.
#
# THE FOUR P50 CEILINGS ARE IDENTICAL ON ALL THREE CUTS, which was checked rather than
# assumed. Products, and the bin each rounds down into:
#
#   cut       multiplier   ns 150B      ns 4096B     ns 32768B    stream 150B
#   11 dirs        3.413   1.000 -> 1.0  1.140 -> 1.0  1.655 -> 1.5  2.212 -> 2.0
#   12 dirs        3.436   1.000 -> 1.0  1.148 -> 1.0  1.643 -> 1.5  2.216 -> 2.0
#   13 dirs        3.472   1.000 -> 1.0  1.151 -> 1.0  1.670 -> 1.5  2.233 -> 2.0
#
# AND THE ORDER OF EVENTS MATTERS, so it is stated. The four ceilings were fixed from
# the 12-directory cut BEFORE the verification run existed. That run then contributed
# five more direct readings and the 13-directory cut reproduces all four bins. It is a
# small out-of-sample confirmation, one run wide, and it is the only one available.
#
# THE ADVISORIES ARE MEASURABLY LESS STABLE THAN THE GATES, and pretending otherwise is
# how the streaming advisory would have been wrong within the hour:
#
#   cut       multiplier   ns 150B      ns 4096B     ns 32768B    stream 150B
#   11 dirs        3.472   2.500 -> 2.5  2.969 -> 2.5  4.142 -> 4.0  6.306 -> 6.0
#   12 dirs        3.324   2.500 -> 2.5  2.882 -> 2.5  3.860 -> 3.5  6.017 -> 6.0
#   13 dirs        3.324   2.500 -> 2.5  2.909 -> 2.5  3.757 -> 3.5  5.997 -> 5.5
#
# TWO OF THE FOUR STRADDLE A BIN EDGE and the lowest-bin clause resolves both, giving
# 2.5, 2.5, 3.5 and 5.5. The diagnosis is worth recording because the two straddles have
# DIFFERENT causes. The 32768B one is genuine estimator movement: its P99 median walked
# 1.193 to 1.161 to 1.130 across the cuts, 5.3 percent, while its P50 median moved only
# 1.4 percent over the same three cuts. The streaming one is not its own movement at
# all, since its P99 median walked just 0.7 percent. It is the ANCHOR moving: the
# anchor's P99 median went 0.720 to 0.752, 4.4 percent, which changed the multiplier
# every advisory is built from. That is the structural reason the tail cannot be pinned
# as tightly as the centre, a P99 median is estimated from far fewer effective samples
# than a P50 median, and it applies with extra force to the anchor because the anchor
# scales all four.
#
# THE RESOLUTION IS POST HOC AND IS LABELLED POST HOC. The lowest-bin clause was written
# after seeing the 13-directory cut. It is defensible for two reasons and neither is
# foresight: the P99 is an advisory that never blocks publication, and taking the lowest
# bin is the strictest choice available, which is the opposite direction from fitting a
# threshold to rescue a run.
#
# ALL EIGHT THRESHOLDS THEN SURVIVED A TWO-RUN OUT-OF-SAMPLE CHECK, which is what the
# frozen cut exists to make possible. Two further quick runs were measured after the
# thresholds were fixed, 2026-09-17-16883b9-dirty-m3pro-macos-quick-r2 and -r3, adding 10
# direct readings. Every one passes its gate with 3.12x to 3.70x fold headroom and not
# one fires its advisory, the closest being the 4096B tail at 2.89x. Pooling them into a
# 15-directory cut moves no product across a bin edge: the P50 products become 1.000,
# 1.147, 1.663 and 2.105 and the P99 products 2.500, 2.881, 3.757 and 5.811, which round
# into the same 1.0, 1.0, 1.5, 2.0 and 2.5, 2.5, 3.5, 5.5. The thresholds are NOT
# re-derived on that cut, deliberately. It is a check, and the frozen cut stays frozen.
#
# THE STREAMING P50 PRODUCT IS THE ONE THAT MOVED MOST, from 2.233 to 2.105, and that is
# named because it is the closest thing here to a warning. It moved 5.7 percent on two
# added readings, both of them low, and its bin edge is at 2.000. So a run of quiet
# streaming cells pushes this product DOWN toward the edge, and if it ever crosses, the
# rule as stated would put the streaming ceiling at 1.5ms rather than 2.0ms. That would
# be a TIGHTENING driven by the host being quiet, which is a perverse direction, and the
# response then is to say so and keep 2.0ms rather than to follow the arithmetic off a
# cliff. The rule sizes a ceiling from a central value, it does not license ratcheting one
# down every time the machine has a good day.
#
# THE RESULT. margin is the threshold over the LARGEST reading that group has ever
# produced on this host, so it is the headroom a fifth evidence run actually has. fold
# is the threshold over that group's own median, so it is the uniform slowdown that
# trips it:
#
#   group                    n   median   product   CEILING     fold   margin over max
#   non-streaming   150B    26    0.288     1.000     1.0ms    3.47x    1.98x of 0.506
#   non-streaming  4096B    10    0.332     1.151     1.0ms    3.02x    1.83x of 0.546
#   non-streaming 32768B    13    0.481     1.670     1.5ms    3.12x    1.67x of 0.899
#   streaming       150B    15    0.643     2.233     2.0ms    3.11x    1.49x of 1.341
#
#   group                    n   median   product  ADVISORY     fold   margin over max
#   non-streaming   150B    26    0.752     2.500     2.5ms    3.32x    1.55x of 1.611
#   non-streaming  4096B    10    0.875     2.909     2.5ms    2.86x    1.76x of 1.419
#   non-streaming 32768B    13    1.130     3.757     3.5ms    3.10x    1.23x of 2.835
#   streaming       150B    15    1.804     5.997     5.5ms    3.05x    2.11x of 2.606
#
# THE POINT OF THE UNIFORM MULTIPLIER is that no group was given a licence. Realised P50
# strictness now spans 3.02x to 3.47x, a 15 percent disagreement, and every group sits
# AT OR STRICTER than the anchor because rounding down can only reduce a ceiling. Before
# this amendment the same span was 2.08x to 3.47x, a 67 percent disagreement:
# direct-payload-32768 carried a 1.0ms ceiling against a 0.481ms median, refusing at
# 2.08 times its own central value, while the anchor refused at 3.47 times its own.
#
# AND NO GROUP IS LEFT AT A CLIFF, which is the failure this amendment exists to
# prevent. The tightest P50 margin is the streaming group's 1.49x, which was accepted
# deliberately in the amendment above, and nothing is below it. direct-payload-32768
# goes from 1.11x to 1.67x, so the cell that would most likely have refused a fifth
# evidence run no longer sits one noise burst from its ceiling. The advisory margins are
# looser as a set and one of them, the 32768B group's 1.23x, is below the accepted
# streaming floor. That is not held to the same standard on purpose: an advisory FIRING
# is its correct behaviour rather than a lost run, so a small margin there costs a line
# of output and nothing else.
#
# EVERY CEILING STILL DETECTS A GENUINE BOTTLENECK, stated per group because a ceiling
# that cannot is not worth having. Two readings of each: the uniform slowdown that trips
# it, and the rise in the term UNIQUE to that group with the shared base term held
# fixed, where the base is the 0.288ms one-write 150-byte round trip:
#
#   - non-streaming 150B, 1.0ms. Trips on a 3.47-fold uniform slowdown. This group IS
#     the base term, so it has no unique term and this is the whole of what it sees.
#   - non-streaming 4096B, 1.0ms. Trips on a 3.02-fold uniform slowdown, the strictest
#     of the four. Its unique term, the payload-proportional cost above the base, is
#     only 44us, so tripping on that term alone needs a 16.4-fold rise in it. Said
#     plainly: this cell is a second reading of the base cost rather than a sensitive
#     probe of 4KB handling, and its value is the uniform-slowdown arm.
#   - non-streaming 32768B, 1.5ms. Trips on a 3.12-fold uniform slowdown. Its unique
#     term is 193us of payload-proportional work, which would have to reach 1212us, a
#     6.3-fold rise. That is the one failure only this cell can see, a mock or a
#     loopback that has become bad at moving 32KB.
#   - streaming 150B, 2.0ms. Trips on a 3.11-fold uniform slowdown. Its unique term is
#     71us per extra SSE segment, which would have to reach 342us, a 4.8-fold rise.
#
# None of these is a subtle regression, and that is said out loud rather than implied.
# This band has never been a sensitivity instrument. It answers one question, whether
# the floor is so high that no proxied number in the run means anything.
#
# WHAT ACTUALLY MOVED, so a reader can audit the diff against the claim. Of the eight
# thresholds, five are unchanged: both 150B P50 ceilings, the 4096B P50 ceiling, and the
# 150B and 4096B non-streaming advisories. Three move:
#
#   threshold                              before    after   why
#   non-streaming 32768B P50 ceiling         1.0ms    1.5ms   the rule, first calibration
#   non-streaming 32768B P99 advisory        2.5ms    3.5ms   the rule, first calibration
#   streaming 150B P99 advisory              5.0ms    5.5ms   ad hoc rounding replaced
#
# THE STREAMING ADVISORY MOVING IS A ROUNDING CORRECTION AND NOT A NEW JUDGEMENT, and it
# is the one number here that a skeptic should press on, because it is the only widening
# not driven by a first calibration. The amendment above computed 2.5 times a 2.52 mode
# ratio, got 6.30ms, and rounded that DOWN TO 5.0 in order to make the streaming arm
# exactly double the non-streaming one. Exactly-double was a taste, not a rule: rounding
# 6.30 down to the nearest whole millisecond gives 6.0, and to the nearest half also
# 6.0, so 5.0 was reachable only by choosing the answer first. Under the stated rule the
# product is 5.997 and the bin is 5.5. The observable consequence of the move is NIL and
# that was checked: the largest streaming P99 anywhere in this tree is 2.606ms, so 5.0
# and 5.5 both fire on zero of 15 readings.
#
# THE ONLY OUTPUT THAT CHANGES ON ANY EXISTING DIRECTORY is the 32768B advisory. Across
# every directory in the tree, and all 64 direct readings in the frozen cut, no
# P50 gate outcome changes at any of the four groups, and the one advisory line that
# disappears is 2026-09-16-31918d9-dirty-m3pro-macos-quick-r1 at 2.835ms, the only
# non-streaming reading ever to fire it. No directory's VALID or INVALID verdict changes,
# because an advisory never blocks publication. That was established by running the
# pre-amendment and post-amendment checkers over every directory and comparing both the
# exit code and the set of BAND1 lines, not by reasoning about the thresholds.
#
# BOTH RAISED ADVISORIES STILL FIRE ON THE CLASS OF BURST THEY EXIST TO NAME, which is
# the test a raised advisory has to survive. The band 5 calibration table below records
# non-streaming closing canaries reaching 4.080 and 3.301ms, which are 5.43 and 4.39
# times the non-streaming P99 median. A burst of that relative size reads 6.1 and 5.0ms
# on the 32768B group against its 3.5ms advisory, and 9.8 and 7.9ms on the streaming
# group against its 5.5ms advisory. All four exceed their threshold.
#
# THIS SUPERSEDES TWO PARAGRAPHS OF THE AMENDMENT ABOVE and they are left standing
# rather than edited, because an amendment record that gets rewritten is not a record.
# Its closing section, THE TIGHTEST GATE LEFT IN BAND 1, states that
# direct-payload-32768 keeps 1.11-fold margin and that widening it would need a
# calibration this tree cannot supply. The margin is now 1.67-fold. The claim about
# calibration was the part that was wrong: the tree supplies 12 readings of that cell,
# and what was missing was not data but a rule for turning a group's own readings into
# its own ceiling. Its WHAT THIS SPLIT DOES NOT FIX section names the per-payload
# advisory miscalibration and declines to fix it, correctly, on the ground that bundling
# it into the mode split would repeat the bundling mistake that amendment was about.
# This amendment is that fix, unbundled, which is the form that objection asked for.
#
# BAND 1 NOW READS contended-cells.txt, AND ONLY TO ANNOTATE A FAILURE. This is a
# reversal of the sentence in that same closing section, which recorded band 1 as the
# only band that does not honour the ledger, so the reason has to be exact. What that
# sentence got RIGHT is the substantive part and it still stands: contention must never
# make a band 1 failure disappear. A floor measured on a busy machine is still the floor
# that run's proxied numbers sit on, and three of the four direct groups are
# single-instance cells with no second repetition to be dropped in favour of, so the
# repetition-dropping mechanism every other band uses cannot rescue them even in
# principle. What it got WRONG is treating "must not excuse" as a reason not to READ the
# file. The fourth evidence attempt recorded a real breach on
# direct-canary-close-nonstream-150 at 56.39 percent idle against a 60 percent floor, a
# cell that band 5 reads as its closing canary and band 2 reads as its baseline. Had
# that cell failed band 1 rather than a streaming cell, the message would have named a
# bottleneck and said nothing about the recorded breach, and a reader would have had to
# cross-reference two files by hand to tell a genuine floor problem from a contended
# sample. So the ledger is now read, a failing cell named in it is ANNOTATED with its
# recorded breach, and the verdict is identical either way. There is deliberately no
# code path by which contention turns a band 1 failure into a pass. A passing cell is
# not annotated at all, because report_contention above already prints the whole ledger
# before this band runs and repeating it on a pass would be noise.
#
# WHAT IS STILL THIN, recorded rather than closed:
#
#   - ALL FOUR CALIBRATIONS COME FROM ONE HOST, and three of the four rest on 10 to 15
#     readings of which all but one run is quick-mode. An evidence run loads the box for
#     52 minutes and a quick run does not, and the fourth attempt showed streaming cells
#     elevated 1.98-fold over quick while non-streaming 150B cells rose only 1.61-fold.
#     A second reference host would change these numbers.
#   - THE STREAMING GROUP KEEPS THE TIGHTEST MARGIN AT 1.49x. That was accepted
#     deliberately in the amendment above and the reasoning is unchanged: calibrating on
#     the evidence cut alone would give 2.6ms, and it rests on 3 readings from the run
#     the band has to judge, which is fitting a band to its own subject.
#   - 4096B IS THE STRICTEST GROUP IN FOLD TERMS at 3.02x, purely because 1.151 rounds
#     down to 1.0. Rounding down is the conservative direction by construction, so this
#     is accepted rather than corrected, but it is the group most likely to be the next
#     one to fire, and its own margin over its worst reading is 1.83x.
#   - THE ANCHOR IS A SINGLE POINT OF FAILURE FOR ALL EIGHT THRESHOLDS. Every one of
#     them is the anchor group's threshold scaled by a ratio, so a mistake in the
#     anchor's own median propagates everywhere at once. Its P50 median is stable across
#     the three cuts to 1.7 percent, which is what licenses the gates. Its P99 median
#     moved 4.4 percent, which is why the advisories needed the lowest-bin clause.
#   - AN UNCALIBRATED GROUP FAILS THE BAND with a named cause rather than borrowing a
#     neighbour's ceiling, which is the same category error this amendment removes. It
#     can only fire if the matrix gains a direct cell at a new mode or payload, which is
#     a matrix change and has to arrive with its own calibration.
#
# NOT RETROACTIVE, on the same terms as the amendment above.
# 2026-09-17-4d3f224-m3pro-macos-evidence-r1 passes the amended band and STAYS INVALID,
# and its committed bands.txt keeps the FAIL line and the INVALID verdict it was judged
# under. The amended form binds the NEXT run.

# One calibrated pair per direct-cell group, keyed by (stream, prompt_bytes). Both
# numbers in each pair are that group's own median times the single multiplier for that
# quantile, rounded down to the nearest 0.5ms. The derivation, the multipliers and the
# per-group evidence are in the third amendment above. Insertion order is the reporting
# order, chosen so a reader walks the non-streaming payload ladder before crossing into
# streaming.
BAND1_DIRECT_GROUP_THRESHOLDS: dict[tuple[bool, int], tuple[float, float]] = {
    (False, 150): (1.0, 2.5),
    (False, 4096): (1.0, 2.5),
    (False, 32768): (1.5, 3.5),
    (True, 150): (2.0, 5.5),
}

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
# throughput catches the condition directly in both shapes.
#
# IT DOES NOT SUBSUME IT COMPLETELY, REVISITED 2026-09-17 when the drop gate stopped
# being an absolute zero and the question of whether to keep it at all had to be
# answered. Drops subtract from completions one for one, measured exactly on two
# recorded cells, so any cell dropping more than this gate's 2 percent already fails
# HERE and a drop tolerance at or above 2 percent would add nothing. At 1 percent it
# adds something the rate gate structurally cannot see: a drop proves the VU pool had no
# free slot at a scheduled arrival, so the pool was momentarily part of what the cell
# measured, and a brief exhaustion of that kind costs hundredths of a percent of
# throughput and is invisible at a 2 percent floor. The two gates fail in opposite blind
# spots and both are kept, each with a tolerance sized to its own measured envelope. See
# STEADY_DROP_TOLERANCE_BASIS_POINTS above.
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
#
# Measured across every cell in this repository's results tree that records a k6 steady
# count, 96 of them: the divergence is EXACTLY ZERO in all 96. So this 1 percent is not
# a tolerance the data is straining against, it is a guard against a future filter or
# CSV-format change, and it is satisfiable with unbounded margin.
RATE_CROSSCHECK_TOLERANCE_FRACTION = 0.01

# THE TWO INTEGRITY TOLERANCES, ADDED 2026-09-17, MIRRORING run.sh.
#
# These four numbers replaced the absolute-zero k6 thresholds
# dropped_iterations{scenario:steady}: count==0 and http_req_failed{scenario:steady}:
# rate==0. The full derivation, the calibration table of every nonzero drop count this
# repository has ever recorded, and the arithmetic that made an absolute zero
# unsatisfiable across a 53-cell matrix all live at STEADY_DROP_TOLERANCE_BASIS_POINTS
# in benchmarks/harness/run.sh. It is not duplicated here. What is duplicated is the
# ARITHMETIC, and that duplication is deliberate and has to stay exact.
#
# WHY check_bands RE-DERIVES THE ALLOWANCE RATHER THAN ONLY READING THE RECORDED ONE.
# A committed directory is supposed to be judgeable by a stranger with this file and
# nothing else, including a directory written before the allowance was recorded at all.
# So the allowance is recomputed from the cell's own rate and steady seconds, and the
# recorded value is used only to cross check that k6 and this file agree.
#
# WHY BASIS POINTS AND FLOOR DIVISION. run.sh computes demanded times points over 10000
# in shell integer arithmetic. A float multiply here, demanded times 0.01, can land a
# hair either side of an integer boundary and would let one cell be judged 300 by k6 and
# 299 here. Integer floor division reproduces the shell exactly.
STEADY_DROP_TOLERANCE_BASIS_POINTS = 100
STEADY_DROP_TOLERANCE_FLOOR = 25
STEADY_FAILED_TOLERANCE_BASIS_POINTS = 5
STEADY_FAILED_TOLERANCE_FLOOR = 5


def tolerance_from_basis_points(demanded: int, points: int, floor: int) -> int:
    """Return the integer allowance for one cell, identical to run.sh's arithmetic.

    One basis point is one ten-thousandth. Floor division rather than a float
    multiply, so this and the shell can never differ by a rounding step.
    """
    return max(floor, (int(demanded) * points) // 10000)

# The contended-repetition exclusion, ADDED 2026-09-16 alongside the sustained-breach
# amendment in run.sh.
#
# WHAT run.sh NOW HANDS OVER. A mid-run host CPU idle reading below the floor no
# longer aborts an evidence run unless the breach is SUSTAINED, meaning two adjacent
# readings or more than a tenth of all of them. The reason is arithmetic: an evidence
# run takes 106 of those readings, ambient single samples on the reference host reach
# down to 59.28 against a floor of 60, and the one evidence-scale sample of the
# confirmed breach rate is 1 in 79. So at least one dip per run is close to
# inevitable, and the strict form aborted the first evidence attempt at 51 minutes 54
# seconds on cell 40 of 53. A gate nobody can satisfy gets deleted.
#
# An isolated dip is now RECORDED instead: the reading carries cpu_idle_breach=yes in
# machine-state.txt and the cell gets a line in contended-cells.txt.
#
# WHY THAT RECORD HAS TO CHANGE ARITHMETIC HERE RATHER THAN ONLY BE PRINTED. A cell
# measured during a dip has suspect numbers, and the contention that invalidated the
# first completed evidence run PASSED every band in this file. So a marking that only
# printed would leave the contaminated repetition inside every median while assuring
# the reader it was contaminated, which is precisely the failure mode
# pre-registration exists to prevent. Every median across repetitions therefore drops
# the contended repetitions before computing.
#
# WHY IT IS AFFORDABLE. THIS IS WHAT THE FIVE-REPETITION DESIGN IS FOR. The published
# quantity is the median of five per-repetition shifts, so it can afford to lose one
# and still be a median of four. Three is the floor: below three, a median stops
# being an order statistic over independent measurements and becomes a single reading
# wearing the word median. Two would make the median an average of the only two
# values left, and one would make it that value.
#
# WHY A RUN WITH FEWER THAN THREE CLEAN REPETITIONS FAILS RATHER THAN WIDENS. The
# alternative would be to keep computing over what is left and note the weakness,
# which is the same move as widening a band to rescue a run. A gated number computed
# from two repetitions is not the pre-registered quantity.
MINIMUM_CLEAN_REPETITIONS = 3

# STREAM_MINIMUM_CLEAN_REPETITIONS, ADDED 2026-09-17, and it is a RELAXATION of the rule
# above for BAND3-STREAM alone. Recorded in the amendment style because the block above
# explicitly pre-rejected this move, saying the remedy was more streaming repetitions
# "never a lower minimum here". That position is overridden here, for a reason the block
# above did not weigh: the same aggregation arithmetic that made count==0 and rate==0
# unsatisfiable makes a three-of-three requirement unsatisfiable too.
#
# THE ARITHMETIC. The streaming matrix runs THREE repetitions where the non-streaming one
# runs five, so with a minimum of three it permits ZERO contended streaming repetitions.
# The pairing spans 6 cells, passthrough-stream and enforce-stream at 3 repetitions, and
# each cell takes 2 host idle readings, so 12 readings must ALL come in clean. Pooling the
# two evidence attempts that carry idle readings gives 1 confirmed breach in 155, so the
# chance that one of those 12 breaches is 7.5 percent, and at the 1-in-79 rate of the
# attempt that actually breached it is 14.2 percent. That failure lands in check_bands,
# which runs after the LAST cell, so it costs the entire 52 minutes rather than stopping
# where it happened. One transient dip anywhere near a streaming cell, and the run is
# gone.
#
# The non-streaming bands are NOT affected and their minimum is unchanged at three. They
# have five repetitions, so they tolerate two contended ones, and the chance of three
# distinct contended repetitions among their 20 readings is roughly 0.03 percent.
#
# WHY THE ORDER-STATISTIC ARGUMENT DOES NOT BIND HERE. It binds on a gate whose window is
# comparable to the quantity, because there a median of two is a coin flip dressed as a
# statistic. BAND3-STREAM is not that gate. Its window is a two-sided 0.60ms ceiling on
# the ABSOLUTE SIZE of a shift whose inherent value is 15us, so it fires only on roughly a
# 40-fold regression or on an enforce arm that was not enforcing. Both of those are
# visible in one repetition. And its CENTRAL VALUE is already advisory-only: the band's own
# note says the reading is drift-dominated and MUST NOT be published as the streaming
# enforcement cost. So nothing published is computed from this median, and there is no
# published number for a shorter median to weaken.
#
# WHAT IS LOST, said plainly. With one or two clean repetitions the printed median is a
# worse estimate of the streaming shift than a median of three would be. That is why the
# band prints a loud advisory saying exactly that whenever it runs below
# MINIMUM_CLEAN_REPETITIONS, rather than quietly reporting a thinner number. Zero clean
# repetitions is still UNEVALUABLE and still fails the run, because then there is nothing
# to apply the ceiling to.
#
# THE BETTER FIX IS STILL THE ONE THE OLD NOTE NAMED, five streaming repetitions in the
# matrix. It costs 6 more cells and roughly 7 minutes of a 52 minute run, and it changes
# the pre-registered cell count from 53 to 59. That is a matrix decision rather than a gate
# decision, so it is not taken here.
STREAM_MINIMUM_CLEAN_REPETITIONS = 1

# The ledger run.sh writes. Its ABSENCE and its EMPTINESS mean different things: an
# absent file is a directory that predates the marking, while a present and empty one
# is a positive statement that no reading breached the floor.
CONTENDED_CELLS_FILENAME = "contended-cells.txt"

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
    failed_steady: int = 0
    failed_warmup: int = 0
    request_count: int = 0
    thresholds: dict = field(default_factory=dict)
    steady_seconds: float = 0.0
    reported_steady_requests: int | None = None
    reported_demanded_requests: int | None = None
    reported_drop_allowance: int | None = None
    reported_failed_allowance: int | None = None
    cpu_seconds: float | None = None
    cpu_milliseconds_per_request: float | None = None

    @property
    def demanded_steady_requests(self) -> int | None:
        """The steady iterations this cell was asked to schedule.

        Recomputed from the demanded rate and the steady window rather than taken
        from the summary, so a directory written before the field existed is still
        judgeable. None when neither is recoverable, which the caller reports as an
        unknown rather than as a passing zero.
        """
        if self.rate <= 0 or self.steady_seconds <= 0:
            return None
        return int(round(self.rate * self.steady_seconds))

    @property
    def drop_allowance(self) -> int:
        """The steady dropped-iteration allowance, identical to the one k6 used."""
        demanded = self.demanded_steady_requests
        if demanded is None:
            # No recoverable demand means the fractional part cannot be computed, so
            # the cell falls back to the absolute floor alone. That is the strictest
            # of the two terms, which is the correct direction when the harness cannot
            # tell how large the window was.
            return STEADY_DROP_TOLERANCE_FLOOR
        return tolerance_from_basis_points(
            demanded, STEADY_DROP_TOLERANCE_BASIS_POINTS, STEADY_DROP_TOLERANCE_FLOOR
        )

    @property
    def failed_allowance(self) -> int:
        """The steady failed-request allowance, identical to the one k6 used."""
        demanded = self.demanded_steady_requests
        if demanded is None:
            return STEADY_FAILED_TOLERANCE_FLOOR
        return tolerance_from_basis_points(
            demanded, STEADY_FAILED_TOLERANCE_BASIS_POINTS, STEADY_FAILED_TOLERANCE_FLOOR
        )

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


def optional_int(summary: dict, key: str) -> int | None:
    """Return one summary field as an int, or None when it is absent.

    Absent means the directory predates the field. None rather than zero, because a
    recorded allowance of zero and no recorded allowance at all are different facts
    and the cross check below reports them differently.
    """
    raw = summary.get(key)
    if isinstance(raw, bool) or not isinstance(raw, (int, float)):
        return None
    return int(raw)


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


@dataclass
class Contention:
    """Which cells run.sh measured while the host was below its CPU idle floor.

    recorded says whether the ledger file existed at all, which separates "this run
    measured no contention" from "this run could not have told you either way".
    """

    recorded: bool
    notes: dict[str, list[str]] = field(default_factory=dict)

    def contended(self, cell: Cell) -> bool:
        return cell.name in self.notes


def read_contention(results_dir: str) -> Contention:
    """Parse contended-cells.txt into the set of cells measured during a dip.

    Comment lines, which run.sh uses for the format header and the end-of-run
    summary, start with a hash and are skipped. A malformed line is skipped rather
    than raised on: this file is a record and a parse failure inside it must not be
    able to stop a run whose measurements are fine.
    """
    path = os.path.join(results_dir, CONTENDED_CELLS_FILENAME)
    if not os.path.exists(path):
        return Contention(recorded=False)
    notes: dict[str, list[str]] = {}
    with open(path, encoding="utf-8") as handle:
        for line in handle:
            stripped = line.strip()
            if not stripped or stripped.startswith("#"):
                continue
            values: dict[str, str] = {}
            for field_text in stripped.split():
                key, separator, value = field_text.partition("=")
                if separator:
                    values[key] = value
            name = values.get("cell")
            if not name:
                continue
            notes.setdefault(name, []).append(
                "{} {} at idle {}".format(
                    values.get("phase", "unknown-phase"),
                    values.get("reason", "unknown-reason"),
                    values.get("cpu_idle_pct", "unknown"),
                )
            )
    return Contention(recorded=True, notes=notes)


@dataclass
class RepetitionSet:
    """The repetition ordinals one paired band may compute its median over."""

    present: list[int]
    dropped: list[int]
    kept: list[int]
    evaluable: bool
    cause: str
    note: str


def usable_repetitions(
    groups: list[list[Cell]],
    contention: Contention,
    minimum: int = MINIMUM_CLEAN_REPETITIONS,
) -> RepetitionSet:
    """Decide which repetitions a paired band may use after dropping contended ones.

    minimum is the count of clean repetitions the calling band needs. It defaults to
    MINIMUM_CLEAN_REPETITIONS, which is what every gate on a published median uses.
    BAND3-STREAM passes STREAM_MINIMUM_CLEAN_REPETITIONS instead, argued in full at that
    constant: three repetitions with a minimum of three permits zero contended ones,
    which is the same absolute-zero-across-many-opportunities defect the integrity
    tolerances above were amended to remove.

    A repetition is dropped when ANY arm of the comparison was contended, because the
    published quantity is a within-pair shift and one contaminated arm contaminates
    the shift.

    The scope is ONE PAIRING, which is why every caller passes only its own arms. A
    repetition ordinal is a join key inside a pairing, not a moment in time: the 150B
    cells of repetition 4 and the 4096B cells of repetition 4 ran minutes apart, so a
    dip during one says nothing about the other. Dropping r4 everywhere because one
    4096B cell dipped would discard measurements that were taken on a quiet host.

    A cell with NO repetition ordinal is never dropped. That is the SINGLE-INSTANCE
    carve-out, and it is deliberate rather than an oversight: the two drift canaries
    and the two direct payload cells run once per matrix, so there is nothing to drop
    them in favour of, and dropping them would leave the band with no cells at all.
    The reason it is safe to leave them in is that the bands reading them already
    tolerate a contended host. Band 5 measures canary drift DIRECTLY and gates it at
    0.25ms of P50 movement, and band 1 judges each of them against its OWN group's
    ceiling, 1.0ms for the two 150-byte canaries and 1.5ms for the 32768-byte cell,
    against an observed range of 0.363 to 0.899ms on the most contended host in this
    repository's results tree, which breached on 5 of its 26 readings. Both passed
    there with room to spare. So the contention is recorded for those cells and no new
    failure path is added for them. Band 1 reads the ledger as of 2026-09-17, but only
    to ANNOTATE a failure it has already decided on, never to excuse one.
    """
    ordinals: set[int] = set()
    dropped: set[int] = set()
    for group in groups:
        for cell in group:
            if cell.repetition is None:
                continue
            ordinals.add(cell.repetition)
            if contention.contended(cell):
                dropped.add(cell.repetition)

    present = sorted(ordinals)
    dropped_list = sorted(dropped)
    kept = sorted(ordinals - dropped)

    if not dropped_list:
        return RepetitionSet(present, dropped_list, kept, True, "", "")

    note = (
        f"{len(dropped_list)} of {len(present)} repetitions dropped for host "
        "contention, "
        + ", ".join(f"r{ordinal}" for ordinal in dropped_list)
        + f", leaving {len(kept)} clean"
    )

    if len(present) >= minimum:
        if len(kept) >= minimum:
            return RepetitionSet(present, dropped_list, kept, True, "", note)
        cause = (
            f"only {len(kept)} of {len(present)} repetitions were measured on a quiet "
            f"host and this band needs at least {minimum}. The "
            "dropped repetitions are "
            + ", ".join(f"r{ordinal}" for ordinal in dropped_list)
            + ", each named in contended-cells.txt with the phase and the idle reading "
            "that disqualified it. THE CAUSE IS HOST CONTENTION AND NOT LEVEE: the "
            "host CPU idle reading fell below its floor while those cells were being "
            "measured, and a repetition measured during a dip cannot go into a median "
            f"that gets published. Below {minimum} the median stops being an order "
            "statistic over independent measurements, so the honest outcome is an "
            "invalid run rather than a narrower median. Rerun on a quiet machine"
        )
        return RepetitionSet(present, dropped_list, kept, False, cause, note)

    # Fewer repetitions than the minimum in the first place, which is quick mode with
    # one. There is nothing to fall back to, so an exclusion here empties the band
    # rather than narrowing it.
    if kept:
        return RepetitionSet(present, dropped_list, kept, True, "", note)
    cause = (
        f"every one of its {len(present)} repetitions was measured during a host "
        "contention dip, so after the exclusion there is nothing left to take a median "
        "of and this band is UNEVALUABLE rather than passing. A run with this few "
        "repetitions has no spare measurement to fall back on, which is why evidence "
        "mode runs five. The contended cells are named in contended-cells.txt"
    )
    return RepetitionSet(present, dropped_list, kept, False, cause, note)


def keep_clean(cells: list[Cell], usable: RepetitionSet) -> list[Cell]:
    """Filter one arm down to the repetitions the band may use."""
    return [
        cell for cell in cells if cell.repetition is None or cell.repetition in usable.kept
    ]


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
        # The whole-invocation failure count is the fallback, because directories
        # written before 2026-09-17 carry only that. It reads zero everywhere in this
        # repository's tree, so the fallback is exact for every one of them rather
        # than merely safe.
        failed_total = int(summary.get("http_req_failed_count", 0))
        failed_steady = int(summary.get("http_req_failed_steady_count", failed_total))
        failed_warmup = int(summary.get("http_req_failed_warmup_count", 0))
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
                failed_count=failed_total,
                failed_steady=failed_steady,
                failed_warmup=failed_warmup,
                request_count=int(summary.get("request_count", 0)),
                thresholds=summary.get("thresholds", {}) or {},
                steady_seconds=parse_steady_seconds(summary),
                reported_steady_requests=(
                    int(reported_steady) if isinstance(reported_steady, (int, float)) else None
                ),
                reported_demanded_requests=optional_int(summary, "demanded_steady_requests"),
                reported_drop_allowance=optional_int(
                    summary, "max_steady_dropped_iterations"
                ),
                reported_failed_allowance=optional_int(
                    summary, "max_steady_failed_requests"
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


def report_contention(report: Report, cells: list[Cell], contention: Contention) -> None:
    """State which cells were measured during a host CPU idle dip, and their fate.

    A REPORT rather than a gate. The gate is inside each band, which drops the
    contended repetitions before computing its median and fails when too few remain.
    This section exists so the exclusion is visible as a list rather than only
    inferable from a repetition ordinal missing out of a per-repetition breakdown.
    """
    if not contention.recorded:
        report.line(
            f"CONTENTION not recorded, this directory has no {CONTENDED_CELLS_FILENAME} "
            "and therefore predates the per-cell contention marking. Every band below "
            "computes over ALL of its repetitions, including any that were measured "
            "during a host CPU idle dip, so read cpu_idle_pct in machine-state.txt by "
            "hand before trusting a median from this run"
        )
        report.line()
        return

    if not contention.notes:
        report.line(
            "CONTENTION none, every host CPU idle reading in this run sat at or above "
            "its floor, so no repetition was excluded from any median below. This is a "
            "positive statement rather than an absent file: the ledger exists and is "
            "empty"
        )
        report.line()
        return

    known = {cell.name for cell in cells}
    repetition_bearing = sorted(
        name for name in contention.notes if REPETITION_PATTERN.search(name)
    )
    single_instance = sorted(
        name
        for name in contention.notes
        if not REPETITION_PATTERN.search(name) and name in known
    )
    unknown = sorted(name for name in contention.notes if name not in known)

    report.line(
        f"CONTENTION {len(contention.notes)} cells were measured while the host CPU "
        "idle reading was below its floor or unreadable. run.sh recorded them and "
        "continued, which it does for any breach that is neither two readings in a row "
        "nor more than a tenth of all readings, because an evidence run takes 106 "
        "readings and one isolated dip is expected on this host"
    )
    for name in sorted(contention.notes):
        report.line(f"  {name:<38} " + ", ".join(contention.notes[name]))
    if repetition_bearing:
        report.line(
            "  EXCLUDED FROM EVERY MEDIAN below, because these carry a repetition "
            "ordinal and the five-repetition design exists so a contaminated "
            "repetition can be dropped, "
            + ", ".join(repetition_bearing)
        )
    if single_instance:
        report.line(
            "  RECORDED AND KEPT, because these run once per matrix and have no "
            "repetition to be dropped in favour of, "
            + ", ".join(single_instance)
            + ". No new failure path is added for them and that is argued rather than "
            "assumed: band 5 measures canary drift directly and gates it, and band 1 "
            "judges each of them against its own group's P50 ceiling, 1.0ms at 150 bytes "
            "and 1.5ms at 32768, with ample headroom against an observed 0.363 to 0.899ms "
            "on the most contended host on record here. Both bands read these cells below "
            "and both judge them on their own numbers. Band 1 does read this ledger, and "
            "ONLY to annotate a failure it has already decided on, so no reading here can "
            "turn a band 1 failure into a pass"
        )
    if unknown:
        report.line(
            "  NAMED IN THE LEDGER BUT ABSENT FROM THIS RUN, which should be "
            "impossible and means the ledger and the cell summaries disagree, "
            + ", ".join(unknown)
        )
    report.line()


def check_integrity(report: Report, cells: list[Cell]) -> None:
    """Re-verify the k6 integrity outcomes from the committed summaries.

    The harness already fails a run on a non-zero k6 exit, but a reader of a
    committed directory should not have to trust that it did.

    AMENDED 2026-09-17 alongside the k6 thresholds. Steady dropped iterations and
    steady failed requests are now compared against per-cell TOLERANCES rather than
    against zero, computed here from the same basis-point arithmetic run.sh uses. The
    reasoning is at STEADY_DROP_TOLERANCE_BASIS_POINTS in run.sh, and this half of the
    change is not optional: leaving an absolute zero here would have re-failed at the
    END of the matrix exactly the run k6 had just correctly tolerated, which is a worse
    outcome than the original defect because it wastes the whole 52 minutes instead of
    stopping at the cell.

    Every RAW COUNT is reported whether it passed or not, so the tolerance can never
    hide a number, and a reader who prefers the old absolute rule can apply it to the
    printed counts by hand.

    The recorded k6 threshold outcomes are still re-verified verbatim and a recorded
    failure is still fatal. That is deliberate: it means a directory whose k6 aborted
    the matrix under a SUPERSEDED rule keeps its verdict, and the only way to get the
    new tolerance is a new run. Re-judging an old abort as a pass would bless a
    directory whose matrix genuinely stopped part way through.
    """
    problems: list[str] = []
    warmup_notes: list[str] = []
    tolerated: list[str] = []
    allowance_disagreements: list[str] = []
    total_drops = 0
    total_failures = 0
    total_demanded = 0
    demand_recoverable = True

    for cell in sorted(cells, key=lambda item: item.name):
        demanded = cell.demanded_steady_requests
        if demanded is None:
            demand_recoverable = False
        else:
            total_demanded += demanded
        total_drops += cell.dropped_steady
        total_failures += cell.failed_steady

        drop_allowance = cell.drop_allowance
        failed_allowance = cell.failed_allowance

        if cell.dropped_steady > drop_allowance:
            problems.append(
                f"{cell.name} dropped {cell.dropped_steady} steady iterations against an "
                f"allowance of {drop_allowance} of {demanded if demanded else 'unknown'} "
                "demanded"
            )
        elif cell.dropped_steady != 0:
            tolerated.append(
                f"{cell.name} steady drops {cell.dropped_steady} of {drop_allowance} allowed"
            )

        if cell.failed_steady > failed_allowance:
            problems.append(
                f"{cell.name} had {cell.failed_steady} failed steady requests against an "
                f"allowance of {failed_allowance} of {demanded if demanded else 'unknown'} "
                "demanded"
            )
        elif cell.failed_steady != 0:
            tolerated.append(
                f"{cell.name} failed steady requests {cell.failed_steady} of "
                f"{failed_allowance} allowed"
            )

        if cell.dropped_warmup != 0:
            warmup_notes.append(f"{cell.name} drops {cell.dropped_warmup}")
        if cell.failed_warmup != 0:
            warmup_notes.append(f"{cell.name} failed requests {cell.failed_warmup}")

        if cell.request_count <= 0:
            problems.append(f"{cell.name} recorded no requests")

        # The allowance cross check. k6 recorded the number it actually enforced, and
        # this file recomputed one. They must agree, or a cell was judged by one rule
        # at run time and a different rule at read time. Reported rather than fatal,
        # because the substantive comparison above already used the recomputed value
        # and a mismatch is a harness-consistency finding rather than a bad measurement.
        for label, recorded, derived in (
            ("drop", cell.reported_drop_allowance, drop_allowance),
            ("failure", cell.reported_failed_allowance, failed_allowance),
        ):
            if recorded is not None and recorded != derived:
                allowance_disagreements.append(
                    f"{cell.name} {label} allowance recorded {recorded} but recomputed "
                    f"{derived}"
                )

        for expression, outcome in sorted(cell.thresholds.items()):
            if outcome != "pass":
                problems.append(f"{cell.name} threshold {expression} reported {outcome}")

    if problems:
        detail = ", ".join(problems)
    else:
        detail = (
            f"{len(cells)} cells, every steady dropped-iteration and failed-request "
            "count inside its per-cell allowance, every k6 threshold passed"
        )
    if tolerated:
        detail += (
            ". TOLERATED AND RECORDED, inside the per-cell allowance rather than absent, "
            + ", ".join(tolerated)
        )
    if warmup_notes:
        detail += (
            ". Warmup counts, tolerated by design and recorded so a climbing count stays "
            "visible, " + ", ".join(warmup_notes)
        )
    if allowance_disagreements:
        detail += (
            ". ALLOWANCE CROSS CHECK DISAGREED, so k6 and this checker did not compute the "
            "same per-cell budget and one of the two arithmetic paths has drifted, "
            + ", ".join(allowance_disagreements)
        )
    report.verdict("INTEGRITY", not problems, detail)

    # THE RUN TOTAL, ADDED 2026-09-17. Per-cell verdicts answer whether any single cell
    # was in trouble. They do not answer what the whole matrix cost, and that aggregate
    # is the number that says whether a tolerance is being leaned on or barely touched.
    # A reader who wants to apply the old absolute rule reads this one line.
    if demand_recoverable and total_demanded > 0:
        drop_share = f"{total_drops / total_demanded * 100:.4f} percent"
        failure_share = f"{total_failures / total_demanded * 100:.4f} percent"
        basis = f"{total_demanded} demanded steady iterations across {len(cells)} cells"
    else:
        drop_share = "an unknown share"
        failure_share = "an unknown share"
        basis = (
            f"{len(cells)} cells, at least one of which records no recoverable demanded "
            "iteration count, so the shares cannot be computed"
        )
    report.line(
        f"INTEGRITY TOTALS {total_drops} steady dropped iterations and {total_failures} "
        f"failed steady requests across the whole run, {drop_share} and {failure_share} "
        f"of {basis}. Both are gated PER CELL against a fraction of that cell's own "
        f"demand, {STEADY_DROP_TOLERANCE_BASIS_POINTS} basis points with a floor of "
        f"{STEADY_DROP_TOLERANCE_FLOOR} for drops and "
        f"{STEADY_FAILED_TOLERANCE_BASIS_POINTS} basis points with a floor of "
        f"{STEADY_FAILED_TOLERANCE_FLOOR} for failures, so this total is context rather "
        "than a gate. It is printed because an absolute-zero rule across a matrix this "
        "size is a lottery and the honest replacement has to show its own aggregate"
    )
    report.line()


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


def band1_group_key(cell: Cell) -> tuple[bool, int]:
    """Return the direct-cell group one cell belongs to.

    A group is a response mode paired with a payload size, because those are the two
    terms that move duration-to-last-byte on this matrix. Both are read from the cell
    summary rather than parsed out of the name, since two of the direct cell names,
    direct-payload-4096 and direct-payload-32768, do not carry their response mode.
    """
    return (cell.stream, cell.prompt_bytes)


def band1_group_label(key: tuple[bool, int]) -> str:
    return ("streaming" if key[0] else "non-streaming") + f" {key[1]}B"


def band1_thresholds(cell: Cell) -> tuple[float, float] | None:
    """Return one direct cell's P50 gate and P99 advisory, or None if uncalibrated.

    None is not an error state to be smoothed over. A direct cell in a group with no
    calibrated pair would otherwise have to borrow a neighbouring group's ceiling,
    which is the exact category error the third amendment above removes, so the caller
    fails the band with the group named instead.
    """
    return BAND1_DIRECT_GROUP_THRESHOLDS.get(band1_group_key(cell))


def band1_by_group(
    entries: dict[tuple[bool, int], list[str]], threshold: dict[tuple[bool, int], float]
) -> str:
    """Render per-cell readings grouped by direct-cell group, naming each threshold.

    The grouping is not cosmetic and neither is naming the threshold inside each group
    label. Band 1 now judges four groups against four different numbers, and a flat
    list of cell names against a single stated threshold would leave a reader unable to
    tell which number judged which cell. Groups appear in the order the threshold table
    declares them, so the reading order is stable across runs and across directories
    whose matrices differ.
    """
    parts = []
    ordered = list(BAND1_DIRECT_GROUP_THRESHOLDS) + [
        key for key in entries if key not in BAND1_DIRECT_GROUP_THRESHOLDS
    ]
    for key in ordered:
        if not entries.get(key):
            continue
        limit = threshold.get(key)
        label = band1_group_label(key)
        stated = f"{label} (against {limit}ms)" if limit is not None else f"{label} (UNCALIBRATED)"
        parts.append(stated + " " + ", ".join(entries[key]))
    return ". ".join(parts)


def band1_contention_note(cell: Cell, contention: Contention) -> str:
    """Annotate a FAILING direct cell that the ledger recorded as contended.

    This annotation can only ever be added to a failure, never subtracted from one.
    Band 1 judges the floor a run's proxied numbers sit on, and a floor measured on a
    busy machine is still that run's floor, so contention here is context for a reader
    rather than grounds for exoneration. Three of the four direct groups are
    single-instance cells anyway, so the repetition-dropping rule the paired bands use
    has nothing to drop them in favour of.
    """
    notes = contention.notes.get(cell.name)
    if not notes:
        return ""
    return (
        " [MEASURED DURING A RECORDED CONTENTION BREACH, "
        + ", ".join(notes)
        + ", which is context and NOT an excuse, the failure stands]"
    )


def check_band1(report: Report, cells: list[Cell], contention: Contention) -> None:
    direct = [cell for cell in cells if cell.role == "direct"]
    if not direct:
        report.verdict("BAND1", False, "no direct-to-mock cell in this run, the band cannot be evaluated")
        return
    offenders = []
    uncalibrated = []
    median_observations: dict[tuple[bool, int], list[str]] = {}
    tail_observations: dict[tuple[bool, int], list[str]] = {}
    loud_tails: dict[tuple[bool, int], list[str]] = {}
    gates: dict[tuple[bool, int], float] = {}
    advisories: dict[tuple[bool, int], float] = {}
    contended_offenders = False
    for cell in sorted(direct, key=lambda item: item.name):
        key = band1_group_key(cell)
        pair = band1_thresholds(cell)
        central = cell.percentile(50)
        tail = cell.percentile(99)
        median_observations.setdefault(key, []).append(f"{cell.name} {central:.3f}")
        tail_observations.setdefault(key, []).append(f"{cell.name} {tail:.3f}")
        if pair is None:
            uncalibrated.append(f"{cell.name} in group {band1_group_label(key)}")
            continue
        gate, advisory = pair
        gates[key] = gate
        advisories[key] = advisory
        if central >= gate:
            note = band1_contention_note(cell, contention)
            contended_offenders = contended_offenders or bool(note)
            offenders.append(f"{cell.name} {central:.3f}ms against its {gate}ms ceiling{note}")
        if tail >= advisory:
            loud_tails.setdefault(key, []).append(f"{cell.name} {tail:.3f}ms")
    observed = band1_by_group(median_observations, gates)
    tails = band1_by_group(tail_observations, advisories)
    detail = (
        "direct P50 below its per-group ceiling, one calibrated pair per response mode "
        "and payload size. Observed: "
        + observed
        + ". Direct P99 recorded for the figures and for anyone applying the original "
        "pre-registered form of this band: "
        + tails
    )
    if offenders or uncalibrated:
        reasons = []
        if offenders:
            reasons.append(
                "direct P50 must be below its per-group ceiling, over budget at "
                + ", ".join(offenders)
                + ". A raised central tendency on a cell with no proxy in the path means the "
                "box or the mock is the bottleneck, so no proxied number in this run means "
                "anything"
            )
            # Scoped to the P50 offenders on purpose. Whether a cell was measured during
            # a dip is a live question about a reading that came in over budget, and it
            # is not a question at all about a group that has no threshold to be over.
            if contended_offenders:
                reasons.append(
                    "One or more failing cells are annotated above as contended. That "
                    "annotation exists so a reader can tell a bottleneck from a contended "
                    "sample, and it changes nothing: band 1 judges the floor this run's "
                    "proxied numbers sit on, and a floor measured on a busy machine is "
                    "still that floor"
                )
            elif not contention.recorded:
                reasons.append(
                    "This directory has no "
                    + CONTENDED_CELLS_FILENAME
                    + ", so whether these cells were measured during a host contention dip "
                    "is UNKNOWN rather than answered no"
                )
        if uncalibrated:
            reasons.append(
                "no calibrated band 1 threshold exists for "
                + ", ".join(uncalibrated)
                + ". A direct cell whose group has never been calibrated is NOT judged "
                "against a neighbouring group's ceiling, because a threshold applied to a "
                "quantity it was not derived from is the defect this band was amended twice "
                "to remove. Calibrate the group from its own readings and add it to "
                "BAND1_DIRECT_GROUP_THRESHOLDS"
            )
        detail = (
            ". ".join(reasons)
            + ". Every direct P50: "
            + observed
            + ". Direct P99 alongside it: "
            + tails
        )
    report.verdict("BAND1", not (offenders or uncalibrated), detail)
    # An advisory, deliberately not a gate. A loud tail on a direct cell is host
    # noise rather than a property of levee, and failing the run on it is the
    # mistake this band was amended to stop making. It still gets said out loud,
    # because a reader comparing two evidence directories needs to know which one
    # was measured on a busy machine.
    if any(loud_tails.values()):
        report.line(
            "BAND1 ADVISORY direct P99 above its per-group advisory threshold at "
            + band1_by_group(loud_tails, advisories)
            + ". This is host tail noise, not a bottleneck, and it does not invalidate the "
            "run. Check cpu_idle_pct in machine-state.txt, which is the field that "
            "discriminates a contended host. Read loadavg there for context only: it was "
            "PROVEN not to discriminate, sitting at 4.0 to 6.4 on the invalidated 43-cell run "
            "and 2.8 to 5.0 on the quiet re-measurements that corrected it. On the reference "
            "host the resident endpoint-security agents and the Spotlight indexer produce "
            "tens-of-milliseconds scheduler stalls that land in every cell including the "
            "direct ones"
        )


def check_band2(report: Report, cells: list[Cell], contention: Contention) -> None:
    direct = select(cells, "direct", stream=False, prompt_bytes=SMALL_PAYLOAD_BYTES)
    measured = select(cells, "passthrough", stream=False, prompt_bytes=SMALL_PAYLOAD_BYTES)
    if not direct or not measured:
        report.verdict(
            "BAND2",
            False,
            f"needs direct and passthrough non-streaming cells at {SMALL_PAYLOAD_BYTES}B, "
            f"found {len(direct)} direct and {len(measured)} passthrough",
        )
        return

    # Only the passthrough arm carries repetition ordinals here. The direct arm at
    # this payload is the two drift canaries, which run once each, so they are the
    # single-instance case and stay in the baseline whatever their host state was.
    usable = usable_repetitions([measured], contention)
    if not usable.evaluable:
        report.verdict("BAND2", False, f"at {SMALL_PAYLOAD_BYTES}B, {usable.cause}")
        return
    passthrough = keep_clean(measured, usable)
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
    if usable.note:
        detail += f". {usable.note}"
    if not ok:
        detail += (
            ". Below the floor means the cell did not traverse the proxy. Above the "
            "ceiling means something beyond the extra loopback hop is being paid"
        )
    report.verdict("BAND2", ok, detail)


def enforcement_arms(
    cells: list[Cell], stream: bool, prompt_bytes: int
) -> tuple[list[Cell], list[Cell]]:
    return (
        select(cells, "passthrough", stream=stream, prompt_bytes=prompt_bytes),
        select(cells, "enforce", stream=stream, prompt_bytes=prompt_bytes),
    )


def enforcement_shifts(
    cells: list[Cell],
    quantile: float,
    stream: bool = False,
    prompt_bytes: int = SMALL_PAYLOAD_BYTES,
    usable: RepetitionSet | None = None,
) -> list[tuple[str, float]]:
    """Return the per-repetition enforce minus passthrough shifts at one quantile.

    usable is the contended-repetition filter. A caller that gates on both the P50 and
    the P99 of the same pairing MUST pass the same RepetitionSet to both, or band 4
    would divide a tail shift measured over one set of repetitions by a median shift
    measured over another.
    """
    passthrough, enforce = enforcement_arms(cells, stream, prompt_bytes)
    if usable is not None:
        passthrough = keep_clean(passthrough, usable)
        enforce = keep_clean(enforce, usable)
    return paired_shifts(passthrough, enforce, quantile)


def check_band3(report: Report, cells: list[Cell], usable: RepetitionSet) -> float:
    """Evaluate the PRIMARY enforcement gate and return its median P50 shift.

    Relocated to BAND3_PRIMARY_PAYLOAD_BYTES on 2026-09-16 for signal to noise. The
    150B reading is still computed and printed, by report_band3_small below, and it
    no longer gates. Band 4 divides by the value returned here, so it receives the
    SAME RepetitionSet from the caller.
    """
    payload = BAND3_PRIMARY_PAYLOAD_BYTES
    if not usable.evaluable:
        report.verdict("BAND3", False, f"at {payload}B, {usable.cause}")
        return 0.0
    shifts = enforcement_shifts(cells, 50, prompt_bytes=payload, usable=usable)
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
    if usable.note:
        # A dropped repetition also narrows the spread the resolution check reads,
        # which makes that check easier to pass. Said out loud rather than left for a
        # reader to notice, since it is the one place the exclusion loosens something.
        detail += (
            f". {usable.note}, so both the median and the spread above are over the "
            "clean repetitions only"
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


def report_band3_small(report: Report, cells: list[Cell], contention: Contention) -> None:
    """Print the 150B enforcement reading, with an advisory and no gate.

    RELOCATED rather than dropped on 2026-09-16. Every number the gated form used to
    print is still printed here, so a reader can apply the original band by hand and
    see what it would have said. What is gone is its power to invalidate a run, for
    the signal-to-noise reasons argued at BAND3_PRIMARY_PAYLOAD_BYTES.

    Contended repetitions are excluded here exactly as they are from the gates, so the
    printed number is the one a reader should compare against the quiet-host +15us.
    When too few clean repetitions remain this section says so and STOPS, and it does
    NOT fail the run: this reading has no power to invalidate one, which is the whole
    point of the relocation, and giving it that power through the contention path
    would reverse a decision made deliberately.
    """
    usable = usable_repetitions(
        list(enforcement_arms(cells, False, SMALL_PAYLOAD_BYTES)), contention
    )
    if not usable.evaluable:
        report.line(
            f"BAND3-SMALL UNEVALUABLE at {SMALL_PAYLOAD_BYTES}B, {usable.cause}. This "
            "is a record and not a gate, so it does not invalidate the run, and the "
            "small-payload enforcement reading is simply unavailable from it"
        )
        return
    shifts = enforcement_shifts(cells, 50, prompt_bytes=SMALL_PAYLOAD_BYTES, usable=usable)
    tail_shifts = enforcement_shifts(
        cells, 99, prompt_bytes=SMALL_PAYLOAD_BYTES, usable=usable
    )
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
        + (f". {usable.note}" if usable.note else "")
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


def report_control_pair(report: Report, cells: list[Cell], contention: Contention) -> None:
    """Print the A/A control shift, whose TRUE VALUE IS ZERO.

    A REPORT and never a gate, for the reasons argued at CONTROL_A_ROLE. It sits
    beside the enforcement readings on purpose: the noise floor and the signal it
    qualifies belong on the same screen.

    Contended repetitions are excluded here too. The control's job is to say what the
    estimator invents at a known zero ON THE HOST THE RUN WAS MEASURED ON, and a
    contended repetition inside it would state the noise floor of a machine the
    published numbers no longer come from. Too few clean repetitions makes it
    unevaluable and does NOT fail the run, for the same reason it is not a gate at
    all: a contended A/A pair still read 13us, so it was never able to certify a host.
    """
    all_arm_a = select(cells, CONTROL_A_ROLE, stream=False, prompt_bytes=SMALL_PAYLOAD_BYTES)
    all_arm_b = select(cells, CONTROL_B_ROLE, stream=False, prompt_bytes=SMALL_PAYLOAD_BYTES)
    usable = usable_repetitions([all_arm_a, all_arm_b], contention)
    if all_arm_a and all_arm_b and not usable.evaluable:
        report.line(
            f"CONTROL-AA UNEVALUABLE, {usable.cause}. This is a control and never a "
            "gate, so it does not invalidate the run. What is lost is the estimator "
            "noise floor measured in the same run that publishes a number, which then "
            "has to be taken from the quiet-host readings of -1, +4 and 0us"
        )
        return
    arm_a = keep_clean(all_arm_a, usable)
    arm_b = keep_clean(all_arm_b, usable)
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
        + (f". {usable.note}" if usable.note else "")
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


def check_band4(
    report: Report, cells: list[Cell], median_p50_shift: float, usable: RepetitionSet
) -> None:
    payload = BAND3_PRIMARY_PAYLOAD_BYTES
    if not usable.evaluable:
        report.verdict(
            "BAND4",
            False,
            f"at {payload}B, {usable.cause}. This band is a ratio against band 3's "
            "median at the same payload size, so it is unevaluable for exactly the "
            "reason band 3 is",
        )
        return
    shifts = enforcement_shifts(cells, 99, prompt_bytes=payload, usable=usable)
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
        + (f". {usable.note}" if usable.note else "")
    )
    if not ok:
        detail += (
            ". A tail shift this far above the median shift means one cell caught a "
            "transient that every median gate missed"
        )
    report.verdict("BAND4", ok, detail)


def check_band3_stream(report: Report, cells: list[Cell], contention: Contention) -> None:
    """Evaluate the streaming companion to band 3.

    The gate is on the absolute size of the shift and is deliberately wide. The
    central value is RECORDED and ADVISED on rather than gated, for the reasons
    tabulated at BAND3_STREAM_SHIFT_MAX_ABSOLUTE_MILLISECONDS above.

    AMENDED 2026-09-17, and this reverses a position the previous version of this
    docstring took explicitly. It used to say that one contended streaming repetition
    making the band unevaluable was "accepted rather than special-cased" and that the
    remedy was "never a lower minimum here". It is now special-cased, at
    STREAM_MINIMUM_CLEAN_REPETITIONS, and the full argument is at that constant.

    In short: three repetitions against a minimum of three permits ZERO contended
    streaming repetitions across 12 host idle readings, which is the same
    absolute-zero-across-many-independent-opportunities defect that cost three evidence
    runs, and it fires at the END of the matrix so it costs all 52 minutes. The
    order-statistic argument that justifies the minimum elsewhere does not bind on a
    two-sided 0.60ms ceiling around a 15us quantity whose central value this band
    already refuses to publish. Zero clean repetitions still fails.
    """
    usable = usable_repetitions(
        list(enforcement_arms(cells, True, SMALL_PAYLOAD_BYTES)),
        contention,
        minimum=STREAM_MINIMUM_CLEAN_REPETITIONS,
    )
    if not usable.evaluable:
        report.verdict("BAND3-STREAM", False, usable.cause)
        return
    shifts = enforcement_shifts(cells, 50, stream=True, usable=usable)
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

    tail_shifts = enforcement_shifts(cells, 99, stream=True, usable=usable)
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
        + (f". {usable.note}" if usable.note else "")
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

    # The thin-median advisory, ADDED 2026-09-17 with STREAM_MINIMUM_CLEAN_REPETITIONS.
    # The ceiling above still applies over whatever repetitions survived, because it is
    # wide enough to be meaningful on one. The MEDIAN is a different matter, so a run
    # that lost streaming repetitions to contention says so loudly here rather than
    # printing a thinner number that looks the same as a full one.
    #
    # It fires only when contention actually COST repetitions, not merely when the
    # matrix ran fewer than three. Quick mode runs one streaming repetition by design and
    # the detail line above already says the spread is not resolvable, so warning about
    # it again would be noise that trains a reader to skip these lines.
    if usable.dropped and len(usable.kept) < MINIMUM_CLEAN_REPETITIONS:
        survivors = (
            "1 clean streaming repetition"
            if len(usable.kept) == 1
            else f"{len(usable.kept)} clean streaming repetitions"
        )
        report.line(
            f"BAND3-STREAM THIN MEDIAN this ceiling was applied over {survivors}, below "
            f"the {MINIMUM_CLEAN_REPETITIONS} that a median needs to be an order "
            "statistic over independent measurements. The GATE still holds, because it is "
            "a two-sided ceiling on absolute size that fires on roughly a 40-fold "
            "regression or on an enforce arm that was not enforcing, and both are visible "
            "in a single repetition. The printed median is correspondingly weaker and must "
            "not be quoted as the streaming enforcement cost, which this band already "
            "refuses to publish in any case. Before 2026-09-17 this situation FAILED the "
            "run outright, at the end of the matrix, which is why it changed. The remedy is "
            "more streaming repetitions in the matrix rather than a different number here"
        )

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

    contention = read_contention(results_dir)

    report_inventory(report, cells)
    report_cost(report, cells)
    # The contention list comes before every gate that acts on it, so a reader sees
    # which repetitions were dropped before seeing the medians they were dropped out of.
    report_contention(report, cells, contention)
    check_integrity(report, cells)
    check_achieved_rate(report, cells)
    check_band1(report, cells, contention)
    check_band2(report, cells, contention)
    # Band 3 and band 4 share ONE repetition filter, resolved here rather than inside
    # either of them. Band 4 divides its tail shift by band 3's median shift, so the
    # two must be computed over the same repetitions or the ratio is arithmetic between
    # two different populations.
    primary_usable = usable_repetitions(
        list(enforcement_arms(cells, False, BAND3_PRIMARY_PAYLOAD_BYTES)), contention
    )
    # The order here is the reading order a skeptic needs. The primary gate first,
    # then the small-payload reading it was relocated from, then the control that
    # says what the estimator behind both of them invents at a true zero. Putting the
    # control last of the three means the noise floor is on screen underneath every
    # shift it qualifies.
    median_p50_shift = check_band3(report, cells, primary_usable)
    report_band3_small(report, cells, contention)
    report_control_pair(report, cells, contention)
    check_band4(report, cells, median_p50_shift, primary_usable)
    check_band3_stream(report, cells, contention)
    check_band5(report, cells)

    report.line()
    if report.failed:
        report.line("VERDICT INVALID this run violated at least one pre-registered band and is NOT publishable")
        return 1
    report.line("VERDICT VALID every pre-registered band passed")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
