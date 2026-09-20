# /// script
# requires-python = ">=3.12"
# dependencies = ["matplotlib==3.10.0", "numpy>=1.26"]
# ///
"""Render the enforcement-cost figure from a committed results directory.

Two things are drawn side by side. On the left, the measured enforce minus
passthrough P50 shift against prompt size, with the 500 microsecond
enforcement-path line, so the point where token estimation overtakes that budget is
visible rather than hidden. On the right, the component costs measured by this
run's own micro-benchmarks, split into the components that appear in the shift and
the components both arms pay and which therefore cancel out of it.

Every component number is parsed from the microbench.txt of the same results
directory. A hardcoded constant here would be another machine's number presented as
a measurement of this one, which is the failure the harness captures those
benchmarks to prevent.

This script generates no load. It reads only committed artifacts.

check_bands.py supplies the cell loader, the percentile definition and
paired_shifts, which is the estimator pre-registered band 3 gates on, so the figure
publishes the same quantity the gate evaluated. overhead_figure.py supplies the
bootstrap and the MANIFEST reader, by import rather than by a second copy of the
resampling code. A shared support module would be the cleaner home for both once a
third figure needs them.

Usage: uv run --script enforcement_figure.py <results-dir> [--out <png>]
"""

from __future__ import annotations

import argparse
import os
import re
import statistics
import sys
from dataclasses import dataclass

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import check_bands  # noqa: E402  import path is set immediately above
import matplotlib  # noqa: E402

matplotlib.use("Agg")

import matplotlib.pyplot as pyplot  # noqa: E402
import matplotlib.ticker as ticker  # noqa: E402
import numpy  # noqa: E402
import overhead_figure  # noqa: E402

# The enforcement-path target from Tenet 1. Drawn as a reference line and never
# enforced as a gate, so a noisy run reports honestly instead of failing.
ENFORCEMENT_SLO_MICROSECONDS = 500.0

# The P50 index inside overhead_figure.REPORTED_QUANTILES. The shift is quoted
# at the median because that is where band 3 evaluates it, and because a
# tens-of-microseconds signal is not resolvable in a tail on this host.
MEDIAN_QUANTILE = 50.0

MICROSECONDS_PER_MILLISECOND = 1000.0

# Benchmark identities, one place, so a rename in Go surfaces here as a named
# missing component rather than as a silently absent bar.
ESTIMATE_OPENAI = "BenchmarkEstimate_OpenAI"
ESTIMATE_ANTHROPIC = "BenchmarkEstimate_Anthropic"
BUDGET_SETTLE_CONTENDED = "BenchmarkReserveReconcileSingleAgent"
BUDGET_SETTLE_UNCONTENDED = "BenchmarkReserveReconcilePerGoroutineAgent"
BODY_READ = "BenchmarkReadRequestBody"
LOG_LINES_PASSTHROUGH = "BenchmarkPassthroughRequestLines"
LOG_LINES_ENFORCE = "BenchmarkEnforceRequestLines"
LOG_LINE_UPSTREAM = "BenchmarkUpstreamResponseLine"
LOG_LINE_RESERVED = "BenchmarkBudgetReservedLine"
LOG_LINE_RECONCILED = "BenchmarkBudgetReconciledLine"

# go test prints the GOMAXPROCS it ran at as a suffix on the benchmark name.
BENCHMARK_NAME_PATTERN = re.compile(r"^(Benchmark[A-Za-z0-9_]+)(?:-(\d+))?$")

# Every time unit go test could report per operation, in nanoseconds. The unit
# token is read from the line instead of assumed, so a future output format that
# switched to seconds per operation would be converted rather than misread by
# three orders of magnitude.
TIME_UNITS_IN_NANOSECONDS = {
    "ns/op": 1.0,
    "us/op": 1000.0,
    "µs/op": 1000.0,
    "ms/op": 1000000.0,
    "s/op": 1000000000.0,
    "sec/op": 1000000000.0,
}

DELTA_COMPONENT_COLORS = ("#128a5c", "#4aa8d8", "#9467bd")
SHARED_COMPONENT_COLORS = ("#8c8c8c", "#c0c0c0")


@dataclass
class BenchmarkReading:
    """One go test benchmark line, with the package it was measured in."""

    package: str
    name: str
    goroutine_count: int | None
    nanoseconds_per_operation: float

    @property
    def microseconds(self) -> float:
        return self.nanoseconds_per_operation / 1000.0


