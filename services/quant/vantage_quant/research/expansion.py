"""How much evidence is there, really?

The previous experiment answered "not enough" with a raw count. That is the
easy half. This module answers the harder half: whether a large count is
actually a large amount of evidence.

Three things can make a big N mean little. A score that never varies ranks
nothing, however many times it fires. Signals that arrive in long contiguous
runs from one uninterrupted trend are one opportunity observed repeatedly, not
hundreds of independent ones. And a grid of strategies times horizons times
regimes will produce a striking number somewhere by chance alone.

So every count here comes with an effective count, every statistic with an
interval, and the whole tested grid is reported rather than its best cell.
"""

from __future__ import annotations

import math
from dataclasses import dataclass
from datetime import datetime, timedelta
from enum import StrEnum
from itertools import pairwise

import numpy as np

from . import verdict
from .verdict import (
    CalibrationVerdict,
    HistoricalSufficiency,
    RedesignFlag,
)

#: Fraction of its theoretical range a score must actually use before any
#: ordering claim is attempted. Below this it is a constant with noise.
MIN_SCORE_SPAN = 0.02

#: Bootstrap resamples. Fixed, and the seed is fixed too: a confidence interval
#: that moved between runs would be another source of noise in a milestone
#: about measuring noise.
BOOTSTRAP_RESAMPLES = 2000
BOOTSTRAP_SEED = 20280103


class StrategyRole(StrEnum):
    """What research question is meaningful for a strategy.

    Asking "does its score predict return?" of a risk filter is a category
    error: it does not emit a directional opinion, so the question has no
    answer and a null result would be misread as a failure.
    """

    ALPHA_DIRECTIONAL = "ALPHA_DIRECTIONAL"
    ALPHA_MEAN_REVERSION = "ALPHA_MEAN_REVERSION"
    ALPHA_BREAKOUT = "ALPHA_BREAKOUT"
    ALPHA_MOMENTUM = "ALPHA_MOMENTUM"
    ENSEMBLE = "ENSEMBLE"
    MODEL_FILTER = "MODEL_FILTER"
    RISK_FILTER = "RISK_FILTER"
    HIGH_RISK_RESEARCH = "HIGH_RISK_RESEARCH"
    OTHER = "OTHER"


#: Role per registered strategy, by key.
#:
#: Assigned from what each strategy DOES rather than from its family label:
#: pre_event_blackout sits in the event_driven family and is a veto, and
#: scoring it as alpha would ask a question it was never built to answer.
STRATEGY_ROLES: dict[str, StrategyRole] = {
    "ma_trend_crossover": StrategyRole.ALPHA_DIRECTIONAL,
    "donchian_breakout": StrategyRole.ALPHA_BREAKOUT,
    "rsi_mean_reversion": StrategyRole.ALPHA_MEAN_REVERSION,
    "macd_momentum": StrategyRole.ALPHA_MOMENTUM,
    "bollinger_zscore_reversion": StrategyRole.ALPHA_MEAN_REVERSION,
    "atr_volatility_regime": StrategyRole.ALPHA_BREAKOUT,
    "session_london_breakout": StrategyRole.ALPHA_BREAKOUT,
    "multi_timeframe_trend": StrategyRole.ALPHA_DIRECTIONAL,
    "pre_event_blackout": StrategyRole.RISK_FILTER,
    "ensemble_weighted_vote": StrategyRole.ENSEMBLE,
    "ml_direction_filter": StrategyRole.MODEL_FILTER,
    "grid_martingale_research": StrategyRole.HIGH_RISK_RESEARCH,
}


def role_for(key: str) -> StrategyRole:
    return STRATEGY_ROLES.get(key, StrategyRole.OTHER)


def scores_a_direction(role: StrategyRole) -> bool:
    """Whether "does the score predict return?" is a meaningful question."""
    return role in (
        StrategyRole.ALPHA_DIRECTIONAL,
        StrategyRole.ALPHA_MEAN_REVERSION,
        StrategyRole.ALPHA_BREAKOUT,
        StrategyRole.ALPHA_MOMENTUM,
        StrategyRole.ENSEMBLE,
        StrategyRole.MODEL_FILTER,
    )


