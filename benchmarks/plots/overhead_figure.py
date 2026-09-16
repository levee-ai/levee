# /// script
# requires-python = ">=3.12"
# dependencies = ["matplotlib==3.10.0", "numpy>=1.26"]
# ///
"""Render the proxy-overhead figure from a committed results directory.

Reads the per-request-row latency CSVs written by run.sh and draws one
empirical cumulative distribution per cell, so both absolute distributions are
visible rather than a single subtracted number. The quoted overhead is a
quantile shift: P99(proxied) minus P99(direct). It is NOT the P99 of
per-request overhead, which cannot be computed without paired samples, and the
figure caption says so.

This script generates no load. It reads only the committed artifacts, so a
stranger can regenerate every figure offline from a cloned repository and check
it against the same rows the sanity bands gate on.

Three conventions are deliberately borrowed from check_bands.py by importing it
rather than restating them, because a second divergent copy of the parser or of
the percentile definition would let a figure publish a number the gate never
saw:

  the cell loader        which artifacts make a cell, which rows survive the
                         steady-scenario filter, and how role, stream mode,
                         payload size and repetition ordinal are recovered
  the percentile         linear interpolation between order statistics
  the baseline rule      a group's quantile is the median across the cells in
                         it, the same form band 2 uses for its direct baseline

Usage: uv run --script overhead_figure.py <results-dir> [--out <png>]
"""

from __future__ import annotations

import argparse
import os
import statistics
import sys
import zlib
from dataclasses import dataclass, field

# check_bands.py sits beside this file. The script directory is already first on
# the import path under both `uv run --script` and a bare interpreter, and
# inserting it explicitly makes the import survive any other launcher too.
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import check_bands  # noqa: E402  import path is set immediately above
import matplotlib  # noqa: E402

# Chosen before pyplot is imported. Agg is a file-only raster backend, so no
# display server is contacted and rendering works the same in CI as by hand.
matplotlib.use("Agg")

import matplotlib.pyplot as pyplot  # noqa: E402
import matplotlib.ticker as ticker  # noqa: E402
import numpy  # noqa: E402

# 2000 resamples at 95 percent, fixed by the design. P99.9 on a 20-second cell
# rests on roughly 10 to 20 order statistics near the tail, so a bare point
# estimate is too flimsy to publish and every quoted percentile carries an
# interval.
BOOTSTRAP_RESAMPLES = 2000
BOOTSTRAP_CONFIDENCE_PERCENT = 95.0

# The seed is fixed so two renders of the same committed directory print the
# same interval. Regenerable means identical statistics and marks, so a
# resampling interval that moved between renders would break that property for
# no benefit.
BOOTSTRAP_SEED = 20260915

# Resamples are drawn in row batches to bound peak memory. One batch is
# batch_rows by sample_count int64 values, so 250 rows against a 10k-row cell is
# roughly 20MB rather than the 160MB a single 2000-row draw would hold.
BOOTSTRAP_BATCH_ROWS = 250

REPORTED_QUANTILES = (50.0, 90.0, 99.0, 99.9)

# Tenet 1's budget. It is a bound on the direct-to-proxied SHIFT, not on
# absolute latency, so it is drawn on the absolute axis as a reference only and
# the label says which of the two it governs. Band 1 in check_bands.py was
# amended for exactly this confusion, where an overhead budget had been borrowed
# as an absolute ceiling on one cell.
PROXY_OVERHEAD_SLO_MILLISECONDS = 1.0

ROLE_COLORS = {
    "direct": "#1b6ca8",
    "passthrough": "#c76a00",
    "enforce": "#128a5c",
}

# Payload size selects the line style, so a reader can tell two payload sizes of
# the same role apart in greyscale as well as in colour.
PAYLOAD_LINE_STYLES = ("-", "--", "-.", ":")

QUANTILE_MARKERS = {50.0: "o", 90.0: "s", 99.0: "D", 99.9: "^"}

