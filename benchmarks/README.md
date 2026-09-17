# Levee benchmark harness

This harness measures how much latency levee adds to an LLM API call, and how
much of that is budget enforcement rather than pure forwarding. It answers those
two questions with committed evidence, so the latency claims in the top-level
README are checkable by a stranger rather than asserted.

It runs entirely on loopback against a mock upstream that replays committed
response fixtures. **It needs no provider API keys, ever.** Nothing here talks to
OpenAI or Anthropic, no key is read from the environment, and adding one would
not make the measurement more realistic, it would only make it unrepeatable.
The fixtures were captured and sanitized once and live in `testdata/fixtures`.

## Quick run

Prerequisites, all of which the harness checks at startup and names on failure:

- Go, the version in `go.mod`. The harness builds both the levee binary and the
  mock from the current tree at run start.
- k6. Pinned by version, see "Obtaining the pinned k6" below.
- uv, for the figure scripts.
- macOS. The environment capture uses `pmset`, `sysctl` and `caffeinate`.

```
make bench-overhead
```

About five minutes, 4 minutes 47 seconds on the reference host. It builds the
binaries, boots the mock, runs the eight-cell quick matrix, evaluates the
pre-registered validity bands, writes a results directory under
`benchmarks/results/`, and renders the overhead figure from it. `make
bench-enforcement` is the same run rendering the enforcement figure instead.

Quick mode is a smoke check on the machinery, not evidence. It uses one
repetition, which cannot resolve the across-repetition spread of anything, and it
warns rather than refuses when the host is too busy to measure on. It does run the
primary enforcement gate, at 4096B, because a local check that cannot exercise the
primary gate is not worth much. Roughly 10 minutes and 13 cells.

The publishable run is:

```
RESULTS_MODE=evidence make bench-overhead
```

Roughly an hour and 53 cells, five repetitions across three payload sizes, and it
refuses to start on a dirty tree, on battery, with Low Power Mode on, or on a host
below the CPU idle floor. It also refuses to continue when the host STAYS busy part way
through, meaning two consecutive sub-floor idle readings or more than a tenth of all of
them. A single isolated dip is recorded rather than fatal, and the repetition it landed
on is excluded from every median. That distinction was added on 2026-09-16 after the
strict form aborted an evidence attempt at 51 minutes 54 seconds on one reading of 106,
and it does not lower the floor.

### Verifying a figure without generating load

```
make figures RESULTS_DIR=benchmarks/results/<dir>
```

This re-renders every figure from the committed rows in that directory and
prints the full numeric tables to stdout. It generates no load and needs neither
k6 nor Go, only uv. This is the command to reach for when the question is "is
that figure actually what the data says". CI runs it on every pull request
against the newest committed evidence directory, so a figure that no longer
follows from its data fails the build.

`benchmarks/results/README.md` documents the directory naming, the commit
policy, the five pre-registered bands with their six recorded amendments, the
host quiescence gate, the A/A control, the record of the four evidence attempts that
did not produce publishable numbers, and the identity rules for committed artifacts.

## Detailed methodology

### What is measured, and the estimator

Three cells are compared, all against the same mock over the same loopback
HTTP/1.1 path:

- **direct**, the load generator straight to the mock. This is the floor and it
  contains no levee.
- **passthrough**, through levee with an agent configured in passthrough mode.
  Adds one proxy hop and no budget work.
- **enforce**, through levee with an agent configured in enforce mode. Adds
  token estimation, budget admission and reconciliation on top of the hop.

**"P99 overhead" means P99(proxied) minus P99(direct). It is a quantile shift.**
It is NOT the P99 of per-request overhead, which is unmeasurable without paired
samples, because no request exists in both arms. Tenet 1 reads colloquially like
the latter, so every claim derived from this harness is worded to match the
estimator that produced it. Presentationally both absolute distributions are
always shown. A bare subtracted number was considered and rejected, since it
hides which arm moved.

**Every quoted number carries its payload size and its arrival rate.** An
unqualified "under 500 microseconds" is forbidden here, for the reason in the
payload section below.

### Why open-loop

A closed-loop load generator waits for each response before sending the next
request. Under that model a slow system silently receives less load, so its
measured latency understates what a real client population would see. That is
coordinated omission, and it is the standard way a latency benchmark flatters
the system under test.

k6's `constant-arrival-rate` executor is an open model. It starts iterations on a
fixed schedule regardless of whether earlier ones finished, so the arrival rate
is held by construction rather than by hope. When the system cannot keep up, the
executor runs out of virtual users and DROPS iterations rather than stretching
its own inter-arrival times.

Dropped iterations are therefore the signal that the open model was violated,
and the harness turns a material number of them into a failed run instead of a
caveat in a write-up:

```
'dropped_iterations{scenario:steady}': ['count<=' + MAX_STEADY_DROPPED_ITERATIONS]
'http_req_failed{scenario:steady}':    ['rate<=' + MAX_STEADY_FAILED_RATE]
'http_reqs{scenario:steady}':          ['count>=' + MIN_STEADY_REQUESTS]
```

