"""Technical indicators.

Every function here is a PURE transformation of a price series into another
series of the same length, aligned to the same index, with leading positions
filled with NaN until enough history exists to compute a value.

Two properties matter more than any individual formula:

1.  **No look-ahead.** The value at index ``i`` depends only on data at indices
    ``<= i``. This is asserted by a test that recomputes each indicator on a
    truncated series and requires the overlapping values to be identical. An
    indicator that fails that test makes every backtest using it fiction.

2.  **Explicit warm-up.** A moving average of period 20 has no value before the
    twentieth observation. Returning a partial average there — which some
    libraries do by default — silently produces confident values from
    insufficient data, and a strategy reading them trades noise at the start of
    every series.

Indicators use float arithmetic deliberately. They are analytical signals, not
money: precision beyond float64 is meaningless against market noise, and the
ledger never sees these numbers. Money uses decimals throughout the control
plane.
"""

from __future__ import annotations

import numpy as np
import pandas as pd

__all__ = [
    "adx",
    "atr",
    "bollinger",
    "donchian",
    "ema",
    "macd",
    "roc",
    "rolling_correlation",
    "rolling_volatility",
    "rsi",
    "sma",
    "stochastic",
    "support_resistance",
    "true_range",
    "wma",
    "zscore",
]


def _validate(series: pd.Series, period: int, name: str) -> None:
    if period < 1:
        raise ValueError(f"{name}: period must be at least 1, got {period}")
    if not isinstance(series, pd.Series):
        raise TypeError(f"{name}: expected a pandas Series")


def sma(series: pd.Series, period: int) -> pd.Series:
    """Simple moving average."""
    _validate(series, period, "sma")
    return series.rolling(window=period, min_periods=period).mean()


def ema(series: pd.Series, period: int) -> pd.Series:
    """Exponential moving average.

    Seeded with a simple average of the first ``period`` observations rather
    than with the first value alone. Seeding from one observation lets a single
    outlier dominate the early series, and the difference persists for many
    bars.
    """
    _validate(series, period, "ema")
    if len(series) < period:
        return pd.Series(np.nan, index=series.index, dtype="float64")

    alpha = 2.0 / (period + 1.0)
    values = series.to_numpy(dtype="float64")
    out = np.full(len(values), np.nan, dtype="float64")

    seed = np.nanmean(values[:period])
    out[period - 1] = seed
    for i in range(period, len(values)):
        if np.isnan(values[i]):
            out[i] = out[i - 1]
            continue
        out[i] = alpha * values[i] + (1 - alpha) * out[i - 1]
    return pd.Series(out, index=series.index)


def wma(series: pd.Series, period: int) -> pd.Series:
    """Linearly weighted moving average."""
    _validate(series, period, "wma")
    weights = np.arange(1, period + 1, dtype="float64")
    weights /= weights.sum()
    return series.rolling(window=period, min_periods=period).apply(
        lambda w: float(np.dot(w, weights)), raw=True
    )


def rsi(series: pd.Series, period: int = 14) -> pd.Series:
    """Relative strength index, using Wilder's smoothing.

    Wilder's original formulation, not a simple average of gains and losses.
    The two diverge materially, and published RSI values follow Wilder.
    """
    _validate(series, period, "rsi")
    delta = series.diff()
    gain = delta.clip(lower=0.0)
    loss = -delta.clip(upper=0.0)

    values_gain = gain.to_numpy(dtype="float64")
    values_loss = loss.to_numpy(dtype="float64")
    n = len(series)
    out = np.full(n, np.nan, dtype="float64")
    if n <= period:
        return pd.Series(out, index=series.index)

    # float() rather than the numpy scalars these expressions produce.
    #
    # np.nanmean returns np.floating, and a newer numpy's stubs say so, which
    # makes to_rsi's `float` parameters a type error. The conversion is exact --
    # np.float64 IS an IEEE-754 double, and every operation below is the same
    # arithmetic either way -- so no indicator value changes. That matters:
    # altering an indicator would alter every strategy that reads it.
    avg_gain = float(np.nanmean(values_gain[1 : period + 1]))
    avg_loss = float(np.nanmean(values_loss[1 : period + 1]))

    def to_rsi(g: float, loss_avg: float) -> float:
        if loss_avg == 0.0:
            return 100.0 if g > 0.0 else 50.0
        rs = g / loss_avg
        return 100.0 - (100.0 / (1.0 + rs))

    out[period] = to_rsi(avg_gain, avg_loss)
    for i in range(period + 1, n):
        g = float(values_gain[i]) if not np.isnan(values_gain[i]) else 0.0
        loss_i = float(values_loss[i]) if not np.isnan(values_loss[i]) else 0.0
        avg_gain = (avg_gain * (period - 1) + g) / period
        avg_loss = (avg_loss * (period - 1) + loss_i) / period
        out[i] = to_rsi(avg_gain, avg_loss)
    return pd.Series(out, index=series.index)


