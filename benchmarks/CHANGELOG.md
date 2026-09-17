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

## 2026-09-17, every band 1 ceiling is derived from its own cell group, replacing two special cases with one rule

**This is band 1's THIRD amendment and the SECOND IN TWO DAYS, and the two share one
cause rather than being repeated tuning.** That is stated first because a reader who
sees two amendments to one band inside two days is entitled to know which it is. Both
fix a threshold calibrated on one quantity and applied to a different quantity reported
in the same unit. The entry below found it on the **response mode** axis, where a
six-write streaming response was judged by a ceiling derived from one-write
non-streaming cells. This one finds it on the **payload size** axis, where a
32768-byte response was judged by a ceiling derived from 150-byte cells. No published
number moves, because no run has yet produced a publishable number.

**Why a third amendment rather than a second patch.** The entry below fixed one axis as
a special case and its own text names the payload dimension as still miscalibrated. Left
alone, `direct-payload-32768` would have entered a fifth evidence run as the tightest
gate in the harness at **1.11-fold margin**, having read 0.899ms against a 1.0ms
ceiling that came from 150-byte cells, and a fifth run lost to it would have been the
same category error collected on a second axis. So band 1 stops being patched one
dimension at a time.

**THE RULE, restateable without reference to any individual run.** Each direct-cell
group's P50 ceiling is that group's own median P50 times a single multiplier held
constant across every group, rounded **down** to the nearest 0.5ms, and where a product
straddles a 0.5ms boundary across the available data cuts it takes the lowest bin. The
P99 advisory is the same rule on the same group's own median P99 with its own single
multiplier. Two multipliers in total, one per quantile, and **no group has a multiplier
of its own**, which is what makes this a redistribution of strictness rather than a
widening.

**The multiplier is not a free parameter.** It is fixed by the one band 1 threshold this
harness has always had, the 1.0ms non-streaming 150-byte P50 ceiling, divided by that
group's own median. The anchor's product is therefore 1.0ms exactly, the round-down is a
no-op on it, and its ceiling is unchanged by construction. P50 multiplier `1.0 / 0.288 =
3.47`. P99 multiplier `2.5 / 0.752 = 3.32`. The two were set independently, amendments
apart, and land within 5 percent of each other.

**This rule subsumes the entry below rather than contradicting it.** That one multiplied
by the measured **mode ratio**. Substitute **group ratio** and the two are the same
rule. Applied to the streaming group it reproduces 2.0ms to three decimal places, so the
streaming P50 gate set the day before is confirmed by the general rule rather than
replaced by it.

**THE RESULT.** `fold` is the uniform slowdown that trips the gate. `margin` is the
gate over the largest reading that group has ever produced on this host, so it is the
headroom a fifth evidence run actually has:

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

**Realised strictness now spans 3.02x to 3.47x, a 15 percent disagreement.** Before this
amendment it spanned **2.08x to 3.47x**, 67 percent, because `direct-payload-32768`
refused at 2.08 times its own central value while the anchor refused at 3.47 times its
own. **No group is left at a cliff**: the tightest margin is the streaming group's 1.49x,
accepted deliberately the day before, and nothing is below it.

**Every ceiling still detects a genuine bottleneck**, stated per group against the
0.288ms one-write 150-byte base term. 150B is the base term itself and sees only the
3.47-fold uniform slowdown. 4096B trips at 3.02-fold uniform, and its own 44us payload
term would need a 16.4-fold rise, so it is a second reading of the base rather than a
sensitive 4KB probe and that is said plainly. 32768B trips at 3.12-fold uniform, and its
193us payload term would need to reach 1212us, a **6.3-fold** rise, which is the one
failure only that cell can see. Streaming trips at 3.11-fold uniform, and its 71us per
SSE segment would need to reach 342us, a **4.8-fold** rise.

**WHAT ACTUALLY MOVED.** Five of the eight thresholds are unchanged. Three move:
`direct-payload-32768` gains a 1.5ms ceiling and a 3.5ms advisory, both first
calibrations, and the streaming advisory goes 5.0ms to 5.5ms. That last one is a
**rounding correction, not a new judgement**, and it is the only widening not driven by
a first calibration: the entry below computed 6.30ms and rounded down to 5.0 in order to
make streaming exactly double non-streaming, which was a taste rather than a rule, since
6.30 rounds down to 6.0 at both whole and half steps. Under the stated rule the product
is 5.997 and the bin is 5.5.

**The only output that changes on any existing directory is the 32768B advisory.**
Verified by running the pre-amendment and post-amendment checkers over every directory
in the tree and comparing exit code and `BAND1` lines rather than reasoning about
thresholds: no P50 gate outcome changes on any of the 64 direct readings, and the one
advisory line that disappears is `2026-09-16-31918d9-dirty-m3pro-macos-quick-r1` at
2.835ms. **No VALID or INVALID verdict changes anywhere**, because an advisory never
blocks publication. Both raised advisories still fire on the class of burst they name:
the band 5 closing canaries at 4.080 and 3.301ms are 5.43x and 4.39x the non-streaming
P99 median, and bursts of that relative size read 6.1 and 5.0ms on the 32768B group
against 3.5ms, and 9.8 and 7.9ms on streaming against 5.5ms.

**THE CALIBRATION CUT IS FROZEN AT 13 DIRECTORIES AND NAMED**, and the P50 bins were
checked against all three cuts that exist. On 11, 12 and 13 directories the products are
1.000, 1.140, 1.655, 2.212 then 1.000, 1.148, 1.643, 2.216 then 1.000, 1.151, 1.670,
2.233, and every one rounds into the same bin. The four ceilings were fixed on the
12-directory cut before this amendment's own verification run existed, so the 13-cut
agreement is a small out-of-sample confirmation.