All three carry a number `run.sh` derives from the cell's own demand, so a
9000-iteration cell is not held to a standard three times tighter than a
30000-iteration one.

`MIN_STEADY_REQUESTS` is the demanded rate times the steady seconds, less a **2
percent** tolerance. That margin is roughly 100 times the legitimate envelope, a
healthy cell delivering 10001 rows against 10000 demanded here, and roughly a
twenty-fifth of the 49 percent shortfall a failed attempt recorded. It cannot
fire on window-boundary effects and it cannot miss saturation.

**AMENDED 2026-09-17. The first two were `count==0` and `rate==0` and both are now
tolerances.** The standard did not soften. An absolute zero cannot survive
aggregation, and this was the third evidence run lost to a gate of that shape.
The run died at cell 38 of 53 on a cell that dropped **one steady iteration of
30001**, 0.003 percent, while reporting p99 2.239ms, 500.0 of 500 rps demanded,
zero warmup drops, zero failed requests, and 76 of 76 host idle readings above
the floor. An evidence run demands 1,224,053 steady iterations, so a rule
requiring exactly zero occurrences across them is a lottery rather than a quality
bar.

- **Drops: 1 percent of demanded steady iterations, floor 25.** Calibrated against
  every cell this repository has recorded, 151 of them carrying 1,438,074 steady
  requests. Seven had nonzero steady drops, worst 0.490 percent, and the two worst
  cannot be saturation at all: 46 drops in a **direct** cell with no levee in its
  path, and 49 in a 150-byte enforce cell at 11 percent of measured capacity. So
  the tolerance sits 2.0 times above the measured benign envelope and **48.7 times
  below the failure it was written for**, the 32768-byte capacity problem that
  dropped 14622 of 30001.
- **Failed requests: 0.05 percent of demanded steady iterations, floor 5.**
  Twenty times tighter in relative terms, because a failed request is levee
  answering 429, erroring, or the loopback stack breaking, while a dropped
  iteration is only the load generator giving up. Zero failures have ever been
  recorded here, which is exactly why the absolute form still had to go: zero
  events in 1,438,074 trials bounds the per-request rate at 2.083e-6 at one-sided
  95 percent confidence, which is up to **2.55 expected failures per evidence
  run**. Every failure shape worth catching is sustained, a one-second 429 storm at
  500 rps being 500 failures against an allowance of 15.

**The drop count is not redundant against the achieved-rate gate**, which is the
first thing to check when relaxing it. Drops subtract from completions one for
one, measured exactly on two recorded cells, so anything above 2 percent already
fails the rate floor and a drop tolerance at or above 2 percent would add
nothing. At 1 percent it catches a shape the rate gate structurally cannot: a
drop proves the VU pool had **no free slot at a scheduled arrival**, so the pool
was momentarily part of what the cell measured. Measured on the real script
against an upstream that blocks the whole pool once, 500 rps over 5 seconds: a
120ms stall dropped 23 of a 25 allowance and both gates passed, while a 150ms
stall dropped 39 and failed the drop gate with the rate gate still clean at 1.5
percent short.

k6 exits **99** on a threshold failure, `run.sh` records the exit code of every
cell in `k6-exit-codes.txt`, and each cell's `summary.json` records the
per-threshold outcome **and the allowance it was held to**, so a stranger
inspecting a committed directory does not have to trust that the harness enforced
anything, and can apply the old absolute rule by hand.

All three thresholds are scoped to the **steady** scenario, deliberately, and
tightening the drop one back to a bare `dropped_iterations` would break every
enforce cell. The reason is cold start, see below. The committed artifact is
steady-scenario rows only, so a gate covers exactly the window whose numbers get
published, no more and no less. **Warmup counts are untouched by the tolerance
amendment**: they were already tolerated by design and they are still recorded
separately, so cold-start cost lands where it is expected and a climbing count
stays visible.

### Cold start, warmup, and why the drop threshold is scoped

Levee's **first** enforce-mode request costs about 130ms on the reference host,
of which roughly 100ms is the one-time construction of the tiktoken
`o200k_base` encoder, against about 4ms for a warm request in that same probe.
This is a product finding, and it is recorded here because it determines two
harness decisions.

What the comparison establishes is the ratio, roughly 30 to 1 between the first
enforced request and a warm one, not the 4ms itself. The steady-window medians
this harness publishes are a few hundred microseconds, measured under sustained
load rather than one request at a time, so the 130ms is a cold-start artifact and
not a claim about steady latency.

At 500 rps the 40-slot VU pool is entirely blocked for that first 130ms while
arrivals continue on schedule, so k6 drops roughly 25 iterations inside the first
52ms of the run. That is deterministic, not flaky. An
unscoped drop threshold would fail every enforce cell of every run while the
steady window was pristine, measured in the same run at zero steady drops and a
4.341ms steady maximum.

So each cell runs two scenarios: `warmup` for 10s starting at 0, and `steady`
starting at 12s. Absorbing cold start is precisely what warmup exists for. The
warmup scenario runs at the SAME rate as steady, so the encoder cache, the
upstream connection pool and the garbage collector are genuinely warm rather
than partially cold when the published window opens. Scenario separation uses
k6's automatic `scenario` tag, never timestamp arithmetic.

