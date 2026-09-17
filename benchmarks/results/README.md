# Committed benchmark evidence

One directory per benchmark run, written by `benchmarks/harness/run.sh`. Every
number in a figure, and every latency claim anywhere in this repository, traces
to one of these directories. The methodology that produced them is in
`benchmarks/README.md`.

## Directory naming

```
<YYYY-MM-DD>-<shortsha>[-dirty]-<hwtag>-<mode>-r<n>
```

- `YYYY-MM-DD` is UTC on the day the run started.
- `shortsha` is `git rev-parse --short HEAD` at run start. The binary under test
  is built from it with `-ldflags -X main.version=bench-<shortsha>`, and every
  proxied cell asserts the running process reports exactly that stamp on its
  admin `/health` endpoint before k6 sends a request.
- `-dirty` appears when `git status --porcelain` was non-empty. A dirty run
  cannot be rebuilt from any commit, so it can never be evidence.
- `hwtag` is an honest hardware label, `m3pro-macos` for the reference host.
  Reproducibility here comes from the methodology and the manifest, not from
  quiet hardware.
- `mode` is `quick` or `evidence`.
- `r<n>` is a same-day run ordinal that resolves collisions. A second run on the
  same day at the same SHA is routine during development, and it must never
  clobber a committed one, so `run.sh` counts up until it finds a free name and
  refuses to write into a directory that already exists.

## What gets committed

Only `evidence` directories. Quick-mode runs are disposable local checks with
one repetition at one payload size, which cannot resolve the enforcement signal
at all, so `.gitignore` in this directory excludes them along with any
dirty-tree run. That exclusion is mechanical rather than a convention someone
has to remember.

Evidence mode refuses to start on a dirty tree, on battery power, with Low
Power Mode on, or on a host below the CPU idle floor. It also refuses to
CONTINUE when the host stays below that floor part way through, so a machine that
becomes busy mid-matrix stops the run where it happened rather than producing
another 40 cells of unusable numbers. **Mid-run that refusal needs SUSTAINED
contention**, two consecutive readings or more than a tenth of all of them, because
one isolated dip in 106 readings is expected on the reference host and a gate refusing
on one could never complete a run. An isolated dip is recorded and its repetition is
excluded from every median instead. The amendment is documented in full below.

## The first completed evidence run was INVALIDATED

Recorded here rather than left in a git log, because a reader who finds the number
without the correction would publish it.

The first 43-cell evidence run, at commit `31918d9` on the reference host, was
**INVALIDATED by band 3**. It measured a median repetition-matched enforce minus
passthrough P50 delta of **+123us at 150 bytes**, per repetition +107, +175, +126,
+114 and +123us, against a pre-registered window of 0 to 100us. **Everything else
about that run was clean:** 43 cells, zero steady dropped iterations, zero failed
requests, every k6 threshold green, every cell at 100.0 percent of its demanded
arrival rate, and 0.076ms of canary drift at P50. That combination is the whole
lesson. A run can pass every integrity gate in this harness and still be measuring
the wrong thing.

**The investigation attributed it to host CPU contention rather than to levee.** Six
pairs measured in both orders on a quiet host read +13, +14, +16, +16, +19 and
+14us. Three background busy loops move the same measurement to +93 and +109us and
move both arms' absolute P50 onto the invalidated artifact's own values, while still
achieving 500.0 rps with zero steady drops. The artifact's own opening direct canary,
with **no levee in the path**, climbs from 0.291 to 0.392ms across the six slices of
its own 60-second window.

**THE CORRECTED FIGURE. Levee's enforcement cost at 150 bytes is +15us net on a
quiet host, not 123us.** It is 38.2us of gross enforce-only work against a 24us
credit for the shared path running faster in the enforce arm. Do not quote the 123us.

**THE HONEST LIMIT.** Contention of that size is proven **SUFFICIENT** to produce
those numbers. It is **NOT proven to be what that run had.** loadavg was effectively
identical in both regimes, 4.0 to 6.4 during the invalidated run against 2.8 to 5.0
during the quiet re-measurements, and **no better proxy was recorded at the time**,
so the actual contention level during those 50 minutes is unrecoverable. The host
quiescence gate below exists to close that gap going forward. It cannot close it
backwards.

**What the harness gained as a result:** the host quiescence gate, the A/A control
pair, and the relocation of the primary enforcement gate to the 4096-byte payload.
All three are documented below with their calibration.

### The second evidence attempt was ABORTED by the gate the first one prompted

Recorded here for the same reason, because a reader who finds the aborted directory
without this note would not know why it stops mid-matrix.

`2026-09-16-377d97f-m3pro-macos-evidence-r1` **died at 51 minutes 54 seconds, after 39
of 53 cells**, refused by the strict form of the host quiescence gate on cell
`passthrough-nonstream-32768-r5` phase `before` reading **58.40 percent** against the
60 percent floor. It has **no MANIFEST**, so it is self-evidently an aborted run and
not evidence. **The gate was right about the reading and wrong about the policy:** that
was the only breach in 79 readings whose median was 73.97, and an evidence run takes
106 such readings on a host whose ambient minimum is 59.28 against a floor of 60. The
amendment that followed is documented under the quiescence gate below. It does not
lower the floor.

### The third evidence attempt was KILLED BY ONE DROPPED ITERATION

`2026-09-17-5b2128c-m3pro-macos-evidence-r1` **died at cell 38 of 53**, refused by the
k6 threshold `dropped_iterations{scenario:steady}: count==0` on cell
`passthrough-nonstream-4096-r5`. That cell dropped **one steady iteration of 30001
scheduled**, which is 0.003 percent. Everything else about it was pristine: p99
2.239ms, **500.0 of 500 rps demanded**, zero warmup drops, zero failed requests, and
**76 of 76 host CPU idle readings above the floor**, so the two quiescence rules added
the day before never even had an opinion. Its own recorded threshold block is the whole
story in two lines, `dropped_iterations{scenario:steady}:count==0 fail` beside
`http_reqs{scenario:steady}:count>=29400 pass`. It has no MANIFEST and it is an aborted
run rather than evidence.

### The fourth evidence attempt COMPLETED and was INVALIDATED by band 1

`2026-09-17-4d3f224-m3pro-macos-evidence-r1` **ran all 53 cells to completion** and
was refused by band 1 and by nothing else. Its three direct streaming cells read
steady P50 **1.341, 1.230 and 0.951ms** against a single ceiling of 1.0ms that was
calibrated on non-streaming cells. Every non-streaming direct cell passed
comfortably. The full diagnosis, the evidence table and the derivation of the
per-mode ceiling that replaced it are under band 1 below.

**The amendment is NOT retroactive and this directory stays INVALID.** The amended
checker would pass it, and that changes nothing: a band amended after seeing a run
and then applied backwards to that run is not a pre-registered band at all. The
directory's own `bands.txt` keeps the `BAND1 FAIL` line and the `VERDICT INVALID`
it was judged under, unmodified, and that file is the historical record. The amended
form binds the NEXT run.

**Recorded for context, and NOT published results.** Because the run completed, the
bands that did pass carry information even though the run is not publishable. These
numbers must not be quoted as levee's measured overhead, and no figure is rendered
from this directory:

```
band            reading
BAND2           +0.287ms passthrough minus direct P50
BAND3 primary   +0.621ms at 4096B, spread 0.029ms across five repetitions
BAND4           +0.454ms P99 shift at 4096B
BAND3-STREAM    +0.115ms streaming enforce minus passthrough P50
BAND5           0.074ms canary drift at P50
A/A control     -0.008ms, a true zero read by the same estimator
```

The A/A control at -0.008ms and the 0.029ms band 3 spread are the two most
informative of these, because together they say the run's estimator was resolving
its 4096B signal cleanly. That is precisely why the streaming floor failure is a
band defect rather than a bad run.

### Four attempts, and three of the four gates could not be satisfied

Read these together, because individually each gate looked reasonable and the
pattern is only visible across them.

```
attempt   commit    died at              gate that fired                  occurrence that fired it
1         31918d9   completed, invalid   band 3, the 150B window          sustained host contention, correctly
2         377d97f   51m54s, cell 39/53   one sub-floor CPU idle reading   1 of 79 readings, at 58.40 pct
3         5b2128c   cell 38/53           dropped_iterations count==0      1 of 1.2 million iterations
4         4d3f224   completed, invalid   band 1, one ceiling for 2 modes  streaming P50 1.341ms against 1.0ms
```

Attempt 4 is a THIRD distinct failure shape and belongs with neither group. It is
not an absolute zero and it is not a correct refusal. It is a threshold applied to a
quantity it was never calibrated against, which the numbers make plain: a 1.0ms
ceiling sits at 3.41 times the non-streaming central value and only 1.54 times the
streaming one, so it was roughly twice as strict on the streaming arm purely by
accident of how it was derived.

Attempt 1 is different in kind and must not be lumped in: **that band was right and the
run was genuinely invalid.** Attempts 2 and 3 are the same mistake twice. Both gates
demanded EXACTLY ZERO occurrences of a rare event across a very large number of
independent opportunities, 106 host readings and 1,224,053 steady iterations
respectively, and neither number of opportunities was ever weighed against the rate of
the event.

**The lesson generalised, so the absolute-zero form is not written again.** Before a
gate is written or tightened, count the opportunities it gets in one full evidence run
and multiply by the observed rate of the thing it fires on. If the product is not
comfortably below one, the gate cannot be satisfied and will be deleted in frustration
rather than obeyed, which loses the protection entirely. That is strictly worse than a
tolerance sized from measured data. An audit of every gate in the harness against that
test was run on 2026-09-17 and the two integrity thresholds below were the remaining
absolute forms it found, along with the streaming repetition minimum.

**Attempt 4 is the second lesson, and the audit above would not have caught it.**
That audit asked whether each gate was satisfiable in aggregate. It did not ask
whether each gate's threshold was calibrated against the quantity it is applied to.
A gate can be perfectly satisfiable on the cells it was derived from and
unsatisfiable on a cell whose underlying quantity differs by a factor of two, which
is exactly what band 1 was. That lesson is written up as a standing requirement in the
next section, because it is the general finding from four lost runs and it belongs in
the public record rather than only in the amendment that discovered it.

## Calibration provenance, a standing requirement for every threshold

**This is a rule for any threshold added or changed from 2026-09-17 onward, not a note
about one past failure.** It exists because band 1 was amended twice in two days for
the same category error on two different axes, and neither amendment would have been
needed had this been checked when the threshold was written.

Every threshold in this harness MUST record three things, and any reader is entitled to
find all three next to the number:

1. **The cells it was CALIBRATED on.** Name them, and name where their readings can be
   recomputed. A threshold justified as a multiple of an expected value has to say where
   that expected value was measured, or the multiple cannot be rechecked once the data
   grows.
2. **Every cell it is APPLIED to.** Not the cells it was tested against. Every cell the
   checker will actually judge with it.
3. **Whether those two sets differ** in response mode, payload size, arrival rate, or
   any other term that moves the underlying quantity. Where they differ, **either split
   the threshold per group or state explicitly why the quantity is the same.** Silence
   here is the defect. It is what let a ceiling derived from one-write 150-byte
   responses judge a six-write response and a 32768-byte response.

Two corollaries, both learned the hard way and both cheap to honour:

- **Freeze and name the calibration cut.** State how many results directories it pools
  and which ones. A later run is judged **against** the frozen cut and its readings are
  recorded, never silently folded back in to move the threshold. Band 1's second
  amendment did not do this and its evidence table was stale within minutes of its own
  verification run, which is how a reader ends up unable to reproduce the number.
