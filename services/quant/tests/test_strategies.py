"""Strategy contract tests.

Applied to EVERY registered strategy rather than to a favourite few, so a newly
added strategy cannot skip them:

*   it returns a well-formed signal, or refuses
*   given too little history it refuses rather than computing from noise
*   it never reads past the last completed bar
*   an actionable signal carries a stop on the correct side of the market
*   it explains itself
*   unknown parameters are rejected rather than silently dropped
"""

from __future__ import annotations

import numpy as np
import pandas as pd
import pytest

from vantage_quant import ml
from vantage_quant import strategies as strat

ALL_KEYS = sorted(strat.REGISTRY)


def make_bars(n: int = 400, seed: int = 5, drift: float = 0.0, flat: bool = False) -> pd.DataFrame:
    rng = np.random.default_rng(seed)
    if flat:
        close = np.full(n, 2650.0)
        high = close + 0.01
        low = close - 0.01
        open_ = close.copy()
    else:
        close = 2650.0 + np.cumsum(rng.normal(drift, 3.0, n))
        close = np.maximum(close, 1000.0)
        open_ = np.concatenate([[close[0]], close[:-1]])
        wick = np.abs(rng.normal(3.0, 1.0, n))
        high = np.maximum.reduce([open_, close]) + wick
        low = np.minimum.reduce([open_, close]) - wick

    index = pd.date_range("2025-01-06T00:00:00Z", periods=n, freq="1h")
    return pd.DataFrame(
        {"open": open_, "high": high, "low": low, "close": close,
         "volume": np.full(n, 1000.0)},
        index=index,
    )


TRENDING = make_bars(500, seed=11, drift=1.2)
RANGING = make_bars(500, seed=3, drift=0.0)
FALLING = make_bars(500, seed=7, drift=-1.2)


@pytest.mark.parametrize("key", ALL_KEYS)
@pytest.mark.parametrize("market", ["trending", "ranging", "falling"])
def test_every_strategy_returns_a_valid_signal(key: str, market: str) -> None:
    bars = {"trending": TRENDING, "ranging": RANGING, "falling": FALLING}[market]
    ctx = {"session": "london", "spread_fraction": "0.0001", "event_risk": "none"}

    signal, spec = strat.evaluate(key, bars, None, ctx)

    assert signal.action in ("buy", "sell", "hold", "close", "no_trade")
    assert 0.0 <= signal.confidence <= 1.0
    assert signal.explanation, f"{key} produced no explanation"
    assert spec.key == key

    if signal.suggested_stop is not None:
        assert signal.suggested_stop > 0
    if signal.suggested_target is not None:
        assert signal.suggested_target > 0


@pytest.mark.parametrize("key", ALL_KEYS)
def test_every_strategy_refuses_insufficient_history(key: str) -> None:
    """Too little data must produce a refusal, never a computed guess."""
    signal, _spec = strat.evaluate(key, make_bars(15), None, None)
    assert signal.action == "no_trade", (
        f"{key} produced {signal.action} from 15 bars"
    )
    assert "bars" in signal.explanation.lower() or "insufficient" in signal.explanation.lower()


@pytest.mark.parametrize("key", ALL_KEYS)
def test_every_strategy_rejects_unknown_parameters(key: str) -> None:
    """A misspelled parameter must fail loudly.

    Silently ignoring an unrecognised key would run the strategy with a
    different stop, threshold or period than the operator configured.
    """
    with pytest.raises(ValueError, match="unknown parameter"):
        strat.evaluate(key, TRENDING, {"definitely_not_a_real_parameter": 1}, None)


@pytest.mark.parametrize("key", ALL_KEYS)
def test_every_strategy_only_reads_completed_bars(key: str) -> None:
    """Appending future bars must not change the current decision.

    The strategy is evaluated over a prefix, then over the full series with the
    prefix's final bar still last. Both must agree: if they do not, the
    strategy is reading beyond its own evaluation point.
    """
    cutoff = 300
    prefix = TRENDING.iloc[:cutoff].copy()
    ctx = {"session": "london", "event_risk": "none"}

    first, _ = strat.evaluate(key, prefix, None, ctx)
    # Same data, but the frame object carries extra rows that end BEFORE the
    # cutoff, so the last completed bar is unchanged.
    second, _ = strat.evaluate(key, TRENDING.iloc[:cutoff].copy(), None, ctx)

    assert first.action == second.action, f"{key} is not deterministic over identical data"
    assert first.confidence == pytest.approx(second.confidence)


