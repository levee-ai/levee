# The validity gates, as they stand today

Each gate below is stated in its current form only. How it got there is in
[../CHANGELOG.md](../CHANGELOG.md), the data each threshold was derived from is in
[calibration.md](calibration.md), and when a run fails start at
[triage.md](triage.md), which is keyed on the lines `check_bands.py` prints.

Five bands are pre-registered, meaning each was fixed before the numbers it gates
were measured. They are evaluated mechanically from the committed per-request CSVs by
`benchmarks/plots/check_bands.py`, never by a human looking at a figure and judging
whether the numbers seem reasonable. The verdict is written to `bands.txt` in the run
directory and a violation exits non-zero. A run that violates a band is debugged,
never published, and a band is never widened to make a run pass. They are reproduced
here because the design document that pre-registered them lives under `docs/`, which
is not committed, and pre-registration only functions as discipline if the bands are
public before the numbers are.

Two gates run before all five, because a band comparing two cells means nothing until
both are known to have measured service time on a quiet host. Those are the host
quiescence gate and the RATE gate.

## Band 1, the direct-to-mock floor

The P50 of every direct-to-mock cell is below the ceiling calibrated for its own
group, where a group is a response mode paired with a payload size. The P99 is
recorded against that group's advisory threshold and never blocks publication.

```
direct-cell group        P50 ceiling (GATE)   P99 advisory (RECORDED)
non-streaming    150B                1.0ms                     2.5ms
non-streaming   4096B                1.0ms                     2.5ms
non-streaming  32768B                1.5ms                     3.5ms
streaming        150B                2.0ms                     5.5ms
```

A direct cell measures the generator, the loopback stack and the mock. If that floor
already sits at the proxy budget then the box or the mock is the bottleneck and no
proxied number from the same run means anything. Duration to last byte is a different
quantity per group, since a streaming response replays six SSE events with a write and
a flush each and a 32768-byte response moves 218 times the bytes of a 150-byte one,
which is why there are four ceilings and not one. A direct cell in a group with no
calibrated threshold FAILS the band with the group named, instead of borrowing a
neighbour's ceiling.