### The cells

The matrix is `{direct, passthrough, enforce}` by `{non-streaming, streaming}`,
plus a payload dimension and an A/A control pair, all run by ONE `run.sh`
invocation against one boot of the mock. Levee is started and stopped per proxied
cell, from a single binary built once from HEAD at run start.

- **An A/A control pair per repetition at 150B**, `controla` and `controlb`, both
  running the **passthrough** config so their repetition-matched P50 shift has a
  known true value of zero. It is the estimator's own noise floor, measured in the
  same run that publishes a number, and it is reported rather than gated. See the
  validity gates below.
- **Payload sizes.** Evidence mode runs 150B, 4096B and 32768B for the proxied
  arms. Quick mode runs **150B and 4096B**, the second of those because the primary
  enforcement gate lives at 4096B and a local check that cannot exercise the
  primary gate is not worth much. Direct cells run at 150B, 4096B and 32768B.
- Cell count: **13 in quick mode and 53 in evidence mode.**

- Rates: **per payload size**, set from that size's measured capacity, in
  `rate_for_payload` in `run.sh`. 500 rps at 150B and at 4096B, **150 rps at
  32768B**, and 250 rps for the streaming cells at 150B. One utilization point
  per cell, see the limits section, and see the capacity section below for why
  the 32KB rate is a fifth of the rest.
- **One 40-slot VU pool in every cell**, fully preallocated, direct and
  passthrough and enforce alike. 40 rather than 50 because
  `internal/budget/store.go` hardcodes 50 admission slots per agent and an
  exhausted slot answers 429 rather than queueing, so an enforce pool must stay
  strictly under it. Equalizing therefore brings the other two arms DOWN to 40
  rather than lifting enforce up. Under any queueing the pool size is part of
  what a latency number measures, so two arms with different pools were not
  comparable, which is why they no longer differ.
- Steady duration 20s in quick mode, 60s in evidence mode.
- Request bodies mirror the fixture chat request, model id
  `gpt-4o-mini-2024-07-18` and `max_tokens: 16`, so the pricing table
  prefix-resolves the model and the reservation estimate stays small.
- OpenAI cells only. The mock also serves the Anthropic routes and they are unit
  tested, but they are not measured, see the rejected list.
- `discardResponseBodies: true` everywhere. The socket is still drained to EOF,
  so the streaming metric of record remains duration to last byte.

The mock replays each fixture with one write and one flush per SSE event and no
pacing, which produces distinct TCP segments so the proxy scanner performs real
per-event forward-and-flush cycles. It never logs per request, bounds request
body reads, and refuses to bind a non-loopback address.

### The payload dimension

The enforcement path tokenizes the whole prompt. The tokenizer itself is linear
and holds no surprises: measured on the reference host at the pinned tiktoken
with `o200k_base`, which is what `gpt-4o-mini` resolves to, it costs roughly
**120ns per byte from 150B all clear through to 64KB**, with no superlinear
region anywhere in that range. The repeated filler this harness generates is not
a pathological input either. Realistic prose costs about **13 percent MORE** per
byte, so the filler understates rather than flatters, and `buildPrompt` is
deliberately left as it is.

Measured precisely, on the **exact request body this harness sends** rather than on
a nearby fixture, one estimator pass costs:

| prompt | one pass | per prompt byte | allocations |
|--------|----------|-----------------|-------------|
| 150B   | 17,364ns | 116ns           | 12,936 B in 163 allocs |
| 4096B  | 423,640ns | 104ns          | 330,552 B in 3,836 allocs |
| 32768B | 3,433,743ns | 106ns       | 2,811,705 B in 30,471 allocs |

**The shipped code makes TWO of those passes per enforced request.** That doubling is
the single most important thing to hold onto when reading any enforcement number
here, and a stale constant that missed it is what put a wrong figure into
`check_bands.py` for several days. The committed `microbench.txt` reads about 8,270
ns/op for `BenchmarkEstimate_OpenAI`, which is **not comparable** to the table above:
that benchmark uses its own shorter prose body and builds its estimator with
`cl100k_base`.

**CORRECTED 2026-09-16.** This section previously published a per-KB tokenizer
figure and derived a 500us crossover near a 4KB prompt and a 1ms crossover near
8KB. Both were wrong by roughly a factor of two, in the direction that
understated enforcement. They came from ONE tokenizer pass, and **the shipped
code makes TWO**. The replacement numbers below are end-to-end deltas measured
through the real binary, so they include both passes and everything else the
enforcement path does.

Measured on the reference host at concurrency 1 against the real `levee serve`
binary, medians, so these are SERVICE TIMES and not quantiles under load:

| prompt | passthrough | enforce | enforcement delta |
|--------|-------------|---------|-------------------|
| 150B   | 0.165ms     | 0.218ms | **53us**          |
| 4096B  | 0.191ms     | 1.365ms | **1.174ms**       |
| 32768B | 0.231ms     | 7.512ms | **7.281ms**       |

