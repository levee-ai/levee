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

Evidence mode refuses to start on a dirty tree, on battery power, or with Low
Power Mode on.

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

**Two of them were amended during implementation, after runs failed them.** That
is exactly the move a skeptical reader should scrutinize, so both amendments are
recorded below in full, with the evidence and the calibration that motivated
them. Neither was widened to rescue a specific run.

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

### Band 3, enforcement over passthrough at the small payload

At the 150-byte payload, the median repetition-matched enforce minus passthrough
P50 delta falls within 0 to 100us, and the spread across repetitions is smaller
than the delta itself. If the spread exceeds the signal, the run cannot resolve
the enforcement cost and the answer is more repetitions, not a wider band.

The window is open at the bottom because the signal is tens of microseconds and
can sit inside the noise of a single repetition. With one repetition the spread
check is arithmetically vacuous, and `bands.txt` says so out loud rather than
letting a vacuous pass read as a real one.

The delta legitimately INCLUDES two extra structured log lines per request, an
enforced request writing three where a passthrough request writes one. Measured
on the reference host at the shipped destination, that is roughly 2.2us of a
measured shift near 25us, so logging is a minority component. The enforcement
figure annotates it separately anyway, so the published number is never mistaken
for pure enforcement work. There is no log-level knob in levee, so the two cells
cannot be equalized by configuration.

At 4KB and 32KB the delta is governed by the measured tokenizer curve rather
than by a fixed window.

### Band 4, the P99 companion to band 3

The median enforce minus passthrough P99 shift does not exceed ten times the
median P50 shift. A tail shift that far above the median shift means one cell
caught a transient even though every median gate passed.

When the median P50 shift is zero or negative a ratio against it is undefined,
so the band falls back to ten times band 3's own 0.1ms ceiling. That is the
widest the ratio form could ever have allowed while band 3 passed, so the
fallback can never be looser than the rule it stands in for.

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

## Invalidation rules

A run is invalid, and is not publishable, when any of these holds:

- Any band above fails. `bands.txt` ends in `VERDICT INVALID`.
- A k6 integrity threshold fails. Both are scoped to the steady scenario:
  `dropped_iterations{scenario:steady}: count==0` and
  `http_req_failed{scenario:steady}: rate==0`. k6 exits 99 on a threshold
  failure, and the exit code of every cell is recorded in `k6-exit-codes.txt`.
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
- `machine-state.txt`, load average, thermal pressure, power source and
  TIME_WAIT count before and after every cell.
- `row-counts.txt`, `k6-exit-codes.txt` and `dropped-iterations.txt`.
- `MANIFEST`.

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
