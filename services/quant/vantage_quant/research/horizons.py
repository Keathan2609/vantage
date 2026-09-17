"""How far ahead each strategy's opinion is supposed to be about.

A single horizon applied to every strategy would measure the wrong thing. An
RSI mean-reversion signal says "this has stretched too far and should snap
back", which is a claim about the next few bars; a multi-timeframe trend signal
says "the larger structure is aligned", which is a claim about the next
several dozen. Scoring both at ten bars would flatter one and bury the other,
and the flattening would look like evidence.

So the horizons are per family and derived from what each strategy's own logic
claims, not from what produces a pleasing number. Several horizons are tested
per family and ALL of them are reported: picking the horizon at which a
strategy looks best, after seeing the results, is the selection bias this whole
milestone exists to avoid.

Versioned, because a later analysis fitted against different horizons is not
comparable with this one and must be able to say so.
"""

from __future__ import annotations

from dataclasses import dataclass

#: Bumped when any horizon's VALUE changes. A result records this, so two
#: analyses can be told apart rather than silently averaged.
HORIZON_CONFIG_VERSION = 1


@dataclass(frozen=True)
class Horizon:
    """One forward window, in bars of the strategy's own timeframe."""

    horizon_id: str
    bars: int
    rationale: str


@dataclass(frozen=True)
class FamilyHorizons:
    family: str
    horizons: tuple[Horizon, ...]
    #: Why this family gets these windows and not others.
    basis: str


def _h(name: str, bars: int, why: str) -> Horizon:
    return Horizon(horizon_id=name, bars=bars, rationale=why)