class EvidenceClass(StrEnum):
    """How much a count is worth, as a research heuristic and not a law."""

    INSUFFICIENT = "INSUFFICIENT"
    EXPLORATORY_ONLY = "EXPLORATORY_ONLY"
    MODERATE_EVIDENCE = "MODERATE_EVIDENCE"
    CALIBRATION_CANDIDATE = "CALIBRATION_CANDIDATE"


def classify_evidence(effective_n: int) -> EvidenceClass:
    """Graded on EFFECTIVE n, never raw.

    Using the raw count here would let a thousand near-identical consecutive
    signals from one trend qualify as a calibration candidate, which is the
    exact overstatement this module exists to prevent.
    """
    if effective_n < 100:
        return EvidenceClass.INSUFFICIENT
    if effective_n < 300:
        return EvidenceClass.EXPLORATORY_ONLY
    if effective_n < 500:
        return EvidenceClass.MODERATE_EVIDENCE
    return EvidenceClass.CALIBRATION_CANDIDATE


@dataclass(frozen=True)
class SignalRate:
    """How often a strategy speaks, per eligible bar."""

    strategy_id: str
    eligible_bars: int
    signals: int
    buy: int
    sell: int

    @property
    def rate(self) -> float:
        return self.signals / self.eligible_bars if self.eligible_bars else 0.0

    def bars_required_for(self, target_observations: int) -> int | None:
        """Eligible bars needed to reach a target, at the measured rate.

        None when the strategy has never signalled: dividing by a zero rate
        would produce an infinity dressed as a plan.
        """
        if self.rate <= 0:
            return None
        return math.ceil(target_observations / self.rate)


@dataclass(frozen=True)
class EpisodeSummary:
    """Independence, approximately.

    An episode is a run of same-direction signals from one strategy with no
    more than `gap_bars` between consecutive members. Hundreds of signals
    inside one uninterrupted trend are one opportunity seen repeatedly;
    counting them as hundreds of independent samples would shrink every
    confidence interval by roughly the square root of a lie.
    """

    raw_n: int
    episodes: int
    largest_episode: int
    mean_episode: float

    @property
    def effective_n(self) -> int:
        """Conservative: the episode count.

        Deliberately the harsher of the available readings. Treating each
        episode as one observation understates evidence when signals really
        are independent, and overstating it is the failure that matters --
        an interval that is too narrow invites a conclusion the data cannot
        carry.
        """
        return self.episodes


def episode_labels(
    bar_times: list[datetime], directions: list[str], *, gap_bars: int = 6,
    bar_duration: timedelta = timedelta(hours=1),
) -> list[int]:
    """An episode id per signal, in the caller's original order.

    Returned rather than only counted because the interval machinery needs to
    resample EPISODES, not signals. Grouping is done in time order and the
    labels are then scattered back, so a caller can align them with whatever
    order it holds its observations in.
    """
    if not bar_times:
        return []

    order = sorted(range(len(bar_times)), key=lambda i: bar_times[i])
    max_gap = bar_duration * gap_bars

    labels = [0] * len(bar_times)
    current = 0
    labels[order[0]] = 0
    for prev, nxt in pairwise(order):
        same_direction = directions[prev] == directions[nxt]
        close_together = bar_times[nxt] - bar_times[prev] <= max_gap
        if not (same_direction and close_together):
            current += 1
        labels[nxt] = current
    return labels


def episodes(
    bar_times: list[datetime], directions: list[str], *, gap_bars: int = 6,
    bar_duration: timedelta = timedelta(hours=1),
) -> EpisodeSummary:
    """Group signals into episodes of contiguous same-direction activity."""
    if not bar_times:
        return EpisodeSummary(raw_n=0, episodes=0, largest_episode=0, mean_episode=0.0)

    labels = episode_labels(
        bar_times, directions, gap_bars=gap_bars, bar_duration=bar_duration
    )
    counts: dict[int, int] = {}
    for label in labels:
        counts[label] = counts.get(label, 0) + 1
    sizes = list(counts.values())

    return EpisodeSummary(
        raw_n=len(bar_times),
        episodes=len(sizes),
        largest_episode=max(sizes),
        mean_episode=sum(sizes) / len(sizes),
    )