DURATION_LABEL = "duration to last byte"
WAITING_LABEL = "time to first byte"

# Fixed per-metric seed offsets. The built-in hash of a string is salted per
# interpreter process, so deriving a seed from it would give a different
# interval on every render and break the regenerability property.
METRIC_SEED_OFFSETS = {DURATION_LABEL: 1, WAITING_LABEL: 2}


@dataclass
class Group:
    """Every cell sharing a role, a stream mode and a payload size.

    In evidence mode a group holds one cell per repetition. In quick mode most
    groups hold exactly one cell, and the direct non-streaming group holds the
    two drift canaries that bracket the matrix.
    """

    role: str
    stream: bool
    prompt_bytes: int
    cells: list[check_bands.Cell] = field(default_factory=list)

    @property
    def rate(self) -> float:
        rates = sorted({cell.rate for cell in self.cells})
        if len(rates) != 1:
            raise ValueError(
                f"group {self.label()} mixes arrival rates {rates}, so no single "
                "rate can qualify its numbers"
            )
        return rates[0]

    def label(self) -> str:
        mode = "stream" if self.stream else "nonstream"
        return f"{self.role} {mode} {self.prompt_bytes}B"

    def qualified_label(self) -> str:
        """The label every quoted number must carry: payload size AND rate.

        Kept short enough to sit in a fixed-width table column, because the
        stdout tables are what gets quoted and a wrapped column is a misread
        waiting to happen.
        """
        return f"{self.label()} {self.rate:g}rps"


def read_manifest(results_dir: str) -> dict[str, str]:
    """Return the KEY=VALUE fields of the run MANIFEST.

    The trailing fixture digest block carries no equals sign and is skipped, so
    a missing key is reported by its name rather than by a parse crash.
    """
    manifest: dict[str, str] = {}
    path = os.path.join(results_dir, "MANIFEST")
    with open(path, encoding="utf-8") as handle:
        for line in handle:
            stripped = line.strip()
            if "=" not in stripped:
                continue
            key, value = stripped.split("=", 1)
            manifest[key] = value
    for required in ("levee_git_tree", "levee_git_sha", "run_mode"):
        if required not in manifest:
            raise ValueError(
                f"MANIFEST has no {required} field, so a figure rendered from it "
                "could not identify the tree it describes"
            )
    return manifest


def group_cells(cells: list[check_bands.Cell]) -> list[Group]:
    """Bucket cells by role, stream mode and payload size, in a stable order."""
    buckets: dict[tuple[str, bool, int], Group] = {}
    for cell in cells:
        key = (cell.role, cell.stream, cell.prompt_bytes)
        buckets.setdefault(key, Group(cell.role, cell.stream, cell.prompt_bytes))
        buckets[key].cells.append(cell)
    for group in buckets.values():
        group.cells.sort(key=lambda cell: cell.name)
    role_order = {"direct": 0, "passthrough": 1, "enforce": 2}
    return sorted(
        buckets.values(),
        key=lambda group: (group.stream, group.prompt_bytes, role_order.get(group.role, 9), group.role),
    )


_ORDERED_CACHE: dict[tuple[int, str], numpy.ndarray] = {}


def ordered_samples(cell: check_bands.Cell, metric: str) -> numpy.ndarray:
    """Return the cell's steady samples for one metric, sorted ascending."""
    key = (cell.name, metric)
    if key not in _ORDERED_CACHE:
        raw = (
            cell.steady_duration_samples
            if metric == DURATION_LABEL
            else cell.steady_waiting_samples
        )
        _ORDERED_CACHE[key] = numpy.sort(numpy.asarray(raw, dtype=numpy.float64))
    return _ORDERED_CACHE[key]