**The advisories were NOT that stable, and the fix is labelled post hoc.** Two of the
four straddled a bin edge between cuts, `4.142 -> 4.0` against `3.757 -> 3.5` on 32768B
and `6.306 -> 6.0` against `5.997 -> 5.5` on streaming. The lowest-bin clause was added
after seeing that and resolves both downward. The two straddles have different causes:
the 32768B P99 median genuinely moved 5.3 percent across the cuts while its P50 median
moved 1.4 percent, whereas the streaming P99 median moved only 0.7 percent and its
straddle came from the **anchor's** own P99 median moving 4.4 percent, which rescales
every advisory at once. A P99 median is estimated from far fewer effective samples than a
P50 median, which is why the tail cannot be pinned as tightly as the centre.

**All eight thresholds then survived a two-run out-of-sample check.** Two further quick
runs measured after the thresholds were fixed added 10 direct readings. Every one passes
its gate with 3.12x to 3.70x fold headroom and not one fires its advisory. Pooling them
into a 15-directory cut moves no product across a bin edge, and the thresholds are
deliberately not re-derived on it, because the frozen cut stays frozen. The one number
worth watching is the streaming P50 product, which moved 5.7 percent DOWN to 2.105
against a bin edge at 2.000: quiet streaming cells push it toward a tightening, and if it
crosses, the answer is to say so and keep 2.0ms rather than ratchet a ceiling down because
the machine had a good day.

**BAND 1 NOW READS `contended-cells.txt`, AND ONLY TO ANNOTATE A FAILURE**, reversing
the entry below's note that it is the only band ignoring the ledger. The substantive half
of that note stands: contention must never make a band 1 failure disappear, because a
floor measured on a busy machine is still the floor that run's proxied numbers sit on,
and three of the four direct groups are single-instance cells that no repetition-dropping
rule could rescue anyway. What was wrong was treating "must not excuse" as a reason not
to **read** the file. The fourth evidence attempt recorded a real breach on
`direct-canary-close-nonstream-150` at 56.39 percent idle, a cell band 5 uses as its
closing canary and band 2 as its baseline, and had that cell failed band 1 the message
would have named a bottleneck while saying nothing about the breach. There is
deliberately no path by which contention turns a failure into a pass, and a passing cell
is not annotated at all.

**A direct cell in an uncalibrated group now FAILS the band** with the group named,
rather than borrowing a neighbour's ceiling. That can only fire if the matrix gains a
direct cell at a new mode or payload, which is a matrix change and must arrive with its
own calibration.

**THE CALIBRATION-PROVENANCE RULE IS NOW A STANDING REQUIREMENT** and is published in
`benchmarks/results/README.md` rather than living only in this entry. For every
threshold: name the cells it was calibrated on, name every cell it is applied to, and
where those sets differ in response mode, payload size or anything else that moves the
quantity, either split the threshold or state why the quantity is the same. Two
corollaries: freeze and name the calibration cut, and check the number against a second
data cut before committing it. That is the general lesson from four lost evidence runs.

**STILL THIN, recorded rather than closed.** All four calibrations come from one host,
and three rest on 10 to 15 readings of which all but one run is quick-mode. The streaming
group keeps the tightest margin at 1.49x. The 4096B group is the strictest in fold terms
at 3.02x and is the most likely next to fire. And the anchor is a single point of failure
for all eight thresholds, since every one is the anchor's threshold scaled by a ratio.

**NOT RETROACTIVE.** `2026-09-17-4d3f224-m3pro-macos-evidence-r1` passes the amended
band and **stays INVALID**, with its own `bands.txt` keeping the `BAND1 FAIL` line and
the `VERDICT INVALID` it was judged under.

## 2026-09-17, band 1 gains a per-mode ceiling after a completed evidence run was refused by one calibrated on the wrong response shape

**This entry exists because a FOURTH evidence run was lost, and this one ran all 53
cells to completion before a band refused it.** No published number moves, because no
run has yet produced a publishable number. What changes is which cells band 1 judges
against which threshold.

**THE RUN.** `2026-09-17-4d3f224-m3pro-macos-evidence-r1` completed the full matrix and
was refused by band 1 and by nothing else. Its three direct streaming cells read steady
P50 **1.341, 1.230 and 0.951ms** against a single ceiling of 1.0ms. Every non-streaming
direct cell passed comfortably: canary-open 0.432, canary-close 0.506, payload-4096
0.546, payload-32768 0.836.

**NOT A CONTENTION FAILURE, checked rather than assumed.** Host idle at those three
cells read 61.8, 60.57 and 70.84 percent against the 60 percent floor, and the two
lowest-idle cells produced the two highest medians, so noise is in the reading. But the
run's `contended-cells.txt` names exactly ONE breaching cell,
`direct-canary-close-nonstream-150` at 56.39 percent idle, and it is none of the three.
Noise contributed. It is not the cause.

**THE CAUSE IS STRUCTURAL.** A streaming response replays six SSE events with a write
and a flush each, six TCP segments and six loopback round trips worth of scheduling. A
non-streaming response is one write. Duration to last byte is a DIFFERENT QUANTITY in
the two modes, so one ceiling could not serve both. Measured at 150 bytes, the only
payload where both modes exist, the streaming P50 median on this host is 0.648ms against
0.293ms non-streaming, a mode ratio of **2.21**, and each extra SSE segment costs about
**71us**.

