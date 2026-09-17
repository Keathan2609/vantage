"""The expanded experiment: long synthetic data, measured properly.

A NEW research run. The 460-observation baseline is untouched and stays
comparable -- it is the record of what the short fixtures could support, and
overwriting it would destroy the only before-picture there is.

The pipeline itself is unchanged. Same observation type, same outcome type,
same horizons, same cost model, same seal. Only the data is larger, and the
reporting now separates a raw count from an effective one.
"""

from __future__ import annotations

import os
import time
from concurrent.futures import ProcessPoolExecutor
from dataclasses import asdict, dataclass, field
from typing import Any

import pandas as pd

from vantage_quant import strategies
from vantage_quant.score_inventory import INVENTORY

from . import analyse, datasets, expansion, generate, horizons, outcome, partition


@dataclass
class HorizonEvidence:
    horizon_id: str
    horizon_bars: int
    count: int
    spearman: float | None
    spearman_ci: expansion.Interval | None
    mean_net_return: float
    net_return_ci: expansion.Interval | None
    positive_net_rate: float
    positive_net_ci: expansion.Interval | None
    quantile_separation_net: float | None
    monotonicity: str
    bins: list[analyse.QuantileBin] = field(default_factory=list)


@dataclass
class ExpandedStrategyReport:
    strategy_id: str
    role: str
    family: str
    signal_rate: expansion.SignalRate
    raw_n: int
    effective_n: int
    episodes: expansion.EpisodeSummary | None
    bars_required_for_500: int | None
    distribution: analyse.ScoreDistribution | None
    score_span_used: float
    #: EVERY horizon, never only the best. The complete grid is the guard
    #: against multiple-comparison mining.
    horizons: list[HorizonEvidence] = field(default_factory=list)
    by_regime: dict[str, int] = field(default_factory=dict)
    by_session: dict[str, int] = field(default_factory=dict)
    by_dataset: dict[str, int] = field(default_factory=dict)
    regime_evidence: dict[str, float | None] = field(default_factory=dict)
    component_evidence: dict[str, float | None] = field(default_factory=dict)
    evidence: str = expansion.EvidenceClass.INSUFFICIENT.value
    readiness: str = expansion.CalibrationReadiness.MORE_DATA_REQUIRED.value
    readiness_reason: str = ""
    note: str = ""


@dataclass
class ExpandedRun:
    run_id: str
    code_sha: str
    generator_version: int
    horizon_config_version: int
    cost_policy_version: int
    partition_version: int
    source_type: str
    bars_per_condition: int
    total_bars: int
    train_bars: int
    assignments: list[partition.DatasetAssignment]
    reports: list[ExpandedStrategyReport]
    runtime_seconds: float
    historical_status: str


def _dataset_assignments(specs: list[datasets.DatasetSpec]) -> list[partition.DatasetAssignment]:
    """Hash and partition BEFORE any outcome is looked at.

    The order matters and is the point of doing it here: generate, hash,
    assign, then analyse. Examining outcomes and then choosing which data
    becomes TEST is the failure a holdout exists to prevent.
    """
    built = [datasets.generate(s) for s in specs]
    rows = [
        (
            d.spec.dataset_id,
            d.frame.index[0].isoformat(),
            d.frame.index[-1].isoformat(),
            len(d.frame),
            d.dataset_hash,
        )
        for d in built
    ]
    ordered = sorted(rows, key=lambda r: r[1])
    n = len(ordered)
    train_end = int(n * partition.TRAIN_FRACTION)
    validation_end = train_end + int(n * partition.VALIDATION_FRACTION)

    out: list[partition.DatasetAssignment] = []
    for i, (dataset_id, first, last, bars, digest) in enumerate(ordered):
        if i < train_end:
            part = partition.Partition.TRAIN
        elif i < validation_end:
            part = partition.Partition.VALIDATION
        else:
            part = partition.Partition.TEST
        out.append(
            partition.DatasetAssignment(
                dataset_id=dataset_id, partition=part, first_bar=first,
                last_bar=last, bars=bars, dataset_hash=digest,
                seal="SEALED_FOR_OUT_OF_SAMPLE" if part is partition.Partition.TEST else "",
            )
        )
    return out