- **Check the threshold against a second data cut before committing it.** If adding or
  removing one run moves the number, the number is not a calibration. Band 1's third
  amendment found two of its four advisory thresholds sitting on a rounding boundary
  exactly this way, and resolves such a straddle to the stricter side.

The two rules compose. The absolute-zero test above asks whether a gate is satisfiable
at all. This one asks whether it is satisfiable **on the cells it will judge**. A gate
has to pass both.

## Pre-registered sanity bands

These bands are pre-registered, meaning each was fixed before the numbers it
gates were measured. Where one was later changed, the change is recorded below as
an amendment with its evidence, never folded in silently. A run that violates a
band is INVALID and gets debugged, never published. They are evaluated
mechanically by
`benchmarks/plots/check_bands.py` from the committed per-request CSVs, never by
a human looking at a figure and judging whether the numbers seem reasonable,
which is the exact failure pre-registration exists to prevent. The verdict is
written to `bands.txt` in the run directory and a violation exits non-zero.

They are reproduced here because the design document that pre-registered them
lives under `docs/`, which is not committed. Pre-registration is only meaningful
if the bands are public before the numbers are.

**Four of them were amended during implementation, and band 1 three times.** That is
exactly the move a skeptical reader should scrutinize, so every amendment is
recorded below in full, with the evidence and the calibration that motivated it.
None was widened to rescue a specific run, and no amendment is applied
retroactively to the run that prompted it.

**Band 1's second and third amendments are one day apart and share one cause**, which
is stated here rather than left to be inferred from two adjacent sections. Both fix a
threshold calibrated on one quantity and applied to a different quantity reported in
the same unit: the second on the response mode axis, the third on the payload size
axis. The third replaces both special cases with a single rule that covers every axis,
so a fourth amendment of that shape should not be needed. The general lesson is written
up as a standing requirement under "Calibration provenance" below.

Bands 1 and 5 were amended after runs failed their original form. Band 3 was
amended for a different reason and the distinction matters: **its window was
correct and the run that failed it was invalid.** The gate moved to a larger
payload size where the same quantity has 160 times the signal-to-noise ratio, and
the 150-byte window is unchanged in shape and still printed on every run. Read
that section carefully before concluding a band was relaxed to make a number
pass, because that is the opposite of what happened.

One distinction to hold onto while reading them, because bands 1 and 5 both grew
a tail clause in those amendments and the two clauses do OPPOSITE things. Band
1's P99 is a RECORDED ADVISORY: it is printed on every run and it never blocks
publication, and its per-group thresholds exist only so host noise is visible rather
than invisible. Band 5's P99 IS A GATE: exceed
1.50ms of canary drift and the run is invalid. A careless reading conflates them and
concludes either that levee's tail is gated when it is not, or that canary drift is
merely noted when it is disqualifying.

### Band 1, direct-to-mock floor

**As amended, the GATE is:** the P50 of every direct-to-mock cell is below the
ceiling calibrated for ITS OWN GROUP, where a group is a response mode paired with
a payload size. That is the whole gate. The P99 of every direct cell is also
recorded in `bands.txt` against that group's ADVISORY threshold, but the tail is not
a verdict on the run and a run is never rejected for it. The advisory exists so that
host noise is stated out loud instead of passing unremarked, not so that it can
block publication.

```
direct-cell group        P50 ceiling (GATE)   P99 advisory (RECORDED)
non-streaming    150B                1.0ms                     2.5ms
non-streaming   4096B                1.0ms                     2.5ms
non-streaming  32768B                1.5ms                     3.5ms
streaming        150B                2.0ms                     5.5ms
```

Every one of those eight numbers is that group's own measured median multiplied by a
single multiplier shared across all four groups, rounded down. The rule and its
derivation are in the third amendment below.

A direct-to-mock cell measures the generator, the loopback stack and the mock.
If that floor already sits at the proxy budget then the box or the mock is the
bottleneck and no proxied number from the same run means anything.

**AMENDED 2026-09-16.** Originally pre-registered as direct P99 below 1.0ms.

Why it moved. The 1.0ms figure comes from Tenet 1, which is a claim about proxy
OVERHEAD, a delta between two distributions, and the original band had borrowed
it as an absolute bound on a single cell. Worse, the P99 form was detecting host
tail noise while claiming to detect a bottleneck, and those are different
things: a genuine bottleneck raises the CENTRAL tendency, while the resident
endpoint-security agents on this host raise only the TAIL, so a P99 bound cannot
tell them apart. The design had already settled this same question elsewhere by
ruling that SLO values are drawn on figures and never enforced as thresholds, so
that a noisy run reports honestly instead of failing. The original P99 form was
that ruling violated from inside the band checker.

Evidence that prompted it, direct P99 across the first five matrices on the
reference host: canary-open 0.547 to 1.353, canary-close 0.660 to 4.080,
payload-32768 0.623 to 1.320, streaming 0.831 to 1.426. The streaming cell's
median P99 of 0.976ms sat on the original threshold by construction, since six
write and flush cycles cost more than one write. Direct P50 over the same runs
was around 0.3ms non-streaming and 0.46ms streaming, so a 1.0ms P50 ceiling sits
at roughly two to three times expected.

Every direct P99 is still printed in `bands.txt`, so a reader can apply the
original stricter band and see what it would have said.

**AMENDED AGAIN 2026-09-17, into the per-mode pair stated above.** The
non-streaming numbers did not move. Streaming gained its own ceiling of 2.0ms at
P50 and its own advisory at 5.0ms.

**This was not a fresh insight, and the record should not read as if it were.**
When band 1 was amended the day before, scoping it by stream mode was explicitly
offered as the alternative and was **rejected** in favour of moving the quantile
from P99 to P50. Two independent defects were bundled into one choice and only one
of them got fixed. The quantile move was right and it stands. It says nothing at
all about a ceiling calibrated on one response shape being applied to a different
one, and that is the defect that was left standing until a completed evidence run
collected on it.

**What failed.** `2026-09-17-4d3f224-m3pro-macos-evidence-r1` completed all 53
cells and was refused by this band and by nothing else. Its three direct streaming
cells read P50 **1.341, 1.230 and 0.951ms** against the 1.0ms ceiling, while every
non-streaming direct cell in the same run passed with room: canary-open 0.432,
canary-close 0.506, payload-4096 0.546, payload-32768 0.836.

**Not a contention failure, and that was checked rather than assumed.** The host
idle readings at those three cells were 61.8, 60.57 and 70.84 percent against the
60 percent floor, and the two lowest-idle cells produced the two highest medians,
so noise is certainly in the reading. But that run's `contended-cells.txt` names
exactly **one** breaching cell, `direct-canary-close-nonstream-150` at 56.39
percent idle, and it is none of these three. Noise contributed. It is not the
cause.

**The cause is structural.** A streaming response replays six SSE events with a
write and a flush each, which is six TCP segments and six loopback round trips
worth of scheduling. A non-streaming response is one write. Duration to last byte
is therefore a **different quantity** in the two modes rather than the same
quantity measured twice, and one ceiling cannot serve both.

Every direct cell reading in this tree, recomputed from the committed steady-only
CSVs. Six early directories are aborted runs that never wrote a filtered CSV and
are absent for that reason:

```
group                  n   P50 median   P50 min   P50 max   P99 median   P99 max
non-streaming   150B   22       0.293     0.255     0.506        0.720     1.611
non-streaming  4096B    8       0.334     0.307     0.546        0.855     1.419
non-streaming 32768B   11       0.485     0.278     0.899        1.193     2.835
streaming       150B   13       0.648     0.540     1.341        1.816     2.606
```

The thirteen streaming P50 readings in full, sorted, because the ceiling is derived
from them: 0.540, 0.555, 0.584, 0.597, 0.600, 0.643, 0.648, 0.649, 0.743, 0.791,
0.951, 1.230, 1.341. The last three are the failing run's three repetitions.

**How the error survived the first amendment**, visible by comparing that
amendment's own cited figures with the table above. It put direct P50 at "around
0.3ms non-streaming and 0.46ms streaming" and concluded that 1.0ms therefore sat at
two to three times expected. The non-streaming figure holds up, 0.293ms measured.
The streaming one does **not**: 0.46ms is below every streaming reading now in this
tree, whose minimum is 0.540ms and whose median is 0.648ms. At 0.46ms the single
ceiling genuinely would have been 2.2 times expected for streaming and a split would
have looked unnecessary. At the measured 0.648ms it is 1.54 times, which is not a
band at all. The conclusion was arithmetically sound on a streaming central value
roughly 30 percent too low. The five matrices it drew on predate the currently
loadable directories, so its figures cannot be recomputed and are left as written.
The lesson is narrow and worth keeping: an amendment that justifies a threshold as a
multiple of an expected value has to name where that expected value was measured, or
the multiple cannot be rechecked when the data grows.

**A provenance trap worth naming**, because it produced two slightly different sets
of numbers for the same cells while this amendment was being derived. That run's
streaming P50 values are 1.341, 1.230 and 0.951 in the committed CSVs and 1.339,
1.238 and 0.947 in the summary JSON `p(50)` fields. This band gates the CSV values,
because the summary aggregates the whole invocation including warmup and gating on
it would gate numbers nobody publishes. Anyone re-deriving these thresholds has to
read the CSV column.

**The derivation, one rule covering both thresholds.** Each streaming threshold is
the non-streaming one multiplied by the **measured mode ratio**, then rounded
**down** to a round figure so the streaming arm stays relatively stricter than the
non-streaming arm it derives from. At P50 that is 1.0 times 2.21, which is 2.21ms,
rounded down to **2.0ms**. At P99 it is 2.5 times 2.52, which is 6.30ms, rounded
down to **5.0ms**. So both streaming thresholds are exactly double their
non-streaming counterparts while both measured mode ratios exceed two, and the
rounding direction is conservative by construction rather than by taste.

```
quantity                                   non-streaming     streaming
P50 ceiling                                        1.0ms         2.0ms
payload-matched 150B P50 median                  0.293ms       0.648ms
ceiling as a multiple of that median                3.41x         3.09x
ceiling over the largest 150B reading               1.98x         1.49x
```

Both rows are payload-matched at 150 bytes, the only payload where both modes exist,
so the non-streaming column is **not** that arm's worst case. Against
`direct-payload-32768`, whose largest reading in this tree is 0.899ms, the unchanged
non-streaming ceiling has only **1.11-fold** margin. That is the tightest margin
anywhere in band 1 after this amendment, and it is examined under the host quiescence
gate below.

So the two arms now refuse a run at almost the same severity, 3.41 against 3.09, a
10 percent disagreement. Under the single 1.0ms ceiling they disagreed by a factor
of **2.2**, the non-streaming arm refusing at 3.41 times its central value while
the streaming arm refused at 1.54 times its own. That is the defect stated as a
number, and it is why the unsplit ceiling cut through the middle of a distribution
whose observed span is 0.540 to 1.341ms.

**It still detects a bottleneck**, which is this band's whole purpose and the test
any widening has to survive. Two failure shapes, both still caught:

- **The box slows down.** A uniform 3.09-fold slowdown trips the streaming gate and
  a 3.41-fold one trips the non-streaming gate, so a genuinely saturated host now
  fails band 1 on both arms at comparable severity. The old single ceiling did not
  have that property, and a host slow enough to matter would have been caught on
  the streaming arm alone at 1.54-fold, which reads as a streaming problem rather
  than as the host problem it is.
