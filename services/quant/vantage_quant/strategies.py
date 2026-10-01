"""Strategy library.

A strategy is a pure function from market history and parameters to an
OPINION. It returns a signal; it does not size positions, place orders, or know
that a broker exists. Everything downstream -- authority, risk, sizing,
idempotency, execution -- happens in the control plane, which treats whatever
arrives here as untrusted input.

Rules every strategy in this module obeys:

*   **It reads only completed bars.** The caller supplies completed bars only,
    and a strategy evaluates the LAST one. It never indexes past the end and
    never uses the in-progress bar, because that bar's close has not happened.

*   **It declares its warm-up.** ``required_bars`` states how much history the
    strategy needs. Given less, it returns NO_TRADE rather than computing an
    indicator from insufficient data.

*   **NO_TRADE is a normal answer.** Most evaluations return it. A strategy
    that always finds a reason to trade is not a strategy.

*   **Its stop is derived from market structure**, usually a multiple of ATR or
    a channel edge -- never a fixed percentage, which is either meaninglessly
    tight in a volatile market or absurdly wide in a quiet one.

*   **It explains itself.** Every signal carries a sentence a human can check.
"""

from __future__ import annotations

import hashlib
import inspect
from collections.abc import Callable
from dataclasses import dataclass, field, replace
from typing import Any, Literal

import numpy as np
import pandas as pd

from vantage_quant import indicators as ind

Action = Literal["buy", "sell", "hold", "close", "no_trade"]

Family = Literal[
    "trend_following",
    "momentum",
    "mean_reversion",
    "breakout",
    "volatility",
    "multi_timeframe",
    "session",
    "statistical",
    "event_driven",
    "ensemble",
    "machine_learning",
    "high_risk_research",
]


@dataclass(frozen=True)
class Signal:
    """A strategy's opinion about one instrument at one bar."""

    action: Action
    confidence: float = 0.0
    suggested_stop: float | None = None
    suggested_target: float | None = None
    explanation: str = ""
    indicators: dict[str, str] = field(default_factory=dict)
    features: dict[str, float] = field(default_factory=dict)
    #: How many completed bars this strategy declares it needs. Reported on
    #: every signal so the caller never has to know it independently.
    required_bars: int = 0
    #: True when the strategy was given fewer than `required_bars` and so
    #: could not form an opinion at all.
    #:
    #: The distinction is the whole point. A no_trade from a strategy that
    #: examined the market is an ABSTENTION and belongs in the consensus; a
    #: no_trade from a strategy that was never given enough history is a
    #: REFUSAL TO ANSWER and does not. Both used to arrive as
    #: `action="no_trade", confidence=0.0` and were indistinguishable, so the
    #: control plane recorded the second as the first -- writing down an
    #: opinion no strategy had formed and then feeding it to the policy.
    #:
    #: Set by `evaluate`, which is the only place that knows both numbers.
    #: A strategy never sets it: by the time its own code runs it has already
    #: been given the history it asked for.
    insufficient_history: bool = False

    def __post_init__(self) -> None:
        if not 0.0 <= self.confidence <= 1.0:
            raise ValueError(f"confidence must be within [0, 1], got {self.confidence}")
        for name, value in (
            ("suggested_stop", self.suggested_stop),
            ("suggested_target", self.suggested_target),
        ):
            if value is not None and value <= 0:
                raise ValueError(f"{name} must be positive, got {value}")


def no_trade(reason: str, **indicators: Any) -> Signal:
    """Build a NO_TRADE signal with its reason recorded."""
    return Signal(
        action="no_trade",
        confidence=0.0,
        explanation=reason,
        indicators={k: _fmt(v) for k, v in indicators.items()},
    )


def _fmt(value: Any) -> str:
    if value is None:
        return "n/a"
    if isinstance(value, float):
        if np.isnan(value):
            return "n/a"
        return f"{value:.5f}".rstrip("0").rstrip(".")
    return str(value)


@dataclass(frozen=True)
class StrategyContext:
    """Non-sensitive market context the control plane supplies.

    Deliberately narrow. It carries the trading session, the current spread and
    whether a high-impact release is near -- nothing about balances, positions
    or identities, because a strategy does not need them and should not hold
    them.
    """

    session: str = "unknown"
    spread_fraction: float = 0.0
    event_risk: str = "none"

    @classmethod
    def from_dict(cls, raw: dict[str, Any] | None) -> StrategyContext:
        raw = raw or {}
        try:
            spread = float(raw.get("spread_fraction", 0.0) or 0.0)
        except (TypeError, ValueError):
            spread = 0.0
        return cls(
            session=str(raw.get("session", "unknown")),
            spread_fraction=spread,
            event_risk=str(raw.get("event_risk", "none")),
        )


@dataclass(frozen=True)
class StrategySpec:
    """A registered strategy."""

    key: str
    name: str
    family: Family
    description: str
    required_bars: int
    default_parameters: dict[str, Any]
    timeframes: tuple[str, ...]
    valid_regimes: tuple[str, ...]
    generate: Callable[[pd.DataFrame, dict[str, Any], StrategyContext], Signal]
    high_risk: bool = False

    @property
    def code_hash(self) -> str:
        """Hash of the strategy's own source.

        A stored backtest references this. If the implementation changes, the
        hash changes, and an old result can no longer be silently attributed to
        the new code.
        """
        try:
            source = inspect.getsource(self.generate)
        except (OSError, TypeError):  # pragma: no cover - source always available in practice
            source = self.key
        return hashlib.sha256(source.encode("utf-8")).hexdigest()[:16]


REGISTRY: dict[str, StrategySpec] = {}


def register(spec: StrategySpec) -> StrategySpec:
    if spec.key in REGISTRY:
        raise ValueError(f"strategy {spec.key!r} is already registered")
    REGISTRY[spec.key] = spec
    return spec


def get(key: str) -> StrategySpec:
    if key not in REGISTRY:
        raise KeyError(f"unknown strategy {key!r}")
    return REGISTRY[key]


