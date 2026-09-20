# Benchmark experiment changelog

Every change that could move a published number gets an entry here: the cell
matrix, the rates, the payload sizes, the estimator, the validity bands, the mock
behaviour, the pinned tool versions, and the retirement of any evidence directory.
Code-level changes that cannot move a number do not need one.

This is separate from the product `CHANGELOG.md` at the repository root. That one
tracks what levee does. This one tracks what the measurement does, so a reader
comparing two published numbers can tell whether the difference is levee or the
ruler. It is not replaceable by `git log`, because the project squash-merges and the
26 granular commits behind these entries are unreachable from `main`.

Each entry says what moved and which published numbers stop being comparable across
it. Why a threshold has the value it does is in
[methodology/calibration.md](methodology/calibration.md), and the gates in their
current form are in [methodology/bands.md](methodology/bands.md).

Dates are UTC, matching the results directory names.

## 2026-09-17, the first valid evidence run is published

`2026-09-17-2a569f4-m3pro-macos-evidence-r1` completed all 53 cells and closed with
VERDICT VALID. Every prior entry changed the ruler while no run had produced a
number, so from this line on an entry that moves a threshold also has to say which
published figure it moves.

**What stops being comparable.** Nothing looking backwards, since nothing before
this run was published. Looking forwards, commit `f295ed4` (branch commit `b5087fa`)
landed after this run and removed the duplicate tokenizer pass, so every enforcement
figure this run publishes describes doubled tokenization that current code does not
perform. Measured effect of the fix on the same host: 42.0 percent faster at 32768
bytes, 38.7 percent at 4096 bytes, 14.3 percent at 150 bytes. No post-fix
enforcement number exists yet and the two cannot be compared. Band 3's 0.95ms
ceiling is stale for the same reason and is now weaker than it reads.

**Three stale figures corrected in the same pass.** The +15us loaded 150-byte
enforcement figure, and the argument that the loaded figure is materially smaller
than the concurrency-1 53us: this run reads +55us with a 57us spread, which does not
resolve a signal that size, so it removes the basis for the comparison without
confirming or refuting +15us. The 47-fold levee CPU spread, whose enforced end
reproduces at 10.63ms per 32KB request while the passthrough end does not, so the
spread reads 17.2-fold and 7.0-fold at a matched payload. And "eight times too high"
for the invalidated first run, which now reads as 123us above a measured band of +11
to +90us.

**The enforcement crossing moved and neither value was wrong.** 500us near 2KB and
1ms near 4KB came from concurrency-1 service-time deltas and stand for that
condition. Under 500 rps of load the crossing sits near 3.4KB. Both are now
published with their condition attached, and the concurrency-1 figure is flagged as
the one to size a worst case from because it is the larger at 4096B.

## 2026-09-17, every band 1 ceiling comes from its own cell group

Band 1's third amendment, and the second in two days. Each direct-cell group's P50
ceiling is now that group's own median P50 times a single multiplier held constant
across every group, rounded down to the nearest 0.5ms, taking the lowest bin where a
product straddles a boundary across data cuts. The P99 advisory follows the same
rule with its own multiplier. Both multipliers are anchored on the 1.0ms and 2.5ms
thresholds band 1 has always carried, so no group has a multiplier of its own and
the anchor is unchanged by construction. This replaces the two special cases below
with one rule covering every axis. Band 1 also now reads `contended-cells.txt`, only
to annotate a failure, and a direct cell in an uncalibrated group fails the band with
the group named instead of borrowing a neighbour's ceiling.

**What stops being comparable.** No published number moves, because no run had yet
produced a publishable number. Three of the eight thresholds change:
`direct-payload-32768` gains a 1.5ms ceiling and a 3.5ms advisory, both first
calibrations, and the streaming P99 advisory goes 5.0ms to 5.5ms as a rounding
correction. The only output that changes on any existing directory is one advisory
line on `2026-09-16-31918d9-dirty-m3pro-macos-quick-r1` at 2.835ms, established by
running both checker versions over every directory in the tree. No VALID or INVALID
verdict changes anywhere, because an advisory never blocks publication.

**Not retroactive.** `2026-09-17-4d3f224-m3pro-macos-evidence-r1` passes the amended
band and stays INVALID, with its own `bands.txt` unmodified.

## 2026-09-17, band 1 gains a per-mode ceiling

A fourth evidence run was lost, and this one ran all 53 cells to completion before a
band refused it. `2026-09-17-4d3f224-m3pro-macos-evidence-r1` read direct streaming
P50 of 1.341, 1.230 and 0.951ms against a single 1.0ms ceiling calibrated on
non-streaming cells, while every non-streaming direct cell passed comfortably. The
cause is structural: a streaming response replays six SSE events with a write and a
flush each where a non-streaming response is one write, so duration to last byte is
a different quantity in the two modes. Streaming gained its own 2.0ms P50 ceiling and
5.0ms P99 advisory, each the non-streaming figure times the measured 2.21 mode ratio,
rounded down. Scoping band 1 by stream mode had been offered and rejected a day
earlier in favour of moving the quantile from P99 to P50. Two independent defects
were bundled into one choice and only one got fixed.

