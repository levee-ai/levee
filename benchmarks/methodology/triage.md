# A gate fired. What now

Grep this file for the line your run printed. Each entry gives what fired, what
to read next, and how to tell a real regression from a host artifact. The gates
themselves are in [bands.md](bands.md) and their thresholds are derived in
[calibration.md](calibration.md).

Everything below except the last two entries is printed by `check_bands.py` into
`bands.txt`. Read `bands.txt` top to bottom before acting, because the
`CONTENTION` block and the `CONTROL-AA` line qualify every shift printed
underneath them.

### `RATE FAIL`

A cell served less than 98 percent of its demanded arrival rate, or its committed
row count disagrees with k6's own steady request count. Its quantiles are queue
residence and no band reading them means anything. Read `achieved-rate.txt`, then
the busy-core count in the `bands.txt` COST table against `cpu_cores` in the
MANIFEST. The confirming arithmetic is Little's Law: the pool size divided by the
achieved rate reproduces the median. A large median arriving together with a large
shortfall is over-demand, not a latency regression. The fix is a lower rate for
that payload size in `rate_for_payload`, sized from a fresh capacity measurement.
Never a wider margin and never a bigger VU pool, because past the knee a bigger
pool makes the number worse.

### `INTEGRITY FAIL`

A cell exceeded its own dropped-iteration or failed-request allowance, or a k6
threshold reported anything other than pass. The line names the raw count and the
allowance. A **failed request** is a fact about levee, so treat any nonzero count
as real: it means a 429, a 5xx, or the loopback stack breaking. A **drop** is the
load generator giving up, so check the RATE line first. If the rate is clean and
the drops are not, the VU pool momentarily had no free slot at a scheduled
arrival, which is a host stall. Four separate cells in this tree dropped exactly
13 that way. Sustained drops climbing with payload size are capacity, and
`cpu-seconds.txt` will show it.

### `BAND1 FAIL direct P50 must be below its per-group ceiling`

A cell with no proxy in its path read a central tendency above its group's
ceiling, so the box or the mock is the bottleneck. The failure message names each
offending cell, its reading and its ceiling. Check whether the cell is annotated
`MEASURED DURING A RECORDED CONTENTION BREACH`, which band 1 prints for
information and never as an excuse. Then read `cpu_idle_pct` for that cell in
`machine-state.txt`. A genuine bottleneck raises the central tendency uniformly
across every direct group. A host artifact raises one or two cells and usually
lands on a `before` reading. The four groups trip at 3.02 to 3.47 times their own
medians, so a failure here is a threefold slowdown and not a subtle regression.

### `BAND1 FAIL no calibrated band 1 threshold exists for`

The matrix gained a direct cell at a response mode or payload size that has never
been calibrated. This is a matrix change, and it has to arrive with its own
calibration added to `BAND1_DIRECT_GROUP_THRESHOLDS`. Borrowing a neighbouring
group's ceiling is the category error this band was amended twice to remove.

### `BAND1 ADVISORY direct P99 above its per-group advisory threshold`

Not a failure, and it never blocks publication. It is host tail noise on a cell
with no levee in it. Read `cpu_idle_pct` in `machine-state.txt`. Do not read
loadavg for this, because it was proven not to discriminate.

### `BAND2 FAIL`

Below the 0.05ms floor the cell did not traverse the proxy at all, so check the
rendered config and the port the load generator was pointed at. Above the 0.6ms
ceiling something beyond the extra loopback hop is being paid, so compare the
passthrough arm's absolute P50 against the direct baseline printed on the same
line and check for a levee-side change on the shared request path.

### `BAND3 FAIL`

The primary enforcement gate. The message says which end was crossed.

Below 0.15ms the enforce arm was probably not enforcing. Check that the rendered
config and the agent header reached it, and compare against the `CONTROL-AA` line:
a non-enforcing arm reads what that control reads, single-digit microseconds
rather than hundreds.

Above 0.95ms the enforcement path got materially more expensive. Count the
tokenizer passes first, since one extra pass at this payload is roughly 340us on
its own. Note that this ceiling is stale and now weaker than it reads, so a
reintroduced second pass would pass it. The real guards for that specific
regression are the `tokenEstimator` interface in `internal/proxy/proxy.go`, which
omits `Estimate` so a whole-body pass is a compile error, and
`TestEnforcedRequestTokenizesBodyOnce`.

"The spread exceeds the signal" is neither of those. The run cannot resolve the
enforcement cost, and the answer is more repetitions.

### `BAND3-SMALL ADVISORY`

Never a failure. The 150-byte reading is outside the window the component
decomposition says inherent enforcement work can produce, which is 38.2us of gross
work against a 24us shared-path credit for a net near +15us. A reading well above
that window is host contention: three background busy loops reproduce exactly
this, moving the same measurement to +93 and +109us while every integrity gate
still reads clean. Read `cpu_idle_pct` and the `CONTROL-AA` line. This reading must
not be published as the enforcement cost at this payload.

A `BAND3-SMALL NOTE` about a negative reading is expected rather than alarming,
for the reason in [bands.md](bands.md#band-3-small-the-150-byte-reading).

### `BAND4 FAIL`

The tail shift at 4096B exceeded ten times the median P50 shift, so one cell caught
a transient that every median gate missed. Find it in the per-repetition list on
the same line, then read that cell's `cpu_idle_pct` and its P99.9 in the inventory
table. Band 4 is a ratio against band 3, so if band 3 also failed, fix that first.

### `BAND3-STREAM FAIL`

The streaming shift exceeded 0.60ms in absolute size. Positive means a real
regression in the estimate, admit and reconcile path. Negative means the enforce
cell probably was not enforcing, so check that the rendered config and the agent
header reached it. This gate fires on roughly a 40-fold regression, so a failure
is not marginal.

`BAND3-STREAM THIN MEDIAN` means contention cost the band repetitions. The gate
still holds. The printed median is weaker and must not be quoted.

### `BAND5 FAIL`

The opening and closing canaries disagree by more than 0.25ms at P50 or 1.50ms at
P99, so the machine moved underneath the experiment by as much as the effect being
measured and no cell can be compared to any other. Look for a thermal event or a
background job that arrived mid run and stayed, in the per-cell `cpu_idle_pct`
readings. An evidence run refuses to continue below the idle floor, so a run that
reached this band without that refusal drifted for some reason other than
sustained CPU contention.

### `VERDICT INVALID could not load the run`

The directory is incomplete or malformed rather than measuring badly. A cell that
fails a k6 threshold never gets a filtered CSV written, so a run that died at a
cell cannot load. Check for a MANIFEST: a directory without one is an aborted run,
self-evidently incomplete, and can never masquerade as evidence.

### `host CPU idle is N percent at startup, below the 60 percent floor`

Printed by `run.sh`, not by the band checker, and no results directory is created.
Quit the browser, the video-conferencing app and any background build, wait for the
endpoint-security agents and the file indexer to settle, and start again. An
unreadable sensor refuses an evidence run at startup on the same terms, because a
gate whose sensor is broken has to fail closed.

### `host quiescence breached on N CONSECUTIVE readings` or `of M mid-run readings`

Printed by `run.sh`. The machine stayed busy rather than dipping once. The
consecutive form aborts at the cell that caused it, the percentage form aborts
after the last cell, and neither writes a MANIFEST. The offending cells are named
in `contended-cells.txt`. A single isolated dip does neither of these and is
recorded instead, with its repetition dropped from every median.
