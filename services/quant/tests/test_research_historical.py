"""Ingestion, quality, partitioning and the taxonomy that receives the results.

The fixtures here are CONSTRUCTED, not market data. They exist to prove the
importer, validator and partitioner behave, and nothing they produce is ever
reported as historical evidence -- a test fixture that got quoted as a finding
would be exactly the fabrication this milestone refuses to commit.
"""

from __future__ import annotations

from datetime import UTC, datetime, timedelta
from pathlib import Path

import pandas as pd
import pytest

from vantage_quant.research import (
    compare,
    datasets,
    expansion,
    historical,
    historical_run,
    quality,
    requirements,
    verdict,
)

# --- helpers ----------------------------------------------------------------


def _write_csv(directory: Path, name: str, rows: list[str], header: str) -> Path:
    directory.mkdir(parents=True, exist_ok=True)
    path = directory / name
    path.write_text("\n".join([header, *rows]) + "\n", encoding="utf-8")
    return path


def _clean_rows(
    count: int, *, start: datetime | None = None, step_hours: int = 1
) -> list[str]:
    """A plausible bar series. Not a market; a shape the importer must accept."""
    base = start or datetime(2024, 1, 2, 0, tzinfo=UTC)
    out = []
    price = 2000.0
    for i in range(count):
        when = base + timedelta(hours=i * step_hours)
        drift = 0.6 if i % 3 else -0.4
        open_, close = price, price + drift
        high, low = max(open_, close) + 0.5, min(open_, close) - 0.5
        out.append(
            f"{when:%Y-%m-%d %H:%M:%S},{open_:.2f},{high:.2f},{low:.2f},{close:.2f}"
        )
        price = close
    return out


HEADER = "timestamp,open,high,low,close"


# --- the taxonomy fix -------------------------------------------------------


def test_the_two_meanings_of_the_old_label_are_now_separate_verdicts() -> None:
    """The defect this milestone opens by fixing.

    MORE_DATA_REQUIRED meant both "collect more bars" and "stop collecting,
    the score is the problem". Acting on the wrong one wastes a milestone.
    """
    thin, _ = expansion.classify(
        role=expansion.StrategyRole.ALPHA_BREAKOUT,
        score_span_used=0.8,
        monotonicity="WEAK_POSITIVE",
        mean_net_return=0.001,
        spearman_interval=None,
        raw_n=90,
        effective_n=20,
    )
    measured, _ = expansion.classify(
        role=expansion.StrategyRole.ALPHA_BREAKOUT,
        score_span_used=0.8,
        monotonicity="WEAK_POSITIVE",
        mean_net_return=0.001,
        spearman_interval=expansion.Interval(0.02, -0.03, 0.07, 500),
        raw_n=2211,
        effective_n=409,
    )
    assert thin is verdict.CalibrationVerdict.INSUFFICIENT_DATA
    assert measured is verdict.CalibrationVerdict.ORDERING_NOT_ESTABLISHED
    assert thin is not measured
    assert thin in verdict.NEEDS_MORE_DATA
    assert measured in verdict.SCORE_IS_THE_PROBLEM


def test_many_signals_but_few_episodes_is_its_own_verdict() -> None:
    """Distinct from INSUFFICIENT_DATA because the remedy differs.

    More bars help only if they contain new episodes rather than longer ones,
    and a reader told "insufficient data" would collect the wrong thing.
    """
    decided, reason = expansion.classify(
        role=expansion.StrategyRole.ALPHA_MEAN_REVERSION,
        score_span_used=0.99,
        monotonicity="NEGATIVE",
        mean_net_return=-0.002,
        spearman_interval=expansion.Interval(-0.13, -0.26, 0.02, 500),
        raw_n=1096,
        effective_n=110,
    )
    assert decided is verdict.CalibrationVerdict.INSUFFICIENT_INDEPENDENT_EPISODES
    assert "independence, not volume" in reason


def test_a_backwards_ordering_is_named_rather_than_called_absent() -> None:
    decided, reason = expansion.classify(
        role=expansion.StrategyRole.ALPHA_MEAN_REVERSION,
        score_span_used=0.99,
        monotonicity="NEGATIVE",
        mean_net_return=0.001,
        spearman_interval=expansion.Interval(-0.30, -0.45, -0.15, 500),
        raw_n=1500,
        effective_n=600,
    )
    assert decided is verdict.CalibrationVerdict.NEGATIVE_ORDERING
    assert "opposite" in reason


def test_prior_runs_are_translated_not_rewritten() -> None:
    """Milestone E's records keep their labels; this maps them for display."""
    mapped, _ = verdict.map_legacy("NON_MONOTONIC")
    assert mapped is verdict.CalibrationVerdict.NON_MONOTONIC_SCORE

    mapped, _ = verdict.map_legacy("MORE_DATA_REQUIRED", effective_n=409, raw_n=2211)
    assert mapped is verdict.CalibrationVerdict.ORDERING_NOT_ESTABLISHED

    mapped, _ = verdict.map_legacy("MORE_DATA_REQUIRED", effective_n=110, raw_n=1096)
    assert mapped is verdict.CalibrationVerdict.INSUFFICIENT_INDEPENDENT_EPISODES

    mapped, _ = verdict.map_legacy("MORE_DATA_REQUIRED", effective_n=0, raw_n=0)
    assert mapped is verdict.CalibrationVerdict.INSUFFICIENT_DATA


def test_an_ambiguous_legacy_label_without_context_refuses_to_guess() -> None:
    """A migration that guesses is indistinguishable from one that knows."""
    mapped, reason = verdict.map_legacy("MORE_DATA_REQUIRED")
    assert mapped is None
    assert "ambiguous" in reason