That delta runs **222 to 287ns per prompt byte** across the range, the lower
figure being the incremental slope from 150B to 32768B and the upper the whole
delta at 4096B divided by its 4096 bytes. Solving for the tenet thresholds
against both ends of that range:

- **500us is crossed between 1744B and 2167B**, so near a **2KB** prompt. The
  old text said 4KB.
- **1ms is crossed between 3489B and 4424B**, so near a **4KB** prompt. The old
  text said 8KB.

A pending product fix removes the duplicate tokenizer pass. Since the two passes
are roughly half the delta at these sizes, that fix should roughly HALVE these
figures and move both crossovers back out by roughly a factor of two, which is
approximately where the old incorrect text already had them. That is a
coincidence of arithmetic and not a reason to leave the old numbers standing: the
published claim has to describe the shipped code, and this correction will be
superseded by a re-measurement rather than by reverting.

**The Tenet 1 consequence is INFERRED, not measured.** Tenet 1 targets under
500us for the full enforcement path, and the table above says that budget is
exhausted at roughly 2KB of prompt while realistic agent contexts run to tens of
KB. The inference step is the one to keep visible: these are concurrency-1
service-time medians, while Tenet 1 is worded against proxy overhead as a P99
quantile shift under load, and no measurement here establishes the latter from
the former. The matrix cells are what measure the quantile shift, and the
crossover above is what tells a reader which payload sizes to look at.

**Two 150-byte numbers appear in this file and they are not the same quantity.** The
53us in the table above is a concurrency-1 service-time delta. The **+15us** figure
quoted with band 3 is a P50 quantile shift between two cells at 500 rps on a quiet
host, and it is the smaller of the two because the shared request path measurably
runs faster in the enforce arm under load, a 24us credit against 38.2us of gross
enforce-only work. **That credit is measured and is not attributed to a named
cause**, so the gap between 53us and 15us is described here rather than explained.
The matrix cells and the bands publish the quantile shift, which is the quantity
Tenet 1 is worded against.

A single 150-byte fixture prompt would publish "enforcement adds about 15
microseconds" as evidence for a claim any user could falsify in minutes. So:

- Enforce and passthrough cells run at three prompt sizes in evidence mode, the
  fixture request at about 150B, 4KB, and 32KB, and at 150B and 4KB in quick mode.
- Direct cells run at 150B, 4KB and 32KB, which proves the generator and the mock
  are payload-insensitive and that any payload effect belongs to levee.
- The enforcement figure plots the measured estimation cost against prompt size
  with the 500us line drawn, so the crossover is VISIBLE rather than hidden.
- **The primary enforcement gate is at 4KB, not at 150B.** The payload dimension
  turned out to matter for a second reason nobody planned for: the 150-byte signal
  is 15us against a 4us estimator noise floor, which is too tight to gate
  reliably, while the same measurement at 4KB is 655us against the same floor. The
  150-byte delta is still recorded on every run.

Larger prompts are synthesized by padding the user message with deterministic
filler. Byte sizes are recorded in the MANIFEST. Response fixtures are
unchanged, because the response side is not the variable under test.

### Enforced throughput is bounded by tokenizer CPU, and is retrograde past the knee

This is both a limitation on the methodology and **a result in its own right**,
so it is published rather than buried. It was found by an evidence run failing,
and the failure is the interesting part.

Levee needs about **10.94ms of CPU per enforced 32768B request** against
**0.233ms for a passthrough one**, a **47-fold spread**. Almost all of the
difference is tokenizer work on the prompt. That single number is what makes one
global arrival rate impossible: a rate that leaves the passthrough arm idle
saturates the enforce arm at the same payload size.

Those two figures are no longer taken on trust. `run.sh` now samples them every
run, and the first matrix to carry the sampling reproduced both independently from
`ps` cputime deltas: **11.32ms per enforced 32KB request against 0.241ms per
passthrough 150B request, a 47.0-fold spread**. Per-cell values are in
`cpu-seconds.txt` and the implied busy core count is in `bands.txt`, so a reader
never has to accept this paragraph as an assertion.

Measured capacity at a 32KB prompt on the reference host, enforce mode, with the
shipped double tokenizer pass:

| offered concurrency | achieved throughput |
|---------------------|---------------------|
| 4                   | **about 400 rps**, the peak |
| 8                   | 295 rps             |
| 40                  | 260 rps             |

**Throughput goes RETROGRADE past the knee.** More concurrency buys less work,
not more, which is the signature of contention rather than of a flat ceiling.
Passthrough at the same payload has capacity above 7100 rps, and enforce at 4096B
has capacity around 2706 rps, so the collapse belongs to tokenizer CPU at large
prompts and not to the proxy hop.

Two consequences, both now enforced in code rather than remembered:

- **Arrival rates are per payload size**, from `rate_for_payload`, each sized to
  roughly 40 percent of that size's measured capacity or lower. The 32KB cells
  run at 150 rps against the 400 rps peak. Sizing against the peak rather than
  against the 260 rps a saturated pool achieved matters, because the retrograde
  region means a saturated cell reports a capacity number that is itself a
  consequence of the saturation.