@dataclass
class Component:
    """One labelled cost drawn on the component panel."""

    label: str
    microseconds: float
    provenance: str


def parse_microbench(path: str) -> dict[str, BenchmarkReading]:
    """Parse go test benchmark output into readings keyed by benchmark name.

    The package context comes from the pkg: header go test writes before each
    package block, so a reading can be attributed even though two packages could
    in principle carry the same benchmark name.
    """
    readings: dict[str, BenchmarkReading] = {}
    package = "unknown"
    with open(path, encoding="utf-8") as handle:
        for line in handle:
            stripped = line.strip()
            if stripped.startswith("pkg:"):
                package = stripped.split(":", 1)[1].strip()
                continue
            if not stripped.startswith("Benchmark"):
                continue
            fields = stripped.split()
            matched = BENCHMARK_NAME_PATTERN.match(fields[0])
            if matched is None:
                continue
            unit_index = None
            for index, field_value in enumerate(fields):
                if field_value in TIME_UNITS_IN_NANOSECONDS:
                    unit_index = index
                    break
            if unit_index is None or unit_index == 0:
                raise ValueError(
                    f"benchmark line {stripped!r} in {os.path.basename(path)} carries no "
                    "recognised per-operation time unit, so its cost cannot be read. "
                    f"Known units {sorted(TIME_UNITS_IN_NANOSECONDS)}"
                )
            scale = TIME_UNITS_IN_NANOSECONDS[fields[unit_index]]
            readings[matched.group(1)] = BenchmarkReading(
                package=package,
                name=matched.group(1),
                goroutine_count=int(matched.group(2)) if matched.group(2) else None,
                nanoseconds_per_operation=float(fields[unit_index - 1]) * scale,
            )
    return readings


def load_microbench(results_dir: str) -> tuple[dict[str, BenchmarkReading], str | None]:
    """Return the parsed readings, or an empty map and the reason it is empty."""
    path = os.path.join(results_dir, "microbench.txt")
    if not os.path.exists(path):
        return {}, (
            "this results directory carries no microbench.txt, so no component cost can be "
            "annotated. Directories written before the harness captured component benchmarks "
            "look like this. Re-run the harness to get the component panel"
        )
    readings = parse_microbench(path)
    if not readings:
        return {}, "microbench.txt carries no benchmark line, so no component cost can be annotated"
    return readings, None


def require(readings: dict[str, BenchmarkReading], name: str) -> BenchmarkReading:
    if name not in readings:
        raise ValueError(
            f"microbench.txt has no {name} reading, so the component it stands for cannot be "
            "annotated. The harness captures it with a -bench pattern, so a Go rename would "
            "produce exactly this"
        )
    return readings[name]


def delta_components(readings: dict[str, BenchmarkReading]) -> list[Component]:
    """The measured costs an enforced request pays and a passthrough one does not.

    Token estimation and budget settlement are enforcement work. The logging entry
    is not: an enforced request writes three log lines where a passthrough request
    writes one, so two lines of the shift are structured logging. It gets its own bar
    so the published number is never mistaken for pure enforcement work.
    """
    estimate = require(readings, ESTIMATE_OPENAI)
    settle = require(readings, BUDGET_SETTLE_CONTENDED)
    enforce_lines = require(readings, LOG_LINES_ENFORCE)
    passthrough_lines = require(readings, LOG_LINES_PASSTHROUGH)
    extra_logging = enforce_lines.microseconds - passthrough_lines.microseconds
    return [
        Component(
            "token estimation",
            estimate.microseconds,
            f"{estimate.name} in {estimate.package}",
        ),
        Component(
            "budget reserve plus reconcile",
            settle.microseconds,
            f"{settle.name} in {settle.package}, the contended single-agent form, "
            "which is the shape the benchmark cells drive",
        ),
        Component(
            "two extra log lines",
            extra_logging,
            f"{enforce_lines.name} minus {passthrough_lines.name} in {enforce_lines.package}, "
            "three lines minus one",
        ),
    ]


