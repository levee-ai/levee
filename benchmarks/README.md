# Levee benchmark harness

This harness measures how much latency levee adds to an LLM API call, and how
much of that is budget enforcement rather than pure forwarding. It answers those
two questions with committed evidence, so the latency claims in the top-level
README are checkable by a stranger instead of asserted.

It runs entirely on loopback against a mock upstream that replays committed
response fixtures. **It needs no provider API keys, ever.** Nothing here talks to
OpenAI or Anthropic, no key is read from the environment, and adding one would not
make the measurement more realistic, only unrepeatable. The fixtures were captured
and sanitized once and live in `testdata/fixtures`.

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
binaries, boots the mock, runs the quick matrix, evaluates the pre-registered
validity bands, writes a results directory under `benchmarks/results/`, and
renders the overhead figure from it. `make bench-enforcement` is the same run
rendering the enforcement figure instead.

Quick mode is a smoke check on the machinery and not evidence. It uses one
repetition, which cannot resolve the across-repetition spread of anything, and it
warns instead of refusing when the host is too busy to measure on. It does run the
primary enforcement gate at 4096B, because a local check that cannot exercise the
primary gate is not worth much. Roughly 10 minutes and 13 cells.

The publishable run is:

```
RESULTS_MODE=evidence make bench-overhead
```

Roughly an hour and 53 cells, five repetitions across three payload sizes. It
refuses to start on a dirty tree, on battery, with Low Power Mode on, or on a host
below the CPU idle floor, and it refuses to continue when the host stays busy part
way through. A single isolated dip is recorded instead of fatal, and the repetition
it landed on is excluded from every median.

### Verifying a figure without generating load

```
make figures RESULTS_DIR=benchmarks/results/<dir>
```

This re-renders every figure from the committed rows in that directory and prints
the full numeric tables to stdout. It generates no load and needs neither k6 nor
Go, only uv. This is the command to reach for when the question is "is that figure
actually what the data says". CI runs it on every pull request against the newest
committed evidence directory, so a figure that no longer follows from its data
fails the build.

## Where the rest of it is documented

| If you want                                    | Read                                                              |
|------------------------------------------------|-------------------------------------------------------------------|
| To check a published number against an artifact | [results/README.md](results/README.md)                            |
| What quantity is measured, and the cell matrix  | [methodology/estimator-and-matrix.md](methodology/estimator-and-matrix.md) |
| The validity gates as they stand today          | [methodology/bands.md](methodology/bands.md)                      |
| Where each threshold came from                  | [methodology/calibration.md](methodology/calibration.md)          |
| A gate just fired and you need to act           | [methodology/triage.md](methodology/triage.md)                     |
| What the numbers do not support                 | [methodology/limits.md](methodology/limits.md)                     |
| Which published numbers stopped being comparable | [CHANGELOG.md](CHANGELOG.md)                                      |

## Measured results

Every number in this section comes from one run,
`benchmarks/results/2026-09-17-2a569f4-m3pro-macos-evidence-r1`, **VERDICT VALID**:
53 cells, five repetitions per non-streaming pair and three per streaming pair,
60-second steady windows, zero of 106 host CPU idle readings below the floor, an
empty `contended-cells.txt` ledger, and a clean identity audit. Re-derive all of it
without generating load:

```
make figures RESULTS_DIR=benchmarks/results/2026-09-17-2a569f4-m3pro-macos-evidence-r1
```

The two committed figures are `benchmarks/plots/overhead-<that directory>.png` and
`benchmarks/plots/enforcement-<that directory>.png`.

Every number below carries its payload size and its arrival rate. Intervals are
percentile bootstrap, 2000 resamples at 95 percent, seeded so a re-render
reproduces them.

