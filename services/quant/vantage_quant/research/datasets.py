"""Long, deterministic research datasets — and what they are NOT.

The previous experiment produced 460 observations across 900 bars and could not
answer its own question: no strategy reached the hundred observations a rank
correlation needs. The fixtures were built to exercise the trading pipeline,
not to support inference, and 140-bar datasets against 120-to-150-bar warm-ups
leave roughly twenty usable signal bars each.

This module generates materially longer data. It does NOT make the strategies
more permissive: required_bars, signal conditions, thresholds, regimes and
horizons are all untouched. More data, not looser rules.

# Synthetic is not market evidence

Everything here is ``SYNTHETIC_CONTROLLED``. A trend strategy that scores well
on a generated trend has demonstrated RESPONSIVENESS TO TREND and nothing more:
the generator and the strategy share a model of what a trend is, so the result
partly measures that agreement rather than the market. Synthetic data proves
pipeline correctness, strategy responsiveness, score range behaviour and
coverage. It cannot establish predictive value, real-world calibration or
trading edge, and no report built on it may claim otherwise.

# Why these are generated rather than committed

CLAUDE.md forbids committing large market datasets; the fourteen replay
fixtures are a deliberate 170 KB exception. Sixteen conditions at thousands of
bars each is megabytes, so the GENERATOR is committed and the data is produced
on demand. Determinism is what makes that safe: the same version and seed
produce the same bars, and the dataset hash proves it.
"""

from __future__ import annotations

import hashlib
from dataclasses import dataclass
from datetime import UTC, datetime, timedelta
from enum import StrEnum

import numpy as np
import pandas as pd

#: Bumped whenever generation LOGIC changes. A dataset records it, so bars
#: produced by two versions are never silently compared.
GENERATOR_VERSION = 1


class SourceType(StrEnum):
    """Where a dataset's bars came from, and therefore what they can prove."""

    #: Generated here. Proves responsiveness and coverage; proves no edge.
    SYNTHETIC_CONTROLLED = "SYNTHETIC_CONTROLLED"
    #: Real recorded market data. Required before any claim about predictive
    #: value or live behaviour.
    HISTORICAL_MARKET = "HISTORICAL_MARKET"


class MarketCondition(StrEnum):
    CLEAN_TREND_UP = "CLEAN_TREND_UP"
    CLEAN_TREND_DOWN = "CLEAN_TREND_DOWN"
    WEAK_TREND = "WEAK_TREND"
    RANGE_LOW_VOL = "RANGE_LOW_VOL"
    RANGE_HIGH_VOL = "RANGE_HIGH_VOL"
    VOLATILITY_EXPANSION = "VOLATILITY_EXPANSION"
    VOLATILITY_CONTRACTION = "VOLATILITY_CONTRACTION"
    BREAKOUT_SUCCESS = "BREAKOUT_SUCCESS"
    FALSE_BREAKOUT = "FALSE_BREAKOUT"
    MOMENTUM_ACCELERATION = "MOMENTUM_ACCELERATION"
    MOMENTUM_DECAY = "MOMENTUM_DECAY"
    TREND_REVERSAL = "TREND_REVERSAL"
    SESSION_BREAKOUT = "SESSION_BREAKOUT"
    MIXED_REGIME = "MIXED_REGIME"
    EVENT_SHOCK = "EVENT_SHOCK"
    POST_EVENT_NORMALIZATION = "POST_EVENT_NORMALIZATION"


@dataclass(frozen=True)
class DatasetSpec:
    """Everything needed to reproduce one dataset exactly."""

    dataset_id: str
    condition: MarketCondition
    bars: int
    seed: int
    instrument: str = "XAUUSD.m"
    timeframe: str = "1h"
    start: datetime = datetime(2028, 1, 3, 0, tzinfo=UTC)
    start_price: float = 2650.0
    source_type: SourceType = SourceType.SYNTHETIC_CONTROLLED
    generator_version: int = GENERATOR_VERSION


@dataclass(frozen=True)
class GeneratedDataset:
    spec: DatasetSpec
    frame: pd.DataFrame
    dataset_hash: str


# --- the price process ------------------------------------------------------
#
# Every condition is expressed as a per-bar DRIFT and VOLATILITY schedule.
# Keeping the shape in one place means a condition differs from its neighbours
# only in those two series, which is what makes the set comparable: a result
# that varies between conditions varies because of drift and volatility rather
# than because each generator was written differently.


