# Committed benchmark evidence

One directory per benchmark run, written by `benchmarks/harness/run.sh`. Every number
in a figure, and every latency claim anywhere in this repository, traces to one of
these directories.

Start here to check a published number. The gates a run was judged under are in
[../methodology/bands.md](../methodology/bands.md), where each threshold came from is
in [../methodology/calibration.md](../methodology/calibration.md), what to do about a
gate that fired is in [../methodology/triage.md](../methodology/triage.md), and the
run instructions and headline numbers are in [../README.md](../README.md).

## Directory naming

```
<YYYY-MM-DD>-<shortsha>[-dirty]-<hwtag>-<mode>-r<n>
```

- `YYYY-MM-DD` is UTC on the day the run started.
- `shortsha` is `git rev-parse --short HEAD` at run start. The binary under test is
  built from it with `-ldflags -X main.version=bench-<shortsha>`, and every proxied
  cell asserts the running process reports exactly that stamp on its admin `/health`
  endpoint before k6 sends a request.
- `-dirty` appears when `git status --porcelain` was non-empty. A dirty run cannot be
  rebuilt from any commit, so it can never be evidence.
- `hwtag` is an honest hardware label, `m3pro-macos` for the reference host.
  Reproducibility here comes from the methodology and the manifest, not from quiet
  hardware.
- `mode` is `quick` or `evidence`.
- `r<n>` is a same-day run ordinal. `run.sh` counts up until it finds a free name and
  refuses to write into a directory that already exists, so a second run at the same
  SHA cannot clobber a committed one.

Only `evidence` directories are committed. Quick-mode runs are disposable local checks
with one repetition, which cannot resolve the enforcement signal at all, so the
`.gitignore` in this directory excludes them along with any dirty-tree run. That
exclusion is mechanical rather than a convention someone has to remember.

## The first completed evidence run was INVALIDATED

Recorded here rather than left in a git log, because a reader who finds the number
without the correction would publish it.

The first 43-cell evidence run, at commit `31918d9` on the reference host, was
INVALIDATED by band 3. It measured a median repetition-matched enforce minus
passthrough P50 delta of +123us at 150 bytes, per repetition +107, +175, +126, +114
and +123us, against a pre-registered window of 0 to 100us. Everything else about that
run was clean: 43 cells, zero steady dropped iterations, zero failed requests, every
k6 threshold green, every cell at 100.0 percent of its demanded arrival rate, and
0.076ms of canary drift at P50. A run can pass every integrity gate in this harness
and still be measuring the wrong thing.

The investigation attributed it to host CPU contention and not to levee. Six pairs
measured in both orders on a quiet host read +13, +14, +16, +16, +19 and +14us. Three
background busy loops move the same measurement to +93 and +109us and move both arms'
absolute P50 onto the invalidated artifact's own values, while still achieving 500.0
rps with zero steady drops. The artifact's own opening direct canary, with no levee in
the path, climbs from 0.291 to 0.392ms across the six slices of its own 60-second
window.

**THE CORRECTED FIGURE. Levee's enforcement cost at 150 bytes is +15us net on a
quiet host, not 123us.** It is 38.2us of gross enforce-only work against a 24us
credit for the shared path running faster in the enforce arm. Do not quote the 123us.

**THE HONEST LIMIT.** Contention of that size is proven SUFFICIENT to produce those
numbers. It is NOT proven to be what that run had. loadavg was effectively identical
in both regimes, 4.0 to 6.4 during the invalidated run against 2.8 to 5.0 during the
quiet re-measurements, and no better proxy was recorded at the time, so the actual
contention level during those 50 minutes is unrecoverable. The host quiescence gate
exists to close that gap going forward. It cannot close it backwards.

## Five attempts produced one publishable run

Four evidence attempts failed before one completed cleanly. None of their directories
is committed, because two were aborted runs with no MANIFEST and the other two were
superseded, so this section is the only record of them.

```
attempt   commit    died at              gate that fired                  occurrence that fired it
1         31918d9   completed, invalid   band 3, the 150B window          sustained host contention, correctly
2         377d97f   51m54s, cell 39/53   one sub-floor CPU idle reading   1 of 79 readings, at 58.40 pct
3         5b2128c   cell 38/53           dropped_iterations count==0      1 of 1.2 million iterations
4         4d3f224   completed, invalid   band 1, one ceiling for 2 modes  streaming P50 1.341ms against 1.0ms
```