> **AMENDED 2026-09-17.** Every enforcement figure in this file, and the committed
> evidence run itself, describe commit `2a569f4` and therefore include a DOUBLED
> tokenization that current code does not perform. The duplicate pass is gone:
> commit `f295ed4` (branch commit `b5087fa`) carries the reservation estimate
> forward instead of recomputing it, so an enforced request now tokenizes once.
> Measured effect of the fix on the same host: 42.0 percent faster at a
> 32768-byte prompt, 38.7 percent at 4096 bytes, 14.3 percent at 150 bytes, with
> allocations roughly halved at the two larger sizes. Read the enforcement costs
> below as roughly double what current code pays at 4KB and above, and the payload
> at which the 500 microsecond line is crossed moves out from near 3.4KB to
> somewhere near 6.8KB. These figures are left as written because they document a
> specific artifact, and rewriting them would misdescribe it. A future evidence run
> will replace them rather than amend them.

### The proxy hop

Quantile shift in milliseconds, treatment minus the direct baseline, at a 150-byte
prompt and 500 rps:

| quantile | passthrough             | enforce                 | A/A control cells |
|----------|-------------------------|-------------------------|-------------------|
| P50      | +0.281 [+0.276, +0.286] | +0.332 [+0.327, +0.338] | +0.284 and +0.296 |
| P90      | +0.431 [+0.426, +0.439] | +0.458 [+0.452, +0.467] | +0.437 and +0.462 |
| P99      | +0.601 [+0.549, +0.643] | +0.627 [+0.580, +0.671] | +0.564 and +0.613 |
| P99.9    | -0.207 [-0.563, +0.319] | -0.122 [-0.520, +0.369] | -0.223 and +0.133 |

**The P99 proxy hop costs +0.601ms at 150 bytes and 500 rps, inside the 1ms Tenet 1
budget, with the whole bootstrap interval inside it too.** Adding budget
enforcement at that payload takes it to +0.627ms, still inside.

That is one payload size. Across the three:

| payload and rate  | passthrough P50 shift | passthrough P99 shift   |
|-------------------|-----------------------|-------------------------|
| 150B at 500 rps   | +0.281                | +0.601 [+0.549, +0.643] |
| 4096B at 500 rps  | +0.254                | +0.474 [+0.427, +0.517] |
| 32768B at 150 rps | +0.670                | +1.446 [+1.192, +1.675] |

