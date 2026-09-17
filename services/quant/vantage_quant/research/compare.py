"""Synthetic versus historical, side by side and never averaged.

The single most useful thing this line of work can produce is the discovery
that a relationship was an artefact of the generator. Averaging the two sources
into one headline would destroy exactly that finding, so they are compared
rather than combined, and every disagreement is classified.

A comparison is only meaningful when both sides were produced the same way.
`AnalysisVersions` travels with each run and `compare` refuses to line up two
runs whose analysis versions differ -- otherwise a difference in horizon
registry would read as a difference in the market.
"""

from __future__ import annotations

from dataclasses import dataclass

from .verdict import AnalysisVersions, CalibrationVerdict, Disagreement

#: How far the observed score range may differ between sources before the two
#: are treated as exercising different parts of the score function. A
#: calibration fitted on one would be extrapolating on the other.
SCORE_RANGE_TOLERANCE = 0.25


@dataclass(frozen=True)
class Side:
    """One source's findings for one strategy."""

    source_type: str
    verdict: str
    effective_n: int
    raw_n: int
    spearman: float | None
    spearman_excludes_zero: bool
    score_span_used: float
    mean_net_return: float
    net_survives_costs: bool
    regimes: tuple[str, ...] = ()


@dataclass(frozen=True)
class StrategyComparison:
    strategy_id: str
    synthetic: Side | None
    historical: Side | None
    disagreement: Disagreement
    detail: str

    @property
    def direction_agreement(self) -> bool | None:
        """Whether both sources rank outcomes the same way, where both can."""
        if not self._both_resolved():
            return None
        assert self.synthetic is not None and self.historical is not None
        a, b = self.synthetic.spearman, self.historical.spearman
        if a is None or b is None:
            return None
        return (a >= 0) == (b >= 0)

    @property
    def cost_survival_agreement(self) -> bool | None:
        if self.synthetic is None or self.historical is None:
            return None
        return self.synthetic.net_survives_costs == self.historical.net_survives_costs

    def _both_resolved(self) -> bool:
        return (
            self.synthetic is not None
            and self.historical is not None
            and self.synthetic.spearman_excludes_zero
            and self.historical.spearman_excludes_zero
        )


def classify_disagreement(
    synthetic: Side | None, historical: Side | None
) -> tuple[Disagreement, str]:
    """Name the way two sources differ, rather than reconciling them."""
    if synthetic is None or historical is None:
        return (
            Disagreement.NOT_COMPARABLE,
            "only one source produced a result for this strategy",
        )

    if historical.raw_n == 0 or synthetic.raw_n == 0:
        return (
            Disagreement.NOT_COMPARABLE,
            "one source produced no observations at all, so there is nothing to "
            "compare rather than a disagreement to explain",
        )

    synthetic_real = synthetic.spearman_excludes_zero
    historical_real = historical.spearman_excludes_zero

    # Score range first: if the two sources exercised different parts of the
    # score function they are not measuring the same thing, and any apparent
    # agreement or disagreement downstream is about different populations.
    if abs(synthetic.score_span_used - historical.score_span_used) > SCORE_RANGE_TOLERANCE:
        return (
            Disagreement.SCORE_RANGE_MISMATCH,
            f"the score used {synthetic.score_span_used:.1%} of its range on "
            f"synthetic data and {historical.score_span_used:.1%} on historical "
            f"data; a calibration fitted on one would extrapolate on the other",
        )

    if synthetic_real and not historical_real:
        return (
            Disagreement.SYNTHETIC_ONLY_RELATIONSHIP,
            "an ordering that the generator produced and the real market does "
            "not, which is what synthetic evidence is always at risk of "
            "manufacturing",
        )

    if historical_real and not synthetic_real:
        return (
            Disagreement.HISTORICAL_ONLY_RELATIONSHIP,
            "an ordering present in real data that the generator failed to "
            "produce; the generator lacks the condition this strategy exploits",
        )

    if synthetic_real and historical_real:
        a, b = synthetic.spearman, historical.spearman
        if a is not None and b is not None and (a >= 0) != (b >= 0):
            return (
                Disagreement.DIRECTION_REVERSAL,
                f"the ordering runs one way on synthetic data ({a:+.3f}) and the "
                f"other on historical data ({b:+.3f})",
            )

    if synthetic.net_survives_costs != historical.net_survives_costs:
        return (
            Disagreement.COST_SURVIVAL_MISMATCH,
            "the net outcome survives costs in one source and not the other",
        )

    return (
        Disagreement.AGREES,
        "both sources reach the same conclusion about the score's ordering",
    )


def compare(
    *,
    synthetic: dict[str, Side],
    historical: dict[str, Side],
    synthetic_versions: AnalysisVersions,
    historical_versions: AnalysisVersions,
) -> list[StrategyComparison]:
    """Line the two sources up, strategy by strategy.

    Refuses mismatched analysis versions rather than producing a comparison
    whose differences are partly methodological. That refusal is the point: a
    silently incomparable table is worse than no table.
    """
    if synthetic_versions != historical_versions:
        raise ValueError(
            "analysis versions differ between the two runs "
            f"({synthetic_versions} vs {historical_versions}); their differences "
            "would be partly methodological rather than about the market"
        )

    out: list[StrategyComparison] = []
    for key in sorted(set(synthetic) | set(historical)):
        left, right = synthetic.get(key), historical.get(key)
        state, detail = classify_disagreement(left, right)
        out.append(
            StrategyComparison(
                strategy_id=key,
                synthetic=left,
                historical=right,
                disagreement=state,
                detail=detail,
            )
        )
    return out


def side_from_report(report: object, *, source_type: str) -> Side:
    """Build a comparison Side from an ExpandedStrategyReport.

    Reads the horizon the verdict was drawn from rather than the best one, so
    the comparison describes the same trades the classification did.
    """
    horizons = list(getattr(report, "horizons", []))
    chosen = None
    for evidence in horizons:
        if evidence.spearman is None:
            continue
        if chosen is None or abs(evidence.spearman) > abs(chosen.spearman):
            chosen = evidence

    interval = chosen.spearman_ci if chosen is not None else None
    net_ci = chosen.net_return_ci if chosen is not None else None
    return Side(
        source_type=source_type,
        verdict=str(getattr(report, "readiness", CalibrationVerdict.INSUFFICIENT_DATA)),
        effective_n=int(getattr(report, "effective_n", 0)),
        raw_n=int(getattr(report, "raw_n", 0)),
        spearman=chosen.spearman if chosen is not None else None,
        spearman_excludes_zero=bool(interval is not None and interval.excludes_zero),
        score_span_used=float(getattr(report, "score_span_used", 0.0)),
        mean_net_return=chosen.mean_net_return if chosen is not None else 0.0,
        net_survives_costs=bool(
            net_ci is not None and net_ci.excludes_zero and net_ci.point > 0
        ),
        regimes=tuple(sorted(getattr(report, "by_regime", {}))),
    )
