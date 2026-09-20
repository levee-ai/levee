# Where every threshold came from

The skeptic's file. For each threshold: the cells it was calibrated on, every
cell it is applied to, the frozen data cut it was derived from, the derivation
itself, and a check against a second cut. The gates in their current form are in
[bands.md](bands.md). When a gate fires, [triage.md](triage.md) is the file to
grep.

## The calibration-provenance requirement

A rule for any threshold added or changed from 2026-09-17 onward. Band 1 was
amended twice in two days for the same category error on two different axes, and
neither amendment would have been needed had this been checked when the threshold
was written.

Every threshold MUST record three things, and any reader is entitled to find all
three next to the number:

1. **The cells it was CALIBRATED on.** Name them, and name where their readings
   can be recomputed. A threshold justified as a multiple of an expected value has
   to say where that expected value was measured, or the multiple cannot be
   rechecked once the data grows.
2. **Every cell it is APPLIED to.** Not the cells it was tested against. Every
   cell the checker will actually judge with it.
3. **Whether those two sets differ** in response mode, payload size, arrival rate,
   or any other term that moves the underlying quantity. Where they differ, either
   split the threshold per group or state explicitly why the quantity is the same.
   Silence here is the defect. It is what let a ceiling derived from one-write
   150-byte responses judge a six-write response and a 32768-byte response.

Two corollaries, both cheap to honour. Freeze and name the calibration cut, state
how many results directories it pools and which ones, and judge a later run
against the frozen cut instead of silently folding its readings back in to move
the threshold. Then check the threshold against a second data cut before
committing it: if adding or removing one run moves the number, the number is not a
calibration.

A companion rule covers satisfiability. Before a gate is written or tightened,
count the opportunities it gets in one full evidence run and multiply by the
observed rate of the thing it fires on. If the product is not comfortably below
one, the gate cannot be satisfied and will be deleted in frustration rather than
obeyed, which loses the protection entirely. Four lost evidence runs are the
evidence for both rules: two died to gates demanding exactly zero occurrences of a
rare event across 106 host readings and 1,224,053 steady iterations respectively,
and a third died to a threshold that was satisfiable on the cells it was derived
from and roughly twice as strict on a cell it was not.

The two rules compose. The satisfiability rule asks whether a gate can be met at
all. This one asks whether it can be met on the cells it will judge.

## Band 1, the direct-to-mock floor

**Calibrated on** every direct cell reading in this tree, recomputed from the
committed steady-only CSVs, with the cut **frozen at 13 loadable directories**.
Six early directories are aborted runs that never wrote a filtered CSV and are
absent for that reason. **Applied to** all four direct-cell groups in the matrix:
the two 150-byte non-streaming drift canaries, `direct-payload-4096`,
`direct-payload-32768`, and the three streaming direct repetitions.

```
group                  n   P50 median   P50 min   P50 max   P99 median   P99 max
non-streaming   150B   26        0.288     0.255     0.506        0.752     1.611
non-streaming  4096B   10        0.332     0.307     0.546        0.875     1.419
non-streaming 32768B   13        0.481     0.278     0.899        1.130     2.835
streaming       150B   15        0.643     0.523     1.341        1.804     2.606
```

**The rule.** Each group's P50 ceiling is that group's own median P50 times a
single multiplier held constant across every group, rounded down to the nearest
0.5ms. The P99 advisory is the same rule on the same group's own median P99 with
its own single multiplier. Two multipliers in total, one per quantile, and no
group has a multiplier of its own.

```
P50 multiplier   1.0 / 0.288 = 3.47
P99 multiplier   2.5 / 0.752 = 3.32
```

Neither multiplier is a free parameter. Both are fixed by the thresholds band 1
has always carried, the 1.0ms non-streaming 150-byte P50 ceiling and the 2.5ms
advisory beside it, divided by the anchor group's own median. The anchor's product
is therefore 1.0ms exactly, the round-down is a no-op on it, its ceiling is
unchanged by construction, and every other group is set at the same relative
strictness the original ceiling expressed. The two were set independently,
amendments apart, and they land within 5 percent of each other.

**Bin check across all three cuts that exist.** The four P50 ceilings are
identical on every cut:

```
cut       multiplier   ns 150B        ns 4096B       ns 32768B      stream 150B
11 dirs        3.413   1.000 -> 1.0   1.140 -> 1.0   1.655 -> 1.5   2.212 -> 2.0
12 dirs        3.436   1.000 -> 1.0   1.148 -> 1.0   1.643 -> 1.5   2.216 -> 2.0
13 dirs        3.472   1.000 -> 1.0   1.151 -> 1.0   1.670 -> 1.5   2.233 -> 2.0
```

The ceilings were fixed on the 12-directory cut before this amendment's own
verification run existed, and the 13-directory cut reproduces all four bins. That
is a small out-of-sample confirmation, one run wide.

The advisories are measurably less stable:

```
cut       multiplier   ns 150B        ns 4096B       ns 32768B      stream 150B
11 dirs        3.472   2.500 -> 2.5   2.969 -> 2.5   4.142 -> 4.0   6.306 -> 6.0
12 dirs        3.324   2.500 -> 2.5   2.882 -> 2.5   3.860 -> 3.5   6.017 -> 6.0
13 dirs        3.324   2.500 -> 2.5   2.909 -> 2.5   3.757 -> 3.5   5.997 -> 5.5
```

Two of the four straddle a bin edge, so the rule carries one addition: where a
product straddles a 0.5ms boundary across the available cuts, the threshold takes
the **lowest** bin any cut produces. That gives 2.5, 2.5, 3.5 and 5.5. The clause
was written after seeing the 13-directory cut and is labelled post hoc. It is
defensible because the P99 is an advisory that never blocks publication and
because taking the lowest bin is the strictest choice available, which is the
opposite direction from fitting a threshold to rescue a run.

The two straddles have different causes. The 32768B one is genuine estimator
movement: its P99 median walked 1.193 to 1.161 to 1.130 across the cuts, 5.3
percent, while its P50 median moved only 1.4 percent. The streaming one is the
**anchor** moving, since the streaming P99 median walked just 0.7 percent while
the anchor's went 0.720 to 0.752, 4.4 percent, which rescales every advisory at
once. A P99 median is estimated from far fewer effective samples than a P50
median, and that applies with extra force to the anchor because the anchor scales
all four.

**The result.** `fold` is the threshold over that group's own median, so it is the
uniform slowdown that trips it. `margin` is the threshold over the largest reading
that group has ever produced on this host.

```
group                    n   median   product   CEILING     fold   margin over max
non-streaming   150B    26    0.288     1.000     1.0ms    3.47x    1.98x of 0.506
non-streaming  4096B    10    0.332     1.151     1.0ms    3.02x    1.83x of 0.546
non-streaming 32768B    13    0.481     1.670     1.5ms    3.12x    1.67x of 0.899
streaming       150B    15    0.643     2.233     2.0ms    3.11x    1.49x of 1.341

group                    n   median   product  ADVISORY     fold   margin over max
non-streaming   150B    26    0.752     2.500     2.5ms    3.32x    1.55x of 1.611
non-streaming  4096B    10    0.875     2.909     2.5ms    2.86x    1.76x of 1.419
non-streaming 32768B    13    1.130     3.757     3.5ms    3.10x    1.23x of 2.835
streaming       150B    15    1.804     5.997     5.5ms    3.05x    2.11x of 2.606
```

Realised P50 strictness spans 3.02x to 3.47x, a 15 percent disagreement, and every
group sits at or stricter than the anchor because rounding down can only reduce a
ceiling. Under the single 1.0ms ceiling the same span was 2.08x to 3.47x, a 67
percent disagreement, with `direct-payload-32768` refusing at 2.08 times its own
central value while the anchor refused at 3.47 times its own.

**Every ceiling still detects a genuine bottleneck**, stated per group against the
0.288ms one-write 150-byte base term. The 150B group IS the base term and sees only
the 3.47-fold uniform slowdown. 4096B trips at 3.02-fold uniform and its own 44us
payload term would need a 16.4-fold rise, so it is a second reading of the base cost
and not a sensitive probe of 4KB handling. 32768B trips at 3.12-fold uniform and its
193us payload term would need to reach 1212us, a 6.3-fold rise, which is the one
failure only that cell can see. Streaming trips at 3.11-fold uniform and its 71us per
SSE segment would need to reach 342us, a 4.8-fold rise. None of these is a subtle
regression. This band answers one question, whether the floor is so high that no
proxied number in the run means anything.