**THIS WAS OFFERED AND REJECTED A DAY EARLIER, and the record says so rather than
presenting the split as a new insight.** When band 1 was amended on 2026-09-16, scoping
it by stream mode was explicitly offered as the alternative and was rejected in favour
of moving the quantile from P99 to P50. Two independent defects were bundled into one
choice and only one got fixed. The quantile move was right and it stands. It says
nothing about a ceiling calibrated on one response shape being applied to another.

**HOW THE ERROR SURVIVED THE FIRST AMENDMENT.** That amendment put direct P50 at
"around 0.3ms non-streaming and 0.46ms streaming" and concluded 1.0ms therefore sat at
two to three times expected. The non-streaming figure holds up at 0.293ms measured. The
streaming one does not: **0.46ms is below every streaming reading now in the tree**,
whose minimum is 0.540ms and median is 0.648ms. At 0.46ms the single ceiling would have
been 2.2 times expected and a split would have looked unnecessary. At 0.648ms it is 1.54
times, which is not a band. So the reasoning was sound on a streaming central value
about 30 percent too low, and the five matrices it drew on predate the currently
loadable directories so its figures cannot be recomputed. An amendment that justifies a
threshold as a multiple of an expected value has to name where that expected value was
measured, or the multiple cannot be rechecked when the data grows.

**THE AMENDED BAND.** Each streaming threshold is the non-streaming one times the
measured mode ratio, rounded DOWN to a round figure so the streaming arm stays
relatively stricter than the arm it derives from. P50 gate: 1.0 times 2.21 is 2.21ms,
rounded down to **2.0ms**. P99 advisory: 2.5 times 2.52 is 6.30ms, rounded down to
**5.0ms**. Both streaming thresholds are therefore exactly double their non-streaming
counterparts while both measured mode ratios exceed two. **The non-streaming values do
not move**, and their behaviour was verified unchanged by running both checker versions
over every results directory in the tree and over isolated boundary cases at 0.999 and
1.000ms.

```
quantity                                   non-streaming     streaming
P50 ceiling                                        1.0ms         2.0ms
payload-matched P50 median                       0.293ms       0.648ms
ceiling as a multiple of that median                3.41x         3.09x
ceiling over the largest reading in the mode        1.98x         1.49x
```

Under the old single ceiling the two arms disagreed by a factor of **2.2**: the
non-streaming arm refused at 3.41 times its central value while the streaming arm
refused at 1.54 times its own. They now agree to within 10 percent, which is the
property that makes this a split rather than a widening. It still fires on a uniform
3.09-fold host slowdown, and on a 4.8-fold rise in the mock's per-segment streaming
cost.

**THE P99 ADVISORY SPLIT ON A WEAKER ARGUMENT, and the argument that prompted it did
not survive measurement.** The split was proposed on the ground that the unsplit 2.5ms
advisory fires on most runs. It does not: across every reading in this tree it fires on
1 of 13 streaming cells and 1 of 41 non-streaming ones. What is true is narrower. The
largest streaming P99 on any run other than the failing one is 2.422ms, **97 percent of
the 2.5ms threshold**, so on the streaming arm the advisory sat one ordinary noise burst
below firing and carried almost no discriminating power.

**NOT RETROACTIVE.** The amended checker passes
`2026-09-17-4d3f224-m3pro-macos-evidence-r1` and that directory **stays INVALID**. Its
own `bands.txt` keeps the `BAND1 FAIL` line and the `VERDICT INVALID` it was judged
under, unmodified. A band amended after seeing a run and then applied backwards to that
run is not a pre-registered band. No figure is rendered from the directory and none of
its numbers is published.

**RECORDED FOR CONTEXT, NOT PUBLISHED.** Because the run completed, the bands that
passed carry information. These are NOT results and must not be quoted as levee's
measured overhead:

```
band            reading
BAND2           +0.287ms passthrough minus direct P50
BAND3 primary   +0.621ms at 4096B, spread 0.029ms across five repetitions
BAND4           +0.454ms P99 shift at 4096B
BAND3-STREAM    +0.115ms streaming enforce minus passthrough P50
BAND5           0.074ms canary drift at P50
A/A control     -0.008ms, a true zero read by the same estimator
```

The A/A control at -0.008ms beside a band 3 spread of 0.029ms is the informative pair:
that run's estimator was resolving its 4096B signal cleanly, which is why the streaming
floor failure is a band defect and not a bad run.

**THE AUDIT QUESTION GAINS A SECOND HALF.** The 2026-09-17 gate audit below asked
whether each gate is satisfiable in aggregate. It did not ask whether each threshold was
calibrated against the quantity it is applied to, and band 1 was satisfiable on the cells
it was derived from while being roughly twice as strict on a cell it was not. So for
every gate: name the cells its threshold was calibrated on, name every cell it is applied
to, and where those sets differ in response mode, payload size or anything else that
moves the quantity, either split the threshold or state why the quantity is the same.

**RECORDED AND NOT FIXED, because it is the tightest gate left in band 1.**
`direct-payload-32768` has read as high as **0.899ms** against its unchanged 1.0ms
non-streaming ceiling, 1.11-fold margin, and it read 0.836ms on the fourth attempt.
Every other direct cell has at least 1.49-fold. Band 1 is also the only band that does
not honour `contended-cells.txt`, deliberately, so no repetition-dropping rule can
rescue that cell on a loaded run.

## 2026-09-17, the two absolute-zero integrity gates become tolerances derived from each cell's demand

**This entry exists because a third evidence run died to a gate that could not be
satisfied, and this time the whole harness was audited for the same defect rather than
only the gate that fired.** No published number moves. What changes is what an
occurrence of a rare event DOES.