- **The mock becomes the bottleneck on its streaming path**, which is the one
  failure only this cell can see. Its per-segment cost would have to rise from
  about 71us to about 341us, a **4.8-fold** rise in the term that is unique to
  streaming.

Neither is a subtle regression, and that is said out loud rather than implied. This
band has never been a sensitivity instrument. It answers one question, whether the
floor is so high that no proxied number in the run means anything.

**Why an inflated streaming floor is not by itself disqualifying**, which is what
licenses sizing this ceiling on relative rather than absolute grounds. The direct
streaming cell has exactly two consumers: this band, and the baseline that
`overhead_figure.py` pairs the streaming arms against. Both published streaming
quantities are **differences**. The figure draws P99 proxied minus P99 direct with
the same-mode direct cell as its baseline, and `BAND3-STREAM` is enforce minus
passthrough and never reads the direct cell at all. A floor inflated uniformly by
host state therefore largely cancels out of both. What would not cancel is a
bottleneck confined to the direct arm, and that shows up as a shrinking or negative
passthrough minus direct shift rather than as a raised absolute floor.

**The residual gap, recorded rather than closed.** Band 2 gates passthrough minus
direct at 150B with a floor of 0.05ms, so a direct arm inflated on its own is caught
for the non-streaming mode. There is no streaming equivalent, so a streaming-only
direct inflation is visible on the figure and gated nowhere. Closing that needs a
streaming band 2, which is a matrix change rather than a threshold change.

**The claim that prompted the P99 split does not survive measurement**, and it is
recorded that way rather than quietly repaired. The split was proposed on the ground
that an unsplit 2.5ms advisory fires on most runs and so becomes noise a reader
learns to ignore. It does not. Across every reading in this tree it fires on **1 of
13** streaming cells and **1 of 41** non-streaming ones. What is true is narrower
and is the real reason to split it: the largest streaming P99 on any run other than
the failing one is **2.422ms, which is 97 percent of the 2.5ms threshold**, so on
the streaming arm the advisory sat one ordinary noise burst below firing and carried
almost no discriminating power when it did fire. At 5.0ms it fires on the class of
burst it exists to name. The band 5 calibration table below records non-streaming
closing canaries reaching 4.080 and 3.301ms, which are 5.7 and 4.6 times the
non-streaming P99 median, and a burst of that relative size on a streaming cell
reads 10.3 and 8.3ms.

**What this split does not fix, found while deriving it and left alone on purpose.**
The advisory is miscalibrated across **payloads** as well as across modes, and worse
in that dimension. The 32768-byte non-streaming cell has a P99 median of 1.193ms
against the same 2.5ms advisory, 2.10 times, the tightest relative advisory of any
group, and it is the only non-streaming cell that has ever fired the advisory, at
2.835ms. A per-payload split would be this same argument in a third dimension. It is
not made here, because this is an advisory that never blocks publication and because
bundling it in would repeat the exact bundling mistake this amendment exists to
correct.

**Limitation, and it argues for a wider number than the one chosen.** The mode ratio
above is pooled across 11 runs, of which 10 are quick-mode. Split by run mode it is
**2.14 on quick data and 2.62 on the one evidence run**, because that run's
streaming cells were elevated 1.98-fold over quick while its non-streaming 150B
cells were elevated only 1.61-fold, which is what six segments of exposure to
scheduler stalls looks like. Calibrating on the evidence cut would give 2.6ms rather
than 2.0ms. It was **not** used, for two reasons: it rests on 3 streaming and 2
non-streaming readings from a single run, and that run is the one that failed, so
sizing the ceiling to it is fitting a band to the run it has to judge. The
consequence is stated plainly instead: at 2.0ms a fifth evidence run as loaded as
the fourth has **1.49-fold margin** on this arm. If a run that passes the quiescence
floor ever reads above 2.0ms here, the response is to examine the mock's per-segment
cost and the host, not to widen the ceiling.

**AMENDED A THIRD TIME 2026-09-17, into the per-group table stated above.** The
per-mode pair becomes one calibrated P50 ceiling and one calibrated P99 advisory for
every direct-cell group in the matrix.

**This is band 1's third amendment and the second in two days, and the two are the
same category error found on two different axes.** A reader who sees two amendments
to one band inside two days deserves to be told at once that they are not repeated
tuning. The error in both is a threshold calibrated on one quantity and applied to a
different quantity that happens to be reported in the same unit. The amendment above
found it on the **response mode** axis, where a six-write streaming response was
judged by a ceiling derived from one-write non-streaming cells. This one finds it on
the **payload size** axis, where a 32768-byte response was judged by a ceiling
derived from 150-byte cells. Nothing about the second fix generalised it, because it
was written as a special case, and its own text names the payload dimension as
unaddressed. That is the shape this amendment removes: it stops patching band 1 one
dimension at a time and states one rule covering every dimension at once.

**The rule, restateable without reference to any individual run.** Each
direct-cell group's P50 ceiling is that group's own median P50 multiplied by a single
multiplier held constant across every group, rounded **down** to the nearest 0.5ms.
The P99 advisory is the same rule on the same group's own median P99 with its own
single multiplier. Two multipliers in total, one per quantile, and no group has a
multiplier of its own.

**The multiplier is not a free parameter**, which is what keeps this from being a
widening. It is fixed by the one band 1 threshold this harness has always had, the
1.0ms non-streaming 150-byte P50 ceiling, divided by that group's own median. So the
anchor group's product is 1.0ms exactly, the round-down is a no-op on it, its ceiling
is unchanged by construction, and every other group is set at the same relative
strictness the original ceiling expressed. The advisory multiplier is fixed the same
way from the 2.5ms non-streaming advisory.

```
P50 multiplier   1.0 / 0.288 = 3.47
P99 multiplier   2.5 / 0.752 = 3.32
```

The two were set independently, amendments apart, and they land within 5 percent of
each other. That is a coherence check rather than an argument, and it is recorded as
one.

**One addition to the rounding rule, forced by measurement and recorded as forced
rather than presented as foresight.** Where a product straddles a 0.5ms boundary
across the data cuts available, the threshold takes the **lowest** bin any cut
produces. Without that clause the streaming P99 advisory would have been 6.0ms on the
cut this amendment was derived from and 5.5ms on the cut that existed forty minutes
later, after its own verification run added five readings. A threshold that depends
on which hour the data was pooled is not a calibration, and resolving the straddle
downward is the only direction that cannot be mistaken for fitting a threshold to
make something pass.

**This rule subsumes the amendment above rather than contradicting it.** That one set
the streaming P50 ceiling to the non-streaming ceiling times the measured **mode
ratio**, rounded down. Substitute **group ratio** for mode ratio and the two rules
are the same rule, because a group ratio is that group's median over the anchor's
median, and 1.0 times that ratio is exactly the group's median times the multiplier.
Applied to the streaming group it reproduces 2.0ms to three decimal places. So the
streaming P50 gate set the day before is **confirmed** by the general rule rather
than replaced by it, which is the strongest evidence available that the
generalisation is the right one.

**The calibration cut is frozen at 13 loadable directories and named here**, which is
the whole point of the calibration-provenance rule recorded further down this file. A
later run is judged **against** the frozen cut and its readings are recorded, never
silently folded back in to move the thresholds, because a threshold that re-derives
itself on every run is not pre-registered. The amendment above did not freeze its cut
and went stale within minutes of its own verification run.

Every direct cell reading in the frozen cut, recomputed from the committed
steady-only CSVs. Six early directories are aborted runs that never wrote a filtered
CSV and are absent for that reason:

```
group                  n   P50 median   P50 min   P50 max   P99 median   P99 max
non-streaming   150B   26        0.288     0.255     0.506        0.752     1.611
non-streaming  4096B   10        0.332     0.307     0.546        0.875     1.419
non-streaming 32768B   13        0.481     0.278     0.899        1.130     2.835
streaming       150B   15        0.643     0.523     1.341        1.804     2.606
```

**That table is two directories larger than the one in the amendment above**, and the
difference is named rather than left for a reader to trip over, because it is the same
staleness trap that amendment fell into. It pooled 11 directories and read n of 22, 8,
11 and 13. The twelfth is `2026-09-17-4d3f224-dirty-m3pro-macos-quick-r1`, that
amendment's own verification run, written minutes after its table was computed and
therefore absent from it. The thirteenth is
`2026-09-17-16883b9-dirty-m3pro-macos-quick-r1`, this amendment's first verification
run. Five direct cells each, which is the whole difference.

**The four P50 ceilings are identical on all three cuts**, which was checked rather
than assumed. Products, and the bin each rounds down into:

```
cut       multiplier   ns 150B        ns 4096B       ns 32768B      stream 150B
11 dirs        3.413   1.000 -> 1.0   1.140 -> 1.0   1.655 -> 1.5   2.212 -> 2.0
12 dirs        3.436   1.000 -> 1.0   1.148 -> 1.0   1.643 -> 1.5   2.216 -> 2.0
13 dirs        3.472   1.000 -> 1.0   1.151 -> 1.0   1.670 -> 1.5   2.233 -> 2.0
```

**And the order of events matters, so it is stated.** The four ceilings were fixed
from the 12-directory cut **before** the verification run existed. That run then
contributed five more direct readings and the 13-directory cut reproduces all four
bins. It is a small out-of-sample confirmation, one run wide, and it is the only one
available.

**The advisories are measurably less stable than the gates**, and pretending
otherwise is how the streaming advisory would have been wrong within the hour:

```
cut       multiplier   ns 150B        ns 4096B       ns 32768B      stream 150B
11 dirs        3.472   2.500 -> 2.5   2.969 -> 2.5   4.142 -> 4.0   6.306 -> 6.0
12 dirs        3.324   2.500 -> 2.5   2.882 -> 2.5   3.860 -> 3.5   6.017 -> 6.0
13 dirs        3.324   2.500 -> 2.5   2.909 -> 2.5   3.757 -> 3.5   5.997 -> 5.5
```

**Two of the four straddle a bin edge** and the lowest-bin clause resolves both,
giving 2.5, 2.5, 3.5 and 5.5. The diagnosis is worth recording because the two
straddles have **different causes**. The 32768B one is genuine estimator movement:
its P99 median walked 1.193 to 1.161 to 1.130 across the cuts, 5.3 percent, while its
P50 median moved only 1.4 percent over the same three cuts. The streaming one is not
its own movement at all, since its P99 median walked just 0.7 percent. It is the
**anchor** moving: the anchor's P99 median went 0.720 to 0.752, 4.4 percent, which
changed the multiplier every advisory is built from. That is the structural reason the
tail cannot be pinned as tightly as the centre, a P99 median is estimated from far
fewer effective samples than a P50 median, and it applies with extra force to the
anchor because the anchor scales all four.

**The resolution is post hoc and is labelled post hoc.** The lowest-bin clause was
written after seeing the 13-directory cut. It is defensible for two reasons and
neither is foresight: the P99 is an advisory that never blocks publication, and taking
the lowest bin is the strictest choice available, which is the opposite direction from
fitting a threshold to rescue a run.

**All eight thresholds then survived a two-run out-of-sample check**, which is what the
frozen cut exists to make possible. Two further quick runs were measured after the
thresholds were fixed, `2026-09-17-16883b9-dirty-m3pro-macos-quick-r2` and `-r3`,
adding 10 direct readings. Every one passes its gate with 3.12x to 3.70x fold headroom
and not one fires its advisory, the closest being the 4096B tail at 2.89x. Pooling them
into a 15-directory cut moves no product across a bin edge: the P50 products become
1.000, 1.147, 1.663 and 2.105 and the P99 products 2.500, 2.881, 3.757 and 5.811, which
round into the same eight thresholds. They are **not** re-derived on that cut,
deliberately. It is a check, and the frozen cut stays frozen.