@dataclass(frozen=True)
class Interval:
    """A bootstrap confidence interval, with its own provenance."""

    point: float
    lower: float
    upper: float
    resamples: int
    note: str = ""

    @property
    def excludes_zero(self) -> bool:
        return (self.lower > 0 and self.upper > 0) or (self.lower < 0 and self.upper < 0)


def _clusters(labels: list[int]) -> list[np.ndarray]:
    """Index groups, one per episode, for resampling whole episodes."""
    grouped: dict[int, list[int]] = {}
    for position, label in enumerate(labels):
        grouped.setdefault(label, []).append(position)
    return [np.asarray(v, dtype=int) for _, v in sorted(grouped.items())]


def bootstrap_mean(
    values: list[float], *, seed: int = BOOTSTRAP_SEED,
    clusters: list[int] | None = None,
) -> Interval | None:
    """A 95% percentile interval for the mean.

    When `clusters` is given, whole EPISODES are resampled rather than single
    signals. This is the difference between an interval built on 1214 signals
    and one built on the 404 independent opportunities they came from -- about
    a factor of 1.7 in width. Grading evidence on the effective count while
    building intervals on the raw one states a precision the data has not got,
    which is the specific way this kind of study overclaims.
    """
    if len(values) < 20:
        return None
    rng = np.random.default_rng(seed)
    arr = np.asarray(values, dtype=float)

    if clusters is None:
        draws = rng.integers(0, arr.size, size=(BOOTSTRAP_RESAMPLES, arr.size))
        means = arr[draws].mean(axis=1)
        note = ""
    else:
        groups = _clusters(clusters)
        if len(groups) < 10:
            return None
        picks = rng.integers(0, len(groups), size=(BOOTSTRAP_RESAMPLES, len(groups)))
        means = np.empty(BOOTSTRAP_RESAMPLES)
        for i in range(BOOTSTRAP_RESAMPLES):
            means[i] = arr[np.concatenate([groups[j] for j in picks[i]])].mean()
        note = f"clustered by episode ({len(groups)} episodes)"

    lo, hi = np.percentile(means, [2.5, 97.5])
    return Interval(
        point=float(arr.mean()), lower=float(lo), upper=float(hi),
        resamples=BOOTSTRAP_RESAMPLES, note=note,
    )


def bootstrap_rate(
    flags: list[bool], *, seed: int = BOOTSTRAP_SEED,
    clusters: list[int] | None = None,
) -> Interval | None:
    """A 95% percentile interval for a proportion."""
    if len(flags) < 20:
        return None
    return bootstrap_mean(
        [1.0 if f else 0.0 for f in flags], seed=seed, clusters=clusters
    )