#: Every registered family, keyed by family name.
#:
#: The bar counts are in the strategy's own timeframe. Every PAPER strategy
#: here runs on 1h bars, so "10 bars" is ten hours; a strategy promoted on a
#: different timeframe keeps the same shape of claim and a different wall
#: duration, which is the correct reading of "continuation over ten bars".
FAMILY_HORIZONS: dict[str, FamilyHorizons] = {
    "trend_following": FamilyHorizons(
        family="trend_following",
        basis=(
            "A moving-average crossover claims the trend persists. Its own ATR "
            "stop sits 2 ATR away, which on this fixture's volatility is "
            "several bars of travel, so the claim is not resolved inside three "
            "bars and is stale well beyond twenty."
        ),
        horizons=(
            _h("short", 5, "enough for the crossover to be confirmed or rejected"),
            _h("medium", 10, "the window the 2-ATR stop implies"),
            _h("long", 20, "where a trend claim has either paid or failed"),
        ),
    ),
    "momentum": FamilyHorizons(
        family="momentum",
        basis=(
            "MACD histogram expansion is a statement about acceleration RIGHT "
            "NOW. Acceleration decays, so the claim is shortest-lived of all "
            "the directional families."
        ),
        horizons=(
            _h("immediate", 3, "while the acceleration that produced it still holds"),
            _h("short", 5, "the usual life of a histogram expansion"),
            _h("medium", 10, "beyond which the signal is describing a different market"),
        ),
    ),
    "mean_reversion": FamilyHorizons(
        family="mean_reversion",
        basis=(
            "An RSI stretch claims price snaps back toward the mean. A "
            "reversion that has not begun within a handful of bars is usually "
            "a trend, which is the failure mode this family has."
        ),
        horizons=(
            _h("immediate", 3, "a snap-back should start almost at once"),
            _h("short", 5, "the reversion window proper"),
            _h("medium", 10, "past which a non-reversion is a trend, not a slow one"),
        ),
    ),
    "breakout": FamilyHorizons(
        family="breakout",
        basis=(
            "A channel break claims continuation beyond the edge. The "
            "strategy's own target is 2.5x its stop, which needs room to run, "
            "so the shortest useful window is longer than a reversion's."
        ),
        horizons=(
            _h("short", 5, "enough to distinguish a break from a wick"),
            _h("medium", 10, "the window the 2.5x target implies"),
            _h("long", 20, "where a false breakout has fully retraced"),
        ),
    ),
    "statistical": FamilyHorizons(
        family="statistical",
        basis=(
            "A z-score deviation claims reversion toward the rolling mean, on "
            "the same timescale as the mean-reversion family and for the same "
            "reason."
        ),
        horizons=(
            _h("immediate", 3, "the deviation should begin closing at once"),
            _h("short", 5, "the reversion window"),
            _h("medium", 10, "past which the mean itself has moved"),
        ),
    ),
    "volatility": FamilyHorizons(
        family="volatility",
        basis=(
            "An expansion claim is about the regime rather than a single move, "
            "so it resolves more slowly than a directional signal."
        ),
        horizons=(
            _h("short", 5, "while the expansion is still measurable"),
            _h("medium", 10, "the usual life of an expansion phase"),
            _h("long", 20, "where the regime has either persisted or reverted"),
        ),
    ),
    "session": FamilyHorizons(
        family="session",
        basis=(
            "A London-open break is explicitly bounded by its session. A "
            "horizon that ran past the session close would score the strategy "
            "on hours it never claimed anything about."
        ),
        horizons=(
            _h("early_session", 4, "the first four hours of the London session"),
            _h("session", 8, "the session the strategy is defined over"),
        ),
    ),
    "multi_timeframe": FamilyHorizons(
        family="multi_timeframe",
        basis=(
            "Alignment across timeframes is the slowest claim any of these "
            "strategies makes; judging it over five bars would measure noise "
            "on the fastest leg."
        ),
        horizons=(
            _h("medium", 10, "the shortest window in which alignment can pay"),
            _h("long", 20, "the claim's natural span"),
            _h("extended", 40, "where a higher-timeframe move has completed"),
        ),
    ),
    "event_driven": FamilyHorizons(
        family="event_driven",
        basis=(
            "A pre-event strategy only ever declines to trade, so it produces "
            "no directional claim to score. The horizons exist so that the "
            "family is represented rather than silently absent."
        ),
        horizons=(
            _h("post_event", 5, "the window after a release resolves"),
            _h("post_event_long", 10, "where the repricing has settled"),
        ),
    ),
    "ensemble": FamilyHorizons(
        family="ensemble",
        basis=(
            "An ensemble inherits its members' timescales, which span three to "
            "twenty bars, so it is measured across that whole range rather "
            "than at one point inside it."
        ),
        horizons=(
            _h("short", 5, "the fastest member's window"),
            _h("medium", 10, "the middle of the members' range"),
            _h("long", 20, "the slowest member's window"),
        ),
    ),
    "machine_learning": FamilyHorizons(
        family="machine_learning",
        basis=(
            "The direction filter composes a base strategy's score with a "
            "model margin, so it is scored over the same range as the base "
            "families it wraps."
        ),
        horizons=(
            _h("short", 5, "the base strategy's short window"),
            _h("medium", 10, "the base strategy's medium window"),
            _h("long", 20, "the base strategy's long window"),
        ),
    ),
    "high_risk_research": FamilyHorizons(
        family="high_risk_research",
        basis=(
            "Disabled and research-only, with a constant score that carries no "
            "information. Horizons are defined so the family cannot vanish "
            "from a coverage check."
        ),
        horizons=(
            _h("short", 5, "nominal"),
            _h("medium", 10, "nominal"),
        ),
    ),
}


def horizons_for(family: str) -> tuple[Horizon, ...]:
    """Every horizon a family is scored over.

    Raises rather than defaulting: a family with no declared horizon has not
    been thought about, and silently scoring it at some arbitrary window is
    how an unexamined number enters a report.
    """
    if family not in FAMILY_HORIZONS:
        raise KeyError(
            f"no research horizon is defined for family {family!r}. Declare one "
            f"in horizons.py with its rationale before scoring the family."
        )
    return FAMILY_HORIZONS[family].horizons


def max_horizon_bars() -> int:
    """The longest window any family is scored over."""
    return max(h.bars for fam in FAMILY_HORIZONS.values() for h in fam.horizons)