**The streaming P50 product is the one that moved most**, from 2.233 to 2.105, and that
is named because it is the closest thing here to a warning. It moved 5.7 percent on two
added readings, both of them low, and its bin edge is at 2.000. So a run of quiet
streaming cells pushes this product **down** toward the edge, and if it ever crosses,
the rule as stated would put the streaming ceiling at 1.5ms rather than 2.0ms. That
would be a **tightening** driven by the host being quiet, which is a perverse
direction, and the response then is to say so and keep 2.0ms rather than to follow the
arithmetic off a cliff. The rule sizes a ceiling from a central value. It does not
license ratcheting one down every time the machine has a good day.

**The result.** `margin` is the threshold over the largest reading that group has ever
produced on this host, so it is the headroom a fifth evidence run actually has. `fold`
is the threshold over that group's own median, so it is the uniform slowdown that
trips it:

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

**The point of the uniform multiplier is that no group was given a licence.** Realised
P50 strictness now spans 3.02x to 3.47x, a 15 percent disagreement, and every group
sits at or **stricter** than the anchor because rounding down can only reduce a
ceiling. Before this amendment the same span was **2.08x to 3.47x**, a 67 percent
disagreement: `direct-payload-32768` carried a 1.0ms ceiling against a 0.481ms median,
refusing at 2.08 times its own central value, while the anchor refused at 3.47 times
its own.

**And no group is left at a cliff**, which is the failure this amendment exists to
prevent. The tightest P50 margin is the streaming group's 1.49x, accepted
deliberately in the amendment above, and nothing is below it. `direct-payload-32768`
goes from **1.11x to 1.67x**, so the cell that would most likely have refused a fifth
evidence run no longer sits one noise burst from its ceiling. The advisory margins are
looser as a set and one of them, the 32768B group's 1.23x, is below the accepted
streaming floor. That is not held to the same standard on purpose: an advisory
**firing** is its correct behaviour rather than a lost run, so a small margin there
costs a line of output and nothing else.

**Every ceiling still detects a genuine bottleneck**, stated per group because a
ceiling that cannot is not worth having. Two readings of each: the uniform slowdown
that trips it, and the rise in the term **unique** to that group with the shared base
term held fixed, where the base is the 0.288ms one-write 150-byte round trip.

- **non-streaming 150B, 1.0ms.** Trips on a 3.47-fold uniform slowdown. This group
  *is* the base term, so it has no unique term and this is the whole of what it sees.
- **non-streaming 4096B, 1.0ms.** Trips on a 3.02-fold uniform slowdown, the
  strictest of the four. Its unique term, the payload-proportional cost above the
  base, is only 44us, so tripping on that term alone needs a 16.4-fold rise in it.
  Said plainly: this cell is a second reading of the base cost rather than a sensitive
  probe of 4KB handling, and its value is the uniform-slowdown arm.
- **non-streaming 32768B, 1.5ms.** Trips on a 3.12-fold uniform slowdown. Its unique
  term is 193us of payload-proportional work, which would have to reach 1212us, a
  **6.3-fold** rise. That is the one failure only this cell can see, a mock or a
  loopback that has become bad at moving 32KB.
- **streaming 150B, 2.0ms.** Trips on a 3.11-fold uniform slowdown. Its unique term is
  71us per extra SSE segment, which would have to reach 342us, a **4.8-fold** rise.

None of these is a subtle regression, and that is said out loud rather than implied.
This band has never been a sensitivity instrument. It answers one question, whether
the floor is so high that no proxied number in the run means anything.

**What actually moved**, so a reader can audit the change against the claim. Of the
eight thresholds, five are unchanged: both 150B P50 ceilings, the 4096B P50 ceiling,
and the 150B and 4096B non-streaming advisories. Three move:

```
threshold                              before    after   why
non-streaming 32768B P50 ceiling         1.0ms    1.5ms   the rule, first calibration
non-streaming 32768B P99 advisory        2.5ms    3.5ms   the rule, first calibration
streaming 150B P99 advisory              5.0ms    5.5ms   ad hoc rounding replaced
```

**The streaming advisory moving is a rounding correction and not a new judgement**, and
it is the one number here a skeptic should press on, because it is the only widening
not driven by a first calibration. The amendment above computed 2.5 times a 2.52 mode
ratio, got 6.30ms, and rounded that down to **5.0** in order to make the streaming arm
exactly double the non-streaming one. Exactly-double was a taste, not a rule: rounding
6.30 down to the nearest whole millisecond gives 6.0, and to the nearest half also
6.0, so 5.0 was reachable only by choosing the answer first. Under the stated rule the
product is 5.997 and the bin is 5.5. The observable consequence of the move is **nil**
and that was checked: the largest streaming P99 anywhere in this tree is 2.606ms, so
5.0 and 5.5 both fire on zero of 15 readings.

**The only output that changes on any existing directory is the 32768B advisory.**
Across every directory in the tree, and all 64 direct readings in the frozen cut, no
P50 gate outcome changes at any of the four groups, and the one advisory line
that disappears is `2026-09-16-31918d9-dirty-m3pro-macos-quick-r1` at 2.835ms, the
only non-streaming reading ever to fire it. No directory's VALID or INVALID verdict
changes, because an advisory never blocks publication. That was established by running
the pre-amendment and post-amendment checkers over every directory and comparing both
the exit code and the set of `BAND1` lines, not by reasoning about the thresholds.

**Both raised advisories still fire on the class of burst they exist to name**, which
is the test a raised advisory has to survive. The band 5 calibration table below
records non-streaming closing canaries reaching 4.080 and 3.301ms, which are 5.43 and
4.39 times the non-streaming P99 median. A burst of that relative size reads 6.1 and
5.0ms on the 32768B group against its 3.5ms advisory, and 9.8 and 7.9ms on the
streaming group against its 5.5ms advisory. All four exceed their threshold.

**This supersedes two paragraphs of the amendment above** and they are left standing
rather than edited, because an amendment record that gets rewritten is not a record.
Its closing section states that `direct-payload-32768` keeps 1.11-fold margin and that
widening it would need a calibration this tree cannot supply. The margin is now
1.67-fold. The claim about calibration was the part that was wrong: the tree supplies
13 readings of that cell, and what was missing was not data but a rule for turning a
group's own readings into its own ceiling. Its "what this split does not fix" section
names the per-payload advisory miscalibration and declines to fix it, correctly, on
the ground that bundling it into the mode split would repeat the bundling mistake that
amendment was about. This amendment is that fix, unbundled, which is the form that
objection asked for.

**Band 1 now reads `contended-cells.txt`, and only to annotate a failure.** This is a
reversal of the sentence in that same closing section, which recorded band 1 as the
only band that does not honour the ledger, so the reason has to be exact. What that
sentence got **right** is the substantive part and it still stands: contention must
never make a band 1 failure disappear. A floor measured on a busy machine is still the
floor that run's proxied numbers sit on, and three of the four direct groups are
single-instance cells with no second repetition to be dropped in favour of, so the
repetition-dropping mechanism every other band uses cannot rescue them even in
principle. What it got **wrong** is treating "must not excuse" as a reason not to
**read** the file. The fourth evidence attempt recorded a real breach on
`direct-canary-close-nonstream-150` at 56.39 percent idle against a 60 percent floor,
a cell that band 5 reads as its closing canary and band 2 reads as its baseline. Had
that cell failed band 1 rather than a streaming cell, the message would have named a
bottleneck and said nothing about the recorded breach, and a reader would have had to
cross-reference two files by hand to tell a genuine floor problem from a contended
sample. So the ledger is now read, a failing cell named in it is **annotated** with
its recorded breach, and the verdict is identical either way. There is deliberately no
code path by which contention turns a band 1 failure into a pass. A passing cell is
not annotated at all, because the contention block already prints the whole ledger
above band 1 and repeating it on a pass would be noise.

**A direct cell in an uncalibrated group fails the band** with the group named, rather
than borrowing a neighbouring group's ceiling, which would be the exact category error
this amendment removes. It can only fire if the matrix gains a direct cell at a new
mode or payload, which is a matrix change and has to arrive with its own calibration.

**What is still thin, recorded rather than closed.**

- **All four calibrations come from one host**, and three of the four rest on 10 to 15
  readings of which all but one run is quick-mode. An evidence run loads the box for 52
  minutes and a quick run does not, and the fourth attempt showed streaming cells
  elevated 1.98-fold over quick while non-streaming 150B cells rose only 1.61-fold. A
  second reference host would change these numbers.
- **The streaming group keeps the tightest margin at 1.49x.** Accepted deliberately in
  the amendment above and the reasoning is unchanged: calibrating on the evidence cut
  alone would give 2.6ms, and it rests on 3 readings from the run the band has to
  judge, which is fitting a band to its own subject.
- **4096B is the strictest group in fold terms at 3.02x**, purely because 1.151 rounds
  down to 1.0. Rounding down is the conservative direction by construction, so this is
  accepted rather than corrected, but it is the group most likely to be the next one to
  fire, and its own margin over its worst reading is 1.83x.
- **The anchor is a single point of failure for all eight thresholds.** Every one of
  them is the anchor group's threshold scaled by a ratio, so a mistake in the anchor's
  own median propagates everywhere at once. Its P50 median is stable across the three
  cuts to 1.7 percent, which is what licenses the gates. Its P99 median moved 4.4
  percent, which is why the advisories needed the lowest-bin clause.

**Not retroactive**, on the same terms as the amendment above.
`2026-09-17-4d3f224-m3pro-macos-evidence-r1` passes the amended band and **stays
INVALID**, and its committed `bands.txt` keeps the `BAND1 FAIL` line and the `VERDICT
INVALID` it was judged under. The amended form binds the next run.

### Band 2, the extra proxy hop

The median passthrough minus direct P50 shift falls within 0.05 to 0.6ms. Below
the floor the extra loopback HTTP hop is missing, which means the cell did not
go through the proxy at all. Above the ceiling something other than the hop is
being paid.

Recorded for accuracy: the design sketched this window as "roughly 0.1 to
0.4ms" and the checker implements the slightly wider 0.05 to 0.6ms. That width
was set when the checker was first written, before any matrix had been measured
against it, and it has not moved since. Unlike bands 1 and 5 this is not an
amendment in response to a failure.

### Band 3, enforcement over passthrough

**As amended, the GATE is:** at the **4096-byte** payload, the median
repetition-matched enforce minus passthrough P50 delta falls within **0.15 to
0.95ms**, and the spread across repetitions is smaller than the delta itself. If
the spread exceeds the signal, the run cannot resolve the enforcement cost and the
answer is more repetitions, not a wider band. With one repetition the spread check
is arithmetically vacuous, and `bands.txt` says so out loud rather than letting a
vacuous pass read as a real one.

The 150-byte delta is still computed and printed, as `BAND3-SMALL`, against an
advisory window of **-0.05 to +0.10ms**. It does not gate.

**AMENDED 2026-09-16.** Originally pre-registered, and until this date enforced, as
the 150-byte delta within 0 to 100us.

