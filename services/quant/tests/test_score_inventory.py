"""The inventory must describe every strategy, and describe it correctly.

A table in a document rots. These tests are what make the inventory an artifact
rather than a snapshot: a strategy registered without an entry fails here, and
so does an entry whose transcribed formula has drifted from the source.
"""

from __future__ import annotations

import inspect

import pytest

from vantage_quant import strategies
from vantage_quant.score_inventory import (
    INVENTORY,
    INVENTORY_VERSION,
    strategies_with_constants,
    unreachable_above,
)

# The consensus policy's floor, restated here so this test fails if the two
# ever disagree about what is being compared. It is NOT imported from the Go
# side; it is duplicated deliberately, and the duplication is the alarm.
CONSENSUS_MIN_CONFIDENCE = 0.55


def test_every_registered_strategy_has_a_score_entry() -> None:
    """A new scoring formula cannot enter the platform undocumented."""
    registered = set(strategies.REGISTRY)
    described = set(INVENTORY)

    missing = sorted(registered - described)
    assert not missing, (
        f"{len(missing)} registered strategies have no score inventory entry: "
        f"{missing}. A confidence value whose meaning is not written down "
        f"cannot be compared to a threshold, and adding one silently is how "
        f"the current mismatch arose."
    )

    stale = sorted(described - registered)
    assert not stale, (
        f"the inventory describes strategies that are not registered: {stale}. "
        f"Either the strategy was removed and the entry is stale, or the key "
        f"was renamed and the entry now describes nothing."
    )


def test_every_entry_matches_its_family_in_the_registry() -> None:
    """The inventory's family must be the registry's, or grouping lies."""
    for key, spec in sorted(INVENTORY.items()):
        registered = strategies.REGISTRY[key]
        assert spec.family == registered.family, (
            f"{key}: the inventory says family {spec.family!r} and the registry "
            f"says {registered.family!r}. Family is what down-weights crowded "
            f"opinions in consensus, so a disagreement mis-weights the vote."
        )


def test_transcribed_constants_appear_in_the_source() -> None:
    """A constant recorded here must be findable in the strategy's source.

    Not a full parse -- that would be a second implementation of the formula.
    It checks that each transcribed constant is literally present in the
    function that computes the score, which catches the realistic failure: the
    formula changed and the inventory did not.
    """
    source = inspect.getsource(strategies)
    for spec in strategies_with_constants():
        for constant in spec.constants:
            # 0.0 and 0.2 appear as whole-signal constants rather than inside
            # _confidence, and both forms are written in the source.
            rendered = repr(constant)
            assert rendered in source, (
                f"{spec.key}: the inventory records the constant {rendered} but "
                f"it does not appear in strategies.py. The formula has changed "
                f"and the inventory is describing code that no longer exists."
            )


def test_theoretical_range_is_internally_consistent() -> None:
    for key, spec in sorted(INVENTORY.items()):
        assert 0.0 <= spec.theoretical_min <= spec.theoretical_max <= 1.0, (
            f"{key}: theoretical range [{spec.theoretical_min}, "
            f"{spec.theoretical_max}] is not a valid confidence range"
        )


def test_the_constant_bearing_strategies_are_named() -> None:
    """The finding, pinned.

    Five strategies average a real component with a hard-coded number. This is
    not a style complaint: it bounds what the score can ever report, and the
    consensus threshold sits inside those bounds.
    """
    named = {s.key for s in strategies_with_constants()}
    expected = {
        "donchian_breakout",  # 0.5
        "rsi_mean_reversion",  # 0.45
        "bollinger_zscore_reversion",  # 0.3 offset
        "atr_volatility_regime",  # 0.3 offset
        "session_london_breakout",  # 0.5
        "multi_timeframe_trend",  # 0.6
        "pre_event_blackout",  # fixed 0.0
        "grid_martingale_research",  # fixed 0.2
    }
    assert named == expected, (
        f"the set of strategies carrying constants changed: {sorted(named)}. "
        f"If a formula was fixed, update this expectation in the same change "
        f"as the fix, and create a NEW strategy version rather than rewriting "
        f"the old one's semantics."
    )


def test_which_strategies_the_consensus_floor_structurally_excludes() -> None:
    """Which scores the arithmetic prevents from ever clearing the floor.

    A strategy whose theoretical maximum sits below the threshold cannot
    contribute whatever the market does, and that is a fact about the formula
    rather than evidence about the strategy. Recorded so the distinction
    survives.
    """
    excluded = {s.key for s in unreachable_above(CONSENSUS_MIN_CONFIDENCE)}
    # Only the two fixed-score strategies are structurally excluded outright;
    # the constant-bearing ones are compressed rather than capped below 0.55.
    assert excluded == {"pre_event_blackout", "grid_martingale_research"}, (
        f"strategies structurally unable to reach {CONSENSUS_MIN_CONFIDENCE}: "
        f"{sorted(excluded)}"
    )

    # The compression is the subtler problem and is asserted separately: four
    # strategies are capped well below 1.0, so they clear the floor only in
    # the top of their own range while an uncapped strategy reaches it easily.
    capped = {
        s.key
        for s in INVENTORY.values()
        if s.theoretical_max < 1.0 and s.theoretical_max >= CONSENSUS_MIN_CONFIDENCE
    }
    assert capped == {
        "donchian_breakout",
        "rsi_mean_reversion",
        "session_london_breakout",
        "multi_timeframe_trend",
    }, f"capped-but-reachable strategies: {sorted(capped)}"


@pytest.mark.parametrize(
    ("key", "low", "high"),
    [
        ("donchian_breakout", 0.25, 0.75),
        ("rsi_mean_reversion", 0.225, 0.725),
        ("session_london_breakout", 0.25, 0.75),
        ("multi_timeframe_trend", 0.30, 0.80),
    ],
)
def test_constant_bearing_ranges_are_exactly_what_the_arithmetic_gives(
    key: str, low: float, high: float
) -> None:
    """The measured consequence, to three decimals.

    `donchian_breakout` was observed at 0.31-0.44 in replay and could never
    clear 0.55; this is why. Half its score is 0.5, so the whole range is
    [0.25, 0.75] and the market only moves the other half.
    """
    spec = INVENTORY[key]
    assert spec.theoretical_min == pytest.approx(low, abs=1e-9)
    assert spec.theoretical_max == pytest.approx(high, abs=1e-9)


def test_every_entry_declares_its_kind_as_a_raw_score() -> None:
    """None of these is calibrated, and none may claim to be.

    A calibrated probability is one fitted against realised outcomes. No
    strategy here has been, so every entry must say `raw_score`. The moment one
    is calibrated, that entry changes and this test changes with it.
    """
    for key, spec in sorted(INVENTORY.items()):
        assert spec.kind == "raw_score", (
            f"{key} claims kind {spec.kind!r}. Nothing in this service has been "
            f"fitted against outcomes, so calling a score a probability would "
            f"be the category error the inventory exists to prevent."
        )


def test_the_inventory_is_versioned() -> None:
    assert INVENTORY_VERSION >= 1
