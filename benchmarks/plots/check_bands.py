# /// script
# requires-python = ">=3.11"
# dependencies = []
# ///
"""Mechanically evaluate the pre-registered sanity bands for one benchmark run.

The bands are gates, not prose. A human reading figures and judging whether the
numbers look reasonable is precisely the failure mode pre-registration exists to
prevent, so every band here is arithmetic over the committed artifacts and every
violation exits non-zero.

Run it the way the harness does, against any committed results directory:

    uv run --script benchmarks/plots/check_bands.py <results-directory>

A bare `python3 check_bands.py` is not the supported path. The PEP 723 header above
pins the interpreter, and uv is what reads it. Exit codes are 0 for VERDICT VALID, 1
for a violated band or a directory that could not be loaded, and 2 for bad arguments.

Every threshold's derivation, calibration table and frozen data cut lives in
benchmarks/methodology/calibration.md, the current rule of each gate in
benchmarks/methodology/bands.md, what to do when one fails in
benchmarks/methodology/triage.md, and the dated amendment history in
benchmarks/CHANGELOG.md. Comments here state only what a number bounds and what a
breach means.

Two sources are read per cell, and the split is deliberate:

  <cell>.duration.csv.gz  the committed filtered per-request rows, steady
                          scenario only. Every percentile in a band comes from
                          here, because these are the same rows the figures
                          publish. The k6 end-of-run summary aggregates the
                          WHOLE invocation including the warmup scenario, so
                          gating on those would gate numbers nobody publishes.
  <cell>.summary.json     cell metadata and the integrity outcomes. Percentiles
                          from this file are reported for reference only, marked
                          as whole-invocation values.

Percentile keys in the summary come from k6 and are spelled p(50), p(90), p(99)
and p(99.9). Percentiles computed here from the CSV use linear interpolation
between order statistics, which is the ordinary definition and is stated so the
arithmetic is reproducible by hand from the committed rows.

Output is written to bands.txt by the harness and committed with the run. It
never prints the absolute results directory path, only the directory name: the
harness audits every committed artifact for home directory paths and would
reject its own gate report.
"""

from __future__ import annotations

import gzip
import json
import os
import re
import statistics
import sys
from dataclasses import dataclass, field

# These plot scripts read committed artifacts and must never generate load, open a socket,
# or shell out. CI proves it with an import-anchored pattern, not a bare word search,
# because "requests" appears here as ordinary prose. No output is the passing state:
#
#   rg -n '^\s*(import|from)\s+(subprocess|socket|urllib|requests)\b' benchmarks/plots/*.py

# Band 1. A direct-to-mock cell measures the generator, the loopback stack and the mock, with no
# proxy in the path. If that floor already sits at the proxy budget, the box or the mock is the
# bottleneck and no proxied number from the run means anything, so a breach invalidates the run
# rather than naming a levee regression. One calibrated pair per direct-cell group, keyed by
# (stream, prompt_bytes): each P50 ceiling is that group's own median P50 times one multiplier
# shared across every group, rounded down to the nearest 0.5ms, and a product straddling a bin
# edge across data cuts takes the lowest bin. The P99 leg is the same rule on the group's own
# median P99 and only ever ADVISES, because a loud tail on a proxy-free cell is host noise. An
# uncalibrated group FAILS by name rather than borrowing a neighbour's ceiling. Band 1 reads
# contended-cells.txt only to ANNOTATE a failure, never to excuse one: a floor measured on a busy
# machine is still that run's floor. See "Band 1, the direct-to-mock floor" in the methodology
# docs. Insertion order below is the reporting order, non-streaming payload ladder first.
BAND1_DIRECT_GROUP_THRESHOLDS: dict[tuple[bool, int], tuple[float, float]] = {
    (False, 150): (1.0, 2.5),
    (False, 4096): (1.0, 2.5),
    (False, 32768): (1.5, 3.5),
    (True, 150): (2.0, 5.5),
}

# Band 2. Passthrough adds one extra loopback HTTP hop over direct. Below the
# floor the extra hop is missing, which means the cell did not go through the
# proxy at all. Above the ceiling something other than the hop is being paid.
BAND2_PASSTHROUGH_SHIFT_MIN_MILLISECONDS = 0.05
BAND2_PASSTHROUGH_SHIFT_MAX_MILLISECONDS = 0.6

# Band 3. The gated quantity is the repetition-matched enforce minus passthrough P50 median at
# BAND3_PRIMARY_PAYLOAD_BYTES: token estimation plus admission plus reconcile, where at 4096B
# the estimation term dominates by two orders of magnitude.
#
# THE GATE IS AT 4096B AND NOT AT 150B FOR SIGNAL TO NOISE. Against the estimator noise floor
# the 150B shift is 3.7:1 and the 4096B shift is 164:1. The 150B window was never wrong about
# the quantity, it just cannot be resolved, so it is RECORDED below instead.
#
# The floor catches an enforce arm that is not enforcing, which reads what the A/A control
# reads, so it sits an order of magnitude clear of that. The ceiling sits 45 percent above the
# measured central value, wide enough to absorb the between-run spread on this host and tight
# enough to catch one extra tokenizer pass. BELOW THE FLOOR, check the rendered config and the
# agent header reached the enforce arm. ABOVE THE CEILING, count the tokenizer passes. The
# response to a repeat breach is never to widen the ceiling. A quick run and an evidence run
# measure different central values here, the evidence regime reading lower and quick mode's
# worst reading leaving little headroom, so check a quick-run failure against an evidence run
# before calling it a regression. See "Band 3, the primary enforcement gate" in calibration.md
# and "Band 3, enforcement over passthrough" in bands.md.
#
# STALE AS OF 2026-09-17, AND THIS CEILING IS NOW WEAKER THAN IT READS. The ceiling was sized
# when the shipped code made TWO tokenizer passes, so only a THIRD would have been a new
# regression. Commit f295ed4 made it tokenize ONCE, so a reintroduced SECOND pass now measures
# roughly 655us and sails under 0.95ms: this band would PASS the very regression that fix
# removed. Deliberately not moved, because recalibrating needs a post-fix evidence run to
# establish the new central value. Until then the real guards are stronger than this band ever
# was: tokenEstimator in internal/proxy/proxy.go omits Estimate, so reintroducing the second
# pass is a COMPILE ERROR, and TestEnforcedRequestTokenizesBodyOnce counts the passes directly.
# Expect a post-fix central value near 300 to 330us and size the floor and ceiling from it.
BAND3_PRIMARY_PAYLOAD_BYTES = 4096
BAND3_PRIMARY_SHIFT_MIN_MILLISECONDS = 0.15
BAND3_PRIMARY_SHIFT_MAX_MILLISECONDS = 0.95

# The 150B enforcement reading, RECORDED with an advisory and no longer a gate. The window is
# the original band 3 shape with one change: the floor is NEGATIVE.
#
# THE NEGATIVE FLOOR IS CORRECT AND READS AS A BUG ON SIGHT, so do not "fix" it to 0.0.
# Enforcement can only ADD work, but the published net is a small difference between two larger
# terms: gross enforce-only work here is 38.2us while the SHARED path runs about 24us FASTER in
# the enforce arm, netting +15us. The credit is a real measured term in shared code, not
# enforcement doing less, so the subtraction does not have to come out positive. Dropping one
# of the two tokenizer passes takes gross work to 20.8us against the same credit, putting the
# net near -3us, so a floor of 0.0 would fail every quiet run on a codebase that had just
# become FASTER. At -0.05ms the floor clears that predicted -3us by an order of magnitude and
# absorbs the estimator noise floor on top. A negative reading is EXPECTED and is not
# enforcement becoming free: the work itself is measured by the 4096B gate and by
# microbench.txt, neither of which can go negative. See "Band 3-SMALL, the 150-byte reading".
BAND3_SMALL_SHIFT_ADVISORY_MIN_MILLISECONDS = -0.05
BAND3_SMALL_SHIFT_ADVISORY_MAX_MILLISECONDS = 0.10

# Band 4. The P99 companion to band 3. A tail shift far above the median shift means one
# cell caught a transient even though every median gate passed. Band 4 is a RATIO against
# band 3's median P50 shift, so it reads the SAME payload band 3 does: a 150B P99 shift over
# a 4096B P50 shift would be arithmetic between two different experiments.
BAND4_TAIL_SHIFT_RATIO_MAX = 10.0

# Band 3 streaming companion. Band 3 and band 4 gate the NON-streaming enforcement shift, so
# without this a streaming enforcement regression could publish silently.
#
# NOTHING ON THE STREAMING PATH IS ENFORCEMENT-CONDITIONAL, which is what sizes this band.
# Per-event usage inspection, stream_options injection and the stream reconcile decision are paid
# by the passthrough cell too and cancel out of the shift, so the streaming shift should sit
# close to the non-streaming one. The gate is therefore on the shift's ABSOLUTE SIZE and is
# deliberately wide, sized to that inherent work plus the drift envelope observed on this host.
# A tight window would be a gate on drift: the streaming reading CHANGES SIGN across runs while
# the non-streaming one never does, and the envelope is an order of magnitude larger than the
# work measured. So the CENTRAL VALUE is recorded and advised on, never gated, and must not be
# published as the streaming enforcement cost. TWO-SIDED deliberately, because a strongly
# negative shift is the signature of an enforce cell that was not enforcing. Never mistake this
# for tight: it fires only on roughly a 40-fold regression, and closing that needs streaming
# repetitions and a streaming drift canary rather than a tighter number here. See
# "Band 3-STREAM, the streaming enforcement shift" in the methodology docs.
BAND3_STREAM_SHIFT_MAX_ABSOLUTE_MILLISECONDS = 0.60

# Band 5. The opening and closing direct cells bracket the whole matrix. If they disagree the
# machine drifted underneath the experiment, no cell can be compared to any other, and the run is
# invalid. TWO ABSOLUTE GATES, P50 drift and P99 drift, each with its own ceiling. The percentage
# is still printed beside both as context and is NOT a gate: a percentage tolerance on a
# sub-millisecond quantity produces an absolute tolerance tighter than the signal the experiment
# measures, so the original 15 percent form could reject a run whose deltas were perfectly
# resolvable. The P50 ceiling is sized to the band 2 shift it protects, with more slack than that
# suggests because passthrough and enforce run back to back so slow drift largely cancels WITHIN
# each pair. What the canary really catches is GROSS drift, a thermal collapse or a background job
# that arrived mid run and stayed, which biases whole pairs. The tail stayed a GATE because that
# is where gross drift shows up as multi-millisecond moves. See "Band 5, the drift canary".
BAND5_CANARY_DRIFT_MAX_MILLISECONDS = 0.25
BAND5_CANARY_TAIL_DRIFT_MAX_MILLISECONDS = 1.50