**Read the reason precisely, because it inverts easily.** The 150-byte window was
CORRECT and the run that failed it was INVALID. The first completed 43-cell
evidence run measured +107, +175, +126, +114 and +123us against that window, median
+123us, and the band refused it. An investigation then proved the band right and
the run wrong:

- On a quiet host, six pairs measured in **both orders** at the same commit, the
  same configs and the same load shape read **+13, +14, +16, +16, +19 and +14us,
  median +15us**.
- An **A/A control**, passthrough against passthrough, whose true value is zero,
  read **-1, +4 and 0us**, so the estimator's noise floor is about **4us**.
- **Three background busy loops** move the same measurement to **+93 and +109us**
  and move both arms' absolute P50 onto the invalidated artifact's own values,
  while still achieving 500.0 rps with zero steady drops and zero failed requests.
  That regime would have passed every integrity gate the harness had.
- The +15us decomposes into 38.2us of gross enforce-only work, two tokenizer passes
  at 17.4us each plus 2.1us of logging plus 0.9us of drift observation plus 0.4us
  for admission and reconcile, offset by a **24us credit** because the SHARED path
  runs faster in the enforce arm. Lock contention is 124ns per request and metrics
  853ns, so neither is in the story, and GC costs 19.7us of CPU and **zero
  latency**, because marking runs on idle and dedicated workers rather than as
  request-goroutine assists.

**So the window was not widened. The gate moved, and the reason is signal to noise
and nothing else:**

```
payload   measured shift   estimator noise floor   ratio    within-run spread
150B          +15us               4us              3.7:1    68us, 55 pct of it
4096B        +655us               4us            164:1      12us, 1.8 pct of it
```

Both spread figures come from the **same** invalidated 43-cell run, which is what
makes the comparison fair rather than selective. A 3.7:1 gate cannot be relied on.
A 164:1 gate can.

**The 4096-byte numbers**, from that run's five repetition-matched pairs.
Passthrough P50 0.790, 0.807, 0.795, 0.797 and 0.798ms against enforce 1.445,
1.453, 1.440, 1.454 and 1.453ms, so the shifts are +655, +646, +645, +657 and
+655us, median **+655us**, spread **12us**.

**How the window was derived.**

- **Floor 0.15ms.** Its job is to catch an enforce arm that is not enforcing, which
  reads what the A/A control reads, 0 to 15us, so the floor is an order of
  magnitude clear of that. Its binding constraint is the **pending product fix**
  that removes the duplicate tokenizer pass. One pass at 4096B measures 423.6us in
  isolation while the in-server double-pass shift is 655us, which puts the
  in-server per-pass cost near 338us, so the post-fix shift is either 317us or
  231us depending on which of those two figures is the honest one. The floor sits
  at least 1.5 times below the lower of them, so the fix cannot fail this gate.
- **Ceiling 0.95ms.** Forty-five percent above the measured 655us. It catches a
  **third tokenizer pass**, which lands near 993us, and a third pass is the most
  likely regression shape precisely because a duplicate SECOND pass is the defect
  the pending fix removes. Stated plainly: it does **not** catch a 20 percent
  regression.

**What the relocation does not do.** The 4096-byte numbers above come from the
INVALIDATED run and they would have PASSED this window. That is the point rather
than an embarrassment: the contention that destroyed the 150-byte band moved this
measurement by less than its own window, which is exactly why this one can be
gated. **The check that refuses a contended host is the host quiescence gate, not
this band**, and no band can do that job.

**A second 4096-byte reading**, from the quick matrix that first ran this gate. It is
recorded because it is the only other one in existence and because it is the less
flattering of the two. That run's host dipped below the idle floor on five of its 26
readings, so an evidence run would have refused it outright:

```
payload   passthrough P50   enforce P50   shift    A/A control   150B shift
4096B         0.599ms         1.404ms    +805us      -27us         +61us
```

Three things to take from that row. The 4096-byte gate passed with 145us to spare.
The A/A control read **-27us at a true zero**, which is the noise a contended host
puts into the estimator and is seven times the 4us quiet floor. And the 150-byte
reading went to +61us, four times its quiet value, on a host where the 4096-byte
reading moved by 23 percent. That is the signal-to-noise argument reproducing itself
inside a single run.

The two readings together, 655us and 805us, bound the **between-run spread at 4096
bytes on this host at 150us**. The ceiling sits 45 percent above the quieter of them
rather than 10 percent above it for exactly that reason. If a run that PASSES the
quiescence floor ever reads above 0.95ms, the response is to count the tokenizer
passes and examine the numbers, not to widen the ceiling.

**The relocation is not retroactive.** The invalidated directory stays invalidated.
Running today's checker over it prints `VERDICT VALID`, which is a property of the
new gate rather than a re-blessing of the old numbers, and its 150-byte reading of
+123us remains contaminated. A run's verdict is the verdict it was published under.

**LIMITATION.** One matrix has ever run 4096-byte pairs, five repetitions inside a
single run, so the WITHIN-run spread is measured at 12us and the BETWEEN-regime
bias at 4096 bytes is not measured at all. The window is 800us wide against a bias
of the 108us magnitude seen at 150 bytes, so such a bias cannot move the verdict.
That headroom is why the window is wide rather than tight.

**On the two extra log lines, CORRECTED 2026-09-16.** This section used to say the
delta "legitimately INCLUDES" two extra structured log lines and treat them as a
pollution source to accommodate, and the band's own failure message named them as
the leading suspect. That framing was wrong. Forcing the passthrough arm to write
the same two lines changed its P50 by **0.0us** and its CPU by **0.0000 ms per
request**, so the marginal cost of the two lines is **not detectable at the P50**
and never explained a 123us reading. The work is still real, 2.1us of CPU by the
run's own `microbench.txt`, so the `logcost` component stays inside the enforcement
figure and is still annotated separately there. What changed is that it is no
longer offered as an explanation for a shift it cannot produce. There is still no
log-level knob in levee, so the two cells cannot be equalized by configuration,
which is why the experiment was run instead.

At 32KB the delta is governed by the measured tokenizer curve rather than by a
fixed window.

### The A/A control pair, ADDED 2026-09-16

Two extra cells per repetition at the 150-byte payload, both running the
**passthrough** config, so the repetition-matched P50 shift between them has a
**known true value of zero**. Whatever it reads is noise the estimator invented.
`bands.txt` prints it as `CONTROL-AA`, immediately below the enforcement readings
it qualifies, and says out loud that its expected value is zero.

**Why it exists.** Band 3 published a 15us enforcement signal without ever
measuring what its own estimator reads when the answer is zero, which is the one
number that says whether 15us is a measurement or a rounding error. On a quiet host
the control reads -1, +4 and 0us, so the floor is about 4us and the 15us signal is
genuinely above it.

**Why it is reported and not gated.** Under the three-busy-loop contention that
reproduced the invalidated run, the A/A pair still read **13us**. A true zero
reported as 13us means a passing control does **not** certify a quiet host, so
gating on it would manufacture exactly the false confidence the control was added
to remove. It is **necessary and not sufficient**. The gate against contention is
the host quiescence gate below.

**Why it restarts levee between the two arms.** The enforcement pair it calibrates
does, so a control that skipped the restart would be measuring a different
estimator. Everything else is identical too: same payload, same rate, same VU pool,
same TIME_WAIT drain, and adjacency in the matrix so it sees the same host.

**Why once per repetition.** The published quantity is the median of the
per-repetition shifts, so the noise floor that matters belongs to that median
rather than to one pair. Matching the repetition count exactly is what makes the
control the same estimator applied to a known zero. It costs 2 cells in quick mode
and 10 in evidence mode.

### Band 4, the P99 companion to band 3

The median enforce minus passthrough P99 shift does not exceed ten times the
median P50 shift. A tail shift that far above the median shift means one cell
caught a transient even though every median gate passed.

When the median P50 shift is zero or negative a ratio against it is undefined,
so the band falls back to ten times band 3's own ceiling. That is the widest the
ratio form could ever have allowed while band 3 passed, so the fallback can never
be looser than the rule it stands in for.

**AMENDED 2026-09-16, and only because band 3 moved.** This band is defined as a
ratio against band 3's median P50 shift, so it followed band 3 to the 4096-byte
payload: a 150-byte P99 shift divided by a 4096-byte P50 shift would be arithmetic
between two different experiments. The 150-byte P99 shift is still printed, beside
the 150-byte median, so nothing that used to be visible stopped being visible. The
move also fixes this band on its own terms, since ten times a 15us median is a 150us
allowance on a quantity whose host-noise component is measured in milliseconds.

### Band 3-STREAM, the streaming enforcement shift, ADDED 2026-09-16

This one was NOT pre-registered. It was added after implementation, and it is
deliberately not a gate on the value it reports, so read it differently from
the five above.

**What it does:** always prints the median streaming enforce minus passthrough
P50 shift. Advises when that shift falls outside the 150-byte advisory window of
-0.05 to +0.10ms, saying the reading is drift-dominated and must not be published
as the streaming enforcement cost. GATES only on absolute magnitude, two-sided, at
0.60ms.

**Why it is not a gate on the value.** The reading is not a stable central
quantity. Across five quick matrices the streaming shift measured +59, +215,
-336, +163 and +26 microseconds. It changes sign, and nothing in the component
decomposition can produce a third of a millisecond of either sign, so the large
readings are between-cell drift rather than code. The code reading is what carries
this claim: nothing on the streaming path is enforcement-conditional, since the
stream_options injection, the per-event usage inspection and the stream reconcile
are all paid by the passthrough arm too and cancel out of the shift. So the
streaming path adds essentially the same enforcement work as the non-streaming
path, which nets +15us on a quiet host.

**A retracted number, recorded because it was published here.** This section
previously cited an in-process probe measuring the true shift at 19.5us streaming
against 16.9us non-streaming, a ratio of 1.16. **Those absolute figures were
wrong** and are withdrawn. One tokenizer pass on the exact load-generator
150-byte body measures 17,364 ns/op, and the shipped code makes two, so 34.8us of
tokenizer work alone exceeds a claimed 16.9us total. The 1.16 ratio came from the
same probe and is therefore unverified rather than measured. What survives is the
code reading above, which is independent of the probe, and the corrected
non-streaming decomposition of 38.2us gross work against a 24us shared-path credit
for a net of +15us.

**Why two-sided, and why 0.60ms.** Sized as the inherent work plus the 336us
observed drift envelope, times roughly 1.7 headroom. Correcting the inherent term
from 19.5us to 15us moves that sum from 356us to 351us and leaves the ceiling
where it was. Two-sided because drift is two-sided while the work is one-sided,
and because a strongly negative shift is also the signature of an enforce cell
that was not actually enforcing. Stated plainly: this fires only on roughly a
40-fold regression and CANNOT catch a doubling. Closing that needs streaming
repetitions and a streaming drift canary, not a tighter number.

**Calibrated on limited data.** The evidence run is its first real test. One
calibration row came from a matrix whose overall verdict was INVALID: 13 of
14989 steady iterations dropped, confined to the closing drift-canary cell,
while all four cells feeding the shift readings recorded zero steady drops and
k6 exited 0. That located fact is what makes the row usable rather than a
judgement call.

### Band 5, the drift canary

**As amended, the GATES are:** the opening and closing direct-to-mock cells
bracket the whole matrix, and their absolute drift is at or below 0.25ms at P50
and at or below 1.50ms at P99. Both quantiles are gates here, unlike band 1 where
only the P50 gates. Exceeding either ceiling invalidates the run. The percentage
difference is still computed and printed beside both gates as context, and is no
longer a gate itself.