def merged_params(spec: StrategySpec, overrides: dict[str, Any] | None) -> dict[str, Any]:
    """Merge caller parameters over the defaults, rejecting unknown keys.

    An unrecognised parameter is an error rather than being ignored: silently
    dropping ``atr_stop_multiple`` because it was misspelled would run the
    strategy with a different stop than the operator configured.
    """
    params = dict(spec.default_parameters)
    if not overrides:
        return params
    unknown = set(overrides) - set(params)
    if unknown:
        raise ValueError(
            f"strategy {spec.key!r} received unknown parameter(s): {sorted(unknown)}"
        )
    params.update(overrides)
    return params


# ---------------------------------------------------------------------------
# Shared helpers
# ---------------------------------------------------------------------------


def _last(series: pd.Series) -> float:
    """The most recent value, or NaN when unavailable."""
    if series is None or len(series) == 0:
        return float("nan")
    value = series.iloc[-1]
    return float(value) if pd.notna(value) else float("nan")


def _prev(series: pd.Series) -> float:
    if series is None or len(series) < 2:
        return float("nan")
    value = series.iloc[-2]
    return float(value) if pd.notna(value) else float("nan")


def _usable(bars: pd.DataFrame, required: int) -> str | None:
    """Return a refusal reason when the data cannot support a decision."""
    if len(bars) < required:
        return f"insufficient history: {len(bars)} bars available, {required} required"
    tail = bars.tail(required)
    if tail[["open", "high", "low", "close"]].isna().to_numpy().any():
        return "market data contains gaps in the evaluation window"
    if (tail[["open", "high", "low", "close"]] <= 0).to_numpy().any():
        return "market data contains non-positive prices"
    return None


def _atr_stop(
    bars: pd.DataFrame, side: Action, entry: float, atr_value: float, multiple: float
) -> float | None:
    """Place a stop a multiple of ATR away from entry.

    ATR rather than a fixed percentage: the same 0.5% stop is noise in a
    volatile session and a wall in a quiet one, so a percentage stop silently
    changes a strategy's risk profile as conditions change.
    """
    if not np.isfinite(atr_value) or atr_value <= 0 or multiple <= 0:
        return None
    distance = atr_value * multiple
    stop = entry - distance if side == "buy" else entry + distance
    return float(stop) if stop > 0 else None


def _target(side: Action, entry: float, stop: float | None, multiple: float) -> float | None:
    """A take-profit at a multiple of the stop distance."""
    if stop is None or multiple <= 0:
        return None
    distance = abs(entry - stop)
    target = entry + distance * multiple if side == "buy" else entry - distance * multiple
    return float(target) if target > 0 else None


def _confidence(*components: float) -> float:
    """Combine 0-1 components into a bounded confidence.

    The mean rather than a product or a sum: a product collapses to nearly zero
    as soon as any component is weak, and a sum can exceed one. Confidence is
    a ranking input, never a probability of profit.
    """
    finite = [c for c in components if np.isfinite(c)]
    if not finite:
        return 0.0
    return float(min(1.0, max(0.0, sum(finite) / len(finite))))


# ---------------------------------------------------------------------------
# Trend following
# ---------------------------------------------------------------------------


def _ma_trend_crossover(
    bars: pd.DataFrame, p: dict[str, Any], ctx: StrategyContext
) -> Signal:
    fast_period = int(p["fast_period"])
    slow_period = int(p["slow_period"])
    atr_period = int(p["atr_period"])

    reason = _usable(bars, slow_period + atr_period + 2)
    if reason:
        return no_trade(reason)

    close = bars["close"]
    fast = ind.ema(close, fast_period)
    slow = ind.ema(close, slow_period)
    atr_series = ind.atr(bars["high"], bars["low"], close, atr_period)

    fast_now, fast_prev = _last(fast), _prev(fast)
    slow_now, slow_prev = _last(slow), _prev(slow)
    atr_now = _last(atr_series)
    price = _last(close)

    readings = {
        "ema_fast": fast_now,
        "ema_slow": slow_now,
        "atr": atr_now,
        "close": price,
    }

    if not all(np.isfinite(v) for v in (fast_now, fast_prev, slow_now, slow_prev, atr_now)):
        return no_trade("indicators have not finished warming up", **readings)

    # A volatility floor. In a dead market a crossover is noise, and the costs
    # of acting on it exceed any edge it might carry.
    atr_fraction = atr_now / price if price > 0 else 0.0
    min_atr = float(p["min_atr_fraction"])
    if atr_fraction < min_atr:
        return no_trade(
            f"volatility too low to trade: ATR is {atr_fraction:.4%} of price, "
            f"floor is {min_atr:.4%}",
            **readings,
        )

    crossed_up = fast_prev <= slow_prev and fast_now > slow_now
    crossed_down = fast_prev >= slow_prev and fast_now < slow_now

    if not (crossed_up or crossed_down):
        position = "above" if fast_now > slow_now else "below"
        return no_trade(
            f"no crossover on the last bar; the fast average is {position} the slow one",
            **readings,
        )

    side: Action = "buy" if crossed_up else "sell"
    separation = abs(fast_now - slow_now) / atr_now if atr_now > 0 else 0.0
    confidence = _confidence(min(1.0, separation / 1.5), min(1.0, atr_fraction / (min_atr * 4)))

    stop = _atr_stop(bars, side, price, atr_now, float(p["atr_stop_multiple"]))
    if stop is None:
        return no_trade("cannot place a volatility-based stop", **readings)

    return Signal(
        action=side,
        confidence=confidence,
        suggested_stop=stop,
        suggested_target=_target(side, price, stop, float(p["target_multiple"])),
        explanation=(
            f"The {fast_period}-period average crossed "
            f"{'above' if crossed_up else 'below'} the {slow_period}-period average "
            f"on the last completed bar, with volatility above the floor "
            f"(ATR {atr_fraction:.3%} of price). Stop placed "
            f"{p['atr_stop_multiple']}x ATR away."
        ),
        indicators={k: _fmt(v) for k, v in readings.items()},
        features={"atr_fraction": atr_fraction, "ma_separation_atr": separation},
    )