def _schedules(
    condition: MarketCondition, n: int, rng: np.random.Generator
) -> tuple[np.ndarray, np.ndarray]:
    """Per-bar drift and volatility for one condition."""
    t = np.linspace(0.0, 1.0, n)
    base_vol = 0.0016  # hourly, roughly gold-like

    drift = np.zeros(n)
    vol = np.full(n, base_vol)

    if condition is MarketCondition.CLEAN_TREND_UP:
        drift = np.full(n, 0.00055)
    elif condition is MarketCondition.CLEAN_TREND_DOWN:
        drift = np.full(n, -0.00055)
    elif condition is MarketCondition.WEAK_TREND:
        # Real enough to be a trend, weak enough that a crossover hesitates.
        drift = np.full(n, 0.00012)
    elif condition is MarketCondition.RANGE_LOW_VOL:
        vol = np.full(n, base_vol * 0.45)
    elif condition is MarketCondition.RANGE_HIGH_VOL:
        vol = np.full(n, base_vol * 1.9)
    elif condition is MarketCondition.VOLATILITY_EXPANSION:
        vol = base_vol * np.linspace(0.4, 2.6, n)
    elif condition is MarketCondition.VOLATILITY_CONTRACTION:
        vol = base_vol * np.linspace(2.6, 0.4, n)
    elif condition is MarketCondition.BREAKOUT_SUCCESS:
        # Coil, then break and run. Repeated so one dataset holds many
        # breakouts rather than one: a single episode is one observation.
        cycles = max(1, n // 400)
        for c in range(cycles):
            lo, hi = c * n // cycles, (c + 1) * n // cycles
            span = hi - lo
            coil, run = lo + int(span * 0.6), hi
            vol[lo:coil] = base_vol * 0.5
            drift[lo:coil] = 0.0
            vol[coil:run] = base_vol * 1.6
            drift[coil:run] = 0.0009 if c % 2 == 0 else -0.0009
    elif condition is MarketCondition.FALSE_BREAKOUT:
        cycles = max(1, n // 400)
        for c in range(cycles):
            lo, hi = c * n // cycles, (c + 1) * n // cycles
            span = hi - lo
            coil = lo + int(span * 0.6)
            poke = coil + int(span * 0.12)
            vol[lo:coil] = base_vol * 0.5
            vol[coil:poke] = base_vol * 1.5
            drift[coil:poke] = 0.0011
            # and straight back through the range
            drift[poke:hi] = -0.0009
    elif condition is MarketCondition.MOMENTUM_ACCELERATION:
        drift = 0.0002 + 0.0009 * t
        vol = base_vol * (0.8 + 0.9 * t)
    elif condition is MarketCondition.MOMENTUM_DECAY:
        drift = 0.0011 * (1.0 - t)
        vol = base_vol * (1.7 - 0.8 * t)
    elif condition is MarketCondition.TREND_REVERSAL:
        cycles = max(1, n // 600)
        for c in range(cycles):
            lo, hi = c * n // cycles, (c + 1) * n // cycles
            mid = (lo + hi) // 2
            drift[lo:mid] = 0.00055
            drift[mid:hi] = -0.00055
    elif condition is MarketCondition.SESSION_BREAKOUT:
        # Quiet Asia, then a London push. The hour pattern is applied below;
        # here only the amplitude is set.
        vol = np.full(n, base_vol * 0.9)
    elif condition is MarketCondition.MIXED_REGIME:
        block = 150
        for i in range(0, n, block):
            phase = (i // block) % 4
            end = min(i + block, n)
            if phase == 0:
                drift[i:end], vol[i:end] = 0.0005, base_vol
            elif phase == 1:
                drift[i:end], vol[i:end] = 0.0, base_vol * 0.5
            elif phase == 2:
                drift[i:end], vol[i:end] = -0.0005, base_vol * 1.4
            else:
                drift[i:end], vol[i:end] = 0.0, base_vol * 2.0
    elif condition is MarketCondition.EVENT_SHOCK:
        # Calm, a violent repricing, calm again. Several per dataset.
        shocks = max(1, n // 500)
        for s in range(shocks):
            at = int((s + 0.5) * n / shocks)
            vol[max(0, at - 2) : at + 6] = base_vol * 6.0
            drift[at : at + 4] = 0.004 if s % 2 == 0 else -0.004
    elif condition is MarketCondition.POST_EVENT_NORMALIZATION:
        shocks = max(1, n // 500)
        for s in range(shocks):
            at = int((s + 0.2) * n / shocks)
            tail = min(n, at + 120)
            vol[at:tail] = base_vol * np.linspace(5.0, 1.0, tail - at)
            drift[at : at + 3] = 0.003 if s % 2 == 0 else -0.003

    # Volatility CLUSTERING on top of every condition. Real markets do not
    # have flat volatility, and a generator that does would make every
    # indicator look better behaved than it is.
    cluster = np.ones(n)
    shock = rng.normal(0.0, 0.25, n)
    for i in range(1, n):
        cluster[i] = 0.92 * cluster[i - 1] + 0.08 * (1.0 + shock[i])
    vol = vol * np.clip(cluster, 0.45, 2.2)

    return drift, vol


def _session_for(hour: int) -> str:
    """The venue session an hour falls in. UTC in, label out."""
    if 0 <= hour < 7:
        return "tokyo"
    if 7 <= hour < 12:
        return "london"
    if 12 <= hour < 16:
        return "london_new_york"
    if 16 <= hour < 21:
        return "new_york"
    return "sydney"


def generate(spec: DatasetSpec) -> GeneratedDataset:
    """Produce one dataset, deterministically from its seed."""
    rng = np.random.default_rng(spec.seed)
    n = spec.bars
    drift, vol = _schedules(spec.condition, n, rng)

    timestamps = [spec.start + timedelta(hours=i) for i in range(n)]
    hours = np.array([ts.hour for ts in timestamps])

    # Session shapes volatility for every condition, not only the session one:
    # a generator with a flat 24-hour profile would let a session strategy
    # find a break at 3am.
    session_scale = np.where(
        (hours >= 7) & (hours < 16), 1.35, np.where((hours < 7), 0.6, 0.95)
    )
    if spec.condition is MarketCondition.SESSION_BREAKOUT:
        # A pronounced London push, so the session strategy has real events to
        # find rather than noise that happens to clear a range.
        session_scale = np.where(
            (hours >= 7) & (hours < 10), 2.4, np.where(hours < 7, 0.35, 0.9)
        )
        drift = drift + np.where((hours >= 7) & (hours < 10), 0.0016, 0.0)
    vol = vol * session_scale

    # Pullbacks: a mean-reverting component so trends breathe. Without it a
    # trend dataset is a straight line and every trend strategy looks perfect.
    pullback = np.zeros(n)
    for i in range(1, n):
        pullback[i] = 0.82 * pullback[i - 1] + rng.normal(0.0, 0.35)
    returns = drift + vol * (rng.normal(0.0, 1.0, n) + 0.45 * pullback)

    # Occasional gaps, at session boundaries only, where a real venue gaps.
    gap_candidates = np.flatnonzero(hours == 0)
    if gap_candidates.size:
        chosen = rng.choice(
            gap_candidates, size=max(1, gap_candidates.size // 8), replace=False
        )
        returns[chosen] += rng.normal(0.0, 0.004, chosen.size)

    closes = spec.start_price * np.exp(np.cumsum(returns))

    # OHLC from a sub-path, so the high and low are places price actually
    # went rather than a fixed cuff around the close. A synthetic bar whose
    # range is a constant multiple of its body makes every ATR identical.
    steps = 6
    opens = np.empty(n)
    highs = np.empty(n)
    lows = np.empty(n)
    prev = spec.start_price
    for i in range(n):
        sub = prev * np.exp(
            np.cumsum(rng.normal(returns[i] / steps, vol[i] / np.sqrt(steps), steps))
        )
        opens[i] = prev
        highs[i] = max(prev, float(sub.max()))
        lows[i] = min(prev, float(sub.min()))
        closes[i] = float(sub[-1])
        prev = closes[i]

    # Spread varies with session and volatility, because it does.
    spread = 0.00012 * (1.0 + 1.6 * (vol / vol.mean())) * np.where(hours < 7, 1.5, 1.0)

    frame = pd.DataFrame(
        {
            "open": np.round(opens, 2),
            "high": np.round(highs, 2),
            "low": np.round(lows, 2),
            "close": np.round(closes, 2),
            "volume": np.round(800 + 600 * (vol / vol.mean()), 0),
        },
        index=pd.DatetimeIndex(timestamps, name="timestamp"),
    )
    frame.attrs["session"] = [_session_for(h) for h in hours]
    frame.attrs["spread_fraction"] = [float(s) for s in np.round(spread, 8)]

    return GeneratedDataset(
        spec=spec, frame=frame, dataset_hash=_hash_frame(spec, frame)
    )


def _hash_frame(spec: DatasetSpec, frame: pd.DataFrame) -> str:
    """Content hash covering the bars AND the identity that produced them."""
    digest = hashlib.sha256()
    digest.update(
        f"{spec.generator_version}|{spec.condition}|{spec.seed}|{spec.instrument}|"
        f"{spec.timeframe}|{spec.start.isoformat()}|{spec.start_price}".encode()
    )
    digest.update(
        frame[["open", "high", "low", "close", "volume"]]
        .to_csv(float_format="%.4f")
        .encode()
    )
    return digest.hexdigest()[:32]


def research_suite(*, bars_per_condition: int, base_seed: int = 20280103) -> list[DatasetSpec]:
    """The full condition set, laid end to end in time.

    Chronological and non-overlapping, so the partition that follows is a
    straight split of an ordered list rather than a choice. Each condition gets
    its own seed derived from the base, so regenerating one does not disturb
    the others.
    """
    specs: list[DatasetSpec] = []
    cursor = datetime(2028, 1, 3, 0, tzinfo=UTC)
    for i, condition in enumerate(MarketCondition):
        specs.append(
            DatasetSpec(
                dataset_id=f"research_{condition.value.lower()}",
                condition=condition,
                bars=bars_per_condition,
                seed=base_seed + i * 1000,
                start=cursor,
            )
        )
        # A week's gap between conditions keeps the ranges clearly separate,
        # which matters because rule 15 forbids two datasets covering the same
        # hours from being replayed into one account.
        cursor = cursor + timedelta(hours=bars_per_condition) + timedelta(days=7)
    return specs