If the two canaries disagree, the machine drifted underneath the experiment and
no cell in between can be compared to any other.

**AMENDED 2026-09-16**, for the same family of reason as band 1.

**ORIGINAL pre-registered form:** the opening and closing canaries agreeing
within 15 PERCENT at both P50 and P99.

Why it moved. The defect was the percentage, not the tail. A percentage
tolerance on a sub-millisecond quantity produces an absolute tolerance tighter
than the signal the experiment measures. Fifteen percent of a 0.265ms canary is
0.040ms, while the deltas this matrix publishes are roughly 0.195ms for band 2
and 0.043 to 0.120ms for band 3. So the original form could reject a run whose
measured deltas were perfectly resolvable, which is incoherent in a validity
gate. Band 1 was amended for the same shape of error, a number borrowed from an
overhead SLO applied as an absolute bound on one cell's latency. Observed canary
drift across seven matrices on the reference host was 645.9, 144.0, 23.6, 57.6,
3.3, 141.2 and 28.8 percent, so the original band passed 1 run in 7 and would
have blocked any evidence run on this host.

Those seven readings are the P99 leg, which is why the tail stayed a GATE rather
than becoming an advisory. The percentages are large because the quantity is
small, not because the tail is uninformative: the two worst of them turn out to
be multi-millisecond absolute moves that any honest validity gate should reject.
Expressing the tail in milliseconds separates those from the merely noisy runs,
which a percentage cannot do at this magnitude.

The P50 ceiling is sized to the band 2 shift it protects. A drift comparable to
the smallest published delta is the point at which the comparison stops meaning
anything. It has more slack than that framing suggests, because the passthrough
and enforce cells run back to back as a pair by design, so slow drift across the
matrix largely cancels WITHIN each pair. What the canary is really there to
catch is GROSS drift, a thermal collapse or a background job that arrived mid
run and stayed, which biases whole pairs rather than cancelling inside them.

**Calibration of the 1.50ms P99 ceiling.** It was chosen after seeing which runs
failed, which is exactly the kind of choice that deserves scrutiny, so the
calibration data is published in full and a reader can judge whether the number
was picked honestly. Six historical matrices on the reference host:

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
noise bursts. So the tail gate demonstrably still detects gross drift, which was
the risk in dropping it, while no longer rejecting a run whose drift is smaller
than the signal being measured, which was the original defect. Under the
original 15 percent form only r11 passes.

The seventh matrix, the 141.2 percent reading, is absent from that table because
its absolutes were not recovered. It would fail the 1.50ms gate only if its
opening canary P99 exceeded 1.062ms, which is inside the 0.547 to 1.353ms range
the table shows, so its verdict under the amended band is genuinely unknown and
is recorded as indeterminate rather than counted either way.

**LIMITATION, stated because the two gates are not equally well evidenced.**
Only ONE historical P50 pair was recovered, 0.265 then 0.341 for an absolute
drift of 0.076ms, so unlike the P99 gate the 0.25ms P50 ceiling has never been
tested against a pathological run. The first evidence run is its first real
test. If it fails there, the numbers get examined rather than the threshold
moved.

## The host quiescence gate, ADDED 2026-09-16

This is not a band either. It runs before the first cell and again at both edges of
every cell after it, and it is the check that would have caught the invalidated
43-cell run before it spent 50 minutes measuring a busy machine.

**What it records.** `run.sh` samples system-wide **CPU idle percentage** into
`machine-state.txt` as `cpu_idle_pct`, twice per cell, while k6 is not running so
the reading is the AMBIENT host rather than the benchmark's own load. In
**evidence** mode a reading below the floor **refuses the run**. In quick mode it
warns and continues, because quick mode is a disposable local check.

**Why CPU idle and not load average.** `machine-state.txt` already carried
`loadavg`, and loadavg is **proven not to discriminate here**. The invalidated run
sat at 4.0 to 6.4 across its cells while the quiet re-measurements that corrected
it sat at 2.8 to 5.0. Those ranges OVERLAP, and the numbers they produced were 108us
apart, so no threshold on loadavg could have separated them. loadavg is still
recorded, deliberately, because a field that demonstrably does not discriminate is
worth keeping visible beside one that does.

**The sampler, and why not the obvious one.** The gate reads the second sample of
`top -l 2 -n 0 -s 2`, which is a true 2-second interval average. A single
`top -l 1 -n 0` sample does respond to load, which was worth confirming rather than
assuming, but it is noisy: five samples with the machine untouched read 60.96,
41.86, 59.75, 57.51 and 58.16. The interval form's spread over ten readings was
59.85 to 72.76 against 41.86 to 65.50 for the instantaneous one.

**The floor is 60 percent idle.** Calibrated against two regimes measured on the
reference host, 26 readings each:

```
regime                                 n    min     median   max
ambient, browser and agents resident   26   59.28   69.11   76.32
ambient plus three busy loops          26   41.87   51.56   58.77
```

Three busy loops is not an arbitrary load. It is the exact condition that
reproduced the invalidated run. The **paired** form of that measurement is the
load-bearing evidence: eight same-moment pairs, one reading with the loops absent
and one with them present seconds later, so ambient drift affects both arms
equally. Every pair moved the same way, by a median of **16.12 points of idle** and
never less than **11.94**.

Sixty sits above **every one** of the 26 contended readings, the highest being
58.77, and below only 2 of the 26 ambient readings, 59.28 and 59.85, which are that
distribution's low tail. It is deliberately not the midpoint of the two ranges: a
refused run costs one rerun, while a contended run that passes publishes a wrong
number as evidence.

**A single dip cannot abort a run.** An evidence run makes 106 of these checks, so
one isolated low reading in a hundred is expected. A reading below the floor is
re-sampled twice more and the **median of the three** decides. That cannot weaken
the gate, because the contended regime's single-sample MAXIMUM was 58.77 against a
floor of 60, so its median is below the floor too. Only a transient can be voted
out, which is the point.

**An unreadable reading refuses an evidence run at startup.** A gate whose sensor is
broken has to fail closed, or the next tool-output change turns the whole check into a
silent pass that still prints reassuring text. Startup is the right place for that to
be strict: a permanently broken sampler is broken there, so a mid-run unreadable
reading after a clean startup is a transient and is handled by the sustained-breach
rules below.

### AMENDED 2026-09-16, mid-run breaches must be SUSTAINED

**The strict form was unsatisfiable, and an unsatisfiable gate gets deleted.** The
second evidence attempt, at commit `377d97f`, **died at 51 minutes 54 seconds after 39
of 53 cells**, on `passthrough-nonstream-32768-r5` phase `before` reading **58.40
percent**. Its directory is `2026-09-16-377d97f-m3pro-macos-evidence-r1`, it has no
MANIFEST, and it is an aborted run rather than evidence.

**The gate was correct in the narrow sense.** The reading was genuinely sub-floor and
the median-of-three confirmation agreed. It was the only breach in that run's 79
readings, whose median was 73.97. **The policy was wrong, and the arithmetic says so:**

- An evidence run takes 53 cells times 2 readings, so **106 mid-run checks**.
- Ambient single samples on this host reach **59.28** against a floor of **60**.
- The one evidence-scale measurement of the CONFIRMED breach rate is that aborted run,
  **1 in 79**, which is 1.34 expected breaches per full run.

So at least one sub-floor reading per run is close to inevitable, and a gate refusing
on one can essentially never complete a run here however quiet the machine is.

**The distinction is SUSTAINED versus TRANSIENT.** The failure the gate exists to catch
was sustained: all five repetitions of the 150-byte enforcement pair inflated together,
roughly **108us of amplification held across an entire 50 minute run**, with every
integrity gate clean. **The floor is NOT lowered.** Contended single samples reach 58.77
against an ambient minimum of 59.28, so the single-sample regimes overlap almost exactly
and only the paired or median form separates them.

**The startup check is unchanged and still refuses on ONE reading.** The asymmetry is
deliberate: refusing at startup costs five seconds, refusing at cell 40 costs the 52
minutes already spent.

**Rule 1, two CONSECUTIVE confirmed sub-floor readings fail the run immediately.** The
before and after readings of one cell are adjacent in the ordered sequence, so a cell
contended throughout trips it at the cell that caused it. **The original failure trips
it trivially**: contention held across a whole run breaches every reading, so the first
adjacent pair arrives at reading 2 of 106 and the run stops in its first two minutes
rather than at minute 52. False-refusal cost at the observed 1.3 percent per-reading
rate, treating readings as independent, is 105 adjacent positions times 0.0127 squared,
so **1.7 percent of quiet runs**. Independence overstates that: across both runs on this
host that carry idle readings, **6 of 6 breaches landed on a `before` reading and 0 of
52 `after` readings breached**, and since the phases alternate an adjacent pair requires
an `after` breach.

**Rule 2, more than 10 percent of all mid-run readings breaching fails the run at the
END**, before the MANIFEST is written. This catches the host that is contended
throughout but intermittently reads above the floor. **That case is recorded, not
hypothetical.** It is the quick matrix at `31918d9` in this tree: **5 of 26 readings**,
worst reading 51.48 which sits inside the proven contended regime, at ordered positions
7, 15, 17, 19 and 21, so **no two were adjacent**. Rule 1 would have passed it.

Why 10 rather than 5 or 15, computed rather than eyeballed. The quiet rate is estimated
from ONE event, so its exact one-sided 95 percent Poisson upper bound is 4.744 events
per 79 readings, which is 6.37 per 106:

```
threshold   fires at    false refusal at 1.34 expected   at the 6.37 upper bound
5 percent   6 of 106                 0.26 percent                61.1 percent
10 percent  11 of 106            0.000019 percent                 6.0 percent
15 percent  16 of 106        0.00000000015 percent                 0.1 percent
```

Five percent could refuse a majority of quiet runs and nothing in the data rules it
out. Fifteen percent sits only 1.25 times below the one contended host on record, so it
has almost no margin against the case it exists to catch. Ten percent is **7.7 times
the observed quiet rate and roughly half the observed contended rate**.

**Everything else records the dip, warns loudly, and continues.** The reading carries
`cpu_idle_breach=yes` in `machine-state.txt` and the cell gets a line in
`contended-cells.txt`. An **absent** ledger means the directory predates the marking. A
**present and empty** one positively states that no reading breached.

**`check_bands.py` acts on the marking rather than only printing it.** Every band whose
value is a median across repetitions drops the contended repetitions before computing:
band 2, band 3 at 4096B, band 4, BAND3-STREAM, BAND3-SMALL and CONTROL-AA. A repetition
is dropped when EITHER arm of its pair was contended. The scope is one pairing, because
a repetition ordinal is a join key rather than a moment in time and the 150B and 4096B
cells of one repetition ran minutes apart. **This is what the five-repetition design is
for:** the published number is the median of five shifts, so it can afford to lose one.
Below **three clean repetitions** the gates fail with the cause named, because below
three a median stops being an order statistic and becomes a single reading wearing the
word median. In quick mode, with one repetition, excluding it leaves nothing, so the
band reads **UNEVALUABLE** and the run is invalid rather than silently passing.

**Single-instance cells are recorded and kept**, meaning the two drift canaries and the
two direct payload cells. All four are non-streaming. There is nothing to drop them in
favour of, and the bands reading them already tolerate a contended host: band 5 gates
canary drift directly at 0.25ms of P50 movement, and band 1 judges each of them against
**its own group's** ceiling, 1.0ms for the two 150-byte canaries and 1.5ms for the
32768-byte cell, against an observed **0.363 to 0.899ms on the most contended host in
this tree**. Both passed there, so no new failure path was added for them.