def macd(
    series: pd.Series, fast: int = 12, slow: int = 26, signal: int = 9
) -> tuple[pd.Series, pd.Series, pd.Series]:
    """MACD line, signal line and histogram."""
    if fast >= slow:
        raise ValueError(f"macd: fast period ({fast}) must be shorter than slow ({slow})")
    macd_line = ema(series, fast) - ema(series, slow)
    signal_line = ema(macd_line.dropna(), signal).reindex(series.index)
    histogram = macd_line - signal_line
    return macd_line, signal_line, histogram


def true_range(high: pd.Series, low: pd.Series, close: pd.Series) -> pd.Series:
    """True range: the greatest of the three standard measures.

    Including the gap from the previous close is the point. A bar that gaps and
    then trades quietly has a small high-low range but a large true range, and
    a stop sized off high-low alone would be far too tight.
    """
    prev_close = close.shift(1)
    ranges = pd.concat(
        [high - low, (high - prev_close).abs(), (low - prev_close).abs()], axis=1
    )
    return ranges.max(axis=1)


def atr(high: pd.Series, low: pd.Series, close: pd.Series, period: int = 14) -> pd.Series:
    """Average true range, Wilder-smoothed."""
    _validate(close, period, "atr")
    tr = true_range(high, low, close).to_numpy(dtype="float64")
    n = len(close)
    out = np.full(n, np.nan, dtype="float64")
    if n <= period:
        return pd.Series(out, index=close.index)

    out[period] = np.nanmean(tr[1 : period + 1])
    for i in range(period + 1, n):
        current = tr[i] if not np.isnan(tr[i]) else out[i - 1]
        out[i] = (out[i - 1] * (period - 1) + current) / period
    return pd.Series(out, index=close.index)


def bollinger(
    series: pd.Series, period: int = 20, num_std: float = 2.0
) -> tuple[pd.Series, pd.Series, pd.Series]:
    """Bollinger bands: upper, middle and lower.

    Uses the population standard deviation (ddof=0), matching the original
    definition. The sample deviation gives slightly wider bands, which shifts
    every threshold a strategy sets against them.
    """
    _validate(series, period, "bollinger")
    middle = sma(series, period)
    deviation = series.rolling(window=period, min_periods=period).std(ddof=0)
    return middle + num_std * deviation, middle, middle - num_std * deviation


def roc(series: pd.Series, period: int = 12) -> pd.Series:
    """Rate of change, as a fraction rather than a percentage."""
    _validate(series, period, "roc")
    return series.pct_change(periods=period, fill_method=None)


def adx(
    high: pd.Series, low: pd.Series, close: pd.Series, period: int = 14
) -> tuple[pd.Series, pd.Series, pd.Series]:
    """Average directional index with the +DI and -DI components.

    ADX measures trend STRENGTH without direction, which is why it is used as a
    filter rather than a signal: a high ADX says a trend exists, not which way
    it points.
    """
    _validate(close, period, "adx")
    up_move = high.diff()
    down_move = -low.diff()

    plus_dm = np.where((up_move > down_move) & (up_move > 0), up_move, 0.0)
    minus_dm = np.where((down_move > up_move) & (down_move > 0), down_move, 0.0)

    tr = true_range(high, low, close)
    atr_series = atr(high, low, close, period)

    plus_di = 100.0 * pd.Series(plus_dm, index=close.index).rolling(
        window=period, min_periods=period
    ).mean() / atr_series
    minus_di = 100.0 * pd.Series(minus_dm, index=close.index).rolling(
        window=period, min_periods=period
    ).mean() / atr_series

    denominator = (plus_di + minus_di).replace(0.0, np.nan)
    dx = 100.0 * (plus_di - minus_di).abs() / denominator
    adx_series = dx.rolling(window=period, min_periods=period).mean()

    _ = tr  # true range is used via atr; kept explicit for readability
    return adx_series, plus_di, minus_di