def bootstrap_spearman(
    x: list[float], y: list[float], *, seed: int = BOOTSTRAP_SEED,
    clusters: list[int] | None = None,
) -> Interval | None:
    """A 95% interval for the rank correlation.

    Resampled in PAIRS, so the coefficient is recomputed on each resample and
    the interval describes uncertainty in the relationship rather than in
    either series alone. With `clusters`, whole episodes are drawn instead of
    single pairs -- see bootstrap_mean for why that matters.
    """
    from .analyse import spearman

    if len(x) < 30 or len(x) != len(y):
        return None
    point, note = spearman(x, y)
    if point is None:
        return None

    rng = np.random.default_rng(seed)
    xa, ya = np.asarray(x, dtype=float), np.asarray(y, dtype=float)
    groups = _clusters(clusters) if clusters is not None else None
    if groups is not None:
        if len(groups) < 10:
            return None
        note = (note + "; " if note else "") + (
            f"clustered by episode ({len(groups)} episodes)"
        )

    coefficients: list[float] = []
    for _ in range(BOOTSTRAP_RESAMPLES // 4):  # pairs are dearer to resample
        if groups is None:
            idx = rng.integers(0, xa.size, size=xa.size)
        else:
            picks = rng.integers(0, len(groups), size=len(groups))
            idx = np.concatenate([groups[j] for j in picks])
        value, _ = spearman(list(xa[idx]), list(ya[idx]))
        if value is not None:
            coefficients.append(value)
    if len(coefficients) < 50:
        return None
    lo, hi = np.percentile(coefficients, [2.5, 97.5])
    return Interval(
        point=point, lower=float(lo), upper=float(hi),
        resamples=len(coefficients), note=note,
    )


def classify(
    *,
    role: StrategyRole,
    score_span_used: float,
    monotonicity: str,
    mean_net_return: float,
    spearman_interval: Interval | None,
    raw_n: int,
    effective_n: int,
) -> tuple[CalibrationVerdict, str]:
    """One verdict for one strategy, naming ONE cause.

    Every verdict is decided here, including for strategies that never
    signalled. A caller that short-circuits an empty sample before reaching
    this function reports that more data would help a veto, which is false --
    no quantity of bars makes a veto emit a directional opinion.

    The ORDER of these checks is the design. Several could apply at once, and
    the one reported is the one a reader should act on first:

      role         -- asking a veto a directional question is a category error
      unobserved   -- an unsampled score is not a constant score
      constant     -- no N rescues a score that does not move
      raw count    -- too little of anything
      episodes     -- plenty of signals, too few independent ones
      shape        -- flat or non-monotone, measured with adequate power
      resolution   -- measured, cannot be resolved from zero
      direction    -- resolved, and pointing the wrong way
      cost         -- resolved, right way, still loses to costs
    """
    if not scores_a_direction(role):
        if role is StrategyRole.RISK_FILTER:
            return (
                CalibrationVerdict.NOT_APPLICABLE,
                "a veto rather than a directional opinion; 'does its score predict "
                "return' is not a question it was built to answer",
            )
        return (
            CalibrationVerdict.RESEARCH_ONLY,
            f"role {role.value} is not evaluated as directional alpha",
        )

    # An unobserved score is not a constant score. Without this, a strategy
    # that never spoke arrives at the span check with a span of 0.0 and is
    # reported NON_INFORMATIVE_SCORE -- a claim about a distribution that was
    # never sampled.
    if raw_n <= 0:
        return (
            CalibrationVerdict.INSUFFICIENT_DATA,
            "never produced an actionable signal, so its score was never observed",
        )

    # A score that never moves cannot rank anything, whatever N says. Checked
    # BEFORE the counts, so a large sample of identical scores cannot pass.
    if score_span_used < MIN_SCORE_SPAN:
        return (
            CalibrationVerdict.NON_INFORMATIVE_SCORE,
            f"the score used {score_span_used:.1%} of its possible range, so it "
            f"carries no ordering information to calibrate",
        )

    if raw_n < verdict.MIN_RAW_FOR_EPISODE_COMPLAINT:
        return (
            CalibrationVerdict.INSUFFICIENT_DATA,
            f"{raw_n} observations in total; collecting more bars is the action",
        )

    # Plenty of signals, too few INDEPENDENT ones. Split from INSUFFICIENT_DATA
    # because the remedy differs: more bars help only if they contain new
    # episodes rather than longer ones.
    if effective_n < verdict.MIN_EPISODES_FOR_ORDERING:
        return (
            CalibrationVerdict.INSUFFICIENT_INDEPENDENT_EPISODES,
            f"{raw_n} signals but only {effective_n} independent episodes; the "
            f"shortage is independence, not volume",
        )

    # An AFFIRMATIVE ordering is required, not merely the absence of a negative
    # verdict. Checking only for NON_MONOTONIC once let INSUFFICIENT_EVIDENCE
    # and FLAT pass as though monotonicity had been demonstrated, and produced
    # a false READY_FOR_CALIBRATION.
    if monotonicity in ("NON_MONOTONIC", "FLAT"):
        return (
            CalibrationVerdict.NON_MONOTONIC_SCORE,
            f"measured with {effective_n} independent episodes and the outcome "
            f"does not move consistently with the score ({monotonicity.lower()}); "
            f"a monotone calibration would be fitted to an ordering that is not "
            f"there",
        )

    if monotonicity not in ("MONOTONIC_POSITIVE", "WEAK_POSITIVE", "NEGATIVE"):
        return (
            CalibrationVerdict.ORDERING_NOT_ESTABLISHED,
            f"monotonicity is {monotonicity}: the score does not spread across "
            f"enough quantile bins to judge shape, despite {effective_n} "
            f"independent episodes. The distribution is the obstacle, not the "
            f"quantity",
        )

    if spearman_interval is not None and not spearman_interval.excludes_zero:
        return (
            CalibrationVerdict.ORDERING_NOT_ESTABLISHED,
            f"the rank correlation interval [{spearman_interval.lower:+.3f}, "
            f"{spearman_interval.upper:+.3f}] includes zero on {effective_n} "
            f"independent episodes, so the relationship cannot be resolved from "
            f"none",
        )

    # Resolved, and pointing the wrong way. Informative rather than absent: the
    # score means the opposite of what the platform renders it as.
    if monotonicity == "NEGATIVE" or (
        spearman_interval is not None and spearman_interval.point < 0
    ):
        return (
            CalibrationVerdict.NEGATIVE_ORDERING,
            "higher scores rank WORSE outcomes; the ordering exists and runs "
            "opposite to the direction the score is presented as meaning",
        )

    if mean_net_return <= 0:
        return (
            CalibrationVerdict.COST_NEGATIVE,
            "the ordering exists but the mean net outcome does not survive costs",
        )

    return (
        CalibrationVerdict.READY_FOR_CALIBRATION,
        f"{effective_n} independent episodes, a rank correlation excluding zero "
        f"and a positive net outcome",
    )


def redesign_flag(
    *,
    strategy_verdict: CalibrationVerdict,
    net_return_interval: Interval | None,
) -> tuple[RedesignFlag, str]:
    """Whether the SCORE or the SIGNAL RULE is the thing to revisit.

    A bad score is not a bad trading hypothesis, and the synthetic run found
    exactly that split: signals whose net outcome interval excluded zero,
    carrying a score that failed to rank those same signals. Nothing is
    redesigned here; this marks where to look.
    """
    if strategy_verdict in (
        CalibrationVerdict.RESEARCH_ONLY,
        CalibrationVerdict.NOT_APPLICABLE,
        CalibrationVerdict.READY_FOR_CALIBRATION,
    ):
        return RedesignFlag.NONE, ""

    if strategy_verdict in verdict.NEEDS_MORE_DATA:
        return RedesignFlag.NONE, "too little evidence to judge either part"

    signals_pay = (
        net_return_interval is not None
        and net_return_interval.excludes_zero
        and net_return_interval.point > 0
    )
    if signals_pay:
        return (
            RedesignFlag.SCORE_REDESIGN_CANDIDATE,
            "the entry rule produces a net outcome whose interval excludes zero "
            "while its score fails to rank those same signals; the confidence "
            "formula is the part that is not working",
        )
    return (
        RedesignFlag.SIGNAL_RESEARCH_REQUIRED,
        "neither the net outcome nor the score ordering is established, so the "
        "trading hypothesis itself is the open question rather than the number "
        "attached to it",
    )


def historical_sufficiency(
    *, role: StrategyRole, raw_n: int, effective_n: int
) -> tuple[HistoricalSufficiency, str]:
    """Whether REAL evidence exists in enough quantity to judge at all.

    Answered separately from calibration readiness because "no real data" and
    "real data showing nothing" are different states that one verdict merges.
    """
    if not scores_a_direction(role):
        return (
            HistoricalSufficiency.NOT_APPLICABLE,
            f"role {role.value} is not judged on directional ordering",
        )
    if raw_n <= 0:
        return (
            HistoricalSufficiency.NO_HISTORICAL_EVIDENCE,
            "no signal was produced on historical data",
        )
    if effective_n < verdict.MIN_EPISODES_FOR_ORDERING:
        return (
            HistoricalSufficiency.MORE_HISTORICAL_DATA_REQUIRED,
            f"{effective_n} independent historical episodes; "
            f"{verdict.MIN_EPISODES_FOR_ORDERING} is the floor for an ordering "
            f"claim",
        )
    return (
        HistoricalSufficiency.HISTORICAL_EVIDENCE_SUFFICIENT,
        f"{effective_n} independent historical episodes",
    )
