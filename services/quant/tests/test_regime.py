"""What the regime classifier says, and why it matters more than it looks.

The consensus policy discards every strategy whose declared regimes exclude the
current one, and no PAPER-promoted strategy declares itself valid in UNKNOWN.
So an UNKNOWN verdict is not a soft outcome: it is a guaranteed NO TRADE for
that bar, whatever the strategies saw.

These tests pin the boundaries so a later edit cannot quietly widen or narrow
the band that produces it.
"""

from __future__ import annotations

import numpy as np
import pandas as pd

from vantage_quant import scanner


def _frame(closes: list[float], spread: float = 1.0) -> pd.DataFrame:
    """A bar series from a close path, with plausible highs and lows."""
    closes_arr = np.asarray(closes, dtype=float)
    opens = np.concatenate(([closes_arr[0]], closes_arr[:-1]))
    return pd.DataFrame(
        {
            "open": opens,
            "high": np.maximum(opens, closes_arr) + spread,
            "low": np.minimum(opens, closes_arr) - spread,
            "close": closes_arr,
            "volume": np.full(len(closes_arr), 1000.0),
        },
        index=pd.date_range("2026-01-05", periods=len(closes_arr), freq="1h", tz="UTC"),
    )


def test_a_thin_series_is_unknown_rather_than_guessed() -> None:
    """Fewer than 60 bars cannot support a verdict, and inventing one would let
    a trend strategy run in a market nobody has characterised."""
    regime, measures = scanner.classify_regime(_frame([2000.0 + i for i in range(30)]))
    assert regime == "UNKNOWN"
    assert measures == {}


def test_a_clean_trend_is_trending() -> None:
    regime, _ = scanner.classify_regime(_frame([2000.0 + i * 2.0 for i in range(200)]))
    assert regime == "TRENDING"


def test_an_oscillation_is_ranging_not_trending() -> None:
    """ADX alone reads a clean cycle as strongly directional: four bars up and
    four down are exactly the sustained runs it measures. Displacement is what
    separates a trend from a market that returns to where it started."""
    closes = [2000.0 + 25.0 * np.sin(i / 4.0) for i in range(240)]
    regime, measures = scanner.classify_regime(_frame(closes))
    assert regime == "RANGING", f"measures={measures}"


def test_event_risk_overrides_the_price_read() -> None:
    """An imminent release changes how every other measure should be read."""
    regime, _ = scanner.classify_regime(
        _frame([2000.0 + i * 2.0 for i in range(200)]), event_risk="high"
    )
    assert regime == "EVENT_RISK"


def test_a_market_going_nowhere_is_a_range_at_any_adx() -> None:
    """The inconsistency this test exists for.

    A market whose displacement is small relative to the distance travelled is
    a range. The classifier said so for ADX above 25 and then, for ADX between
    20 and 25 -- LESS directional by the same measure -- fell through to
    UNKNOWN. The same market got two labels, and the weaker reading got the
    more conservative one.

    Measured across the sixteen generated conditions, that fallthrough took
    9.5% of all decision points, every one of them a guaranteed NO TRADE.
    """
    rng = np.random.default_rng(20260930)
    # A slow drift with heavy mean reversion: enough directional persistence to
    # lift ADX off the floor, not enough displacement to be a trend.
    closes, level = [], 2000.0
    for _ in range(300):
        level += rng.normal(0.0, 1.2) + (2000.0 - level) * 0.05
        closes.append(level)

    regime, measures = scanner.classify_regime(_frame(closes))
    assert regime != "UNKNOWN", (
        f"a market going nowhere was called unclassifiable: {measures}. "
        f"Every UNKNOWN bar is a guaranteed NO TRADE."
    )


def test_a_weak_but_efficient_move_stays_unknown() -> None:
    """The band is narrowed, not removed.

    Price getting somewhere while ADX has not confirmed it is what an emerging
    trend and a false start look like alike. That is genuinely ambiguous, and
    it is exactly when a mean-reversion strategy must not be told it is safe.

    The window is FOUND in the project's own deterministic generator rather
    than hand-built: an ADX between 20 and 25 that is simultaneously efficient
    is a narrow target, and a contrived series that misses it tests the wrong
    branch while looking like it passed. An earlier attempt did exactly that --
    a clean 14-bar run put ADX at 89.
    """
    from vantage_quant.research import datasets

    found = None
    for spec in datasets.research_suite(bars_per_condition=600):
        frame = datasets.generate(spec).frame
        for end in range(60, len(frame), 3):
            regime, measures = scanner.classify_regime(frame.iloc[:end])
            adx, eff = measures.get("adx", 0.0), measures.get("efficiency", 0.0)
            if 20.0 < adx <= 25.0 and eff >= scanner.TREND_EFFICIENCY_MIN:
                found = (regime, measures)
                break
        if found:
            break

    assert found is not None, (
        "no window in the generated suite lands in the ambiguous band; this "
        "test can no longer check what it claims to"
    )
    regime, measures = found
    assert regime == "UNKNOWN", (
        f"a weak-ADX but efficient move was labelled {regime}: {measures}. "
        f"That is an emerging trend or a false start, and calling it RANGING "
        f"tells a mean-reversion strategy it is safe in the one market that "
        f"is about to run."
    )


def test_the_measures_always_accompany_a_verdict() -> None:
    """A regime with no numbers behind it cannot be argued with."""
    regime, measures = scanner.classify_regime(
        _frame([2000.0 + i * 1.5 for i in range(200)])
    )
    assert regime != "UNKNOWN"
    for key in ("adx", "efficiency", "volatility_ratio", "atr_fraction"):
        assert key in measures, f"{key} missing from the account of the label"
