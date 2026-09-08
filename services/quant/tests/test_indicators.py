"""Indicator correctness tests.

Two kinds of assertion appear here, and the second matters more:

*   **Golden vectors.** RSI is checked against the widely published worked
    example for Wilder's formulation. An indicator that merely "looks
    reasonable" on a chart can be wrong by enough to invert a signal.

*   **Structural properties.** Every indicator is checked for look-ahead:
    computing it over the first N bars must give identical values to computing
    it over the whole series and truncating. An indicator that fails this makes
    every backtest that uses it worthless, and the failure is invisible in the
    output.
"""

from __future__ import annotations

import numpy as np
import pandas as pd
import pytest

from vantage_quant import indicators as ind

# Wilder's published RSI worked example.
RSI_CLOSES = [
    44.34, 44.09, 44.15, 43.61, 44.33, 44.83, 45.10, 45.42, 45.84, 46.08,
    45.89, 46.03, 45.61, 46.28, 46.28, 46.00, 46.03, 46.41, 46.22, 45.64,
    46.21, 46.25, 45.71, 46.45, 45.78, 45.35, 44.03, 44.18, 44.22, 44.57,
    43.42, 42.66, 43.13,
]

# Expected RSI(14) from the same worked example, starting at index 14.
RSI_EXPECTED = [
    70.53, 66.32, 66.55, 69.41, 66.36, 57.97, 62.93, 63.26, 56.06, 62.38,
    54.71, 50.42, 39.99, 41.46, 41.87, 45.46, 37.30, 33.08, 37.77,
]


@pytest.fixture
def closes() -> pd.Series:
    return pd.Series(RSI_CLOSES, dtype="float64")


@pytest.fixture
def ohlc() -> pd.DataFrame:
    """A deterministic OHLC frame with coherent bars."""
    rng = np.random.default_rng(20250101)
    n = 300
    close = 2650.0 + np.cumsum(rng.normal(0, 3.0, n))
    spread = np.abs(rng.normal(4.0, 1.5, n))
    high = close + spread
    low = close - spread
    open_ = np.concatenate([[close[0]], close[:-1]])
    high = np.maximum.reduce([high, open_, close])
    low = np.minimum.reduce([low, open_, close])
    return pd.DataFrame({"open": open_, "high": high, "low": low, "close": close})


def test_rsi_matches_published_values(closes: pd.Series) -> None:
    result = ind.rsi(closes, period=14)

    # Nothing before the warm-up completes.
    assert result.iloc[:14].isna().all(), "RSI must not report values during warm-up"

    got = result.iloc[14 : 14 + len(RSI_EXPECTED)].to_numpy()
    # Tolerance of 0.1 on a 0-100 scale. The published table is quoted to two
    # decimals and rounds its intermediate smoothed averages at each step,
    # which accumulates a small offset against an implementation that carries
    # full precision throughout. The tolerance is still tight enough to catch
    # a genuine formulation error: using simple averages instead of Wilder's
    # smoothing shifts these values by several points, not hundredths.
    np.testing.assert_allclose(got, RSI_EXPECTED, atol=0.1)


def test_rsi_bounds() -> None:
    rising = pd.Series(np.arange(1, 60, dtype="float64"))
    falling = pd.Series(np.arange(60, 1, -1, dtype="float64"))

    # A monotonically rising series has no losses, so RSI pins at 100.
    assert ind.rsi(rising, 14).dropna().max() == pytest.approx(100.0)
    # A monotonically falling series pins at 0.
    assert ind.rsi(falling, 14).dropna().min() == pytest.approx(0.0, abs=1e-9)

    mixed = pd.Series(RSI_CLOSES)
    values = ind.rsi(mixed, 14).dropna()
    assert ((values >= 0) & (values <= 100)).all(), "RSI must stay within [0, 100]"


def test_sma_is_an_arithmetic_mean() -> None:
    series = pd.Series([1.0, 2.0, 3.0, 4.0, 5.0, 6.0])
    result = ind.sma(series, 3)
    assert result.iloc[:2].isna().all()
    assert result.iloc[2] == pytest.approx(2.0)
    assert result.iloc[5] == pytest.approx(5.0)


