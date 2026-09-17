"""The experiment: generate observations, score them, report what was found.

One entry point, one question. Every strategy is run over every TRAIN dataset,
every actionable signal becomes an observation, each observation is scored over
its family's horizons, and the result is described without anything being
fitted.

The run records its own identity -- code SHA, dataset hashes, horizon and cost
policy versions, partition assignment -- so a later analysis can say whether it
is comparable with this one rather than assuming it.
"""

from __future__ import annotations

import json
from dataclasses import asdict, dataclass, field
from pathlib import Path
from typing import Any

import pandas as pd

from vantage_quant import strategies
from vantage_quant.score_inventory import INVENTORY, INVENTORY_VERSION

from . import analyse, generate, horizons, outcome, partition


@dataclass
class StrategyReport:
    strategy_id: str
    strategy_version: int
    family: str
    observations: int
    buy: int
    sell: int
    by_regime: dict[str, int] = field(default_factory=dict)
    by_dataset: dict[str, int] = field(default_factory=dict)
    by_session: dict[str, int] = field(default_factory=dict)
    distribution: analyse.ScoreDistribution | None = None
    #: One relationship per horizon. ALL are reported; the "best" is named
    #: separately rather than being the only one shown, because reporting only
    #: the best horizon is how a result gets cherry-picked.
    relationships: list[analyse.RelationshipResult] = field(default_factory=list)
    best_horizon: str = ""
    classification: analyse.Classification = analyse.Classification.NO_OBSERVATIONS
    note: str = ""


@dataclass
class ResearchRun:
    run_id: str
    code_sha: str
    horizon_config_version: int
    cost_policy_version: int
    partition_version: int
    inventory_version: int
    assignments: list[partition.DatasetAssignment]
    reports: list[StrategyReport]
    total_observations: int


def fixture_dir() -> Path:
    here = Path(__file__).resolve()
    return here.parents[3] / "control-api" / "internal" / "replay" / "fixtures"


def discover_datasets() -> list[partition.DatasetAssignment]:
    """Every committed fixture, assigned chronologically."""
    import csv

    found: list[tuple[str, str, str, int, Path]] = []
    for path in sorted(fixture_dir().glob("*.csv")):
        with path.open() as handle:
            rows = list(csv.DictReader(handle))
        if not rows:
            continue
        found.append(
            (path.stem, rows[0]["timestamp"], rows[-1]["timestamp"], len(rows), path)
        )
    return partition.assign(found)


def run(*, run_id: str = "signal-research-1") -> ResearchRun:
    """Generate, score and describe. TRAIN only."""
    assignments = discover_datasets()
    explorable = partition.explorable(assignments)
    # The seal, enforced rather than trusted. This raises if a sealed dataset
    # ever reaches the analysis path.
    partition.guard_not_sealed(explorable)

    sha = generate.code_sha()
    costs = outcome.research_cost_model()

    loaded = [
        generate.load_fixture(fixture_dir() / f"{a.dataset_id}.csv", a)
        for a in explorable
    ]

    reports: list[StrategyReport] = []
    total = 0

    for _key, spec in sorted(strategies.REGISTRY.items()):
        signals: list[generate.StrategySignals] = []
        scored: dict[str, list[tuple[Any, outcome.SignalOutcome]]] = {}

        for bars in loaded:
            produced = generate.observe_dataset(spec, bars, sha=sha)
            # Warm-up is recorded by the generator and excluded here. It is
            # not evidence: indicators are still filling, and production
            # suppresses trading through the same window.
            analysable = [s for s in produced if s.observation.is_analysable()]
            signals.extend(analysable)

            for horizon in horizons.horizons_for(spec.family):
                for sig in analysable:
                    idx = int(
                        bars.frame.index.get_indexer(
                            pd.Index([sig.observation.bar_time])
                        )[0]
                    )
                    if idx < 0:
                        continue
                    # STRICTLY after the signal's own bar. This slice is the
                    # information boundary between observation and outcome.
                    future = bars.frame.iloc[idx + 1 :]
                    result = outcome.compute_outcome(
                        sig.observation,
                        future_bars=future,
                        horizon_id=horizon.horizon_id,
                        horizon_bars=horizon.bars,
                        suggested_stop=sig.suggested_stop,
                        suggested_target=sig.suggested_target,
                        costs=costs,
                    )
                    if result is None:
                        continue
                    scored.setdefault(horizon.horizon_id, []).append(
                        (sig.observation, result)
                    )

        total += len(signals)
        reports.append(_report_for(spec, signals, scored))

    return ResearchRun(
        run_id=run_id,
        code_sha=sha,
        horizon_config_version=horizons.HORIZON_CONFIG_VERSION,
        cost_policy_version=outcome.COST_POLICY_VERSION,
        partition_version=partition.PARTITION_VERSION,
        inventory_version=INVENTORY_VERSION,
        assignments=assignments,
        reports=reports,
        total_observations=total,
    )


