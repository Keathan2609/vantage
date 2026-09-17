"""What a strategy said, at the moment it said it.

A SignalObservation is everything knowable at the bar the signal was produced
on, and NOTHING that happened afterwards. That boundary is the point of the
type: an observation carrying a forward return would let an analysis fit
against a future the strategy could not see, and the result would look like
skill.

The boundary is structural rather than a convention. This module imports
nothing from ``outcome``, and a test walks the import graph to prove it stays
that way. If you find yourself wanting a future field here, the analysis wants
``SignalOutcome`` instead.

An observation is recorded for EVERY actionable signal, whether or not
consensus, risk, authority or the OMS would have accepted it. Recording only
executed signals would measure the policy rather than the score, and the
current policy executes almost nothing -- which is the selection bias this
milestone exists to escape.
"""

from __future__ import annotations

import hashlib
from dataclasses import dataclass, field
from datetime import datetime
from typing import Literal

Direction = Literal["buy", "sell"]
EvaluationPhase = Literal["warmup", "evaluation"]


@dataclass(frozen=True)
class SignalObservation:
    """One strategy's opinion at one bar, with its point-in-time context."""

    observation_id: str
    strategy_id: str
    strategy_version: int
    strategy_family: str
    instrument: str
    direction: Direction
    bar_time: datetime

    #: The number the strategy reported, unchanged.
    raw_score: float
    #: What that number IS. Always "raw_score" today; nothing here has been
    #: fitted against outcomes, and claiming otherwise would be the category
    #: error the score inventory exists to prevent.
    confidence_kind: str

    #: The individual values the score was built from, where the strategy
    #: exposes them. This is how an analysis can ask whether a COMPONENT
    #: carries information that the combined score hides -- a formula that
    #: averages a real component with a constant may bury a usable signal.
    score_components: dict[str, float] = field(default_factory=dict)

    #: Point-in-time market context. Every one of these is knowable at
    #: ``bar_time`` and none depends on a later bar.
    regime: str = "UNKNOWN"
    session: str = "unknown"
    spread_fraction: float = 0.0
    #: Realised volatility over the bars BEFORE this one, never after.
    trailing_volatility: float = 0.0
    event_risk: str = "none"

    #: Where the observation came from, so it can be reproduced and so a
    #: sealed partition can be excluded by dataset rather than by memory.
    dataset_id: str = ""
    dataset_hash: str = ""
    replay_run_id: str = ""
    code_sha: str = ""

    #: Warm-up observations are recorded but never analysed: indicators are
    #: still filling and the strategy's own platform suppresses trading
    #: through them, so treating them as evidence would score noise.
    evaluation_phase: EvaluationPhase = "evaluation"

    #: The entry price a hypothetical fill would have received, computed from
    #: THIS bar and the next bar's open under the platform's own execution
    #: model. Included because it is knowable without seeing the outcome: the
    #: next bar's open is the fill, and the fill is not a result.
    entry_reference: float = 0.0

    def is_analysable(self) -> bool:
        """Whether this observation may enter an exploratory report."""
        return self.evaluation_phase == "evaluation"


def observation_id(
    *, strategy_id: str, dataset_id: str, bar_time: datetime, direction: str
) -> str:
    """A stable id for one (strategy, dataset, bar, direction).

    Derived rather than random so that regenerating the dataset produces the
    same ids and an outcome computed against an earlier generation still
    joins. A random id would make every regeneration a new universe.
    """
    material = f"{strategy_id}|{dataset_id}|{bar_time.isoformat()}|{direction}"
    return hashlib.sha256(material.encode("utf-8")).hexdigest()[:32]
