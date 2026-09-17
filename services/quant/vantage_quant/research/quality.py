"""Is this file fit to draw a conclusion from?

Real market files are dirty in ways synthetic bars never are: a provider drops
a week, a daylight-saving change shifts an hour, a broken export repeats the
same close for six hundred rows, a timestamp column arrives in local time with
no marker. Every one of those produces research output that looks exactly like
research output.

So a historical dataset is examined before it is allowed near a strategy, and
the examination produces a record rather than a boolean. A file with a gap in
August is usable for a question about January; a file whose OHLC is impossible
is usable for nothing. Only the second is refused outright -- a validator that
rejected everything imperfect would reject every real market file in existence,
and one that reported only PASS/FAIL would hide the August gap from the person
drawing the conclusion.

Nothing here repairs anything. A validator that silently fixes its input
destroys the evidence that the input was broken.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from datetime import timedelta
from enum import StrEnum

import numpy as np
import pandas as pd

#: Checker version. A dataset records the version that cleared it, because a
#: later, stricter checker must not be assumed to have seen an older file.
QUALITY_CHECKER_VERSION = 1

#: A bar whose range exceeds this multiple of the recent median is flagged for
#: a human to look at. Not rejected: real markets gap on news, and a validator
#: that refuses genuine volatility refuses exactly the periods worth studying.
ABNORMAL_RANGE_MULTIPLE = 20.0

#: Consecutive identical closes above which the series is treated as suspected
#: flatlining. A real hourly gold series does not print the same close 12 times
#: running; a broken export does.
FLATLINE_RUN = 12


class Severity(StrEnum):
    """How much a finding matters.

    ERROR means the data cannot support research. WARNING means it can, with
    the reader told what they are working with -- which is the common and
    correct outcome for real market data.
    """

    ERROR = "ERROR"
    WARNING = "WARNING"
    INFO = "INFO"


class DatasetStatus(StrEnum):
    """Where a dataset is in its journey from file to evidence."""

    IMPORTED = "IMPORTED"
    VALIDATING = "VALIDATING"
    VALID = "VALID"
    VALID_WITH_WARNINGS = "VALID_WITH_WARNINGS"
    REJECTED = "REJECTED"


@dataclass(frozen=True)
class Finding:
    code: str
    severity: Severity
    count: int
    detail: str


@dataclass
class DataQualityReport:
    """Everything known about a dataset's defects, and none of them fixed."""

    dataset_id: str
    checker_version: int
    rows: int
    first_bar: str
    last_bar: str
    findings: list[Finding] = field(default_factory=list)

    #: Coverage, where a market clock can distinguish a closed market from a
    #: provider that simply lost the data.
    expected_bars: int | None = None
    actual_bars: int = 0
    missing_bars: int | None = None
    coverage_fraction: float | None = None
    closed_market_bars: int | None = None

    @property
    def errors(self) -> list[Finding]:
        return [f for f in self.findings if f.severity is Severity.ERROR]

    @property
    def warnings(self) -> list[Finding]:
        return [f for f in self.findings if f.severity is Severity.WARNING]

    @property
    def status(self) -> DatasetStatus:
        if self.errors:
            return DatasetStatus.REJECTED
        if self.warnings:
            return DatasetStatus.VALID_WITH_WARNINGS
        return DatasetStatus.VALID

    def summary(self) -> str:
        return (
            f"{self.dataset_id}: {self.status.value}, {self.rows} bars, "
            f"{len(self.errors)} error(s), {len(self.warnings)} warning(s)"
        )


def _add(
    findings: list[Finding], code: str, severity: Severity, count: int, detail: str
) -> None:
    if count > 0:
        findings.append(Finding(code=code, severity=severity, count=count, detail=detail))