**The third evidence attempt died at cell 38 of 53.** At commit `5b2128c`, on cell
`passthrough-nonstream-4096-r5`, refused by `dropped_iterations{scenario:steady}:
count==0`. That cell dropped **one steady iteration of 30001 scheduled**, 0.003 percent,
while reporting p99 2.239ms, **500.0 of 500 rps demanded**, zero warmup drops, zero
failed requests, and **76 of 76 host CPU idle readings above the floor**. The directory
is `2026-09-17-5b2128c-m3pro-macos-evidence-r1`, it has no MANIFEST, and it stays an
aborted run rather than evidence.

**THREE ATTEMPTS, THREE DIFFERENT GATES, AND TWO OF THEM THE SAME MISTAKE.**

```
attempt   commit    died at              gate that fired                  occurrence that fired it
1         31918d9   completed, invalid   band 3, the 150B window          sustained host contention, correctly
2         377d97f   51m54s, cell 39/53   one sub-floor CPU idle reading   1 of 79 readings, at 58.40 pct
3         5b2128c   cell 38/53           dropped_iterations count==0      1 of 1.2 million iterations
```

Attempt 1 is different in kind and must not be lumped in: that band was right and the
run was genuinely invalid. Attempts 2 and 3 are one mistake twice. Both gates demanded
EXACTLY ZERO occurrences of a rare event across a very large number of independent
opportunities, 106 host readings and **1,224,053 steady iterations**, and in neither case
was the opportunity count ever multiplied by the rate of the event.

**THE AUDIT.** Every validity gate in `overhead.js`, `run.sh` and `check_bands.py` was
enumerated and each was asked how many independent chances it gets to fire in one
evidence run and whether it is satisfiable in aggregate on a normally behaving machine.
Three failed that test. The two k6 integrity thresholds below, and the streaming
repetition minimum. Everything else was left exactly as it was.

**THE DROP TOLERANCE, 1 percent of demanded steady iterations with an absolute floor of
25.** Calibrated against every cell this repository has recorded, **151 cells carrying
1,438,074 steady requests**. Seven had nonzero steady drops, at 0.003, 0.030, 0.130,
0.130, 0.130, 0.460 and 0.490 percent of their own demand. The worst two are the
load-bearing ones because neither can be saturation: **46 drops in a direct cell** with
no levee in its path, P50 0.339ms, and **49 in a 150-byte enforce cell** at roughly 11
percent of measured capacity, P50 0.503ms with a P99 of 9.154ms. So 0.490 percent is the
measured benign envelope and the tolerance sits **2.0 times above it**. All 151 recorded
cells pass under the new rule.

**It still catches the failure it was written for by 48.7 times.** That failure is the
32768-byte capacity problem: 14622 steady drops of 30001, **48.7 percent**, a 152.4ms
median that Little's Law attributes entirely to 40 requests waiting. The floor of 25 is
just under two of the exactly-13-drop stalls that four separate recorded cells show, and
it binds only below 2500 demanded iterations, which no cell in either mode reaches.

**THE FAILED-REQUEST TOLERANCE, 0.05 percent with a floor of 5**, twenty times tighter in
relative terms because the two counts mean different things. A dropped iteration is the
load generator giving up. A failed request is levee answering 429, erroring, or the
loopback stack breaking. **Zero failed requests have ever been recorded here**, 0 in
1,438,074, which is precisely why the absolute form still had to go: zero events in
1,438,074 trials bounds the per-request rate at **2.083e-6** at one-sided 95 percent
confidence, which over an evidence run's 1,224,053 steady requests is **up to 2.55
expected failures**. Every failure shape worth catching is sustained instead, a
one-second 429 storm at 500 rps being 500 failures against an allowance of 15.

**IS THE DROP GATE NOW REDUNDANT AGAINST THE ACHIEVED-RATE GATE.** Asked deliberately,
because the honest thing to do with a gate that adds nothing is delete it rather than
give it a tolerance. Drops subtract from completions **one for one**, measured exactly on
two recorded cells, so anything above the rate gate's 2 percent already fails there and a
drop tolerance at or above 2 percent would be strictly redundant. At 1 percent it is not,
and the reason is sensitivity rather than the narrow band between the two numbers: a drop
proves the VU pool had **no free slot at a scheduled arrival**, so the pool was
momentarily part of what the cell measured, and that costs hundredths of a percent of
throughput. Measured on the real script against an upstream that blocks the whole 40-slot
pool once, 500 rps over a 5 second window, 2500 demanded:

```
stall   steady drops   drop gate      achieved           rate gate   k6 exit
120ms             23   pass, 25 max   495.4 of 500 rps   pass             0
150ms             39   FAIL, 25 max   492.4 of 500 rps   pass            99
```

The second row settles it. The two gates fail in opposite blind spots, so both are kept,
each with a tolerance sized to its own measured envelope.

**THE STREAMING REPETITION MINIMUM, the third gate the audit found.** The contention
exclusion needs three clean repetitions before a median is an order statistic, and the
streaming matrix runs **three**, so it permitted **zero** contended streaming
repetitions across the 12 host idle readings its pairing spans. Pooling the two evidence
attempts that carry idle readings, 1 breach in 155, that is **7.5 percent of runs**, and
it fires after the last cell so it costs all 52 minutes. BAND3-STREAM now needs **one**
clean streaming repetition and prints a THIN MEDIAN line whenever contention cost it any.
That is defensible only because this gate is a two-sided **0.60ms** ceiling on a 15us
quantity, so it fires on roughly a 40-fold regression or a non-enforcing arm and both are
visible in one repetition, and because its central value is already advisory-only and the
band already refuses to publish it. Zero clean repetitions still fails. **The
non-streaming minimum is unchanged at three**, where five repetitions tolerate two
contended ones and the same arithmetic gives roughly 0.03 percent.