- **A cell that misses its demanded rate cannot publish a latency number.** See
  the validity gates below.

The failure that produced this is worth stating concretely, because it is what a
reader of an older number needs in order to distrust it. An evidence attempt
demanded 500 rps at 32768B enforce, dropped 14622 steady iterations of 30001,
**48.7 percent**, exited 99,
and reported a **152.4ms median**. The honest service time there is **8.5ms**.
The remaining 144ms was queue residence, and Little's Law closes the gap exactly:
40 requests in flight over the 260 rps actually achieved is 154ms, against 152.4ms
observed. Nothing was wrong with levee. The cell was asked for more than the
machine could do, and a latency measurement of an over-demanded system is a
measurement of its queue.

**What this does NOT establish.** These capacity figures are one host, one
payload size per figure, and the shipped double pass. They are enough to size an
arrival rate and to state the shape, and they are not a capacity model. There is
still no rate sweep in the matrix, so the knee is known at 32KB enforce and
nowhere else.

### Repetition, pairing, and the drift canary

The enforce-minus-passthrough signal is tens of microseconds, which is smaller
than plausible drift between two cells run minutes apart on an unpinned laptop.
Single sequential cells cannot support that claim. So:

- The passthrough and enforce cells for a given payload size run **back to back
  as a pair**, which cancels slow drift inside the pair.
- The pair repeats N times, five in evidence mode and one in quick mode.
- The published delta is the MEDIAN of the per-repetition deltas, with the spread
  across repetitions shown beside it. If the spread exceeds the delta, the run
  cannot resolve the signal and the answer is more repetitions.
- A direct-to-mock cell runs at the START and at the END of the whole matrix as
  a drift canary. If the two disagree beyond the band 5 ceilings, the machine
  moved underneath the experiment and the entire run is invalid regardless of
  every other gate.

### Structured logging is inside the shift, and its marginal cost is not detectable

An enforced request writes three structured log lines where a passthrough
request writes one, so two extra lines are inside the measured
enforce-minus-passthrough delta. That is deliberate, and it cannot be avoided:
levee hardcodes its log level and has no logging configuration section, so the
two cells cannot be equalized by configuration.

**CORRECTED 2026-09-16.** This section, and band 3's own failure message, used to
treat those two lines as a pollution source to accommodate and as the leading
suspect whenever the enforcement delta came in high. Since configuration could not
equalize the arms, the experiment was run instead: the passthrough arm was forced to
write the same two lines. Its P50 moved by **0.0us** and its CPU by **0.0000
milliseconds per request**. So the marginal cost of the two lines is **not
detectable at the P50**, and it never explained a high reading. The work is still
real, 2.1us of CPU by the per-run `logcost` benchmark, so it stays inside the
enforcement figure and is still annotated separately there. What it is not is an
explanation for a shift it cannot produce, and treating it as one sent an
investigation looking in the wrong place.

The cost is measured per run rather than hardcoded. A tiny benchmark package,
`benchmarks/harness/logcost`, replicates the shipped logger exactly, a JSON
handler at info level writing to a real `/dev/null` file descriptor, and
benchmarks the real call-site attribute shapes plus the one-line and three-line
composites. `run.sh` runs it on the same host during the same run and stores the
output in `microbench.txt`, and the enforcement figure parses that file. There
are no baked-in microsecond constants in the plot scripts, because a constant
would be another machine's number presented as a measurement of this one.

Measured on the reference host, the two extra lines cost about **2.1us of CPU**,
against 38.2us of gross enforce-only work at the 150-byte payload, so logging is a
minority component of the work even though its effect on the P50 is unresolvable.
The figure annotates it separately anyway, so the published number is never mistaken
for pure enforcement work. Destination dominates encoder for this cost: a real
`/dev/null` descriptor is materially more expensive than `io.Discard`, while the
text-versus-JSON handler choice moves it only a few percent.

That last point has a consequence for the in-repo reference number. The
`BenchmarkProxy_NonStreaming` benchmark under `internal/proxy`, which is the
anchor the expected proxy-hop shift was sized against, builds its logger as a
TEXT handler writing to `io.Discard`. It therefore carries roughly 60 percent of
the shipped logging cost, and understates it by about 1.4us. That reference
number is **not logger-identical to the shipped path**. It is close enough to
anchor an expectation and not close enough to quote as the shipped cost.

### Streaming, time to first byte

Streaming cells report two series, time to first byte from `http_req_waiting`
and duration to last byte from `http_req_duration`. Both were probe-verified
against a real SSE stream before being adopted as the metrics of record, with
`http_req_waiting` matching time to first byte to the microsecond.

**Time to first byte sits just below full-stream duration BY CONSTRUCTION**,
measured between 60 and 85 percent of duration across the streaming cells,
because the mock replays every stored event with no pacing. There is no
generation time to wait through. Against a real provider that gap would open to
the width of the generation itself.