def assess(
    frame: pd.DataFrame,
    *,
    dataset_id: str,
    timeframe: timedelta,
    timezone_was_declared: bool,
    has_real_spread: bool,
    market_clock: MarketClock | None = None,
) -> DataQualityReport:
    """Examine a normalised frame and describe every defect found.

    The frame is expected to be UTC-indexed and column-normalised already;
    this asks whether its CONTENTS can support a conclusion, not whether it
    parsed.
    """
    findings: list[Finding] = []
    rows = len(frame)

    report = DataQualityReport(
        dataset_id=dataset_id,
        checker_version=QUALITY_CHECKER_VERSION,
        rows=rows,
        first_bar=frame.index[0].isoformat() if rows else "",
        last_bar=frame.index[-1].isoformat() if rows else "",
        actual_bars=rows,
    )
    if rows == 0:
        findings.append(
            Finding("EMPTY", Severity.ERROR, 1, "the dataset contains no bars")
        )
        report.findings = findings
        return report

    # --- structural: these make the data uninterpretable -------------------

    _add(
        findings, "DUPLICATE_TIMESTAMP", Severity.ERROR,
        int(frame.index.duplicated().sum()),
        "the same instant appears more than once, so a bar is ambiguous",
    )
    _add(
        findings, "OUT_OF_ORDER", Severity.ERROR,
        0 if frame.index.is_monotonic_increasing else 1,
        "rows are not in chronological order; a forward-looking outcome "
        "computed over them would read the wrong bars",
    )

    ohlc = frame[["open", "high", "low", "close"]]
    _add(
        findings, "NON_FINITE_PRICE", Severity.ERROR,
        int((~np.isfinite(ohlc.to_numpy(dtype=float))).sum()),
        "NaN or infinity in a price column",
    )
    _add(
        findings, "NON_POSITIVE_PRICE", Severity.ERROR,
        int((ohlc <= 0).to_numpy().sum()),
        "a zero or negative price, which no instrument quotes",
    )

    impossible = (
        (frame["high"] < frame["low"])
        | (frame["high"] < frame["open"])
        | (frame["high"] < frame["close"])
        | (frame["low"] > frame["open"])
        | (frame["low"] > frame["close"])
    )
    _add(
        findings, "IMPOSSIBLE_OHLC", Severity.ERROR, int(impossible.sum()),
        "high is not the highest or low is not the lowest; every indicator "
        "computed from these bars describes a market that cannot exist",
    )

    if "bid" in frame.columns and "ask" in frame.columns:
        crossed = frame["bid"] > frame["ask"]
        _add(
            findings, "CROSSED_QUOTE", Severity.ERROR, int(crossed.sum()),
            "bid above ask, which is not a quote",
        )

    # --- interpretive: these need saying, not refusing ---------------------

    if not timezone_was_declared:
        findings.append(
            Finding(
                "TIMEZONE_ASSUMED", Severity.WARNING, 1,
                "the source declared no timezone and the timestamps were taken "
                "as UTC. Gold and FX histories are commonly exported in broker "
                "server time, and a silent offset moves every session boundary",
            )
        )

    if not has_real_spread:
        findings.append(
            Finding(
                "NO_OBSERVED_SPREAD", Severity.WARNING, 1,
                "the source carries no bid/ask, so transaction costs are an "
                "ASSUMPTION rather than an observation. Results are ESTIMATED_COSTS",
            )
        )

    gaps = frame.index.to_series().diff().dropna()
    if len(gaps):
        irregular = int((gaps != timeframe).sum())
        _add(
            findings, "IRREGULAR_SPACING", Severity.WARNING, irregular,
            f"intervals between bars that are not {timeframe}; expected across "
            f"weekends and the daily break, and a defect anywhere else",
        )
        big = int((gaps > timeframe * 72).sum())
        _add(
            findings, "LARGE_GAP", Severity.WARNING, big,
            "gaps longer than three days, which exceed a normal weekend close",
        )

    ranges = (frame["high"] - frame["low"]).to_numpy(dtype=float)
    median_range = float(np.median(ranges)) if len(ranges) else 0.0
    if median_range > 0:
        _add(
            findings, "ABNORMAL_RANGE", Severity.WARNING,
            int((ranges > median_range * ABNORMAL_RANGE_MULTIPLE).sum()),
            f"bars whose range exceeds {ABNORMAL_RANGE_MULTIPLE:.0f}x the median. "
            f"Real news does this; so does a bad tick",
        )

    closes = frame["close"].to_numpy(dtype=float)
    _add(
        findings, "SUSPECTED_FLATLINE", Severity.WARNING,
        _longest_repeat(closes) if _longest_repeat(closes) >= FLATLINE_RUN else 0,
        f"{_longest_repeat(closes)} consecutive identical closes, which usually "
        f"means a stalled feed rather than a still market",
    )

    if "spread" in frame.columns:
        spread = frame["spread"].to_numpy(dtype=float)
        finite = spread[np.isfinite(spread)]
        if finite.size:
            median_spread = float(np.median(finite))
            if median_spread > 0:
                _add(
                    findings, "ABNORMAL_SPREAD", Severity.WARNING,
                    int((finite > median_spread * 25).sum()),
                    "spreads far above the median, typical of a rollover or a "
                    "thin session and also of a bad quote",
                )

    index = pd.DatetimeIndex(frame.index)
    saturday_bars = int((index.dayofweek == 5).sum())
    _add(
        findings, "WEEKEND_BARS", Severity.WARNING, saturday_bars,
        "bars timestamped on a Saturday. FX and metals are closed; this is "
        "usually a timezone offset rather than trading",
    )
    report.findings = findings

    if market_clock is not None:
        _measure_coverage(report, frame, timeframe, market_clock)
    return report


