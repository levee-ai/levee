# Benchmark experiment changelog

Every change that could move a published number gets an entry here: the cell
matrix, the rates, the payload sizes, the estimator, the validity bands, the mock
behaviour, the pinned tool versions, and the retirement of any evidence
directory. Code-level changes that cannot move a number do not need one.

This is separate from the product `CHANGELOG.md` at the repository root. That one
tracks what levee does. This one tracks what the measurement does, so that a
reader comparing two published numbers can tell whether the difference is levee
or the ruler.

Dates are UTC, matching the results directory names.

## 2026-09-16, per-cell arrival rates and the achieved-rate gate

**This entry exists so nobody compares a number across it without noticing.** The
arrival rates and the VU pools both changed, after the first evidence attempt
revealed that the 32768B enforce cell had been demanding more than the machine's
capacity. Any 32KB number taken before this change is queue residence, not
service time, and is not comparable to anything taken after it. The 150B and
4096B numbers are unaffected in their rate and are affected in their VU pool, see
below.

**What the failed attempt showed.** The 32768B enforce cell demanded 500 rps
against roughly 400 rps of measured capacity. It dropped 14622 steady iterations,
exited 99, and reported a 152.4ms median. The honest service time there is 8.5ms.
Little's Law closes the remaining 144ms exactly: 40 requests in flight over the
260 rps actually achieved is 154ms, against 152.4ms observed. Nothing was wrong
with levee, and the drop threshold behaved correctly by refusing to let the number
be published.

**Rates are now per payload size**, from `rate_for_payload` in `run.sh`, each
sized to roughly 40 percent of that size's measured capacity or lower. 500 rps at
150B and 4096B, both unchanged and both already under 18 percent utilization
against a measured 2706 rps enforce capacity at 4096B. **150 rps at 32768B**, down
from 500, against an enforce capacity of about 400 rps peak. 250 rps streaming at
150B, unchanged. Every rate carries the measured capacity it derives from in a
comment beside it, and the MANIFEST now records one demanded-rate field per
payload size instead of a single `rate_nonstreaming_rps`.

**One global rate was never sound.** Levee needs about 10.94ms of CPU per enforced
32768B request against 0.233ms for a passthrough one, a 47-fold spread, so a rate
that leaves passthrough idle saturates enforce at the same payload size.

**VU pools are now equal in every cell**, one fully preallocated 40-slot pool,
where passthrough and direct previously ran 50 to 100 against enforce's 40 to 40.
Under any queueing the pool size is part of what a latency number measures, so the
two arms were not comparable. 40 rather than 50 because
`internal/budget/store.go` hardcodes 50 admission slots per agent and an exhausted
slot answers 429 rather than queueing, so equalizing means bringing the other arms
down. `run.sh` now refuses to start when the pool is not strictly under 50. With
the new rates the busiest cell needs about 1.3 concurrent workers, so 40 is
roughly 30 times the requirement.

**New gate, achieved versus demanded arrival rate**, at cell time as
`http_reqs{scenario:steady}: count>=MIN_STEADY_REQUESTS` and again over the
committed artifacts in `check_bands.py`, with a 2 percent tolerance. It refuses to
publish a latency number from any cell that missed its demand, and it **subsumes
the drop count as a validity signal**, because a pool large enough to absorb a
growing backlog lets a saturated cell drop nothing while still completing less work
than was demanded. The drop threshold is unchanged at `count==0`.

**New artifact, `cpu-seconds.txt`**, levee's CPU seconds across each cell's steady
window from `ps -o cputime` deltas at the two window edges, plus CPU milliseconds
per request. `bands.txt` prints the implied busy core count and the measured VU
headroom beside it. Saturation is now readable off the artifact rather than
inferred. The first matrix to carry it reproduced the 47-fold CPU spread
independently, 11.32ms per enforced 32KB request against 0.241ms per passthrough
150B request, so that figure is now measured per run rather than quoted.

**Verification of this change.** A quick matrix reached `VERDICT VALID` with every
band, the new RATE gate, and the COST table passing. A targeted matrix at 150B and
32768B put the previously failing cell at its new 150 rps: **3001 steady requests,
150.05 rps achieved of 150 demanded, zero steady drops, zero warmup drops, k6 exit
0, and a 7.701ms median against a 9.502ms P99**. That median is service time, in
the neighbourhood of the 8.5ms the failed cell should have reported, rather than
the 152.4ms of queue residence it did report. The cell needed 1.16 of its 40 VUs,
so the pool has 34.6 times the concurrency it requires.

**Corrected a published claim.** `benchmarks/README.md` said enforcement crosses
500us near a 4KB prompt and 1ms near 8KB. Those were derived from ONE tokenizer
pass and the shipped code makes TWO, so both understated enforcement by roughly a
factor of two. Measured end to end through the real binary at concurrency 1, the
enforcement delta is 53us at 150B, 1.174ms at 4096B and 7.281ms at 32768B, which
is 222 to 287ns per prompt byte. **500us is crossed near 2KB and 1ms near 4KB.** A
pending product fix removing the duplicate pass should roughly halve these. The
tokenizer itself is linear at about 120ns per byte from 150B to 64KB with no
superlinear region, and the harness filler is not a pathological input: realistic
prose costs about 13 percent more per byte, so `buildPrompt` is deliberately
unchanged.