This is stated plainly because the compression is a property of the mock, not a
property of levee, and a reader who mistook it for the latter would draw exactly
the wrong conclusion. Both series are reported with their real magnitudes, and
the figure prints the per-cell share so it can be checked.

Paced SSE emission was considered and rejected, see the rejected list.

### Validity gates

Five pre-registered sanity bands are evaluated mechanically from the committed
per-request CSVs by `benchmarks/plots/check_bands.py`, a verdict is written to
`bands.txt` in the results directory, and a violation exits loudly. A run that
violates a band is debugged, never published, and a band is never widened to
make a run pass.

**The RATE gate runs before all five of them**, because a band comparing two
cells only means something once both are known to have reported service time
rather than queue residence, and that is the check which establishes it. It
re-derives each cell's achieved throughput from the committed rows, compares it
against the demanded rate recorded in the MANIFEST and in every `summary.json`,
and fails the run when any cell is more than 2 percent short. It also cross checks
the committed row count against k6's own steady request count, so a summary that
disagrees with the published rows cannot pass silently. Per-cell figures land in
`achieved-rate.txt` as well as in the `bands.txt` inventory table.

**The integrity gates carry tolerances rather than absolute zeros, amended
2026-09-17.** `check_bands.py` re-derives each cell's dropped-iteration and
failed-request allowance from the same basis-point arithmetic `run.sh` uses,
compares the recorded raw counts against it, and cross checks its own figure
against the allowance k6 recorded. That half of the amendment is not optional:
leaving an absolute zero in the band checker would have re-failed at the END of
the matrix exactly the cell k6 had just correctly tolerated, which wastes the
whole 52 minutes instead of stopping where it happened. `bands.txt` prints an
`INTEGRITY TOTALS` line naming the run's total steady drops and failed requests
with their percentages, because a rule that replaced an absolute zero has to show
its own aggregate. Every raw count is printed whether it passed or not.

**The host quiescence gate runs before the RATE gate, and before any measurement at
all.** It is the newest gate and the one with the sharpest lesson behind it. Every
band and the RATE gate read artifacts a run already produced, so neither can tell a
quiet host from a busy one: the first completed evidence run passed every integrity
check, served 100.0 percent of its demanded rate in all 43 cells, and still measured
an enforcement cost eight times too high because the machine was contended.
`run.sh` now samples system-wide CPU idle percentage into `machine-state.txt` twice
per cell, and an evidence run refuses to START below **60 percent idle** on a single
reading. Load average is still recorded and is deliberately **not** gated: it was
proven not to discriminate, reading 4.0 to 6.4 during the invalidated run against
2.8 to 5.0 during the quiet re-measurements that corrected it. The calibration, the
sampler, and what the gate cannot catch are all in
`benchmarks/results/README.md`.

**MID-RUN the refusal needs SUSTAINED contention, amended 2026-09-16.** Two consecutive
sub-floor readings abort the run where they happened, and more than a tenth of all
mid-run readings aborts it after the last cell, before the MANIFEST is written. One
isolated dip does neither. The reason is arithmetic rather than tolerance: an evidence
run takes 106 of these readings, ambient single samples on the reference host reach
59.28 against a floor of 60, and the strict form duly aborted an evidence attempt at 51
minutes 54 seconds on the single sub-floor reading in 79. A gate nobody can satisfy gets
deleted, which loses the protection entirely. **The floor did not move**, because
contended single samples reach 58.77 and only the paired or median form separates the
two regimes. An isolated dip instead lands in a new artifact, `contended-cells.txt`, and
`check_bands.py` DROPS that repetition out of every median it computes, failing with the
cause named when fewer than three clean repetitions of five survive. That is what five
repetitions are for.

**BAND3-STREAM is the one exception to that minimum, amended 2026-09-17.** The streaming
matrix runs three repetitions, so a minimum of three permitted ZERO contended streaming
repetitions across the 12 host idle readings its pairing spans, which is the same
absolute-zero-across-many-opportunities defect the integrity tolerances were amended to
remove, and it fired at the end of the matrix so it cost all 52 minutes. Pooling the two
evidence attempts that carry idle readings puts that at 7.5 percent of runs. The band now
applies its ceiling over whatever repetitions survived and prints a loud THIN MEDIAN line,
which is defensible only because that ceiling is a two-sided 0.60ms bound on a 15us
quantity whose central value the band already refuses to publish. Zero clean streaming
repetitions still fails. The non-streaming minimum is unchanged at three.

**Exercising the quiescence rules without paying for a matrix.** `run.sh` can be
SOURCED, in which case it defines every function and runs nothing, and
`LEVEE_BENCH_SYNTHETIC_IDLE_READINGS` then feeds a whitespace-separated list of idle
percentages to the sampler, one value per call. A driver of a dozen lines can therefore
walk `record_machine_state` and `enforce_quiescence_breach_budget` through any breach
pattern in seconds. Note that a sub-floor first sample consumes THREE values, because
the confirmation re-samples twice and returns the median, while a clean one consumes
one. The hook **cannot affect a real run**: `preflight` refuses to start in either mode
while the variable is set, and `preflight` is on the only path that creates a results
directory.