**NOTHING IS HIDDEN.** Every raw count is recorded whether it passed or not.
`dropped-iterations.txt` gained the allowance and the demanded count on every line, a new
**`failed-requests.txt`** carries the same shape for failures with a line per cell
regardless of outcome, each `summary.json` gained `demanded_steady_requests`,
`max_steady_dropped_iterations`, `max_steady_failed_requests`, `max_steady_failed_rate`
and the steady and warmup failure split, the MANIFEST records both tolerances with their
floors, and `bands.txt` prints a new **`INTEGRITY TOTALS`** line naming the run's total
steady drops and failed requests with their percentages of total demand. `check_bands.py`
re-derives both allowances with integer basis-point arithmetic identical to the shell's
and reports a disagreement if its figure and k6's differ.

**Both halves had to change together.** Leaving an absolute zero in `check_bands.py`
would have re-failed at the END of the matrix exactly the cell k6 had just correctly
tolerated, wasting all 52 minutes instead of the 38 cells the original gate wasted.

**THE LESSON GENERALISED, so it is not learned a fourth time.** Before writing or
tightening a gate, count the opportunities it gets in one full evidence run and multiply
by the observed rate of the thing it fires on. If the product is not comfortably below
one, the gate cannot be satisfied and will eventually be deleted in frustration rather
than obeyed, which loses the protection entirely. That is strictly worse than a tolerance
sized from measured data.

**Verification of this change.** No evidence matrix was run, because one costs 52 minutes
of a person's machine time. Instead:

- The threshold FORM was probed at the pinned k6 v2.2.0 before any of it was written.
  `count<=30` and `rate<=0.05` on tagged sub-metrics both parse and evaluate. A saturated
  window dropped 2754 of 4000 and reported `ok false` with exit 99 against `count<=30`
  and `ok true` against `count<=99999`. Six forced steady failures in a 100-iteration
  window reported rate 0.06 with `ok false` against `rate<=0.05` and `ok true` against
  `rate<=0.06`, so the boundary lands exactly where the arithmetic puts it.
- The REAL `overhead.js` was driven with allowances from the REAL `run.sh` functions
  against a scripted upstream. Healthy passed with the expressions carrying 25 and 0.05.
  Saturation dropped 723 of a 25 allowance and exited 99. Exactly 5 forced steady
  failures passed and 6 failed. A pool-blocking stall dropped 23 and passed while a
  longer one dropped 39 and failed with the rate gate clean.
- The shell and Python allowance arithmetic were cross checked across eight cell shapes,
  including every shape in both modes, and are **bit identical**.
- `check_bands.py` was driven over the REAL aborted evidence directory with one cell's
  counts rewritten: 1 drop passes and is listed as tolerated, 14622 drops fail naming
  both numbers, 300 drops and 15 failures pass exactly at the boundary, 301 drops fail,
  1 failed request passes, and 500 failed requests fail.
- A `RESULTS_MODE=quick` matrix reached **VERDICT VALID** end to end.
- The checker was re-run over all 16 directories in the results tree and **every verdict
  is unchanged**. None could have changed: a cell that fails a k6 threshold never gets a
  filtered CSV written, so every directory carrying a nonzero drop count already fails to
  load, which is a structural guarantee rather than a coincidence.

## 2026-09-16, the host quiescence gate now fails on SUSTAINED contention rather than on one dip

**This entry exists because the gate added in the entry below was unsatisfiable, and
an unsatisfiable gate gets deleted.** Nothing here changes the 60 percent idle floor,
the sampler, or any published number. What changes is what a mid-run breach DOES.

**The second evidence attempt died at 51 minutes 54 seconds.** At commit `377d97f`,
after **39 of 53 cells**, on cell `passthrough-nonstream-32768-r5` phase `before`
reading **58.40 percent** against the 60 percent floor. That directory is
`2026-09-16-377d97f-m3pro-macos-evidence-r1`, it has no MANIFEST, and it is an
aborted run rather than evidence. **The gate was correct in the narrow sense.** The
reading was genuinely sub-floor and the median-of-three confirmation agreed with it.
It was the only breach in the run's 79 recorded readings, whose median was 73.97.

**THE POLICY WAS WRONG, AND THE ARITHMETIC SAYS SO.** An evidence run takes 53 cells
times 2 readings, so **106 mid-run checks**. Ambient single samples on the reference
host reach down to **59.28** against a floor of **60**. The one evidence-scale
measurement of the CONFIRMED breach rate is that aborted run itself, **1 in 79**,
which is 1.34 expected breaches per full run. So at least one sub-floor reading per
run is close to inevitable, and a gate that refuses on one **can essentially never
complete a run on this host no matter how quiet the machine is**.

**The distinction now implemented is SUSTAINED versus TRANSIENT.** The failure the
gate exists to catch was sustained: all five repetitions of the 150-byte enforcement
pair inflated together, roughly **108us of amplification held across an entire 50
minute run**, with every integrity gate reading clean. One transient dip is a
different thing. **The floor is NOT lowered**, because contended single samples reach
58.77 against an ambient minimum of 59.28, so the single-sample regimes overlap
almost exactly and only the paired or median form separates them.

**The startup check is unchanged and is still a hard gate on ONE reading.** The
asymmetry is the design: refusing at startup costs five seconds, refusing at cell 40
costs the 52 minutes already spent. A cheap refusal should be eager and an expensive
one should be sure.

