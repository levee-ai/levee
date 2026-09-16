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
CONTINUE if the host drops below that floor part way through, so a machine that
becomes busy mid-matrix stops the run where it happened rather than producing
another 40 cells of unusable numbers.

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

**Three of them were amended during implementation.** That is exactly the move a
skeptical reader should scrutinize, so every amendment is recorded below in full,
with the evidence and the calibration that motivated it. None was widened to
rescue a specific run.

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
publication, and its 2.5ms threshold exists only so host noise is visible rather
than invisible. Band 5's P99 IS A GATE: exceed 1.50ms of canary drift and the run
is invalid. A careless reading conflates them and concludes either that levee's
tail is gated when it is not, or that canary drift is merely noted when it is
disqualifying.

### Band 1, direct-to-mock floor

**As amended, the GATE is:** the P50 of every direct-to-mock cell is below 1.0ms.
That is the whole gate. The P99 of every direct cell is also recorded in
`bands.txt`, and an ADVISORY line fires at or above 2.5ms, but the tail is not a
verdict on the run and a run is never rejected for it. The advisory threshold
exists so that host noise is stated out loud instead of passing unremarked, not so
that it can block publication.

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

**An unreadable reading refuses an evidence run.** A gate whose sensor is broken
has to fail closed, or the next tool-output change turns the whole check into a
silent pass that still prints reassuring text.

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

**The drop threshold is unchanged at `count==0`, deliberately.** It did its job
correctly on the attempt that prompted this gate, by refusing to publish a
queue-time number as a latency number. The new gate is added beside it, not in
place of it.

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

## Invalidation rules

A run is invalid, and is not publishable, when any of these holds:

- Any band above fails. `bands.txt` ends in `VERDICT INVALID`.
- **The host quiescence gate refuses**, meaning the host read below the CPU idle
  floor before the first cell or at either edge of any cell after it, or its idle
  percentage could not be read at all. In evidence mode the run aborts where it
  happened, so there is no completed directory to judge. A quick-mode run records
  the same condition as a warning and continues, which is why quick directories can
  carry sub-floor readings in `machine-state.txt`.
- **The RATE gate fails**, meaning at least one cell served less than 98 percent
  of its demanded arrival rate, or its committed row count disagrees with k6's own
  steady request count. That cell's quantiles are queue residence and no band
  reading them means anything.
- A k6 integrity threshold fails. All three are scoped to the steady scenario:
  `dropped_iterations{scenario:steady}: count==0`,
  `http_req_failed{scenario:steady}: rate==0` and
  `http_reqs{scenario:steady}: count>=MIN_STEADY_REQUESTS`. k6 exits 99 on a
  threshold failure, and the exit code of every cell is recorded in
  `k6-exit-codes.txt`.
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
  floor that reading was held to.
- `achieved-rate.txt`, the demanded and achieved arrival rate of every cell with
  the shortfall percentage, which is what the RATE gate reads and what says
  whether a latency number is service time or queue residence.
- `cpu-seconds.txt`, levee's consumed CPU seconds across each cell's steady
  window and the implied CPU milliseconds per request, from `ps -o cputime`
  deltas taken at the two window edges. Direct cells record `na` because no levee
  is in their path. This makes saturation readable off the artifact instead of
  inferred from a latency curve.
- `row-counts.txt`, `k6-exit-codes.txt` and `dropped-iterations.txt`.
- `MANIFEST`, which records the demanded arrival rate for every payload size in
  the matrix as a separate field rather than one global rate, plus the shortfall
  tolerance and the single VU pool size.

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
