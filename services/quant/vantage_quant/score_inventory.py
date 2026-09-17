"""What every strategy's ``confidence`` value actually is.

A strategy returns a number between 0 and 1 called ``confidence``, and the
control plane's consensus policy compares it to thresholds that read as
confidence levels: discard below 0.55, require 0.60 net. Those comparisons are
only meaningful if the number means the same thing for every strategy.

It does not. ``_confidence`` averages hand-chosen components, and several
strategies average a REAL component with a hard-coded constant. Averaging with
a constant compresses the range toward that constant and caps it: a strategy
whose score is ``(penetration + 0.5) / 2`` can never report below 0.25 or above
0.75, whatever the market does, and its typical reading sits near 0.5 because
half the value is not about the market at all.

This module is the inventory. It exists as CODE rather than as a table in a
document because a table rots: ``test_score_inventory.py`` fails the moment a
strategy is registered without an entry here, so a new scoring formula cannot
enter the platform without someone writing down what it means.

Nothing here changes a strategy's behaviour. Recording what a number is must
come before deciding what to do about it.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Literal

ScoreKind = Literal["raw_score", "calibrated_probability"]

#: Version of this inventory's semantics. Bumped when an entry's MEANING
#: changes, so a calibration artifact can record which reading it was fitted
#: against.
INVENTORY_VERSION = 1


@dataclass(frozen=True)
class ScoreSpec:
    """What one strategy's confidence value is, and what it is not."""

    #: Registry key.
    key: str
    family: str
    #: The formula as it appears in strategies.py, transcribed rather than
    #: paraphrased so it can be diffed against the source.
    formula: str
    #: Each named input that varies with the market.
    components: tuple[str, ...]
    #: Each hard-coded number averaged in alongside them. This is the field
    #: the inventory exists for.
    constants: tuple[float, ...]
    #: The range the formula can produce, given every component is bounded to
    #: [0, 1]. A strategy whose theoretical maximum sits below a threshold can
    #: never clear it, and that is a property of the arithmetic rather than of
    #: the market.
    theoretical_min: float
    theoretical_max: float
    #: What the author intended the number to express.
    intended_meaning: str
    kind: ScoreKind = "raw_score"
    #: Set when the formula has a property that makes cross-strategy
    #: comparison unsound. Empty when it does not.
    comparability_notes: tuple[str, ...] = field(default_factory=tuple)

    @property
    def has_constant_component(self) -> bool:
        return bool(self.constants)

    def can_reach(self, threshold: float) -> bool:
        """Whether the arithmetic permits this score to reach a threshold."""
        return self.theoretical_max >= threshold

    def floor_above(self, threshold: float) -> bool:
        """Whether the arithmetic prevents this score falling to a threshold."""
        return self.theoretical_min > threshold


def _mean_range(parts: list[tuple[float, float]]) -> tuple[float, float]:
    """Range of the mean of components with the given individual ranges."""
    lo = sum(p[0] for p in parts) / len(parts)
    hi = sum(p[1] for p in parts) / len(parts)
    return lo, hi


_VARIABLE = (0.0, 1.0)


def _const(value: float) -> tuple[float, float]:
    return (value, value)


#: Every registered strategy, keyed by registry key.
#:
#: Ordered as they appear in strategies.py so the two can be read side by side.
INVENTORY: dict[str, ScoreSpec] = {}


def _add(spec: ScoreSpec) -> ScoreSpec:
    INVENTORY[spec.key] = spec
    return spec


_add(
    ScoreSpec(
        key="ma_trend_crossover",
        family="trend_following",
        formula="mean(min(1, separation / 1.5), min(1, atr_fraction / (min_atr * 4)))",
        components=("separation", "atr_fraction"),
        constants=(),
        theoretical_min=_mean_range([_VARIABLE, _VARIABLE])[0],
        theoretical_max=_mean_range([_VARIABLE, _VARIABLE])[1],
        intended_meaning=(
            "How far apart the moving averages are, tempered by whether "
            "volatility is high enough for the move to pay for its costs."
        ),
    )
)

_add(
    ScoreSpec(
        key="donchian_breakout",
        family="breakout",
        formula="mean(min(1, penetration / 1.0), 0.5)",
        components=("penetration",),
        constants=(0.5,),
        theoretical_min=_mean_range([_VARIABLE, _const(0.5)])[0],
        theoretical_max=_mean_range([_VARIABLE, _const(0.5)])[1],
        intended_meaning="How far price has broken beyond the channel edge.",
        comparability_notes=(
            "Half the value is the constant 0.5, so the score can never leave "
            "[0.25, 0.75] and sits near 0.5 when penetration is unremarkable. "
            "A 0.55 threshold therefore requires penetration above 0.6, which "
            "is a statement about the arithmetic and not about the breakout.",
        ),
    )
)

_add(
    ScoreSpec(
        key="rsi_mean_reversion",
        family="mean_reversion",
        formula="mean(min(1, stretch * 2.0), 0.45)",
        components=("stretch",),
        constants=(0.45,),
        theoretical_min=_mean_range([_VARIABLE, _const(0.45)])[0],
        theoretical_max=_mean_range([_VARIABLE, _const(0.45)])[1],
        intended_meaning="How far past the oversold/overbought line price has stretched.",
        comparability_notes=(
            "Capped at 0.725 by the constant, so it clears a 0.55 floor only "
            "in the upper third of its own range while a two-component "
            "strategy reaches 1.0. The two are not on one scale.",
        ),
    )
)

