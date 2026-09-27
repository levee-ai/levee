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
`benchmarks/results/2026-09-27-b62ebfb-m3pro-macos-evidence-r1`, **VERDICT VALID**:
53 cells, five repetitions per non-streaming pair and three per streaming pair,
60-second steady windows, zero host CPU idle readings below the floor, an empty
`contended-cells.txt` ledger, and a clean identity audit. It is the first evidence
run built after commit `f295ed4` removed the duplicate tokenization pass, so the
enforcement figures below describe the code as it ships. The prior published run
and what stopped being comparable across the fix are recorded in
[CHANGELOG.md](CHANGELOG.md) under 2026-09-27. Re-derive all of it without
generating load:

```
make figures RESULTS_DIR=benchmarks/results/2026-09-27-b62ebfb-m3pro-macos-evidence-r1
```

The two committed figures are `benchmarks/plots/overhead-<that directory>.png` and
`benchmarks/plots/enforcement-<that directory>.png`.

Every number below carries its payload size and its arrival rate. Intervals are
percentile bootstrap, 2000 resamples at 95 percent, seeded so a re-render
reproduces them.

One property of this run shapes how its two number families read. The direct
baseline ran roughly twice as fast as in the prior run, 0.329ms against 0.558ms at
P50, and it drifted upward while the run progressed: the opening canary read
0.231ms and the closing one 0.427ms, a drift at 85 percent of the band 5 ceiling.
Every vs-direct shift therefore rests on a fast and moving baseline. The paired
enforce minus passthrough numbers do not, because both arms of each pair run
adjacent in time, which is why the pairing exists. A same-code replication run two
hours later (`2026-09-27-8f52c67-m3pro-macos-evidence-r1`, VERDICT VALID) measured
the difference directly: the paired enforcement shift replicated to within 4
percent, 479us against 498us at 4096B, while the vs-direct P50 hop read +0.154ms
against +0.361ms here, 2.3-fold apart. Read every vs-direct number in this section
as carrying run identity, and every paired number as portable.

### The proxy hop

Quantile shift in milliseconds, treatment minus the direct baseline, at a 150-byte
prompt and 500 rps:

| quantile | passthrough             | enforce                 | A/A control cells |
|----------|-------------------------|-------------------------|-------------------|
| P50      | +0.361 [+0.356, +0.365] | +0.443 [+0.440, +0.447] | +0.373 and +0.384 |
| P90      | +0.773 [+0.768, +0.779] | +0.830 [+0.819, +0.840] | +0.767 and +0.769 |
| P99      | +1.026 [+0.990, +1.077] | +1.067 [+1.024, +1.102] | +1.006 and +1.052 |
| P99.9    | +3.117 [+2.020, +3.506] | +2.630 [+1.900, +3.342] | +2.838 and +3.121 |

**The P99 proxy hop reads +1.026ms at 150 bytes and 500 rps in this run, above the
1ms Tenet 1 budget, with the interval straddling it.** The prior run read +0.601ms
inside the budget. The movement is in the baseline, not the treatment arm: the
treatment absolute P99 is 1.801ms here against roughly 1.98ms before, so the
proxied arm got faster while the direct arm got faster still, and a shift against a
faster baseline widens. Both readings are honest measurements of their own runs.
What this run supports is that the P99 hop at this payload is near the 1ms budget
line, not comfortably inside it.

The A/A control cells matter twice here. They read +1.006 and +1.052 at P99,
indistinguishable from the passthrough arm they duplicate, so the estimator is
resolving cleanly. And they show the same above-1ms shift for cells with no levee
difference between them, which is what a baseline-driven widening looks like.

That is one payload size. Across the three:

| payload and rate  | passthrough P50 shift | passthrough P99 shift   |
|-------------------|-----------------------|-------------------------|
| 150B at 500 rps   | +0.361                | +1.026 [+0.990, +1.077] |
| 4096B at 500 rps  | +0.315                | +0.671 [+0.629, +0.746] |
| 32768B at 150 rps | +0.675                | +1.863 [+1.434, +1.998] |