def _observe_one(
    payload: tuple[str, datasets.DatasetSpec, str, str],
) -> tuple[str, int, list[Any]]:
    """One (strategy, dataset) task, for the worker pool.

    Takes a SPEC rather than a frame: regenerating from a seed inside the
    worker is cheaper than pickling megabytes of bars across a process
    boundary, and determinism makes the two identical.
    """
    key, spec, digest, sha = payload
    built = datasets.generate(spec)
    bars = generate.DatasetBars(
        dataset_id=spec.dataset_id, dataset_hash=digest, frame=built.frame
    )
    produced = generate.observe_dataset(strategies.REGISTRY[key], bars, sha=sha)
    analysable = [s for s in produced if s.observation.is_analysable()]

    # Bars on which this strategy COULD have spoken: past its own declared
    # history, past the research warm-up, and with a next bar to fill at.
    # Measured rather than assumed, because a signal rate against the wrong
    # denominator produces a required-data estimate that is wrong by the same
    # factor.
    floor = max(strategies.REGISTRY[key].required_bars, generate.WARMUP_BARS)
    eligible = max(0, len(built.frame) - floor - 1)

    costs = outcome.research_cost_model()
    spec_obj = strategies.REGISTRY[key]
    scored: list[Any] = []
    for horizon in horizons.horizons_for(spec_obj.family):
        for sig in analysable:
            idx = int(built.frame.index.get_indexer(pd.Index([sig.observation.bar_time]))[0])
            if idx < 0:
                continue
            result = outcome.compute_outcome(
                sig.observation,
                future_bars=built.frame.iloc[idx + 1 :],
                horizon_id=horizon.horizon_id,
                horizon_bars=horizon.bars,
                suggested_stop=sig.suggested_stop,
                suggested_target=sig.suggested_target,
                costs=costs,
            )
            if result is not None:
                scored.append((sig.observation, result))
    return key, eligible, [(s.observation, None) for s in analysable] + scored


def run(
    *,
    bars_per_condition: int = 1200,
    run_id: str = "signal-research-2-expanded",
    workers: int | None = None,
) -> ExpandedRun:
    started = time.monotonic()
    specs = datasets.research_suite(bars_per_condition=bars_per_condition)
    assignments = _dataset_assignments(specs)

    explorable = partition.explorable(assignments)
    partition.guard_not_sealed(explorable)
    train_ids = {a.dataset_id: a for a in explorable}
    train_specs = [s for s in specs if s.dataset_id in train_ids]

    sha = generate.code_sha()
    tasks = [
        (key, spec, train_ids[spec.dataset_id].dataset_hash, sha)
        for key in sorted(strategies.REGISTRY)
        for spec in train_specs
    ]

    collected: dict[str, list[Any]] = {key: [] for key in strategies.REGISTRY}
    eligible_bars: dict[str, int] = dict.fromkeys(strategies.REGISTRY, 0)
    max_workers = workers or min(12, (os.cpu_count() or 4))
    with ProcessPoolExecutor(max_workers=max_workers) as pool:
        for key, eligible, rows in pool.map(_observe_one, tasks, chunksize=1):
            collected[key].extend(rows)
            eligible_bars[key] += eligible

    reports = [
        _report(strategies.REGISTRY[key], collected[key], eligible_bars[key])
        for key in sorted(strategies.REGISTRY)
    ]

    from . import historical

    return ExpandedRun(
        run_id=run_id,
        code_sha=sha,
        generator_version=datasets.GENERATOR_VERSION,
        horizon_config_version=horizons.HORIZON_CONFIG_VERSION,
        cost_policy_version=outcome.COST_POLICY_VERSION,
        partition_version=partition.PARTITION_VERSION,
        source_type=datasets.SourceType.SYNTHETIC_CONTROLLED.value,
        bars_per_condition=bars_per_condition,
        total_bars=sum(a.bars for a in assignments),
        train_bars=sum(a.bars for a in explorable),
        assignments=assignments,
        reports=reports,
        runtime_seconds=time.monotonic() - started,
        historical_status=historical.status(),
    )