**Mid-run rule 1, two CONSECUTIVE confirmed sub-floor readings fail the run
immediately.** The before and after readings of one cell are adjacent in the ordered
sequence, so a cell contended from start to finish trips it at the cell that caused
it. **The original failure trips it trivially**: contention held across a whole run
breaches every reading, so the first adjacent pair arrives at reading 2 of 106 and the
run stops in its first two minutes. False-refusal cost on the observed 1.3 percent
per-reading rate, treating readings as independent, is 105 adjacent positions times
0.0127 squared, so **1.7 percent of quiet runs**. Independence overstates it: across
the two runs on this host that carry idle readings, **6 of 6 breaches landed on a
`before` reading and 0 of 52 `after` readings breached at all**, and since the phases
strictly alternate an adjacent pair requires an `after` breach.

**Mid-run rule 2, more than 10 percent of all mid-run readings breaching fails the run
at the END, before the MANIFEST is written.** This catches the pervasive-but-
intermittent host that never lands two breaches side by side. **That case is not
hypothetical.** It is the quick matrix at commit `31918d9` in this repository's own
results tree: **5 of 26 readings**, worst reading 51.48 which sits inside the proven
contended regime, at ordered positions 7, 15, 17, 19 and 21, so **no two were ever
adjacent**. Rule 1 would have passed that host.

Why 10 and not 5 or 15, computed rather than asserted. The quiet rate is estimated
from ONE event in one run, so its exact one-sided 95 percent Poisson upper bound is
4.744 events per 79 readings, which is 6.37 per 106:

```
threshold   fires at    false refusal at 1.34 expected   at the 6.37 upper bound
5 percent   6 of 106                 0.26 percent                61.1 percent
10 percent  11 of 106            0.000019 percent                 6.0 percent
15 percent  16 of 106        0.00000000015 percent                 0.1 percent
```

Five percent could refuse a majority of quiet runs and nothing in the data rules that
out. Fifteen percent sits only 1.25 times below the one contended host on record, so
it has almost no margin against the case it exists to catch. Ten percent is **7.7
times the observed quiet rate and roughly half the observed contended rate**, and it
is the only one of the three affordable at the pessimistic end while still clear of
the contended rate.

**Everything else records the dip, warns loudly, and continues.** The reading carries
`cpu_idle_breach=yes` in `machine-state.txt`, and the cell gets a line in a new
artifact, **`contended-cells.txt`**, with its phase, its reason and its idle reading.
An **absent** file means the directory predates the marking. A **present and empty**
one is a positive statement that no reading breached.

**`check_bands.py` ACTS on that marking rather than only printing it.** Every band
whose value is a median across repetitions **drops the contended repetitions before
computing**: band 2, band 3 at 4096B, band 4, BAND3-STREAM, BAND3-SMALL and
CONTROL-AA. A repetition is dropped when EITHER arm of its pair was contended, and the
scope is one pairing, because a repetition ordinal is a join key rather than a moment
in time and the 150B and 4096B cells of one repetition ran minutes apart. **This is
what the five-repetition design is for**: the published quantity is the median of five
per-repetition shifts, so it can afford to lose one. Below **three clean repetitions**
the gates fail with the cause named, because below three a median stops being an order
statistic and becomes a single reading wearing the word median. In quick mode, with
one repetition, excluding it leaves nothing, so the band is reported **UNEVALUABLE**
and the run is INVALID rather than silently passing.

**Single-instance cells are recorded and NOT dropped**, meaning the two drift canaries
and the two direct payload cells. There is nothing to drop them in favour of, and the
bands that read them already tolerate a contended host: band 5 measures canary drift
directly and gates it at 0.25ms, and band 1's ceiling is 1.0ms of direct P50 against
an observed **0.363 to 0.899ms on the most contended host in this results tree**. No
new failure path was added for them.

**What neither rule can do.** Both are built on the same 60 percent floor and inherit
its resolution. The proven contended regime costs 16 points of idle, one busy loop
costs about a third of that and still passes. These rules make the gate survivable.
They do not make it more sensitive.

**Verification of this change.** The sustained-breach logic was proven with **injected
synthetic readings**, not with an evidence matrix, because an evidence run costs 52
minutes of a person's machine time. `run.sh` gained a test hook,
`LEVEE_BENCH_SYNTHETIC_IDLE_READINGS`, read in exactly one function, and it **cannot
affect a real run**: `preflight` refuses to start any run in either mode while it is
set, and `preflight` is on the only path that creates a results directory, so the hook
is reachable only from a script that has SOURCED `run.sh` and therefore never calls
`main`. Five scenarios were driven through the real functions in seconds:

- One isolated confirmed dip in evidence mode: **run continues**, the reading is
  marked, the cell is recorded, the MANIFEST is written.
- Two consecutive confirmed dips in evidence mode: **fails at the second reading**,
  exit 1, leaving `attempts.txt`, `contended-cells.txt` and `machine-state.txt` and
  **no MANIFEST**. The same readings in quick mode warn and continue.
- Two non-adjacent breaches in 12 readings, 16.7 percent: **fails after the last
  cell**, before the MANIFEST, with the cause in `attempts.txt`.
- Two non-adjacent breaches in 22 readings, 9.1 percent: **completes**.
- A single sub-floor sample followed by two clean ones: **voted out** by the
  median-of-three confirmation and never counted as a breach.

The startup gate was re-verified in evidence mode on a clean tree with three
background busy loops present. It refuses in the first seconds and **no results
directory is created**. The exclusion arithmetic was verified against a copy of the
aborted evidence directory, which carries five real repetitions at 4096B: marking one
of them contended moved the band 3 median from **0.678ms to 0.685ms**, narrowed the
reported spread from 0.139 to 0.039ms across 4 repetitions, and carried the same
filter into band 4. Marking three of the five made band 3 and band 4 FAIL with the
cause named. A `RESULTS_MODE=quick` matrix reached `VERDICT VALID` end to end.