# The achieved-versus-demanded arrival rate gate. Every band below compares quantiles between
# cells, which assumes each cell's latency distribution is SERVICE TIME. A cell demanding more
# than its capacity reports QUEUE RESIDENCE instead, a property of the arrival rate and the pool
# size rather than of levee, and no band can tell the two apart because a queue raises the central
# tendency exactly the way real work does. This NEITHER SUBSUMES THE DROP GATE NOR IS SUBSUMED BY
# IT: drops subtract from completions one for one so a cell dropping more than this tolerance
# already fails here, while below that a drop proves the VU pool had no free slot at a scheduled
# arrival, which is invisible at this floor. The two fail in opposite blind spots and both are
# kept. The tolerance is roughly 100 times the legitimate shortfall envelope and still far smaller
# than the failure it catches, because saturation here is RETROGRADE past the knee: an
# over-demanded cell does not miss by a few percent, it collapses. Sizing nearer the envelope
# would start rejecting runs for single-iteration host stalls. See "The RATE gate margin".
RATE_SHORTFALL_TOLERANCE_FRACTION = 0.02

# A cell's achieved rate is recomputed from the COMMITTED CSV rows and cross checked
# against the figure k6 reported, because the summary is written by the process under
# measurement while the CSV is the artifact that gets published. The two count the same
# population, so a wider disagreement means one of them is not describing the published
# window. The measured divergence is exactly zero on every cell in this tree, so this is a
# guard against a future filter or CSV-format change rather than a tolerance being strained.
RATE_CROSSCHECK_TOLERANCE_FRACTION = 0.01

# THE TWO INTEGRITY TOLERANCES, MIRRORING run.sh. These four numbers replaced the absolute-zero k6
# thresholds dropped_iterations{scenario:steady}: count==0 and http_req_failed{scenario:steady}:
# rate==0. The derivation lives at STEADY_DROP_TOLERANCE_BASIS_POINTS in benchmarks/harness/run.sh
# and is not duplicated. What IS duplicated is the ARITHMETIC, deliberately, and it has to stay
# exact. check_bands RE-DERIVES the allowance rather than only reading the recorded one, because a
# committed directory has to be judgeable by a stranger with this file and nothing else, including
# one written before the allowance was recorded. The recorded value only cross checks agreement.
#
# WHY BASIS POINTS AND FLOOR DIVISION. run.sh computes demanded times points over 10000 in shell
# integer arithmetic. A float multiply here, demanded times 0.01, can land a hair either side of an
# integer boundary and would let one cell be judged 300 by k6 and 299 here. Integer floor division
# reproduces the shell exactly. See "The integrity tolerances" in calibration.md.
STEADY_DROP_TOLERANCE_BASIS_POINTS = 100
STEADY_DROP_TOLERANCE_FLOOR = 25
STEADY_FAILED_TOLERANCE_BASIS_POINTS = 5
STEADY_FAILED_TOLERANCE_FLOOR = 5