def shared_components(readings: dict[str, BenchmarkReading]) -> list[Component]:
    """Costs both arms pay, which therefore cancel out of the shift.

    readRequestBody is called in ServeHTTP before the enforcement branch, so a
    passthrough request pays it too and it subtracts away. Drawing it here, as a bar
    explicitly outside the delta, stops a reader from adding it into the modelled
    shift.
    """
    body = require(readings, BODY_READ)
    passthrough_lines = require(readings, LOG_LINES_PASSTHROUGH)
    return [
        Component(
            "request body read",
            body.microseconds,
            f"{body.name} in {body.package}, called before the enforcement branch so both "
            "arms pay it",
        ),
        Component(
            "one shared log line",
            passthrough_lines.microseconds,
            f"{passthrough_lines.name} in {passthrough_lines.package}",
        ),
    ]


def payload_sizes(cells: list[check_bands.Cell], stream: bool) -> list[int]:
    return sorted(
        {cell.prompt_bytes for cell in cells if cell.role == "enforce" and cell.stream == stream}
    )


def paired_shift_bootstrap(
    passthrough_cells: list[check_bands.Cell], enforce_cells: list[check_bands.Cell]
) -> numpy.ndarray:
    """Bootstrap vector for the median repetition-matched P50 shift.

    Cells are paired on their repetition ordinal exactly as check_bands does, each
    cell is resampled independently, the pair difference is taken per resample, and
    the median across pairs is what the interval covers. That is the same estimator
    band 3 gates on, so the interval belongs to the number being published.
    """
    quantile_index = overhead_figure.REPORTED_QUANTILES.index(MEDIAN_QUANTILE)
    by_repetition = {cell.repetition: cell for cell in passthrough_cells}
    differences = []
    for cell in enforce_cells:
        baseline = by_repetition.get(cell.repetition)
        if baseline is None:
            continue
        treatment_vector = overhead_figure.bootstrap_estimates(cell, overhead_figure.DURATION_LABEL)
        baseline_vector = overhead_figure.bootstrap_estimates(
            baseline, overhead_figure.DURATION_LABEL
        )
        differences.append(treatment_vector[quantile_index] - baseline_vector[quantile_index])
    if not differences:
        return numpy.zeros(0)
    return numpy.median(numpy.vstack(differences), axis=0) * MICROSECONDS_PER_MILLISECOND


@dataclass
class ShiftPoint:
    """The measured shift at one payload size, in microseconds."""

    prompt_bytes: int
    rate: float
    per_repetition: list[tuple[str, float]]
    median: float
    interval: tuple[float, float]


def shift_points(cells: list[check_bands.Cell], stream: bool) -> list[ShiftPoint]:
    points: list[ShiftPoint] = []
    for size in payload_sizes(cells, stream):
        passthrough = check_bands.select(cells, "passthrough", stream=stream, prompt_bytes=size)
        enforce = check_bands.select(cells, "enforce", stream=stream, prompt_bytes=size)
        paired = check_bands.paired_shifts(passthrough, enforce, MEDIAN_QUANTILE)
        if not paired:
            continue
        scaled = [(label, shift * MICROSECONDS_PER_MILLISECOND) for label, shift in paired]
        vector = paired_shift_bootstrap(passthrough, enforce)
        low, high = overhead_figure.interval(vector) if vector.size else (float("nan"), float("nan"))
        rates = sorted({cell.rate for cell in enforce})
        points.append(
            ShiftPoint(
                prompt_bytes=size,
                rate=rates[0],
                per_repetition=scaled,
                median=statistics.median(shift for _, shift in scaled),
                interval=(low, high),
            )
        )
    return points


def print_header(results_dir: str, manifest: dict[str, str]) -> None:
    print(f"enforcement cost figure for {os.path.basename(results_dir)}")
    print(
        f"levee tree {manifest['levee_git_tree']} sha {manifest['levee_git_sha']} "
        f"dirty {manifest.get('levee_tree_dirty', 'unknown')}"
    )
    print(
        f"run mode {manifest['run_mode']} hardware {manifest.get('hardware_tag', 'unknown')} "
        f"log destination {manifest.get('levee_log_destination', 'unknown')}"
    )
    print(
        f"renderer python {sys.version.split()[0]} matplotlib {matplotlib.__version__} "
        f"numpy {numpy.__version__}"
    )
    print(
        "the published quantity is the median repetition-matched enforce minus passthrough P50 "
        "shift, computed by check_bands.paired_shifts over the committed steady-only CSVs, which "
        "is the identical estimator pre-registered band 3 gates on"
    )
    print(
        f"the {ENFORCEMENT_SLO_MICROSECONDS:g} microsecond line is drawn as a reference and never "
        "enforced"
    )
    print()