**An A/A control pair states the estimator's noise floor in the same run that
publishes a number.** Two cells per repetition at the small payload, both running
the passthrough config, so their repetition-matched P50 shift has a known true value
of zero. `bands.txt` prints it beside the enforcement readings. It is reported and
never gated, because a contended A/A pair still read 13us: it is necessary and not
sufficient, and the gate against contention is the quiescence floor above.

The bands, their six recorded amendments, the calibration behind the amended
tail ceilings, and the invalidation rules are all in
`benchmarks/results/README.md`. They are published there rather than only in the
design document because pre-registration only functions as discipline if the
bands are public before the numbers are.

**Band 1 carries a PER-GROUP ceiling as of 2026-09-17**, one calibrated P50 gate and
one recorded P99 advisory for each response mode and payload size it judges: 1.0ms
and 2.5ms non-streaming at 150B and at 4096B, 1.5ms and 3.5ms non-streaming at
32768B, 2.0ms and 5.5ms streaming at 150B. Each of the eight is that group's own
measured median times a single multiplier shared across all four groups, rounded down,
with the multiplier fixed by the 1.0ms ceiling this band has always carried divided by
that group's own median. So no group has a number of its own and the anchor is
unchanged by construction.

The reason is that duration to last byte is a **different quantity** per group rather
than the same quantity measured repeatedly. A streaming response replays six SSE
events with a write and a flush each, and a 32768-byte response moves 218 times the
bytes of a 150-byte one, so one ceiling cannot serve all of them. Band 1 was amended
twice in two days for this, on the response mode axis and then on the payload size
axis, and the second amendment replaces both special cases with the one rule. The
fourth evidence attempt completed all 53 cells and was invalidated by the unsplit
form. Band 1 also reads `contended-cells.txt` as of this amendment, and only to
annotate a failure, never to excuse one. The derivation, the per-group evidence table
and the standing calibration-provenance requirement are all with the band.

Orchestration safety is part of validity, not separate from it. `run.sh` traps
on exit and kills its whole process group, asserts every port is free before
using it with the occupying PID named on failure, polls both readiness
endpoints, and requires levee's admin `/health` to report the exact build stamp
of the binary this run compiled before any proxied cell starts. That last check
is what makes an orphaned listener from a previous run impossible to measure by
accident. Percentile arithmetic is linear interpolation between order
statistics, and the figures compute their percentiles through the same function
the gates use, so a figure cannot publish a number the gate never saw.

### Known asymmetries and limits

Stated rather than smoothed over, because each one bounds what the numbers
support.

- **HTTP/1.1 idle-connection churn on the levee-to-mock leg.** Levee's upstream
  transport keeps the standard library default of two idle connections per host,
  so at the 500 rps the 150B and 4096B cells run at, that leg opens and closes
  connections in a way the direct
  baseline does not. The default is read from the standard library rather than
  separately probed, so treat the churn magnitude as inferred, not measured. It
  is a real asymmetry either way, and it is correctly charged to the proxy rather
  than subtracted out, because it is what levee actually does. The
  TIME_WAIT count is recorded at every cell boundary in `machine-state.txt`, and
  the next cell waits for it to fall so one cell's churn cannot bleed into the
  next.
- **k6's `http_req_duration` excludes connection acquisition.** Generator-side
  connection setup is invisible in both baselines, which is what makes them
  comparable. Levee-to-mock churn lands inside proxied `http_req_waiting`, so it
  is attributed to the proxy, which is correct.
- **Arrivals are evenly spaced, not Poisson.** A constant-arrival-rate executor
  spaces requests uniformly. Real traffic arrives in bursts, and bursty arrivals
  queue. So the tails here understate what a bursty client population would see.
- **One utilization point per cell.** There is no rate sweep inside the matrix,
  so a published cell says nothing about where its own knee is. The knee at 32KB
  enforce is known, from the separate capacity measurement in the section above,
  and every rate is now chosen against a measured capacity for that payload size.
  No other cell's knee has been measured, and the ones at 150B and 4096B are
  bounded only from below, by the 2706 rps figure at 4096B.
- **Levee CPU per request is sampled, so saturation is visible rather than
  inferred.** `run.sh` reads `ps -o cputime` for the levee process at the two
  edges of every cell's steady window and records the delta and the per-request
  cost in `cpu-seconds.txt`, and `check_bands.py` prints the implied busy core
  count beside it. Compare that against `cpu_cores` in the MANIFEST. This is a
  report and not a gate, because the gate on saturation is the achieved-rate check
  which measures the consequence directly. Two limits on it: `ps` reports the
  process's own time and not that of any reaped child, which is correct for a
  single-process Go binary and would not be for a supervisor, and its resolution
  is a hundredth of a second, which is immaterial against the smallest delta in
  the matrix at roughly 2.3 CPU seconds.