def order_positions(count: int, quantiles: tuple[float, ...]):
    """Return the interpolation triple check_bands.percentile computes.

    Same arithmetic, vectorised: position (count-1)*quantile/100, the two
    neighbouring order statistic indices, and the interpolation fraction. Held
    identical to the gate on purpose, and confirmed by probe to reproduce
    check_bands.percentile to zero absolute deviation on every cell of a real
    run rather than merely to floating-point tolerance.
    """
    position = (count - 1) * numpy.asarray(quantiles, dtype=numpy.float64) / 100.0
    lower = numpy.floor(position).astype(numpy.int64)
    upper = numpy.minimum(lower + 1, count - 1)
    return lower, upper, position - lower


_BOOTSTRAP_CACHE: dict[tuple[int, str], numpy.ndarray] = {}


def bootstrap_estimates(cell: check_bands.Cell, metric: str) -> numpy.ndarray:
    """Return a (quantile, resample) array of bootstrap percentile estimates.

    Indices into the SORTED sample are resampled and then partially sorted,
    rather than resampling the values themselves. Mapping a sorted index set
    through a nondecreasing array yields the resample already in order, so the
    order statistics a percentile needs come out of one partial sort of integers
    instead of a full sort of floats.
    """
    key = (cell.name, metric)
    if key in _BOOTSTRAP_CACHE:
        return _BOOTSTRAP_CACHE[key]
    ordered = ordered_samples(cell, metric)
    count = ordered.shape[0]
    lower, upper, fraction = order_positions(count, REPORTED_QUANTILES)
    partition_positions = numpy.unique(numpy.concatenate([lower, upper]))
    # Seeded per cell and metric so the interval for one cell never depends on
    # how many other cells were rendered before it, which would make a
    # single-cell re-render disagree with the full figure. crc32 is used rather
    # than the built-in hash because the built-in is salted per process.
    generator = numpy.random.default_rng(
        [
            BOOTSTRAP_SEED,
            zlib.crc32(cell.name.encode("utf-8")),
            METRIC_SEED_OFFSETS[metric],
        ]
    )
    estimates = numpy.empty((len(REPORTED_QUANTILES), BOOTSTRAP_RESAMPLES), dtype=numpy.float64)
    filled = 0
    while filled < BOOTSTRAP_RESAMPLES:
        rows = min(BOOTSTRAP_BATCH_ROWS, BOOTSTRAP_RESAMPLES - filled)
        draws = generator.integers(0, count, size=(rows, count))
        picked = numpy.partition(draws, partition_positions, axis=1)
        low = ordered[picked[:, lower]]
        high = ordered[picked[:, upper]]
        estimates[:, filled : filled + rows] = (low + (high - low) * fraction).T
        filled += rows
    _BOOTSTRAP_CACHE[key] = estimates
    return estimates


def interval(vector: numpy.ndarray) -> tuple[float, float]:
    """Return the percentile bootstrap interval of one estimate vector.

    The interval edges go through check_bands.percentile, so the same
    interpolation rule produces the point estimate and the interval around it.
    """
    tail = (100.0 - BOOTSTRAP_CONFIDENCE_PERCENT) / 2.0
    values = sorted(float(value) for value in vector)
    return (
        check_bands.percentile(values, tail),
        check_bands.percentile(values, 100.0 - tail),
    )


def cell_quantile(cell: check_bands.Cell, metric: str, quantile: float) -> float:
    samples = (
        cell.steady_duration_samples
        if metric == DURATION_LABEL
        else cell.steady_waiting_samples
    )
    return check_bands.percentile(samples, quantile)


def group_quantile(group: Group, metric: str, quantile: float) -> float:
    """The group's quantile, medianed across its cells.

    Medianing across cells is what stops one polluted repetition from
    dominating a published number, and it is the same form band 2 uses when it
    builds its direct baseline from the two canaries.
    """
    return statistics.median(cell_quantile(cell, metric, quantile) for cell in group.cells)


def group_quantile_bootstrap(group: Group, metric: str, quantile_index: int) -> numpy.ndarray:
    """Bootstrap vector for a group quantile, resampling inside each cell.

    Each cell is resampled independently and the median across cells is taken
    per resample, so the interval covers the same estimator the point estimate
    uses rather than a different one that happens to be easier to resample.
    """
    stacked = numpy.vstack(
        [bootstrap_estimates(cell, metric)[quantile_index] for cell in group.cells]
    )
    return numpy.median(stacked, axis=0)


