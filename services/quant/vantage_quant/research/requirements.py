"""Exactly what historical data would unblock the next run.

When no legitimate local dataset exists, the honest output is not a smaller
experiment -- it is a precise statement of what is missing. A vague "we need
some gold data" produces a file that turns out to be the wrong timeframe, in
broker server time, with no header, three weeks after it was asked for.

Nothing here downloads, fetches or suggests a provider to scrape. It states
requirements; acquiring the data is a human decision with licensing attached.
"""

from __future__ import annotations

from dataclasses import dataclass, field

from vantage_quant import strategies

from . import generate, historical, verdict

#: Independent episodes wanted before an ordering claim is attempted. From the
#: same floor the classifier uses, so the requirement and the gate agree.
TARGET_EPISODES = verdict.MIN_EPISODES_FOR_ORDERING

#: Measured signal rate of the most talkative directional strategy on synthetic
#: data. Used only to translate an episode target into a bar count, and stated
#: as an estimate because a historical rate is exactly what is unknown.
SYNTHETIC_REFERENCE_EPISODE_RATE = 750 / 8632


@dataclass(frozen=True)
class DataRequirement:
    instrument: str
    timeframe: str
    minimum_bars: int
    minimum_span_months: int
    required_columns: tuple[str, ...]
    optional_columns: tuple[str, ...]
    accepted_file_types: tuple[str, ...]
    timezone_requirement: str
    notes: tuple[str, ...] = field(default_factory=tuple)

    def as_lines(self) -> list[str]:
        return [
            f"instrument:          {self.instrument}",
            f"timeframe:           {self.timeframe}",
            f"minimum bars:        {self.minimum_bars:,}",
            f"minimum span:        {self.minimum_span_months} months",
            f"required columns:    {', '.join(self.required_columns)}",
            f"optional columns:    {', '.join(self.optional_columns)}",
            f"accepted file types: {', '.join(self.accepted_file_types)}",
            f"timezone:            {self.timezone_requirement}",
            *(f"note:                {n}" for n in self.notes),
        ]


def _minimum_bars() -> int:
    """Bars needed for the longest warm-up plus a usable episode count.

    Derived rather than picked: the floor is whichever registered strategy
    declares the most history, and the rest is the episode target divided by a
    measured rate. Stated as an estimate because the historical rate is the
    unknown this data would settle.
    """
    warmup = max(
        max(s.required_bars for s in strategies.REGISTRY.values()),
        generate.WARMUP_BARS,
    )
    for_episodes = int(TARGET_EPISODES / SYNTHETIC_REFERENCE_EPISODE_RATE)
    return warmup + for_episodes


def requirement(
    *, instrument: str = "XAUUSD", timeframe: str = "1h"
) -> DataRequirement:
    minimum = _minimum_bars()
    # ~120 tradable hours a week on a 24x5 venue.
    months = max(6, round(minimum / (120 * 4.33)))
    return DataRequirement(
        instrument=instrument,
        timeframe=timeframe,
        minimum_bars=minimum,
        minimum_span_months=months,
        required_columns=("timestamp", *historical.REQUIRED_COLUMNS),
        optional_columns=historical.OPTIONAL_COLUMNS,
        accepted_file_types=historical.ALLOWED_SUFFIXES,
        timezone_requirement=(
            "UTC preferred. If the export is in broker server time or any local "
            "zone, state which -- it is passed explicitly and recorded, never "
            "inferred from this machine"
        ),
        notes=(
            "generic spot Gold prices, NOT broker XAUUSD.m contract semantics; "
            "price-series research stays separate from contract specification",
            "bid/ask are optional and valuable: without them costs are an "
            "assumption and every result is labelled ESTIMATED_COSTS",
            "one high-quality series beats several questionable ones",
            "a broad chronological span, not periods chosen for known "
            "performance -- letting the market supply the regimes is what keeps "
            "the sample defensible",
            "licensing must permit this research use, and the note is recorded "
            "with the dataset",
        ),
    )


def waiting_report(*, instrument: str = "XAUUSD", timeframe: str = "1h") -> str:
    """The WAITING_FOR_HISTORICAL_DATA status, with its specification."""
    lines = [
        "WAITING_FOR_HISTORICAL_DATA",
        "",
        f"No dataset is present in {historical.data_directory()}.",
        "Nothing was downloaded; acquiring data is a human decision with "
        "licensing attached.",
        "",
        "Place a file there and re-run. Required:",
        "",
    ]
    lines.extend("  " + line for line in requirement(
        instrument=instrument, timeframe=timeframe
    ).as_lines())
    return "\n".join(lines)