_add(
    ScoreSpec(
        key="macd_momentum",
        family="momentum",
        formula="mean(min(1, strength * 3.0), min(1, acceleration))",
        components=("strength", "acceleration"),
        constants=(),
        theoretical_min=_mean_range([_VARIABLE, _VARIABLE])[0],
        theoretical_max=_mean_range([_VARIABLE, _VARIABLE])[1],
        intended_meaning="Histogram expansion, scaled by how fast it is expanding.",
    )
)

_add(
    ScoreSpec(
        key="bollinger_zscore_reversion",
        family="statistical",
        formula="mean(min(1, (abs(z) - entry_z) / 2.0 + 0.3))",
        components=("z_excess",),
        constants=(0.3,),
        theoretical_min=0.3,
        theoretical_max=1.0,
        intended_meaning="How far beyond the entry z-score the deviation has gone.",
        comparability_notes=(
            "The 0.3 is an OFFSET inside a single component rather than a "
            "second averaged term, so it raises the floor without capping the "
            "ceiling: a signal that barely qualifies still reports 0.3.",
        ),
    )
)

_add(
    ScoreSpec(
        key="atr_volatility_regime",
        family="volatility",
        formula="mean(min(1, (ratio - expansion) + 0.3), abs(position - 0.5) * 2)",
        components=("expansion_excess", "range_position"),
        constants=(0.3,),
        theoretical_min=_mean_range([(0.3, 1.0), _VARIABLE])[0],
        theoretical_max=_mean_range([(0.3, 1.0), _VARIABLE])[1],
        intended_meaning="Volatility expansion, weighted by position within the range.",
        comparability_notes=(
            "The 0.3 offset applies to one of two averaged components, so it "
            "lifts the floor to 0.15 rather than to 0.3.",
        ),
    )
)

_add(
    ScoreSpec(
        key="session_london_breakout",
        family="session",
        formula="mean(min(1, penetration), 0.5)",
        components=("penetration",),
        constants=(0.5,),
        theoretical_min=_mean_range([_VARIABLE, _const(0.5)])[0],
        theoretical_max=_mean_range([_VARIABLE, _const(0.5)])[1],
        intended_meaning="How far price broke the Asian range as London opened.",
        comparability_notes=(
            "Same shape as donchian_breakout: bounded to [0.25, 0.75] by the "
            "constant half.",
        ),
    )
)

_add(
    ScoreSpec(
        key="multi_timeframe_trend",
        family="multi_timeframe",
        formula="mean(0.6, min(1, abs(fast - slow) / atr))",
        components=("ma_separation_in_atr",),
        constants=(0.6,),
        theoretical_min=_mean_range([_const(0.6), _VARIABLE])[0],
        theoretical_max=_mean_range([_const(0.6), _VARIABLE])[1],
        intended_meaning="Agreement across timeframes, scaled by separation.",
        comparability_notes=(
            "The constant is 0.6 and comes FIRST, so the score starts at 0.30 "
            "and reaches 0.80. It is the only strategy whose floor sits above "
            "a quarter, which makes its weakest signal look stronger than "
            "another strategy's moderate one.",
        ),
    )
)

_add(
    ScoreSpec(
        key="pre_event_blackout",
        family="event_driven",
        formula="0.0 (constant)",
        components=(),
        constants=(0.0,),
        theoretical_min=0.0,
        theoretical_max=0.0,
        intended_meaning=(
            "This strategy only ever declines to trade, so its score is not a "
            "confidence in a direction and is fixed at zero."
        ),
        comparability_notes=(
            "Never actionable, so the score is never compared to a threshold. "
            "Listed for completeness rather than as a scoring formula.",
        ),
    )
)

_add(
    ScoreSpec(
        key="ensemble_weighted_vote",
        family="ensemble",
        formula="mean(max(buy_share, sell_share))",
        components=("winning_vote_share",),
        constants=(),
        theoretical_min=0.0,
        theoretical_max=1.0,
        intended_meaning="The winning side's share of member votes.",
        comparability_notes=(
            "A share of votes, not a strength of conviction. Two members "
            "agreeing weakly produce the same 1.0 as two agreeing strongly.",
        ),
    )
)

_add(
    ScoreSpec(
        key="ml_direction_filter",
        family="machine_learning",
        formula="mean(base.confidence, min(1, (directional - 0.5) * 2))",
        components=("base_strategy_score", "model_directional_margin"),
        constants=(),
        theoretical_min=0.0,
        theoretical_max=1.0,
        intended_meaning=(
            "A base strategy's score tempered by how far the model's "
            "directional output sits from even odds."
        ),
        comparability_notes=(
            "COMPOSED from another strategy's raw score, so it inherits that "
            "strategy's scale distortion as well as its own. Calibrating this "
            "without first calibrating its base would fit a moving target.",
        ),
    )
)

_add(
    ScoreSpec(
        key="grid_martingale_research",
        family="high_risk_research",
        formula="0.2 (constant)",
        components=(),
        constants=(0.2,),
        theoretical_min=0.2,
        theoretical_max=0.2,
        intended_meaning=(
            "A fixed low score marking the approach as research-only; it is "
            "not a measurement of anything."
        ),
        comparability_notes=(
            "Constant, so it carries no information about the market at all. "
            "The strategy is disabled and high-risk.",
        ),
    )
)


def strategies_with_constants() -> list[ScoreSpec]:
    """Every strategy whose score averages in a hard-coded number."""
    return [s for s in INVENTORY.values() if s.has_constant_component]


def unreachable_above(threshold: float) -> list[ScoreSpec]:
    """Strategies whose arithmetic cannot produce a score at the threshold.

    These can never contribute to a consensus that discards below it, whatever
    the market does -- which is a property of the formula, not evidence about
    the strategy.
    """
    return [s for s in INVENTORY.values() if not s.can_reach(threshold)]