**The 32KB row is OUTSIDE the 1ms budget, at 1.9 times it, and it is the pure proxy
hop with no budget work in it.** Two things bound it: the 32768B direct baseline is
a SINGLE cell, so that shift rests on one baseline instead of a group of five, and
the 32KB cells run at 150 rps for the capacity reason in
[methodology/limits.md](methodology/limits.md#enforced-throughput-is-bounded-by-tokenizer-cpu).
With enforcement on, the same cell's P99 shift is +3.440ms.

At P99.9 the picture is unstable across runs rather than within this one. This
run's 150-byte P99.9 intervals exclude zero where the prior run's spanned it in
both directions. Two consecutive valid runs disagreeing about the sign region means
**no durable P99.9 overhead claim is supported, in either direction.** See
[methodology/limits.md](methodology/limits.md#p999-is-not-resolvable-on-this-host).

The A/A control agreeing with the passthrough arm does not make the overhead number
meaningless. What that agreement does and does not license is in
[methodology/limits.md](methodology/limits.md#the-two-number-families-are-not-equally-well-controlled).

### The cost of enforcement over pure forwarding

Median repetition-matched enforce minus passthrough P50 shift in microseconds,
which is the quantity band 3 gates:

| prompt and rate           | reps | shift     | interval       | per repetition                     |
|---------------------------|------|-----------|----------------|------------------------------------|
| 150B at 500 rps           | 5    | +60       | [56, 67]       | +234, +56, +89, +60, +50           |
| 4096B at 500 rps          | 5    | +498      | [495, 502]     | +502, +498, +492, +522, +469       |
| 32768B at 150 rps         | 5    | +2668     | [2658, 2682]   | +2677, +2653, +2668, +2793, +2663  |
| streaming 150B at 250 rps | 3    | see below | [50, 75]       | +79, +20, +63                      |

**4096B at 500 rps is the row to quote**, +498us with a 53us spread across five
repetitions. It is the primary gate's own payload, and it is the direct measure of
what the tokenization fix bought: the same row read +605us against the pre-fix
binary. At 32KB the saving is larger, +2,668us against +5,784us, a 54 percent
reduction, consistent with tokenization being the dominant term and now running
once.

150B at 500 rps is NOT resolved by this run. The 184us spread across repetitions,
driven by one +234us repetition, exceeds the 60us median, and the harness's own
repetition rule says a run in that state cannot resolve its signal. It is recorded,
it sits inside its advisory window, and it is not a publishable central value. Ten
runs on this host have read this quantity between +11 and +90us with occasional
outliers above.

Streaming at 150B and 250 rps is bounded and not measured. Three repetitions read
+79, +20 and +63us. Band 3-STREAM deliberately gates only the ABSOLUTE size of that
shift, against a two-sided 0.60ms ceiling, and refuses to publish a central value.
The honest statement is that streaming enforcement is bounded below 0.6ms at this
payload and rate and its magnitude is unresolved.

**32KB enforcement costs 2.668ms, which is 5.3 times the 500us target.** That is
the largest result in the matrix: a 32768-byte prompt is an ordinary agent context,
and at that size the enforcement path is the dominant term in the request, 3.971ms
of enforce P50 against 1.318ms of passthrough P50. The fix halved this number and
it is still five times over the target, because one tokenization pass over 32KB is
expensive by itself.

### Where the 500 microsecond line is crossed

Under sustained load, group P50s across five repetitions with the paired
enforcement delta beside them:

| prompt and rate   | passthrough P50 | enforce P50 | enforcement delta |
|-------------------|-----------------|-------------|-------------------|
| 150B at 500 rps   | 0.690ms         | 0.772ms     | 60us, unresolved  |
| 4096B at 500 rps  | 0.761ms         | 1.263ms     | 498us             |
| 32768B at 150 rps | 1.318ms         | 3.971ms     | 2668us            |

The delta column is not the difference of the two columns beside it. It is the
median of the five repetition-matched differences, which is the estimator band 3
gates, and the two disagree by a few microseconds because a median of differences
is not the difference of medians.

Three derivations of the crossing from those three points, all linear in prompt
bytes because the tokenizer is linear in prompt bytes. The measured slope from 150B
to 4096B is 111ns per byte and puts the crossing at 4114B. The whole delta at 4096B
over its 4096 bytes is 122ns per byte and puts it at 4112B. The 4096B to 32768B
slope is 76ns per byte and extrapolating from the measured 4096B point puts it at
4122B. **So under 500 rps of load, 500us is crossed just past a 4KB prompt, between
4.11KB and 4.13KB by all three derivations.** The 4096B measurement itself reads
498us [495, 502], so the budget sits exactly at that payload: the interval
straddles the 500us line. The 1ms line falls in the 4096B to 32768B segment, whose
76ns per byte slope puts it near 10.7KB. The pre-fix curve was convex, this one is
concave, because the fixed per-request enforcement cost of roughly 60us now
dominates the small end while the halved tokenization flattens the large end.

At concurrency 1 against the real `levee serve` binary, medians, so these are
SERVICE TIMES and not quantiles under load:

| prompt | passthrough | enforce | enforcement delta |
|--------|-------------|---------|-------------------|
| 150B   | 0.165ms     | 0.218ms | 53us              |
| 4096B  | 0.191ms     | 1.365ms | 1.174ms           |
| 32768B | 0.231ms     | 7.512ms | 7.281ms           |

**This table was probed against the PRE-FIX binary at commit `2a569f4` and is the
one table in this file the 2026-09-27 run does not supersede**, because the run
measures only under load. Its enforce column includes the doubled tokenization: the
loaded post-fix deltas above suggest roughly half its 4096B and 32768B deltas for
current code, but that is inference, not measurement. It stays published with this
label until re-probed against the current binary. Anyone sizing a worst case for
one isolated enforced call should still take this column, because Tenet 3 forbids
under-counting and this is the conservative end.

**Which figure to use for what.** The loaded numbers are the headline. They are
what the pre-registered bands gate, they are re-derivable from a committed artifact
by a stranger running `make figures`, and Tenet 1 is worded as a quantile shift
under load. The concurrency-1 numbers are for reasoning about a SINGLE request in
isolation, with the pre-fix caveat above.

### What Tenet 1 gets from this

Tenet 1 targets under 500us for the full enforcement path. Under load that budget
holds at 150B, sits exactly at the line at 4096B, where the measured interval
[495, 502] straddles 500us, and is exceeded 5.3-fold at 32768B, all measured
rather than inferred. The tokenization fix moved the crossing from near 3.4KB to
just past 4KB and halved the 32KB cost. The 1ms P99 proxy-hop budget read above
the line at 150B in this run, +1.026ms against +0.601ms in the prior run, with the
movement attributable to a faster direct baseline rather than a slower proxy: the
proxied arm's absolute P99 improved between the runs.

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