register(
    StrategySpec(
        key="ma_trend_crossover",
        name="Moving Average Trend Crossover",
        family="trend_following",
        description=(
            "Enters in the direction of a fast/slow exponential moving-average "
            "crossover, but only when volatility clears an ATR floor, so it stands "
            "aside in markets too quiet for the move to pay for its costs."
        ),
        required_bars=120,
        default_parameters={
            "fast_period": 20,
            "slow_period": 50,
            "atr_period": 14,
            "atr_stop_multiple": 2.0,
            "min_atr_fraction": 0.0008,
            "target_multiple": 3.0,
        },
        timeframes=("15m", "1h", "4h", "1d"),
        valid_regimes=("TRENDING", "HIGH_VOLATILITY"),
        generate=_ma_trend_crossover,
    )
)


# ---------------------------------------------------------------------------
# Breakout
# ---------------------------------------------------------------------------


def _donchian_breakout(bars: pd.DataFrame, p: dict[str, Any], ctx: StrategyContext) -> Signal:
    channel = int(p["channel_period"])
    atr_period = int(p["atr_period"])

    reason = _usable(bars, channel + atr_period + 2)
    if reason:
        return no_trade(reason)

    upper, _middle, lower = ind.donchian(bars["high"], bars["low"], channel)
    atr_series = ind.atr(bars["high"], bars["low"], bars["close"], atr_period)

    price = _last(bars["close"])
    upper_now, lower_now = _last(upper), _last(lower)
    atr_now = _last(atr_series)

    readings = {"channel_high": upper_now, "channel_low": lower_now, "atr": atr_now, "close": price}
    if not all(np.isfinite(v) for v in (upper_now, lower_now, atr_now)):
        return no_trade("channel has not finished warming up", **readings)

    # The channel excludes the current bar (see indicators.donchian), so this
    # is a genuine break of prior structure rather than a tautology.
    side: Action
    level: float
    if price > upper_now:
        side, level = "buy", upper_now
    elif price < lower_now:
        side, level = "sell", lower_now
    else:
        return no_trade(
            f"price {price:.2f} is inside the {channel}-bar channel "
            f"({lower_now:.2f} to {upper_now:.2f})",
            **readings,
        )

    penetration = abs(price - level) / atr_now if atr_now > 0 else 0.0
    confidence = _confidence(min(1.0, penetration / 1.0), 0.5)

    # The stop sits at the opposite channel edge when that is closer than the
    # ATR stop, because structure is a better stop than arithmetic.
    atr_based = _atr_stop(bars, side, price, atr_now, float(p["atr_stop_multiple"]))
    structural = lower_now if side == "buy" else upper_now
    if atr_based is None:
        stop = float(structural)
    elif side == "buy":
        stop = float(max(atr_based, min(structural, price * 0.999)))
    else:
        stop = float(min(atr_based, max(structural, price * 1.001)))

    return Signal(
        action=side,
        confidence=confidence,
        suggested_stop=stop,
        suggested_target=_target(side, price, stop, float(p["target_multiple"])),
        explanation=(
            f"Price {price:.2f} broke the {channel}-bar "
            f"{'high' if side == 'buy' else 'low'} of {level:.2f} by "
            f"{penetration:.2f}x ATR. The channel is computed from prior bars only, "
            f"so this is a break of established structure."
        ),
        indicators={k: _fmt(v) for k, v in readings.items()},
        features={"penetration_atr": penetration},
    )


register(
    StrategySpec(
        key="donchian_breakout",
        name="Donchian Channel Breakout",
        family="breakout",
        description=(
            "Buys a break of the N-bar high and sells a break of the N-bar low. "
            "The channel excludes the current bar, so a new high is a break of "
            "prior structure rather than a bar breaking its own range."
        ),
        required_bars=120,
        default_parameters={
            "channel_period": 20,
            "atr_period": 14,
            "atr_stop_multiple": 2.0,
            "target_multiple": 2.5,
        },
        timeframes=("15m", "1h", "4h", "1d"),
        valid_regimes=("TRENDING", "HIGH_VOLATILITY"),
        generate=_donchian_breakout,
    )
)


# ---------------------------------------------------------------------------
# Mean reversion
# ---------------------------------------------------------------------------


def _rsi_mean_reversion(bars: pd.DataFrame, p: dict[str, Any], ctx: StrategyContext) -> Signal:
    rsi_period = int(p["rsi_period"])
    bb_period = int(p["bb_period"])
    atr_period = int(p["atr_period"])

    reason = _usable(bars, max(rsi_period, bb_period) + atr_period + 5)
    if reason:
        return no_trade(reason)

    close = bars["close"]
    rsi_series = ind.rsi(close, rsi_period)
    upper, middle, lower = ind.bollinger(close, bb_period, float(p["bb_std"]))
    atr_series = ind.atr(bars["high"], bars["low"], close, atr_period)

    price = _last(close)
    rsi_now = _last(rsi_series)
    atr_now = _last(atr_series)
    upper_now, lower_now, middle_now = _last(upper), _last(lower), _last(middle)

    readings = {
        "rsi": rsi_now,
        "bb_upper": upper_now,
        "bb_middle": middle_now,
        "bb_lower": lower_now,
        "atr": atr_now,
        "close": price,
    }
    if not all(np.isfinite(v) for v in (rsi_now, atr_now, upper_now, lower_now)):
        return no_trade("indicators have not finished warming up", **readings)

    oversold = float(p["oversold"])
    overbought = float(p["overbought"])

    # Mean reversion needs a range to revert within. Price outside the envelope
    # is more likely a trend beginning than an extreme about to snap back, and
    # fading a trend is how reversion strategies produce their worst losses.
    if price > upper_now or price < lower_now:
        return no_trade(
            f"price {price:.2f} is outside the Bollinger envelope "
            f"({lower_now:.2f} to {upper_now:.2f}); this looks like trend, not an extreme",
            **readings,
        )

    side: Action
    if rsi_now <= oversold:
        side = "buy"
        stretch = (oversold - rsi_now) / max(oversold, 1.0)
    elif rsi_now >= overbought:
        side = "sell"
        stretch = (rsi_now - overbought) / max(100.0 - overbought, 1.0)
    else:
        return no_trade(
            f"RSI {rsi_now:.1f} is between the thresholds "
            f"({oversold:.0f} / {overbought:.0f}); no exhaustion to fade",
            **readings,
        )

    stop = _atr_stop(bars, side, price, atr_now, float(p["atr_stop_multiple"]))
    if stop is None:
        return no_trade("cannot place a volatility-based stop", **readings)

    return Signal(
        action=side,
        confidence=_confidence(min(1.0, stretch * 2.0), 0.45),
        suggested_stop=stop,
        # The target is the middle band: reversion aims at the mean, not at a
        # multiple of risk.
        suggested_target=float(middle_now) if np.isfinite(middle_now) else None,
        explanation=(
            f"RSI is {rsi_now:.1f}, {'below' if side == 'buy' else 'above'} the "
            f"{'oversold' if side == 'buy' else 'overbought'} threshold, while price "
            f"remains inside the Bollinger envelope. Targeting the mean at "
            f"{middle_now:.2f}."
        ),
        indicators={k: _fmt(v) for k, v in readings.items()},
        features={"rsi": rsi_now, "stretch": stretch},
    )


