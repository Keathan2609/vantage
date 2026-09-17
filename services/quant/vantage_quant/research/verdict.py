"""What the evidence says, in categories that mean one thing each.

The previous milestone shipped a verdict called MORE_DATA_REQUIRED and then
had to explain, in prose, that it meant two incompatible things:

  A. too little evidence to say anything, and
  B. enough evidence to say there is no usable relationship.

Those call for opposite actions. A says collect more bars; B says stop
collecting and redesign the score. Reading the label alone, a reader cannot
tell which they are being told, and a reader who acts on the wrong one wastes
a milestone. `donchian_breakout` carried that label with 409 independent
episodes behind it, and so did `atr_volatility_regime` with 20.

Fixing this BEFORE historical evidence arrives is deliberate. Importing real
market results into an ambiguous category would bake the ambiguity into the
comparison that matters most.

# Versioning

The analysis methodology is versioned as a whole, because a verdict is only
comparable with another verdict produced the same way. `ANALYSIS_VERSION` and
the component versions travel with every run.

# Prior runs are not rewritten

Milestone E's records keep their original labels. `map_legacy` translates them
for display, and where the old label is genuinely ambiguous it says so rather
than guessing -- an invented precision in a migration is still an invented
precision.
"""

from __future__ import annotations

from dataclasses import dataclass
from enum import StrEnum

#: The methodology as a whole. Bump when any component version below changes,
#: because a verdict is only comparable with one produced the same way.
ANALYSIS_VERSION = "signal-research-analysis/v1"


@dataclass(frozen=True)
class AnalysisVersions:
    """Every versioned input to a verdict, recorded with the verdict.

    Carried rather than assumed: a historical result and a synthetic result
    compared across different horizon registries or cost models would differ
    for reasons that have nothing to do with the market.
    """

    analysis: str = ANALYSIS_VERSION
    horizon_registry: int = 1
    cost_model: int = 1
    clustering_rule: int = 1
    bootstrap: int = 1
    classification: int = 2  # this module; v1 was the overloaded taxonomy
    generator: int = 1
    partition: int = 1
    normalization: int = 1


class CalibrationVerdict(StrEnum):
    """Why a strategy may or may not have a calibration fitted to its score.

    Each value names ONE cause. Where two causes could apply, the order of
    checks in `classify` decides, and that order is documented there.
    """

    #: Score varies, evidence is adequate and independent, ordering is real,
    #: stable and survives costs. Nothing has been fitted; this is permission
    #: to try.
    READY_FOR_CALIBRATION = "READY_FOR_CALIBRATION"

    #: Too few observations, or none at all. Collecting more bars is the
    #: action.
    INSUFFICIENT_DATA = "INSUFFICIENT_DATA"

    #: Plenty of signals, too few INDEPENDENT ones. More bars help only if
    #: they contain new episodes rather than longer ones.
    INSUFFICIENT_INDEPENDENT_EPISODES = "INSUFFICIENT_INDEPENDENT_EPISODES"

    #: The score barely moves, so it cannot rank anything. No quantity of data
    #: fixes a constant.
    NON_INFORMATIVE_SCORE = "NON_INFORMATIVE_SCORE"

    #: Measured with adequate power; outcome does not move consistently with
    #: score. A monotone calibration would be fitted to an ordering that is
    #: not there.
    NON_MONOTONIC_SCORE = "NON_MONOTONIC_SCORE"

    #: Measured, and the relationship cannot be resolved from zero -- the
    #: interval spans it, or the score does not spread across enough quantile
    #: bins to judge shape. Distinct from INSUFFICIENT_DATA: the sample is
    #: adequate and the answer is still "cannot tell", which usually means the
    #: score's distribution is the problem rather than its quantity.
    ORDERING_NOT_ESTABLISHED = "ORDERING_NOT_ESTABLISHED"

    #: Higher score, worse outcome, consistently. Informative -- but the score
    #: means the opposite of what the platform renders it as.
    NEGATIVE_ORDERING = "NEGATIVE_ORDERING"

    #: The ordering exists and the mean net outcome still does not survive
    #: costs.
    COST_NEGATIVE = "COST_NEGATIVE"

    #: Not evaluated as directional alpha by role (high-risk research,
    #: ensembles pending their own semantics).
    RESEARCH_ONLY = "RESEARCH_ONLY"

    #: The question does not apply. A veto emits no directional opinion, so
    #: "does its score predict return" has no answer to be short of.
    NOT_APPLICABLE = "NOT_APPLICABLE"


#: Verdicts meaning "the sample is too small". Separated because the action is
#: different from every other verdict: these are the only ones where waiting
#: for more data is the right response.
NEEDS_MORE_DATA = frozenset(
    {
        CalibrationVerdict.INSUFFICIENT_DATA,
        CalibrationVerdict.INSUFFICIENT_INDEPENDENT_EPISODES,
    }
)

#: Verdicts meaning "measured, and the score is the problem". Waiting for more
#: data here spends a milestone to re-learn what is already known.
SCORE_IS_THE_PROBLEM = frozenset(
    {
        CalibrationVerdict.NON_INFORMATIVE_SCORE,
        CalibrationVerdict.NON_MONOTONIC_SCORE,
        CalibrationVerdict.ORDERING_NOT_ESTABLISHED,
        CalibrationVerdict.NEGATIVE_ORDERING,
    }
)


