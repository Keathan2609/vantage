"""Opportunity scanner and regime classification.

The scanner ranks instruments. It does not trade, and nothing it returns
reaches a broker: a candidate the user acts on goes through the same order
pipeline, with the same gates, as any manual order.

Ranking is deliberately conservative in two ways. Cost is subtracted rather
than ignored, so a wide spread pushes an instrument down the list even when its
trend looks attractive; and event risk caps the score, because a strong setup
into a high-impact release is not a strong setup.
"""

from __future__ import annotations

from dataclasses import dataclass
from typing import Any, Literal

import numpy as np
import pandas as pd

from vantage_quant import indicators as ind
from vantage_quant import strategies as strat

Regime = Literal[
    "TRENDING",
    "RANGING",
    "HIGH_VOLATILITY",
    "LOW_VOLATILITY",
    "EVENT_RISK",
    "RISK_OFF",
    "UNKNOWN",
]


# The least share of its own path a trend must actually cover. See
# classify_regime for why ADX alone is not enough.
TREND_EFFICIENCY_MIN = 0.30


def classify_regime(
    bars: pd.DataFrame, event_risk: str = "none"
) -> tuple[Regime, dict[str, float]]:
    """Classify the market regime from price behaviour.

    UNKNOWN is a real answer and is returned whenever the evidence is thin.
    Forcing a label onto an ambiguous market would let a strategy that declares
    itself valid only in trends run in a market nobody can characterise.
    """
    if len(bars) < 60:
        return "UNKNOWN", {}

    close = bars["close"]
    high = bars["high"]
    low = bars["low"]

    adx_series, plus_di, minus_di = ind.adx(high, low, close, 14)
    atr_series = ind.atr(high, low, close, 14)

    adx_now = float(adx_series.iloc[-1]) if pd.notna(adx_series.iloc[-1]) else float("nan")
    atr_now = float(atr_series.iloc[-1]) if pd.notna(atr_series.iloc[-1]) else float("nan")
    atr_baseline = float(atr_series.tail(50).mean())
    price = float(close.iloc[-1])

    # Efficiency ratio: net displacement against the distance actually
    # travelled, over the same window ADX uses.
    #
    # ADX cannot tell a trend from a regular oscillation. A cycle with a period
    # of eight bars spends four bars going up and four coming down, and those
    # sustained runs are exactly what ADX measures -- so a clean sine wave
    # scores as strongly directional. The scenario matrix caught it: a
    # deliberately range-bound fixture was classified TRENDING twelve times and
    # RANGING never, which would run every trend strategy in precisely the
    # market it should sit out.
    #
    # Displacement is the missing half. A trend GETS SOMEWHERE: over 14 bars
    # its net move is a large fraction of the path length. An oscillation
    # returns to where it started, so its ratio collapses towards zero however
    # decisive each leg looked.
    window = min(14, len(close) - 1)
    travelled = float(close.diff().abs().tail(window).sum())
    displacement = abs(float(close.iloc[-1]) - float(close.iloc[-1 - window]))
    efficiency = displacement / travelled if travelled > 0 else 0.0

    measures = {
        "adx": adx_now if np.isfinite(adx_now) else 0.0,
        "atr_fraction": atr_now / price if np.isfinite(atr_now) and price > 0 else 0.0,
        "volatility_ratio": (
            atr_now / atr_baseline if np.isfinite(atr_now) and atr_baseline > 0 else 1.0
        ),
        "plus_di": float(plus_di.iloc[-1]) if pd.notna(plus_di.iloc[-1]) else 0.0,
        "minus_di": float(minus_di.iloc[-1]) if pd.notna(minus_di.iloc[-1]) else 0.0,
        "efficiency": efficiency,
    }

    if not np.isfinite(adx_now) or not np.isfinite(atr_now):
        return "UNKNOWN", measures

    # Event risk overrides the price-derived read: an imminent release changes
    # how every other measure should be interpreted.
    if event_risk in ("high", "medium"):
        return "EVENT_RISK", measures

    volatility_ratio = measures["volatility_ratio"]
    if volatility_ratio > 1.5:
        return "HIGH_VOLATILITY", measures
    if volatility_ratio < 0.6:
        return "LOW_VOLATILITY", measures
    # ADX above 25 is the conventional threshold for a trending market, and it
    # is necessary but NOT sufficient: see the efficiency note above.
    #
    # 0.30 is a development default, not the output of a study. It separates
    # the fixtures cleanly -- a clean trend runs well above it and an
    # eight-bar cycle well below -- and it is stated as a constant so a later
    # calibration is a visible change rather than a tweak.
    if adx_now > 25 and efficiency >= TREND_EFFICIENCY_MIN:
        return "TRENDING", measures
    if adx_now < 20:
        return "RANGING", measures
    # Directional by ADX but going nowhere: an oscillation, not a trend. The
    # honest label is RANGING, which is what a mean-reversion strategy needs
    # and what a trend strategy must sit out.
    if adx_now > 25:
        return "RANGING", measures

    # ADX between 20 and 25 -- the only band with no branch of its own, which
    # used to fall through to UNKNOWN.
    #
    # That mattered more than it looked. No PAPER-promoted strategy declares
    # itself valid in UNKNOWN, so the consensus policy discards every opinion
    # and the bar is a guaranteed NO TRADE. Measured across the sixteen
    # generated market conditions, the fallthrough took 9.5% of all decision
    # points -- about one in ten -- and the band matched ADX 20-25 almost
    # exactly (9.6%).
    #
    # It was also INCONSISTENT with the branch directly above. A market going
    # nowhere is called RANGING at ADX 30 on the stated reasoning that
    # displacement, not ADX, decides whether something is a trend. At ADX 22 --
    # LESS directional by the same measure -- it was called unclassifiable.
    # The same market got two different labels, and the weaker reading got the
    # more conservative one.
    #
    # Measured efficiency in the band: median 0.203, against 0.154 for RANGING
    # and 0.511 for TRENDING. It is a range population, not a trend one.
    #
    # So the same rule applies here: going nowhere is a range. What stays
    # UNKNOWN is the genuinely ambiguous remainder -- price IS getting
    # somewhere while ADX has not confirmed it, which is what an emerging trend
    # and a false start look like alike, and is exactly when a mean-reversion
    # strategy should not be told it is safe.
    if efficiency < TREND_EFFICIENCY_MIN:
        return "RANGING", measures
    return "UNKNOWN", measures