**New published result.** Enforced throughput is bounded by tokenizer CPU, about
400 rps at a 32KB prompt on the reference host with the shipped double pass, and
it is RETROGRADE past the knee: 400 rps at concurrency 4, 295 at 8, 260 at 40.
Published as a property rather than buried as an embarrassment.

## 2026-09-16, initial methodology

The harness, the measurement matrix, the validity gates, and the figures. No
evidence run predates this entry, so there is no number published before this
methodology existed.

**Tooling.** k6 v2.2.0, installed from the project's own release binary rather
than Homebrew, with `K6_NO_USAGE_REPORT=true` on every invocation. Go per
`go.mod`, currently go1.26.3 on the reference host. uv 0.10.12 for the figure
scripts, each with a committed per-script lockfile pinning exact versions and
sha256 hashes.

**Estimator.** Proxy overhead is a QUANTILE SHIFT, P99(proxied) minus
P99(direct), not the P99 of per-request overhead. Both absolute distributions are
always published alongside the shift, and every number carries its payload size
and arrival rate.

**Cell matrix.** `{direct to mock, levee passthrough, levee enforce}` by
`{non-streaming, streaming}`, one `run.sh` invocation against one boot of the
mock, levee restarted per proxied cell from a binary built once from HEAD.

- Rates 500 rps non-streaming and 250 rps streaming, one utilization point per
  cell, evenly spaced arrivals from k6's `constant-arrival-rate` open-model
  executor. **SUPERSEDED by the entry above**: the non-streaming rate is now per
  payload size, and 32768B runs at 150 rps.
- Warmup 10s at the steady rate, steady window starting at 12s, 20s long in
  quick mode and 60s in evidence mode.
- Payload sizes 150B, 4KB and 32KB for the proxied cells in evidence mode, 150B
  and 32KB for direct cells. Quick mode runs 150B only.
- Passthrough and enforce run back to back as a pair, five repetitions in
  evidence mode and one in quick mode, and the published delta is the median of
  the per-repetition deltas with the spread shown.
- Direct drift canaries open and close the matrix.
- OpenAI cells only. The Anthropic mock routes exist and are unit tested but are
  not measured, because the OpenAI tiktoken path upper-bounds the Anthropic
  character heuristic and is therefore the conservative choice.
- Integrity thresholds `dropped_iterations{scenario:steady}: count==0` and
  `http_req_failed{scenario:steady}: rate==0`, both scoped to the steady scenario
  because levee's first enforce-mode request costs about 130ms while the encoder
  is built and warmup exists to absorb it.

**Pre-registered validity bands**, published in full in
`benchmarks/results/README.md` and evaluated mechanically by
`benchmarks/plots/check_bands.py`:

1. Every direct-to-mock cell P50 below 1.0ms. The P99 is recorded, and above
   2.5ms it raises a non-blocking advisory rather than failing the run, so the
   P50 is the only gate here. **AMENDED on this date**, from the original P99
   below 1.0ms. The 1.0ms figure is a claim about proxy overhead, a delta, and
   the original band had borrowed it as an absolute bound on one cell, where it
   detected host tail noise while claiming to detect a bottleneck.
2. Median passthrough minus direct P50 shift within 0.05 to 0.6ms.
3. At 150B, median repetition-matched enforce minus passthrough P50 delta within
   0 to 100us, with the across-repetition spread smaller than the delta.
4. Median enforce minus passthrough P99 shift no more than ten times the median
   P50 shift.
5. Opening and closing direct canaries within 0.25ms absolute drift at P50 and
   1.50ms at P99, both of them gates, unlike band 1 where the tail only
   advises. **AMENDED on this date**, from the original 15 percent
   agreement at both quantiles. A percentage tolerance on a sub-millisecond
   quantity is tighter than the deltas the experiment publishes. The 1.50ms
   ceiling is calibrated against six historical matrices and rejects two of
   them, and that calibration table is published with the band.

Both amendments are recorded with their evidence in
`benchmarks/results/README.md` rather than applied quietly, because both were
made after runs failed the original form.

**Measured facts recorded because they shape the methodology.** Token estimation
is linear at roughly 125us per KB with `o200k_base`, so the enforcement path
crosses the 500us target near a 4KB prompt, which is why payload size is a
controlled dimension. **THAT CROSSOVER IS WRONG and is SUPERSEDED by the entry
above**, which measured it end to end through the real binary: it was derived from
one tokenizer pass where the shipped code makes two, and 500us is crossed near 2KB
rather than 4KB. The sentence is left standing rather than edited because this is a
historical record. The conclusion it supports, that payload size must be a
controlled dimension, holds a fortiori. Levee's first enforce-mode request costs
about 130ms
against about 4ms steady, roughly 100ms of it the one-time encoder build, which
is why warmup exists. The two extra log lines an enforced request writes cost
about 2.2us of a roughly 25us measured shift, measured per run by the
`benchmarks/harness/logcost` benchmark rather than hardcoded. Time to first byte
sits at 60 to 85 percent of full-stream duration because the mock replays events
with no pacing, which is a property of the mock and not of levee.

**Provenance.** Each run writes a MANIFEST last, after the bands pass and the
identity audit is clean, recording the tool versions, the host, the sysctls, the
per-cell power and load and thermal readings, the fixture digests, and both the
commit SHA and the tree hash. The tree hash is the field that survives this
project's squash merges.