def _report_for(
    spec: strategies.StrategySpec,
    signals: list[generate.StrategySignals],
    scored: dict[str, list[tuple[Any, outcome.SignalOutcome]]],
) -> StrategyReport:
    observations = [s.observation for s in signals]
    inv = INVENTORY.get(spec.key)

    report = StrategyReport(
        strategy_id=spec.key,
        strategy_version=1,
        family=spec.family,
        observations=len(observations),
        buy=sum(1 for o in observations if o.direction == "buy"),
        sell=sum(1 for o in observations if o.direction == "sell"),
    )
    if not observations:
        report.classification = analyse.Classification.NO_OBSERVATIONS
        report.note = _no_observation_reason(spec)
        return report

    for o in observations:
        report.by_regime[o.regime] = report.by_regime.get(o.regime, 0) + 1
        report.by_dataset[o.dataset_id] = report.by_dataset.get(o.dataset_id, 0) + 1
        report.by_session[o.session] = report.by_session.get(o.session, 0) + 1

    report.distribution = analyse.describe_scores(
        spec.key,
        [o.raw_score for o in observations],
        theoretical_min=inv.theoretical_min if inv else 0.0,
        theoretical_max=inv.theoretical_max if inv else 1.0,
    )

    best: analyse.RelationshipResult | None = None
    for horizon_id, pairs in sorted(scored.items()):
        obs = [p[0] for p in pairs]
        outs = [p[1] for p in pairs]
        scores = [o.raw_score for o in obs]
        raw = [r.forward_raw_return for r in outs]
        net = [r.forward_net_return for r in outs]
        mfe = [r.maximum_favorable_excursion for r in outs]
        mae = [r.maximum_adverse_excursion for r in outs]

        coefficient, note = analyse.spearman(scores, net)
        bins = analyse.quantile_bins(scores, raw, net, mfe, mae)
        usable = [b for b in bins if b.sufficient]
        separation = (
            usable[-1].mean_net_return - usable[0].mean_net_return
            if len(usable) >= 2
            else None
        )
        result = analyse.RelationshipResult(
            strategy_id=spec.key,
            horizon_id=horizon_id,
            count=len(pairs),
            spearman=coefficient,
            spearman_note=note,
            bins=bins,
            monotonicity=analyse.classify_monotonicity(bins, coefficient),
            quantile_separation_net=separation,
            mean_net_return=sum(net) / len(net) if net else 0.0,
            mean_raw_return=sum(raw) / len(raw) if raw else 0.0,
        )
        report.relationships.append(result)

        # "Best evidence" means the strongest measured ordering, chosen by
        # |spearman| among horizons that HAVE one. Every horizon is reported
        # regardless, so this names a focus rather than hiding the rest.
        if coefficient is not None and (
            best is None or best.spearman is None or abs(coefficient) > abs(best.spearman)
        ):
            best = result

    chosen = best or (report.relationships[0] if report.relationships else None)
    if chosen is not None:
        report.best_horizon = chosen.horizon_id
        report.classification = analyse.classify(chosen, observations=len(observations))
    else:
        report.classification = analyse.Classification.INSUFFICIENT_EVIDENCE
        report.note = "no horizon produced a complete outcome window"
    return report


def _no_observation_reason(spec: strategies.StrategySpec) -> str:
    """Why a strategy produced nothing, so it cannot vanish silently."""
    if spec.high_risk:
        return "high-risk research strategy; disabled and not expected to signal"
    if spec.key == "pre_event_blackout":
        return "only ever declines to trade, so it produces no directional signal"
    if spec.required_bars > 140:
        return (
            f"requires {spec.required_bars} bars and the TRAIN fixtures are "
            f"shorter, so it never reached its own minimum history"
        )
    return "produced no actionable signal on any TRAIN dataset"


def to_json(run_result: ResearchRun) -> str:
    """Structured output, so later work does not parse prose."""

    def default(value: Any) -> Any:
        if hasattr(value, "value"):  # Enum
            return value.value
        if hasattr(value, "__dataclass_fields__"):
            return asdict(value)
        return str(value)

    return json.dumps(asdict(run_result), default=default, indent=2)