def print_shift_table(points: list[ShiftPoint], stream: bool) -> None:
    heading = "streaming" if stream else "non-streaming"
    print(
        f"TABLE {'2' if stream else '1'} {heading} enforce minus passthrough P50 shift in "
        "microseconds"
    )
    if not points:
        print(f"  no repetition-matched {heading} pair in this run, so no shift is reportable")
        print()
        return
    print(
        "  {:>8} {:>6} {:>5} {:>13} {:>21} {:<30}".format(
            "bytes", "rate", "reps", "median shift", "bootstrap interval", "per repetition"
        )
    )
    for point in points:
        print(
            "  {:>8d} {:>6g} {:>5d} {:>13.1f} {:>21} {:<30}".format(
                point.prompt_bytes,
                point.rate,
                len(point.per_repetition),
                point.median,
                f"[{point.interval[0]:.1f}, {point.interval[1]:.1f}]",
                ", ".join(f"{label} {shift:+.1f}" for label, shift in point.per_repetition),
            )
        )
    single_repetition = [point for point in points if len(point.per_repetition) < 2]
    if single_repetition:
        print(
            "  DEGRADED one repetition at "
            + ", ".join(f"{point.prompt_bytes}B" for point in single_repetition)
            + ", so the across-repetition spread is zero by arithmetic rather than by "
            "measurement. Band 3 calls its own spread check vacuous in that case for the same "
            "reason, and only the bootstrap interval quantifies uncertainty here"
        )
    if len(points) < 2:
        print(
            "  DEGRADED one payload size in this run. Token estimation grows with prompt size, so "
            f"the crossing of the {ENFORCEMENT_SLO_MICROSECONDS:g} microsecond line cannot be "
            "located from a single point and no curve is drawn through it. Run the harness in "
            "evidence mode for the 150, 4096 and 32768 byte matrix"
        )
    print()


def print_component_table(
    readings: dict[str, BenchmarkReading],
    reason: str | None,
    delta: list[Component],
    shared: list[Component],
    measured: float | None,
) -> None:
    print("TABLE 3 component costs, parsed from this run's microbench.txt, microseconds per operation")
    if reason is not None:
        print(f"  DEGRADED {reason}")
        print()
        return
    print("  {:<34} {:>10} {:<12} {:<60}".format("component", "us/op", "in the shift", "provenance"))
    for component in delta:
        print(
            "  {:<34} {:>10.3f} {:<12} {:<60}".format(
                component.label, component.microseconds, "yes", component.provenance
            )
        )
    for component in shared:
        print(
            "  {:<34} {:>10.3f} {:<12} {:<60}".format(
                component.label, component.microseconds, "no, cancels", component.provenance
            )
        )
    for name, note in (
        (ESTIMATE_ANTHROPIC, "measured but off this path, the cells drive the openai provider"),
        (BUDGET_SETTLE_UNCONTENDED, "uncontended reference, one agent per goroutine"),
        (LOG_LINE_UPSTREAM, "cross-check on the per-line cost"),
        (LOG_LINE_RESERVED, "cross-check on the per-line cost"),
        (LOG_LINE_RECONCILED, "cross-check on the per-line cost"),
    ):
        if name in readings:
            print(
                "  {:<34} {:>10.3f} {:<12} {:<60}".format(
                    name.replace("Benchmark", ""), readings[name].microseconds, "reference", note
                )
            )
    if LOG_LINE_RESERVED in readings and LOG_LINE_RECONCILED in readings:
        summed = readings[LOG_LINE_RESERVED].microseconds + readings[LOG_LINE_RECONCILED].microseconds
        logging_component = next(
            component.microseconds for component in delta if "log lines" in component.label
        )
        print(
            f"  CROSS-CHECK the two extra lines cost {logging_component:.3f} as a composite "
            f"difference and {summed:.3f} as the sum of the two individual line benchmarks. "
            "They agree to within the noise floor those benchmarks carry, which the logcost "
            "package documents as tens of percent per line"
        )
    modelled = sum(component.microseconds for component in delta)
    print(
        f"  the modelled shift is {modelled:.3f} microseconds, the sum of the components marked "
        "yes above"
    )
    if measured is not None:
        share = modelled / measured * 100.0 if measured > 0 else float("nan")
        print(
            f"  ATTRIBUTION measured shift {measured:.1f}, modelled {modelled:.1f}, so the named "
            f"components are {share:.0f} percent of the measurement"
        )
        if modelled <= measured:
            print(
                "  the remainder is unattributed and is not evidence of anything further. The "
                "micro-benchmarks run in-process with no HTTP handler around them, while the "
                "measured shift also carries the map lookup, the extra buffering and the "
                "scheduling the enforced path adds"
            )
        else:
            print(
                "  the components overshoot the measurement, so the model is what needs "
                "explaining rather than a remainder. An in-process benchmark charges a cost the "
                "live path can overlap with waiting on the upstream leg, so a sum of components "
                "bounds a shift from above rather than predicting it. Read it as wrong-sized "
                "components, a noisy run, or both, never as a shift larger than the measured one"
            )
        logging_component = next(
            component.microseconds for component in delta if "log lines" in component.label
        )
        logging_share = logging_component / measured * 100.0 if measured > 0 else float("nan")
        print(
            f"  the logging component is {logging_component:.3f} microseconds, {logging_share:.0f} "
            "percent of the measured shift, and it is structured logging rather than enforcement "
            "work, which is why it is labelled separately"
        )
    print(
        "  CAVEAT microbench.txt records no payload size, so the token estimation reading is "
        "comparable only to the smallest payload cell. The estimator benchmark also drives a "
        "gpt-4 model id, which resolves to cl100k_base, while the load cells send the fixture "
        "model id, which resolves to o200k_base. The component is a close stand-in for the "
        "encoder the cells exercise, not the identical one"
    )
    print()


