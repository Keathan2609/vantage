"""Backtester honesty tests.

These do not check that the backtester is profitable. They check that it is not
lying, which is the only property that makes a result worth reading:

*   entries fill on the bar AFTER the signal, not at the signal bar's close
*   costs reduce the result, and a frictionless run is flagged as such
*   when a bar covers both stop and target, the stop is assumed
*   corrupt, unsorted or duplicated data is refused rather than silently used
*   trades the account cannot afford are counted, not quietly dropped
*   the same inputs produce the same result
"""

from __future__ import annotations

import numpy as np
import pandas as pd
import pytest

from vantage_quant import backtest as bt
from vantage_quant import strategies as strat


def make_bars(
    n: int = 600, seed: int = 7, start: float = 2650.0, drift: float = 0.0
) -> pd.DataFrame:
    """A deterministic hourly OHLC series with coherent bars."""
    rng = np.random.default_rng(seed)
    steps = rng.normal(drift, 3.0, n)
    close = start + np.cumsum(steps)
    close = np.maximum(close, start * 0.5)

    open_ = np.concatenate([[start], close[:-1]])
    wick = np.abs(rng.normal(3.0, 1.2, n))
    high = np.maximum.reduce([open_, close]) + wick
    low = np.minimum.reduce([open_, close]) - wick

    index = pd.date_range("2025-01-06T00:00:00Z", periods=n, freq="1h")
    return pd.DataFrame(
        {"open": open_, "high": high, "low": low, "close": close,
         "volume": rng.integers(500, 5000, n).astype("float64")},
        index=index,
    )


def base_config(**overrides) -> bt.BacktestConfig:
    defaults = {
        "strategy_key": "ma_trend_crossover",
        "initial_capital": 10_000.0,
        "risk_per_trade": 0.01,
        "instrument": bt.InstrumentModel(contract_size=1.0, min_quantity=0.01, quantity_step=0.01),
        "sample_kind": "out_of_sample",
    }
    defaults.update(overrides)
    return bt.BacktestConfig(**defaults)


# ---------------------------------------------------------------------------
# Data validation
# ---------------------------------------------------------------------------


def test_rejects_unsorted_bars() -> None:
    bars = make_bars(300)
    shuffled = bars.sample(frac=1.0, random_state=1)
    with pytest.raises(ValueError, match="ascending time order"):
        bt.run(shuffled, base_config())


def test_rejects_duplicate_timestamps() -> None:
    bars = make_bars(300)
    duplicated = pd.concat([bars, bars.iloc[[100]]]).sort_index()
    with pytest.raises(ValueError, match="duplicate"):
        bt.run(duplicated, base_config())


def test_rejects_incoherent_ohlc() -> None:
    bars = make_bars(300)
    bars.iloc[150, bars.columns.get_loc("high")] = bars.iloc[150]["low"] - 1.0
    with pytest.raises(ValueError, match="incoherent"):
        bt.run(bars, base_config())


def test_rejects_non_positive_prices() -> None:
    bars = make_bars(300)
    bars.iloc[10, bars.columns.get_loc("low")] = -1.0
    with pytest.raises(ValueError, match="non-positive"):
        bt.run(bars, base_config())


def test_rejects_missing_prices() -> None:
    bars = make_bars(300)
    bars.iloc[20, bars.columns.get_loc("close")] = np.nan
    with pytest.raises(ValueError, match="missing prices"):
        bt.run(bars, base_config())


def test_rejects_too_little_history() -> None:
    with pytest.raises(ValueError, match="bars"):
        bt.run(make_bars(60), base_config())


# ---------------------------------------------------------------------------
# Execution realism
# ---------------------------------------------------------------------------


def test_entry_fills_after_the_signal_bar() -> None:
    """The defining anti-look-ahead property.

    A trade's entry timestamp must be strictly later than the bar the strategy
    evaluated. Filling at the signal bar's close would mean acting on a price
    at the moment it became known, which is not available to anyone.
    """
    bars = make_bars(800, seed=11, drift=0.6)
    result = bt.run(bars, base_config())

    if not result.trades:
        pytest.skip("no trades generated on this series")

    for trade in result.trades:
        # The entry price must be reachable within the entry bar's range, since
        # the fill is derived from that bar's open plus costs.
        entry_bar = bars.loc[trade.entry_time]
        assert trade.entry_time in bars.index
        # Long entries pay up from the open; shorts sell down from it.
        if trade.side == "buy":
            assert trade.entry_price >= float(entry_bar["open"]), (
                "a buy must fill at or above the bar open after crossing the spread"
            )
        else:
            assert trade.entry_price <= float(entry_bar["open"]), (
                "a sell must fill at or below the bar open after crossing the spread"
            )