**Where that leaves the least margin, updated by the 2026-09-17 per-group amendment.**
`direct-payload-32768` has read as high as **0.899ms**, and that is now measured against
a 1.5ms ceiling calibrated on the 32768-byte group's own 13 readings rather than against
a 1.0ms ceiling borrowed from the 150-byte cells. Its margin goes from 1.11-fold to
**1.67-fold**, and it read 0.836ms on the fourth evidence attempt. No direct group now
carries less than 1.49-fold, which is the streaming group's deliberately accepted floor.

**Band 1 does read `contended-cells.txt` as of 2026-09-17, and only to ANNOTATE a
failure.** It still evaluates every direct cell whatever its host state, deliberately,
because a floor measured on a busy machine is still the floor that run's proxied numbers
sit on, and there is no code path by which a recorded breach turns a band 1 failure into
a pass. What the ledger buys is that a failing cell named in it says so in its own
failure message, so a reader can tell a genuine floor problem from a contended sample
without either being excused. A passing cell is not annotated, since the contention
block above band 1 already prints the whole ledger.

**What neither rule can do.** Both are built on the same floor and inherit its
resolution. The proven contended regime costs 16 points of idle, one busy loop costs
about a third of that and still passes. These rules make the gate survivable, not more
sensitive.

**Where the sampler is still weak, recorded rather than fixed.** The `before` reading
runs systematically LOWER than the `after` reading here, by 6.6 points in one recorded
run and 10.9 in another, and every breach ever recorded landed on a `before` reading.
That sample is taken right after a levee spawn, a config render and the previous cell's
TIME_WAIT drain, so its 2 second window can overlap the harness's own setup work rather
than pure ambient load. The bias is left in place because every calibration figure in
this gate was measured through the same sampler and moving the sample point would
orphan all of them. It is written down so nobody reads a low `before` reading as proof
of an outside job.

**What it cannot do.** The paired effect of the proven contended regime is 16 points
of idle, so the gate resolves THAT regime and cannot resolve a milder one. One busy
loop costs roughly a third as much and would pass. The gate is **necessary and not
sufficient**, exactly like the A/A control it ships beside.

**The honest limit on the whole story.** The invalidated run recorded **no idle
figure at all**, because the field did not exist yet. So contention of this size is
proven **SUFFICIENT** to produce that run's numbers and is **NOT proven** to be what
that run actually had. This floor is calibrated against the reproduction, not
against the failure. The ambient regime above is also not a quiet host: it carried a
browser, a video-conferencing app, resident endpoint-security agents and several
concurrent tool sessions, so it bounds how loaded a passing host may be rather than
describing a prepared one.

## The RATE gate, achieved versus demanded arrival rate, ADDED 2026-09-16

This is not a band. It runs BEFORE all five of them and it is the check that
makes them meaningful, so read it first.

Every band compares quantiles between cells, and that comparison assumes each
cell's distribution is SERVICE TIME. A cell demanding more than its capacity
reports QUEUE RESIDENCE instead, which is a property of the arrival rate and the
VU pool rather than of levee, and **no band can tell the two apart**, because a
queue raises the central tendency exactly the way real work does.

The gate recomputes each cell's achieved throughput from the committed rows,
compares it against the demanded rate recorded in the MANIFEST and in the cell
summary, and fails the run when any cell is more than **2 percent** short. It also
cross checks the committed row count against k6's own steady request count within
1 percent, so a summary that disagrees with the rows it is published beside cannot
pass silently. k6 enforces the same floor at cell time as
`http_reqs{scenario:steady}: count>=MIN_STEADY_REQUESTS`, so a short cell aborts
the run where it happened rather than at the end.

**It subsumes the drop count as a validity signal.** A dropped iteration means the
VU pool ran out of workers, which is one symptom of over-demand and not the
condition itself. Give the pool enough slots to hold the backlog and a saturated
cell drops nothing while still completing less work than was demanded, so the drop
count reads clean and the median is residence time. Throughput catches both
shapes.

**The drop threshold now carries a tolerance rather than `count==0`, amended
2026-09-17**, and the question of whether to keep it at all was asked at the same
time. It is kept. Drops subtract from completions one for one, so anything above
this gate's 2 percent already fails HERE and a drop tolerance at or above 2
percent would add nothing. At 1 percent it catches a shape this gate structurally
cannot see: a drop proves the VU pool had no free slot at a scheduled arrival, so
the pool was momentarily part of what the cell measured, and that costs hundredths
of a percent of throughput. Measured on the real script against an upstream that
blocks the whole pool once, at 500 rps over 5 seconds, 2500 demanded:

```
stall   steady drops   drop gate      achieved           rate gate   k6 exit
120ms             23   pass, 25 max   495.4 of 500 rps   pass             0
150ms             39   FAIL, 25 max   492.4 of 500 rps   pass            99
```

The second row is the answer. It fails on the drop count while this gate reads
clean at 1.5 percent short. The two gates fail in opposite blind spots and both
are kept. Full derivation in the integrity-tolerance section below.

**Why the margin is 2 percent.** The legitimate envelope is far smaller: a healthy
cell OVERSHOOTS slightly, 10001 rows against 10000 demanded at 500 rps over a 20
second window on this host, and a k6 probe at 20 rps over 2 seconds delivered
exactly 40 of 40. The only honest source of shortfall is work in flight when the
window closes, bounded by the pool size times one service time, which at the
matrix's worst cell is a couple of requests in 9000 or 0.02 percent. So 2 percent
is roughly 100 times the envelope, and still 25 times smaller than the 49 percent
shortfall the failed attempt recorded. Throughput on the enforced path is
retrograde past its knee, so an over-demanded cell does not miss by a few percent,
it collapses. Sizing the margin nearer the envelope would start rejecting runs for
single-iteration host stalls, which is the mistake bands 1 and 5 were both amended
to stop making.

**What prompted it.** An evidence attempt demanded 500 rps at 32768B enforce
against roughly 400 rps of measured capacity. It dropped 14622 steady iterations,
exited 99, and reported a 152.4ms median where the honest service time is 8.5ms.
Little's Law closes the gap exactly: 40 in flight over the 260 rps achieved is
154ms against 152.4ms observed.

The fix for a firing RATE gate is a **lower rate for that payload size** in
`rate_for_payload`, sized from measured capacity. It is never a wider margin, and
it is never a bigger VU pool: past the knee a bigger pool makes the number worse.

## The integrity tolerances, AMENDED 2026-09-17 from two absolute zeros

**This is a relaxation and it is recorded as one.** Two gates that demanded exactly zero
occurrences now permit a measured envelope. Everything below is the evidence that each
still catches the failure it was written for by orders of magnitude, and a reader who
concludes the bar was lowered to rescue a run should read the third-attempt record
above: no run was rescued, and the directory that died is still invalid.

### What they were, and why the form could not survive

```
dropped_iterations{scenario:steady}: count==0
http_req_failed{scenario:steady}:    rate==0
```

An evidence run is 53 cells whose steady windows demand **1,224,053 iterations**
between them, 1,428,053 counting warmup:

```
cells   demanded steady iterations each   subtotal   what they are
   33                            30000     990000   500 rps for 60s
   11                             9000      99000   150 rps for 60s, the 32KB cells
    9                            15000     135000   250 rps for 60s, streaming
```

A rule requiring zero occurrences across 1.2 million independent opportunities is a
lottery. The drop threshold duly lost it on one iteration in 30001, and the failure
threshold was one transient connection reset away from doing the same.

### The drop tolerance, 1 percent of demanded steady iterations with a floor of 25

Calibrated against every cell this repository has ever recorded, **151 cells carrying
1,438,074 steady requests**. Seven recorded nonzero steady drops:

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

**The two worst rows are the load-bearing ones, because neither can be saturation.** The
46 landed in a **direct** cell, which has no levee in its path at all and reported P50
0.339ms. The 49 landed in a 150-byte enforce cell running at roughly 11 percent of its
measured capacity, which reported P50 0.503ms with a P99 of 9.154ms, the signature of a
host stall rather than a queue. So **0.490 percent is the measured benign envelope on
this host** and the tolerance sits 2.0 times above it. Under the new rule **all 151
recorded cells pass**, including all seven of these.

The floor of 25 exists so a low-volume cell is not held to a tighter standard than a
high-volume one. Four separate cells dropped **exactly 13**, which is the observed size
of one host stall here, and 25 is just under two of those. It binds only below 2500
demanded iterations, which no cell in either mode reaches, so it is a guard for a future
low-rate cell rather than an active allowance.

**It still catches the failure it was written for by 48.7 times.** That failure is the
32768-byte capacity problem documented in the RATE gate section above: 14622 steady
drops of 30001, **48.7 percent**, against an allowance of 1 percent. Verified rather
than argued, at the pinned k6 v2.2.0: a deliberately saturated steady window dropped
2754 of 4000 and reported `ok false` with exit 99 against `count<=30`, then `ok true`
against `count<=99999`, so the tolerance is what decides and the mechanism fires.

### The failed-request tolerance, 0.05 percent with a floor of 5

Twenty times tighter in relative terms, deliberately, because the two counts mean
different things. A dropped iteration is the load generator giving up and says nothing
about levee. A failed request is levee answering **429**, erroring with a **5xx**, or
the loopback stack breaking, and every one of those is a fact about the system under
test.

**There is no observed benign envelope to size it against.** Zero failed requests have
ever been recorded here, **0 in 1,438,074 steady requests**. That absence is exactly why
the absolute form still had to go: zero events in 1,438,074 trials bounds the
per-request failure rate at **2.083e-6** at one-sided 95 percent confidence, which over
the 1,224,053 steady requests of an evidence run is **up to 2.55 expected failures**. So
the recorded data does not rule out that `rate==0` loses a 52 minute run more often than
not, and it would have died exactly the way the drop gate just did.

**Every failure shape worth catching is sustained rather than singular.** An exhausted
per-agent admission slot answers 429 for as long as the cell stays over the cap, so one
second of that at 500 rps is 500 failures against an allowance of 15. An exhausted
budget answers 429 for the entire remainder of the cell, tens of thousands. Verified at
the pinned k6 through the real `overhead.js` and its real threshold expressions:

```
forced steady failures   allowance   threshold                                 k6 exit
                     5           5   http_req_failed{scenario:steady}:rate<=0.05   0
                     6           5   http_req_failed{scenario:steady}:rate<=0.05  99
                   101           5   http_req_failed{scenario:steady}:rate<=0.05  99
```

### Warmup counts are untouched

They were already tolerated by design, because levee's first enforced request builds the
`o200k_base` encoder and blocks the pool for roughly 130ms, so the enforce cells
legitimately drop tens of iterations there. That cost still lands in warmup, is still
recorded per cell, and no tolerance was applied to it.

### Nothing is hidden by either tolerance

Every raw count is recorded whether it passed or not. `dropped-iterations.txt` and the
new `failed-requests.txt` carry the steady count, the warmup count, the allowance and
the demanded count per cell, each `summary.json` carries all of them plus the threshold
expression k6 actually evaluated, and `bands.txt` prints an `INTEGRITY TOTALS` line
naming the run's total steady drops and failed requests with their percentages of total
demand. A reader who prefers the old absolute rule can apply it to any committed
directory by hand.

