"""Does any score relationship survive REAL market data?

The synthetic experiment answered a narrower question than it looked like it
answered. A trend strategy scoring well on a generated trend has demonstrated
that it agrees with the generator about what a trend is. Only real bars can
separate that agreement from an edge.

This run asks the same questions of the same machinery -- same observation
type, same horizons, same cost model, same episode clustering, same cluster
bootstrap, same classifier -- over historical data instead. Any difference in
the results is therefore a difference in the DATA, which is the only way the
comparison means anything.

# What it does NOT do

No calibration is fitted. No strategy formula, threshold or consensus policy
changes. The historical TEST partition is sealed and never read, and neither
is the synthetic one. Nothing is downloaded: if a named dataset is absent the
run fails loudly rather than reaching for a network.

# Separate run, separate identity

Historical results get their own run id and carry `HISTORICAL_MARKET` as their
source type. They are never merged into a synthetic headline number. A
statistic averaged across the two describes no market that exists.
"""

from __future__ import annotations

import os
import time
from concurrent.futures import ProcessPoolExecutor
from dataclasses import asdict, dataclass, field
from pathlib import Path
from typing import Any

from vantage_quant import strategies

from . import (
    expanded,
    expansion,
    generate,
    historical,
    partition,
    quality,
    verdict,
)
from .datasets import SourceType


@dataclass
class HistoricalDatasetRecord:
    """One imported dataset, as the report needs to describe it."""

    dataset_id: str
    partition: str
    seal: str
    instrument: str
    timeframe: str
    first_bar: str
    last_bar: str
    bars: int
    status: str
    original_file_hash: str
    normalized_dataset_hash: str
    provider: str
    acquisition_method: str
    licensing_note: str
    original_timezone: str
    timezone_declared: bool
    cost_basis: str
    coverage_fraction: float | None
    missing_bars: int | None
    expected_bars: int | None
    errors: list[str] = field(default_factory=list)
    warnings: list[str] = field(default_factory=list)


@dataclass
class HistoricalRun:
    run_id: str
    source_type: str
    code_sha: str
    analysis_versions: verdict.AnalysisVersions
    quality_checker_version: int
    datasets: list[HistoricalDatasetRecord]
    reports: list[expanded.ExpandedStrategyReport]
    sufficiency: dict[str, str]
    sufficiency_reason: dict[str, str]
    total_bars: int
    train_bars: int
    runtime_seconds: float
    bars_per_second: float
    observations_per_second: float
    status: str
    note: str = ""


class NoHistoricalDataError(RuntimeError):
    """No usable historical dataset is present locally.

    Raised rather than returning an empty run: a run full of zeroes reads like
    a measurement showing nothing, when the truth is that nothing was measured.
    """


def import_all(
    *,
    instrument: str = "XAUUSD",
    timeframe: str = "1h",
    root: Path | None = None,
    provider: str = "UNRECORDED",
    acquisition_method: str = "UNRECORDED",
    licensing_note: str = "UNRECORDED",
    declared_timezone: str | None = None,
) -> list[historical.HistoricalDataset]:
    """Import and validate every allowlisted local dataset.

    A REJECTED dataset is skipped with its reason kept, not silently dropped
    and not repaired. Research proceeds on what remains, and the report says
    what was excluded.
    """
    names = historical.available(root)
    if not names:
        raise NoHistoricalDataError(historical.status(root))

    imported: list[historical.HistoricalDataset] = []
    for name in names:
        try:
            imported.append(
                historical.load(
                    name,
                    instrument=instrument,
                    timeframe=timeframe,
                    root=root,
                    provider=provider,
                    acquisition_method=acquisition_method,
                    licensing_note=licensing_note,
                    declared_timezone=declared_timezone,
                    market_clock=quality.MarketClock(),
                    reject_on_error=False,
                )
            )
        except (historical.MalformedDatasetError, historical.UnsafeDatasetPathError):
            # Re-raised rather than skipped: a malformed file in the allowlist
            # is a problem with the import, not a dataset to route around.
            raise
    return imported