def stochastic(
    high: pd.Series, low: pd.Series, close: pd.Series, period: int = 14, smooth: int = 3
) -> tuple[pd.Series, pd.Series]:
    """Stochastic oscillator %K and %D."""
    _validate(close, period, "stochastic")
    highest = high.rolling(window=period, min_periods=period).max()
    lowest = low.rolling(window=period, min_periods=period).min()
    span = (highest - lowest).replace(0.0, np.nan)
    percent_k = 100.0 * (close - lowest) / span
    percent_d = percent_k.rolling(window=smooth, min_periods=smooth).mean()
    return percent_k, percent_d


def donchian(
    high: pd.Series, low: pd.Series, period: int = 20
) -> tuple[pd.Series, pd.Series, pd.Series]:
    """Donchian channel: upper, middle and lower.

    The channel is computed over the PREVIOUS ``period`` bars, excluding the
    current one. Including the current bar's own high in the upper band makes a
    breakout trivially true whenever the bar sets a new high, which is the
    single most common way a channel-breakout backtest fools its author.
    """
    _validate(high, period, "donchian")
    upper = high.shift(1).rolling(window=period, min_periods=period).max()
    lower = low.shift(1).rolling(window=period, min_periods=period).min()
    return upper, (upper + lower) / 2.0, lower


def rolling_volatility(
    series: pd.Series, period: int = 20, annualise: int | None = None
) -> pd.Series:
    """Standard deviation of log returns.

    ``annualise`` is the number of periods per year for the series' timeframe.
    When omitted the raw per-period deviation is returned, which is what a stop
    or a position size should be computed from.
    """
    _validate(series, period, "rolling_volatility")
    # np.log over a Series is typed as Any, which silently disables checking
    # of everything downstream of it. Re-wrapping keeps the Series contract.
    log_returns = pd.Series(np.log(series / series.shift(1)), index=series.index)
    vol: pd.Series = log_returns.rolling(window=period, min_periods=period).std(ddof=0)
    if annualise:
        vol = vol * float(np.sqrt(annualise))
    return vol


def zscore(series: pd.Series, period: int = 20) -> pd.Series:
    """Rolling z-score: deviation from the mean in standard deviations."""
    _validate(series, period, "zscore")
    mean = series.rolling(window=period, min_periods=period).mean()
    std = series.rolling(window=period, min_periods=period).std(ddof=0).replace(0.0, np.nan)
    return (series - mean) / std


def rolling_correlation(a: pd.Series, b: pd.Series, period: int = 60) -> pd.Series:
    """Rolling Pearson correlation of two return series.

    Correlation is treated as risk INFORMATION, not as a constant. It is
    unstable, and it tends to move toward one across risk assets in exactly the
    conditions where a diversification assumption is being relied on.
    """
    _validate(a, period, "rolling_correlation")
    return a.rolling(window=period, min_periods=period).corr(b)


def support_resistance(
    high: pd.Series, low: pd.Series, lookback: int = 50, touches: int = 2, tolerance: float = 0.002
) -> tuple[list[float], list[float]]:
    """Cluster recent swing highs and lows into resistance and support levels.

    A level is only returned when at least ``touches`` swing points fall within
    ``tolerance`` of it, so a single spike does not become a "level".
    """
    if lookback < 5:
        raise ValueError("support_resistance: lookback must be at least 5")

    highs = high.tail(lookback)
    lows = low.tail(lookback)

    def cluster(points: pd.Series) -> list[float]:
        values = sorted(float(v) for v in points.dropna())
        levels: list[float] = []
        group: list[float] = []
        for value in values:
            if group and abs(value - group[-1]) / max(group[-1], 1e-9) > tolerance:
                if len(group) >= touches:
                    levels.append(sum(group) / len(group))
                group = []
            group.append(value)
        if len(group) >= touches:
            levels.append(sum(group) / len(group))
        return levels

    swing_highs = highs[(highs.shift(1) < highs) & (highs.shift(-1) < highs)]
    swing_lows = lows[(lows.shift(1) > lows) & (lows.shift(-1) > lows)]
    return cluster(swing_highs), cluster(swing_lows)