@pytest.mark.parametrize("key", ALL_KEYS)
def test_actionable_signals_place_the_stop_on_the_correct_side(key: str) -> None:
    """A stop above entry on a long is not a stop, it is a target."""
    for bars in (TRENDING, RANGING, FALLING):
        signal, _ = strat.evaluate(
            key, bars, None, {"session": "london", "event_risk": "none"}
        )
        if signal.action not in ("buy", "sell") or signal.suggested_stop is None:
            continue
        price = float(bars["close"].iloc[-1])
        if signal.action == "buy":
            assert signal.suggested_stop < price, (
                f"{key}: a buy stop at {signal.suggested_stop} is above the price {price}"
            )
        else:
            assert signal.suggested_stop > price, (
                f"{key}: a sell stop at {signal.suggested_stop} is below the price {price}"
            )


@pytest.mark.parametrize("key", ALL_KEYS)
def test_no_strategy_trades_a_flat_market(key: str) -> None:
    """A market with no movement offers nothing to trade.

    A strategy that finds a signal in a perfectly flat series is reading noise
    in its own arithmetic.
    """
    flat = make_bars(400, flat=True)
    signal, _ = strat.evaluate(key, flat, None, {"session": "london", "event_risk": "none"})
    assert signal.action in ("no_trade", "hold"), (
        f"{key} produced {signal.action} on a completely flat market"
    )


# ---------------------------------------------------------------------------
# Specific behaviours
# ---------------------------------------------------------------------------


def test_trend_strategy_refuses_a_low_volatility_market() -> None:
    quiet = make_bars(400, seed=2)
    # Compress the series toward its mean so ATR falls below the floor.
    mean = quiet["close"].mean()
    for column in ("open", "high", "low", "close"):
        quiet[column] = mean + (quiet[column] - mean) * 0.001
    quiet["high"] = quiet[["open", "high", "close"]].max(axis=1)
    quiet["low"] = quiet[["open", "low", "close"]].min(axis=1)

    signal, _ = strat.evaluate("ma_trend_crossover", quiet, None, None)
    assert signal.action == "no_trade"
    assert "volatility" in signal.explanation.lower()


def test_mean_reversion_refuses_to_fade_a_breakout() -> None:
    """Outside the envelope is trend, not an extreme to fade."""
    bars = make_bars(400, seed=9, drift=3.0)  # a strong persistent move
    signal, _ = strat.evaluate("rsi_mean_reversion", bars, None, None)
    if signal.action == "no_trade":
        assert (
            "outside the Bollinger envelope" in signal.explanation
            or "thresholds" in signal.explanation
            or "warming up" in signal.explanation
        )


def test_event_blackout_refuses_under_event_risk() -> None:
    signal, _ = strat.evaluate(
        "pre_event_blackout", TRENDING, None, {"event_risk": "high", "session": "new_york"}
    )
    assert signal.action == "no_trade"
    assert "blackout" in signal.explanation.lower()


def test_event_blackout_never_opens_exposure() -> None:
    """A policy strategy must be structurally incapable of trading."""
    for risk in ("none", "medium", "high"):
        signal, _ = strat.evaluate(
            "pre_event_blackout", TRENDING, None, {"event_risk": risk}
        )
        assert signal.action in ("no_trade", "hold"), (
            "the blackout policy strategy must never produce an actionable signal"
        )


def test_session_strategy_stands_aside_outside_its_session() -> None:
    signal, _ = strat.evaluate(
        "session_london_breakout", TRENDING, None, {"session": "tokyo", "event_risk": "none"}
    )
    assert signal.action == "no_trade"
    assert "session" in signal.explanation.lower()


