# The estimator and the cell matrix

What quantity this harness measures, and the matrix that produces it. The
thresholds that judge a run are in [bands.md](bands.md), their derivations in
[calibration.md](calibration.md), and what the numbers do not support in
[limits.md](limits.md).

## What is measured

Three cells are compared, all against the same mock over the same loopback
HTTP/1.1 path.

- **direct**, the load generator straight to the mock. The floor, containing no
  levee.
- **passthrough**, through levee with an agent configured in passthrough mode.
  One proxy hop and no budget work.
- **enforce**, through levee with an agent configured in enforce mode. Token
  estimation, budget admission and reconciliation on top of the hop.

"P99 overhead" means P99(proxied) minus P99(direct). It is a quantile shift. It
is not the P99 of per-request overhead, which is unmeasurable without paired
samples, because no request exists in both arms. Tenet 1 reads colloquially like
the latter, so every claim derived from this harness is worded to match the
estimator that produced it. Both absolute distributions are always shown. A bare
subtracted number was considered and rejected, since it hides which arm moved.

Every quoted number carries its payload size and its arrival rate. An
unqualified "under 500 microseconds" is forbidden here, because the enforcement
cost is linear in prompt bytes and spans two orders of magnitude across the
matrix.

## Why open-loop

A closed-loop load generator waits for each response before sending the next
request. Under that model a slow system silently receives less load, so its
measured latency understates what a real client population would see. That is
coordinated omission, and it is the standard way a latency benchmark flatters
the system under test.

k6's `constant-arrival-rate` executor is an open model. It starts iterations on
a fixed schedule whether or not earlier ones finished, so the arrival rate is
held by construction. When the system cannot keep up, the executor runs out of
virtual users and DROPS iterations instead of stretching its own inter-arrival
times.

Dropped iterations are therefore the signal that the open model was violated,
and a material number of them fails the run:

```
'dropped_iterations{scenario:steady}': ['count<=' + MAX_STEADY_DROPPED_ITERATIONS]
'http_req_failed{scenario:steady}':    ['rate<=' + MAX_STEADY_FAILED_RATE]
'http_reqs{scenario:steady}':          ['count>=' + MIN_STEADY_REQUESTS]
```