register(
    StrategySpec(
        key="rsi_mean_reversion",
        name="RSI Mean Reversion",
        family="mean_reversion",
        description=(
            "Fades exhaustion: buys deeply oversold and sells deeply overbought RSI "
            "readings, but only while price sits inside its Bollinger envelope, so "
            "it declines to fade a market that has broken out."
        ),
        required_bars=120,
        default_parameters={
            "rsi_period": 14,
            "oversold": 28,
            "overbought": 72,
            "bb_period": 20,
            "bb_std": 2.0,
            "atr_period": 14,
            "atr_stop_multiple": 1.5,
            "target_multiple": 1.5,
        },
        timeframes=("15m", "1h", "4h"),
        valid_regimes=("RANGING", "LOW_VOLATILITY"),
        generate=_rsi_mean_reversion,
    )
)


# ---------------------------------------------------------------------------
# Momentum
# ---------------------------------------------------------------------------


def _macd_momentum(bars: pd.DataFrame, p: dict[str, Any], ctx: StrategyContext) -> Signal:
    slow = int(p["slow"])
    atr_period = int(p["atr_period"])

    reason = _usable(bars, slow + int(p["signal"]) + atr_period + 5)
    if reason:
        return no_trade(reason)

    close = bars["close"]
    macd_line, signal_line, histogram = ind.macd(
        close, int(p["fast"]), slow, int(p["signal"])
    )
    atr_series = ind.atr(bars["high"], bars["low"], close, atr_period)

    price = _last(close)
    hist_now, hist_prev = _last(histogram), _prev(histogram)
    macd_now, signal_now = _last(macd_line), _last(signal_line)
    atr_now = _last(atr_series)

    readings = {
        "macd": macd_now,
        "signal": signal_now,
        "histogram": hist_now,
        "atr": atr_now,
        "close": price,
    }
    if not all(np.isfinite(v) for v in (hist_now, hist_prev, macd_now, signal_now, atr_now)):
        return no_trade("indicators have not finished warming up", **readings)

    # Expansion, not merely sign. A positive but shrinking histogram is
    # momentum fading, which is the opposite of what this strategy wants.
    expanding_up = hist_now > 0 and hist_now > hist_prev
    expanding_down = hist_now < 0 and hist_now < hist_prev

    if not (expanding_up or expanding_down):
        return no_trade(
            f"MACD histogram is {hist_now:+.4f} and not expanding "
            f"(previous {hist_prev:+.4f}); momentum is not building",
            **readings,
        )

    side: Action = "buy" if expanding_up else "sell"
    strength = abs(hist_now) / atr_now if atr_now > 0 else 0.0
    acceleration = abs(hist_now - hist_prev) / max(abs(hist_prev), 1e-9)

    stop = _atr_stop(bars, side, price, atr_now, float(p["atr_stop_multiple"]))
    if stop is None:
        return no_trade("cannot place a volatility-based stop", **readings)

    return Signal(
        action=side,
        confidence=_confidence(min(1.0, strength * 3.0), min(1.0, acceleration)),
        suggested_stop=stop,
        suggested_target=_target(side, price, stop, float(p["target_multiple"])),
        explanation=(
            f"The MACD histogram is {hist_now:+.4f}, expanding from {hist_prev:+.4f}, "
            f"so momentum is building to the "
            f"{'upside' if side == 'buy' else 'downside'}."
        ),
        indicators={k: _fmt(v) for k, v in readings.items()},
        features={"histogram": hist_now, "strength_atr": strength},
    )


register(
    StrategySpec(
        key="macd_momentum",
        name="MACD Momentum",
        family="momentum",
        description=(
            "Follows MACD histogram EXPANSION rather than its sign, so it acts while "
            "momentum is building rather than after it has begun to fade."
        ),
        required_bars=120,
        default_parameters={
            "fast": 12,
            "slow": 26,
            "signal": 9,
            "atr_period": 14,
            "atr_stop_multiple": 2.0,
            "target_multiple": 2.0,
        },
        timeframes=("15m", "1h", "4h", "1d"),
        valid_regimes=("TRENDING",),
        generate=_macd_momentum,
    )
)


# ---------------------------------------------------------------------------
# Statistical
# ---------------------------------------------------------------------------