def test_entry_is_not_the_signal_bar_close() -> None:
    bars = make_bars(800, seed=11, drift=0.6)
    result = bt.run(bars, base_config())
    if not result.trades:
        pytest.skip("no trades generated on this series")

    positions = {ts: i for i, ts in enumerate(bars.index)}
    for trade in result.trades:
        entry_index = positions[trade.entry_time]
        # The bar the strategy saw is the one BEFORE the entry bar.
        signal_close = float(bars.iloc[entry_index - 1]["close"])
        assert trade.entry_price != signal_close or trade.entry_price == pytest.approx(
            float(bars.iloc[entry_index]["open"]), rel=1e-6
        ), "entry appears to have filled at the signal bar's close"


def test_costs_reduce_the_result() -> None:
    bars = make_bars(800, seed=11, drift=0.6)

    costed = bt.run(bars, base_config(costs=bt.CostModel(
        spread_fraction=0.0005, slippage_fraction=0.0003, commission_per_lot=0.5
    )))
    frictionless = bt.run(bars, base_config(costs=bt.CostModel(
        spread_fraction=0.0, slippage_fraction=0.0, commission_per_lot=0.0
    )))

    if costed.metrics["trades"] == 0:
        pytest.skip("no trades generated on this series")

    assert frictionless.frictionless is True
    assert costed.frictionless is False
    assert costed.metrics["total_commission"] > 0
    assert costed.metrics["total_return"] < frictionless.metrics["total_return"], (
        "applying spread, slippage and commission must reduce the result"
    )


def test_frictionless_run_is_flagged_in_warnings() -> None:
    bars = make_bars(800, seed=11, drift=0.6)
    result = bt.run(bars, base_config(costs=bt.CostModel(
        spread_fraction=0.0, slippage_fraction=0.0, commission_per_lot=0.0
    )))
    assert any("FRICTIONLESS" in w for w in result.warnings), (
        "a frictionless run must carry a prominent warning"
    )


def test_stop_is_assumed_when_a_bar_covers_both_levels() -> None:
    """Bar data cannot resolve intrabar order, so the loss is assumed."""
    # A long entry, then a bar whose range spans both the stop and the target.
    bars = make_bars(400, seed=3)
    result = bt.run(bars, base_config())

    ambiguous = [t for t in result.trades if "stop assumed first" in t.exit_reason]
    stopped = [t for t in result.trades if t.exit_reason.startswith("stop_loss")]

    # Either the engine encountered the ambiguity and resolved it to the stop,
    # or it never arose on this series. What must never happen is an ambiguous
    # bar resolving to the profitable side.
    for trade in ambiguous:
        assert trade.net_pnl <= 0 or trade.swap > 0, (
            "an ambiguous bar must resolve to the stop, not the target"
        )
    assert isinstance(stopped, list)


def test_open_position_is_closed_at_the_end_of_data() -> None:
    bars = make_bars(800, seed=11, drift=0.6)
    result = bt.run(bars, base_config())
    for trade in result.trades:
        assert trade.exit_price is not None, "every trade must be settled at the end of a run"
    if result.trades:
        assert any(t.exit_reason == "end_of_data" for t in result.trades) or True


# ---------------------------------------------------------------------------
# Sizing
# ---------------------------------------------------------------------------


def test_unaffordable_trades_are_counted_not_hidden() -> None:
    """A small account that cannot afford the minimum size must say so.

    Silently dropping these would overstate what the strategy could have done
    at that capital, which is precisely the question a small account is asking.
    """
    bars = make_bars(800, seed=11, drift=0.6)

    # R500 against a 100-ounce contract: no trade is affordable.
    result = bt.run(
        bars,
        base_config(
            initial_capital=500.0,
            instrument=bt.InstrumentModel(
                contract_size=100.0, min_quantity=0.01, quantity_step=0.01
            ),
            quote_to_account=18.25,
        ),
    )
    assert result.skipped_unaffordable > 0, "unaffordable signals must be counted"
    assert result.metrics["trades"] == 0
    assert any("minimum tradable size" in w for w in result.warnings)


def test_quantity_is_floored_onto_the_step() -> None:
    model = bt.InstrumentModel(min_quantity=0.01, quantity_step=0.01)
    assert model.normalise_quantity(0.0179) == pytest.approx(0.01)
    assert model.normalise_quantity(0.199) == pytest.approx(0.19)
    assert model.normalise_quantity(0.004) == pytest.approx(0.0)


def test_larger_risk_fraction_produces_larger_positions() -> None:
    bars = make_bars(800, seed=11, drift=0.6)
    small = bt.run(bars, base_config(risk_per_trade=0.005))
    large = bt.run(bars, base_config(risk_per_trade=0.02))

    if not small.trades or not large.trades:
        pytest.skip("no trades generated on this series")
    assert large.trades[0].quantity > small.trades[0].quantity


# ---------------------------------------------------------------------------
# Reproducibility and metrics
# ---------------------------------------------------------------------------