**The 32KB row is OUTSIDE the 1ms budget, at 1.4 times it, and it is the pure proxy
hop with no budget work in it.** It is stated here rather than left to the figure
because it is the one Tenet 1 exceedance in this matrix that is not about
enforcement at all. Two things bound it: the 32768B direct baseline is a SINGLE
cell, so that shift rests on one baseline instead of a group of five, and the 32KB
cells run at 150 rps for the capacity reason in
[methodology/limits.md](methodology/limits.md#enforced-throughput-is-bounded-by-tokenizer-cpu).
With enforcement on, the same cell's P99 shift is +7.149ms.

At P99.9 every 150-byte shift goes negative and every interval spans zero. That is
a limit of the measurement and not a finding about levee: **no P99.9 overhead claim
is supported by this run, in either direction.** See
[methodology/limits.md](methodology/limits.md#p999-is-not-resolvable-on-this-host).

The A/A control agreeing with the passthrough arm does not make the overhead number
meaningless. What that agreement does and does not license is in
[methodology/limits.md](methodology/limits.md#the-two-number-families-are-not-equally-well-controlled).

### The cost of enforcement over pure forwarding

Median repetition-matched enforce minus passthrough P50 shift in microseconds,
which is the quantity band 3 gates:

| prompt and rate           | reps | shift     | interval     | per repetition                    |
|---------------------------|------|-----------|--------------|-----------------------------------|
| 150B at 500 rps           | 5    | +55       | [49, 62]     | +55, +66, +75, +18, +44           |
| 4096B at 500 rps          | 5    | +605      | [602, 609]   | +608, +601, +613, +605, +596      |
| 32768B at 150 rps         | 5    | +5784     | [5764, 5793] | +5618, +5699, +5810, +5784, +5787 |
| streaming 150B at 250 rps | 3    | see below | [17, 45]     | +15, +97, +31                     |

**4096B at 500 rps is the row to quote**, +605us with a 17us spread across five
repetitions. It is the primary gate's own payload and the only row here whose
spread is small against its own value.

150B at 500 rps is NOT resolved by this run. The 57us spread across repetitions
exceeds the 55us median, and the harness's own repetition rule says a run in that
state cannot resolve its signal. It is recorded, it sits inside its advisory
window, and it is not a publishable central value. Nine runs on this host have read
this quantity between +11 and +90us.

Streaming at 150B and 250 rps is bounded and not measured. Three repetitions read
+15, +97 and +31us. Band 3-STREAM deliberately gates only the ABSOLUTE size of that
shift, against a two-sided 0.60ms ceiling, and refuses to publish a central value.
The honest statement is that streaming enforcement is bounded below 0.6ms at this
payload and rate and its magnitude is unresolved.

**32KB enforcement costs 5.784ms, which is 11.6 times the 500us target.** That is
the largest result in the matrix and it is not marginal: a 32768-byte prompt is an
ordinary agent context, and at that size the enforcement path is the dominant term
in the request, 7.311ms of enforce P50 against 1.550ms of passthrough P50. Halve it
for the doubled-tokenization amendment above and it is still an order of magnitude
over the target.

### Where the 500 microsecond line is crossed

There are two crossings here and neither supersedes the other. Each is only
meaningful with its condition attached.

Under sustained load, group P50s across five repetitions with the paired
enforcement delta beside them:

| prompt and rate   | passthrough P50 | enforce P50 | enforcement delta         |
|-------------------|-----------------|-------------|---------------------------|
| 150B at 500 rps   | 0.839ms         | 0.890ms     | 55us, unresolved          |
| 4096B at 500 rps  | 0.856ms         | 1.457ms     | 605us                     |
| 32768B at 150 rps | 1.550ms         | 7.311ms     | 5784us                    |

The delta column is not the difference of the two columns beside it. It is the
median of the five repetition-matched differences, which is the estimator band 3
gates, and the two disagree by a few microseconds because a median of differences
is not the difference of medians.

Three derivations of the crossing from those three points, all linear in prompt
bytes because the tokenizer is linear in prompt bytes. The measured slope from 150B
to 4096B is 139ns per byte and puts the crossing at 3343B. The whole delta at 4096B
over its 4096 bytes is 148ns per byte and puts it at 3386B. The 4096B to 32768B
slope is 181ns per byte and extrapolating back down from the measured 4096B point
puts it at 3515B. **So under 500 rps of load, 500us is crossed between 3.3KB and
3.5KB, near a 3.4KB prompt, and 1ms between 6.3KB and 6.9KB.** The three disagree
by 5 percent because the loaded curve is slightly convex: a single straight line
through the 150B and 32768B points overstates the measured 4096B delta by 143us, so
the crossing is taken from the segment it actually falls in.

At concurrency 1 against the real `levee serve` binary, medians, so these are
SERVICE TIMES and not quantiles under load:

| prompt | passthrough | enforce | enforcement delta |
|--------|-------------|---------|-------------------|
| 150B   | 0.165ms     | 0.218ms | 53us              |
| 4096B  | 0.191ms     | 1.365ms | 1.174ms           |
| 32768B | 0.231ms     | 7.512ms | 7.281ms           |

That delta runs 222 to 287ns per prompt byte across the range. Solving for the
tenet thresholds against both ends of that range, **at concurrency 1, 500us is
crossed between 1744B and 2167B, so near a 2KB prompt, and 1ms between 3489B and
4424B, so near 4KB.**

**Which figure to use for what.** The loaded numbers are the headline. They are
what the pre-registered bands gate, they are re-derivable from a committed artifact
by a stranger running `make figures`, and Tenet 1 is worded as a quantile shift
under load. The concurrency-1 numbers are for reasoning about a SINGLE request in
isolation, and they are what the component decomposition attaches to, because they
carry no queueing and no host scheduling noise.

**The headline is the SMALLER of the two at 4096B, 605us against 1174us, and that
direction matters.** Tenet 3 forbids under-counting, so anyone sizing a worst case
for one isolated enforced call should take the concurrency-1 column.

**Why the two differ, with the arithmetic.** Between concurrency 1 and 500 rps the
passthrough arm's 4096B P50 rises from 0.191ms to 0.856ms, +0.665ms, while the
enforce arm's rises from 1.365ms to 1.457ms, +0.092ms. The difference between them
therefore falls by 0.573ms, from 1.174ms to 0.601ms, which accounts for the whole
gap between the two conditions. Almost all of the movement is in the passthrough
baseline. The same asymmetry shows up again between the 20-second quick regime and
the 60-second evidence regime at the same rate and payload: passthrough 4096B P50
reads 0.607ms as the median of eight quick runs against 0.856ms here, +0.249ms,
while enforce moves 1.435ms to 1.457ms, +0.022ms. A separate probe measured that
regime effect directly at +0.200ms, again in the passthrough arm with the enforce
arm flat.

**What is NOT established is WHY the two arms do not inflate equally.** The
arithmetic above is measured and reproducible from the committed bands files. A
mechanism is not, and the obvious candidate does not survive on its own: the
levee-to-mock connection churn is on both arms' path, so it cannot by itself
explain a movement that lands in one of them.

The enforcement figure will look like it disagrees, and it does not. Its x axis is
a log scale, so the segment it draws between the 150B and 4096B points meets the
500us line at an apparently smaller prompt, near 2.3KB. That line is a visual join
between measured points and not a fit, and prompt bytes enter the cost linearly, so
the arithmetic above is where the crossing comes from.

### What Tenet 1 gets from this

Tenet 1 targets under 500us for the full enforcement path. Under load that budget
holds at 150B, is exceeded by 21 percent at 4096B, and is exceeded 11.6-fold at
32768B, all measured rather than inferred. The inference step that used to sit
here, from concurrency-1 service times to a loaded quantile shift, is no longer
load bearing for the tenet claim.

## The estimator

"P99 overhead" means P99(proxied) minus P99(direct). It is a quantile shift. It is
NOT the P99 of per-request overhead, which is unmeasurable without paired samples,
because no request exists in both arms. Tenet 1 reads colloquially like the latter,
so every claim derived from this harness is worded to match the estimator that
produced it. Both absolute distributions are always shown, because a bare
subtracted number hides which arm moved.

Every quoted number carries its payload size and its arrival rate. An unqualified
"under 500 microseconds" is forbidden here, for the reason in the crossing section
above.

Full treatment, including the three cells, the open-model load generator and the
whole matrix: [methodology/estimator-and-matrix.md](methodology/estimator-and-matrix.md).

## Reproducing a figure

```
git clone <this repo>
make figures RESULTS_DIR=benchmarks/results/<evidence-dir>
```

That needs only uv. The plot scripts are `uv run --script` files with PEP 723
inline dependencies, `requires-python` set, and a COMMITTED per-script lockfile
carrying exact versions and sha256 hashes. `--locked` asserts that lockfile still
resolves, so a drifted dependency fails loudly instead of silently re-rendering
against different library versions. Pinning matplotlib alone would leave numpy,
pillow, contourpy and fonttools floating, which is fidelity drift now and a
resolution failure later.

"Regenerable" means identical statistics and marks, not byte-identical PNGs.
Matplotlib output is not byte-stable across machines and font sets. Each figure is
annotated with its source results-directory name, its levee tree hash, and the
renderer versions, so a figure circulating detached from the repository stays
self-describing.

## Obtaining the pinned k6

Install the GitHub release binary for the exact version recorded in the MANIFEST of
the run being reproduced, from the k6 project's own releases page. Do not use `brew
install k6`. Homebrew has no versioned formulae for it, so `brew install` tracks
whatever is current and will drift away from the recorded version, which silently
changes the measurement tool underneath a comparison.

`run.sh` records the full `k6 version` string in the MANIFEST for that reason, and
sets `K6_NO_USAGE_REPORT=true` on every invocation. k6 reports anonymous usage
statistics by default, which is an undisclosed outbound connection during a run
advertised as loopback-only, and an uncontrolled variable besides. The setting is
recorded in the MANIFEST too.