def _bollinger_zscore_reversion(
    bars: pd.DataFrame, p: dict[str, Any], ctx: StrategyContext
) -> Signal:
    period = int(p["period"])
    atr_period = int(p["atr_period"])

    reason = _usable(bars, period + atr_period + 5)
    if reason:
        return no_trade(reason)

    close = bars["close"]
    z = ind.zscore(close, period)
    atr_series = ind.atr(bars["high"], bars["low"], close, atr_period)
    mean = ind.sma(close, period)

    price = _last(close)
    z_now = _last(z)
    atr_now = _last(atr_series)
    mean_now = _last(mean)

    readings = {"zscore": z_now, "mean": mean_now, "atr": atr_now, "close": price}
    if not all(np.isfinite(v) for v in (z_now, atr_now, mean_now)):
        return no_trade("indicators have not finished warming up", **readings)

    entry_z = float(p["entry_z"])
    if abs(z_now) < entry_z:
        return no_trade(
            f"deviation from the {period}-bar mean is {z_now:+.2f} standard deviations, "
            f"inside the {entry_z:.1f} threshold",
            **readings,
        )

    side: Action = "buy" if z_now < 0 else "sell"
    stop = _atr_stop(bars, side, price, atr_now, float(p["atr_stop_multiple"]))
    if stop is None:
        return no_trade("cannot place a volatility-based stop", **readings)

    return Signal(
        action=side,
        confidence=_confidence(min(1.0, (abs(z_now) - entry_z) / 2.0 + 0.3)),
        suggested_stop=stop,
        suggested_target=float(mean_now),
        explanation=(
            f"Price is {z_now:+.2f} standard deviations from its {period}-bar mean of "
            f"{mean_now:.2f}, beyond the {entry_z:.1f} entry threshold. Targeting the mean."
        ),
        indicators={k: _fmt(v) for k, v in readings.items()},
        features={"zscore": z_now},
    )


register(
    StrategySpec(
        key="bollinger_zscore_reversion",
        name="Bollinger Z-Score Reversion",
        family="statistical",
        description=(
            "Trades statistical deviation from a rolling mean, requiring a minimum "
            "z-score to act and targeting the mean rather than a risk multiple."
        ),
        required_bars=100,
        default_parameters={
            "period": 20,
            "entry_z": 2.0,
            "exit_z": 0.5,
            "atr_period": 14,
            "atr_stop_multiple": 1.5,
        },
        timeframes=("15m", "1h", "4h"),
        valid_regimes=("RANGING",),
        generate=_bollinger_zscore_reversion,
    )
)


# ---------------------------------------------------------------------------
# Volatility regime
# ---------------------------------------------------------------------------


def _atr_volatility_regime(
    bars: pd.DataFrame, p: dict[str, Any], ctx: StrategyContext
) -> Signal:
    atr_period = int(p["atr_period"])
    lookback = int(p["lookback"])

    reason = _usable(bars, atr_period + lookback + 5)
    if reason:
        return no_trade(reason)

    atr_series = ind.atr(bars["high"], bars["low"], bars["close"], atr_period)
    atr_now = _last(atr_series)
    atr_baseline = float(atr_series.tail(lookback).mean())
    price = _last(bars["close"])

    readings = {"atr": atr_now, "atr_baseline": atr_baseline, "close": price}
    if not np.isfinite(atr_now) or not np.isfinite(atr_baseline) or atr_baseline <= 0:
        return no_trade("volatility baseline unavailable", **readings)

    ratio = atr_now / atr_baseline
    expansion = float(p["expansion_ratio"])

    if ratio < expansion:
        return no_trade(
            f"volatility is {ratio:.2f}x its {lookback}-bar baseline, below the "
            f"{expansion:.2f}x expansion threshold; regime is not expanding",
            **readings,
        )

    # Volatility is expanding. Direction comes from where price sits relative
    # to the recent range: expansion alone says nothing about which way.
    recent = bars.tail(lookback)
    position = (price - recent["low"].min()) / max(
        recent["high"].max() - recent["low"].min(), 1e-9
    )
    if 0.35 <= position <= 0.65:
        return no_trade(
            f"volatility is expanding ({ratio:.2f}x) but price sits mid-range "
            f"({position:.0%}), giving no directional read",
            **readings,
        )

    side: Action = "buy" if position > 0.65 else "sell"
    stop = _atr_stop(bars, side, price, atr_now, float(p["atr_stop_multiple"]))
    if stop is None:
        return no_trade("cannot place a volatility-based stop", **readings)

    return Signal(
        action=side,
        confidence=_confidence(min(1.0, (ratio - expansion) + 0.3), abs(position - 0.5) * 2),
        suggested_stop=stop,
        suggested_target=_target(side, price, stop, 2.0),
        explanation=(
            f"Volatility expanded to {ratio:.2f}x its {lookback}-bar baseline with price "
            f"at {position:.0%} of the recent range, favouring the "
            f"{'upside' if side == 'buy' else 'downside'}."
        ),
        indicators={k: _fmt(v) for k, v in readings.items()},
        features={"atr_ratio": ratio, "range_position": position},
    )


register(
    StrategySpec(
        key="atr_volatility_regime",
        name="ATR Volatility Regime",
        family="volatility",
        description=(
            "Trades only while volatility is expanding against its own baseline, and "
            "takes direction from where price sits in the recent range. Expansion "
            "alone is not a direction."
        ),
        required_bars=150,
        default_parameters={
            "atr_period": 14,
            "lookback": 50,
            "expansion_ratio": 1.4,
            "atr_stop_multiple": 2.0,
        },
        timeframes=("15m", "1h", "4h"),
        valid_regimes=("HIGH_VOLATILITY",),
        generate=_atr_volatility_regime,
    )
)


# ---------------------------------------------------------------------------
# Session
# ---------------------------------------------------------------------------