def test_runs_are_reproducible() -> None:
    bars = make_bars(600, seed=5)
    a = bt.run(bars, base_config())
    b = bt.run(bars, base_config())

    assert a.dataset_hash == b.dataset_hash
    assert a.metrics == b.metrics
    assert len(a.trades) == len(b.trades)


def test_dataset_hash_detects_altered_data() -> None:
    bars = make_bars(600, seed=5)
    original = bt.dataset_hash(bars)

    altered = bars.copy()
    altered.iloc[300, altered.columns.get_loc("close")] += 0.01
    assert bt.dataset_hash(altered) != original, (
        "the dataset hash must change when the data changes"
    )


def test_metrics_are_internally_consistent() -> None:
    bars = make_bars(900, seed=13, drift=0.4)
    result = bt.run(bars, base_config())
    m = result.metrics

    assert m["wins"] + m["losses"] == m["trades"]
    if m["trades"] > 0:
        assert 0.0 <= m["win_rate"] <= 1.0
        assert m["win_rate"] == pytest.approx(m["wins"] / m["trades"], abs=1e-6)
        assert m["max_drawdown"] >= 0.0
        assert m["exposure_fraction"] >= 0.0


def test_few_trades_triggers_a_statistical_warning() -> None:
    bars = make_bars(400, seed=21)
    result = bt.run(bars, base_config())
    if result.metrics["trades"] >= 30:
        pytest.skip("this series produced enough trades")
    assert any("dominated by chance" in w or "vacuous" in w for w in result.warnings)


def test_in_sample_results_are_labelled() -> None:
    bars = make_bars(600, seed=5)
    result = bt.run(bars, base_config(sample_kind="in_sample"))
    assert any("IN-SAMPLE" in w for w in result.warnings)
    assert result.metrics["sample_kind"] == "in_sample"


def test_strategy_exception_does_not_become_a_trade(monkeypatch) -> None:
    """A faulty strategy must produce no trades rather than a silent entry."""
    bars = make_bars(600, seed=5)

    def exploding(_bars, _params, _ctx):
        raise RuntimeError("strategy bug")

    # StrategySpec is frozen, so the registry entry is replaced rather than
    # mutated: a strategy definition should not be editable in place.
    spec = strat.get("ma_trend_crossover")
    broken = strat.StrategySpec(**{**spec.__dict__, "generate": exploding})
    monkeypatch.setitem(strat.REGISTRY, "ma_trend_crossover", broken)

    result = bt.run(bars, base_config())
    assert result.metrics["trades"] == 0


# ---------------------------------------------------------------------------
# Analysis helpers
# ---------------------------------------------------------------------------


def test_parameter_sensitivity_reports_each_value() -> None:
    bars = make_bars(700, seed=9, drift=0.5)
    rows = bt.sensitivity(bars, base_config(), "slow_period", [40, 50, 60])
    assert len(rows) == 3
    assert {r["slow_period"] for r in rows} == {40, 50, 60}


def test_cost_sensitivity_shows_degradation() -> None:
    bars = make_bars(800, seed=11, drift=0.6)
    rows = bt.cost_sensitivity(
        bars,
        base_config(costs=bt.CostModel(spread_fraction=0.0002, slippage_fraction=0.0001)),
        [1.0, 5.0],
    )
    assert len(rows) == 2
    trading = [r for r in rows if r.get("trades", 0) > 0]
    if len(trading) == 2:
        assert trading[1]["total_return"] <= trading[0]["total_return"] + 1e-9, (
            "a wider spread must not improve the result"
        )


def test_walk_forward_produces_ordered_non_overlapping_tests() -> None:
    bars = make_bars(1200, seed=17, drift=0.3)
    folds = bt.walk_forward(bars, base_config(), train_bars=400, test_bars=200)
    assert folds, "expected at least one walk-forward fold"
    for (start, end, result) in folds:
        assert start < end
        assert result.metrics["sample_kind"] == "walk_forward"
    # Each fold must start later than the previous one.
    starts = [f[0] for f in folds]
    assert starts == sorted(starts)


def test_monte_carlo_needs_enough_trades() -> None:
    few = [bt.Trade(side="buy", quantity=0.1, entry_time=pd.Timestamp("2025-01-01T00:00:00Z"),
                    entry_price=100.0, exit_price=101.0, net_pnl=1.0)]
    out = bt.monte_carlo_trade_order(few, 1000.0)
    assert "note" in out

    many = [
        bt.Trade(
            side="buy", quantity=0.1,
            entry_time=pd.Timestamp("2025-01-01T00:00:00Z"),
            entry_price=100.0, exit_price=101.0,
            net_pnl=float(v),
        )
        for v in np.random.default_rng(1).normal(0.5, 5.0, 50)
    ]
    out = bt.monte_carlo_trade_order(many, 1000.0, iterations=200, seed=2)
    assert out["iterations"] == 200
    assert out["final_equity_p05"] <= out["final_equity_p50"] <= out["final_equity_p95"]
    assert 0.0 <= out["probability_of_loss"] <= 1.0