Attempt 1 must not be lumped in with the rest: that band was right and the run was
genuinely invalid. Attempts 2 and 3 are the same mistake twice, a gate demanding
exactly zero occurrences of a rare event across a very large number of independent
opportunities. Attempt 4 is a third shape, a threshold applied to a quantity it was
never calibrated against: a 1.0ms ceiling sat at 3.41 times the non-streaming central
value and only 1.54 times the streaming one, so it was roughly twice as strict on the
streaming arm by accident of how it was derived. Both lessons are standing
requirements in
[../methodology/calibration.md](../methodology/calibration.md#the-calibration-provenance-requirement).

This was not a fifth roll of the same dice. Each failure changed a rule, and every one
of those changes is load bearing for the fifth run finishing:

| attempt | what it cost                 | what it changed |
|---------|------------------------------|-----------------|
| 1       | 43 cells, roughly 50 minutes | the host quiescence gate, the A/A control pair, and the primary enforcement gate relocated to 4096B |
| 2       | 39 of 53 cells, 51m54s       | the mid-run quiescence refusal now needs SUSTAINED contention, with the floor unmoved |
| 3       | 38 of 53 cells               | the two integrity absolute zeros became tolerances derived from each cell's own demand |
| 4       | all 53 cells, then invalid   | every band 1 ceiling derived from its own cell group, replacing two special cases |

### The published run

`2026-09-17-2a569f4-m3pro-macos-evidence-r1` ran all 53 cells in 70 minutes 16
seconds, from 15:32:19Z to 16:42:35Z, and its `bands.txt` closes with **VERDICT VALID,
every pre-registered band passed**. It is the first run in this repository whose
numbers are publishable, and [../README.md](../README.md) publishes them.

What it recorded about its own conditions, which is the part the four failures bought:

```
RATE          all 53 cells at 100.0 percent of demanded arrival rate
INTEGRITY     0 steady dropped iterations and 0 failed steady requests of
              1,224,000 demanded, every cell inside its own allowance, against
              109 warmup drops across 11 cells which are tolerated by design
CONTENTION    0 of 106 host CPU idle readings below the 60 percent floor, so
              contended-cells.txt is PRESENT and EMPTY and no repetition was
              excluded from any median
BAND1         direct P50 inside all four per-group ceilings, tightest margin
              direct-payload-4096 at 0.602ms against 1.0ms, 1.66-fold
BAND2         +0.281ms passthrough minus direct P50
BAND3         +0.605ms at 4096B, spread 0.017ms across five repetitions
BAND3-SMALL   +0.055ms at 150B, spread 0.057ms, recorded and not gated
BAND4         +0.383ms P99 shift at 4096B against an allowance of 6.050ms
BAND3-STREAM  +0.031ms streaming, inside a two-sided 0.60ms ceiling on its size
BAND5         0.020ms canary drift at P50 and 0.103ms at P99
A/A CONTROL   -0.011ms with a 0.047ms spread, a true zero read by the estimator
```

It exercised every one of those amendments at least once. It took 106 quiescence
readings under the amended mid-run rule and needed none excluded, tolerated 109 warmup
drops while its steady windows stayed at zero, judged four direct cell groups against
four separately derived ceilings, and read its 150-byte enforcement signal at +55us
with a 57us spread, which is the measurement that relocating the primary gate to 4096B
was made in anticipation of.

Its `attempts.txt` records one attempt and not five, because `run.sh` writes that
ledger per results directory and the four prior attempts lived in four directories of
their own. The cross-attempt history is this section. That is a gap between the
artifact and the ledger convention below, and it is noted rather than repaired by
hand: a committed evidence artifact is not edited after the fact.

### The fourth attempt's other readings, recorded and NOT published

`2026-09-17-4d3f224-m3pro-macos-evidence-r1` completed, so the bands that did pass
carry information even though the run is not publishable. These must not be quoted as
levee's measured overhead, and no figure is rendered from that directory:

```
band            reading
BAND2           +0.287ms passthrough minus direct P50
BAND3 primary   +0.621ms at 4096B, spread 0.029ms across five repetitions
BAND4           +0.454ms P99 shift at 4096B
BAND3-STREAM    +0.115ms streaming enforce minus passthrough P50
BAND5           0.074ms canary drift at P50
A/A control     -0.008ms, a true zero read by the same estimator
```

The A/A control at -0.008ms beside the 0.029ms band 3 spread is the informative pair,
because together they say that run's estimator was resolving its 4096B signal cleanly.
That is why the streaming floor failure is a band defect and not a bad run.

## Amendments are never retroactive

A band amended after seeing a run, then applied backwards to that run, is not a
pre-registered band at all. A run's verdict is the verdict it was published under, and
an amended checker passing an old directory changes nothing about it.

`2026-09-17-4d3f224-m3pro-macos-evidence-r1` passes both later forms of band 1 and
**stays INVALID**. Its own `bands.txt` keeps the `BAND1 FAIL` line and the `VERDICT
INVALID` it was judged under, unmodified, and that file is the historical record. The
first evidence run's directory stays invalidated on the same terms: running today's
checker over it prints `VERDICT VALID`, which is a property of the relocated band 3
gate and not a re-blessing of the old numbers, and its +123us reading remains
contaminated.

## Invalidation rules

A run is invalid, and is not publishable, when any of these holds. What to do about
each is in [../methodology/triage.md](../methodology/triage.md).

- Any pre-registered band fails. `bands.txt` ends in `VERDICT INVALID`.
- The host quiescence gate refuses. At startup that means one reading below the CPU
  idle floor, or an unreadable one. Mid-run it means two consecutive confirmed
  sub-floor readings, which aborts where it happened, or more than 10 percent of all
  mid-run readings, which aborts after the last cell. Either way there is no MANIFEST.
  An isolated dip does not refuse. Quick mode records every one of these conditions as
  a warning and continues, which is why quick directories can carry sub-floor readings.
- Too few clean repetitions survive the contention exclusion. Fewer than three makes
  band 2, band 3 or band 4 fail with the cause named, and in quick mode a single
  contended repetition makes the band UNEVALUABLE. The remedy is a rerun on a quiet
  machine, never a wider band. BAND3-STREAM needs one clean streaming repetition.
- The RATE gate fails, meaning a cell served less than 98 percent of its demanded
  arrival rate or its committed row count disagrees with k6's own steady request
  count. That cell's quantiles are queue residence.
- A k6 integrity threshold fails. All three are scoped to the steady scenario and
  carry a number derived from that cell's own demand. k6 exits 99, and every cell's
  exit code is recorded in `k6-exit-codes.txt`.
- Any 429 appears in an enforce cell. That points at the per-agent admission
  concurrency cap rather than at latency, and it perturbs levee-side state.
- The version assert fails, meaning the process answering `/health` is not the binary
  this run built. That is the orphaned-listener signature.
- The identity audit finds identifying content in any artifact.

Pre-named broken-run signatures, so a reader does not have to rediscover them. P99
exploding as the rate rises points at HTTP/1.1 connection churn on the mock leg, so
check the TIME_WAIT readings in `machine-state.txt` and `somaxconn` in the MANIFEST.
Any 429 points at the concurrency cap. Both baselines drifting together points at the
load generator interfering with itself, and the answer is a lower rate. A
version-assert failure points at an orphaned levee from a previous run.

`attempts.txt` is the run-attempt ledger of the surviving directory. It records the
total number of attempts behind the published run, and for each invalidated attempt
the reason and which band or threshold fired. Without it, rerun-until-clean makes the
published run the survivor of an unknown selection, which is the exact bias
pre-registration exists to defeat.

## Identity denylist

No committed artifact may contain a hostname, a username, a machine serial number, a
home-directory path, or an environment dump. Every manifest field is produced by a
narrow pinned command for that reason, which is also why there is no `uname -a` and no
`system_profiler` output anywhere.

`run.sh` enforces this with a post-run audit that copies the results directory aside,
GUNZIPS every `.gz` first, and then greps the whole tree for the current username from
`id -un`, the short hostname, `/Users/`, and serial numbers. It refuses to declare the
run committable on a hit. The gunzip step matters: a compressed per-request CSV would
otherwise evade both this audit and the no-AI-references check.

## What makes a directory complete

- One filtered per-request CSV, gzipped, and one `summary.json` per cell. The CSV is
  filtered to steady-scenario rows by `run.sh`, which is committed code rather than a
  manual step, and the pre-filter and post-filter row counts go into `row-counts.txt`.
- `bands.txt`, the mechanical band evaluation and the run verdict. Every committed
  copy predates 2026-09-19, when the checker began printing two pointer lines after
  the verdict naming `methodology/bands.md`, `methodology/calibration.md` and
  `methodology/triage.md`. So a regenerated `bands.txt` carries two lines a committed
  one does not, and the committed copies are left as written because each records one
  specific run. Every number and every verdict is unchanged.
- `attempts.txt`, the attempt ledger.
- `microbench.txt`, the Go micro-benchmark output for the annotated enforcement
  components, measured on the same host during the same run. The enforcement figure
  parses this file and never carries hardcoded constants.
- `machine-state.txt`, system-wide CPU idle percentage, load average, thermal
  pressure, power source and TIME_WAIT count before and after every cell.
  `cpu_idle_pct` is the field the quiescence gate acts on and the only one proven to
  discriminate a contended host. `cpu_idle_floor_pct` records the floor that reading
  was held to, and `cpu_idle_breach` the per-reading verdict.
- `contended-cells.txt`, one line per reading that breached the idle floor or could
  not be read, with the cell, the phase, the reason and the value. An ABSENT file means
  the directory predates the marking. A PRESENT and empty one positively states that no
  reading breached.
- `achieved-rate.txt`, the demanded and achieved arrival rate of every cell with the
  shortfall percentage, which is what says whether a latency number is service time or
  queue residence.
- `cpu-seconds.txt`, levee's consumed CPU seconds across each cell's steady window and
  the implied CPU milliseconds per request, from `ps -o cputime` deltas at the two
  window edges. Direct cells record `na` because no levee is in their path.
- `row-counts.txt` and `k6-exit-codes.txt`.
- `dropped-iterations.txt`, the steady and warmup dropped-iteration counts of every
  cell, with the allowance and the demanded iteration count on the same line.
- `failed-requests.txt`, the same shape for failed requests. Every cell writes a line
  whether it failed a request or not, so a present and all-zero file is a positive
  statement rather than an absence of evidence.
- `MANIFEST`, which records the demanded arrival rate for every payload size as a
  separate field rather than one global rate, plus the shortfall tolerance, the single
  VU pool size, and the two integrity tolerances in basis points with their floors.
  Those last four are the RULE the whole matrix was judged under, recorded so two
  directories held to different standards cannot be compared by accident.

The MANIFEST is written LAST, after the bands pass and the audit is clean. A directory
without a MANIFEST is an aborted run, self-evidently incomplete, and can never
masquerade as evidence.

## Figure pairing and retention

A figure reaches the repository in the SAME commit as the results directory it was
rendered from, or in the one immediately after it. The published run split that way:
the directory landed in `test(bench): record overhead evidence run on m3pro-macos` and
its two figures in the commit that follows. Adjacent is enough because the pairing is
carried by the artifact and not by the commit boundary. The figure filename embeds its
source directory name and the figure footer carries that name plus the levee tree
hash, so a figure circulating detached from the repository stays self-describing. What
is NOT acceptable is a committed figure whose source directory is absent from the
repository, in any commit.

Every evidence directory that any published number cites stays, plus the newest one. A
superseded directory whose numbers no longer appear in the README, in a committed
figure, or in any external write-up may be retired by deleting it in a commit of its
own. Retiring one is itself a [../CHANGELOG.md](../CHANGELOG.md) event, and the entry
names the directory, what superseded it, and confirms nothing still cites it. Deleting
evidence quietly is indistinguishable from deleting inconvenient evidence.

## Provenance under a squash merge

The MANIFEST records both `levee_git_sha` and `levee_git_tree`, the latter from `git
rev-parse HEAD^{tree}`. This project squash-merges, which rewrites the commit, so the
recorded SHA is generally NOT reachable from `main` after the merge.

`levee_git_tree` is the field to verify against `main`. A squash merge preserves the
tree when the squashed content is identical to the branch head, which holds when
`main` did not move under the branch. If `main` did move, the squashed tree differs and
neither identifier will match, in which case the branch commit is the only exact
provenance. The recovery path chosen for this repository is the documented fetch:

```
git fetch origin pull/<N>/head
```

That makes the original commit, and therefore the exact tree the run was built from,
reachable locally again. A fresh evidence run from post-merge `main` is the alternative
for anyone who wants a SHA reachable from `main`, and it is a new run with new numbers
rather than a re-labelling of this one.
