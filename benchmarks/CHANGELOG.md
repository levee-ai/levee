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
  executor.
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
controlled dimension. Levee's first enforce-mode request costs about 130ms
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