@dataclass
class Candidate:
    instrument_id: str
    symbol: str
    regime: str
    trend_score: float
    momentum_score: float
    volatility: float
    spread_fraction: float
    event_risk: str
    score: float
    agreeing_strategies: list[str]
    explanation: str
    indicators: dict[str, str]

    def to_dict(self) -> dict[str, Any]:
        return {
            "instrument_id": self.instrument_id,
            "symbol": self.symbol,
            "regime": self.regime,
            "trend_score": f"{self.trend_score:.4f}",
            "momentum_score": f"{self.momentum_score:.4f}",
            "volatility": f"{self.volatility:.6f}",
            "spread_fraction": f"{self.spread_fraction:.6f}",
            "event_risk": self.event_risk,
            "score": f"{self.score:.4f}",
            "agreeing_strategies": self.agreeing_strategies,
            "explanation": self.explanation,
            "indicators": self.indicators,
        }


# Strategies polled for agreement. Deliberately drawn from DIFFERENT families:
# three trend strategies agreeing is one opinion counted three times.
SCAN_STRATEGIES = (
    "ma_trend_crossover",
    "macd_momentum",
    "rsi_mean_reversion",
    "donchian_breakout",
)


def scan_instrument(
    instrument_id: str,
    symbol: str,
    bars: pd.DataFrame,
    spread_fraction: float,
    session: str,
    event_risk: str,
) -> Candidate | None:
    """Score one instrument."""
    if len(bars) < 60:
        return None

    regime, measures = classify_regime(bars, event_risk)
    close = bars["close"]
    price = float(close.iloc[-1])

    # Trend: where price sits relative to a long average, normalised by ATR so
    # the number is comparable across instruments at different price levels.
    ema50 = ind.ema(close, 50)
    atr_series = ind.atr(bars["high"], bars["low"], close, 14)
    atr_now = float(atr_series.iloc[-1]) if pd.notna(atr_series.iloc[-1]) else float("nan")
    ema_now = float(ema50.iloc[-1]) if pd.notna(ema50.iloc[-1]) else float("nan")

    if not np.isfinite(atr_now) or atr_now <= 0 or not np.isfinite(ema_now):
        return None

    trend_score = float(np.tanh((price - ema_now) / (atr_now * 3)))
    roc_series = ind.roc(close, 12)
    roc_now = float(roc_series.iloc[-1]) if pd.notna(roc_series.iloc[-1]) else 0.0
    momentum_score = float(np.tanh(roc_now * 40))
    volatility = atr_now / price if price > 0 else 0.0

    ctx = strat.StrategyContext(
        session=session, spread_fraction=spread_fraction, event_risk=event_risk
    )
    agreeing: list[str] = []
    confidences: list[float] = []
    directions: set[str] = set()

    for key in SCAN_STRATEGIES:
        try:
            spec = strat.get(key)
        except KeyError:
            continue
        if len(bars) < spec.required_bars:
            continue
        try:
            signal = spec.generate(bars, dict(spec.default_parameters), ctx)
        except Exception:
            continue
        if signal.action in ("buy", "sell"):
            agreeing.append(f"{key}:{signal.action}")
            confidences.append(signal.confidence)
            directions.add(signal.action)

    agreement = float(np.mean(confidences)) if confidences else 0.0

    # Conflicting directions cancel rather than add. Two strategies pointing
    # opposite ways is not twice the conviction; it is a reason to stand aside.
    if len(directions) > 1:
        agreement *= 0.25

    # Costs and event risk reduce the score. A tight setup on a wide spread is
    # not attractive, and a strong setup into a release is a trap.
    spread_penalty = min(1.0, spread_fraction / 0.002)
    event_penalty = {"high": 0.7, "medium": 0.35}.get(event_risk, 0.0)

    raw = 0.4 * abs(trend_score) + 0.3 * abs(momentum_score) + 0.3 * agreement
    score = max(0.0, raw * (1.0 - spread_penalty * 0.5) * (1.0 - event_penalty))

    explanation_parts = [
        f"Regime {regime}",
        f"trend {trend_score:+.2f}",
        f"momentum {momentum_score:+.2f}",
        f"volatility {volatility:.2%} of price",
        f"spread {spread_fraction:.4%}",
    ]
    if agreeing:
        explanation_parts.append(f"{len(agreeing)} strategy signal(s): {', '.join(agreeing)}")
    else:
        explanation_parts.append("no strategy is signalling")
    if len(directions) > 1:
        explanation_parts.append("signals conflict in direction, so agreement is discounted")
    if event_risk != "none":
        explanation_parts.append(f"event risk {event_risk}, score reduced")

    return Candidate(
        instrument_id=instrument_id,
        symbol=symbol,
        regime=regime,
        trend_score=trend_score,
        momentum_score=momentum_score,
        volatility=volatility,
        spread_fraction=spread_fraction,
        event_risk=event_risk,
        score=score,
        agreeing_strategies=agreeing,
        explanation="; ".join(explanation_parts) + ".",
        indicators={
            "adx": f"{measures.get('adx', 0):.1f}",
            "atr": f"{atr_now:.4f}",
            "ema50": f"{ema_now:.4f}",
            "close": f"{price:.4f}",
            "volatility_ratio": f"{measures.get('volatility_ratio', 1):.2f}",
        },
    )


def correlation_matrix(
    returns: dict[str, pd.Series], period: int = 60
) -> dict[str, dict[str, str]]:
    """Rolling correlation between instrument return series.

    Reported as information, never as a constant. Correlations move, and they
    tend to move toward one across risk assets in exactly the conditions where
    a diversification assumption is being relied on.
    """
    keys = sorted(returns)
    out: dict[str, dict[str, str]] = {}
    for a in keys:
        row: dict[str, str] = {}
        for b in keys:
            if a == b:
                row[b] = "1.0000"
                continue
            series_a, series_b = returns[a], returns[b]
            joined = pd.concat([series_a, series_b], axis=1).dropna()
            if len(joined) < period:
                row[b] = "n/a"
                continue
            value = float(joined.iloc[:, 0].tail(period).corr(joined.iloc[:, 1].tail(period)))
            row[b] = f"{value:.4f}" if np.isfinite(value) else "n/a"
        out[a] = row
    return out