`check_bands.py` re-derives both allowances from the same integer basis-point arithmetic
rather than trusting the recorded ones, and reports a disagreement if its figure and
k6's differ. That half of the change was mandatory rather than cosmetic: leaving an
absolute zero in the band checker would have re-failed at the END of the matrix exactly
the cell k6 had just correctly tolerated, wasting all 52 minutes instead of the 38 cells
the original gate wasted.

### The streaming repetition minimum, relaxed in the same pass

The contention exclusion needs three clean repetitions before a median is an order
statistic. The streaming matrix runs **three** repetitions, so that minimum permitted
**zero** contended streaming repetitions across the 12 host idle readings its pairing
spans. Pooling the two evidence attempts that carry idle readings, 1 confirmed breach in
155, that is a **7.5 percent** chance per run, and it fires inside `check_bands.py`
after the last cell, so it costs the entire 52 minutes rather than stopping where it
happened. Same defect, same cost.

BAND3-STREAM now needs **one** clean streaming repetition and prints a THIN MEDIAN line
whenever contention cost it any. That is defensible only because of what this particular
gate is: a two-sided **0.60ms** ceiling on the absolute size of a shift whose inherent
value is 15us, so it fires on roughly a 40-fold regression or on an enforce arm that was
not enforcing, and both are visible in one repetition. Its central value is already
advisory-only, and the band itself says the reading must not be published as the
streaming enforcement cost, so **no published number is computed from this median**.
Zero clean streaming repetitions is still UNEVALUABLE and still fails.

**The non-streaming minimum is unchanged at three**, and it does not have this problem:
five repetitions tolerate two contended ones, which puts the same arithmetic at roughly
0.03 percent per run.

**The better fix is still five streaming repetitions in the matrix**, which the band's
own limitation note has been asking for. It costs 6 more cells and roughly 7 minutes of
a 52 minute run and moves the pre-registered cell count from 53 to 59, so it is a matrix
decision rather than a gate decision and was not taken here.

## Invalidation rules

A run is invalid, and is not publishable, when any of these holds:

- Any band above fails. `bands.txt` ends in `VERDICT INVALID`.
- **The host quiescence gate refuses.** At STARTUP that means one reading below the
  CPU idle floor, or an unreadable one. MID-RUN it means a SUSTAINED breach, either
  two consecutive confirmed sub-floor readings or more than 10 percent of all mid-run
  readings, per the amendment above. In evidence mode the consecutive rule aborts the
  run where it happened and the percentage rule aborts it after the last cell, so in
  both cases there is no MANIFEST and no completed directory to judge. An isolated dip
  does NOT refuse: it is recorded in `contended-cells.txt`, marked
  `cpu_idle_breach=yes` in `machine-state.txt`, and its repetition is dropped from
  every median. A quick-mode run records every one of these conditions as a warning
  and continues, which is why quick directories can carry sub-floor readings.
- **Too few clean repetitions survive the contention exclusion.** Fewer than three
  clean repetitions of a pairing makes band 2, band 3 or band 4 fail with
  the cause named, and in quick mode a single contended repetition makes the band
  UNEVALUABLE. The cause is host contention rather than levee, and the remedy is a
  rerun on a quiet machine rather than a wider band. **BAND3-STREAM is the exception
  since 2026-09-17**: it needs one clean streaming repetition rather than three, for
  the aggregation reason documented with the integrity tolerances below, and it prints
  a THIN MEDIAN line whenever contention cost it a repetition.
- **The RATE gate fails**, meaning at least one cell served less than 98 percent
  of its demanded arrival rate, or its committed row count disagrees with k6's own
  steady request count. That cell's quantiles are queue residence and no band
  reading them means anything.
- A k6 integrity threshold fails. All three are scoped to the steady scenario and all
  three carry a number derived from that cell's own demand:
  `dropped_iterations{scenario:steady}: count<=MAX_STEADY_DROPPED_ITERATIONS`,
  `http_req_failed{scenario:steady}: rate<=MAX_STEADY_FAILED_RATE` and
  `http_reqs{scenario:steady}: count>=MIN_STEADY_REQUESTS`. k6 exits 99 on a
  threshold failure, and the exit code of every cell is recorded in
  `k6-exit-codes.txt`. The first two were `count==0` and `rate==0` until 2026-09-17,
  see the integrity-tolerance section below for why an absolute zero could not survive
  a 53-cell matrix.
- Any 429 appears in an enforce cell. That points at the per-agent admission
  concurrency cap rather than at latency, and it also perturbs levee-side state.
- The version assert fails, meaning the process answering `/health` is not the
  binary this run built. That is the orphaned-listener signature.
- The identity audit finds identifying content in any artifact.

Pre-named broken-run signatures, so a reader does not have to rediscover them:
P99 exploding as the rate rises points at HTTP/1.1 connection churn on the mock
leg, so check the TIME_WAIT readings in `machine-state.txt` and `somaxconn` in
the MANIFEST. Any 429 points at the concurrency cap. Both baselines drifting
together points at the load generator interfering with itself, and the answer is
a lower rate. A version-assert failure points at an orphaned levee from a
previous run.

**A small-payload enforcement delta several times its expected +15us, with every
other gate clean, points at host CPU contention rather than at levee.** That is the
exact signature that invalidated the first evidence run. Check `cpu_idle_pct` in
`machine-state.txt` first, then the `CONTROL-AA` line in `bands.txt` for what the
estimator invented at a known zero. Do not check loadavg for this: it does not
discriminate. The confirming detail in the invalidated case was a direct canary,
with no levee in its path at all, drifting a third of a millisecond inside its own
60-second window.

A median in the **tens or hundreds of milliseconds on an enforce cell** points at
that cell being over-demanded, not at a latency regression. Check
`achieved-rate.txt` first, then the busy-core count in the `bands.txt` COST table
against `cpu_cores` in the MANIFEST. The signature is a large median arriving
together with a large shortfall, and the arithmetic that confirms it is Little's
Law: the pool size divided by the achieved rate reproduces the median. The answer
is a lower rate for that payload size, from a fresh capacity measurement.

### The attempts.txt convention

`attempts.txt` is the run-attempt ledger of the surviving directory. It records
the total number of attempts behind the published run, and for each invalidated
attempt the reason and which band or threshold fired. When a run is invalidated
and rerun, the failed attempt's reason is copied into the surviving directory's
ledger.

Without it, rerun-until-clean makes the published run the survivor of an unknown
selection, which is the exact bias pre-registration exists to defeat.

## Identity denylist

No committed artifact may contain a hostname, a username, a machine serial
number, a home-directory path, or an environment dump. Every manifest field is
produced by a narrow pinned command for that reason, which is also why there is
no `uname -a` and no `system_profiler` output anywhere.

`run.sh` enforces this with a post-run audit that copies the results directory
aside, GUNZIPS every `.gz` first, and then greps the whole tree for the current
username from `id -un`, the short hostname, `/Users/`, and serial numbers. It
refuses to declare the run committable on a hit. The gunzip step matters: a
compressed per-request CSV would otherwise evade both this audit and the
no-AI-references check.

## Figure pairing

A figure commit always accompanies the results commit it was rendered from. The
figure filename embeds its source directory name, and the figure itself is
annotated with that name, so a figure circulating detached from the repository
stays self-describing.

"Regenerable" means identical statistics and marks, not byte-identical PNGs.
Matplotlib output is not byte-stable across machines and font sets.

## What makes a directory complete

- One filtered per-request CSV, gzipped, and one `summary.json` per cell. The
  CSV is filtered to steady-scenario rows by `run.sh`, which is committed code
  rather than a manual step, and the pre-filter and post-filter row counts go
  into `row-counts.txt` so the derivation is checkable.
- `bands.txt`, the mechanical band evaluation and the run verdict.
- `attempts.txt`, the attempt ledger.
- `microbench.txt`, the Go micro-benchmark output for the annotated enforcement
  components, measured on the same host during the same run. The enforcement
  figure parses this file and never carries hardcoded constants.
- `machine-state.txt`, system-wide CPU idle percentage, load average, thermal
  pressure, power source and TIME_WAIT count before and after every cell.
  `cpu_idle_pct` is the field the quiescence gate acts on and the only one of them
  proven to discriminate a contended host. `cpu_idle_floor_pct` beside it records the
  floor that reading was held to, and `cpu_idle_breach` records the per-reading
  verdict so a reader does not have to reapply the floor by hand.
- `contended-cells.txt`, one line per reading that breached the idle floor or could
  not be read, with the cell, the phase, the reason and the value. An ABSENT file means
  the directory predates the marking. A PRESENT and empty one positively states that no
  reading breached. `check_bands.py` drops the repetition of every cell named here out
  of every median it computes, and says so in `bands.txt`.
- `achieved-rate.txt`, the demanded and achieved arrival rate of every cell with
  the shortfall percentage, which is what the RATE gate reads and what says
  whether a latency number is service time or queue residence.
- `cpu-seconds.txt`, levee's consumed CPU seconds across each cell's steady
  window and the implied CPU milliseconds per request, from `ps -o cputime`
  deltas taken at the two window edges. Direct cells record `na` because no levee
  is in their path. This makes saturation readable off the artifact instead of
  inferred from a latency curve.
- `row-counts.txt` and `k6-exit-codes.txt`.
- `dropped-iterations.txt`, the steady and warmup dropped-iteration counts of every
  cell, **with the allowance and the demanded iteration count on the same line** since
  2026-09-17, so a reader sees how far inside or outside its budget each cell sat.
- `failed-requests.txt`, added 2026-09-17, the same shape for failed requests: the
  steady count, the warmup count, the steady rate, the allowance and the demand. Every
  cell writes a line whether it failed a request or not, so a present and all-zero file
  is a positive statement rather than an absence of evidence.
- `MANIFEST`, which records the demanded arrival rate for every payload size in
  the matrix as a separate field rather than one global rate, plus the shortfall
  tolerance, the single VU pool size, and the two integrity tolerances in basis points
  with their floors. Those last four are the RULE the whole matrix was judged under,
  recorded so two directories held to different standards cannot be compared by
  accident.

The MANIFEST is written LAST, after the bands pass and the audit is clean. A
directory without a MANIFEST is an aborted run, self-evidently incomplete, and
can never masquerade as evidence.

## Retention

Every evidence directory that any published number cites stays, plus the newest
one. A superseded directory whose numbers no longer appear in the README, in a
committed figure, or in any external write-up may be retired by deleting it in a
commit of its own.

Retiring an evidence directory is itself a `benchmarks/CHANGELOG.md` event. The
entry names the directory, what superseded it, and confirms nothing still cites
it. Deleting evidence quietly is indistinguishable from deleting inconvenient
evidence.

## Provenance under a squash merge

The MANIFEST records both `levee_git_sha` and `levee_git_tree`, the latter from
`git rev-parse HEAD^{tree}`. This project squash-merges, which rewrites the
commit, so the recorded SHA is generally NOT reachable from `main` after the
merge.

`levee_git_tree` is the field to verify against `main`. A squash merge preserves
the tree when the squashed content is identical to the branch head, which holds
when `main` did not move under the branch. If `main` did move, the squashed tree
differs and neither identifier will match, in which case the branch commit is
the only exact provenance.

**The recovery path chosen for this repository is the documented fetch**, rather
than regenerating the published directory from post-merge `main`:

```
git fetch origin pull/<N>/head
```

That makes the original commit, and therefore the exact tree the run was built
from, reachable locally again. A fresh evidence run from post-merge `main` is
the alternative for anyone who wants a SHA reachable from `main`, and it is a
new run with new numbers rather than a re-labelling of this one.