- **macOS only.** The environment capture is `pmset`, `sysctl` and
  `caffeinate`, and there is no CPU pinning because macOS offers none. A Linux
  reproducer will have to rewrite the environment capture and the power and
  thermal assertions: the sysctl names differ, `somaxconn` and the ephemeral port
  range live elsewhere, and Linux binds the whole loopback range without an
  interface alias where macOS does not. It should expect a different tail, most
  likely a tighter one, since the tail here is dominated by host scheduling noise
  rather than by levee, but that expectation is unverified until someone runs it.
- **The measurement host is not quiet, and this cost one whole evidence run.** The
  reference machine carries resident endpoint-security agents. Load average during
  runs ranged from roughly 5 to 36 on 12 cores across this session, and the
  machine-state readings in the surviving quick runs span 5.07 to 13.32.
  Consequently the P50 is stable at the payload sizes where the signal is large and
  **quick mode is a lottery for the tail**: a single 20-second cell can catch a
  noise burst that moves its P99 by milliseconds. Evidence mode exists for that
  reason, with longer windows, five repetitions, back-to-back pairing, the drift
  canary, and bootstrap confidence intervals on every published percentile.
  **That was not enough.** The first completed 43-cell evidence run measured a
  150-byte enforcement delta of +123us where the quiet-host figure is +15us, and
  every other gate passed. Three consequences now live in code rather than in this
  paragraph: the host quiescence gate refuses a contended evidence run, the A/A
  control states the estimator's noise floor in the same run that publishes a
  number, and the primary enforcement gate sits at the payload size where the
  signal is 160 times that floor rather than 4 times it.
- **P99.9 rests on few observations**, roughly 30 in a non-streaming evidence
  cell and half that streaming, so it is always published with its bootstrap
  interval and should not be read as a point estimate.

### Reproducing a figure

```
git clone <this repo>
make figures RESULTS_DIR=benchmarks/results/<evidence-dir>
```

That needs only uv. The plot scripts are `uv run --script` files with PEP 723
inline dependencies, `requires-python` set, and a COMMITTED per-script lockfile
carrying exact versions and sha256 hashes. `--locked` asserts that lockfile
still resolves, so a drifted dependency fails loudly instead of silently
re-rendering against different library versions. Pinning matplotlib alone would
leave numpy, pillow, contourpy and fonttools floating, which is fidelity drift
now and a resolution failure later.

"Regenerable" means identical statistics and marks, not byte-identical PNGs.
Matplotlib output is not byte-stable across machines and font sets.

Each figure is annotated with its source results-directory name, its levee tree
hash, and the renderer versions, so a figure circulating detached from the
repository stays self-describing.

### Obtaining the pinned k6

Install the **GitHub release binary for the exact version recorded in the
MANIFEST** of the run being reproduced, from the k6 project's own releases page.
Do not use `brew install k6`. Homebrew has no versioned formulae for it, so
`brew install` tracks whatever is current and will drift away from the recorded
version, which silently changes the measurement tool underneath a comparison.

`run.sh` records the full `k6 version` string in the MANIFEST for exactly this
reason, and sets `K6_NO_USAGE_REPORT=true` on every invocation. k6 reports
anonymous usage statistics by default, which is an undisclosed outbound
connection during a run advertised as loopback-only, and an uncontrolled
variable besides. The setting is recorded in the MANIFEST too.

### Considered and rejected

- **Paced SSE emission in the mock.** Sleeping between events to imitate
  generation would add an identical constant to both baselines, so it cannot
  change a difference, and k6 exposes no per-chunk timings that would let the
  pacing itself be observed. The cost would be a slower run and a mock that
  looks more realistic without measuring anything more. The consequence of not
  pacing is the compressed time-to-first-byte gap, which is stated above rather
  than hidden.
- **A lone subtracted number.** Publishing only "levee adds X" hides which arm
  moved and hides the shape of both distributions. Both absolute distributions
  are always shown, with markers and intervals.
- **wrk2 as a second generator.** Dormant since 2024, does not build on
  darwin/arm64 with open pull requests going back years, and has no Homebrew
  formula. A cross-check that cannot be installed is not a cross-check. vegeta
  and oha are the maintained candidates if a second generator is ever wanted.
- **Anthropic measurement cells.** The OpenAI path is the CONSERVATIVE choice,
  not the convenient one. Its tiktoken estimator costs about 9us at fixture size
  and 125us per KB, while the Anthropic character heuristic costs well under a
  microsecond, so OpenAI upper-bounds Anthropic. Measuring only Anthropic would
  have been cherry-picking the fast path. The mock serves the Anthropic routes
  and they are unit tested, so extending the matrix later is cheap.
- **A TLS mock behind a locally installed certificate authority.** It would
  plant a standing CA in the operator's keychain purely to run a benchmark,
  introduce an HTTP/2-versus-HTTP/1.1 asymmetry between the two legs, and make
  the reproduce story heavier for an outside reviewer. Instead a narrowly scoped
  configuration change allows plaintext upstreams only when the host is a
  literal loopback address, which keeps both legs on uniform HTTP/1.1.
- **In-process assembly of levee inside a Go test.** Faster and easier to
  instrument, and it would not test the shipped binary. The whole point is to
  measure `levee serve` as a user runs it, including its flag parsing, its
  config validation, its logger and its real listeners.