def _session_london_breakout(
    bars: pd.DataFrame, p: dict[str, Any], ctx: StrategyContext
) -> Signal:
    atr_period = int(p["atr_period"])
    reason = _usable(bars, atr_period + 40)
    if reason:
        return no_trade(reason)

    if not isinstance(bars.index, pd.DatetimeIndex):
        return no_trade("session strategies require timestamped bars")

    required_session = str(p["session"])
    if ctx.session != required_session:
        return no_trade(
            f"the {required_session} session is not active (currently {ctx.session})",
            session=ctx.session,
        )

    last_time = bars.index[-1]
    day = last_time.normalize()
    range_start = int(p["range_start_hour"])
    range_end = int(p["range_end_hour"])

    # pd.to_timedelta with an explicit unit rather than Timedelta(hours=...):
    # a zero-hour Timedelta is constructed with a generic NumPy unit, which
    # pandas deprecates and will eventually reject.
    window = bars[
        (bars.index >= day + pd.to_timedelta(range_start, unit="h"))
        & (bars.index < day + pd.to_timedelta(range_end, unit="h"))
    ]
    if len(window) < 3:
        return no_trade(
            f"the {range_start:02d}:00-{range_end:02d}:00 range has only {len(window)} bars today"
        )

    range_high = float(window["high"].max())
    range_low = float(window["low"].min())
    price = _last(bars["close"])
    atr_now = _last(ind.atr(bars["high"], bars["low"], bars["close"], atr_period))

    readings = {
        "range_high": range_high,
        "range_low": range_low,
        "atr": atr_now,
        "close": price,
        "session": ctx.session,
    }
    if not np.isfinite(atr_now):
        return no_trade("volatility unavailable", **readings)

    side: Action
    level: float
    if price > range_high:
        side, level = "buy", range_high
    elif price < range_low:
        side, level = "sell", range_low
    else:
        return no_trade(
            f"price {price:.2f} is still inside today's pre-session range "
            f"({range_low:.2f} to {range_high:.2f})",
            **readings,
        )

    stop = float(range_low) if side == "buy" else float(range_high)
    penetration = abs(price - level) / atr_now if atr_now > 0 else 0.0

    return Signal(
        action=side,
        confidence=_confidence(min(1.0, penetration), 0.5),
        suggested_stop=stop,
        suggested_target=_target(side, price, stop, 1.5),
        explanation=(
            f"Price broke the {range_start:02d}:00-{range_end:02d}:00 range "
            f"{'high' if side == 'buy' else 'low'} of {level:.2f} during the "
            f"{required_session} session, with the stop at the opposite range edge."
        ),
        indicators={k: _fmt(v) for k, v in readings.items()},
        features={"penetration_atr": penetration},
    )


register(
    StrategySpec(
        key="session_london_breakout",
        name="London Session Breakout",
        family="session",
        description=(
            "Trades the break of the pre-London range as the London session opens, and "
            "stands aside entirely outside that window."
        ),
        required_bars=120,
        default_parameters={
            "range_start_hour": 0,
            "range_end_hour": 7,
            "session": "london",
            "atr_period": 14,
            "atr_stop_multiple": 1.5,
        },
        timeframes=("5m", "15m", "1h"),
        valid_regimes=("TRENDING", "HIGH_VOLATILITY"),
        generate=_session_london_breakout,
    )
)


# ---------------------------------------------------------------------------
# Multi-timeframe
# ---------------------------------------------------------------------------


def _multi_timeframe_trend(
    bars: pd.DataFrame, p: dict[str, Any], ctx: StrategyContext
) -> Signal:
    htf_period = int(p["htf_period"])
    atr_period = int(p["atr_period"])

    reason = _usable(bars, htf_period + atr_period + 10)
    if reason:
        return no_trade(reason)

    close = bars["close"]
    # The higher timeframe is approximated by a longer average on the same
    # series. Resampling to a genuinely higher timeframe inside a strategy
    # risks partial-bar leakage; a longer lookback expresses the same idea
    # without it.
    htf_trend = ind.ema(close, htf_period)
    fast = ind.ema(close, int(p["ltf_fast"]))
    slow = ind.ema(close, int(p["ltf_slow"]))
    atr_series = ind.atr(bars["high"], bars["low"], close, atr_period)

    price = _last(close)
    htf_now, htf_prev = _last(htf_trend), _prev(htf_trend)
    fast_now, slow_now = _last(fast), _last(slow)
    fast_prev, slow_prev = _prev(fast), _prev(slow)
    atr_now = _last(atr_series)

    readings = {
        "htf_trend": htf_now,
        "ema_fast": fast_now,
        "ema_slow": slow_now,
        "atr": atr_now,
        "close": price,
    }
    if not all(
        np.isfinite(v)
        for v in (htf_now, htf_prev, fast_now, slow_now, fast_prev, slow_prev, atr_now)
    ):
        return no_trade("indicators have not finished warming up", **readings)

    htf_up = htf_now > htf_prev and price > htf_now
    htf_down = htf_now < htf_prev and price < htf_now
    if not (htf_up or htf_down):
        return no_trade(
            "the higher-timeframe trend is not clearly directional", **readings
        )

    crossed_up = fast_prev <= slow_prev and fast_now > slow_now
    crossed_down = fast_prev >= slow_prev and fast_now < slow_now

    # The entry must AGREE with the higher timeframe. A lower-timeframe
    # crossover against the dominant trend is the classic way this family
    # loses money.
    if htf_up and not crossed_up:
        return no_trade("higher timeframe is up but no lower-timeframe entry", **readings)
    if htf_down and not crossed_down:
        return no_trade("higher timeframe is down but no lower-timeframe entry", **readings)

    side: Action = "buy" if htf_up else "sell"
    stop = _atr_stop(bars, side, price, atr_now, float(p["atr_stop_multiple"]))
    if stop is None:
        return no_trade("cannot place a volatility-based stop", **readings)

    return Signal(
        action=side,
        confidence=_confidence(0.6, min(1.0, abs(fast_now - slow_now) / atr_now)),
        suggested_stop=stop,
        suggested_target=_target(side, price, stop, 2.5),
        explanation=(
            f"The higher-timeframe trend is {'up' if side == 'buy' else 'down'} and the "
            f"lower-timeframe averages crossed in the same direction, so entry and trend "
            f"agree."
        ),
        indicators={k: _fmt(v) for k, v in readings.items()},
        features={},
    )


register(
    StrategySpec(
        key="multi_timeframe_trend",
        name="Multi-Timeframe Trend",
        family="multi_timeframe",
        description=(
            "Requires the higher-timeframe trend to agree with the lower-timeframe "
            "entry, refusing counter-trend crossovers."
        ),
        required_bars=150,
        default_parameters={
            "htf_period": 50,
            "ltf_fast": 10,
            "ltf_slow": 20,
            "atr_period": 14,
            "atr_stop_multiple": 2.0,
        },
        timeframes=("5m", "15m", "1h"),
        valid_regimes=("TRENDING",),
        generate=_multi_timeframe_trend,
    )
)