def test_ema_seeds_from_a_simple_average() -> None:
    series = pd.Series([10.0, 11.0, 12.0, 13.0, 14.0, 15.0])
    result = ind.ema(series, 3)
    # The seed is the mean of the first three observations.
    assert result.iloc[2] == pytest.approx(11.0)
    # Subsequent values apply alpha = 2/(period+1) = 0.5.
    assert result.iloc[3] == pytest.approx(0.5 * 13.0 + 0.5 * 11.0)


def test_ema_responds_faster_than_sma() -> None:
    # A step change: the EMA must move toward the new level faster.
    series = pd.Series([10.0] * 20 + [20.0] * 5)
    ema_val = ind.ema(series, 10).iloc[-1]
    sma_val = ind.sma(series, 10).iloc[-1]
    assert ema_val > sma_val


def test_true_range_includes_the_gap(ohlc: pd.DataFrame) -> None:
    # A bar with a tiny high-low range that gaps far from the previous close
    # must report a LARGE true range, or a stop sized from it would be far too
    # tight through exactly the conditions that cause gaps.
    frame = pd.DataFrame(
        {
            "high": [100.0, 130.0],
            "low": [99.0, 129.5],
            "close": [99.5, 129.8],
        }
    )
    tr = ind.true_range(frame["high"], frame["low"], frame["close"])
    assert tr.iloc[1] == pytest.approx(30.5), "true range must span the gap from the prior close"


def test_atr_is_positive_and_warms_up(ohlc: pd.DataFrame) -> None:
    result = ind.atr(ohlc["high"], ohlc["low"], ohlc["close"], 14)
    assert result.iloc[:14].isna().all()
    assert (result.dropna() > 0).all()


def test_bollinger_bands_are_ordered(ohlc: pd.DataFrame) -> None:
    upper, middle, lower = ind.bollinger(ohlc["close"], 20, 2.0)
    valid = upper.notna()
    assert (upper[valid] >= middle[valid]).all()
    assert (middle[valid] >= lower[valid]).all()

    # Two standard deviations should contain the large majority of observations.
    inside = ((ohlc["close"] <= upper) & (ohlc["close"] >= lower))[valid]
    assert inside.mean() > 0.85


def test_macd_histogram_is_the_difference(ohlc: pd.DataFrame) -> None:
    macd_line, signal_line, histogram = ind.macd(ohlc["close"])
    valid = histogram.notna()
    np.testing.assert_allclose(
        histogram[valid].to_numpy(),
        (macd_line[valid] - signal_line[valid]).to_numpy(),
        atol=1e-9,
    )


def test_macd_rejects_inverted_periods(ohlc: pd.DataFrame) -> None:
    with pytest.raises(ValueError):
        ind.macd(ohlc["close"], fast=26, slow=12)


def test_donchian_excludes_the_current_bar() -> None:
    # The whole point: a bar that sets a new high must NOT already be above its
    # own channel, or every new high registers as a breakout.
    high = pd.Series([10.0, 11.0, 12.0, 13.0, 20.0])
    low = pd.Series([9.0, 10.0, 11.0, 12.0, 19.0])
    upper, _, lower = ind.donchian(high, low, period=4)

    # At the final bar the channel is built from bars 0..3, whose max is 13.
    assert upper.iloc[4] == pytest.approx(13.0)
    assert lower.iloc[4] == pytest.approx(9.0)
    # And the breakout is therefore detectable.
    assert high.iloc[4] > upper.iloc[4]


def test_stochastic_is_bounded(ohlc: pd.DataFrame) -> None:
    percent_k, percent_d = ind.stochastic(ohlc["high"], ohlc["low"], ohlc["close"])
    k = percent_k.dropna()
    assert ((k >= 0) & (k <= 100)).all()
    assert percent_d.dropna().between(0, 100).all()


def test_zscore_is_centred(ohlc: pd.DataFrame) -> None:
    z = ind.zscore(ohlc["close"], 50).dropna()
    # Over a random walk the z-score should be roughly centred and mostly
    # within a few standard deviations.
    assert abs(z.mean()) < 1.0
    assert (z.abs() < 6).all()


def test_rolling_volatility_scales_with_annualisation(ohlc: pd.DataFrame) -> None:
    raw = ind.rolling_volatility(ohlc["close"], 20).dropna()
    annual = ind.rolling_volatility(ohlc["close"], 20, annualise=252).dropna()
    np.testing.assert_allclose(annual.to_numpy(), raw.to_numpy() * np.sqrt(252), rtol=1e-9)