def test_the_score_and_the_signal_rule_are_flagged_separately() -> None:
    """donchian: the entry rule paid, the score failed to rank it.

    Collapsing those into one verdict would hide the most actionable finding
    the synthetic run produced.
    """
    flag, reason = expansion.redesign_flag(
        strategy_verdict=verdict.CalibrationVerdict.ORDERING_NOT_ESTABLISHED,
        net_return_interval=expansion.Interval(0.0071, 0.0045, 0.0095, 500),
    )
    assert flag is verdict.RedesignFlag.SCORE_REDESIGN_CANDIDATE
    assert "confidence formula" in reason

    flag, _ = expansion.redesign_flag(
        strategy_verdict=verdict.CalibrationVerdict.NON_MONOTONIC_SCORE,
        net_return_interval=expansion.Interval(0.0, -0.0003, 0.0003, 500),
    )
    assert flag is verdict.RedesignFlag.SIGNAL_RESEARCH_REQUIRED


def test_a_thin_sample_flags_neither_redesign() -> None:
    """Too little evidence to blame the score OR the hypothesis."""
    flag, _ = expansion.redesign_flag(
        strategy_verdict=verdict.CalibrationVerdict.INSUFFICIENT_DATA,
        net_return_interval=None,
    )
    assert flag is verdict.RedesignFlag.NONE


# --- import security --------------------------------------------------------


def test_a_dataset_is_addressed_by_name_never_by_path(tmp_path: Path) -> None:
    """An arbitrary path parameter is a file-read primitive on the host."""
    historical.data_directory(tmp_path).mkdir(parents=True)
    hostile = [
        "../secrets.csv",
        "/etc/passwd",
        "a/b.csv",
        "..\\x.csv",
        "C:\\Windows\\win.ini",
        ".env",
        "",
    ]
    for name in hostile:
        with pytest.raises(historical.UnsafeDatasetPathError):
            historical.load(name, instrument="XAUUSD", timeframe="1h", root=tmp_path)


def test_a_sibling_directory_sharing_a_prefix_is_not_inside(tmp_path: Path) -> None:
    """`research-data-elsewhere` starts with `research-data` and is not it."""
    directory = historical.data_directory(tmp_path)
    directory.mkdir(parents=True)
    sibling = tmp_path / (historical.HISTORICAL_DATA_DIRNAME + "-elsewhere")
    sibling.mkdir()
    (sibling / "leak.csv").write_text("x", encoding="utf-8")
    with pytest.raises(historical.UnsafeDatasetPathError):
        historical.load(
            "../research-data-elsewhere/leak.csv",
            instrument="X", timeframe="1h", root=tmp_path,
        )


def test_an_unknown_suffix_is_refused(tmp_path: Path) -> None:
    directory = historical.data_directory(tmp_path)
    directory.mkdir(parents=True)
    (directory / "probe.exe").write_bytes(b"MZ")
    with pytest.raises(historical.UnsafeDatasetPathError):
        historical.load("probe.exe", instrument="X", timeframe="1h", root=tmp_path)