def _longest_repeat(values: np.ndarray) -> int:
    """Longest run of identical consecutive values."""
    if values.size == 0:
        return 0
    changes = np.flatnonzero(np.diff(values) != 0)
    boundaries = np.concatenate(([-1], changes, [values.size - 1]))
    return int(np.diff(boundaries).max())


@dataclass(frozen=True)
class MarketClock:
    """When the venue is open, so a gap can be attributed rather than counted.

    A missing Sunday is the market being shut. A missing Wednesday is the
    provider losing data. Reporting both as "missing bars" makes a complete
    dataset look broken and hides the one that is.

    Modelled on `fx_metals_24x5`, the same calendar the control plane uses:
    open Sunday 22:00 UTC through Friday 21:00 UTC, with a daily break.
    """

    open_weekday_start: int = 6  # Sunday
    open_hour_utc: int = 22
    close_weekday: int = 4  # Friday
    close_hour_utc: int = 21
    daily_break_hour_utc: int = 21
    daily_break_hours: int = 1

    def is_open(self, when: pd.Timestamp) -> bool:
        weekday, hour = when.dayofweek, when.hour
        if weekday == 5:  # Saturday
            return False
        if weekday == 6:  # Sunday: opens in the evening
            return hour >= self.open_hour_utc
        if weekday == self.close_weekday and hour >= self.close_hour_utc:
            return False
        break_start = self.daily_break_hour_utc
        return not break_start <= hour < break_start + self.daily_break_hours


def _measure_coverage(
    report: DataQualityReport,
    frame: pd.DataFrame,
    timeframe: timedelta,
    clock: MarketClock,
) -> None:
    """Expected versus actual bars, with closures excluded from 'missing'."""
    full = pd.date_range(frame.index[0], frame.index[-1], freq=timeframe, tz="UTC")
    open_bars = [t for t in full if clock.is_open(t)]
    expected = len(open_bars)

    report.expected_bars = expected
    report.closed_market_bars = len(full) - expected
    present = frame.index.intersection(pd.DatetimeIndex(open_bars))
    report.missing_bars = expected - len(present)
    report.coverage_fraction = (len(present) / expected) if expected else None

    if report.coverage_fraction is not None and report.coverage_fraction < 0.90:
        report.findings.append(
            Finding(
                "LOW_COVERAGE", Severity.WARNING, report.missing_bars or 0,
                f"only {report.coverage_fraction:.1%} of the bars the market "
                f"clock says should exist are present. Missing bars are not "
                f"interpolated -- a fabricated bar is a fabricated trade",
            )
        )
