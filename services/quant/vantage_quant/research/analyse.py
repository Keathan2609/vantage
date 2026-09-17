"""Does a higher raw score mean a better outcome?

Every number here is descriptive. Nothing is fitted, nothing is calibrated, and
no threshold is derived: this milestone establishes whether an ordering
relationship EXISTS, because calibration cannot rescue a score that carries no
useful ordering information.

Two disciplines run through all of it. Sample sizes are checked before a
conclusion is drawn and INSUFFICIENT_EVIDENCE is a first-class answer rather
than a gap to be filled with a number. And each strategy is analysed on its own
scale: the score inventory showed the formulas produce incompatible ranges, so
comparing "0.40-0.50" across strategies would compare different things.
"""

from __future__ import annotations

import math
from dataclasses import dataclass, field
from enum import StrEnum
from itertools import pairwise

import numpy as np

#: Below this, a strategy's relationship is reported as INSUFFICIENT_EVIDENCE.
#: Not a statistical threshold so much as an honesty floor: a Spearman
#: coefficient over twenty points is noise wearing a decimal point.
MIN_OBSERVATIONS_FOR_RELATIONSHIP = 100

#: Below this, a single quantile bin's mean is not reported as a finding.
MIN_OBSERVATIONS_PER_BIN = 20

#: Quantile bins. Five is the most that leaves usable counts at the sample
#: sizes these fixtures produce.
QUANTILE_BINS = 5


class Monotonicity(StrEnum):
    MONOTONIC_POSITIVE = "MONOTONIC_POSITIVE"
    WEAK_POSITIVE = "WEAK_POSITIVE"
    FLAT = "FLAT"
    NON_MONOTONIC = "NON_MONOTONIC"
    NEGATIVE = "NEGATIVE"
    INSUFFICIENT_EVIDENCE = "INSUFFICIENT_EVIDENCE"


class Classification(StrEnum):
    PROMISING_SCORE = "PROMISING_SCORE"
    WEAK_SCORE = "WEAK_SCORE"
    NON_MONOTONIC = "NON_MONOTONIC"
    COST_NEGATIVE = "COST_NEGATIVE"
    PREDICTIVE_BUT_COST_NEGATIVE = "PREDICTIVE_BUT_COST_NEGATIVE"
    INSUFFICIENT_EVIDENCE = "INSUFFICIENT_EVIDENCE"
    NO_OBSERVATIONS = "NO_OBSERVATIONS"


@dataclass(frozen=True)
class ScoreDistribution:
    """What a strategy's score actually emits, against what it could."""

    strategy_id: str
    count: int
    minimum: float
    p05: float
    p10: float
    p25: float
    median: float
    p75: float
    p90: float
    p95: float
    maximum: float
    mean: float
    stdev: float
    theoretical_min: float
    theoretical_max: float

    @property
    def observed_span(self) -> float:
        return self.maximum - self.minimum

    @property
    def theoretical_span(self) -> float:
        return self.theoretical_max - self.theoretical_min

    @property
    def span_used(self) -> float:
        """Fraction of the formula's possible range the market actually moved.

        A low number means the score barely varies in practice, so a threshold
        placed inside that range separates almost nothing.
        """
        if self.theoretical_span <= 0:
            return 0.0
        return self.observed_span / self.theoretical_span


@dataclass(frozen=True)
class QuantileBin:
    index: int
    lower: float
    upper: float
    count: int
    mean_raw_return: float
    median_raw_return: float
    mean_net_return: float
    median_net_return: float
    positive_raw_rate: float
    positive_net_rate: float
    mean_mfe: float
    mean_mae: float
    sufficient: bool


@dataclass(frozen=True)
class RelationshipResult:
    strategy_id: str
    horizon_id: str
    count: int
    spearman: float | None
    spearman_note: str
    bins: list[QuantileBin] = field(default_factory=list)
    monotonicity: Monotonicity = Monotonicity.INSUFFICIENT_EVIDENCE
    #: Net return of the top bin minus the bottom bin. The practical question:
    #: does acting only on high scores beat acting on low ones, after costs?
    quantile_separation_net: float | None = None
    mean_net_return: float = 0.0
    mean_raw_return: float = 0.0