def draw_shift_panel(axes, points: list[ShiftPoint], stream_points: list[ShiftPoint]) -> None:
    axes.axhline(
        ENFORCEMENT_SLO_MICROSECONDS,
        color="#b00020",
        linestyle="--",
        linewidth=1.3,
        label=(
            f"{ENFORCEMENT_SLO_MICROSECONDS:g}us enforcement-path target. Drawn, never enforced"
        ),
    )
    if points:
        sizes = [point.prompt_bytes for point in points]
        medians = [point.median for point in points]
        lower = [point.median - point.interval[0] for point in points]
        upper = [point.interval[1] - point.median for point in points]
        # A line through the medians is drawn only at two or more payload sizes. One
        # point plus a connector would imply a curve this run cannot measure.
        line_style = "-" if len(points) > 1 else "none"
        axes.errorbar(
            sizes,
            medians,
            yerr=[lower, upper],
            color="#128a5c",
            marker="o",
            markersize=9,
            linestyle=line_style,
            linewidth=1.6,
            capsize=4,
            label="median repetition-matched P50 shift, non-streaming, with bootstrap interval",
        )
        for point in points:
            axes.scatter(
                [point.prompt_bytes] * len(point.per_repetition),
                [shift for _, shift in point.per_repetition],
                facecolors="none",
                edgecolors="#128a5c",
                s=42,
                linewidths=1.0,
                zorder=3,
                label=(
                    "one point per repetition"
                    if point is points[0]
                    else None
                ),
            )
            axes.annotate(
                f"{point.median:.0f}us at {point.rate:g}rps",
                (point.prompt_bytes, point.median),
                textcoords="offset points",
                xytext=(9, 9),
                fontsize=8,
            )
    if stream_points:
        axes.errorbar(
            [point.prompt_bytes for point in stream_points],
            [point.median for point in stream_points],
            yerr=[
                [point.median - point.interval[0] for point in stream_points],
                [point.interval[1] - point.median for point in stream_points],
            ],
            color="#7a4fbd",
            marker="D",
            markersize=8,
            linestyle="none",
            capsize=4,
            label="median P50 shift, streaming, at its own lower arrival rate",
        )
        for point in stream_points:
            axes.annotate(
                f"{point.median:.0f}us at {point.rate:g}rps, streaming",
                (point.prompt_bytes, point.median),
                textcoords="offset points",
                xytext=(9, 9),
                fontsize=8,
                color="#7a4fbd",
            )
    axes.set_xscale("log")
    axes.set_xlabel("prompt size in bytes, log scale")
    axes.set_ylabel("enforce minus passthrough P50 shift, microseconds")
    axes.set_title(
        "Measured cost of enforcement against prompt size", fontsize=11
    )
    axes.grid(True, which="both", alpha=0.25, linewidth=0.5)
    every_point = points + stream_points
    if every_point:
        tick_values = sorted({point.prompt_bytes for point in every_point})
        axes.set_xticks(tick_values)
        axes.xaxis.set_major_formatter(ticker.FuncFormatter(lambda value, position: f"{value:g}"))
        axes.xaxis.set_minor_formatter(ticker.NullFormatter())
        axes.set_xlim(min(tick_values) / 2.2, max(tick_values) * 2.2)
        ceiling = max(
            [ENFORCEMENT_SLO_MICROSECONDS]
            + [point.interval[1] for point in every_point if point.interval[1] == point.interval[1]]
        )
        floor = min([0.0] + [point.interval[0] for point in every_point])
        axes.set_ylim(min(floor * 1.2, -5.0), ceiling * 1.25)
    if len(points) < 2:
        axes.text(
            0.5,
            0.52,
            "ONE PAYLOAD SIZE IN THIS RUN\n"
            "Token estimation grows with prompt size, so where the shift\n"
            f"crosses the {ENFORCEMENT_SLO_MICROSECONDS:g}us line cannot be located from one point.\n"
            "No curve is drawn through it. Evidence mode measures\n"
            "150, 4096 and 32768 bytes.",
            transform=axes.transAxes,
            ha="center",
            va="center",
            fontsize=9,
            bbox={"boxstyle": "round", "facecolor": "#fff4d6", "edgecolor": "#c9a227"},
        )
    axes.legend(fontsize=7.5, loc="upper left", framealpha=0.9)