# ---------------------------------------------------------------------------
# Event driven
# ---------------------------------------------------------------------------


def _pre_event_blackout(bars: pd.DataFrame, p: dict[str, Any], ctx: StrategyContext) -> Signal:
    """A policy strategy that never opens exposure.

    It exists to make the event-risk pathway observable end to end: given
    elevated event risk it returns NO_TRADE with that reason, and otherwise it
    returns HOLD. It is deliberately incapable of producing an actionable
    signal, so it can be enabled safely.
    """
    if ctx.event_risk in ("high", "medium"):
        return no_trade(
            f"a high-impact economic release is inside the blackout window "
            f"({p['blackout_minutes_before']} minutes before / "
            f"{p['blackout_minutes_after']} after); spreads widen and stops slip "
            f"through releases",
            event_risk=ctx.event_risk,
        )
    return Signal(
        action="hold",
        confidence=0.0,
        explanation="No high-impact release is near. This policy strategy opens no exposure.",
        indicators={"event_risk": ctx.event_risk},
    )


register(
    StrategySpec(
        key="pre_event_blackout",
        name="Pre-Event Blackout",
        family="event_driven",
        description=(
            "An event-risk policy strategy. It never opens exposure; it reports whether "
            "a high-impact release makes trading inadvisable, which makes the event "
            "pathway visible without adding risk."
        ),
        required_bars=20,
        default_parameters={"blackout_minutes_before": 30, "blackout_minutes_after": 15},
        timeframes=("5m", "15m", "1h", "4h"),
        valid_regimes=("EVENT_RISK",),
        generate=_pre_event_blackout,
    )
)


# ---------------------------------------------------------------------------
# Ensemble
# ---------------------------------------------------------------------------


def _ensemble_weighted_vote(
    bars: pd.DataFrame, p: dict[str, Any], ctx: StrategyContext
) -> Signal:
    members = list(p["members"])
    min_agreement = float(p["min_agreement"])

    votes: dict[str, float] = {"buy": 0.0, "sell": 0.0}
    total_weight = 0.0
    detail: dict[str, str] = {}
    stops: list[float] = []

    for key in members:
        try:
            spec = get(key)
        except KeyError:
            detail[key] = "not registered"
            continue
        member_signal = spec.generate(bars, dict(spec.default_parameters), ctx)
        detail[key] = f"{member_signal.action}@{member_signal.confidence:.2f}"
        total_weight += 1.0
        if member_signal.action in votes:
            votes[member_signal.action] += max(member_signal.confidence, 0.05)
            if member_signal.suggested_stop is not None:
                stops.append(member_signal.suggested_stop)

    if total_weight == 0:
        return no_trade("no ensemble members could be evaluated", **detail)

    buy_share = votes["buy"] / total_weight
    sell_share = votes["sell"] / total_weight

    # Disagreement produces NO_TRADE. Majority voting is not intelligence: two
    # trend strategies agreeing is one opinion counted twice, and a split vote
    # is a reason to stand aside rather than to pick a side.
    if max(buy_share, sell_share) < min_agreement:
        return no_trade(
            f"members disagree: buy weight {buy_share:.2f}, sell weight {sell_share:.2f}, "
            f"agreement threshold {min_agreement:.2f}",
            **detail,
        )
    if buy_share > 0 and sell_share > 0:
        return no_trade(
            f"members are split in direction (buy {buy_share:.2f} / sell {sell_share:.2f})",
            **detail,
        )

    side: Action = "buy" if buy_share > sell_share else "sell"
    price = _last(bars["close"])
    atr_now = _last(ind.atr(bars["high"], bars["low"], bars["close"], int(p["atr_period"])))

    # The most conservative member stop is adopted.
    stop: float | None
    if stops:
        stop = max(stops) if side == "buy" else min(stops)
    else:
        stop = _atr_stop(bars, side, price, atr_now, float(p["atr_stop_multiple"]))
    if stop is None:
        return no_trade("cannot place a stop for the ensemble signal", **detail)

    return Signal(
        action=side,
        confidence=_confidence(max(buy_share, sell_share)),
        suggested_stop=float(stop),
        suggested_target=_target(side, price, float(stop), 2.0),
        explanation=(
            f"{len(members)} members voted; {'buy' if side == 'buy' else 'sell'} weight "
            f"{max(buy_share, sell_share):.2f} cleared the {min_agreement:.2f} threshold "
            f"with no opposing votes. The most conservative member stop was adopted."
        ),
        indicators=detail,
        features={"buy_share": buy_share, "sell_share": sell_share},
    )


register(
    StrategySpec(
        key="ensemble_weighted_vote",
        name="Ensemble Weighted Vote",
        family="ensemble",
        description=(
            "Aggregates several families by confidence weight and returns no-trade when "
            "they disagree or when agreement is thin. Adopts the most conservative "
            "member stop."
        ),
        required_bars=150,
        default_parameters={
            "members": ["ma_trend_crossover", "macd_momentum", "rsi_mean_reversion"],
            "min_agreement": 0.6,
            "atr_period": 14,
            "atr_stop_multiple": 2.0,
        },
        timeframes=("15m", "1h", "4h"),
        valid_regimes=("TRENDING", "RANGING"),
        generate=_ensemble_weighted_vote,
    )
)


# ---------------------------------------------------------------------------
# Machine learning
# ---------------------------------------------------------------------------