**Second-cut check.** Two further quick runs measured after the thresholds were
fixed, `2026-09-17-16883b9-dirty-m3pro-macos-quick-r2` and `-r3`, added 10 direct
readings. Every one passes its gate with 3.12x to 3.70x fold headroom and not one
fires its advisory, the closest being the 4096B tail at 2.89x. Pooling them into a
15-directory cut moves no product across a bin edge: the P50 products become
1.000, 1.147, 1.663 and 2.105 and the P99 products 2.500, 2.881, 3.757 and 5.811,
which round into the same eight thresholds. They are deliberately not re-derived
on that cut. It is a check, and the frozen cut stays frozen.

**The one number to watch** is the streaming P50 product, which moved 5.7 percent
DOWN to 2.105 on those two readings against a bin edge at 2.000. Quiet streaming
cells push this product toward the edge, and if it crosses, the rule as stated would
put the streaming ceiling at 1.5ms. That is a tightening driven by the host being
quiet, so the response then is to say so and keep 2.0ms. The rule sizes a ceiling
from a central value and does not license ratcheting one down when the machine has a
good day.

**A provenance trap.** The fourth evidence attempt's streaming P50 values are 1.341,
1.230 and 0.951 in the committed CSVs and 1.339, 1.238 and 0.947 in the summary JSON
`p(50)` fields. This band gates the CSV values, because the summary aggregates the
whole invocation including warmup and gating on it would gate numbers nobody
publishes. Anyone re-deriving these thresholds has to read the CSV column.

**Still thin, recorded rather than closed.** All four calibrations come from one host,
and three of the four rest on 10 to 15 readings of which all but one run is
quick-mode. An evidence run loads the box for 52 minutes and a quick run does not,
and the fourth attempt showed streaming cells elevated 1.98-fold over quick while
non-streaming 150B cells rose only 1.61-fold. The streaming group keeps the tightest
margin at 1.49x, accepted deliberately, because calibrating on the evidence cut alone
would give 2.6ms from 3 readings taken on the run the band has to judge. 4096B is the
strictest group in fold terms at 3.02x, purely because 1.151 rounds down to 1.0, and
it is the likeliest to fire next. And the anchor is a single point of failure for all
eight thresholds, since every one is the anchor's threshold scaled by a ratio. Its
P50 median is stable across the three cuts to 1.7 percent, which licenses the gates,
while its P99 median moved 4.4 percent, which is why the advisories needed the
lowest-bin clause.

## Band 2, the extra proxy hop

**Calibrated on** nothing measured. The design sketched this window as roughly 0.1
to 0.4ms and the checker implements the wider 0.05 to 0.6ms, set when the checker
was first written and before any matrix had been measured against it. It has not
moved since. **Applied to** the 150-byte non-streaming passthrough cells, with the
two 150-byte direct canaries as the baseline. Unlike bands 1 and 5 this is not an
amendment in response to a failure, and unlike band 3 it has no measured central
value behind it.

## Band 3, the primary enforcement gate

**Calibrated on** the five repetition-matched 4096-byte pairs of the first
completed 43-cell evidence run at commit `31918d9`, the run band 3 itself
invalidated. **Applied to** the 4096-byte non-streaming
passthrough-and-enforce pairing of every run.

The gate sits at 4096 bytes and not at 150 bytes for one reason, which is signal
to noise:

```
payload   measured shift   estimator noise floor   ratio    within-run spread
150B          +15us               4us              3.7:1    68us, 55 pct of it
4096B        +655us               4us            164:1      12us, 1.8 pct of it
```

Both spread figures come from the SAME invalidated run, which is what makes the
comparison fair rather than selective. A 3.7:1 gate cannot be relied on. A 164:1
gate can. The 150-byte window was CORRECT and the run that failed it was INVALID,
so this relocation is not a widening. The 150-byte reading is still printed on
every run as `BAND3-SMALL`.

**The 4096-byte numbers** from that run: passthrough P50 0.790, 0.807, 0.795,
0.797 and 0.798ms against enforce 1.445, 1.453, 1.440, 1.454 and 1.453ms, so the
shifts are +655, +646, +645, +657 and +655us, median **+655us**, spread **12us**.

**Floor 0.15ms.** Its job is to catch an enforce arm that is not enforcing, which
reads what the A/A control reads, 0 to 15us, so the floor is an order of magnitude
clear of that. Its binding constraint was the then-pending fix removing the
duplicate tokenizer pass. One pass at 4096B measures 423.6us in isolation while
the in-server double-pass shift is 655us, which puts the in-server per-pass cost
near 338us, so the post-fix shift is either 317us or 231us depending on which of
those two figures is the honest one. The floor sits at least 1.5 times below the
lower of them.