**Where the sampler is still weak, recorded rather than fixed.** The `before` reading
runs systematically LOWER than the `after` reading on this host, by 6.6 points in one
recorded run and 10.9 in another, and every breach ever recorded here landed on a
`before` reading. That sample is taken right after a levee spawn, a config render and
the previous cell's TIME_WAIT drain, so its 2 second window can overlap the harness's
own setup work rather than pure ambient load. The bias is left in place because every
calibration figure in the gate was measured through this same sampler and moving the
sample point would orphan all of them. It is written down so nobody reads a low
`before` reading as proof of an outside job.

## 2026-09-16, the first evidence run was INVALIDATED, and the harness gained a host quiescence gate

**This entry exists so nobody reads 123us as levee's enforcement cost.** The first
completed 43-cell evidence run, at commit 31918d9 on the reference host, was
**INVALIDATED by band 3**. It measured a median repetition-matched enforce minus
passthrough P50 delta of **+123us at 150 bytes**, per repetition +107, +175, +126,
+114 and +123us, against a pre-registered window of 0 to 100us. Every other gate
passed: 43 cells, zero steady dropped iterations, zero failed requests, every k6
threshold green, every cell at 100.0 percent of its demanded arrival rate, and
0.076ms of canary drift at P50.

**The investigation attributed it to host CPU contention rather than to levee.** The
evidence, in the order it was taken:

- On a quiet host, six pairs measured in **both orders** at the same commit, the
  same configs and the same load shape read **+13, +14, +16, +16, +19 and +14us,
  median +15us**.
- An **A/A control**, passthrough against passthrough, whose true value is zero,
  read **-1, +4 and 0us**, so the estimator's noise floor is about **4us**.
- **Three background busy loops** move the same measurement to **+93 and +109us**
  and move both arms' absolute P50 onto the invalidated artifact's own values,
  while still achieving 500.0 rps with zero steady drops and zero failed requests.
  That regime would have passed every integrity gate the harness had.
- The invalidated artifact's own opening direct canary, with **no levee in the
  path**, climbs from 0.291 to 0.392ms across the six slices of its own 60-second
  window.

**THE CORRECTED FIGURE. Levee's enforcement cost at 150 bytes is +15us net on a
quiet host**, not 123us. It decomposes into 38.2us of gross enforce-only work, two
tokenizer passes at 17.4us each plus 2.1us of logging plus 0.9us of drift
observation plus 0.4us for admission and reconcile, offset by a **24us credit**
because the SHARED path runs faster in the enforce arm. Lock contention is 124ns per
request and metrics 853ns, so neither is in the story, and GC costs 19.7us of CPU
and **zero latency**, because marking runs on idle and dedicated workers rather than
as request-goroutine assists.

**THE HONEST LIMIT, stated because it bounds every claim above.** Contention of that
size is proven **SUFFICIENT** to produce the invalidated run's numbers. It is **NOT
proven to be what that run had.** loadavg was effectively identical in both regimes,
4.0 to 6.4 during the invalidated run against 2.8 to 5.0 during the quiet
re-measurements, and **no better proxy was recorded at the time**, so the actual
contention level during those 50 minutes is unrecoverable. That gap is exactly what
the new gate closes going forward and cannot close backwards.

**New gate, host quiescence.** `run.sh` now samples system-wide CPU idle percentage
into `machine-state.txt` as `cpu_idle_pct`, before and after every cell and once
before the first one, from the second sample of `top -l 2 -n 0 -s 2`. In evidence
mode a reading below **60 percent** refuses the run. In quick mode it warns.
Calibrated against 26 readings in each of two regimes, ambient at 59.28 to 76.32 and
ambient plus three busy loops at 41.87 to 58.77, with a paired same-moment form
showing the loops cost a median of 16.12 points of idle and never less than 11.94.
An unreadable sensor refuses an evidence run rather than passing silently. A single
sub-floor reading is re-sampled twice and the median decides, so one transient
cannot abort a 50 minute run while sustained contention still does. **loadavg is
kept and not gated**, because a field proven not to discriminate is worth keeping
visible beside one that does.

**New control cells, A/A.** Two cells per repetition at 150 bytes, both running the
passthrough config, so their repetition-matched P50 shift has a known true value of
zero. `bands.txt` prints it as `CONTROL-AA` beside the enforcement readings and
states that its expected value is zero. It is **reported and never gated**, because
a contended A/A pair still read 13us, so a small control reading does not certify a
quiet host. Necessary, not sufficient. Cost is 2 cells in quick mode and 10 in
evidence mode.

**The PRIMARY enforcement gate moved from 150 bytes to 4096 bytes.** Read the reason
precisely: **the 150-byte band was correct and the run that failed it was invalid.**
The gate moved because a 15us signal against a 4us noise floor is 3.7 to 1 and
cannot be gated reliably, while the same measurement at 4096 bytes is +655us against
the same floor, 164 to 1. Both spreads come from the same invalidated run: 12us
across five repetitions at 4096 bytes, which is 1.8 percent of the signal, against
68us at 150 bytes, which is 55 percent of it. The new window is **0.15 to 0.95ms**,
the floor sized to admit the pending single-pass fix and reject a non-enforcing arm,
the ceiling sized to catch a third tokenizer pass near 993us. The 150-byte delta is
still computed and printed on every run as `BAND3-SMALL` against an advisory window
of **-0.05 to +0.10ms**, and it no longer gates. Band 4 followed band 3 to 4096
bytes because it is a ratio against band 3's median. Quick mode's default payload
set gained 4096 so a local check can still exercise the primary gate.