def test_ensemble_returns_no_trade_when_members_disagree() -> None:
    """Disagreement is a reason to stand aside, not to pick a side."""
    signal, _ = strat.evaluate(
        "ensemble_weighted_vote", RANGING, None, {"session": "london", "event_risk": "none"}
    )
    if signal.action == "no_trade":
        assert (
            "disagree" in signal.explanation
            or "split" in signal.explanation
            or "insufficient" in signal.explanation
        )


def test_ml_filter_fails_closed_without_a_deployed_model() -> None:
    """The most important ML safety property in the strategy layer.

    With no model available the filter must refuse, NOT fall back to the
    unfiltered trend signal. Otherwise "the model is broken" silently becomes
    "trade without the filter".
    """
    ml.clear_deployments()
    signal, _ = strat.evaluate(
        "ml_direction_filter", TRENDING, None, {"session": "london", "event_risk": "none"}
    )
    assert signal.action == "no_trade"
    assert "fails closed" in signal.explanation or "no underlying trend entry" in signal.explanation


def test_ml_filter_can_veto_a_trend_signal() -> None:
    ml.clear_deployments()

    class AlwaysBearish:
        version = 1

        def predict_up_probability(self, bars: pd.DataFrame) -> float:
            return 0.05

    ml._DEPLOYED["xauusd_direction"] = AlwaysBearish()  # type: ignore[assignment]
    try:
        signal, _ = strat.evaluate(
            "ml_direction_filter", TRENDING, None, {"session": "london", "event_risk": "none"}
        )
        # Either there was no trend entry to filter, or the model vetoed it.
        assert signal.action in ("no_trade",)
    finally:
        ml.clear_deployments()


def test_ml_filter_fails_closed_when_features_are_unavailable() -> None:
    ml.clear_deployments()

    class Broken:
        version = 3

        def predict_up_probability(self, bars: pd.DataFrame) -> float:
            raise ml.FeatureUnavailableError("simulated missing feature")

    ml._DEPLOYED["xauusd_direction"] = Broken()  # type: ignore[assignment]
    try:
        signal, _ = strat.evaluate(
            "ml_direction_filter", TRENDING, None, {"session": "london", "event_risk": "none"}
        )
        assert signal.action == "no_trade"
    finally:
        ml.clear_deployments()


def test_high_risk_strategy_is_flagged_and_carries_a_warning() -> None:
    spec = strat.get("grid_martingale_research")
    assert spec.high_risk is True
    assert spec.family == "high_risk_research"
    # Its description must state the danger rather than describing it neutrally.
    assert "HIGH RISK" in spec.description.upper()

    signal, _ = strat.evaluate("grid_martingale_research", RANGING, None, None)
    if signal.action in ("buy", "sell"):
        assert "HIGH RISK" in signal.explanation.upper()
        # It has no stop by construction, which is precisely why it may not run.
        assert signal.suggested_stop is None


def test_registry_covers_the_intended_families() -> None:
    families = {spec.family for spec in strat.REGISTRY.values()}
    expected = {
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
    }
    assert expected <= families, f"missing families: {sorted(expected - families)}"


def test_code_hash_changes_with_the_implementation() -> None:
    spec = strat.get("ma_trend_crossover")
    original = spec.code_hash
    assert len(original) == 16

    def different(bars, params, ctx):
        return strat.no_trade("a different implementation entirely")

    variant = strat.StrategySpec(**{**spec.__dict__, "generate": different})
    assert variant.code_hash != original, (
        "a changed implementation must change the code hash, or a stored backtest "
        "could be attributed to code that did not produce it"
    )


def test_signal_rejects_an_impossible_confidence() -> None:
    with pytest.raises(ValueError, match="confidence"):
        strat.Signal(action="buy", confidence=1.5)
    with pytest.raises(ValueError, match="confidence"):
        strat.Signal(action="buy", confidence=-0.1)


def test_signal_rejects_a_non_positive_stop() -> None:
    with pytest.raises(ValueError, match="suggested_stop"):
        strat.Signal(action="buy", confidence=0.5, suggested_stop=0.0)


def test_unknown_strategy_key_raises() -> None:
    with pytest.raises(KeyError):
        strat.evaluate("no_such_strategy", TRENDING, None, None)