**What stops being comparable.** No published number moves. The non-streaming values
do not move, verified by running both checker versions over every results directory
and over isolated boundary cases at 0.999 and 1.000ms.

**Not retroactive.** The amended checker passes `4d3f224` and that directory stays
INVALID. No figure is rendered from it and none of its numbers is published.

## 2026-09-17, the two integrity gates become tolerances

A third evidence run died to a gate that could not be satisfied, so the whole harness
was audited for the same defect rather than only the gate that fired.
`dropped_iterations{scenario:steady}: count==0` and
`http_req_failed{scenario:steady}: rate==0` become 1 percent of demanded steady
iterations with a floor of 25, and 0.05 percent with a floor of 5, both per cell from
that cell's own demand. The audit found a third gate of the same shape, the streaming
repetition minimum, which goes from three clean repetitions to one for BAND3-STREAM
only. `dropped-iterations.txt` gained the allowance and the demanded count on every
line, a new `failed-requests.txt` carries the same shape for failures, the MANIFEST
records both tolerances with their floors, and `bands.txt` prints an `INTEGRITY
TOTALS` line.

**What stops being comparable.** No published number moves. What changes is what an
occurrence of a rare event DOES. Two directories held to different integrity rules
are still comparable on their latency numbers, because the tolerance decides whether
a run publishes at all and not what it measures, and the MANIFEST records which rule
each was judged under. Every raw count is still recorded whether it passed or not, so
the old absolute rule can be applied to any committed directory by hand. The checker
was re-run over all 16 directories with every verdict unchanged, which none could
have contradicted: a cell that fails a k6 threshold never gets a filtered CSV
written, so any directory carrying a nonzero drop count already fails to load.

## 2026-09-16, the quiescence gate fails on sustained contention only

The gate added in the entry below was unsatisfiable. The second evidence attempt died
at 51 minutes 54 seconds after 39 of 53 cells, on the only sub-floor reading in that
run's 79. A mid-run breach now fails the run on two consecutive confirmed sub-floor
readings, or on more than 10 percent of all mid-run readings. One isolated dip is
recorded in a new `contended-cells.txt`, and `check_bands.py` drops that repetition
out of every median it computes. The 60 percent floor does not move, the sampler does
not change, and the startup check still refuses on one reading.

**What stops being comparable.** No published number moves. A median computed after
this change may be over four repetitions rather than five, which `bands.txt` states
on the line that prints it, and below three clean repetitions the band fails instead
of publishing a thin median. A directory predating the change has no
`contended-cells.txt` at all, so whether its cells were contended is unknown rather
than answered no.

## 2026-09-16, the first evidence run is invalidated and the quiescence gate is added