**The 150-byte advisory floor is NEGATIVE on purpose.** The pending product fix
removes the duplicate tokenizer pass, dropping gross enforce-only work from 38.2us
to 20.8us against the same 24us shared-path credit, which puts the net near **-3us**.
A floor of zero would fail every quiet run on a codebase that had just become
faster. A negative reading there does not mean enforcement became free: the work is
measured by the 4096-byte gate and by `microbench.txt`, neither of which can go
negative.

**Corrected two committed constants.** `check_bands.py` recorded the estimator at
"EstimateSplit 4.32us, Estimate 4.29us" and an in-process total shift of 16.9us.
Measured on the exact load-generator 150-byte body at the pinned toolchain,
go1.26.3 on an Apple M3 Pro with the fixture model id resolving to `o200k_base`:
**17,364 ns/op, 12,936 B/op, 163 allocs/op**, and 116, 104 and 106 ns per prompt
byte at 150, 4096 and 32768 bytes. So one pass is 17.4us, two are 34.8us, and a
16.9us total was arithmetically impossible. The 4.32us figure also contradicted this
repository's own README, which has said 120ns per prompt byte all along. The
retracted probe's streaming-to-non-streaming ratio of 1.16 goes with it and is now
marked unverified. The 0.60ms streaming ceiling is unchanged: recomputed with the
corrected 15us inherent term, its basis moves from 356us to 351us.

**Corrected the log-line framing.** Band 3's comment treated two extra structured
log lines as a pollution source to accommodate and named them as the leading suspect
when the band failed. Forcing the passthrough arm to write the same two lines
changed its P50 by **0.0us** and its CPU by **0.0000 ms per request**, so their
marginal cost is not detectable at the P50 and never explained a 123us reading. The
`logcost` component stays inside the enforcement figure because the work is real,
2.1us of CPU by the run's own `microbench.txt`. What changed is that it is no longer
offered as an explanation for a shift it cannot produce.

**The relocation is not retroactive.** The invalidated directory stays invalidated.
Running today's checker over it prints `VERDICT VALID`, which is a property of the
new gate rather than a re-blessing of the old numbers, and its 150-byte reading of
+123us remains contaminated.

**Verification of this change.** A quick matrix of 13 cells reached `VERDICT VALID`
with every band, the RATE gate and the COST table passing, and it happened to run on
a host that dipped below the idle floor on **five of its 26 readings**, which makes
it a better test than a clean one would have been:

- `BAND3 PASS` at 4096B, +0.805ms against the 0.15 to 0.95ms window.
- `BAND3-SMALL RECORDED` +0.061ms at 150B, four times the +15us quiet value on a host
  where the 4096B reading moved 23 percent. The relocation working as designed.
- `CONTROL-AA RECORDED` **-0.027ms at a true zero**, so the estimator invented 27us
  of shift on that host against 4us on a quiet one. That is the number band 3 never
  had, printed beside the reading it qualifies.
- The quiescence gate warned five times with the exact cell and phase named, and
  quick mode continued, as designed.
- `machine-state.txt` carries `cpu_idle_pct` from 51.48 to 76.68 across the run while
  `loadavg` sat between 4.65 and 8.33. The two fields disagree inside one artifact,
  which is the finding that motivated the gate, now visible rather than asserted.

The refusal path was verified separately in evidence mode with three background busy
loops present. The run refuses at startup, before any cell, naming the reading and
the floor.

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
  and 32KB for direct cells. Quick mode runs 150B only. **SUPERSEDED**: a 4KB
  direct cell was added later the same day, and quick mode now runs 150B and 4KB
  because the primary enforcement gate moved to 4KB, see the entry above.
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
   **AMENDED later the same day, see the entry above:** the gate moved to the 4096B
   payload with a 0.15 to 0.95ms window, and the 150B delta is now recorded against
   an advisory window of -0.05 to +0.10ms. The reason was signal to noise, NOT a
   failing run. This window was correct.
4. Median enforce minus passthrough P99 shift no more than ten times the median
   P50 shift. **AMENDED later the same day:** it followed band 3 to 4096B, because
   it is a ratio against band 3's median.
5. Opening and closing direct canaries within 0.25ms absolute drift at P50 and
   1.50ms at P99, both of them gates, unlike band 1 where the tail only
   advises. **AMENDED on this date**, from the original 15 percent
   agreement at both quantiles. A percentage tolerance on a sub-millisecond
   quantity is tighter than the deltas the experiment publishes. The 1.50ms
   ceiling is calibrated against six historical matrices and rejects two of
   them, and that calibration table is published with the band.

Both amendments are recorded with their evidence in
`benchmarks/results/README.md` rather than applied quietly, because both were
made after runs failed the original form. The band 3 and band 4 amendments that
arrived later the same day are recorded there too, and were made for the opposite
reason: the window they moved was correct and the run that failed it was invalid.

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
`benchmarks/harness/logcost` benchmark rather than hardcoded. **The framing of that
last figure is CORRECTED by the entry above**: the 2.1us of CPU is real, and its
effect on the P50 is 0.0us, so the two lines were never a pollution source and never
explained a high reading. Time to first byte
sits at 60 to 85 percent of full-stream duration because the mock replays events
with no pacing, which is a property of the mock and not of levee.

**Provenance.** Each run writes a MANIFEST last, after the bands pass and the
identity audit is clean, recording the tool versions, the host, the sysctls, the
per-cell power and load and thermal readings, plus the per-cell CPU idle readings
added by the entry above, the fixture digests, and both the
commit SHA and the tree hash. The tree hash is the field that survives this
project's squash merges.