def _assign(
    imported: list[historical.HistoricalDataset],
) -> list[partition.DatasetAssignment]:
    """Chronological TRAIN / VALIDATION / TEST, decided BEFORE any analysis.

    Ordered by first bar and split by position, exactly as the synthetic run
    does. The order matters and is the point: import, hash, assign, then
    analyse. Choosing the holdout after seeing outcomes is the failure a
    holdout exists to prevent.
    """
    usable = [
        d for d in imported if d.status is not quality.DatasetStatus.REJECTED
    ]
    ordered = sorted(usable, key=lambda d: d.frame.index[0])
    n = len(ordered)
    train_end = max(1, int(n * partition.TRAIN_FRACTION)) if n else 0
    validation_end = train_end + int(n * partition.VALIDATION_FRACTION)

    out: list[partition.DatasetAssignment] = []
    for i, dataset in enumerate(ordered):
        if i < train_end:
            part = partition.Partition.TRAIN
        elif i < validation_end:
            part = partition.Partition.VALIDATION
        else:
            part = partition.Partition.TEST
        out.append(
            partition.DatasetAssignment(
                dataset_id=dataset.dataset_id,
                partition=part,
                first_bar=dataset.frame.index[0].isoformat(),
                last_bar=dataset.frame.index[-1].isoformat(),
                bars=len(dataset.frame),
                dataset_hash=dataset.provenance.normalized_dataset_hash[:32],
                seal=(
                    "SEALED_FOR_OUT_OF_SAMPLE"
                    if part is partition.Partition.TEST
                    else ""
                ),
            )
        )
    return out


@dataclass(frozen=True)
class _Task:
    """One (strategy, dataset) unit of work for the pool.

    Carries the dataset NAME rather than its bars. Pickling frames across a
    process boundary costs more than re-reading a file, and a name keeps the
    allowlist check inside the worker instead of trusting whatever arrived.
    """

    strategy_key: str
    dataset_name: str
    dataset_id: str
    digest: str
    sha: str
    root: Path | None
    instrument: str
    timeframe: str
    declared_timezone: str | None


def _observe_task(task: _Task) -> tuple[str, int, list[Any]]:
    """Re-import the dataset in the worker, then evaluate one strategy over it.

    `reject_on_error` stays True here even though the parent already validated:
    a file that changed between the two reads must not be silently analysed.
    """
    dataset = historical.load(
        task.dataset_name,
        instrument=task.instrument,
        timeframe=task.timeframe,
        root=task.root,
        declared_timezone=task.declared_timezone,
        reject_on_error=True,
    )
    return expanded.observe_frame(
        task.strategy_key,
        frame=dataset.frame,
        dataset_id=task.dataset_id,
        digest=task.digest,
        sha=task.sha,
    )