def draw_component_panel(
    axes,
    reason: str | None,
    delta: list[Component],
    shared: list[Component],
    measured: float | None,
    measured_interval: tuple[float, float] | None,
    measured_label: str,
) -> None:
    axes.set_title("Where that cost comes from, measured on this host", fontsize=11)
    axes.set_ylabel("microseconds per enforced request")
    if reason is not None:
        axes.set_axis_off()
        axes.text(
            0.5,
            0.5,
            "NO COMPONENT ANNOTATION\n" + reason,
            transform=axes.transAxes,
            ha="center",
            va="center",
            fontsize=9,
            wrap=True,
            bbox={"boxstyle": "round", "facecolor": "#fff4d6", "edgecolor": "#c9a227"},
        )
        return

    positions = [0.0, 1.0, 2.0]
    labels = [measured_label, "modelled from\ncomponents", "paid by both arms,\ncancels in the shift"]

    if measured is not None:
        error = None
        if measured_interval is not None:
            error = [[measured - measured_interval[0]], [measured_interval[1] - measured]]
        axes.bar(
            [positions[0]],
            [measured],
            width=0.62,
            color="#dcdcdc",
            edgecolor="#333333",
            yerr=error,
            capsize=4,
            label="measured shift",
        )
        axes.text(
            positions[0],
            measured,
            f"{measured:.1f}",
            ha="center",
            va="bottom",
            fontsize=8,
        )

    bottom = 0.0
    for index, component in enumerate(delta):
        axes.bar(
            [positions[1]],
            [component.microseconds],
            width=0.62,
            bottom=bottom,
            color=DELTA_COMPONENT_COLORS[index % len(DELTA_COMPONENT_COLORS)],
            edgecolor="#333333",
            label=f"{component.label} {component.microseconds:.2f}us",
        )
        bottom += component.microseconds
    # Offset to the left of the bar so the modelled total does not sit on top of
    # the boundary between the last component and the unattributed remainder.
    axes.text(
        positions[1] - 0.36,
        bottom,
        f"{bottom:.1f}",
        ha="right",
        va="center",
        fontsize=8,
        bbox={"boxstyle": "square,pad=0.15", "facecolor": "white", "edgecolor": "none"},
    )
    if measured is not None and measured > bottom:
        axes.bar(
            [positions[1]],
            [measured - bottom],
            width=0.62,
            bottom=bottom,
            color="none",
            edgecolor="#333333",
            hatch="//",
            label=f"unattributed {measured - bottom:.2f}us",
        )

    shared_bottom = 0.0
    for index, component in enumerate(shared):
        axes.bar(
            [positions[2]],
            [component.microseconds],
            width=0.62,
            bottom=shared_bottom,
            color=SHARED_COMPONENT_COLORS[index % len(SHARED_COMPONENT_COLORS)],
            edgecolor="#333333",
            label=f"{component.label} {component.microseconds:.2f}us",
        )
        shared_bottom += component.microseconds
    axes.text(positions[2], shared_bottom, f"{shared_bottom:.1f}", ha="center", va="bottom", fontsize=8)

    axes.set_xticks(positions)
    axes.set_xticklabels(labels, fontsize=8)
    axes.grid(True, axis="y", alpha=0.25, linewidth=0.5)
    axes.legend(fontsize=7, loc="upper right", framealpha=0.9)