def representative(group: Group) -> check_bands.Cell:
    """The cell whose distribution is drawn for the group.

    A curve needs one real sample, so a group of repetitions has to nominate
    one. The rule is the median P99, taking the LOWER median when the count is
    even, because tail pollution lands in P99 first and a median rejects it
    while an average would absorb it. The rule is applied identically to
    baseline and treatment groups, and every candidate P99 is printed so the
    choice is auditable. Applied to a baseline the lower median is the
    conservative direction, since a smaller direct P99 makes the reported shift
    larger.
    """
    ranked = sorted(group.cells, key=lambda cell: (cell_quantile(cell, DURATION_LABEL, 99.0), cell.name))
    return ranked[(len(ranked) - 1) // 2]


def find_baseline(groups: list[Group], stream: bool, prompt_bytes: int) -> Group | None:
    for group in groups:
        if group.role == "direct" and group.stream == stream and group.prompt_bytes == prompt_bytes:
            return group
    return None


def print_header(results_dir: str, manifest: dict[str, str]) -> None:
    print(f"proxy overhead figure for {os.path.basename(results_dir)}")
    print(
        f"levee tree {manifest['levee_git_tree']} sha {manifest['levee_git_sha']} "
        f"dirty {manifest.get('levee_tree_dirty', 'unknown')}"
    )
    print(
        f"run mode {manifest['run_mode']} hardware {manifest.get('hardware_tag', 'unknown')} "
        f"k6 {manifest.get('k6_version', 'unknown')}"
    )
    print(f"renderer python {sys.version.split()[0]} matplotlib {matplotlib.__version__} numpy {numpy.__version__}")
    print(
        "percentiles are linear interpolation between order statistics over the committed "
        "steady-only CSVs, the identical definition and the identical rows check_bands.py gates on"
    )
    print(
        f"intervals are percentile bootstrap, {BOOTSTRAP_RESAMPLES} resamples at "
        f"{BOOTSTRAP_CONFIDENCE_PERCENT:g} percent, seeded so a re-render reproduces them"
    )
    print(
        "ESTIMATOR the quoted overhead is a QUANTILE SHIFT, P99(proxied) minus P99(direct). "
        "It is NOT the P99 of per-request overhead, which is unmeasurable without paired "
        "samples, so both absolute distributions stay visible on the figure"
    )
    print(
        f"the {PROXY_OVERHEAD_SLO_MILLISECONDS:g}ms Tenet 1 line bounds that SHIFT, not absolute "
        "latency. It is drawn as a reference and never enforced, so a noisy run reports honestly"
    )
    print()


def print_cell_table(groups: list[Group]) -> None:
    print("TABLE 1 steady-scenario percentiles per cell in milliseconds, with bootstrap interval")
    print(
        "  {:<38} {:>7} {:>6} {:>7} {:<21} {:>6} {:>8} {:>19}".format(
            "cell", "bytes", "rate", "rows", "metric", "pctile", "value", "interval"
        )
    )
    for group in groups:
        metrics = [DURATION_LABEL]
        if group.stream:
            metrics.append(WAITING_LABEL)
        for metric in metrics:
            for cell in group.cells:
                if metric == WAITING_LABEL and not cell.steady_waiting_samples:
                    continue
                estimates = bootstrap_estimates(cell, metric)
                rows = len(ordered_samples(cell, metric))
                for index, quantile in enumerate(REPORTED_QUANTILES):
                    low, high = interval(estimates[index])
                    print(
                        "  {:<38} {:>7d} {:>6g} {:>7d} {:<21} {:>6g} {:>8.3f} {:>19}".format(
                            cell.name,
                            cell.prompt_bytes,
                            cell.rate,
                            rows,
                            metric,
                            quantile,
                            cell_quantile(cell, metric, quantile),
                            f"[{low:.3f}, {high:.3f}]",
                        )
                    )
    print()


def print_group_table(groups: list[Group]) -> None:
    print("TABLE 2 group quantiles medianed across cells, and the cell drawn on the figure")
    print(
        "  {:<36} {:>6} {:>8} {:>8} {:>8} {:>8} {:>9} {:<38}".format(
            "group", "cells", "p50", "p90", "p99", "p99.9", "p99spread", "cell drawn"
        )
    )
    for group in groups:
        tails = [cell_quantile(cell, DURATION_LABEL, 99.0) for cell in group.cells]
        print(
            "  {:<36} {:>6d} {:>8.3f} {:>8.3f} {:>8.3f} {:>8.3f} {:>9.3f} {:<38}".format(
                group.qualified_label(),
                len(group.cells),
                group_quantile(group, DURATION_LABEL, 50.0),
                group_quantile(group, DURATION_LABEL, 90.0),
                group_quantile(group, DURATION_LABEL, 99.0),
                group_quantile(group, DURATION_LABEL, 99.9),
                max(tails) - min(tails),
                representative(group).name,
            )
        )
    single = [group.label() for group in groups if len(group.cells) == 1]
    if single:
        print(
            "  DEGRADED one cell in "
            + ", ".join(single)
            + ", so the across-cell spread of those groups is zero by arithmetic rather than "
            "by measurement and the median is that one cell. Only the bootstrap interval "
            "quantifies uncertainty for them"
        )
    print()


def print_shift_table(groups: list[Group]) -> None:
    print("TABLE 3 quantile shift in milliseconds, treatment minus the direct baseline")
    print(
        "  each row is one payload size at one arrival rate. An unqualified latency number is "
        "not reportable, so both qualifiers are on every row"
    )
    print(
        "  {:<36} {:>6} {:>9} {:>10} {:>9} {:>19}".format(
            "arm against its direct baseline", "pctile", "direct", "treatment", "shift", "shift interval"
        )
    )
    printed = False
    for group in groups:
        if group.role == "direct":
            continue
        baseline = find_baseline(groups, group.stream, group.prompt_bytes)
        if baseline is None:
            print(
                f"  SKIPPED {group.qualified_label()} has no direct cell at the same payload "
                "size and stream mode in this run, so its shift is not computable. Check which "
                "direct cells run_matrix produced for this run, since a treatment arm can only "
                "be differenced against a direct baseline measured at the same payload size and "
                "stream mode"
            )
            continue
        printed = True
        for index, quantile in enumerate(REPORTED_QUANTILES):
            direct_value = group_quantile(baseline, DURATION_LABEL, quantile)
            treatment_value = group_quantile(group, DURATION_LABEL, quantile)
            shift_vector = group_quantile_bootstrap(group, DURATION_LABEL, index) - group_quantile_bootstrap(
                baseline, DURATION_LABEL, index
            )
            low, high = interval(shift_vector)
            print(
                "  {:<36} {:>6g} {:>9.3f} {:>10.3f} {:>+9.3f} {:>19}".format(
                    group.qualified_label(),
                    quantile,
                    direct_value,
                    treatment_value,
                    treatment_value - direct_value,
                    f"[{low:+.3f}, {high:+.3f}]",
                )
            )
        paired = check_bands.paired_shifts(baseline.cells, group.cells, 99.0)
        if paired:
            print(
                "  repetition-matched P99 shifts for the row above, "
                + ", ".join(f"{label} {shift:+.3f}" for label, shift in paired)
            )
    if not printed:
        print("  no treatment group in this run has a direct baseline, so no shift is reportable")
    print()


def print_streaming_table(groups: list[Group]) -> None:
    streaming = [group for group in groups if group.stream]
    if not streaming:
        print("TABLE 4 no streaming cell in this run")
        print()
        return
    print("TABLE 4 streaming cells, time to first byte against duration to last byte, milliseconds")
    print(
        "  {:<36} {:>6} {:>9} {:>9} {:>9} {:>8}".format(
            "group", "pctile", "ttfb", "duration", "receive", "ttfbpct"
        )
    )
    for group in streaming:
        for quantile in REPORTED_QUANTILES:
            waiting = group_quantile(group, WAITING_LABEL, quantile)
            duration = group_quantile(group, DURATION_LABEL, quantile)
            share = (waiting / duration * 100.0) if duration > 0 else float("nan")
            print(
                "  {:<36} {:>6g} {:>9.3f} {:>9.3f} {:>9.3f} {:>7.1f}%".format(
                    group.qualified_label(), quantile, waiting, duration, duration - waiting, share
                )
            )
    print(
        "  receive is duration minus ttfb, the time taking the rest of the stream after the first "
        "byte arrives. The mock replays every stored event with no pacing, so ttfb sits just below "
        "duration BY CONSTRUCTION and receive is the minority share. A zero-latency upstream "
        "compresses that gap, and against a real provider it would open to the width of the "
        "generation itself. Both series are reported with their real magnitudes so the compression "
        "is visible rather than presented as a property of levee"
    )
    print()


def draw_ecdf(axes, group: Group, metric: str, style: str, alpha: float, label: str) -> None:
    cell = representative(group)
    ordered = ordered_samples(cell, metric)
    count = ordered.shape[0]
    if count == 0:
        return
    fractions = numpy.arange(1, count + 1, dtype=numpy.float64) / count
    # The final point sits at exactly 1.0, which the logit axis cannot place.
    # Dropping it keeps the curve honest and loses nothing a marker reports.
    axes.step(
        ordered[:-1],
        fractions[:-1],
        where="post",
        color=ROLE_COLORS.get(group.role, "#444444"),
        linestyle=style,
        alpha=alpha,
        linewidth=1.5,
        label=label,
    )
    estimates = bootstrap_estimates(cell, metric)
    for index, quantile in enumerate(REPORTED_QUANTILES):
        value = cell_quantile(cell, metric, quantile)
        low, high = interval(estimates[index])
        axes.errorbar(
            [value],
            [quantile / 100.0],
            xerr=[[value - low], [high - value]],
            color=ROLE_COLORS.get(group.role, "#444444"),
            marker=QUANTILE_MARKERS[quantile],
            markersize=6,
            alpha=alpha,
            capsize=3,
            linestyle="none",
        )


def style_axes(axes, title: str, drawn_values: list[float]) -> None:
    axes.set_xscale("log")
    axes.set_yscale("logit")
    axes.set_ylim(0.01, 0.9995)
    ticks = [0.01, 0.1, 0.5, 0.9, 0.99, 0.999]
    axes.set_yticks(ticks)
    axes.set_yticklabels(["1%", "10%", "50%", "90%", "99%", "99.9%"])
    axes.set_ylabel("cumulative fraction of steady request rows\n(logit scale, so the tail stays readable)")
    axes.set_xlabel("HTTP duration in milliseconds, log scale")
    axes.set_title(title, fontsize=11)
    axes.grid(True, which="both", alpha=0.25, linewidth=0.5)
    axes.axvline(
        PROXY_OVERHEAD_SLO_MILLISECONDS,
        color="#b00020",
        linestyle="--",
        linewidth=1.3,
        label=(
            f"{PROXY_OVERHEAD_SLO_MILLISECONDS:g}ms Tenet 1 budget, which bounds the "
            "direct-to-proxied SHIFT and not absolute latency. Drawn, never enforced"
        ),
    )
    if drawn_values:
        axes.set_xlim(min(drawn_values) * 0.8, max(drawn_values) * 1.6)
    # The whole data range spans about two decades, so the decade ticks alone
    # would leave a reader unable to read a value off the axis. A few labelled
    # minor ticks in plain milliseconds are what make the picture checkable
    # against the stdout table. The general-format callable is used rather than
    # the scalar formatter, which rounds a sub-millisecond tick to 0.
    millisecond_format = ticker.FuncFormatter(lambda value, position: f"{value:g}")
    axes.xaxis.set_major_locator(ticker.LogLocator(base=10.0, subs=(1.0,)))
    axes.xaxis.set_minor_locator(ticker.LogLocator(base=10.0, subs=(2.0, 3.0, 5.0)))
    axes.xaxis.set_major_formatter(millisecond_format)
    axes.xaxis.set_minor_formatter(millisecond_format)
    axes.tick_params(axis="x", which="minor", labelsize=7.0)
    # An empirical cumulative distribution rises from lower left to upper right,
    # so the lower right corner is always empty and is the one place a legend
    # cannot hide a curve.
    axes.legend(fontsize=7.5, loc="lower right", framealpha=0.9)


def shift_annotation(groups: list[Group], stream: bool) -> str:
    lines = []
    for group in groups:
        if group.role == "direct" or group.stream != stream:
            continue
        baseline = find_baseline(groups, group.stream, group.prompt_bytes)
        if baseline is None:
            continue
        index = REPORTED_QUANTILES.index(99.0)
        direct_value = group_quantile(baseline, DURATION_LABEL, 99.0)
        treatment_value = group_quantile(group, DURATION_LABEL, 99.0)
        shift_vector = group_quantile_bootstrap(group, DURATION_LABEL, index) - group_quantile_bootstrap(
            baseline, DURATION_LABEL, index
        )
        low, high = interval(shift_vector)
        lines.append(
            f"{group.role} {group.prompt_bytes}B at {group.rate:g}/s: "
            f"{treatment_value:.3f} minus {direct_value:.3f} = {treatment_value - direct_value:+.3f}ms "
            f"[{low:+.3f}, {high:+.3f}]"
        )
    if not lines:
        return "no direct baseline at these payload sizes, so no P99 shift is computable"
    return "P99 quantile shift versus direct\n" + "\n".join(lines)


def render(results_dir: str, manifest: dict[str, str], groups: list[Group], out_path: str) -> None:
    figure, (axes_nonstream, axes_stream) = pyplot.subplots(2, 1, figsize=(13.0, 13.5))

    payload_styles: dict[int, str] = {}
    for group in groups:
        if group.prompt_bytes not in payload_styles:
            payload_styles[group.prompt_bytes] = PAYLOAD_LINE_STYLES[
                len(payload_styles) % len(PAYLOAD_LINE_STYLES)
            ]

    nonstream_values: list[float] = []
    for group in [item for item in groups if not item.stream]:
        cell = representative(group)
        draw_ecdf(
            axes_nonstream,
            group,
            DURATION_LABEL,
            payload_styles[group.prompt_bytes],
            1.0,
            f"{group.qualified_label()}, {len(group.cells)} cell(s), drawn {cell.name}",
        )
        nonstream_values.append(cell_quantile(cell, DURATION_LABEL, 99.9))
        nonstream_values.append(check_bands.percentile(cell.steady_duration_samples, 0.5))
    style_axes(
        axes_nonstream,
        "Non-streaming cells. One curve per cell group, so both absolute distributions stay visible",
        nonstream_values,
    )
    axes_nonstream.text(
        0.01,
        0.97,
        shift_annotation(groups, stream=False),
        transform=axes_nonstream.transAxes,
        fontsize=7.5,
        va="top",
        ha="left",
        bbox={"boxstyle": "round", "facecolor": "#f4f4f4", "edgecolor": "#999999"},
    )

    stream_groups = [item for item in groups if item.stream]
    stream_values: list[float] = []
    if stream_groups:
        for group in stream_groups:
            cell = representative(group)
            draw_ecdf(
                axes_stream,
                group,
                DURATION_LABEL,
                payload_styles[group.prompt_bytes],
                1.0,
                f"{group.qualified_label()} {DURATION_LABEL}, drawn {cell.name}",
            )
            stream_values.append(cell_quantile(cell, DURATION_LABEL, 99.9))
            if cell.steady_waiting_samples:
                draw_ecdf(
                    axes_stream,
                    group,
                    WAITING_LABEL,
                    ":",
                    0.55,
                    f"{group.qualified_label()} {WAITING_LABEL}",
                )
                stream_values.append(check_bands.percentile(cell.steady_waiting_samples, 0.5))
        style_axes(
            axes_stream,
            "Streaming cells. Time to first byte sits just below duration to last byte because a "
            "zero-latency mock compresses that gap by construction",
            stream_values,
        )
        axes_stream.text(
            0.01,
            0.97,
            shift_annotation(groups, stream=True),
            transform=axes_stream.transAxes,
            fontsize=7.5,
            va="top",
            ha="left",
            bbox={"boxstyle": "round", "facecolor": "#f4f4f4", "edgecolor": "#999999"},
        )
    else:
        axes_stream.set_axis_off()
        axes_stream.text(
            0.5,
            0.5,
            "no streaming cell in this run, so the streaming panel is empty",
            transform=axes_stream.transAxes,
            ha="center",
            va="center",
            fontsize=11,
        )

    caption = (
        f"Source {os.path.basename(results_dir)}. levee tree {manifest['levee_git_tree']} "
        f"sha {manifest['levee_git_sha']} dirty {manifest.get('levee_tree_dirty', 'unknown')}. "
        f"Run mode {manifest['run_mode']}, hardware {manifest.get('hardware_tag', 'unknown')}, "
        f"upstream {manifest.get('upstream_scheme', 'unknown')}.\n"
        "ESTIMATOR the quoted overhead is a QUANTILE SHIFT, P99(proxied) minus P99(direct). It is "
        "NOT the P99 of per-request overhead, which is unmeasurable without paired samples, which "
        "is why both absolute distributions are drawn.\n"
        f"Markers are P50, P90, P99 and P99.9 with a {BOOTSTRAP_CONFIDENCE_PERCENT:g} percent "
        f"percentile bootstrap over {BOOTSTRAP_RESAMPLES} resamples. Percentiles come from the "
        "committed steady-only CSVs by linear interpolation between order statistics, the same rows "
        "and the same definition check_bands.py gates on.\n"
        "Where a group holds several repetitions the curve drawn is its median-P99 cell and the "
        "group quantile is medianed across cells, so one polluted repetition cannot dominate. "
        "Every label carries its payload size and arrival rate because an unqualified latency "
        "number is not reportable."
    )
    figure.text(0.01, 0.005, caption, fontsize=7.5, va="bottom", ha="left", wrap=True)
    figure.tight_layout(rect=(0.0, 0.085, 1.0, 1.0))
    figure.savefig(out_path, dpi=160)
    pyplot.close(figure)


def main(argv: list[str]) -> int:
    parser = argparse.ArgumentParser(
        description="Render the proxy overhead figure from a committed results directory."
    )
    parser.add_argument("results_dir", help="a results directory written by benchmarks/harness/run.sh")
    parser.add_argument("--out", default=None, help="output PNG path")
    arguments = parser.parse_args(argv[1:])

    results_dir = arguments.results_dir.rstrip(os.sep)
    out_path = arguments.out or os.path.join(
        os.path.dirname(os.path.abspath(__file__)),
        f"overhead-{os.path.basename(results_dir)}.png",
    )

    try:
        manifest = read_manifest(results_dir)
        cells = check_bands.load_cells(results_dir)
    except (OSError, ValueError) as error:
        print(f"cannot render, {error}", file=sys.stderr)
        return 1

    groups = group_cells(cells)
    print_header(results_dir, manifest)
    print_cell_table(groups)
    print_group_table(groups)
    print_shift_table(groups)
    print_streaming_table(groups)

    render(results_dir, manifest, groups, out_path)
    print(f"wrote {out_path}")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