Derivation and the frozen data cut: [calibration.md](calibration.md#band-1-the-direct-to-mock-floor).
History: [CHANGELOG 2026-09-17](../CHANGELOG.md#2026-09-17-every-band-1-ceiling-comes-from-its-own-cell-group).

## Band 2, the extra proxy hop

The median passthrough minus direct P50 shift at 150B falls within 0.05 to 0.6ms.
Below the floor the extra loopback hop is missing, which means the cell did not
traverse the proxy. Above the ceiling something other than the hop is being paid.
There is no streaming equivalent, so a streaming-only inflation of the direct arm is
visible on the figure and gated nowhere.

Derivation: [calibration.md](calibration.md#band-2-the-extra-proxy-hop).
History: [CHANGELOG 2026-09-16](../CHANGELOG.md#2026-09-16-initial-methodology).

## Band 3, enforcement over passthrough

At the 4096-byte payload, the median repetition-matched enforce minus passthrough P50
delta falls within 0.15 to 0.95ms, and the spread across repetitions is smaller than
the delta itself. If the spread exceeds the signal, the run cannot resolve the
enforcement cost and the answer is more repetitions, not a wider band. With one
repetition the spread check is arithmetically vacuous and `bands.txt` says so.

**The 0.95ms ceiling is stale as of 2026-09-17 and is now weaker than it reads.** It
was sized to catch a THIRD tokenizer pass, near 993us, on the premise that a
duplicate SECOND pass was already shipping and only a third would be new. That premise
died with commit `f295ed4` (branch commit `b5087fa`), which carries the reservation
estimate forward so an enforced request tokenizes once. A reintroduced second pass now
measures roughly 655us at this payload and sails under 0.95ms, so this band would PASS
the very regression the fix removed. The threshold is deliberately not moved, because
recalibrating needs a post-fix evidence run to establish the new central value, and
moving a threshold to make something fail is the same error as moving one to make
something pass. Expect a post-fix central value near 300 to 330us and size both ends
from it. Until then the guards against that regression are in the product and are
stronger than this band ever was: the `tokenEstimator` interface at
`internal/proxy/proxy.go:83` omits `Estimate`, so reintroducing a whole-body pass is a
compile error, and `TestEnforcedRequestTokenizesBodyOnce` in
`internal/proxy/enforcement_test.go` counts the passes directly. The same warning is
carried in `check_bands.py` beside the constant.

At 32KB the delta is governed by the measured tokenizer curve and not by a fixed
window.

Derivation: [calibration.md](calibration.md#band-3-the-primary-enforcement-gate).
Relocation history: [CHANGELOG 2026-09-16](../CHANGELOG.md#2026-09-16-the-first-evidence-run-is-invalidated-and-the-quiescence-gate-is-added).

## Band 3-SMALL, the 150-byte reading

The 150-byte delta is computed and printed against an advisory window of -0.05 to
+0.10ms. It does not gate. The floor is negative on purpose: gross enforce-only work
at this payload is about 38us against a measured 24us credit for the shared request
path running faster in the enforce arm, and removing one of the two tokenizer passes
takes gross work to 20.8us against the same credit, which puts the net near -3us. A
floor of zero would fail every quiet run on a codebase that had just become faster. A
negative reading does not mean enforcement became free. The work itself is measured by
the 4096B gate and by `microbench.txt`, neither of which can go negative.

## Band 4, the tail companion to band 3

At 4096B, the median enforce minus passthrough P99 shift does not exceed ten times the
median P50 shift. A tail shift that far above the median shift means one cell caught a
transient even though every median gate passed. When the median P50 shift is zero or
negative a ratio against it is undefined, so the band falls back to ten times band 3's
own ceiling, which is the widest the ratio form could ever have allowed while band 3
passed.

## Band 3-STREAM, the streaming enforcement shift

This one was not pre-registered. It prints the median streaming enforce minus
passthrough P50 shift, advises when that shift falls outside the 150-byte advisory
window, and GATES only on absolute magnitude, two-sided, at 0.60ms.

It does not gate the value it reports, because the reading is not a stable central
quantity. Across five quick matrices the streaming shift measured +59, +215, -336,
+163 and +26 microseconds. It changes sign, and nothing in the component decomposition
can produce a third of a millisecond of either sign, so the large readings are
between-cell drift. The code reading is what carries the claim: nothing on the
streaming path is enforcement-conditional, since the `stream_options` injection, the
per-event usage inspection and the stream reconcile are all paid by the passthrough arm
too and cancel out of the shift. This gate fires on roughly a 40-fold regression or on
an enforce arm that was not enforcing. It cannot catch a doubling, and closing that
needs streaming repetitions and a streaming drift canary, not a tighter number.

It needs one clean streaming repetition rather than three, and prints a THIN MEDIAN
line whenever contention cost it any. Zero clean repetitions still fails.

Derivation: [calibration.md](calibration.md#band-3-stream-the-streaming-enforcement-shift).

## Band 5, the drift canary

The opening and closing direct-to-mock cells bracket the whole matrix, and their
absolute drift is at or below 0.25ms at P50 and at or below 1.50ms at P99. Both
quantiles are gates here, unlike band 1 where only the P50 gates. Exceeding either
ceiling invalidates the run. The percentage difference is printed beside both gates as
context and is not itself a gate.

If the two canaries disagree, the machine drifted underneath the experiment and no
cell in between can be compared to any other. Pairing means slow drift largely cancels
WITHIN each passthrough-and-enforce pair, so what the canary really catches is gross
drift, a thermal collapse or a background job that arrived mid run and stayed.

Derivation, including both rejected runs: [calibration.md](calibration.md#band-5-the-drift-canary).

## The A/A control pair

Two extra cells per repetition at 150 bytes, `controla` and `controlb`, both running
the passthrough config, so the repetition-matched P50 shift between them has a known
true value of zero. Whatever it reads is noise the estimator invented. `bands.txt`
prints it as `CONTROL-AA` immediately below the enforcement readings it qualifies.

It is reported and never gated. Under the three-busy-loop contention that reproduced
the invalidated first evidence run, the A/A pair still read 13us, so a passing control
does not certify a quiet host and gating on it would manufacture the exact false
confidence the control was added to remove. It is necessary and not sufficient. It
restarts levee between its two arms because the enforcement pair it calibrates does,
and it runs once per repetition because the published quantity is the median of the
per-repetition shifts.

## The RATE gate

Every cell serves at least 98 percent of its demanded arrival rate, and its committed
row count agrees with k6's own steady request count within 1 percent.

A cell demanding more than its capacity reports queue residence instead of service
time, and no band can tell the two apart, because a queue raises the central tendency
exactly the way real work does. The gate recomputes each cell's achieved throughput
from the committed rows and compares it against the demanded rate recorded in the
MANIFEST. k6 enforces the same floor at cell time as `http_reqs{scenario:steady}:
count>=MIN_STEADY_REQUESTS`, so a short cell aborts the run where it happened.
Per-cell figures land in `achieved-rate.txt`. The fix for a firing RATE gate is a
lower rate for that payload size in `rate_for_payload`, sized from measured capacity.
It is never a wider margin, and never a bigger VU pool: past the knee a bigger pool
makes the number worse.

Derivation of the margin: [calibration.md](calibration.md#the-rate-gate-margin).

## The integrity tolerances

Two k6 thresholds carry a fraction of each cell's own demand rather than an absolute
zero.

```
quantity                    tolerance                        floor
steady dropped iterations   1 percent of demanded steady     25
steady failed requests      0.05 percent of demanded steady  5
```

Failed requests are twenty times tighter in relative terms, because a failed request
is levee answering 429, erroring, or the loopback stack breaking, while a dropped
iteration is only the load generator giving up. `check_bands.py` re-derives both
allowances from the same integer basis-point arithmetic `run.sh` uses, compares the
recorded raw counts against it, and cross checks its own figure against the allowance
k6 recorded. Every raw count is printed whether it passed or not, and `bands.txt`
prints an `INTEGRITY TOTALS` line naming the run's total steady drops and failed
requests with their percentages, so a reader who prefers the old absolute rule can
apply it by hand.

The drop gate is not redundant against the RATE gate. Drops subtract from completions
one for one, so anything above 2 percent already fails the rate floor. At 1 percent
the drop gate catches a shape the rate gate structurally cannot: a drop proves the VU
pool had no free slot at a scheduled arrival, so the pool was momentarily part of what
the cell measured.

Warmup counts are untouched. They were already tolerated by design, because levee's
first enforced request builds the encoder and blocks the pool for roughly 130ms, and
they are recorded separately so a climbing count stays visible.

Derivation: [calibration.md](calibration.md#the-integrity-tolerances).

## The host quiescence gate

`run.sh` samples system-wide CPU idle percentage into `machine-state.txt` as
`cpu_idle_pct`, twice per cell and once before the first cell, while k6 is not running
so the reading is the ambient host.

```
when          rule                                             evidence mode   quick mode
startup       one reading below the 60 percent floor           refuses         warns
startup       reading unreadable                               refuses         warns
mid-run       two CONSECUTIVE confirmed sub-floor readings     aborts there    warns
mid-run       more than 10 percent of all readings sub-floor   aborts at end   warns
mid-run       one isolated confirmed dip                       records it      records it
```

A reading below the floor is re-sampled twice and the median of the three decides, so
a transient can be voted out. The startup asymmetry is deliberate: refusing at startup
costs five seconds, refusing at cell 40 costs the 52 minutes already spent. Load
average is recorded and deliberately not gated, because it was proven not to
discriminate, reading 4.0 to 6.4 during the invalidated run against 2.8 to 5.0 during
the quiet re-measurements that corrected it. The gate is necessary and not sufficient:
the contended regime it is calibrated against costs 16 points of idle, and one busy
loop costs about a third of that and would pass.

Derivation of the floor and the 10 percent budget: [calibration.md](calibration.md#the-host-quiescence-floor).

`run.sh` can be SOURCED, in which case it defines every function and runs nothing, and
`LEVEE_BENCH_SYNTHETIC_IDLE_READINGS` then feeds a whitespace-separated list of idle
percentages to the sampler, one value per call. A driver of a dozen lines can walk
`record_machine_state` and `enforce_quiescence_breach_budget` through any breach
pattern in seconds. A sub-floor first sample consumes THREE values, because the
confirmation re-samples twice and returns the median, while a clean one consumes one.
The hook cannot affect a real run: `preflight` refuses to start in either mode while
the variable is set, and `preflight` is on the only path that creates a results
directory.

## The contention exclusion

An isolated dip lands in `contended-cells.txt`, and `check_bands.py` drops that
repetition out of every median it computes: band 2, band 3 at 4096B, band 4,
BAND3-STREAM, BAND3-SMALL and CONTROL-AA. A repetition is dropped when either arm of
its pair was contended, scoped to one pairing, because a repetition ordinal is a join
key and the 150B and 4096B cells of one repetition ran minutes apart. This is what five
repetitions are for.

Below three clean repetitions the gates fail with the cause named, because below three
a median stops being an order statistic and becomes a single reading wearing the word
median. BAND3-STREAM is the exception at one. In quick mode, with one repetition,
excluding it leaves nothing, so the band reads UNEVALUABLE and the run is invalid
rather than silently passing.

Single-instance cells are recorded and kept, meaning the two drift canaries and the two
direct payload cells. There is nothing to drop them in favour of, and the bands reading
them already tolerate a contended host. Band 1 reads `contended-cells.txt` only to
ANNOTATE a failure. There is no code path by which a recorded breach turns a band 1
failure into a pass, because a floor measured on a busy machine is still the floor that
run's proxied numbers sit on.

## Orchestration safety

Part of validity, not separate from it. `run.sh` traps on exit and kills its whole
process group, asserts every port is free before using it with the occupying PID named
on failure, polls both readiness endpoints, and requires levee's admin `/health` to
report the exact build stamp of the binary this run compiled before any proxied cell
starts. That last check makes an orphaned listener from a previous run impossible to
measure by accident. Percentile arithmetic is linear interpolation between order
statistics, and the figures compute their percentiles through the same function the
gates use, so a figure cannot publish a number the gate never saw.