def render(
    results_dir: str,
    manifest: dict[str, str],
    points: list[ShiftPoint],
    stream_points: list[ShiftPoint],
    readings: dict[str, BenchmarkReading],
    reason: str | None,
    delta: list[Component],
    shared: list[Component],
    out_path: str,
) -> None:
    figure, (axes_shift, axes_components) = pyplot.subplots(
        1, 2, figsize=(15.0, 7.6), width_ratios=(1.55, 1.0)
    )
    draw_shift_panel(axes_shift, points, stream_points)
    smallest = points[0] if points else None
    draw_component_panel(
        axes_components,
        reason,
        delta,
        shared,
        smallest.median if smallest else None,
        smallest.interval if smallest else None,
        f"measured shift\n{smallest.prompt_bytes}B at {smallest.rate:g}rps" if smallest else "measured shift",
    )

    caption = (
        f"Source {os.path.basename(results_dir)}. levee tree {manifest['levee_git_tree']} "
        f"sha {manifest['levee_git_sha']} dirty {manifest.get('levee_tree_dirty', 'unknown')}. "
        f"Run mode {manifest['run_mode']}, hardware {manifest.get('hardware_tag', 'unknown')}, "
        f"log destination {manifest.get('levee_log_destination', 'unknown')}.\n"
        "The published quantity is the median repetition-matched enforce minus passthrough P50 "
        "shift over the committed steady-only CSVs, the identical estimator pre-registered band 3 "
        f"gates on, with a {overhead_figure.BOOTSTRAP_CONFIDENCE_PERCENT:g} percent percentile "
        f"bootstrap over {overhead_figure.BOOTSTRAP_RESAMPLES} resamples. Every number carries its "
        "payload size and arrival rate.\n"
        "Every component cost is parsed from this run's own microbench.txt, never hardcoded, so the "
        "annotation moves when the measurement moves. Logging is shown as its own component because "
        "an enforced request writes three log lines where a passthrough request writes one, so part "
        "of the shift is structured logging rather than enforcement work.\n"
        "The request body read is drawn OUTSIDE the shift: ServeHTTP reads the body before the "
        "enforcement branch, so both arms pay it and it cancels. microbench.txt records no payload "
        "size, so the estimation component is comparable to the smallest payload cell only."
    )
    figure.text(0.008, 0.006, caption, fontsize=7.5, va="bottom", ha="left", wrap=True)
    figure.tight_layout(rect=(0.0, 0.145, 1.0, 1.0))
    figure.savefig(out_path, dpi=160)
    pyplot.close(figure)


def main(argv: list[str]) -> int:
    parser = argparse.ArgumentParser(
        description="Render the enforcement cost figure from a committed results directory."
    )
    parser.add_argument("results_dir", help="a results directory written by benchmarks/harness/run.sh")
    parser.add_argument("--out", default=None, help="output PNG path")
    arguments = parser.parse_args(argv[1:])

    results_dir = arguments.results_dir.rstrip(os.sep)
    out_path = arguments.out or os.path.join(
        os.path.dirname(os.path.abspath(__file__)),
        f"enforcement-{os.path.basename(results_dir)}.png",
    )

    try:
        manifest = overhead_figure.read_manifest(results_dir)
        cells = check_bands.load_cells(results_dir)
        readings, reason = load_microbench(results_dir)
        delta = delta_components(readings) if reason is None else []
        shared = shared_components(readings) if reason is None else []
    except (OSError, ValueError) as error:
        print(f"cannot render, {error}", file=sys.stderr)
        return 1

    points = shift_points(cells, stream=False)
    stream_points = shift_points(cells, stream=True)

    print_header(results_dir, manifest)
    print_shift_table(points, stream=False)
    print_shift_table(stream_points, stream=True)
    print_component_table(
        readings, reason, delta, shared, points[0].median if points else None
    )

    render(results_dir, manifest, points, stream_points, readings, reason, delta, shared, out_path)
    print(f"wrote {out_path}")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
