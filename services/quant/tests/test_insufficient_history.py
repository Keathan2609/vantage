"""A strategy that could not form an opinion must say so, machine-readably.

Every strategy declares `required_bars`, and `evaluate` already refuses below
it with a prose reason. Prose is not enough. The control plane records the
answer, and until it can tell "the strategy looked and declined" apart from
"the strategy was never given enough history", the two are the same row in
`strategy_runs` and the same abstention in the consensus.

That is not hypothetical. Measured over the replay fixtures, with a 60-instant
warm-up and a 50-bar floor in the orchestrator against strategies needing 120:

    trend_clean       280 of 405 recorded signals (69%) could only be no_trade
    drawdown          280 of 305 (92%)
    false_breakout    280 of 305 (92%)

Every one was written down as a strategy's opinion at confidence 0.000, and
read back as "no strategy fired on a clean trend". Given the history they ask
for, the same strategies produce 49 actionable signals on trend_clean at mean
confidence 0.561.
"""

from __future__ import annotations

import numpy as np
import pandas as pd
import pytest

from vantage_quant import strategies as strat

ALL_KEYS = sorted(strat.REGISTRY)


def bars(n: int) -> pd.DataFrame:
    """A plain rising series. The shape does not matter here; the length does."""
    close = 2650.0 + np.arange(n, dtype=float) * 0.5
    open_ = np.concatenate([[close[0]], close[:-1]])
    return pd.DataFrame(
        {
            "open": open_,
            "high": np.maximum(open_, close) + 1.0,
            "low": np.minimum(open_, close) - 1.0,
            "close": close,
            "volume": np.full(n, 1000.0),
        },
        index=pd.date_range("2026-01-05T00:00:00Z", periods=n, freq="1h"),
    )


@pytest.mark.parametrize("key", ALL_KEYS)
def test_a_starved_evaluation_is_distinguishable_from_an_opinion(key: str) -> None:
    """One bar short of the requirement is a refusal to answer, not an answer."""
    spec = strat.get(key)
    signal, _ = strat.evaluate(key, bars(spec.required_bars - 1), None, None)

    assert signal.action == "no_trade"
    assert signal.insufficient_history is True, (
        f"{key} was given {spec.required_bars - 1} bars and needs "
        f"{spec.required_bars}, and reported the refusal only in prose. The "
        f"control plane cannot tell that apart from an abstention."
    )
    assert signal.required_bars == spec.required_bars


@pytest.mark.parametrize("key", ALL_KEYS)
def test_a_fed_evaluation_is_never_marked_starved(key: str) -> None:
    """The flag must not be a synonym for no_trade.

    A strategy with ample history that declines has formed an opinion, and
    dropping that from the consensus would be the opposite defect.
    """
    spec = strat.get(key)
    signal, _ = strat.evaluate(key, bars(spec.required_bars + 200), None, None)

    assert signal.insufficient_history is False, (
        f"{key} had ample history and was still marked starved; the flag would "
        f"then silence real abstentions"
    )
    assert signal.required_bars == spec.required_bars


def test_the_boundary_is_exactly_the_declared_requirement() -> None:
    """Off by one here moves a whole bar of every replay between the two
    categories, so it is pinned rather than left to the implementation."""
    for key in ALL_KEYS:
        spec = strat.get(key)
        short, _ = strat.evaluate(key, bars(spec.required_bars - 1), None, None)
        exact, _ = strat.evaluate(key, bars(spec.required_bars), None, None)
        assert short.insufficient_history is True, key
        assert exact.insufficient_history is False, key