def tolerance_from_basis_points(demanded: int, points: int, floor: int) -> int:
    """Return the integer allowance for one cell, identical to run.sh's arithmetic.

    One basis point is one ten-thousandth. Floor division rather than a float
    multiply, so this and the shell can never differ by a rounding step.
    """
    return max(floor, (int(demanded) * points) // 10000)

# The contended-repetition exclusion. run.sh no longer aborts an evidence run on an isolated host
# CPU idle dip, only on a SUSTAINED breach, because a run takes over a hundred readings and at
# least one dip is close to inevitable on the reference host. An isolated dip is RECORDED instead:
# cpu_idle_breach=yes in machine-state.txt and a line in contended-cells.txt. THAT RECORD HAS TO
# CHANGE ARITHMETIC HERE RATHER THAN ONLY BE PRINTED, because the contention that invalidated the
# first completed evidence run PASSED every band in this file, so a marking that only printed
# would leave the contaminated repetition inside every median while assuring the reader it was
# contaminated. Every median drops the contended repetitions first.
#
# THREE IS THE FLOOR, and this is what the five-repetition design is for. Below three a median
# stops being an order statistic over independent measurements: two makes it an average of the
# only two values left, and one makes it that value. A run with fewer than three clean repetitions
# FAILS rather than computing over what is left and noting the weakness, which would be the same
# move as widening a band to rescue a run. See "The contention exclusion" in bands.md.
MINIMUM_CLEAN_REPETITIONS = 3

# STREAM_MINIMUM_CLEAN_REPETITIONS is a RELAXATION of the rule above for BAND3-STREAM alone. The
# streaming matrix runs THREE repetitions where the non-streaming one runs five, so a minimum of
# three permits ZERO contended streaming repetitions across the 12 host idle readings its 6 cells
# take. That is the same absolute-zero-across-many-opportunities defect the integrity tolerances
# above were amended to remove, and it lands after the LAST cell so it costs the whole run. The
# non-streaming bands are unaffected and keep a minimum of three. THE ORDER-STATISTIC ARGUMENT
# DOES NOT BIND HERE: it binds on a gate whose window is comparable to the quantity, and this one
# is a wide two-sided ceiling on a shift's ABSOLUTE size whose central value is already
# advisory-only, so nothing published is computed from this median. Zero clean repetitions still
# fails, and the band prints a loud advisory whenever it runs below MINIMUM_CLEAN_REPETITIONS.
# See "The streaming repetition minimum" in calibration.md.
STREAM_MINIMUM_CLEAN_REPETITIONS = 1

# The ledger run.sh writes. Its ABSENCE and its EMPTINESS mean different things: an
# absent file is a directory that predates the marking, while a present and empty one
# is a positive statement that no reading breached the floor.
CONTENDED_CELLS_FILENAME = "contended-cells.txt"

# The small payload. Band 2, the recorded 150B enforcement advisory, the streaming
# companion and the A/A control all read cells at this size. The PRIMARY enforcement
# gate does not any more, see BAND3_PRIMARY_PAYLOAD_BYTES above.
SMALL_PAYLOAD_BYTES = 150

# The A/A control pair. Two cells that both run the PASSTHROUGH config at the small payload, so the
# repetition-matched P50 shift between them has a KNOWN TRUE VALUE OF ZERO and whatever it reads is
# the estimator's own noise. REPORTED AND NEVER GATED: a CONTENDED A/A pair still read 13us, so a
# passing control does NOT certify a quiet host and gating on it would create exactly the false
# confidence the control was added to remove. It is necessary and not sufficient, and the check
# that refuses a contended host is the host quiescence floor in run.sh.
#
# The role names come from run.sh's cell names and are what check_bands derives a role from, the
# text before the first hyphen, so these two strings and run.sh must agree. See "The A/A control
# pair" in bands.md.
CONTROL_A_ROLE = "controla"
CONTROL_B_ROLE = "controlb"

DURATION_METRIC = "http_req_duration"
WAITING_METRIC = "http_req_waiting"

REPETITION_PATTERN = re.compile(r"-r(\d+)$")


def spread_phrase(values: list[float]) -> str:
    """Describe the across-repetition spread, or say it cannot be resolved.

    A single repetition has a spread of zero by arithmetic rather than by
    measurement, and printing "spread 0.000ms across 1 repetitions" invites exactly
    the misreading that a quick-mode run resolved something.
    """
    if len(values) < 2:
        return "single repetition, so the spread is not resolvable"
    return f"spread {max(values) - min(values):.3f}ms across {len(values)} repetitions"


@dataclass
class Cell:
    """One k6 invocation, as recovered from the two artifacts it wrote."""

    name: str
    role: str
    stream: bool
    prompt_bytes: int
    rate: float
    max_vus: int
    repetition: int | None
    steady_duration_samples: list[float] = field(default_factory=list)
    steady_waiting_samples: list[float] = field(default_factory=list)
    summary_duration: dict = field(default_factory=dict)
    dropped_steady: int = 0
    dropped_warmup: int = 0
    failed_count: int = 0
    failed_steady: int = 0
    failed_warmup: int = 0
    request_count: int = 0
    thresholds: dict = field(default_factory=dict)
    steady_seconds: float = 0.0
    reported_steady_requests: int | None = None
    reported_demanded_requests: int | None = None
    reported_drop_allowance: int | None = None
    reported_failed_allowance: int | None = None
    cpu_seconds: float | None = None
    cpu_milliseconds_per_request: float | None = None

    @property
    def demanded_steady_requests(self) -> int | None:
        """The steady iterations this cell was asked to schedule.

        Recomputed from the demanded rate and the steady window rather than taken
        from the summary, so a directory written before the field existed is still
        judgeable. None when neither is recoverable, which the caller reports as an
        unknown rather than as a passing zero.
        """
        if self.rate <= 0 or self.steady_seconds <= 0:
            return None
        return int(round(self.rate * self.steady_seconds))

    @property
    def drop_allowance(self) -> int:
        """The steady dropped-iteration allowance, identical to the one k6 used."""
        demanded = self.demanded_steady_requests
        if demanded is None:
            # No recoverable demand means the fractional part cannot be computed, so
            # the cell falls back to the absolute floor alone. That is the strictest
            # of the two terms, which is the correct direction when the harness cannot
            # tell how large the window was.
            return STEADY_DROP_TOLERANCE_FLOOR
        return tolerance_from_basis_points(
            demanded, STEADY_DROP_TOLERANCE_BASIS_POINTS, STEADY_DROP_TOLERANCE_FLOOR
        )

    @property
    def failed_allowance(self) -> int:
        """The steady failed-request allowance, identical to the one k6 used."""
        demanded = self.demanded_steady_requests
        if demanded is None:
            return STEADY_FAILED_TOLERANCE_FLOOR
        return tolerance_from_basis_points(
            demanded, STEADY_FAILED_TOLERANCE_BASIS_POINTS, STEADY_FAILED_TOLERANCE_FLOOR
        )

    def percentile(self, quantile: float) -> float:
        return percentile(self.steady_duration_samples, quantile)

    def waiting_percentile(self, quantile: float) -> float:
        return percentile(self.steady_waiting_samples, quantile)

    @property
    def csv_steady_requests(self) -> int:
        """The published row count, which is the population every band uses."""
        return len(self.steady_duration_samples)

    @property
    def achieved_rate(self) -> float | None:
        """Achieved steady throughput recomputed from the committed CSV rows.

        None when the steady duration is unrecoverable, which is the only case
        where the arithmetic cannot be done rather than merely coming out short.
        """
        if self.steady_seconds <= 0:
            return None
        return self.csv_steady_requests / self.steady_seconds

    @property
    def achieved_fraction(self) -> float | None:
        achieved = self.achieved_rate
        if achieved is None or self.rate <= 0:
            return None
        return achieved / self.rate


def percentile(values: list[float], quantile: float) -> float:
    """Return the quantile of values by linear interpolation between neighbours.

    quantile is a percentage. An empty input is a programming error here rather
    than a missing measurement, because every caller has already established
    that the cell produced steady rows.
    """
    if not values:
        raise ValueError("percentile of an empty sample")
    ordered = sorted(values)
    if len(ordered) == 1:
        return ordered[0]
    position = (len(ordered) - 1) * quantile / 100.0
    lower_index = int(position)
    upper_index = min(lower_index + 1, len(ordered) - 1)
    fraction = position - lower_index
    return ordered[lower_index] + (ordered[upper_index] - ordered[lower_index]) * fraction


def read_steady_samples(csv_path: str) -> tuple[list[float], list[float]]:
    """Return the duration and waiting samples from one filtered cell CSV.

    The column positions are read out of the header rather than assumed, so a k6
    CSV layout change fails here with a named cause instead of silently
    producing an empty or wrongly keyed sample.
    """
    durations: list[float] = []
    waitings: list[float] = []
    with gzip.open(csv_path, "rt", newline="") as handle:
        header_line = handle.readline()
        if not header_line:
            raise ValueError(f"{os.path.basename(csv_path)} is empty")
        header = header_line.rstrip("\n").split(",")
        for required in ("metric_name", "metric_value", "scenario"):
            if required not in header:
                raise ValueError(
                    f"{os.path.basename(csv_path)} header has no {required} column, "
                    "the k6 CSV format changed"
                )
        metric_index = header.index("metric_name")
        value_index = header.index("metric_value")
        scenario_index = header.index("scenario")
        for line in handle:
            columns = line.rstrip("\n").split(",")
            if len(columns) <= max(metric_index, value_index, scenario_index):
                continue
            if columns[scenario_index] != "steady":
                raise ValueError(
                    f"{os.path.basename(csv_path)} carries a "
                    f"{columns[scenario_index]!r} scenario row, the filter in run.sh "
                    "did not do its job"
                )
            metric = columns[metric_index]
            if metric == DURATION_METRIC:
                durations.append(float(columns[value_index]))
            elif metric == WAITING_METRIC:
                waitings.append(float(columns[value_index]))
    return durations, waitings


DURATION_SUFFIX_SECONDS = {"ms": 0.001, "s": 1.0, "m": 60.0, "h": 3600.0}


def parse_steady_seconds(summary: dict) -> float:
    """Return the steady window length in seconds from one cell summary.

    The numeric field is preferred. Directories written before it existed carry
    only the k6 duration STRING, so that is parsed as a fallback rather than
    refusing to gate an older run at all. An unparseable value returns 0, which
    the caller reports as an unrecoverable rate rather than as a passing one.
    """
    numeric = summary.get("steady_seconds")
    if isinstance(numeric, (int, float)) and numeric > 0:
        return float(numeric)
    text = str(summary.get("steady_duration", "")).strip()
    for suffix, multiplier in sorted(
        DURATION_SUFFIX_SECONDS.items(), key=lambda item: -len(item[0])
    ):
        if text.endswith(suffix):
            try:
                return float(text[: -len(suffix)]) * multiplier
            except ValueError:
                return 0.0
    return 0.0


def optional_int(summary: dict, key: str) -> int | None:
    """Return one summary field as an int, or None when it is absent.

    Absent means the directory predates the field. None rather than zero, because a
    recorded allowance of zero and no recorded allowance at all are different facts
    and the cross check below reports them differently.
    """
    raw = summary.get(key)
    if isinstance(raw, bool) or not isinstance(raw, (int, float)):
        return None
    return int(raw)


def optional_number(values: dict[str, str], key: str) -> float | None:
    """Return one KEY=VALUE field as a float, or None when absent or "na".

    "na" is what run.sh writes for a cell with no levee in its path, so it means
    "not applicable here" rather than "measured as zero", and the two must not
    collapse into the same value.
    """
    raw = values.get(key)
    if raw is None or raw == "na":
        return None
    try:
        return float(raw)
    except ValueError:
        return None


def read_cpu_records(results_dir: str) -> dict[str, tuple[float | None, float | None]]:
    """Return per-cell CPU seconds and CPU milliseconds per request.

    Parsed from cpu-seconds.txt, which run.sh writes from ps cputime deltas at the two
    edges of each cell's steady window. A missing file means the run predates the
    sampling. Direct cells record "na" because no levee is in their path.
    """
    path = os.path.join(results_dir, "cpu-seconds.txt")
    records: dict[str, tuple[float | None, float | None]] = {}
    if not os.path.exists(path):
        return records
    with open(path, encoding="utf-8") as handle:
        for line in handle:
            fields = line.split()
            if not fields:
                continue
            name = fields[0]
            values: dict[str, str] = {}
            for field_text in fields[1:]:
                if "=" in field_text:
                    key, _, value = field_text.partition("=")
                    values[key] = value

            records[name] = (
                optional_number(values, "cpu_seconds"),
                optional_number(values, "cpu_ms_per_request"),
            )
    return records


@dataclass
class Contention:
    """Which cells run.sh measured while the host was below its CPU idle floor.

    recorded says whether the ledger file existed at all, which separates "this run
    measured no contention" from "this run could not have told you either way".
    """

    recorded: bool
    notes: dict[str, list[str]] = field(default_factory=dict)

    def contended(self, cell: Cell) -> bool:
        return cell.name in self.notes


def read_contention(results_dir: str) -> Contention:
    """Parse contended-cells.txt into the set of cells measured during a dip.

    A malformed line is skipped rather than raised on: this file is a record, and a
    parse failure inside it must not stop a run whose measurements are fine.
    """
    path = os.path.join(results_dir, CONTENDED_CELLS_FILENAME)
    if not os.path.exists(path):
        return Contention(recorded=False)
    notes: dict[str, list[str]] = {}
    with open(path, encoding="utf-8") as handle:
        for line in handle:
            stripped = line.strip()
            if not stripped or stripped.startswith("#"):
                continue
            values: dict[str, str] = {}
            for field_text in stripped.split():
                key, separator, value = field_text.partition("=")
                if separator:
                    values[key] = value
            name = values.get("cell")
            if not name:
                continue
            notes.setdefault(name, []).append(
                "{} {} at idle {}".format(
                    values.get("phase", "unknown-phase"),
                    values.get("reason", "unknown-reason"),
                    values.get("cpu_idle_pct", "unknown"),
                )
            )
    return Contention(recorded=True, notes=notes)


@dataclass
class RepetitionSet:
    """The repetition ordinals one paired band may compute its median over."""

    present: list[int]
    dropped: list[int]
    kept: list[int]
    evaluable: bool
    cause: str
    note: str


def usable_repetitions(
    groups: list[list[Cell]],
    contention: Contention,
    minimum: int = MINIMUM_CLEAN_REPETITIONS,
) -> RepetitionSet:
    """Decide which repetitions a paired band may use after dropping contended ones.

    minimum defaults to MINIMUM_CLEAN_REPETITIONS. BAND3-STREAM passes
    STREAM_MINIMUM_CLEAN_REPETITIONS, argued at that constant. A repetition is dropped
    when ANY arm was contended, because the published quantity is a within-pair shift.

    THE SCOPE IS ONE PAIRING, which is why every caller passes only its own arms. A
    repetition ordinal is a join key inside a pairing, not a moment in time: the 150B
    and 4096B cells of repetition 4 ran minutes apart, so a dip during one says nothing
    about the other and dropping r4 everywhere would discard quiet-host measurements.

    A cell with NO repetition ordinal is never dropped. That SINGLE-INSTANCE carve-out is
    deliberate: the drift canaries and the direct payload cells run once per matrix, so
    there is nothing to drop them in favour of. It is safe because the bands reading them
    already tolerate a contended host, band 5 gating canary drift directly and band 1
    judging each against its own group's ceiling. Band 1 reads the ledger only to
    ANNOTATE a failure it has already decided on, never to excuse one.
    """
    ordinals: set[int] = set()
    dropped: set[int] = set()
    for group in groups:
        for cell in group:
            if cell.repetition is None:
                continue
            ordinals.add(cell.repetition)
            if contention.contended(cell):
                dropped.add(cell.repetition)

    present = sorted(ordinals)
    dropped_list = sorted(dropped)
    kept = sorted(ordinals - dropped)

    if not dropped_list:
        return RepetitionSet(present, dropped_list, kept, True, "", "")

    note = (
        f"{len(dropped_list)} of {len(present)} repetitions dropped for host "
        "contention, "
        + ", ".join(f"r{ordinal}" for ordinal in dropped_list)
        + f", leaving {len(kept)} clean"
    )

    if len(present) >= minimum:
        if len(kept) >= minimum:
            return RepetitionSet(present, dropped_list, kept, True, "", note)
        cause = (
            f"only {len(kept)} of {len(present)} repetitions were measured on a quiet "
            f"host and this band needs at least {minimum}. The "
            "dropped repetitions are "
            + ", ".join(f"r{ordinal}" for ordinal in dropped_list)
            + ", each named in contended-cells.txt with the phase and the idle reading "
            "that disqualified it. THE CAUSE IS HOST CONTENTION AND NOT LEVEE: the "
            "host CPU idle reading fell below its floor while those cells were being "
            "measured, and a repetition measured during a dip cannot go into a median "
            f"that gets published. Below {minimum} the median stops being an order "
            "statistic over independent measurements, so the honest outcome is an "
            "invalid run rather than a narrower median. Rerun on a quiet machine"
        )
        return RepetitionSet(present, dropped_list, kept, False, cause, note)

    # Fewer repetitions than the minimum in the first place, which is quick mode with
    # one. There is nothing to fall back to, so an exclusion here empties the band
    # rather than narrowing it.
    if kept:
        return RepetitionSet(present, dropped_list, kept, True, "", note)
    cause = (
        f"every one of its {len(present)} repetitions was measured during a host "
        "contention dip, so after the exclusion there is nothing left to take a median "
        "of and this band is UNEVALUABLE rather than passing. A run with this few "
        "repetitions has no spare measurement to fall back on, which is why evidence "
        "mode runs five. The contended cells are named in contended-cells.txt"
    )
    return RepetitionSet(present, dropped_list, kept, False, cause, note)


def keep_clean(cells: list[Cell], usable: RepetitionSet) -> list[Cell]:
    """Filter one arm down to the repetitions the band may use."""
    return [
        cell for cell in cells if cell.repetition is None or cell.repetition in usable.kept
    ]


def load_cells(results_dir: str) -> list[Cell]:
    """Load every cell in the results directory, newest naming scheme aside.

    A summary without its CSV, or the reverse, is a partial run and is refused.
    """
    cpu_records = read_cpu_records(results_dir)
    cells: list[Cell] = []
    for entry in sorted(os.listdir(results_dir)):
        if not entry.endswith(".summary.json"):
            continue
        name = entry[: -len(".summary.json")]
        with open(os.path.join(results_dir, entry), encoding="utf-8") as handle:
            summary = json.load(handle)
        csv_path = os.path.join(results_dir, f"{name}.duration.csv.gz")
        if not os.path.exists(csv_path):
            raise ValueError(f"cell {name} has a summary but no filtered CSV")
        durations, waitings = read_steady_samples(csv_path)
        if not durations:
            raise ValueError(f"cell {name} has no steady {DURATION_METRIC} rows")
        repetition_match = REPETITION_PATTERN.search(name)
        # Only STEADY drops invalidate a cell. A warmup drop is levee's cold
        # start being absorbed by the phase that exists to absorb it, and the
        # enforce cells legitimately drop tens of iterations there while the
        # tokenizer is built on the first request. The k6 threshold is scoped
        # the same way, so gating on the total here would re-fail exactly what
        # the threshold correctly tolerated. The warmup count is reported rather
        # than discarded, because a climbing warmup count is a real signal.
        dropped_total = int(summary.get("dropped_iterations", 0))
        dropped_steady = int(summary.get("dropped_iterations_steady", dropped_total))
        dropped_warmup = int(summary.get("dropped_iterations_warmup", 0))
        # The whole-invocation failure count is the fallback for directories that carry
        # only that. It reads zero everywhere in this tree, so the fallback is exact for
        # every one of them rather than merely safe.
        failed_total = int(summary.get("http_req_failed_count", 0))
        failed_steady = int(summary.get("http_req_failed_steady_count", failed_total))
        failed_warmup = int(summary.get("http_req_failed_warmup_count", 0))
        reported_steady = summary.get("steady_request_count")
        cpu_seconds, cpu_per_request = cpu_records.get(name, (None, None))
        cells.append(
            Cell(
                name=name,
                role=name.split("-", 1)[0],
                stream=bool(summary.get("stream")),
                prompt_bytes=int(summary.get("prompt_bytes", 0)),
                rate=float(summary.get("rate", 0)),
                max_vus=int(summary.get("max_vus", 0)),
                repetition=int(repetition_match.group(1)) if repetition_match else None,
                steady_duration_samples=durations,
                steady_waiting_samples=waitings,
                summary_duration=summary.get("http_req_duration", {}) or {},
                dropped_steady=dropped_steady,
                dropped_warmup=dropped_warmup,
                failed_count=failed_total,
                failed_steady=failed_steady,
                failed_warmup=failed_warmup,
                request_count=int(summary.get("request_count", 0)),
                thresholds=summary.get("thresholds", {}) or {},
                steady_seconds=parse_steady_seconds(summary),
                reported_steady_requests=(
                    int(reported_steady) if isinstance(reported_steady, (int, float)) else None
                ),
                reported_demanded_requests=optional_int(summary, "demanded_steady_requests"),
                reported_drop_allowance=optional_int(
                    summary, "max_steady_dropped_iterations"
                ),
                reported_failed_allowance=optional_int(
                    summary, "max_steady_failed_requests"
                ),
                cpu_seconds=cpu_seconds,
                cpu_milliseconds_per_request=cpu_per_request,
            )
        )
    if not cells:
        raise ValueError("no cell summaries found, this directory is not a run")
    return cells


def select(cells: list[Cell], role: str, stream: bool, prompt_bytes: int | None = None) -> list[Cell]:
    chosen = [cell for cell in cells if cell.role == role and cell.stream == stream]
    if prompt_bytes is not None:
        chosen = [cell for cell in chosen if cell.prompt_bytes == prompt_bytes]
    return sorted(chosen, key=lambda cell: cell.name)


def paired_shifts(
    baseline_cells: list[Cell], treatment_cells: list[Cell], quantile: float
) -> list[tuple[str, float]]:
    """Return per-repetition treatment minus baseline quantile shifts.

    Cells are matched on their repetition ordinal, which is what makes the shift a
    within-pair comparison rather than one across cells that ran minutes apart.
    """
    by_repetition = {cell.repetition: cell for cell in baseline_cells}
    shifts: list[tuple[str, float]] = []
    for cell in treatment_cells:
        baseline = by_repetition.get(cell.repetition)
        if baseline is None:
            continue
        shifts.append((f"r{cell.repetition}", cell.percentile(quantile) - baseline.percentile(quantile)))
    return shifts


class Report:
    """Accumulates the gate report and remembers whether anything failed."""

    def __init__(self) -> None:
        self.failed = False

    def line(self, text: str = "") -> None:
        print(text)

    def verdict(self, label: str, ok: bool, detail: str) -> None:
        if not ok:
            self.failed = True
        print(f"{label} {'PASS' if ok else 'FAIL'} {detail}")


def report_inventory(report: Report, cells: list[Cell]) -> None:
    report.line("cell inventory and steady-scenario percentiles in milliseconds")
    report.line(
        "  {:<38} {:>7} {:>8} {:>8} {:>8} {:>8} {:>9} {:>7} {:>8}".format(
            "cell", "rows", "p50", "p90", "p99", "p99.9", "summaryp99", "demand", "achieved"
        )
    )
    for cell in sorted(cells, key=lambda item: item.name):
        summary_p99 = cell.summary_duration.get("p(99)")
        achieved = cell.achieved_rate
        report.line(
            "  {:<38} {:>7d} {:>8.3f} {:>8.3f} {:>8.3f} {:>8.3f} {:>9} {:>7g} {:>8}".format(
                cell.name,
                len(cell.steady_duration_samples),
                cell.percentile(50),
                cell.percentile(90),
                cell.percentile(99),
                cell.percentile(99.9),
                f"{summary_p99:.3f}" if isinstance(summary_p99, (int, float)) else "absent",
                cell.rate,
                f"{achieved:.1f}" if achieved is not None else "unknown",
            )
        )
    report.line(
        "  summaryp99 is the k6 whole-invocation value including warmup, "
        "shown for reference only"
    )
    report.line(
        "  demand and achieved are the arrival rate in requests per second, demanded by the "
        "harness and achieved in the steady window. They must agree, see the RATE gate: a cell "
        "short of its demand reports queue residence rather than service time, so every "
        "percentile on its row would be a number about the arrival rate and the VU pool"
    )
    # Time to first byte is reported for the streaming cells because against a
    # zero-latency upstream it sits only slightly below duration to EOF, and the
    # methodology has to show both magnitudes rather than imply a separation the mock
    # cannot produce.
    streaming = [cell for cell in cells if cell.stream and cell.steady_waiting_samples]
    if streaming:
        report.line("  time to first byte, steady scenario, streaming cells only")
        for cell in sorted(streaming, key=lambda item: item.name):
            report.line(
                "  {:<38} waiting p50 {:>7.3f} p99 {:>7.3f}".format(
                    cell.name, cell.waiting_percentile(50), cell.waiting_percentile(99)
                )
            )
    report.line()


def report_contention(report: Report, cells: list[Cell], contention: Contention) -> None:
    """State which cells were measured during a host CPU idle dip, and their fate.

    A REPORT rather than a gate. The gate is inside each band, which drops contended
    repetitions before computing its median. This section exists so the exclusion is
    visible as a list rather than only inferable from an ordinal missing out of a
    per-repetition breakdown.
    """
    if not contention.recorded:
        report.line(
            f"CONTENTION not recorded, this directory has no {CONTENDED_CELLS_FILENAME} "
            "and therefore predates the per-cell contention marking. Every band below "
            "computes over ALL of its repetitions, including any that were measured "
            "during a host CPU idle dip, so read cpu_idle_pct in machine-state.txt by "
            "hand before trusting a median from this run"
        )
        report.line()
        return

    if not contention.notes:
        report.line(
            "CONTENTION none, every host CPU idle reading in this run sat at or above "
            "its floor, so no repetition was excluded from any median below. This is a "
            "positive statement rather than an absent file: the ledger exists and is "
            "empty"
        )
        report.line()
        return

    known = {cell.name for cell in cells}
    repetition_bearing = sorted(
        name for name in contention.notes if REPETITION_PATTERN.search(name)
    )
    single_instance = sorted(
        name
        for name in contention.notes
        if not REPETITION_PATTERN.search(name) and name in known
    )
    unknown = sorted(name for name in contention.notes if name not in known)

    report.line(
        f"CONTENTION {len(contention.notes)} cells were measured while the host CPU "
        "idle reading was below its floor or unreadable. run.sh recorded them and "
        "continued, which it does for any breach that is neither two readings in a row "
        "nor more than a tenth of all readings, because an evidence run takes 106 "
        "readings and one isolated dip is expected on this host"
    )
    for name in sorted(contention.notes):
        report.line(f"  {name:<38} " + ", ".join(contention.notes[name]))
    if repetition_bearing:
        report.line(
            "  EXCLUDED FROM EVERY MEDIAN below, because these carry a repetition "
            "ordinal and the five-repetition design exists so a contaminated "
            "repetition can be dropped, "
            + ", ".join(repetition_bearing)
        )
    if single_instance:
        report.line(
            "  RECORDED AND KEPT, because these run once per matrix and have no "
            "repetition to be dropped in favour of, "
            + ", ".join(single_instance)
            + ". No new failure path is added for them and that is argued rather than "
            "assumed: band 5 measures canary drift directly and gates it, and band 1 "
            "judges each of them against its own group's P50 ceiling, 1.0ms at 150 bytes "
            "and 1.5ms at 32768, with ample headroom against an observed 0.363 to 0.899ms "
            "on the most contended host on record here. Both bands read these cells below "
            "and both judge them on their own numbers. Band 1 does read this ledger, and "
            "ONLY to annotate a failure it has already decided on, so no reading here can "
            "turn a band 1 failure into a pass"
        )
    if unknown:
        report.line(
            "  NAMED IN THE LEDGER BUT ABSENT FROM THIS RUN, which should be "
            "impossible and means the ledger and the cell summaries disagree, "
            + ", ".join(unknown)
        )
    report.line()


def check_integrity(report: Report, cells: list[Cell]) -> None:
    """Re-verify the k6 integrity outcomes from the committed summaries.

    The harness already fails a run on a non-zero k6 exit, but a reader of a committed
    directory should not have to trust that it did.

    Steady drops and failed requests are compared against per-cell TOLERANCES rather than
    zero, from the same basis-point arithmetic run.sh uses. Every RAW COUNT is reported
    whether it passed or not, so the tolerance can never hide a number. A recorded k6
    threshold failure is still fatal, so a directory whose k6 aborted under a SUPERSEDED
    rule keeps its verdict and the only way to get the new tolerance is a new run.
    """
    problems: list[str] = []
    warmup_notes: list[str] = []
    tolerated: list[str] = []
    allowance_disagreements: list[str] = []
    total_drops = 0
    total_failures = 0
    total_demanded = 0
    demand_recoverable = True

    for cell in sorted(cells, key=lambda item: item.name):
        demanded = cell.demanded_steady_requests
        if demanded is None:
            demand_recoverable = False
        else:
            total_demanded += demanded
        total_drops += cell.dropped_steady
        total_failures += cell.failed_steady

        drop_allowance = cell.drop_allowance
        failed_allowance = cell.failed_allowance

        if cell.dropped_steady > drop_allowance:
            problems.append(
                f"{cell.name} dropped {cell.dropped_steady} steady iterations against an "
                f"allowance of {drop_allowance} of {demanded if demanded else 'unknown'} "
                "demanded"
            )
        elif cell.dropped_steady != 0:
            tolerated.append(
                f"{cell.name} steady drops {cell.dropped_steady} of {drop_allowance} allowed"
            )

        if cell.failed_steady > failed_allowance:
            problems.append(
                f"{cell.name} had {cell.failed_steady} failed steady requests against an "
                f"allowance of {failed_allowance} of {demanded if demanded else 'unknown'} "
                "demanded"
            )
        elif cell.failed_steady != 0:
            tolerated.append(
                f"{cell.name} failed steady requests {cell.failed_steady} of "
                f"{failed_allowance} allowed"
            )

        if cell.dropped_warmup != 0:
            warmup_notes.append(f"{cell.name} drops {cell.dropped_warmup}")
        if cell.failed_warmup != 0:
            warmup_notes.append(f"{cell.name} failed requests {cell.failed_warmup}")

        if cell.request_count <= 0:
            problems.append(f"{cell.name} recorded no requests")

        # The allowance cross check. They must agree, or a cell was judged by one rule at
        # run time and another at read time. Reported rather than fatal, because the
        # comparison above already used the recomputed value, so a mismatch is a
        # harness-consistency finding rather than a bad measurement.
        for label, recorded, derived in (
            ("drop", cell.reported_drop_allowance, drop_allowance),
            ("failure", cell.reported_failed_allowance, failed_allowance),
        ):
            if recorded is not None and recorded != derived:
                allowance_disagreements.append(
                    f"{cell.name} {label} allowance recorded {recorded} but recomputed "
                    f"{derived}"
                )

        for expression, outcome in sorted(cell.thresholds.items()):
            if outcome != "pass":
                problems.append(f"{cell.name} threshold {expression} reported {outcome}")

    if problems:
        detail = ", ".join(problems)
    else:
        detail = (
            f"{len(cells)} cells, every steady dropped-iteration and failed-request "
            "count inside its per-cell allowance, every k6 threshold passed"
        )
    if tolerated:
        detail += (
            ". TOLERATED AND RECORDED, inside the per-cell allowance rather than absent, "
            + ", ".join(tolerated)
        )
    if warmup_notes:
        detail += (
            ". Warmup counts, tolerated by design and recorded so a climbing count stays "
            "visible, " + ", ".join(warmup_notes)
        )
    if allowance_disagreements:
        detail += (
            ". ALLOWANCE CROSS CHECK DISAGREED, so k6 and this checker did not compute the "
            "same per-cell budget and one of the two arithmetic paths has drifted, "
            + ", ".join(allowance_disagreements)
        )
    report.verdict("INTEGRITY", not problems, detail)

    # THE RUN TOTAL. Per-cell verdicts say whether any single cell was in trouble, not
    # what the whole matrix cost, and that aggregate is what says whether a tolerance is
    # being leaned on. A reader applying the old absolute rule reads this one line.
    if demand_recoverable and total_demanded > 0:
        drop_share = f"{total_drops / total_demanded * 100:.4f} percent"
        failure_share = f"{total_failures / total_demanded * 100:.4f} percent"
        basis = f"{total_demanded} demanded steady iterations across {len(cells)} cells"
    else:
        drop_share = "an unknown share"
        failure_share = "an unknown share"
        basis = (
            f"{len(cells)} cells, at least one of which records no recoverable demanded "
            "iteration count, so the shares cannot be computed"
        )
    report.line(
        f"INTEGRITY TOTALS {total_drops} steady dropped iterations and {total_failures} "
        f"failed steady requests across the whole run, {drop_share} and {failure_share} "
        f"of {basis}. Both are gated PER CELL against a fraction of that cell's own "
        f"demand, {STEADY_DROP_TOLERANCE_BASIS_POINTS} basis points with a floor of "
        f"{STEADY_DROP_TOLERANCE_FLOOR} for drops and "
        f"{STEADY_FAILED_TOLERANCE_BASIS_POINTS} basis points with a floor of "
        f"{STEADY_FAILED_TOLERANCE_FLOOR} for failures, so this total is context rather "
        "than a gate. It is printed because an absolute-zero rule across a matrix this "
        "size is a lottery and the honest replacement has to show its own aggregate"
    )
    report.line()


def check_achieved_rate(report: Report, cells: list[Cell]) -> None:
    """Refuse to publish a latency number from a cell that missed its demand.

    Runs BEFORE every band, because a band comparing two cells is only meaningful once
    both are known to have reported service time rather than queue residence.
    """
    floor = 1.0 - RATE_SHORTFALL_TOLERANCE_FRACTION
    problems: list[str] = []
    observations: list[str] = []

    for cell in sorted(cells, key=lambda item: item.name):
        achieved = cell.achieved_rate
        fraction = cell.achieved_fraction
        if achieved is None or fraction is None:
            problems.append(
                f"{cell.name} has no recoverable steady duration or demanded rate, so whether "
                "it served its demand is unknown and its latency number cannot be published"
            )
            continue

        observations.append(
            f"{cell.name} {achieved:.1f} of {cell.rate:g} demanded "
            f"({fraction * 100:.1f} percent)"
        )
        if fraction < floor:
            problems.append(
                f"{cell.name} achieved {achieved:.1f} rps of {cell.rate:g} demanded, "
                f"{(1.0 - fraction) * 100:.1f} percent short"
            )

        # The cross check. k6 counted the steady requests itself, and the committed rows
        # were filtered out of the raw CSV by a separate code path in run.sh. A
        # disagreement means one of them is not describing the window the bands will read.
        if cell.reported_steady_requests is not None and cell.reported_steady_requests > 0:
            divergence = abs(
                cell.csv_steady_requests - cell.reported_steady_requests
            ) / cell.reported_steady_requests
            if divergence > RATE_CROSSCHECK_TOLERANCE_FRACTION:
                problems.append(
                    f"{cell.name} committed {cell.csv_steady_requests} steady rows but k6 "
                    f"counted {cell.reported_steady_requests} steady requests, a "
                    f"{divergence * 100:.1f} percent disagreement, so the published rows are "
                    "not the window k6 measured"
                )

    if problems:
        detail = (
            ", ".join(problems)
            + f". A cell below {floor * 100:.0f} percent of its demanded arrival rate is "
            "reporting queue residence rather than service time, so its quantiles measure the "
            "arrival rate and the VU pool rather than levee. The fix is a lower rate for that "
            "payload size in rate_for_payload, sized from measured capacity, never a wider "
            "band. Throughput on the enforced path is RETROGRADE past its knee, so a cell that "
            "misses its demand will miss it by more when given a bigger pool, not less"
        )
        report.verdict("RATE", False, detail)
        return

    detail = (
        f"every cell served at least {floor * 100:.0f} percent of its demanded arrival rate, "
        + ", ".join(observations)
    )
    report.verdict("RATE", True, detail)


def report_cost(report: Report, cells: list[Cell]) -> None:
    """Print levee CPU cost per request, so saturation is self-evident.

    A REPORT and not a gate. The gate on saturation is the achieved-rate check above,
    which measures the consequence. This measures the cause, so a reader never has to
    infer saturation from the shape of a latency curve.
    """
    measured = [cell for cell in cells if cell.cpu_milliseconds_per_request is not None]
    if not measured:
        report.line(
            "COST levee CPU per request was not sampled in this run, so saturation cannot be "
            "read off the artifact and has to be inferred from the achieved rates above"
        )
        report.line()
        return

    report.line(
        "COST levee CPU per request, from ps cputime deltas across the steady window, with the "
        "VU pool headroom each cell actually needed. Direct cells are absent from the CPU "
        "columns because no levee is in their path"
    )
    report.line(
        "  {:<38} {:>9} {:>11} {:>11} {:>9} {:>8} {:>9}".format(
            "cell", "cpu_s", "cpu_ms/req", "demand_rps", "cores", "vus_need", "headroom"
        )
    )
    for cell in sorted(measured, key=lambda item: item.name):
        # Cores is the honest saturation number, and a cell approaching the host's core
        # count in the MANIFEST is at its knee whatever its latency says.
        cores = (cell.cpu_milliseconds_per_request or 0.0) * cell.rate / 1000.0
        # vus_need is Little's Law on the MEASURED median, so it is the concurrency the
        # cell genuinely required rather than a pre-declared estimate. It answers whether
        # one pool can deliver every per-payload rate without dropping, measured after the
        # fact rather than asserted before it.
        vus_needed = cell.rate * cell.percentile(50) / 1000.0
        headroom = (cell.max_vus / vus_needed) if vus_needed > 0 else float("inf")
        report.line(
            "  {:<38} {:>9.2f} {:>11.4f} {:>11g} {:>9.2f} {:>8.2f} {:>8.1f}x".format(
                cell.name,
                cell.cpu_seconds if cell.cpu_seconds is not None else float("nan"),
                cell.cpu_milliseconds_per_request or 0.0,
                cell.rate,
                cores,
                vus_needed,
                headroom,
            )
        )
    report.line(
        "  cores is cpu_ms/req times demand_rps over 1000, the count of cores the cell kept "
        "busy. Against hw.ncpu in the MANIFEST it says how close the cell ran to its knee"
    )
    report.line(
        "  vus_need is Little's Law on the measured P50, demand_rps times service time, so it "
        "is the concurrency the cell really needed. headroom is the 40-slot pool over it. A "
        "headroom near 1 means the pool is about to become part of what is measured"
    )
    report.line()


def band1_group_key(cell: Cell) -> tuple[bool, int]:
    """Return the direct-cell group one cell belongs to.

    Both terms are read from the cell SUMMARY rather than parsed out of the name, because
    direct-payload-4096 and direct-payload-32768 do not carry their response mode.
    """
    return (cell.stream, cell.prompt_bytes)


def band1_group_label(key: tuple[bool, int]) -> str:
    return ("streaming" if key[0] else "non-streaming") + f" {key[1]}B"


def band1_thresholds(cell: Cell) -> tuple[float, float] | None:
    """Return one direct cell's P50 gate and P99 advisory, or None if uncalibrated.

    None is not an error state to be smoothed over. A cell whose group has no calibrated
    pair would otherwise borrow a neighbouring group's ceiling, so the caller fails the
    band with the group named instead.
    """
    return BAND1_DIRECT_GROUP_THRESHOLDS.get(band1_group_key(cell))


def band1_by_group(
    entries: dict[tuple[bool, int], list[str]], threshold: dict[tuple[bool, int], float]
) -> str:
    """Render per-cell readings grouped by direct-cell group, naming each threshold.

    Band 1 judges several groups against different numbers, so a flat list against a
    single stated threshold would leave a reader unable to tell which number judged which
    cell. Groups appear in the order the threshold table declares them, so the reading
    order is stable across directories whose matrices differ.
    """
    parts = []
    ordered = list(BAND1_DIRECT_GROUP_THRESHOLDS) + [
        key for key in entries if key not in BAND1_DIRECT_GROUP_THRESHOLDS
    ]
    for key in ordered:
        if not entries.get(key):
            continue
        limit = threshold.get(key)
        label = band1_group_label(key)
        stated = f"{label} (against {limit}ms)" if limit is not None else f"{label} (UNCALIBRATED)"
        parts.append(stated + " " + ", ".join(entries[key]))
    return ". ".join(parts)


def band1_contention_note(cell: Cell, contention: Contention) -> str:
    """Annotate a FAILING direct cell that the ledger recorded as contended.

    This annotation can only ever be ADDED to a failure, never subtracted from one. Band 1
    judges the floor a run's proxied numbers sit on, and a floor measured on a busy machine
    is still that floor, so contention here is context rather than exoneration. Most direct
    groups are single-instance cells anyway, with nothing to drop them in favour of.
    """
    notes = contention.notes.get(cell.name)
    if not notes:
        return ""
    return (
        " [MEASURED DURING A RECORDED CONTENTION BREACH, "
        + ", ".join(notes)
        + ", which is context and NOT an excuse, the failure stands]"
    )


def check_band1(report: Report, cells: list[Cell], contention: Contention) -> None:
    direct = [cell for cell in cells if cell.role == "direct"]
    if not direct:
        report.verdict("BAND1", False, "no direct-to-mock cell in this run, the band cannot be evaluated")
        return
    offenders = []
    uncalibrated = []
    median_observations: dict[tuple[bool, int], list[str]] = {}
    tail_observations: dict[tuple[bool, int], list[str]] = {}
    loud_tails: dict[tuple[bool, int], list[str]] = {}
    gates: dict[tuple[bool, int], float] = {}
    advisories: dict[tuple[bool, int], float] = {}
    contended_offenders = False
    for cell in sorted(direct, key=lambda item: item.name):
        key = band1_group_key(cell)
        pair = band1_thresholds(cell)
        central = cell.percentile(50)
        tail = cell.percentile(99)
        median_observations.setdefault(key, []).append(f"{cell.name} {central:.3f}")
        tail_observations.setdefault(key, []).append(f"{cell.name} {tail:.3f}")
        if pair is None:
            uncalibrated.append(f"{cell.name} in group {band1_group_label(key)}")
            continue
        gate, advisory = pair
        gates[key] = gate
        advisories[key] = advisory
        if central >= gate:
            note = band1_contention_note(cell, contention)
            contended_offenders = contended_offenders or bool(note)
            offenders.append(f"{cell.name} {central:.3f}ms against its {gate}ms ceiling{note}")
        if tail >= advisory:
            loud_tails.setdefault(key, []).append(f"{cell.name} {tail:.3f}ms")
    observed = band1_by_group(median_observations, gates)
    tails = band1_by_group(tail_observations, advisories)
    detail = (
        "direct P50 below its per-group ceiling, one calibrated pair per response mode "
        "and payload size. Observed: "
        + observed
        + ". Direct P99 recorded for the figures and for anyone applying the original "
        "pre-registered form of this band: "
        + tails
    )
    if offenders or uncalibrated:
        reasons = []
        if offenders:
            reasons.append(
                "direct P50 must be below its per-group ceiling, over budget at "
                + ", ".join(offenders)
                + ". A raised central tendency on a cell with no proxy in the path means the "
                "box or the mock is the bottleneck, so no proxied number in this run means "
                "anything"
            )
            # Scoped to the P50 offenders on purpose. Whether a cell was measured during
            # a dip is a live question about a reading that came in over budget, and it
            # is not a question at all about a group that has no threshold to be over.
            if contended_offenders:
                reasons.append(
                    "One or more failing cells are annotated above as contended. That "
                    "annotation exists so a reader can tell a bottleneck from a contended "
                    "sample, and it changes nothing: band 1 judges the floor this run's "
                    "proxied numbers sit on, and a floor measured on a busy machine is "
                    "still that floor"
                )
            elif not contention.recorded:
                reasons.append(
                    "This directory has no "
                    + CONTENDED_CELLS_FILENAME
                    + ", so whether these cells were measured during a host contention dip "
                    "is UNKNOWN rather than answered no"
                )
        if uncalibrated:
            reasons.append(
                "no calibrated band 1 threshold exists for "
                + ", ".join(uncalibrated)
                + ". A direct cell whose group has never been calibrated is NOT judged "
                "against a neighbouring group's ceiling, because a threshold applied to a "
                "quantity it was not derived from is the defect this band was amended twice "
                "to remove. Calibrate the group from its own readings and add it to "
                "BAND1_DIRECT_GROUP_THRESHOLDS"
            )
        detail = (
            ". ".join(reasons)
            + ". Every direct P50: "
            + observed
            + ". Direct P99 alongside it: "
            + tails
        )
    report.verdict("BAND1", not (offenders or uncalibrated), detail)
    # An advisory, deliberately not a gate. A loud tail on a direct cell is host noise
    # rather than a property of levee. It still gets said out loud, because a reader
    # comparing two evidence directories needs to know which was measured on a busy box.
    if any(loud_tails.values()):
        report.line(
            "BAND1 ADVISORY direct P99 above its per-group advisory threshold at "
            + band1_by_group(loud_tails, advisories)
            + ". This is host tail noise, not a bottleneck, and it does not invalidate the "
            "run. Check cpu_idle_pct in machine-state.txt, which is the field that "
            "discriminates a contended host. Read loadavg there for context only: it was "
            "PROVEN not to discriminate, sitting at 4.0 to 6.4 on the invalidated 43-cell run "
            "and 2.8 to 5.0 on the quiet re-measurements that corrected it. On the reference "
            "host the resident endpoint-security agents and the Spotlight indexer produce "
            "tens-of-milliseconds scheduler stalls that land in every cell including the "
            "direct ones"
        )


def check_band2(report: Report, cells: list[Cell], contention: Contention) -> None:
    direct = select(cells, "direct", stream=False, prompt_bytes=SMALL_PAYLOAD_BYTES)
    measured = select(cells, "passthrough", stream=False, prompt_bytes=SMALL_PAYLOAD_BYTES)
    if not direct or not measured:
        report.verdict(
            "BAND2",
            False,
            f"needs direct and passthrough non-streaming cells at {SMALL_PAYLOAD_BYTES}B, "
            f"found {len(direct)} direct and {len(measured)} passthrough",
        )
        return

    # Only the passthrough arm carries repetition ordinals here. The direct arm at
    # this payload is the two drift canaries, which run once each, so they are the
    # single-instance case and stay in the baseline whatever their host state was.
    usable = usable_repetitions([measured], contention)
    if not usable.evaluable:
        report.verdict("BAND2", False, f"at {SMALL_PAYLOAD_BYTES}B, {usable.cause}")
        return
    passthrough = keep_clean(measured, usable)
    baseline = statistics.median([cell.percentile(50) for cell in direct])
    shifts = [(cell.name, cell.percentile(50) - baseline) for cell in passthrough]
    median_shift = statistics.median([shift for _, shift in shifts])
    ok = (
        BAND2_PASSTHROUGH_SHIFT_MIN_MILLISECONDS
        <= median_shift
        <= BAND2_PASSTHROUGH_SHIFT_MAX_MILLISECONDS
    )
    detail = (
        f"passthrough minus direct P50 median {median_shift:.3f}ms, window "
        f"{BAND2_PASSTHROUGH_SHIFT_MIN_MILLISECONDS} to "
        f"{BAND2_PASSTHROUGH_SHIFT_MAX_MILLISECONDS}ms, direct baseline P50 "
        f"{baseline:.3f}ms from {len(direct)} cells, per cell "
        + ", ".join(f"{name} {shift:+.3f}" for name, shift in shifts)
    )
    if usable.note:
        detail += f". {usable.note}"
    if not ok:
        detail += (
            ". Below the floor means the cell did not traverse the proxy. Above the "
            "ceiling means something beyond the extra loopback hop is being paid"
        )
    report.verdict("BAND2", ok, detail)


def enforcement_arms(
    cells: list[Cell], stream: bool, prompt_bytes: int
) -> tuple[list[Cell], list[Cell]]:
    return (
        select(cells, "passthrough", stream=stream, prompt_bytes=prompt_bytes),
        select(cells, "enforce", stream=stream, prompt_bytes=prompt_bytes),
    )


def enforcement_shifts(
    cells: list[Cell],
    quantile: float,
    stream: bool = False,
    prompt_bytes: int = SMALL_PAYLOAD_BYTES,
    usable: RepetitionSet | None = None,
) -> list[tuple[str, float]]:
    """Return the per-repetition enforce minus passthrough shifts at one quantile.

    usable is the contended-repetition filter. A caller that gates on both the P50 and
    the P99 of the same pairing MUST pass the same RepetitionSet to both, or band 4
    would divide a tail shift measured over one set of repetitions by a median shift
    measured over another.
    """
    passthrough, enforce = enforcement_arms(cells, stream, prompt_bytes)
    if usable is not None:
        passthrough = keep_clean(passthrough, usable)
        enforce = keep_clean(enforce, usable)
    return paired_shifts(passthrough, enforce, quantile)


def check_band3(report: Report, cells: list[Cell], usable: RepetitionSet) -> float:
    """Evaluate the PRIMARY enforcement gate and return its median P50 shift.

    The 150B reading is still computed and printed by report_band3_small below and no
    longer gates. Band 4 divides by the value returned here, so it receives the SAME
    RepetitionSet from the caller.
    """
    payload = BAND3_PRIMARY_PAYLOAD_BYTES
    if not usable.evaluable:
        report.verdict("BAND3", False, f"at {payload}B, {usable.cause}")
        return 0.0
    shifts = enforcement_shifts(cells, 50, prompt_bytes=payload, usable=usable)
    if not shifts:
        report.verdict(
            "BAND3",
            False,
            f"no repetition-matched enforce and passthrough pair at {payload}B, so the "
            "PRIMARY enforcement gate has no cells to read and this run cannot be "
            f"published. Include {payload} in PROMPT_SIZES. A payload-restricted run is a "
            "capacity check rather than a validity check, and this verdict is the correct "
            "outcome for one rather than a harness fault",
        )
        return 0.0
    values = [shift for _, shift in shifts]
    median_shift = statistics.median(values)
    spread = max(values) - min(values)
    in_window = (
        BAND3_PRIMARY_SHIFT_MIN_MILLISECONDS
        <= median_shift
        <= BAND3_PRIMARY_SHIFT_MAX_MILLISECONDS
    )
    if len(values) >= 2:
        spread_resolved = spread < median_shift
        spread_detail = spread_phrase(values)
    else:
        # One repetition gives a spread of zero, which would pass the resolution
        # check by arithmetic without measuring anything. Say so rather than
        # letting a vacuous pass read as a real one.
        spread_resolved = True
        spread_detail = (
            "spread not evaluated, a single repetition cannot resolve it, "
            "this check is vacuous in quick mode"
        )
    ok = in_window and spread_resolved
    detail = (
        f"PRIMARY enforcement gate at {payload}B, enforce minus passthrough P50 median "
        f"{median_shift:.3f}ms, window {BAND3_PRIMARY_SHIFT_MIN_MILLISECONDS} to "
        f"{BAND3_PRIMARY_SHIFT_MAX_MILLISECONDS}ms, {spread_detail}, per repetition "
        + ", ".join(f"{label} {shift:+.3f}" for label, shift in shifts)
    )
    if usable.note:
        # A dropped repetition also narrows the spread the resolution check reads,
        # which makes that check easier to pass. Said out loud rather than left for a
        # reader to notice, since it is the one place the exclusion loosens something.
        detail += (
            f". {usable.note}, so both the median and the spread above are over the "
            "clean repetitions only"
        )
    if median_shift < BAND3_PRIMARY_SHIFT_MIN_MILLISECONDS:
        detail += (
            ". Below the floor means the enforce arm was probably not enforcing, so check "
            "that the rendered config and the agent header reached it, and compare against "
            "the A/A control below: a non-enforcing arm reads what that control reads, which "
            "is single-digit microseconds rather than hundreds"
        )
    elif median_shift > BAND3_PRIMARY_SHIFT_MAX_MILLISECONDS:
        detail += (
            ". Above the ceiling means the enforcement path got materially more expensive, "
            "and the first thing to check is the number of tokenizer passes, since one "
            "extra pass at this payload size is roughly 340us on its own"
        )
    elif not spread_resolved:
        detail += (
            ". The spread exceeds the signal, so this run cannot resolve the "
            "enforcement cost. More repetitions are needed, not a wider band"
        )
    report.verdict("BAND3", ok, detail)
    return median_shift


def report_band3_small(report: Report, cells: list[Cell], contention: Contention) -> None:
    """Print the 150B enforcement reading, with an advisory and no gate.

    Every number the gated form printed is still printed, so a reader can apply the
    original band by hand. What is gone is its power to invalidate a run, so too few clean
    repetitions makes this section say so and STOP rather than fail: giving it that power
    through the contention path would reverse a decision made deliberately.
    """
    usable = usable_repetitions(
        list(enforcement_arms(cells, False, SMALL_PAYLOAD_BYTES)), contention
    )
    if not usable.evaluable:
        report.line(
            f"BAND3-SMALL UNEVALUABLE at {SMALL_PAYLOAD_BYTES}B, {usable.cause}. This "
            "is a record and not a gate, so it does not invalidate the run, and the "
            "small-payload enforcement reading is simply unavailable from it"
        )
        return
    shifts = enforcement_shifts(cells, 50, prompt_bytes=SMALL_PAYLOAD_BYTES, usable=usable)
    tail_shifts = enforcement_shifts(
        cells, 99, prompt_bytes=SMALL_PAYLOAD_BYTES, usable=usable
    )
    if not shifts:
        report.line(
            f"BAND3-SMALL absent, no repetition-matched enforce and passthrough pair at "
            f"{SMALL_PAYLOAD_BYTES}B in this run, so the recorded small-payload "
            "enforcement reading is unavailable. This is a record and not a gate, so it "
            "does not invalidate the run"
        )
        return

    values = [shift for _, shift in shifts]
    median_shift = statistics.median(values)
    inside = (
        BAND3_SMALL_SHIFT_ADVISORY_MIN_MILLISECONDS
        <= median_shift
        <= BAND3_SMALL_SHIFT_ADVISORY_MAX_MILLISECONDS
    )
    tail_note = ""
    if tail_shifts:
        tail_median = statistics.median([shift for _, shift in tail_shifts])
        tail_note = (
            f". P99 shift median {tail_median:+.3f}ms at this payload, printed because band 4 "
            "no longer gates it, context only"
        )
    report.line(
        f"BAND3-SMALL RECORDED enforce minus passthrough P50 median {median_shift:+.3f}ms at "
        f"{SMALL_PAYLOAD_BYTES}B, {spread_phrase(values)}, advisory window "
        f"{BAND3_SMALL_SHIFT_ADVISORY_MIN_MILLISECONDS} to "
        f"{BAND3_SMALL_SHIFT_ADVISORY_MAX_MILLISECONDS}ms, per repetition "
        + ", ".join(f"{label} {shift:+.3f}" for label, shift in shifts)
        + tail_note
        + (f". {usable.note}" if usable.note else "")
    )
    if not inside:
        report.line(
            f"BAND3-SMALL ADVISORY the {median_shift:+.3f}ms reading is outside the "
            f"{BAND3_SMALL_SHIFT_ADVISORY_MIN_MILLISECONDS} to "
            f"{BAND3_SMALL_SHIFT_ADVISORY_MAX_MILLISECONDS}ms window that the component "
            "decomposition says inherent enforcement work can produce at this payload, which "
            "is 38.2us of gross enforce-only work against a 24us shared-path credit for a net "
            "near +15us. A reading well above that window is host contention rather than "
            "levee: three background busy loops reproduce exactly this, moving the same "
            "measurement to +93 and +109us while every integrity gate still reads clean. "
            "Check cpu_idle_pct in machine-state.txt and the A/A control below, which reads "
            "the noise the same estimator invents when the true answer is zero. THIS READING "
            "MUST NOT BE PUBLISHED as the enforcement cost at this payload. It does NOT "
            "invalidate the run, because the gate that decides enforcement cost is at "
            f"{BAND3_PRIMARY_PAYLOAD_BYTES}B where the signal is roughly 160 times the "
            "estimator noise floor rather than 4 times it, and a run whose "
            f"{BAND3_PRIMARY_PAYLOAD_BYTES}B gate passes while this advisory fires is a run "
            "with a sound enforcement measurement and an unusable small-payload one"
        )
    if median_shift < 0:
        report.line(
            "BAND3-SMALL NOTE a negative reading here is expected rather than alarming. "
            "Gross enforce-only work at this payload is 38.2us and the shared path runs 24us "
            "FASTER in the enforce arm, so the net is a small difference between two larger "
            "terms. Removing one of the two tokenizer passes takes gross work to 20.8us "
            "against the same credit, which puts the net near -3us. Enforcement did not "
            "become free, and the work itself is measured by the "
            f"{BAND3_PRIMARY_PAYLOAD_BYTES}B gate and by microbench.txt, neither of which "
            "can go negative"
        )


def report_control_pair(report: Report, cells: list[Cell], contention: Contention) -> None:
    """Print the A/A control shift, whose TRUE VALUE IS ZERO.

    A REPORT and never a gate, for the reasons at CONTROL_A_ROLE. It sits beside the
    enforcement readings so the noise floor and the signal it qualifies share a screen.
    Contended repetitions are excluded because the control's job is to say what the
    estimator invents at a known zero ON THE HOST THIS RUN WAS MEASURED ON. Too few clean
    repetitions makes it unevaluable and does NOT fail the run.
    """
    all_arm_a = select(cells, CONTROL_A_ROLE, stream=False, prompt_bytes=SMALL_PAYLOAD_BYTES)
    all_arm_b = select(cells, CONTROL_B_ROLE, stream=False, prompt_bytes=SMALL_PAYLOAD_BYTES)
    usable = usable_repetitions([all_arm_a, all_arm_b], contention)
    if all_arm_a and all_arm_b and not usable.evaluable:
        report.line(
            f"CONTROL-AA UNEVALUABLE, {usable.cause}. This is a control and never a "
            "gate, so it does not invalidate the run. What is lost is the estimator "
            "noise floor measured in the same run that publishes a number, which then "
            "has to be taken from the quiet-host readings of -1, +4 and 0us"
        )
        return
    arm_a = keep_clean(all_arm_a, usable)
    arm_b = keep_clean(all_arm_b, usable)
    if not arm_a or not arm_b:
        report.line(
            "CONTROL-AA absent, this run has no passthrough-versus-passthrough control pair, "
            "so the estimator's noise floor is not measured in the same run that publishes a "
            "number and has to be taken from the quiet-host readings of -1, +4 and 0us. Any "
            "directory written before the control was added to the matrix reads this way"
        )
        return

    shifts = paired_shifts(arm_a, arm_b, 50)
    if not shifts:
        report.line(
            "CONTROL-AA present but unpaired, the two control arms share no repetition "
            "ordinal, so no shift can be computed"
        )
        return

    values = [shift for _, shift in shifts]
    median_shift = statistics.median(values)
    report.line(
        f"CONTROL-AA RECORDED passthrough minus passthrough P50 median {median_shift:+.3f}ms, "
        f"{spread_phrase(values)}, per repetition "
        + ", ".join(f"{label} {shift:+.3f}" for label, shift in shifts)
        + (f". {usable.note}" if usable.note else "")
    )
    report.line(
        "  This is a CONTROL and its EXPECTED VALUE IS ZERO. Both arms run the same "
        "passthrough config at the same payload and rate, with the same levee restart and "
        "TIME_WAIT drain between them as the enforcement pair, so whatever it reads is noise "
        "the estimator invented rather than work levee did. Read it as the resolution limit "
        "of every shift printed above it"
    )
    report.line(
        "  It is NOT a gate, and a small reading here does NOT certify a quiet host. On a "
        "quiet host this control reads -1, +4 and 0us, so the floor is about 4us. Under the "
        "three-busy-loop contention that reproduced the invalidated 43-cell run it still read "
        "13us, which is a true zero reported as 13us. So the control is necessary and not "
        "sufficient, and the check that refuses a contended host is the cpu_idle_pct floor in "
        "run.sh rather than this line"
    )


def check_band4(
    report: Report, cells: list[Cell], median_p50_shift: float, usable: RepetitionSet
) -> None:
    payload = BAND3_PRIMARY_PAYLOAD_BYTES
    if not usable.evaluable:
        report.verdict(
            "BAND4",
            False,
            f"at {payload}B, {usable.cause}. This band is a ratio against band 3's "
            "median at the same payload size, so it is unevaluable for exactly the "
            "reason band 3 is",
        )
        return
    shifts = enforcement_shifts(cells, 99, prompt_bytes=payload, usable=usable)
    if not shifts:
        report.verdict(
            "BAND4",
            False,
            f"no repetition-matched enforce and passthrough pair at {payload}B, and this band "
            "follows band 3 to that payload size because it is a ratio against band 3's median, "
            "so it cannot be evaluated",
        )
        return
    values = [shift for _, shift in shifts]
    median_tail_shift = statistics.median(values)
    if median_p50_shift > 0:
        allowance = BAND4_TAIL_SHIFT_RATIO_MAX * median_p50_shift
        basis = f"{BAND4_TAIL_SHIFT_RATIO_MAX:g} times the median P50 shift of {median_p50_shift:.3f}ms"
    else:
        # A ratio against a zero or negative median shift is undefined, so the
        # band falls back to the same multiple of band 3's own ceiling. That is
        # the widest the ratio form could ever have allowed while band 3 passed,
        # so the fallback can never be looser than the rule it stands in for.
        allowance = BAND4_TAIL_SHIFT_RATIO_MAX * BAND3_PRIMARY_SHIFT_MAX_MILLISECONDS
        basis = (
            f"{BAND4_TAIL_SHIFT_RATIO_MAX:g} times the band 3 ceiling of "
            f"{BAND3_PRIMARY_SHIFT_MAX_MILLISECONDS}ms, because the median P50 shift of "
            f"{median_p50_shift:.3f}ms is not positive and a ratio against it is undefined"
        )
    ok = median_tail_shift <= allowance
    detail = (
        f"at {payload}B, enforce minus passthrough P99 median {median_tail_shift:.3f}ms against an "
        f"allowance of {allowance:.3f}ms, which is {basis}, per repetition "
        + ", ".join(f"{label} {shift:+.3f}" for label, shift in shifts)
        + (f". {usable.note}" if usable.note else "")
    )
    if not ok:
        detail += (
            ". A tail shift this far above the median shift means one cell caught a "
            "transient that every median gate missed"
        )
    report.verdict("BAND4", ok, detail)


def check_band3_stream(report: Report, cells: list[Cell], contention: Contention) -> None:
    """Evaluate the streaming companion to band 3.

    The gate is on the absolute size of the shift and is deliberately wide. The central
    value is RECORDED and ADVISED on rather than gated, for the reasons at
    BAND3_STREAM_SHIFT_MAX_ABSOLUTE_MILLISECONDS above.

    This band alone passes STREAM_MINIMUM_CLEAN_REPETITIONS rather than the usual
    minimum, argued at that constant. Zero clean repetitions still fails.
    """
    usable = usable_repetitions(
        list(enforcement_arms(cells, True, SMALL_PAYLOAD_BYTES)),
        contention,
        minimum=STREAM_MINIMUM_CLEAN_REPETITIONS,
    )
    if not usable.evaluable:
        report.verdict("BAND3-STREAM", False, usable.cause)
        return
    shifts = enforcement_shifts(cells, 50, stream=True, usable=usable)
    if not shifts:
        report.verdict(
            "BAND3-STREAM",
            False,
            f"no repetition-matched streaming enforce and passthrough pair at "
            f"{SMALL_PAYLOAD_BYTES}B, the band cannot be evaluated",
        )
        return
    values = [shift for _, shift in shifts]
    median_shift = statistics.median(values)
    ceiling = BAND3_STREAM_SHIFT_MAX_ABSOLUTE_MILLISECONDS
    ok = abs(median_shift) <= ceiling

    tail_shifts = enforcement_shifts(cells, 99, stream=True, usable=usable)
    if tail_shifts:
        tail_note = (
            f". Streaming P99 shift median "
            f"{statistics.median([shift for _, shift in tail_shifts]):.3f}ms, context only, "
            "no gate"
        )
    else:
        tail_note = ""

    detail = (
        f"streaming enforce minus passthrough P50 median {median_shift:+.3f}ms against a "
        f"two-sided ceiling of {ceiling}ms on its absolute size, per repetition "
        + ", ".join(f"{label} {shift:+.3f}" for label, shift in shifts)
        + tail_note
        + (f". {usable.note}" if usable.note else "")
    )
    if not ok:
        detail += (
            ". A shift this large in either direction is beyond what the measured inherent "
            "streaming enforcement cost plus the known drift envelope on this host can "
            "produce. Positive means a real regression in the estimate, admit, reconcile "
            "path. Negative means the enforce cell probably was not enforcing, so check "
            "that the rendered config and the agent header reached it"
        )
    report.verdict("BAND3-STREAM", ok, detail)

    # The thin-median advisory. The ceiling above still applies over whatever repetitions
    # survived, because it is wide enough to be meaningful on one. The MEDIAN is a
    # different matter, so a run that LOST streaming repetitions to contention says so
    # loudly rather than printing a thinner number that looks like a full one. It fires
    # only when contention actually cost repetitions, not merely when the matrix ran fewer
    # than three: quick mode runs one by design and the detail line above already says the
    # spread is not resolvable.
    if usable.dropped and len(usable.kept) < MINIMUM_CLEAN_REPETITIONS:
        survivors = (
            "1 clean streaming repetition"
            if len(usable.kept) == 1
            else f"{len(usable.kept)} clean streaming repetitions"
        )
        report.line(
            f"BAND3-STREAM THIN MEDIAN this ceiling was applied over {survivors}, below "
            f"the {MINIMUM_CLEAN_REPETITIONS} that a median needs to be an order "
            "statistic over independent measurements. The GATE still holds, because it is "
            "a two-sided ceiling on absolute size that fires on roughly a 40-fold "
            "regression or on an enforce arm that was not enforcing, and both are visible "
            "in a single repetition. The printed median is correspondingly weaker and must "
            "not be quoted as the streaming enforcement cost, which this band already "
            "refuses to publish in any case. Before 2026-09-17 this situation FAILED the "
            "run outright, at the end of the matrix, which is why it changed. The remedy is "
            "more streaming repetitions in the matrix rather than a different number here"
        )

    # The advisory, deliberately not a gate. Inherent enforcement work at this
    # payload nets +15us by the component decomposition, so a reading outside the
    # 150B advisory window is drift rather than work, and a reader who quotes it as
    # the streaming enforcement cost would be quoting host noise.
    if not (
        BAND3_SMALL_SHIFT_ADVISORY_MIN_MILLISECONDS
        <= median_shift
        <= BAND3_SMALL_SHIFT_ADVISORY_MAX_MILLISECONDS
    ):
        report.line(
            f"BAND3-STREAM ADVISORY the {median_shift:+.3f}ms streaming shift is outside the "
            f"{BAND3_SMALL_SHIFT_ADVISORY_MIN_MILLISECONDS} to "
            f"{BAND3_SMALL_SHIFT_ADVISORY_MAX_MILLISECONDS}ms window the component "
            "decomposition says inherent enforcement work can produce at this payload. This "
            "reading is drift-dominated and must NOT be published as the streaming "
            "enforcement cost. It "
            "does not invalidate the run, because the matrix runs one streaming repetition "
            "with no streaming drift canary and so cannot resolve a shift this small. Check "
            "cpu_idle_pct in machine-state.txt, which is the field that discriminates a "
            "contended host, and the A/A control for what the estimator invents at a true zero"
        )


def check_band5(report: Report, cells: list[Cell]) -> None:
    opening = [cell for cell in cells if "canary-open" in cell.name]
    closing = [cell for cell in cells if "canary-close" in cell.name]
    if len(opening) != 1 or len(closing) != 1:
        report.verdict(
            "BAND5",
            False,
            f"expected exactly one opening and one closing drift canary, found "
            f"{len(opening)} and {len(closing)}. A run missing a canary cannot be "
            "checked for drift and is not evidence",
        )
        return
    open_cell, close_cell = opening[0], closing[0]
    observations = []
    offenders = []
    # Both quantiles are absolute gates with their own ceiling, evaluated the same way
    # because they ask the same question at different parts of the distribution. The two
    # ceilings differ by an order of magnitude, and that gap is the point: the central
    # tendency should barely move across a matrix, while a tail on this host legitimately
    # moves by hundreds of microseconds without the experiment being compromised.
    for label, quantile, ceiling in (
        ("P50", 50, BAND5_CANARY_DRIFT_MAX_MILLISECONDS),
        ("P99", 99, BAND5_CANARY_TAIL_DRIFT_MAX_MILLISECONDS),
    ):
        first = open_cell.percentile(quantile)
        last = close_cell.percentile(quantile)
        drift = abs(last - first)
        # The percentage is kept in the ratio form the ORIGINAL band gated on, so a figure
        # printed here stays comparable to the historical readings tabulated under
        # "Band 5, the drift canary" in calibration.md.
        smaller = min(first, last)
        fraction = (max(first, last) / smaller) - 1.0 if smaller > 0 else float("inf")
        observations.append(
            f"{label} {first:.3f} then {last:.3f}, absolute drift {drift:.3f}ms against a "
            f"ceiling of {ceiling}ms, which is {fraction * 100:.1f} percent of the smaller "
            "cell and is context rather than a gate"
        )
        if drift > ceiling:
            offenders.append(f"{label} drifted {drift:.3f}ms past its {ceiling}ms ceiling")
    detail = "opening and closing direct canaries, " + ". ".join(observations)
    if offenders:
        detail += (
            ". "
            + ", ".join(offenders)
            + ". A drift this size means the machine moved underneath the experiment by as "
            "much as the effect being measured, so no cell can be compared to any other "
            "and the whole run is invalid. Look for a thermal event or a background job "
            "that arrived mid run and stayed, in the per-cell cpu_idle_pct readings in "
            "machine-state.txt. An evidence run refuses to continue below the idle floor "
            "recorded there, so a run that reached this band without that refusal drifted "
            "for some reason other than sustained CPU contention"
        )
    report.verdict("BAND5", not offenders, detail)


def main(argv: list[str]) -> int:
    if len(argv) != 2:
        print("usage: check_bands.py <results-directory>", file=sys.stderr)
        return 2
    results_dir = argv[1]

    report = Report()
    # Only the directory name is printed. The absolute path is a home directory
    # path, and the harness audits this file for exactly that.
    report.line(f"pre-registered sanity band evaluation for {os.path.basename(results_dir)}")
    report.line(f"checker python {sys.version.split()[0]}")
    report.line()

    try:
        cells = load_cells(results_dir)
    except (OSError, ValueError, json.JSONDecodeError) as error:
        print(f"VERDICT INVALID could not load the run, {error}")
        return 1

    contention = read_contention(results_dir)

    report_inventory(report, cells)
    report_cost(report, cells)
    # The contention list comes before every gate that acts on it, so a reader sees
    # which repetitions were dropped before seeing the medians they were dropped out of.
    report_contention(report, cells, contention)
    check_integrity(report, cells)
    check_achieved_rate(report, cells)
    check_band1(report, cells, contention)
    check_band2(report, cells, contention)
    # Band 3 and band 4 share ONE repetition filter, resolved here rather than inside
    # either of them. Band 4 divides its tail shift by band 3's median shift, so the
    # two must be computed over the same repetitions or the ratio is arithmetic between
    # two different populations.
    primary_usable = usable_repetitions(
        list(enforcement_arms(cells, False, BAND3_PRIMARY_PAYLOAD_BYTES)), contention
    )
    # The order here is the reading order a skeptic needs: the primary gate, then the
    # small-payload reading it was relocated from, then the control that says what the
    # estimator behind both invents at a true zero. The control goes last so the noise
    # floor is on screen underneath every shift it qualifies.
    median_p50_shift = check_band3(report, cells, primary_usable)
    report_band3_small(report, cells, contention)
    report_control_pair(report, cells, contention)
    check_band4(report, cells, median_p50_shift, primary_usable)
    check_band3_stream(report, cells, contention)
    check_band5(report, cells)

    report.line()
    if report.failed:
        report.line("VERDICT INVALID this run violated at least one pre-registered band and is NOT publishable")
        return 1
    report.line("VERDICT VALID every pre-registered band passed")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