**This entry exists so nobody reads 123us as levee's enforcement cost.** The first
completed 43-cell evidence run, at commit `31918d9`, was INVALIDATED by band 3 after
measuring +123us at 150 bytes against a pre-registered window of 0 to 100us. Every
other gate passed. The investigation attributed it to host CPU contention and not to
levee, and the corrected figure is +15us net on a quiet host. The full record is in
[results/README.md](results/README.md#the-first-completed-evidence-run-was-invalidated).

Three things were added as a result. A host quiescence gate that samples system-wide
CPU idle percentage and refuses an evidence run below 60 percent. An A/A control pair
of two passthrough cells per repetition, reported and never gated, because a contended
A/A pair still read 13us so a small control reading does not certify a quiet host. And
the primary enforcement gate moved from 150 bytes to 4096 bytes, where the same
quantity has roughly 160 times the signal-to-noise ratio instead of 3.7. Band 4
followed band 3 because it is a ratio against band 3's median, and quick mode's payload
set gained 4096 so a local check can still exercise the primary gate. Read the band 3
move precisely: the 150-byte window was CORRECT and the run that failed it was INVALID.
The window was not widened. The 150-byte delta is still printed as `BAND3-SMALL`
against an advisory window of -0.05 to +0.10ms, whose floor is negative on purpose
because gross enforce-only work is 38.2us against a 24us shared-path credit, and
removing one tokenizer pass puts the net near -3us.

**What stops being comparable.** The +123us reading and anything derived from it. Two
committed constants were also corrected: `check_bands.py` recorded the estimator at
"EstimateSplit 4.32us, Estimate 4.29us" and an in-process total shift of 16.9us, where
one pass on the exact load-generator 150-byte body measures 17,364 ns/op and the code
made two, so a 16.9us total was arithmetically impossible. The retracted probe's
streaming-to-non-streaming ratio of 1.16 goes with it. The two extra log lines an
enforced request writes were also reframed: the 2.1us of CPU is real and their effect
on the P50 is 0.0us, so they were never a pollution source.

**Not retroactive.** The invalidated directory stays invalidated. Running today's
checker over it prints `VERDICT VALID`, which is a property of the relocated gate and
not a re-blessing of the old numbers.

## 2026-09-16, per-cell arrival rates and the RATE gate

**This entry exists so nobody compares a number across it without noticing.** The
arrival rates and the VU pools both changed, after the first evidence attempt revealed
that the 32768B enforce cell had been demanding more than the machine's capacity. Any
32KB number taken before this change is queue residence and not service time, and is
not comparable to anything taken after it. The 150B and 4096B numbers are unaffected
in their rate and are affected in their VU pool.

Rates are now per payload size, from `rate_for_payload`, each sized to roughly 40
percent of that size's measured capacity or lower: 500 rps at 150B and 4096B, 150 rps
at 32768B down from 500, and 250 rps streaming at 150B unchanged. The MANIFEST records
one demanded-rate field per payload size instead of a single global rate. VU pools are
now equal in every cell, one fully preallocated 40-slot pool, where passthrough and
direct previously ran 50 to 100 against enforce's 40. Under any queueing the pool size
is part of what a latency number measures, so the arms were not comparable. 40 rather
than 50 because `internal/budget/store.go` hardcodes 50 admission slots per agent and
an exhausted slot answers 429 instead of queueing.

New gate, achieved versus demanded arrival rate, at cell time as
`http_reqs{scenario:steady}: count>=MIN_STEADY_REQUESTS` and again over the committed
artifacts in `check_bands.py`, with a 2 percent tolerance. New artifact,
`cpu-seconds.txt`, so saturation is readable off the artifact instead of inferred.

**Also corrected here.** `benchmarks/README.md` said enforcement crosses 500us near a
4KB prompt and 1ms near 8KB. Both came from ONE tokenizer pass where the shipped code
made TWO, so both understated enforcement by roughly a factor of two. Measured end to
end through the real binary at concurrency 1, 500us is crossed near 2KB and 1ms near
4KB.

**New published result.** Enforced throughput is bounded by tokenizer CPU, about 400
rps at a 32KB prompt with the shipped double pass, and it is RETROGRADE past the knee:
400 rps at concurrency 4, 295 at 8, 260 at 40.

## 2026-09-16, initial methodology

The harness, the measurement matrix, the validity gates, and the figures. No evidence
run predates this entry, so there is no number published before this methodology
existed and nothing here can break a comparison.

**Tooling.** k6 v2.2.0, installed from the project's own release binary rather than
Homebrew, with `K6_NO_USAGE_REPORT=true` on every invocation. Go per `go.mod`,
currently go1.26.3 on the reference host. uv 0.10.12 for the figure scripts, each with
a committed per-script lockfile pinning exact versions and sha256 hashes.

**Estimator.** Proxy overhead is a quantile shift, P99(proxied) minus P99(direct), and
not the P99 of per-request overhead. Both absolute distributions are always published
alongside the shift, and every number carries its payload size and arrival rate.

**Cell matrix.** `{direct to mock, levee passthrough, levee enforce}` by
`{non-streaming, streaming}`, one `run.sh` invocation against one boot of the mock,
levee restarted per proxied cell from a binary built once from HEAD. Warmup 10s at the
steady rate, steady window starting at 12s. Passthrough and enforce run back to back
as a pair, five repetitions in evidence mode and one in quick mode, with the published
delta the median of the per-repetition deltas. Direct drift canaries open and close the
matrix. OpenAI cells only, because the tiktoken path upper-bounds the Anthropic
character heuristic and is therefore the conservative choice. The rates, payload sizes
and integrity thresholds this entry set are all SUPERSEDED by the entries above.

**The five pre-registered bands**, stated here in the form they were pre-registered in
and all since amended except band 2. Band 1, every direct-to-mock cell P50 below 1.0ms.
Band 2, median passthrough minus direct P50 shift within 0.05 to 0.6ms. Band 3, at 150B
the median repetition-matched enforce minus passthrough P50 delta within 0 to 100us
with the across-repetition spread smaller than the delta. Band 4, median enforce minus
passthrough P99 shift no more than ten times the median P50 shift. Band 5, opening and
closing direct canaries agreeing within 15 percent at both quantiles.

**Measured facts that shaped the methodology.** Token estimation was recorded as linear
at roughly 125us per KB, giving a 500us crossover near a 4KB prompt, which is why
payload size became a controlled dimension. That crossover figure is wrong and is
superseded above. The sentence is left standing because this is a historical record,
and the conclusion it supports holds a fortiori. Levee's first enforce-mode request
costs about 130ms against about 4ms steady, roughly 100ms of it the one-time encoder
build, which is why warmup exists. Time to first byte sits at 60 to 85 percent of
full-stream duration because the mock replays events with no pacing, which is a
property of the mock and not of levee.

**Provenance.** Each run writes a MANIFEST last, after the bands pass and the identity
audit is clean, recording the tool versions, the host, the sysctls, the per-cell power
and load and thermal readings, the fixture digests, and both the commit SHA and the
tree hash. The tree hash is the field that survives this project's squash merges.