def describe_scores(
    strategy_id: str, scores: list[float], *, theoretical_min: float, theoretical_max: float
) -> ScoreDistribution:
    """The observed distribution of one strategy's raw score."""
    arr = np.asarray(scores, dtype=float)
    if arr.size == 0:
        zero = 0.0
        return ScoreDistribution(
            strategy_id=strategy_id, count=0,
            minimum=zero, p05=zero, p10=zero, p25=zero, median=zero,
            p75=zero, p90=zero, p95=zero, maximum=zero, mean=zero, stdev=zero,
            theoretical_min=theoretical_min, theoretical_max=theoretical_max,
        )
    q = np.percentile(arr, [5, 10, 25, 50, 75, 90, 95])
    return ScoreDistribution(
        strategy_id=strategy_id,
        count=int(arr.size),
        minimum=float(arr.min()),
        p05=float(q[0]), p10=float(q[1]), p25=float(q[2]), median=float(q[3]),
        p75=float(q[4]), p90=float(q[5]), p95=float(q[6]),
        maximum=float(arr.max()),
        mean=float(arr.mean()),
        stdev=float(arr.std(ddof=1)) if arr.size > 1 else 0.0,
        theoretical_min=theoretical_min,
        theoretical_max=theoretical_max,
    )


def spearman(x: list[float], y: list[float]) -> tuple[float | None, str]:
    """Rank correlation, with an honest note when it cannot be computed.

    Rank-based rather than Pearson because the question is about ORDERING --
    "do higher scores rank better" -- and because the score scales are
    arbitrary, so a linear coefficient would be measuring the formula's units.

    Returns None with a reason rather than a number when the input cannot
    support one. A score with no variance produces an undefined correlation,
    and reporting 0.0 there would read as "measured, and no relationship" when
    the truth is "the score never moved".
    """
    if len(x) != len(y):
        raise ValueError("spearman: mismatched lengths")
    n = len(x)
    if n < MIN_OBSERVATIONS_FOR_RELATIONSHIP:
        return None, f"only {n} observations; {MIN_OBSERVATIONS_FOR_RELATIONSHIP} required"

    xa, ya = np.asarray(x, dtype=float), np.asarray(y, dtype=float)
    if np.allclose(xa, xa[0]):
        return None, "the score has no variance, so its ordering is undefined"
    if np.allclose(ya, ya[0]):
        return None, "every outcome is identical, so there is nothing to rank against"

    rx, ry = _rank(xa), _rank(ya)
    rxc, ryc = rx - rx.mean(), ry - ry.mean()
    denom = math.sqrt(float((rxc**2).sum()) * float((ryc**2).sum()))
    if denom == 0:
        return None, "degenerate ranks"
    return float((rxc * ryc).sum() / denom), ""


def _rank(a: np.ndarray) -> np.ndarray:
    """Average ranks, so ties do not invent an ordering that is not there."""
    order = a.argsort()
    ranks = np.empty(len(a), dtype=float)
    ranks[order] = np.arange(1, len(a) + 1, dtype=float)
    # Average tied groups.
    _, inverse, counts = np.unique(a, return_inverse=True, return_counts=True)
    sums = np.zeros(len(counts))
    np.add.at(sums, inverse, ranks)
    return (sums / counts)[inverse]