**Ceiling 0.95ms.** Forty-five percent above the measured 655us, sized to catch a
third tokenizer pass near 993us. That premise is now stale and the ceiling is
weaker than it reads. The full statement is in
[bands.md](bands.md#band-3-enforcement-over-passthrough).

**Second-cut check**, from the quick matrix that first ran this gate. It is the
less flattering of the only two 4096-byte readings in existence, and its host
dipped below the idle floor on five of its 26 readings so an evidence run would
have refused it outright:

```
payload   passthrough P50   enforce P50   shift    A/A control   150B shift
4096B         0.599ms         1.404ms    +805us      -27us         +61us
```

The gate passed with 145us to spare. The A/A control read -27us at a true zero,
which is the noise a contended host puts into the estimator and is seven times the
4us quiet floor. And the 150-byte reading went to +61us, four times its quiet
value, on a host where the 4096-byte reading moved by 23 percent. That is the
signal-to-noise argument reproducing itself inside a single run. The two readings
together, **655us and 805us**, bound the between-run spread at 4096 bytes on this
host at 150us, which is why the ceiling sits 45 percent above the quieter of them
rather than 10 percent above it. If a run that PASSES the quiescence floor ever
reads above 0.95ms, the response is to count the tokenizer passes.

**The between-regime bias is now measured.** The eight quick runs, at a 20-second
steady window, read 0.786, 0.789, 0.805, 0.807, 0.815, 0.823, 0.837 and 0.865ms,
median 0.811. The one valid evidence run, at a 60-second window, read 0.605ms. The
two groups do not overlap, the closest pair being 0.613 against 0.786, so the bias is
-0.206ms with the EVIDENCE regime reading LOWER, and the movement is in the
passthrough arm. The window is 800us wide, so a bias of that size cannot move the
verdict, and that headroom is why the window is wide rather than tight.

**The 150-byte advisory window, -0.05 to +0.10ms**, comes from the component
decomposition of the +15us quiet-host reading: 38.2us of gross enforce-only work,
being two tokenizer passes at 17.4us each plus 2.1us of logging plus 0.9us of
drift observation plus 0.4us for admission and reconcile, offset by a **24us
credit** because the shared request path runs faster in the enforce arm. That
credit is measured and is not attributed to a named cause. Lock contention is
124ns per request and metrics 853ns, so neither is in the story, and garbage
collection costs 19.7us of CPU and zero latency, because marking runs on idle and
dedicated workers rather than as request-goroutine assists. With one tokenizer
pass gross work is 20.8us against the same credit, which puts the net near -3us
and is why the advisory floor is negative.

## Band 4, the tail companion to band 3

**Calibrated on** nothing of its own. It is defined as a ratio against band 3's
median P50 shift, so it follows band 3 to whatever payload band 3 gates and is
**applied to** the same 4096-byte pairing. Ten times a 15us median would be a
150us allowance on a quantity whose host-noise component is measured in
milliseconds, which is the second reason it moved off 150 bytes. The 150-byte P99
shift is still printed beside the 150-byte median.

## Band 3-STREAM, the streaming enforcement shift

**Calibrated on** five quick matrices, whose streaming shifts read +59, +215,
-336, +163 and +26 microseconds. **Applied to** the streaming
passthrough-and-enforce pairing at 150 bytes.

The 0.60ms two-sided ceiling is the inherent work plus the 336us observed drift
envelope, times roughly 1.7 headroom. Correcting the inherent term from a
retracted 19.5us to the measured 15us moves that sum from 356us to 351us and
leaves the ceiling where it was. Two-sided because drift is two-sided while the
work is one-sided, and because a strongly negative shift is also the signature of
an enforce cell that was not enforcing.

**A retracted number, recorded because it was published.** An in-process probe
measured the true shift at 19.5us streaming against 16.9us non-streaming, a ratio
of 1.16. Those absolute figures were wrong and are withdrawn: one tokenizer pass
on the exact load-generator 150-byte body measures 17,364 ns/op, and the code then
made two, so 34.8us of tokenizer work alone exceeds a claimed 16.9us total. The
1.16 ratio came from the same probe and is unverified rather than measured. What
survives is the code reading in [bands.md](bands.md#band-3-stream-the-streaming-enforcement-shift),
which is independent of the probe.

**Calibrated on limited data**, and one calibration row came from a matrix whose
overall verdict was INVALID: 13 of 14989 steady iterations dropped, confined to
the closing drift-canary cell, while all four cells feeding the shift readings
recorded zero steady drops and k6 exited 0. That located fact is what makes the
row usable rather than a judgement call.

## Band 5, the drift canary

**Calibrated on** six historical matrices on the reference host at the P99 leg,
and on exactly one recovered pair at the P50 leg. **Applied to** the opening and
closing direct-to-mock canary cells of every run.

The original band gated a 15 PERCENT agreement at both quantiles. A percentage
tolerance on a sub-millisecond quantity produces an absolute tolerance tighter
than the signal the experiment measures: 15 percent of a 0.265ms canary is
0.040ms, while the deltas this matrix publishes are roughly 0.195ms for band 2 and
0.043 to 0.120ms for band 3. Observed drift across seven matrices was 645.9,
144.0, 23.6, 57.6, 3.3, 141.2 and 28.8 percent, so the original form passed 1 run
in 7.

**The 1.50ms P99 ceiling was chosen after seeing which runs failed**, which is
exactly the kind of choice that deserves scrutiny, so the calibration data is
published in full:

```
run   open P99  close P99  drift pct  absolute drift  at 1.50ms
r3    0.547     4.080          645.9         3.533ms  FAIL
r4    1.353     3.301          144.0         1.948ms  FAIL
r8    0.556     0.687           23.6         0.131ms  pass
r10   0.564     0.889           57.6         0.325ms  pass
r11   0.682     0.660            3.3         0.022ms  pass
last  0.777     1.001           28.8         0.224ms  pass
```

Two rejections out of six, landing on exactly the two runs whose closing canary
showed a multi-millisecond spike and which were independently attributed to host
noise bursts. So the tail gate still detects gross drift, which was the risk in
dropping the percentage form, while no longer rejecting a run whose drift is
smaller than the signal being measured. Under the original 15 percent form only
r11 passes.

The seventh matrix, the 141.2 percent reading, is absent because its absolutes
were not recovered. It would fail the 1.50ms gate only if its opening canary P99
exceeded 1.062ms, which is inside the 0.547 to 1.353ms range the table shows, so
its verdict under the amended band is genuinely unknown and is recorded as
indeterminate rather than counted either way.

**LIMITATION, because the two gates are not equally well evidenced.** Only ONE
historical P50 pair was recovered, 0.265 then 0.341 for an absolute drift of
0.076ms, so unlike the P99 gate the 0.25ms P50 ceiling has never been tested
against a pathological run. The P50 ceiling is sized to the band 2 shift it
protects: a drift comparable to the smallest published delta is the point at which
the comparison stops meaning anything. If it fails, the numbers get examined
rather than the threshold moved.

## The host quiescence floor

**Calibrated on** 26 readings in each of two regimes measured on the reference
host. **Applied to** every mid-run reading of every cell and to the startup
reading, in evidence mode as a refusal and in quick mode as a warning.

```
regime                                 n    min     median   max
ambient, browser and agents resident   26   59.28   69.11   76.32
ambient plus three busy loops          26   41.87   51.56   58.77
```

Three busy loops is not an arbitrary load. It is the exact condition that
reproduced the invalidated 43-cell run. The **paired** form is the load-bearing
evidence: eight same-moment pairs, one reading with the loops absent and one with
them present seconds later, so ambient drift affects both arms equally. Every pair
moved the same way, by a median of **16.12 points of idle** and never less than
**11.94**.

Sixty sits above every one of the 26 contended readings, the highest being 58.77,
and below only 2 of the 26 ambient readings, 59.28 and 59.85, which are that
distribution's low tail. It is deliberately not the midpoint of the two ranges: a
refused run costs one rerun, while a contended run that passes publishes a wrong
number as evidence.

**The sampler** reads the second sample of `top -l 2 -n 0 -s 2`, a true 2-second
interval average. A single `top -l 1 -n 0` sample does respond to load, which was
worth confirming, but it is noisy: five samples with the machine untouched read
60.96, 41.86, 59.75, 57.51 and 58.16. The interval form's spread over ten readings
was 59.85 to 72.76 against 41.86 to 65.50 for the instantaneous one.

**The mid-run budget is 10 percent, computed rather than eyeballed.** The quiet
rate is estimated from ONE event, so its exact one-sided 95 percent Poisson upper
bound is 4.744 events per 79 readings, which is 6.37 per 106:

```
threshold   fires at    false refusal at 1.34 expected   at the 6.37 upper bound
5 percent   6 of 106                 0.26 percent                61.1 percent
10 percent  11 of 106            0.000019 percent                 6.0 percent
15 percent  16 of 106        0.00000000015 percent                 0.1 percent
```

Five percent could refuse a majority of quiet runs and nothing in the data rules
it out. Fifteen percent sits only 1.25 times below the one contended host on
record, so it has almost no margin against the case it exists to catch. Ten
percent is 7.7 times the observed quiet rate and roughly half the observed
contended rate.

**The consecutive rule costs 1.7 percent of quiet runs** at the observed 1.3
percent per-reading rate, treating readings as independent: 105 adjacent positions
times 0.0127 squared.

That independence figure is the only support this threshold has. An earlier
argument that it overstated the risk, because breaches only ever landed on the
`before` phase, was falsified on 2026-09-19 by a run with 9 `after` breaches out of
13, recorded in [the CHANGELOG](../CHANGELOG.md).

The phase asymmetry itself is real. The `before` reading measures 3.2 to 10.9 points
lower than the `after` reading across four runs, because its sampling window
overlaps the harness setup work described below. It cannot be used to rule out
contention, since a loaded host breaches on both phases.

**The pervasive case is recorded, not hypothetical.** It is the quick matrix at
`31918d9` in this tree: 5 of 26 readings breaching, worst reading 51.48 which sits
inside the proven contended regime, at ordered positions 7, 15, 17, 19 and 21, so
no two were adjacent. The consecutive rule would have passed it.

**Where the sampler is still weak.** The `before` reading runs systematically LOWER
than the `after` reading here, by 6.6 points in one recorded run and 10.9 in another,
and every breach ever recorded landed on a `before` reading. That sample is taken
right after a levee spawn, a config render and the previous cell's TIME_WAIT drain,
so its 2 second window can overlap the harness's own setup work instead of pure
ambient load. The bias is left in place because every calibration figure in this gate
was measured through the same sampler and moving the sample point would orphan all of
them. It is written down so nobody reads a low `before` reading as proof of an outside
job.

**The honest limit on the whole story.** The invalidated run recorded no idle figure
at all, because the field did not exist yet, so this floor is calibrated against the
reproduction and not against the failure. The ambient regime above is also not a quiet
host: it carried a browser, a video-conferencing app, resident endpoint-security agents
and several concurrent tool sessions, so it bounds how loaded a passing host may be
rather than describing a prepared one.

## The RATE gate margin

**Calibrated on** the legitimate envelope of a healthy cell. **Applied to** every
cell in the matrix.

A healthy cell OVERSHOOTS slightly, 10001 rows against 10000 demanded at 500 rps over
a 20 second window on this host, and a k6 probe at 20 rps over 2 seconds delivered
exactly 40 of 40. The only honest source of shortfall is work in flight when the
window closes, bounded by the pool size times one service time, which at the matrix's
worst cell is a couple of requests in 9000, or 0.02 percent. So the 2 percent margin
is roughly 100 times the envelope and still 25 times smaller than the 49 percent
shortfall a failed attempt recorded. Throughput on the enforced path is retrograde
past its knee, so an over-demanded cell collapses instead of missing by a few
percent, and sizing the margin nearer the envelope would start rejecting runs for
single-iteration host stalls. What prompted the gate is documented with the capacity
measurement in [limits.md](limits.md#enforced-throughput-is-bounded-by-tokenizer-cpu).

## The integrity tolerances

**Calibrated on** every cell this repository has ever recorded, **151 cells
carrying 1,438,074 steady requests**. **Applied to** every cell's steady window,
per cell, against a fraction of that cell's own demand.

An evidence run is 53 cells whose steady windows demand 1,224,053 iterations
between them, 1,428,053 counting warmup:

```
cells   demanded steady iterations each   subtotal   what they are
   33                            30000     990000   500 rps for 60s
   11                             9000      99000   150 rps for 60s, the 32KB cells
    9                            15000     135000   250 rps for 60s, streaming
```

### The drop tolerance, 1 percent with a floor of 25

Seven of the 151 cells recorded nonzero steady drops:

```
drops   demanded   pct of demand   cell                                pool
    1      30001           0.003   passthrough-nonstream-4096-r5       40
    3      10001           0.030   direct-canary-open-nonstream-150    50 to 100
   13      10001           0.130   direct-canary-open-nonstream-150    50 to 100
   13      10001           0.130   direct-canary-open-nonstream-150    50 to 100
   13      10001           0.130   direct-canary-close-nonstream-150   50 to 100
   46      10001           0.460   direct-payload-4096                 50 to 100
   49      10001           0.490   enforce-nonstream-150-r1            40
```

The two worst rows are the load-bearing ones, because neither can be saturation.
The 46 landed in a **direct** cell, which has no levee in its path at all and
reported P50 0.339ms. The 49 landed in a 150-byte enforce cell running at roughly
11 percent of its measured capacity, which reported P50 0.503ms with a P99 of
9.154ms, the signature of a host stall rather than a queue. So 0.490 percent is
the measured benign envelope on this host and the tolerance sits 2.0 times above
it. All 151 recorded cells pass under the rule.

The floor of 25 exists so a low-volume cell is not held to a tighter standard than
a high-volume one. Four separate cells dropped exactly 13, the observed size of one
host stall here, and 25 is just under two of those. It binds only below 2500
demanded iterations, which no cell in either mode reaches.

**It still catches the failure it was written for by 48.7 times.** That failure is
the 32768-byte capacity problem: 14622 steady drops of 30001, 48.7 percent,
against an allowance of 1 percent. Verified at the pinned k6 v2.2.0: a
deliberately saturated steady window dropped 2754 of 4000 and reported `ok false`
with exit 99 against `count<=30`, then `ok true` against `count<=99999`, so the
tolerance is what decides and the mechanism fires.

**Not redundant against the RATE gate**, measured on the real script against an
upstream that blocks the whole pool once, at 500 rps over 5 seconds with 2500
demanded:

```
stall   steady drops   drop gate      achieved           rate gate   k6 exit
120ms             23   pass, 25 max   495.4 of 500 rps   pass             0
150ms             39   FAIL, 25 max   492.4 of 500 rps   pass            99
```

The second row settles it. It fails on the drop count while the rate gate reads
clean at 1.5 percent short. The two gates fail in opposite blind spots.

### The failed-request tolerance, 0.05 percent with a floor of 5

**There is no observed benign envelope to size it against.** Zero failed requests
have ever been recorded here, 0 in 1,438,074 steady requests. That absence is
exactly why an absolute zero could not stay: zero events in 1,438,074 trials
bounds the per-request failure rate at **2.083e-6** at one-sided 95 percent
confidence, which over the 1,224,053 steady requests of an evidence run is **up to
2.55 expected failures per run**. So the recorded data does not rule out that
`rate==0` loses a 52 minute run more often than not.

Every failure shape worth catching is sustained rather than singular. An exhausted
per-agent admission slot answers 429 for as long as the cell stays over the cap,
so one second of that at 500 rps is 500 failures against an allowance of 15. An
exhausted budget answers 429 for the entire remainder of the cell, tens of
thousands. Verified at the pinned k6 through the real `overhead.js` and its real
threshold expressions:

```
forced steady failures   allowance   threshold                                       k6 exit
                     5           5   http_req_failed{scenario:steady}:rate<=0.05       0
                     6           5   http_req_failed{scenario:steady}:rate<=0.05      99
                   101           5   http_req_failed{scenario:steady}:rate<=0.05      99
```

### The streaming repetition minimum

The contention exclusion needs three clean repetitions before a median is an order
statistic. The streaming matrix runs three repetitions, so that minimum permitted
ZERO contended streaming repetitions across the 12 host idle readings its pairing
spans. Pooling the two evidence attempts that carry idle readings, 1 confirmed
breach in 155, that is a 7.5 percent chance per run, and it fires inside
`check_bands.py` after the last cell so it costs the entire 52 minutes.
BAND3-STREAM therefore needs one clean streaming repetition, which is defensible
only because that gate is a two-sided 0.60ms ceiling on a 15us quantity whose
central value the band already refuses to publish. The non-streaming minimum is
unchanged at three, where five repetitions tolerate two contended ones and the
same arithmetic gives roughly 0.03 percent per run.

The better fix is five streaming repetitions in the matrix. That costs 6 more
cells and roughly 7 minutes of a 52 minute run and moves the pre-registered cell
count from 53 to 59, so it is a matrix decision rather than a gate decision and
has not been taken.