All three carry a number `run.sh` derives from the cell's own demand, so a
9000-iteration cell is not held to a standard three times tighter than a
30000-iteration one. `MIN_STEADY_REQUESTS` is the demanded rate times the steady
seconds, less a 2 percent tolerance. The two integrity allowances are derived in
[calibration.md](calibration.md#the-integrity-tolerances).

k6 exits 99 on a threshold failure, `run.sh` records every cell's exit code in
`k6-exit-codes.txt`, and each `summary.json` records the per-threshold outcome
and the allowance the cell was held to. A stranger inspecting a committed
directory does not have to trust that the harness enforced anything, and can
apply a stricter rule by hand.

All three thresholds are scoped to the **steady** scenario. The committed
artifact is steady-scenario rows only, so a gate covers exactly the window whose
numbers get published. Warmup counts are recorded separately and are tolerated
by design, for the reason below.

## Cold start and the warmup scenario

Levee's first enforce-mode request costs about 130ms on the reference host, of
which roughly 100ms is the one-time construction of the tiktoken `o200k_base`
encoder, against about 4ms for a warm request in the same probe. What the
comparison establishes is the ratio, roughly 30 to 1, and not the 4ms itself.
The medians this harness publishes are a few hundred microseconds under
sustained load, so the 130ms is a cold-start artifact and not a claim about
steady latency.

At 500 rps the 40-slot VU pool is entirely blocked for that first 130ms while
arrivals continue on schedule, so k6 drops roughly 25 iterations inside the
first 52ms. That is deterministic. An unscoped drop threshold would fail every
enforce cell of every run while the steady window was pristine, measured in the
same run at zero steady drops and a 4.341ms steady maximum.

So each cell runs two scenarios: `warmup` for 10s starting at 0, and `steady`
starting at 12s. The warmup scenario runs at the SAME rate as steady, so the
encoder cache, the upstream connection pool and the garbage collector are
genuinely warm when the published window opens. Scenario separation uses k6's
automatic `scenario` tag, never timestamp arithmetic.

## The cells

The matrix is `{direct, passthrough, enforce}` by `{non-streaming, streaming}`,
plus a payload dimension and an A/A control pair, all run by ONE `run.sh`
invocation against one boot of the mock. Levee is started and stopped per
proxied cell, from a single binary built once from HEAD at run start.

- **Cell count: 13 in quick mode and 53 in evidence mode.**
- **An A/A control pair per repetition at 150B**, `controla` and `controlb`,
  both running the passthrough config so their repetition-matched P50 shift has
  a known true value of zero. It is the estimator's own noise floor, measured in
  the same run that publishes a number. See [bands.md](bands.md#the-aa-control-pair).
- **Payload sizes.** Evidence mode runs 150B, 4096B and 32768B for the proxied
  arms. Quick mode runs 150B and 4096B, the second because the primary
  enforcement gate lives at 4096B and a local check that cannot exercise the
  primary gate is not worth much. Direct cells run all three sizes, which proves
  the generator and the mock are payload-insensitive and that any payload effect
  belongs to levee.
- **Rates are per payload size**, set from that size's measured capacity in
  `rate_for_payload`. 500 rps at 150B and at 4096B, 150 rps at 32768B, 250 rps
  for the streaming cells at 150B. One utilization point per cell. The capacity
  measurement behind the 32KB rate is in [limits.md](limits.md#enforced-throughput-is-bounded-by-tokenizer-cpu).
- **One 40-slot VU pool in every cell**, fully preallocated, in all three arms.
  40 rather than 50 because `internal/budget/store.go` hardcodes 50 admission
  slots per agent and an exhausted slot answers 429 instead of queueing, so an
  enforce pool must stay strictly under it. Equalizing therefore brings the other
  two arms DOWN to 40. Under any queueing the pool size is part of what a latency
  number measures, so two arms with different pools were not comparable.
- Steady duration 20s in quick mode, 60s in evidence mode.
- Request bodies mirror the fixture chat request, model id
  `gpt-4o-mini-2024-07-18` and `max_tokens: 16`, so the pricing table
  prefix-resolves the model and the reservation estimate stays small.
- OpenAI cells only. The mock serves the Anthropic routes and they are unit
  tested, but they are not measured. The OpenAI tiktoken estimator costs about
  9us at fixture size while the Anthropic character heuristic costs well under a
  microsecond, so OpenAI upper-bounds Anthropic and measuring only Anthropic
  would have been cherry-picking the fast path.
- `discardResponseBodies: true` everywhere. The socket is still drained to EOF,
  so the streaming metric of record remains duration to last byte.

The mock replays each fixture with one write and one flush per SSE event and no
pacing, which produces distinct TCP segments so the proxy scanner performs real
per-event forward-and-flush cycles. It never logs per request, bounds request
body reads, and refuses to bind a non-loopback address.

## The payload dimension

The enforcement path tokenizes the whole prompt, and the tokenizer is linear.
Measured on the reference host at the pinned tiktoken with `o200k_base`, which
is what `gpt-4o-mini` resolves to, it costs roughly 120ns per prompt byte from
150B clear through to 64KB with no superlinear region. The repeated filler this
harness generates is not a pathological input: realistic prose costs about 13
percent MORE per byte, so the filler understates, and `buildPrompt` is
deliberately left as it is.

One estimator pass, measured on the exact request body this harness sends:

| prompt | one pass    | per prompt byte | allocations            |
|--------|-------------|-----------------|------------------------|
| 150B   | 17,364ns    | 116ns           | 12,936 B in 163 allocs |
| 4096B  | 423,640ns   | 104ns           | 330,552 B in 3,836 allocs |
| 32768B | 3,433,743ns | 106ns           | 2,811,705 B in 30,471 allocs |

The committed `microbench.txt` reads about 8,270 ns/op for
`BenchmarkEstimate_OpenAI`, which is **not comparable** to that table: the
benchmark uses its own shorter prose body and builds its estimator with
`cl100k_base`.

Larger prompts are synthesized by padding the user message with deterministic
filler. Byte sizes are recorded in the MANIFEST. Response fixtures are
unchanged, because the response side is not the variable under test.

The primary enforcement gate is at 4KB, not at 150B. The payload dimension
turned out to matter for a second reason nobody planned for: the 150-byte signal
is comparable to the estimator's own noise floor, while the same measurement at
4KB is roughly 160 times that floor. The signal-to-noise arithmetic is in
[calibration.md](calibration.md#band-3-the-primary-enforcement-gate). The
150-byte delta is still recorded on every run.

## Repetition, pairing and the drift canary

The enforce-minus-passthrough signal at 150 bytes is tens of microseconds, which
is smaller than plausible drift between two cells run minutes apart on an
unpinned laptop. Single sequential cells cannot support that claim. So:

- The passthrough and enforce cells for a given payload size run **back to back
  as a pair**, which cancels slow drift inside the pair.
- The pair repeats N times, five in evidence mode and one in quick mode.
- The published delta is the MEDIAN of the per-repetition deltas, with the spread
  across repetitions shown beside it. If the spread exceeds the delta, the run
  cannot resolve the signal and the answer is more repetitions.
- A direct-to-mock cell runs at the START and at the END of the whole matrix as a
  drift canary. If the two disagree beyond the band 5 ceilings, the machine moved
  underneath the experiment and the run is invalid whatever every other gate says.

## Streaming, time to first byte

Streaming cells report two series, time to first byte from `http_req_waiting`
and duration to last byte from `http_req_duration`. Both were probe-verified
against a real SSE stream before being adopted as the metrics of record, with
`http_req_waiting` matching time to first byte to the microsecond.

Time to first byte sits just below full-stream duration by construction,
measured between 60 and 85 percent of duration across the streaming cells,
because the mock replays every stored event with no pacing. There is no
generation time to wait through. Against a real provider that gap would open to
the width of the generation itself. The compression is a property of the mock,
and a reader who mistook it for a property of levee would draw the wrong
conclusion. Both series are reported with their real magnitudes and the figure
prints the per-cell share.

Paced SSE emission was considered and rejected. Sleeping between events would
add an identical constant to both baselines, so it cannot change a difference,
and k6 exposes no per-chunk timings that would let the pacing itself be observed.

## Structured logging is inside the shift

An enforced request writes three structured log lines where a passthrough
request writes one, so two extra lines sit inside the measured
enforce-minus-passthrough delta. That cannot be avoided: levee hardcodes its log
level and has no logging configuration section, so the two cells cannot be
equalized by configuration.

Since configuration could not equalize the arms, the experiment was run instead.
The passthrough arm was forced to write the same two lines. Its P50 moved by
0.0us and its CPU by 0.0000 milliseconds per request. The marginal cost of the
two lines is not detectable at the P50, and it never explained a high
enforcement reading. Treating it as one sent an investigation looking in the
wrong place.

The work is still real, 2.1us of CPU by the per-run `logcost` benchmark, so it
stays inside the enforcement figure and is annotated separately there. The cost
is measured per run rather than hardcoded: `benchmarks/harness/logcost`
replicates the shipped logger exactly, a JSON handler at info level writing to a
real `/dev/null` file descriptor, and benchmarks the real call-site attribute
shapes plus the one-line and three-line composites. `run.sh` runs it on the same
host during the same run, stores the output in `microbench.txt`, and the
enforcement figure parses that file. A baked-in constant would be another
machine's number presented as a measurement of this one.

Destination dominates encoder for this cost. A real `/dev/null` descriptor is
materially more expensive than `io.Discard`, while the text-versus-JSON handler
choice moves it only a few percent. That has a consequence for the in-repo
reference number: `BenchmarkProxy_NonStreaming` under `internal/proxy`, the
anchor the expected proxy-hop shift was sized against, builds its logger as a
TEXT handler writing to `io.Discard`. It carries roughly 60 percent of the
shipped logging cost and understates it by about 1.4us. It is close enough to
anchor an expectation and not close enough to quote as the shipped cost.