def test_adx_is_bounded_and_directional(ohlc: pd.DataFrame) -> None:
    adx_series, _plus_di, _minus_di = ind.adx(ohlc["high"], ohlc["low"], ohlc["close"], 14)
    valid = adx_series.dropna()
    assert ((valid >= 0) & (valid <= 100)).all()

    # In a persistent uptrend +DI should dominate -DI.
    n = 120
    trend_close = pd.Series(np.linspace(100, 200, n))
    trend_high = trend_close + 1.0
    trend_low = trend_close - 1.0
    _, p_di, m_di = ind.adx(trend_high, trend_low, trend_close, 14)
    assert p_di.dropna().mean() > m_di.dropna().mean()


def test_support_resistance_requires_multiple_touches() -> None:
    # A single spike must not become a "level". The repeated base pattern
    # supplies the swing points; the spike occurs exactly ONCE, appended at the
    # end, so it has one touch and must be discarded.
    base_high = [100.0, 101.0, 100.5, 100.2, 100.4, 101.0, 100.6] * 7
    base_low = [99.0, 98.5, 99.2, 98.8, 99.1, 98.6, 99.3] * 7
    high = pd.Series([*base_high, 100.3, 130.0, 100.2])
    low = pd.Series([*base_low, 99.0, 98.0, 99.1])
    resistance, support = ind.support_resistance(high, low, lookback=50, touches=3)
    assert all(level < 120 for level in resistance), "an isolated spike must not form a level"
    assert support, "repeated swing lows should form at least one support level"


@pytest.mark.parametrize(
    "name,fn",
    [
        ("sma", lambda df: ind.sma(df["close"], 20)),
        ("ema", lambda df: ind.ema(df["close"], 20)),
        ("wma", lambda df: ind.wma(df["close"], 20)),
        ("rsi", lambda df: ind.rsi(df["close"], 14)),
        ("atr", lambda df: ind.atr(df["high"], df["low"], df["close"], 14)),
        ("roc", lambda df: ind.roc(df["close"], 12)),
        ("zscore", lambda df: ind.zscore(df["close"], 20)),
        ("volatility", lambda df: ind.rolling_volatility(df["close"], 20)),
        ("bollinger_upper", lambda df: ind.bollinger(df["close"], 20)[0]),
        ("donchian_upper", lambda df: ind.donchian(df["high"], df["low"], 20)[0]),
        ("macd_line", lambda df: ind.macd(df["close"])[0]),
        ("stochastic_k", lambda df: ind.stochastic(df["high"], df["low"], df["close"])[0]),
        ("adx", lambda df: ind.adx(df["high"], df["low"], df["close"], 14)[0]),
    ],
)
def test_no_lookahead(ohlc: pd.DataFrame, name: str, fn) -> None:
    """An indicator's value at bar i must not depend on bars after i.

    This is the test that makes backtests trustworthy. It recomputes each
    indicator over a prefix of the data and requires the overlapping values to
    be identical: if a future bar can change a past value, the strategy reading
    it in a backtest is reading information it could not have had.
    """
    cutoff = 200
    full = fn(ohlc)
    prefix = fn(ohlc.iloc[:cutoff].copy())

    a = full.iloc[:cutoff].to_numpy(dtype="float64")
    b = prefix.to_numpy(dtype="float64")

    both_nan = np.isnan(a) & np.isnan(b)
    comparable = ~both_nan
    np.testing.assert_allclose(
        a[comparable], b[comparable], rtol=1e-9, atol=1e-9,
        err_msg=f"{name} changed a historical value when future bars were added",
    )


def test_indicators_reject_invalid_periods(ohlc: pd.DataFrame) -> None:
    for fn in (
        lambda: ind.sma(ohlc["close"], 0),
        lambda: ind.ema(ohlc["close"], -1),
        lambda: ind.rsi(ohlc["close"], 0),
        lambda: ind.zscore(ohlc["close"], 0),
    ):
        with pytest.raises(ValueError):
            fn()


def test_indicators_are_length_preserving_and_aligned(ohlc: pd.DataFrame) -> None:
    for series in (
        ind.sma(ohlc["close"], 20),
        ind.rsi(ohlc["close"], 14),
        ind.atr(ohlc["high"], ohlc["low"], ohlc["close"], 14),
        ind.macd(ohlc["close"])[0],
    ):
        assert len(series) == len(ohlc)
        assert series.index.equals(ohlc.index)