def test_an_oversized_file_is_refused_before_it_is_read(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    """Loading a 40 GB CSV is a denial of service against this machine."""
    directory = historical.data_directory(tmp_path)
    _write_csv(directory, "big.csv", _clean_rows(5), HEADER)
    monkeypatch.setattr(historical, "MAX_FILE_BYTES", 10)
    with pytest.raises(historical.MalformedDatasetError, match="above the"):
        historical.load("big.csv", instrument="X", timeframe="1h", root=tmp_path)


def test_an_empty_file_is_refused(tmp_path: Path) -> None:
    directory = historical.data_directory(tmp_path)
    directory.mkdir(parents=True)
    (directory / "empty.csv").write_text("", encoding="utf-8")
    with pytest.raises(historical.MalformedDatasetError):
        historical.load("empty.csv", instrument="X", timeframe="1h", root=tmp_path)


def test_duplicate_columns_are_refused_rather_than_silently_disambiguated(
    tmp_path: Path,
) -> None:
    """Pandas renames the second `close` and one of them quietly wins."""
    directory = historical.data_directory(tmp_path)
    _write_csv(
        directory, "dupe.csv",
        ["2024-01-02 00:00:00,2000,2001,1999,2000.5,1234"],
        "timestamp,open,high,low,close,close",
    )
    with pytest.raises(historical.MalformedDatasetError, match="duplicate column"):
        historical.load("dupe.csv", instrument="X", timeframe="1h", root=tmp_path)


def test_a_formula_like_column_name_is_refused(tmp_path: Path) -> None:
    """A cell beginning `=` becomes executable when re-exported to a spreadsheet."""
    directory = historical.data_directory(tmp_path)
    _write_csv(
        directory, "formula.csv",
        ["2024-01-02 00:00:00,2000,2001,1999,2000.5"],
        "timestamp,open,high,low,=cmd|'/c calc'!A1",
    )
    with pytest.raises(historical.MalformedDatasetError, match="formula"):
        historical.load("formula.csv", instrument="X", timeframe="1h", root=tmp_path)


def test_unparseable_timestamps_are_refused(tmp_path: Path) -> None:
    directory = historical.data_directory(tmp_path)
    _write_csv(
        directory, "bad_time.csv",
        ["not-a-date,2000,2001,1999,2000.5"],
        HEADER,
    )
    with pytest.raises(historical.MalformedDatasetError):
        historical.load("bad_time.csv", instrument="X", timeframe="1h", root=tmp_path)


def test_a_missing_timestamp_column_is_refused(tmp_path: Path) -> None:
    directory = historical.data_directory(tmp_path)
    _write_csv(directory, "no_time.csv", ["2000,2001,1999,2000.5"], "open,high,low,close")
    with pytest.raises(historical.MalformedDatasetError, match="timestamp"):
        historical.load("no_time.csv", instrument="X", timeframe="1h", root=tmp_path)


def test_a_missing_directory_raises_rather_than_returning_nothing(
    tmp_path: Path,
) -> None:
    """An empty frame flows onward and produces zeroes that read as a result."""
    with pytest.raises(historical.HistoricalDataUnavailableError):
        historical.load("anything.csv", instrument="X", timeframe="1h", root=tmp_path)


def test_nothing_is_fetched_when_a_dataset_is_missing(tmp_path: Path) -> None:
    """No network fallback, by construction (brief section 50)."""
    historical.data_directory(tmp_path).mkdir(parents=True)
    with pytest.raises(historical.HistoricalDataUnavailableError, match="not present"):
        historical.load("absent.csv", instrument="X", timeframe="1h", root=tmp_path)


# --- normalisation and provenance -------------------------------------------


def test_provenance_records_both_hashes_and_never_invents_a_source(
    tmp_path: Path,
) -> None:
    directory = historical.data_directory(tmp_path)
    _write_csv(directory, "probe.csv", _clean_rows(60), HEADER)
    loaded = historical.load(
        "probe.csv", instrument="XAUUSD", timeframe="1h", root=tmp_path
    )
    p = loaded.provenance
    assert p.source_type.value == "HISTORICAL_MARKET"
    assert len(p.original_file_hash) == 64
    assert len(p.normalized_dataset_hash) == 64
    assert p.original_file_hash != p.normalized_dataset_hash
    # Unstated facts are recorded as unstated. An invented licence note looks
    # like diligence and is worse than none.
    assert p.provider == "UNRECORDED"
    assert p.licensing_note == "UNRECORDED"
    assert p.acquisition_method == "UNRECORDED"


def test_the_original_hash_survives_normalisation(tmp_path: Path) -> None:
    """RAW stays answerable: 'was that in the file, or did we do it?'"""
    import hashlib

    directory = historical.data_directory(tmp_path)
    path = _write_csv(directory, "probe.csv", _clean_rows(40), HEADER)
    on_disk = hashlib.sha256(path.read_bytes()).hexdigest()
    loaded = historical.load(
        "probe.csv", instrument="XAUUSD", timeframe="1h", root=tmp_path
    )
    assert loaded.provenance.original_file_hash == on_disk
    # And the file itself is untouched.
    assert hashlib.sha256(path.read_bytes()).hexdigest() == on_disk


def test_an_undeclared_timezone_is_recorded_as_an_assumption(tmp_path: Path) -> None:
    """Never inferred from this machine's locale.

    Gold and FX exports are commonly in broker server time; a silent offset
    moves every session boundary in the analysis.
    """
    directory = historical.data_directory(tmp_path)
    _write_csv(directory, "naive.csv", _clean_rows(40), HEADER)
    loaded = historical.load(
        "naive.csv", instrument="XAUUSD", timeframe="1h", root=tmp_path
    )
    assert loaded.provenance.original_timezone == "UNDECLARED"
    assert loaded.provenance.timezone_declared is False
    assert any("ASSUMED UTC" in t for t in loaded.provenance.transformations)
    assert any(f.code == "TIMEZONE_ASSUMED" for f in loaded.quality_report.warnings)


def test_a_declared_timezone_is_converted_and_recorded(tmp_path: Path) -> None:
    directory = historical.data_directory(tmp_path)
    _write_csv(directory, "ny.csv", _clean_rows(40), HEADER)
    loaded = historical.load(
        "ny.csv", instrument="XAUUSD", timeframe="1h", root=tmp_path,
        declared_timezone="America/New_York",
    )
    assert loaded.provenance.original_timezone == "America/New_York"
    assert loaded.provenance.timezone_declared is True
    assert str(loaded.frame.index.tz) == "UTC"
    # 2024-01-02 00:00 New York is 05:00 UTC.
    assert loaded.frame.index[0] == pd.Timestamp("2024-01-02 05:00", tz="UTC")


def test_a_daylight_saving_transition_survives_conversion(tmp_path: Path) -> None:
    """US DST began 2024-03-10. The offset must change ACROSS it.

    A conversion that applied one fixed offset would shift every bar after the
    transition by an hour, moving every session label with it.
    """
    directory = historical.data_directory(tmp_path)
    rows = _clean_rows(1, start=datetime(2024, 3, 9, 12)) + _clean_rows(
        1, start=datetime(2024, 3, 11, 12)
    )
    _write_csv(directory, "dst.csv", rows, HEADER)
    loaded = historical.load(
        "dst.csv", instrument="XAUUSD", timeframe="1h", root=tmp_path,
        declared_timezone="America/New_York",
    )
    before, after = loaded.frame.index[0], loaded.frame.index[1]
    assert before == pd.Timestamp("2024-03-09 17:00", tz="UTC")  # EST, UTC-5
    assert after == pd.Timestamp("2024-03-11 16:00", tz="UTC")  # EDT, UTC-4


def test_rows_are_sorted_and_duplicates_dropped_with_a_record(tmp_path: Path) -> None:
    directory = historical.data_directory(tmp_path)
    _write_csv(
        directory, "messy.csv",
        [
            "2024-01-02 01:00:00,2001,2002,2000,2001.5",
            "2024-01-02 00:00:00,2000,2001,1999,2000.5",
            "2024-01-02 01:00:00,2001,2002,2000,2001.5",
        ],
        HEADER,
    )
    loaded = historical.load(
        "messy.csv", instrument="XAUUSD", timeframe="1h", root=tmp_path
    )
    assert loaded.frame.index.is_monotonic_increasing
    assert len(loaded.frame) == 2
    assert any("sorted by timestamp" in t for t in loaded.provenance.transformations)
    assert any("duplicate timestamp" in t for t in loaded.provenance.transformations)


def test_an_absent_spread_is_declared_rather_than_synthesised(tmp_path: Path) -> None:
    """Section 8: do not silently invent bid/ask.

    A backtest that reports costs it never paid is worse than one that admits
    it is estimating.
    """
    directory = historical.data_directory(tmp_path)
    _write_csv(directory, "nospread.csv", _clean_rows(40), HEADER)
    loaded = historical.load(
        "nospread.csv", instrument="XAUUSD", timeframe="1h", root=tmp_path
    )
    assert loaded.provenance.has_observed_spread is False
    assert loaded.provenance.cost_basis == "ESTIMATED_COSTS"
    assert "bid" not in loaded.frame.columns
    assert any(f.code == "NO_OBSERVED_SPREAD" for f in loaded.quality_report.warnings)


def test_a_real_spread_is_recognised(tmp_path: Path) -> None:
    directory = historical.data_directory(tmp_path)
    _write_csv(
        directory, "spread.csv",
        [
            "2024-01-02 00:00:00,2000,2001,1999,2000.5,2000.3,2000.7",
            "2024-01-02 01:00:00,2000.5,2001.5,2000,2001,2000.8,2001.2",
        ],
        "timestamp,open,high,low,close,bid,ask",
    )
    loaded = historical.load(
        "spread.csv", instrument="XAUUSD", timeframe="1h", root=tmp_path
    )
    assert loaded.provenance.has_observed_spread is True
    assert loaded.provenance.cost_basis == "OBSERVED_SPREAD"


def test_a_zero_spread_column_is_not_an_observed_spread(tmp_path: Path) -> None:
    """A column of zeroes has not observed anything."""
    directory = historical.data_directory(tmp_path)
    _write_csv(
        directory, "zero.csv",
        [
            "2024-01-02 00:00:00,2000,2001,1999,2000.5,0",
            "2024-01-02 01:00:00,2000.5,2001.5,2000,2001,0",
        ],
        "timestamp,open,high,low,close,spread",
    )
    loaded = historical.load(
        "zero.csv", instrument="XAUUSD", timeframe="1h", root=tmp_path
    )
    assert loaded.provenance.cost_basis == "ESTIMATED_COSTS"


def test_unrecognised_columns_are_dropped_and_recorded(tmp_path: Path) -> None:
    directory = historical.data_directory(tmp_path)
    _write_csv(
        directory, "extra.csv",
        ["2024-01-02 00:00:00,2000,2001,1999,2000.5,whatever"],
        "timestamp,open,high,low,close,mystery",
    )
    loaded = historical.load(
        "extra.csv", instrument="XAUUSD", timeframe="1h", root=tmp_path
    )
    assert "mystery" not in loaded.frame.columns
    assert any("mystery" in t for t in loaded.provenance.transformations)


# --- quality ----------------------------------------------------------------


def _frame(rows: list[tuple[str, float, float, float, float]]) -> pd.DataFrame:
    frame = pd.DataFrame(
        rows, columns=["timestamp", "open", "high", "low", "close"]
    )
    frame.index = pd.DatetimeIndex(
        pd.to_datetime(frame.pop("timestamp"), utc=True), name="timestamp"
    )
    return frame


def _assess(frame: pd.DataFrame, **kwargs: object) -> quality.DataQualityReport:
    defaults: dict = {
        "dataset_id": "probe",
        "timeframe": timedelta(hours=1),
        "timezone_was_declared": True,
        "has_real_spread": True,
    }
    defaults.update(kwargs)
    return quality.assess(frame, **defaults)  # type: ignore[arg-type]


def test_impossible_ohlc_rejects_the_dataset() -> None:
    """Every indicator built on these describes a market that cannot exist."""
    frame = _frame(
        [
            ("2024-01-02 00:00", 2000, 1990, 1999, 2000.5),  # high below low
            ("2024-01-02 01:00", 2000, 2001, 1999, 2000.5),
        ]
    )
    report = _assess(frame)
    assert report.status is quality.DatasetStatus.REJECTED
    assert any(f.code == "IMPOSSIBLE_OHLC" for f in report.errors)


def test_a_non_positive_price_rejects_the_dataset() -> None:
    frame = _frame([("2024-01-02 00:00", 0.0, 1.0, 0.0, 0.5)])
    assert _assess(frame).status is quality.DatasetStatus.REJECTED


def test_a_crossed_quote_rejects_the_dataset() -> None:
    frame = _frame([("2024-01-02 00:00", 2000, 2001, 1999, 2000.5)])
    frame["bid"], frame["ask"] = 2001.0, 2000.0
    report = _assess(frame)
    assert any(f.code == "CROSSED_QUOTE" for f in report.errors)
    assert report.status is quality.DatasetStatus.REJECTED


def test_a_rejected_dataset_cannot_enter_research(tmp_path: Path) -> None:
    """The gate, not merely the label."""
    directory = historical.data_directory(tmp_path)
    _write_csv(
        directory, "broken.csv",
        [
            "2024-01-02 00:00:00,2000,1990,1999,2000.5",
            "2024-01-02 01:00:00,2000,2001,1999,2000.5",
        ],
        HEADER,
    )
    with pytest.raises(historical.DatasetRejectedError):
        historical.load("broken.csv", instrument="X", timeframe="1h", root=tmp_path)


def test_a_gap_is_a_warning_not_a_rejection() -> None:
    """A validator that refused every imperfect file would refuse every real one."""
    rows = [
        ("2024-01-02 00:00", 2000, 2001, 1999, 2000.5),
        ("2024-01-09 00:00", 2000, 2001, 1999, 2000.5),
    ]
    report = _assess(_frame(rows))
    assert report.status is quality.DatasetStatus.VALID_WITH_WARNINGS
    assert any(f.code == "LARGE_GAP" for f in report.warnings)


def test_a_flatline_is_flagged() -> None:
    """A stalled feed, not a still market."""
    rows = [
        (f"2024-01-02 {h:02d}:00", 2000, 2000.5, 1999.5, 2000.0) for h in range(20)
    ]
    report = _assess(_frame(rows))
    assert any(f.code == "SUSPECTED_FLATLINE" for f in report.warnings)


def test_saturday_bars_are_flagged_as_a_likely_timezone_error() -> None:
    """FX and metals are shut; bars there usually mean an offset."""
    rows = [("2024-01-06 12:00", 2000, 2001, 1999, 2000.5)]  # a Saturday
    report = _assess(_frame(rows))
    assert any(f.code == "WEEKEND_BARS" for f in report.warnings)


def test_a_clean_series_is_valid_with_no_findings() -> None:
    rows = [
        (f"2024-01-02 {h:02d}:00", 2000 + h, 2002 + h, 1999 + h, 2001 + h)
        for h in range(10)
    ]
    report = _assess(_frame(rows))
    assert report.status is quality.DatasetStatus.VALID
    assert report.findings == []


def test_an_empty_frame_is_rejected_rather_than_reported_as_clean() -> None:
    frame = _frame([("2024-01-02 00:00", 2000, 2001, 1999, 2000.5)]).iloc[0:0]
    report = _assess(frame)
    assert report.status is quality.DatasetStatus.REJECTED


# --- market clock and coverage ----------------------------------------------


def test_the_market_clock_knows_when_the_venue_is_shut() -> None:
    clock = quality.MarketClock()
    assert not clock.is_open(pd.Timestamp("2024-01-06 12:00", tz="UTC"))  # Saturday
    assert not clock.is_open(pd.Timestamp("2024-01-07 12:00", tz="UTC"))  # Sun morning
    assert clock.is_open(pd.Timestamp("2024-01-07 23:00", tz="UTC"))  # Sun evening
    assert clock.is_open(pd.Timestamp("2024-01-09 12:00", tz="UTC"))  # Tuesday
    assert not clock.is_open(pd.Timestamp("2024-01-09 21:00", tz="UTC"))  # daily break


def test_a_closed_market_is_not_counted_as_missing_data() -> None:
    """Section 12: distinguish closure from a provider losing the data.

    Counting the weekend as missing makes a complete dataset look broken and
    hides the one that is.
    """
    index = pd.date_range("2024-01-08 00:00", "2024-01-12 20:00", freq="1h", tz="UTC")
    clock = quality.MarketClock()
    index = pd.DatetimeIndex([t for t in index if clock.is_open(t)])
    frame = pd.DataFrame(
        {
            "open": 2000.0, "high": 2001.0, "low": 1999.0, "close": 2000.5,
        },
        index=index,
    )
    report = _assess(frame, market_clock=clock)
    assert report.missing_bars == 0
    assert report.coverage_fraction == 1.0
    assert report.closed_market_bars is not None and report.closed_market_bars > 0


def test_missing_provider_data_lowers_coverage() -> None:
    """A hole in the MIDDLE, which is how a provider actually loses a week.

    Coverage is measured between the first and last bar present, so truncation
    at either end is invisible to it by construction -- nothing in the file can
    say the series ought to have continued. Gaps inside the span are what it
    detects, and those are what a lost week looks like.
    """
    index = pd.date_range("2024-01-08 00:00", "2024-01-11 20:00", freq="1h", tz="UTC")
    clock = quality.MarketClock()
    open_bars = [t for t in index if clock.is_open(t)]
    half = len(open_bars) // 2
    kept = pd.DatetimeIndex(open_bars[: half // 2] + open_bars[half:])
    frame = pd.DataFrame(
        {"open": 2000.0, "high": 2001.0, "low": 1999.0, "close": 2000.5}, index=kept
    )
    report = _assess(frame, market_clock=clock)
    assert report.coverage_fraction is not None
    assert report.coverage_fraction < 0.9
    assert report.missing_bars is not None and report.missing_bars > 0
    assert any(f.code == "LOW_COVERAGE" for f in report.warnings)


# --- resampling -------------------------------------------------------------


def test_resampling_only_ever_coarsens() -> None:
    """Producing finer bars would assert trades nobody made."""
    frame = _frame(
        [(f"2024-01-02 {h:02d}:00", 2000, 2001, 1999, 2000.5) for h in range(8)]
    )
    with pytest.raises(historical.MalformedDatasetError, match="only ever coarsens"):
        historical.resample(
            frame, source_timeframe="1h", target_timeframe="15m", transformations=[]
        )


def test_aggregation_takes_first_max_min_last() -> None:
    frame = _frame(
        [
            ("2024-01-02 00:00", 2000, 2005, 1995, 2001),
            ("2024-01-02 01:00", 2001, 2010, 1990, 2002),
            ("2024-01-02 02:00", 2002, 2004, 1998, 2003),
            ("2024-01-02 03:00", 2003, 2006, 1997, 2004),
        ]
    )
    transformations: list[str] = []
    out = historical.resample(
        frame, source_timeframe="1h", target_timeframe="4h",
        transformations=transformations,
    )
    assert len(out) == 1
    assert out.iloc[0]["open"] == 2000
    assert out.iloc[0]["high"] == 2010
    assert out.iloc[0]["low"] == 1990
    assert out.iloc[0]["close"] == 2004
    assert any("never interpolated" in t for t in transformations)


def test_an_empty_period_is_dropped_not_filled() -> None:
    """A forward-filled bar is a fabricated trade."""
    frame = _frame(
        [
            ("2024-01-02 00:00", 2000, 2005, 1995, 2001),
            ("2024-01-03 00:00", 2001, 2010, 1990, 2002),
        ]
    )
    out = historical.resample(
        frame, source_timeframe="1h", target_timeframe="4h", transformations=[]
    )
    assert len(out) == 2  # two populated windows, not the 7 between them


# --- partitioning and the seal ----------------------------------------------


def _fake_dataset(name: str, start: str, bars: int) -> historical.HistoricalDataset:
    index = pd.date_range(start, periods=bars, freq="1h", tz="UTC")
    frame = pd.DataFrame(
        {"open": 2000.0, "high": 2001.0, "low": 1999.0, "close": 2000.5}, index=index
    )
    report = quality.DataQualityReport(
        dataset_id=name, checker_version=1, rows=bars,
        first_bar=index[0].isoformat(), last_bar=index[-1].isoformat(),
        actual_bars=bars,
    )
    provenance = historical.Provenance(
        source=f"{name}.csv",
        source_type=historical.SourceType.HISTORICAL_MARKET,
        instrument="XAUUSD", timeframe="1h",
        start=index[0].to_pydatetime(), end=index[-1].to_pydatetime(),
        bars=bars, ingested_at=datetime.now(UTC),
        original_file_hash="0" * 64, normalized_dataset_hash=name.ljust(64, "0"),
        original_timezone="UTC", timezone_declared=True,
    )
    return historical.HistoricalDataset(
        dataset_id=name, frame=frame, provenance=provenance, quality_report=report
    )


def test_historical_data_is_partitioned_chronologically_and_test_is_sealed() -> None:
    datasets = [
        _fake_dataset("d4", "2024-04-01", 50),
        _fake_dataset("d1", "2024-01-01", 50),
        _fake_dataset("d3", "2024-03-01", 50),
        _fake_dataset("d2", "2024-02-01", 50),
    ]
    assignments = historical_run._assign(datasets)
    ordered = [a.dataset_id for a in assignments]
    assert ordered == ["d1", "d2", "d3", "d4"], "must order by first bar, not by input"

    sealed = [a for a in assignments if a.seal]
    assert sealed, "a TEST partition must exist and be sealed"
    assert all(a.seal == "SEALED_FOR_OUT_OF_SAMPLE" for a in sealed)
    assert all(
        a.partition.value == "TEST" for a in assignments if a.seal
    )


def test_the_sealed_partition_never_reaches_analysis() -> None:
    """The guard is called by every entry point, not at each call site."""
    from vantage_quant.research import partition

    datasets = [
        _fake_dataset(f"d{i}", f"2024-0{i}-01", 50) for i in range(1, 5)
    ]
    assignments = historical_run._assign(datasets)
    explorable = partition.explorable(assignments)
    assert all(not a.seal for a in explorable)
    assert all(a.partition.value == "TRAIN" for a in explorable)

    with pytest.raises(partition.SealedDatasetError):
        partition.guard_not_sealed(assignments)


def test_a_rejected_dataset_is_excluded_from_partitioning() -> None:
    good = _fake_dataset("good", "2024-01-01", 50)
    bad = _fake_dataset("bad", "2024-02-01", 50)
    bad.quality_report.findings.append(
        quality.Finding("IMPOSSIBLE_OHLC", quality.Severity.ERROR, 3, "broken")
    )
    assignments = historical_run._assign([good, bad])
    assert [a.dataset_id for a in assignments] == ["good"]


# --- the run refuses to invent a result -------------------------------------


def test_the_historical_run_refuses_when_no_data_exists(tmp_path: Path) -> None:
    """A run full of zeroes reads like a measurement showing nothing."""
    with pytest.raises(historical_run.NoHistoricalDataError):
        historical_run.run(root=tmp_path)


def test_the_status_line_is_honest_when_nothing_is_present(tmp_path: Path) -> None:
    line = historical.status(tmp_path)
    assert "REAL_MARKET_VALIDATION_PENDING" in line
    assert "SYNTHETIC_CONTROLLED" in line


def test_the_waiting_report_states_exactly_what_is_needed() -> None:
    text = requirements.waiting_report()
    assert "WAITING_FOR_HISTORICAL_DATA" in text
    for expected in ("instrument", "timeframe", "minimum bars", "required columns"):
        assert expected in text
    assert ".csv" in text


def test_the_minimum_bar_count_follows_the_longest_warmup() -> None:
    """Derived, not picked: a number nobody can justify is a number to ignore."""
    from vantage_quant import strategies

    longest = max(s.required_bars for s in strategies.REGISTRY.values())
    assert requirements.requirement().minimum_bars > longest


# --- comparison -------------------------------------------------------------


def _side(
    *, source: str, spearman: float | None, excludes_zero: bool, span: float = 0.9,
    survives: bool = True, raw: int = 1000, eff: int = 400,
) -> compare.Side:
    return compare.Side(
        source_type=source, verdict="X", effective_n=eff, raw_n=raw,
        spearman=spearman, spearman_excludes_zero=excludes_zero,
        score_span_used=span, mean_net_return=0.001, net_survives_costs=survives,
    )


def test_a_synthetic_only_relationship_is_named() -> None:
    """The finding this whole milestone exists to be able to make."""
    state, detail = compare.classify_disagreement(
        _side(source="SYNTHETIC_CONTROLLED", spearman=0.12, excludes_zero=True),
        _side(source="HISTORICAL_MARKET", spearman=0.01, excludes_zero=False),
    )
    assert state is verdict.Disagreement.SYNTHETIC_ONLY_RELATIONSHIP
    assert "manufacturing" in detail


def test_a_direction_reversal_is_named() -> None:
    state, _ = compare.classify_disagreement(
        _side(source="SYNTHETIC_CONTROLLED", spearman=0.20, excludes_zero=True),
        _side(source="HISTORICAL_MARKET", spearman=-0.18, excludes_zero=True),
    )
    assert state is verdict.Disagreement.DIRECTION_REVERSAL


def test_a_score_range_mismatch_outranks_everything_downstream() -> None:
    """Different parts of the score function are different populations."""
    state, _ = compare.classify_disagreement(
        _side(source="SYNTHETIC_CONTROLLED", spearman=0.2, excludes_zero=True, span=0.99),
        _side(source="HISTORICAL_MARKET", spearman=0.2, excludes_zero=True, span=0.30),
    )
    assert state is verdict.Disagreement.SCORE_RANGE_MISMATCH


def test_a_cost_survival_mismatch_is_named() -> None:
    state, _ = compare.classify_disagreement(
        _side(source="SYNTHETIC_CONTROLLED", spearman=0.2, excludes_zero=True,
              survives=True),
        _side(source="HISTORICAL_MARKET", spearman=0.2, excludes_zero=True,
              survives=False),
    )
    assert state is verdict.Disagreement.COST_SURVIVAL_MISMATCH


def test_one_missing_side_is_not_a_disagreement() -> None:
    state, detail = compare.classify_disagreement(
        _side(source="SYNTHETIC_CONTROLLED", spearman=0.2, excludes_zero=True), None
    )
    assert state is verdict.Disagreement.NOT_COMPARABLE
    assert "only one source" in detail


def test_comparing_across_analysis_versions_is_refused() -> None:
    """Otherwise a methodology change reads as a market difference."""
    with pytest.raises(ValueError, match="analysis versions differ"):
        compare.compare(
            synthetic={},
            historical={},
            synthetic_versions=verdict.AnalysisVersions(),
            historical_versions=verdict.AnalysisVersions(cost_model=99),
        )


def test_the_two_sources_are_never_merged_into_one_number() -> None:
    """Both sides stay addressable on every row."""
    rows = compare.compare(
        synthetic={
            "a": _side(source="SYNTHETIC_CONTROLLED", spearman=0.2, excludes_zero=True)
        },
        historical={
            "a": _side(source="HISTORICAL_MARKET", spearman=0.02, excludes_zero=False)
        },
        synthetic_versions=verdict.AnalysisVersions(),
        historical_versions=verdict.AnalysisVersions(),
    )
    assert len(rows) == 1
    assert rows[0].synthetic is not None and rows[0].historical is not None
    assert rows[0].synthetic.source_type != rows[0].historical.source_type


# --- sufficiency ------------------------------------------------------------


def test_no_historical_evidence_is_distinct_from_too_little() -> None:
    none_at_all, _ = expansion.historical_sufficiency(
        role=expansion.StrategyRole.ALPHA_BREAKOUT, raw_n=0, effective_n=0
    )
    some, _ = expansion.historical_sufficiency(
        role=expansion.StrategyRole.ALPHA_BREAKOUT, raw_n=500, effective_n=40
    )
    enough, _ = expansion.historical_sufficiency(
        role=expansion.StrategyRole.ALPHA_BREAKOUT, raw_n=5000, effective_n=900
    )
    assert none_at_all is verdict.HistoricalSufficiency.NO_HISTORICAL_EVIDENCE
    assert some is verdict.HistoricalSufficiency.MORE_HISTORICAL_DATA_REQUIRED
    assert enough is verdict.HistoricalSufficiency.HISTORICAL_EVIDENCE_SUFFICIENT


def test_a_veto_is_not_asked_for_historical_directional_evidence() -> None:
    state, _ = expansion.historical_sufficiency(
        role=expansion.StrategyRole.RISK_FILTER, raw_n=0, effective_n=0
    )
    assert state is verdict.HistoricalSufficiency.NOT_APPLICABLE


# --- analysis versioning ----------------------------------------------------


def test_every_versioned_input_travels_with_a_verdict() -> None:
    versions = verdict.AnalysisVersions()
    assert versions.analysis == verdict.ANALYSIS_VERSION
    for field_name in (
        "horizon_registry", "cost_model", "clustering_rule", "bootstrap",
        "classification", "generator", "partition", "normalization",
    ):
        assert isinstance(getattr(versions, field_name), int)


def test_the_classification_version_moved_with_the_taxonomy() -> None:
    """v1 was the overloaded taxonomy; results are not comparable across it."""
    assert verdict.AnalysisVersions().classification == 2


# --- end to end -------------------------------------------------------------


def test_the_whole_historical_pipeline_runs_over_local_files(tmp_path: Path) -> None:
    """Import, validate, partition, seal, evaluate -- on constructed bars.

    Proves the wiring, not the market. These bars are a fixture: they are
    never reported as historical evidence, and the run they produce is thrown
    away with the temp directory.

    The value of the test is that the same `observe_frame` and `build_report`
    the synthetic run uses are reached through the historical path, so a
    difference between the two sources cannot come from two analysis
    implementations drifting apart.
    """
    directory = historical.data_directory(tmp_path)
    start = datetime(2024, 1, 2, 0, tzinfo=UTC)
    for i in range(4):
        _write_csv(
            directory,
            f"probe_{i}.csv",
            _clean_rows(240, start=start + timedelta(days=40 * i)),
            HEADER,
        )

    result = historical_run.run(
        root=tmp_path,
        instrument="XAUUSD",
        timeframe="1h",
        provider="constructed-test-fixture",
        acquisition_method="generated by the test suite",
        licensing_note="not market data; fixture only",
    )

    assert result.source_type == "HISTORICAL_MARKET"
    assert result.status == "COMPLETE"
    assert len(result.reports) == len(strategy_keys())

    # The partition happened, and TEST is sealed and was never evaluated.
    sealed = [d for d in result.datasets if d.seal]
    assert sealed, "a sealed TEST partition must exist"
    trained_on = {d.dataset_id for d in result.datasets if d.partition == "TRAIN"}
    for report in result.reports:
        assert set(report.by_dataset) <= trained_on, (
            f"{report.strategy_id} produced observations outside TRAIN"
        )

    # Provenance survived into the record for every dataset.
    for record in result.datasets:
        assert len(record.original_file_hash) == 64
        assert record.provider == "constructed-test-fixture"
        assert record.cost_basis == "ESTIMATED_COSTS"

    # Every strategy got a sufficiency answer and a verdict from the new
    # taxonomy rather than the overloaded one.
    valid = {v.value for v in verdict.CalibrationVerdict}
    for report in result.reports:
        assert report.readiness in valid
        assert result.sufficiency[report.strategy_id] in {
            s.value for s in verdict.HistoricalSufficiency
        }

    assert result.analysis_versions == verdict.AnalysisVersions()
    assert result.bars_per_second > 0


def strategy_keys() -> list[str]:
    from vantage_quant import strategies

    return sorted(strategies.REGISTRY)


# --- the minimal research view ----------------------------------------------


def test_the_dataset_view_reports_quality_and_the_seal(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch, capsys: pytest.CaptureFixture[str]
) -> None:
    """Section 47: a text table, not a frontend milestone.

    It must surface the facts a reader needs to decide whether to trust a
    conclusion: status, coverage, hash, warnings, and which partition is
    sealed away from this milestone.
    """
    from vantage_quant.research import __main__ as cli

    directory = historical.data_directory(tmp_path)
    start = datetime(2024, 1, 2, 0, tzinfo=UTC)
    for i in range(4):
        _write_csv(
            directory, f"probe_{i}.csv",
            _clean_rows(60, start=start + timedelta(days=30 * i)), HEADER,
        )
    monkeypatch.setattr(
        historical, "data_directory", lambda root=None: directory
    )

    assert cli.main(["datasets"]) == 0
    out = capsys.readouterr().out

    assert "probe_0" in out and "probe_3" in out
    assert "VALID_WITH_WARNINGS" in out
    assert "TRAIN" in out and "TEST" in out
    assert "SEALED_FOR_OUT_OF_SAMPLE" in out
    assert "not read by this milestone" in out
    assert "sha256(raw)=" in out
    assert "costs=ESTIMATED_COSTS" in out
    assert "TIMEZONE_ASSUMED" in out


def test_the_view_says_what_is_needed_when_nothing_is_present(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch, capsys: pytest.CaptureFixture[str]
) -> None:
    from vantage_quant.research import __main__ as cli

    directory = historical.data_directory(tmp_path)
    directory.mkdir(parents=True)
    monkeypatch.setattr(historical, "data_directory", lambda root=None: directory)

    assert cli.main(["datasets"]) == 0
    out = capsys.readouterr().out
    assert "WAITING_FOR_HISTORICAL_DATA" in out
    assert "minimum bars" in out


def test_the_historical_command_does_not_fail_hard_without_data(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch, capsys: pytest.CaptureFixture[str]
) -> None:
    """"No data yet" is a legitimate state, not an error to paper over."""
    from vantage_quant.research import __main__ as cli

    directory = historical.data_directory(tmp_path)
    directory.mkdir(parents=True)
    monkeypatch.setattr(historical, "data_directory", lambda root=None: directory)

    assert cli.main(["historical"]) == 0
    assert "WAITING_FOR_HISTORICAL_DATA" in capsys.readouterr().out


def test_the_parallel_and_serial_paths_agree(tmp_path: Path) -> None:
    """Workers must not change a result, only how long it takes.

    The pool re-imports each dataset by name inside the worker rather than
    receiving a pickled frame, so this also checks that the re-read reproduces
    what the parent validated.
    """
    directory = historical.data_directory(tmp_path)
    start = datetime(2024, 1, 2, 0, tzinfo=UTC)
    for i in range(2):
        _write_csv(
            directory, f"probe_{i}.csv",
            _clean_rows(200, start=start + timedelta(days=40 * i)), HEADER,
        )

    serial = historical_run.run(root=tmp_path, workers=1)
    parallel = historical_run.run(root=tmp_path, workers=4)

    assert [r.raw_n for r in serial.reports] == [r.raw_n for r in parallel.reports]
    assert [r.effective_n for r in serial.reports] == [
        r.effective_n for r in parallel.reports
    ]
    assert [r.readiness for r in serial.reports] == [
        r.readiness for r in parallel.reports
    ]
    assert [r.score_span_used for r in serial.reports] == [
        r.score_span_used for r in parallel.reports
    ]


# --- the manifest boundary ---------------------------------------------------


def test_an_exported_manifest_decides_the_source_type(tmp_path: Path) -> None:
    """The defect this exists for.

    This importer labelled everything in the research directory
    HISTORICAL_MARKET. That is right for a file an operator placed there and
    WRONG for a snapshot Vantage exported from its own generated bars -- which
    is exactly what the market-data layer now writes there. A directory is not
    evidence of a source type.
    """
    import json as json_module

    directory = historical.data_directory(tmp_path)
    _write_csv(directory, "exported.csv", _clean_rows(60), HEADER)
    (directory / "exported.manifest.json").write_text(
        json_module.dumps(
            {
                "source_type": "SYNTHETIC_CONTROLLED",
                "provider": "mock",
                "dataset_hash": "abc123",
                "timezone": "UTC",
            }
        ),
        encoding="utf-8",
    )

    loaded = historical.load(
        "exported.csv", instrument="XAUUSD", timeframe="1h", root=tmp_path
    )
    assert loaded.provenance.source_type is datasets.SourceType.SYNTHETIC_CONTROLLED
    assert loaded.provenance.provider == "mock"
    assert "Vantage market-data store" in loaded.provenance.acquisition_method


def test_a_file_with_no_manifest_is_still_treated_as_operator_supplied(
    tmp_path: Path,
) -> None:
    """The hand-placed workflow is unchanged: the caller declares provenance."""
    directory = historical.data_directory(tmp_path)
    _write_csv(directory, "byhand.csv", _clean_rows(60), HEADER)
    loaded = historical.load(
        "byhand.csv", instrument="XAUUSD", timeframe="1h", root=tmp_path,
        provider="an-operator-stated-provider",
    )
    assert loaded.provenance.source_type is datasets.SourceType.HISTORICAL_MARKET
    assert loaded.provenance.provider == "an-operator-stated-provider"


def test_an_unrecognised_source_type_is_refused_rather_than_defaulted(
    tmp_path: Path,
) -> None:
    """Neither guess is safe, so neither is made.

    Defaulting to HISTORICAL_MARKET is how generated bars become real
    evidence; defaulting to SYNTHETIC_CONTROLLED discards real observations.
    """
    import json as json_module

    directory = historical.data_directory(tmp_path)
    _write_csv(directory, "weird.csv", _clean_rows(60), HEADER)
    (directory / "weird.manifest.json").write_text(
        json_module.dumps({"source_type": "SOMETHING_ELSE"}), encoding="utf-8"
    )
    with pytest.raises(historical.MalformedDatasetError, match="refusing to guess"):
        historical.load("weird.csv", instrument="X", timeframe="1h", root=tmp_path)


def test_a_malformed_manifest_is_ignored_rather_than_half_applied(
    tmp_path: Path,
) -> None:
    """A manifest contributing a provider but not a source type would be worse
    than none: partial provenance reads as complete provenance."""
    directory = historical.data_directory(tmp_path)
    _write_csv(directory, "broken.csv", _clean_rows(60), HEADER)
    (directory / "broken.manifest.json").write_text("{not json", encoding="utf-8")

    loaded = historical.load(
        "broken.csv", instrument="XAUUSD", timeframe="1h", root=tmp_path,
        provider="declared-by-caller",
    )
    assert loaded.provenance.source_type is datasets.SourceType.HISTORICAL_MARKET
    assert loaded.provenance.provider == "declared-by-caller"