def _ml_direction_filter(bars: pd.DataFrame, p: dict[str, Any], ctx: StrategyContext) -> Signal:
    """A trend signal filtered by a trained directional model.

    The model is a FILTER, never the signal. A trend entry must exist first;
    the model can only veto it. And when no model is deployed, or its features
    cannot be computed, the strategy FAILS CLOSED to no-trade rather than
    falling back to the unfiltered signal -- otherwise "the model is broken"
    would silently become "trade without the filter".
    """
    from vantage_quant import ml  # imported lazily: heavier dependency

    base_params = {
        "fast_period": int(p["fast_period"]),
        "slow_period": int(p["slow_period"]),
        "atr_period": int(p["atr_period"]),
        "atr_stop_multiple": float(p["atr_stop_multiple"]),
        "min_atr_fraction": 0.0,
        "target_multiple": 2.0,
    }
    base = _ma_trend_crossover(bars, base_params, ctx)
    if base.action not in ("buy", "sell"):
        return no_trade(f"no underlying trend entry to filter ({base.explanation})")

    model = ml.load_deployed_model(str(p["model_key"]))
    if model is None:
        return no_trade(
            f"no model is deployed under key {p['model_key']!r}; the filter fails closed "
            f"rather than trading unfiltered"
        )

    try:
        probability = model.predict_up_probability(bars)
    except ml.FeatureUnavailableError as exc:
        return no_trade(f"model features unavailable ({exc}); failing closed")

    threshold = float(p["min_probability"])
    agrees = probability >= threshold if base.action == "buy" else (1.0 - probability) >= threshold
    directional = probability if base.action == "buy" else 1.0 - probability

    if not agrees:
        return no_trade(
            f"the model gives {directional:.1%} probability to the {base.action} direction, "
            f"below the {threshold:.0%} threshold",
            model_probability=probability,
            model_version=model.version,
        )

    return Signal(
        action=base.action,
        confidence=_confidence(base.confidence, min(1.0, (directional - 0.5) * 2)),
        suggested_stop=base.suggested_stop,
        suggested_target=base.suggested_target,
        explanation=(
            f"{base.explanation} The deployed model (version {model.version}) assigns "
            f"{directional:.1%} probability to this direction, clearing the "
            f"{threshold:.0%} filter."
        ),
        indicators={
            **base.indicators,
            "model_probability": _fmt(probability),
            "model_version": str(model.version),
        },
        features={**base.features, "model_probability": probability},
    )


register(
    StrategySpec(
        key="ml_direction_filter",
        name="ML Direction Filter",
        family="machine_learning",
        description=(
            "A trend entry filtered by a trained directional-probability model. The "
            "model can only veto, never initiate, and the strategy fails closed to "
            "no-trade when no model is deployed or its features cannot be computed."
        ),
        required_bars=150,
        default_parameters={
            "model_key": "xauusd_direction",
            "min_probability": 0.58,
            "fast_period": 20,
            "slow_period": 50,
            "atr_period": 14,
            "atr_stop_multiple": 2.0,
        },
        timeframes=("1h", "4h"),
        valid_regimes=("TRENDING",),
        generate=_ml_direction_filter,
    )
)


# ---------------------------------------------------------------------------
# High-risk research
# ---------------------------------------------------------------------------


def _grid_martingale_research(
    bars: pd.DataFrame, p: dict[str, Any], ctx: StrategyContext
) -> Signal:
    """Grid / martingale: registered for study, never for execution.

    This family produces a smooth equity curve for a long time and then loses
    everything, because it adds to losing positions and its position size grows
    geometrically while the account does not. It is included so that the
    behaviour can be BACKTESTED and its failure mode examined, and it is
    permanently barred from execution: the control plane refuses to enable a
    high-risk strategy, and a database constraint refuses to store the row that
    would allow it.
    """
    reason = _usable(bars, int(p["atr_period"]) + 30)
    if reason:
        return no_trade(reason)

    close = bars["close"]
    atr_now = _last(ind.atr(bars["high"], bars["low"], close, int(p["atr_period"])))
    price = _last(close)
    mean = _last(ind.sma(close, 20))

    if not all(np.isfinite(v) for v in (atr_now, mean)):
        return no_trade("indicators have not finished warming up")

    step = atr_now * float(p["grid_step_atr"])
    levels_away = abs(price - mean) / step if step > 0 else 0.0
    if levels_away < 1.0:
        return no_trade(
            f"price is within one grid step of the mean ({levels_away:.2f} levels)"
        )

    side: Action = "buy" if price < mean else "sell"
    return Signal(
        action=side,
        confidence=0.2,
        suggested_stop=None,
        suggested_target=float(mean),
        explanation=(
            f"HIGH RISK RESEARCH. Grid logic would add a level {levels_away:.1f} steps from "
            f"the mean with no stop. This strategy cannot be enabled for execution; it "
            f"exists so its failure mode can be measured rather than argued about."
        ),
        indicators={"levels_from_mean": _fmt(levels_away), "atr": _fmt(atr_now)},
        features={"levels_from_mean": levels_away},
    )


register(
    StrategySpec(
        key="grid_martingale_research",
        name="Grid / Martingale (Research Only)",
        family="high_risk_research",
        description=(
            "HIGH RISK RESEARCH ONLY. Adds to losing positions on a grid with no stop. "
            "Included so the failure mode can be measured; permanently barred from "
            "execution by the control plane and by a database constraint."
        ),
        required_bars=60,
        default_parameters={
            "grid_step_atr": 1.0,
            "max_levels": 5,
            "multiplier": 2.0,
            "atr_period": 14,
        },
        timeframes=("15m", "1h"),
        valid_regimes=("UNKNOWN",),
        generate=_grid_martingale_research,
        high_risk=True,
    )
)


def evaluate(
    key: str,
    bars: pd.DataFrame,
    parameters: dict[str, Any] | None,
    context: dict[str, Any] | None = None,
) -> tuple[Signal, StrategySpec]:
    """Evaluate one strategy over completed bars."""
    spec = get(key)
    params = merged_params(spec, parameters)
    ctx = StrategyContext.from_dict(context)

    if len(bars) < spec.required_bars:
        return (
            replace(
                no_trade(
                    f"{spec.name} requires {spec.required_bars} bars and received {len(bars)}"
                ),
                required_bars=spec.required_bars,
                insufficient_history=True,
            ),
            spec,
        )
    # Stamped onto every answer, not only the refusals, so a caller can report
    # what the strategy asked for without holding a second copy of the number.
    return replace(spec.generate(bars, params, ctx), required_bars=spec.required_bars), spec