def _report(
    spec: strategies.StrategySpec, rows: list[Any], eligible_bars: int
) -> ExpandedStrategyReport:
    observations = [o for o, r in rows if r is None]
    scored = [(o, r) for o, r in rows if r is not None]
    role = expansion.role_for(spec.key)
    inv = INVENTORY.get(spec.key)

    rate = expansion.SignalRate(
        strategy_id=spec.key,
        eligible_bars=eligible_bars,
        signals=len(observations),
        buy=sum(1 for o in observations if o.direction == "buy"),
        sell=sum(1 for o in observations if o.direction == "sell"),
    )

    report = ExpandedStrategyReport(
        strategy_id=spec.key,
        role=role.value,
        family=spec.family,
        signal_rate=rate,
        raw_n=len(observations),
        effective_n=0,
        episodes=None,
        bars_required_for_500=None,
        distribution=None,
        score_span_used=0.0,
    )

    report.bars_required_for_500 = rate.bars_required_for(500)

    if not observations:
        report.note = (
            f"no actionable signal across {eligible_bars} eligible TRAIN bars"
        )
        # Through readiness() rather than around it. A veto that never emits a
        # directional signal is NOT_APPLICABLE, not short of data, and only the
        # role knows which.
        verdict, reason = expansion.readiness(
            role=role,
            evidence=expansion.EvidenceClass.INSUFFICIENT,
            score_span_used=0.0,
            monotonicity="INSUFFICIENT_EVIDENCE",
            mean_net_return=0.0,
            spearman_interval=None,
            raw_n=0,
        )
        report.readiness = verdict.value
        report.readiness_reason = reason
        return report

    summary = expansion.episodes(
        [o.bar_time for o in observations], [o.direction for o in observations]
    )
    report.episodes = summary
    report.effective_n = summary.effective_n

    # Episode id per observation, so every interval below resamples episodes
    # rather than signals. Keyed by (dataset, bar time) because a strategy
    # emits at most one observation per bar and the datasets do not overlap
    # in time.
    labels = expansion.episode_labels(
        [o.bar_time for o in observations], [o.direction for o in observations]
    )
    episode_of = {
        (o.dataset_id, o.bar_time): label
        for o, label in zip(observations, labels, strict=True)
    }

    for o in observations:
        report.by_regime[o.regime] = report.by_regime.get(o.regime, 0) + 1
        report.by_session[o.session] = report.by_session.get(o.session, 0) + 1
        report.by_dataset[o.dataset_id] = report.by_dataset.get(o.dataset_id, 0) + 1

    report.distribution = analyse.describe_scores(
        spec.key,
        [o.raw_score for o in observations],
        theoretical_min=inv.theoretical_min if inv else 0.0,
        theoretical_max=inv.theoretical_max if inv else 1.0,
    )
    report.score_span_used = report.distribution.span_used

    by_horizon: dict[str, list[tuple[Any, Any]]] = {}
    for o, r in scored:
        by_horizon.setdefault(r.horizon_id, []).append((o, r))

    best_ci: expansion.Interval | None = None
    best_mono = "INSUFFICIENT_EVIDENCE"
    best_net = 0.0
    for horizon in horizons.horizons_for(spec.family):
        pairs = by_horizon.get(horizon.horizon_id, [])
        if not pairs:
            continue
        scores = [o.raw_score for o, _ in pairs]
        net = [r.forward_net_return for _, r in pairs]
        raw = [r.forward_raw_return for _, r in pairs]
        mfe = [r.maximum_favorable_excursion for _, r in pairs]
        mae = [r.maximum_adverse_excursion for _, r in pairs]
        positive = [r.positive_net_outcome for _, r in pairs]

        clusters = [episode_of[(o.dataset_id, o.bar_time)] for o, _ in pairs]

        coefficient, _note = analyse.spearman(scores, net)
        bins = analyse.quantile_bins(scores, raw, net, mfe, mae)
        usable = [b for b in bins if b.sufficient]
        evidence = HorizonEvidence(
            horizon_id=horizon.horizon_id,
            horizon_bars=horizon.bars,
            count=len(pairs),
            spearman=coefficient,
            spearman_ci=expansion.bootstrap_spearman(scores, net, clusters=clusters),
            mean_net_return=sum(net) / len(net),
            net_return_ci=expansion.bootstrap_mean(net, clusters=clusters),
            positive_net_rate=sum(1 for p in positive if p) / len(positive),
            positive_net_ci=expansion.bootstrap_rate(positive, clusters=clusters),
            quantile_separation_net=(
                usable[-1].mean_net_return - usable[0].mean_net_return
                if len(usable) >= 2 else None
            ),
            monotonicity=analyse.classify_monotonicity(bins, coefficient).value,
            bins=bins,
        )
        report.horizons.append(evidence)

        if coefficient is not None and (
            best_ci is None or abs(coefficient) > abs(best_ci.point)
        ):
            best_ci = evidence.spearman_ci
            best_mono = evidence.monotonicity
            best_net = evidence.mean_net_return

    # Regime-conditional ordering, where each regime has enough to say anything.
    longest = max(horizons.horizons_for(spec.family), key=lambda h: h.bars).horizon_id
    for regime in sorted(report.by_regime):
        pairs = [
            (o, r) for o, r in by_horizon.get(longest, []) if o.regime == regime
        ]
        if len(pairs) < analyse.MIN_OBSERVATIONS_FOR_RELATIONSHIP:
            report.regime_evidence[regime] = None
            continue
        coefficient, _ = analyse.spearman(
            [o.raw_score for o, _ in pairs], [r.forward_net_return for _, r in pairs]
        )
        report.regime_evidence[regime] = coefficient

    # Does a COMPONENT rank outcomes better than the combined score? For a
    # formula that averages a real value with a constant, it may.
    component_names = sorted(
        {name for o in observations for name in o.score_components}
    )
    for name in component_names[:8]:
        pairs = [
            (o, r) for o, r in by_horizon.get(longest, []) if name in o.score_components
        ]
        if len(pairs) < analyse.MIN_OBSERVATIONS_FOR_RELATIONSHIP:
            report.component_evidence[name] = None
            continue
        coefficient, _ = analyse.spearman(
            [o.score_components[name] for o, _ in pairs],
            [r.forward_net_return for _, r in pairs],
        )
        report.component_evidence[name] = coefficient

    report.evidence = expansion.classify_evidence(report.effective_n).value
    verdict, reason = expansion.readiness(
        role=role,
        evidence=expansion.EvidenceClass(report.evidence),
        score_span_used=report.score_span_used,
        monotonicity=best_mono,
        mean_net_return=best_net,
        spearman_interval=best_ci,
        raw_n=report.raw_n,
    )
    report.readiness = verdict.value
    report.readiness_reason = reason
    return report


def to_dict(result: ExpandedRun) -> dict[str, Any]:
    return asdict(result)


def to_json(result: ExpandedRun) -> str:
    """Structured output, so the report is written from data rather than prose.

    Enum members and datetimes are rendered explicitly. ``asdict`` leaves
    StrEnum values as enum instances, and the default encoder would fail on
    them -- silently stringifying everything with ``default=str`` instead
    would also stringify a float that should stay a number.
    """
    import json

    def default(value: Any) -> Any:
        if hasattr(value, "value"):  # Enum
            return value.value
        if hasattr(value, "isoformat"):  # datetime
            return value.isoformat()
        if hasattr(value, "__dataclass_fields__"):
            return asdict(value)
        return str(value)

    return json.dumps(to_dict(result), default=default, indent=2)