def quantile_bins(
    scores: list[float],
    raw_returns: list[float],
    net_returns: list[float],
    mfes: list[float],
    maes: list[float],
    *,
    bins: int = QUANTILE_BINS,
) -> list[QuantileBin]:
    """Split by score quantile and describe each bin's outcomes.

    Quantiles rather than fixed score ranges, because the inventory showed the
    formulas occupy incompatible ranges: a fixed 0.40-0.50 band is the middle
    of one strategy's range and outside another's entirely.
    """
    arr = np.asarray(scores, dtype=float)
    if arr.size == 0:
        return []

    edges = np.unique(np.percentile(arr, np.linspace(0, 100, bins + 1)))
    if len(edges) < 2:
        # Every score identical: one bin, and the analysis will say the score
        # has no variance rather than pretending to five.
        edges = np.array([arr.min(), arr.max() + 1e-12])

    out: list[QuantileBin] = []
    for i in range(len(edges) - 1):
        lo, hi = edges[i], edges[i + 1]
        last = i == len(edges) - 2
        mask = (arr >= lo) & (arr <= hi) if last else (arr >= lo) & (arr < hi)
        idx = np.flatnonzero(mask)
        if idx.size == 0:
            continue
        raw = np.asarray(raw_returns, dtype=float)[idx]
        net = np.asarray(net_returns, dtype=float)[idx]
        favourable = np.asarray(mfes, dtype=float)[idx]
        adverse = np.asarray(maes, dtype=float)[idx]
        out.append(
            QuantileBin(
                index=i + 1,
                lower=float(lo),
                upper=float(hi),
                count=int(idx.size),
                mean_raw_return=float(raw.mean()),
                median_raw_return=float(np.median(raw)),
                mean_net_return=float(net.mean()),
                median_net_return=float(np.median(net)),
                positive_raw_rate=float((raw > 0).mean()),
                positive_net_rate=float((net > 0).mean()),
                mean_mfe=float(favourable.mean()),
                mean_mae=float(adverse.mean()),
                sufficient=idx.size >= MIN_OBSERVATIONS_PER_BIN,
            )
        )
    return out


def classify_monotonicity(bins: list[QuantileBin], coefficient: float | None) -> Monotonicity:
    """Describe how outcome moves with score, across the bins.

    Descriptive only. It does not enable or disable anything, and a strategy
    classified NON_MONOTONIC is not thereby condemned -- it means the score's
    ordering is not usable as an ordering, which is a different and more
    specific finding than "the strategy is bad".
    """
    usable = [b for b in bins if b.sufficient]
    if len(usable) < 3:
        return Monotonicity.INSUFFICIENT_EVIDENCE

    means = [b.mean_net_return for b in usable]
    rises = sum(1 for a, b in pairwise(means) if b > a)
    falls = sum(1 for a, b in pairwise(means) if b < a)
    steps = len(means) - 1
    span = means[-1] - means[0]

    # "Flat" is measured against the outcomes' own scale rather than an
    # absolute: a 0.01% span is flat for an hourly gold return and enormous
    # for something else.
    scale = float(np.std(means)) if len(means) > 1 else 0.0
    if scale == 0 or abs(span) < 0.25 * scale:
        return Monotonicity.FLAT

    if rises == steps:
        return Monotonicity.MONOTONIC_POSITIVE
    if falls == steps:
        return Monotonicity.NEGATIVE
    if span > 0 and rises > falls:
        return Monotonicity.WEAK_POSITIVE
    if span < 0 and falls > rises:
        return Monotonicity.NEGATIVE
    if coefficient is not None and abs(coefficient) < 0.05:
        return Monotonicity.FLAT
    return Monotonicity.NON_MONOTONIC


def classify(result: RelationshipResult, *, observations: int) -> Classification:
    """The provisional verdict for one strategy at its best-evidence horizon."""
    if observations == 0:
        return Classification.NO_OBSERVATIONS
    if observations < MIN_OBSERVATIONS_FOR_RELATIONSHIP:
        return Classification.INSUFFICIENT_EVIDENCE
    if result.monotonicity is Monotonicity.INSUFFICIENT_EVIDENCE:
        return Classification.INSUFFICIENT_EVIDENCE

    directional = result.monotonicity in (
        Monotonicity.MONOTONIC_POSITIVE,
        Monotonicity.WEAK_POSITIVE,
    )

    # A signal can order outcomes correctly and still lose money once the
    # spread is paid. That is a different finding from "no information", and
    # collapsing the two would discard a score worth calibrating.
    if directional and result.mean_net_return <= 0:
        return Classification.PREDICTIVE_BUT_COST_NEGATIVE
    if directional:
        return Classification.PROMISING_SCORE
    if result.monotonicity is Monotonicity.NON_MONOTONIC:
        return Classification.NON_MONOTONIC
    if result.mean_net_return <= 0:
        return Classification.COST_NEGATIVE
    return Classification.WEAK_SCORE