class RedesignFlag(StrEnum):
    """Whether the signal rule or the score formula is the thing to revisit.

    A bad score is not a bad trading hypothesis. The synthetic run found
    `donchian_breakout` producing signals whose net outcome interval excluded
    zero while its score failed to rank those same signals -- the entry rule
    was doing work the confidence number was not. Collapsing the two into one
    verdict would have hidden that.

    Nothing is redesigned on the strength of this flag; it marks where to look.
    """

    NONE = "NONE"

    #: Signals look useful, the score does not rank them. Revisit the
    #: confidence formula, not the entry rule.
    SCORE_REDESIGN_CANDIDATE = "SCORE_REDESIGN_CANDIDATE"

    #: Signal outcome AND score ordering are both weak. The trading hypothesis
    #: itself is the open question.
    SIGNAL_RESEARCH_REQUIRED = "SIGNAL_RESEARCH_REQUIRED"


class HistoricalSufficiency(StrEnum):
    """Whether REAL evidence exists in enough quantity to judge at all.

    Asked before calibration readiness, and answered separately, because
    "we have no real data" and "we have real data showing nothing" are
    different states that a single readiness verdict would merge.
    """

    HISTORICAL_EVIDENCE_SUFFICIENT = "HISTORICAL_EVIDENCE_SUFFICIENT"
    MORE_HISTORICAL_DATA_REQUIRED = "MORE_HISTORICAL_DATA_REQUIRED"
    NO_HISTORICAL_EVIDENCE = "NO_HISTORICAL_EVIDENCE"
    NOT_APPLICABLE = "NOT_APPLICABLE"


class Disagreement(StrEnum):
    """How synthetic and historical evidence differ for one strategy.

    Classified rather than averaged. Averaging a synthetic result with a
    historical one produces a number describing no market that exists, and
    hides the single most informative outcome this milestone can produce --
    that a relationship was an artefact of the generator.
    """

    AGREES = "AGREES"

    #: Present in synthetic, absent in real data. The most likely finding, and
    #: the one the generator was always at risk of manufacturing.
    SYNTHETIC_ONLY_RELATIONSHIP = "SYNTHETIC_ONLY_RELATIONSHIP"

    #: Absent in synthetic, present in real data. The generator failed to
    #: produce the condition the strategy exploits.
    HISTORICAL_ONLY_RELATIONSHIP = "HISTORICAL_ONLY_RELATIONSHIP"

    #: Ordering runs the opposite way in the two sources.
    DIRECTION_REVERSAL = "DIRECTION_REVERSAL"

    #: The score occupies a materially different part of its range. A
    #: calibration fitted on one would be extrapolating on the other.
    SCORE_RANGE_MISMATCH = "SCORE_RANGE_MISMATCH"

    #: Net outcome survives costs in one source and not the other.
    COST_SURVIVAL_MISMATCH = "COST_SURVIVAL_MISMATCH"

    #: One side produced too little to compare. Not a disagreement; the
    #: absence of a comparison, said plainly.
    NOT_COMPARABLE = "NOT_COMPARABLE"


# --- migration --------------------------------------------------------------

#: Legacy verdicts that map to exactly one current verdict.
_UNAMBIGUOUS_LEGACY: dict[str, CalibrationVerdict] = {
    "READY_FOR_CALIBRATION": CalibrationVerdict.READY_FOR_CALIBRATION,
    "NON_INFORMATIVE_SCORE": CalibrationVerdict.NON_INFORMATIVE_SCORE,
    "NON_MONOTONIC": CalibrationVerdict.NON_MONOTONIC_SCORE,
    "COST_NEGATIVE": CalibrationVerdict.COST_NEGATIVE,
    "RESEARCH_ONLY": CalibrationVerdict.RESEARCH_ONLY,
    "NOT_APPLICABLE": CalibrationVerdict.NOT_APPLICABLE,
}


def map_legacy(
    legacy: str, *, effective_n: int | None = None, raw_n: int | None = None
) -> tuple[CalibrationVerdict | None, str]:
    """Translate a Milestone E verdict for display beside current ones.

    Prior run records are NOT rewritten -- they are the record of what was
    concluded at the time, and editing them would destroy the only
    before-picture there is. This translates on the way out.

    `MORE_DATA_REQUIRED` is the ambiguous one. With the counts from the same
    record it can usually be resolved; without them this returns None and a
    reason, because a migration that guesses is indistinguishable from a
    migration that knows.
    """
    settled = _UNAMBIGUOUS_LEGACY.get(legacy)
    if settled is not None:
        return settled, ""

    if legacy != "MORE_DATA_REQUIRED":
        return None, f"unrecognised legacy verdict {legacy!r}"

    if effective_n is None:
        return None, (
            "legacy MORE_DATA_REQUIRED is ambiguous between INSUFFICIENT_DATA "
            "and a measured absence of ordering; effective_n is needed to tell "
            "them apart"
        )

    if raw_n is not None and raw_n <= 0:
        return CalibrationVerdict.INSUFFICIENT_DATA, "never signalled"

    if effective_n < MIN_EPISODES_FOR_ORDERING:
        if raw_n is not None and raw_n >= MIN_RAW_FOR_EPISODE_COMPLAINT:
            return (
                CalibrationVerdict.INSUFFICIENT_INDEPENDENT_EPISODES,
                f"{raw_n} signals but only {effective_n} independent episodes",
            )
        return (
            CalibrationVerdict.INSUFFICIENT_DATA,
            f"{effective_n} independent episodes",
        )

    # Adequate independent evidence and still MORE_DATA_REQUIRED means the old
    # taxonomy was saying "measured, could not resolve an ordering".
    return (
        CalibrationVerdict.ORDERING_NOT_ESTABLISHED,
        f"{effective_n} independent episodes were available, so the legacy "
        f"label was not about quantity",
    )


#: Independent episodes below which no ordering claim is attempted.
MIN_EPISODES_FOR_ORDERING = 300

#: Raw signals above which a low episode count is the interesting fact, rather
#: than simply "not much happened".
MIN_RAW_FOR_EPISODE_COMPLAINT = 300