def run(
    *,
    run_id: str = "signal-research-3-historical",
    instrument: str = "XAUUSD",
    timeframe: str = "1h",
    root: Path | None = None,
    provider: str = "UNRECORDED",
    acquisition_method: str = "UNRECORDED",
    licensing_note: str = "UNRECORDED",
    declared_timezone: str | None = None,
    workers: int | None = None,
) -> HistoricalRun:
    """The historical experiment. TRAIN only; TEST is sealed and unread."""
    started = time.monotonic()
    imported = import_all(
        instrument=instrument,
        timeframe=timeframe,
        root=root,
        provider=provider,
        acquisition_method=acquisition_method,
        licensing_note=licensing_note,
        declared_timezone=declared_timezone,
    )

    assignments = _assign(imported)
    if not assignments:
        raise NoHistoricalDataError(
            "every imported dataset was REJECTED by quality validation; "
            "nothing is eligible for research"
        )

    by_id = {d.dataset_id: d for d in imported}
    records = [
        _record(by_id[a.dataset_id], a) for a in assignments
    ] + [
        _record(d, None)
        for d in imported
        if d.status is quality.DatasetStatus.REJECTED
    ]

    explorable = partition.explorable(assignments)
    partition.guard_not_sealed(explorable)
    train = [by_id[a.dataset_id] for a in explorable]

    sha = generate.code_sha()
    collected: dict[str, list[Any]] = {key: [] for key in strategies.REGISTRY}
    eligible_bars: dict[str, int] = dict.fromkeys(strategies.REGISTRY, 0)

    tasks = [
        _Task(
            strategy_key=key,
            dataset_name=dataset.provenance.source,
            dataset_id=dataset.dataset_id,
            digest=dataset.provenance.normalized_dataset_hash[:32],
            sha=sha,
            root=root,
            instrument=instrument,
            timeframe=timeframe,
            declared_timezone=declared_timezone,
        )
        for key in sorted(strategies.REGISTRY)
        for dataset in train
    ]

    max_workers = workers or min(12, (os.cpu_count() or 4))
    if max_workers > 1 and len(tasks) > 1:
        with ProcessPoolExecutor(max_workers=max_workers) as pool:
            results = list(pool.map(_observe_task, tasks, chunksize=1))
    else:
        # One worker means run here: a pool of one pays the spawn cost to gain
        # nothing, and a single-process path keeps the run debuggable.
        results = [_observe_task(task) for task in tasks]

    for key, eligible, rows in results:
        collected[key].extend(rows)
        eligible_bars[key] += eligible

    reports = [
        expanded.build_report(
            strategies.REGISTRY[key], collected[key], eligible_bars[key]
        )
        for key in sorted(strategies.REGISTRY)
    ]

    sufficiency: dict[str, str] = {}
    sufficiency_reason: dict[str, str] = {}
    for report in reports:
        state, why = expansion.historical_sufficiency(
            role=expansion.role_for(report.strategy_id),
            raw_n=report.raw_n,
            effective_n=report.effective_n,
        )
        sufficiency[report.strategy_id] = state.value
        sufficiency_reason[report.strategy_id] = why

    runtime = time.monotonic() - started
    train_bars = sum(len(d.frame) for d in train)
    observations = sum(r.raw_n for r in reports)

    return HistoricalRun(
        run_id=run_id,
        source_type=SourceType.HISTORICAL_MARKET.value,
        code_sha=sha,
        analysis_versions=verdict.AnalysisVersions(),
        quality_checker_version=quality.QUALITY_CHECKER_VERSION,
        datasets=records,
        reports=reports,
        sufficiency=sufficiency,
        sufficiency_reason=sufficiency_reason,
        total_bars=sum(len(d.frame) for d in imported),
        train_bars=train_bars,
        runtime_seconds=runtime,
        bars_per_second=train_bars / runtime if runtime else 0.0,
        observations_per_second=observations / runtime if runtime else 0.0,
        status="COMPLETE",
    )


def _record(
    dataset: historical.HistoricalDataset,
    assignment: partition.DatasetAssignment | None,
) -> HistoricalDatasetRecord:
    p, q = dataset.provenance, dataset.quality_report
    return HistoricalDatasetRecord(
        dataset_id=dataset.dataset_id,
        partition=assignment.partition.value if assignment else "EXCLUDED",
        seal=assignment.seal if assignment else "",
        instrument=p.instrument,
        timeframe=p.timeframe,
        first_bar=q.first_bar,
        last_bar=q.last_bar,
        bars=q.rows,
        status=dataset.status.value,
        original_file_hash=p.original_file_hash,
        normalized_dataset_hash=p.normalized_dataset_hash,
        provider=p.provider,
        acquisition_method=p.acquisition_method,
        licensing_note=p.licensing_note,
        original_timezone=p.original_timezone,
        timezone_declared=p.timezone_declared,
        cost_basis=p.cost_basis,
        coverage_fraction=q.coverage_fraction,
        missing_bars=q.missing_bars,
        expected_bars=q.expected_bars,
        errors=[f"{f.code} x{f.count}" for f in q.errors],
        warnings=[f"{f.code} x{f.count}" for f in q.warnings],
    )


def to_json(result: HistoricalRun) -> str:
    import json

    def default(value: Any) -> Any:
        if hasattr(value, "value"):
            return value.value
        if hasattr(value, "isoformat"):
            return value.isoformat()
        if hasattr(value, "__dataclass_fields__"):
            return asdict(value)
        return str(value)

    return json.dumps(asdict(result), default=default, indent=2)
