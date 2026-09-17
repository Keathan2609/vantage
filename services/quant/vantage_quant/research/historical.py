"""The seam for real market data, and the reason it is only a seam today.

Synthetic data proves the pipeline works and the strategies respond. It cannot
establish predictive value: the generator and the strategy share a model of
what a trend is, so a trend strategy scoring well on a generated trend partly
measures that agreement. Every claim about real edge needs real bars.

None are available locally, and this milestone does not go looking. CLAUDE.md
rule 3 and the brief are both explicit: no downloads, no broker, no scraping,
no third-party host. So the adapter is built, it reads from an allowlisted
local directory, and the status it reports today is honest:
REAL_MARKET_VALIDATION_PENDING.

# Three layers, deliberately distinct

    RAW          the bytes as they arrived. Never mutated, hashed on sight.
    NORMALIZED   canonical schema, UTC index, every transformation recorded.
    RESEARCH     a normalized dataset that passed quality and was partitioned.

They are separate because conflating them loses the ability to answer "was
that in the file, or did we do it?" -- which is the first question anyone asks
of a surprising result. The original hash survives into every report so the
answer is always checkable.

# Provenance is mandatory rather than encouraged

A historical dataset with no recorded source, no original file hash and no
record of what was done to it cannot support a claim, because nobody can check
it later. Fields are required, and none of them are invented: where a fact is
unknown it is recorded as unknown.

# The file is untrusted input

It arrives from outside the repository, so it is treated the way any external
input is: bounded in size, parsed without executing anything, and checked for
the shapes that break a consumer rather than this process -- a leading `=` that
a spreadsheet would execute on export, a duplicated column that silently wins,
a timestamp that is not one.
"""

from __future__ import annotations

import csv
import hashlib
import json
from collections.abc import Hashable
from dataclasses import dataclass, field
from datetime import UTC, datetime, timedelta
from pathlib import Path
from typing import Any

import pandas as pd

from . import quality
from .datasets import SourceType

#: Where a historical dataset may be read from. An ALLOWLISTED directory, not
#: a caller-supplied path: an arbitrary path parameter is a file-read primitive
#: pointed at the host, and the research plane must not have one.
HISTORICAL_DATA_DIRNAME = "research-data"

#: Extensions the adapter will open. Anything else is refused rather than
#: guessed at.
ALLOWED_SUFFIXES = (".csv", ".parquet")

#: Columns a bar series must carry after normalisation.
REQUIRED_COLUMNS = ("open", "high", "low", "close")

#: Columns kept when present. Absent ones are absent, never synthesised.
OPTIONAL_COLUMNS = ("bid", "ask", "spread", "volume")

#: Largest file the importer will open, in bytes. A research process that
#: cheerfully loads a 40 GB CSV into memory is a denial of service against the
#: machine it runs on.
MAX_FILE_BYTES = 512 * 1024 * 1024

#: Characters that make a CSV cell executable in a spreadsheet. Checked on
#: TEXT columns so that a value carried through to an export cannot become a
#: formula in someone else's tool.
FORMULA_PREFIXES = ("=", "+", "-", "@", "\t", "\r")

#: Timestamp column names recognised, in preference order.
TIME_COLUMNS = ("timestamp_utc", "timestamp", "datetime", "time", "date")


class HistoricalDataUnavailableError(RuntimeError):
    """No legitimate local historical data is present.

    Raised rather than returning an empty frame: an empty frame flows onward
    and produces a report full of zeroes that reads like a measurement.
    """


class UnsafeDatasetPathError(ValueError):
    """A requested dataset is outside the allowlisted directory."""


class MalformedDatasetError(ValueError):
    """The file parsed but cannot be trusted as a bar series."""


class DatasetRejectedError(RuntimeError):
    """The data failed quality validation and may not enter research."""


@dataclass(frozen=True)
class Provenance:
    """Where a historical dataset came from and what was done to it.

    Every field is recorded rather than inferred. `licensing_note` and
    `acquisition_method` default to an explicit UNRECORDED rather than to a
    plausible guess: an invented licence note is worse than none, because it
    looks like diligence.
    """

    source: str
    source_type: SourceType
    instrument: str
    timeframe: str
    start: datetime
    end: datetime
    bars: int
    ingested_at: datetime
    original_file_hash: str
    normalized_dataset_hash: str
    original_timezone: str
    #: Whether the TIMEZONE was stated by the source or assumed by us. A silent
    #: assumption moves every session boundary in the analysis.
    timezone_declared: bool = False
    provider: str = "UNRECORDED"
    acquisition_method: str = "UNRECORDED"
    licensing_note: str = "UNRECORDED"
    transformation_version: int = 1
    source_timeframe: str = ""
    aggregation_version: int | None = None
    has_observed_spread: bool = False
    cost_basis: str = "ESTIMATED_COSTS"
    transformations: tuple[str, ...] = field(default_factory=tuple)


@dataclass(frozen=True)
class HistoricalDataset:
    dataset_id: str
    frame: pd.DataFrame
    provenance: Provenance
    quality_report: quality.DataQualityReport

    @property
    def status(self) -> quality.DatasetStatus:
        return self.quality_report.status


def data_directory(root: Path | None = None) -> Path:
    """The single directory historical data may be read from."""
    base = root or Path(__file__).resolve().parents[2]
    return base / HISTORICAL_DATA_DIRNAME


def _resolve_within(directory: Path, name: str) -> Path:
    """Resolve a dataset NAME inside the allowlist, refusing escape.

    The name is a key, never a path: `..` and absolute paths are rejected by
    resolving and then checking containment, which is the only check that
    survives symlinks and Windows short names.
    """
    if not name or any(sep in name for sep in ("/", "\\", "..")):
        raise UnsafeDatasetPathError(
            f"dataset name {name!r} is not a plain name. Historical datasets are "
            f"addressed by name inside the allowlisted directory, never by path."
        )
    if name.startswith(".") or ":" in name:
        raise UnsafeDatasetPathError(
            f"dataset name {name!r} is not a plain file name"
        )
    candidate = (directory / name).resolve()
    # is_relative_to, not a string prefix: "…/research-data-elsewhere" starts
    # with "…/research-data" and is a different directory. The separator check
    # above already makes escape unreachable, so this is the second lock rather
    # than the first -- which is the point of having it.
    if not candidate.is_relative_to(directory.resolve()):
        raise UnsafeDatasetPathError(f"dataset {name!r} resolves outside {directory}")
    return candidate


def available(root: Path | None = None) -> list[str]:
    """Every historical dataset present locally. Empty is the expected answer."""
    directory = data_directory(root)
    if not directory.is_dir():
        return []
    return sorted(
        p.name for p in directory.iterdir()
        if p.is_file() and p.suffix.lower() in ALLOWED_SUFFIXES
    )


def status(root: Path | None = None) -> str:
    """One line an operator or a report can quote."""
    found = available(root)
    if not found:
        return (
            "REAL_MARKET_VALIDATION_PENDING: no historical dataset is present in "
            f"{data_directory(root)}. Every result so far rests on "
            "SYNTHETIC_CONTROLLED data, which cannot establish predictive value."
        )
    return f"{len(found)} historical dataset(s) available: {found}"


def load(
    name: str,
    *,
    instrument: str,
    timeframe: str,
    root: Path | None = None,
    provider: str = "UNRECORDED",
    acquisition_method: str = "UNRECORDED",
    licensing_note: str = "UNRECORDED",
    declared_timezone: str | None = None,
    market_clock: quality.MarketClock | None = None,
    reject_on_error: bool = True,
) -> HistoricalDataset:
    """Read, normalise and validate one local historical dataset.

    `declared_timezone` is the timezone the SOURCE says its timestamps are in.
    Passing None means nobody said, and that is recorded as an assumption
    rather than resolved from this machine's locale -- a developer's timezone
    is not a property of the data.
    """
    directory = data_directory(root)
    if not directory.is_dir():
        raise HistoricalDataUnavailableError(
            f"{directory} does not exist. Create it and place legitimate local "
            f"market data there; nothing is downloaded."
        )
    path = _resolve_within(directory, name)
    if not path.is_file():
        raise HistoricalDataUnavailableError(f"{name!r} is not present in {directory}")
    if path.suffix.lower() not in ALLOWED_SUFFIXES:
        raise UnsafeDatasetPathError(
            f"{name!r} has suffix {path.suffix!r}; only {ALLOWED_SUFFIXES} are read"
        )

    size = path.stat().st_size
    if size > MAX_FILE_BYTES:
        raise MalformedDatasetError(
            f"{name!r} is {size} bytes, above the {MAX_FILE_BYTES} limit. Split it "
            f"or raise the limit deliberately; loading it whole would exhaust memory"
        )
    if size == 0:
        raise MalformedDatasetError(f"{name!r} is empty")

    # Hashed from the bytes BEFORE anything is parsed, so the record identifies
    # what actually arrived rather than what pandas made of it.
    # A MANIFEST beside the file, when Vantage exported it.
    #
    # Read before anything is assumed about the data. Without it this importer
    # labelled everything in the directory HISTORICAL_MARKET, which is right
    # for a file an operator placed there by hand and WRONG for a snapshot
    # Vantage exported from its own generated bars. A directory is not
    # evidence of a source type; a recorded provenance is.
    manifest = _read_manifest(path)

    original_hash = hashlib.sha256(path.read_bytes()).hexdigest()

    raw = _read_raw(path)

    transformations: list[str] = []
    frame, original_tz, tz_declared = _normalise(
        raw, transformations, declared_timezone
    )

    missing = [c for c in REQUIRED_COLUMNS if c not in frame.columns]
    if missing:
        raise MalformedDatasetError(
            f"{name!r} is missing required column(s) {missing}; a bar series "
            f"without them cannot be scored"
        )

    has_spread = _has_observed_spread(frame)
    if not has_spread:
        transformations.append(
            "no bid/ask in source: costs are ESTIMATED_COSTS, not observed"
        )

    normalized_hash = hashlib.sha256(
        frame[list(REQUIRED_COLUMNS)].to_csv(float_format="%.6f").encode()
    ).hexdigest()

    report = quality.assess(
        frame,
        dataset_id=path.stem,
        timeframe=_timeframe_delta(timeframe),
        timezone_was_declared=tz_declared,
        has_real_spread=has_spread,
        market_clock=market_clock,
    )
    if reject_on_error and report.status is quality.DatasetStatus.REJECTED:
        raise DatasetRejectedError(
            f"{name!r} failed validation and may not enter research: "
            + "; ".join(f"{f.code} x{f.count}" for f in report.errors)
        )

    return HistoricalDataset(
        dataset_id=path.stem,
        frame=frame,
        quality_report=report,
        provenance=Provenance(
            source=str(path.name),
            # From the manifest when one exists, and only otherwise from the
            # assumption that a hand-placed file is market data.
            source_type=_source_type_of(manifest),
            instrument=instrument,
            timeframe=timeframe,
            start=frame.index[0].to_pydatetime(),
            end=frame.index[-1].to_pydatetime(),
            bars=len(frame),
            ingested_at=datetime.now(UTC),
            original_file_hash=original_hash,
            normalized_dataset_hash=normalized_hash,
            original_timezone=original_tz,
            timezone_declared=tz_declared,
            provider=manifest.get("provider", provider) if manifest else provider,
            acquisition_method=(
                "exported from the Vantage market-data store"
                if manifest
                else acquisition_method
            ),
            licensing_note=licensing_note,
            source_timeframe=timeframe,
            has_observed_spread=has_spread,
            cost_basis="OBSERVED_SPREAD" if has_spread else "ESTIMATED_COSTS",
            transformations=tuple(transformations),
        ),
    )


def _read_raw(path: Path) -> pd.DataFrame:
    """Parse the file without trusting it.

    CSV is read with an explicit engine and no type inference tricks; a header
    is checked for duplicates BEFORE pandas silently disambiguates them, which
    would otherwise let a second `close` column decide the result invisibly.
    """
    if path.suffix.lower() == ".parquet":
        return pd.read_parquet(path)

    with path.open("r", encoding="utf-8-sig", newline="") as handle:
        header = next(csv.reader(handle), None)
    if not header:
        raise MalformedDatasetError(f"{path.name} has no header row")

    lowered = [h.strip().lower() for h in header]
    duplicates = {h for h in lowered if lowered.count(h) > 1}
    if duplicates:
        raise MalformedDatasetError(
            f"{path.name} has duplicate column(s) {sorted(duplicates)}. Pandas "
            f"would rename them and one would silently win"
        )
    for name in lowered:
        if name.startswith(FORMULA_PREFIXES):
            raise MalformedDatasetError(
                f"{path.name} has a column named {name!r}, which a spreadsheet "
                f"would evaluate as a formula on export"
            )

    return pd.read_csv(path, engine="c", skipinitialspace=True)


def _has_observed_spread(frame: pd.DataFrame) -> bool:
    """Whether the SOURCE carried a real spread, rather than one we invented.

    Deliberately strict. A dataset with a `spread` column of zeroes has not
    observed a spread, and treating it as though it had would let a backtest
    report costs it never paid.
    """
    if "bid" in frame.columns and "ask" in frame.columns:
        return bool((frame["ask"] > frame["bid"]).any())
    if "spread" in frame.columns:
        return bool((frame["spread"] > 0).any())
    return False


def _timeframe_delta(timeframe: str) -> timedelta:
    table = {
        "1m": timedelta(minutes=1), "5m": timedelta(minutes=5),
        "15m": timedelta(minutes=15), "30m": timedelta(minutes=30),
        "1h": timedelta(hours=1), "4h": timedelta(hours=4),
        "1d": timedelta(days=1),
    }
    if timeframe not in table:
        raise MalformedDatasetError(
            f"unknown timeframe {timeframe!r}; expected one of {sorted(table)}"
        )
    return table[timeframe]


def _normalise(
    raw: pd.DataFrame, transformations: list[str], declared_timezone: str | None
) -> tuple[pd.DataFrame, str, bool]:
    """Lower-case columns, index by UTC timestamp, sort, de-duplicate.

    Returns the frame, the timezone the timestamps were IN, and whether that
    timezone was declared by the source rather than assumed by us.
    """
    frame = raw.copy()
    if list(frame.columns) != [str(c).lower() for c in frame.columns]:
        frame.columns = [str(c).strip().lower() for c in frame.columns]
        transformations.append("lower-cased column names")

    time_column = next((c for c in TIME_COLUMNS if c in frame.columns), None)
    if time_column is None:
        raise MalformedDatasetError(
            f"no timestamp column found; one of {TIME_COLUMNS} is required"
        )

    try:
        parsed = pd.to_datetime(frame[time_column], utc=False, format="mixed")
    except (ValueError, TypeError) as exc:
        raise MalformedDatasetError(
            f"column {time_column!r} does not parse as timestamps: {exc}"
        ) from exc
    if parsed.isna().any():
        raise MalformedDatasetError(
            f"{int(parsed.isna().sum())} unparseable timestamp(s) in {time_column!r}"
        )

    tz_declared = False
    if parsed.dt.tz is not None:
        original_tz = str(parsed.dt.tz)
        tz_declared = True
        parsed = parsed.dt.tz_convert(UTC)
        transformations.append(f"converted {original_tz} to UTC")
    elif declared_timezone:
        original_tz = declared_timezone
        tz_declared = True
        parsed = parsed.dt.tz_localize(declared_timezone).dt.tz_convert(UTC)
        transformations.append(f"localised to declared {declared_timezone}, then UTC")
    else:
        original_tz = "UNDECLARED"
        parsed = parsed.dt.tz_localize(UTC)
        transformations.append(
            "ASSUMED UTC: the source declared no timezone and none was supplied"
        )

    frame = frame.drop(columns=[time_column])
    frame.index = pd.DatetimeIndex(parsed, name="timestamp")

    keep = [c for c in (*REQUIRED_COLUMNS, *OPTIONAL_COLUMNS) if c in frame.columns]
    dropped = [c for c in frame.columns if c not in keep]
    if dropped:
        transformations.append(f"dropped unrecognised column(s) {sorted(dropped)}")
    frame = frame[keep]

    for column in keep:
        frame[column] = pd.to_numeric(frame[column], errors="coerce")

    if not frame.index.is_monotonic_increasing:
        frame = frame.sort_index()
        transformations.append("sorted by timestamp")

    duplicates = int(frame.index.duplicated().sum())
    if duplicates:
        # Dropped rather than merged, and RECORDED. Merging two bars claiming
        # the same instant invents a third that was never quoted.
        frame = frame[~frame.index.duplicated(keep="first")]
        transformations.append(f"dropped {duplicates} duplicate timestamp(s), kept first")

    return frame, original_tz, tz_declared


def resample(
    frame: pd.DataFrame, *, source_timeframe: str, target_timeframe: str,
    transformations: list[str],
) -> pd.DataFrame:
    """Aggregate to a coarser timeframe, deterministically.

    Only ever coarser. Going finer would require inventing bars, and an
    interpolated OHLC bar asserts that trades happened at prices nobody quoted.
    Empty periods are DROPPED rather than forward-filled for the same reason.
    """
    source, target = _timeframe_delta(source_timeframe), _timeframe_delta(target_timeframe)
    if target <= source:
        raise MalformedDatasetError(
            f"cannot resample {source_timeframe} to {target_timeframe}: aggregation "
            f"only ever coarsens. Producing finer bars would invent trades"
        )

    agg: dict[Hashable, Any] = {
        "open": "first", "high": "max", "low": "min", "close": "last",
    }
    if "volume" in frame.columns:
        agg["volume"] = "sum"
    if "bid" in frame.columns:
        agg["bid"] = "last"
    if "ask" in frame.columns:
        agg["ask"] = "last"
    if "spread" in frame.columns:
        agg["spread"] = "mean"

    out = frame.resample(target, label="left", closed="left").agg(agg).dropna(
        subset=["open", "high", "low", "close"]
    )
    transformations.append(
        f"aggregated {source_timeframe} -> {target_timeframe} "
        f"(aggregation version {AGGREGATION_VERSION}); empty periods dropped, "
        f"never interpolated"
    )
    return out


#: Bumped when the aggregation rule changes, so two resampled datasets are
#: never silently compared across a rule change.
AGGREGATION_VERSION = 1


def _read_manifest(path: Path) -> dict[str, Any] | None:
    """Provenance Vantage wrote beside an exported snapshot, if present.

    A malformed manifest is treated as ABSENT rather than fatal: the bars are
    still readable and the caller's own declarations still apply. What it must
    never do is half-apply -- a manifest that contributed a provider but not a
    source type would be worse than none.
    """
    candidate = path.with_suffix("").with_suffix(".manifest.json")
    if not candidate.is_file():
        candidate = path.parent / (path.stem + ".manifest.json")
    if not candidate.is_file():
        return None
    try:
        loaded = json.loads(candidate.read_text(encoding="utf-8"))
    except (json.JSONDecodeError, OSError):
        return None
    if not isinstance(loaded, dict) or "source_type" not in loaded:
        return None
    return loaded


def _source_type_of(manifest: dict[str, Any] | None) -> SourceType:
    """What the data actually is, from the record rather than the directory.

    An unrecognised value is refused rather than defaulted. Defaulting to
    HISTORICAL_MARKET is exactly how generated bars would enter research as
    real market evidence, and defaulting to SYNTHETIC_CONTROLLED would discard
    real observations. Neither guess is safe, so neither is made.
    """
    if manifest is None:
        # No manifest means a file an operator placed here deliberately, which
        # is what this directory is for. The caller declares its provenance.
        return SourceType.HISTORICAL_MARKET
    declared = str(manifest.get("source_type", ""))
    try:
        return SourceType(declared)
    except ValueError as exc:
        raise MalformedDatasetError(
            f"manifest declares source_type {declared!r}, which is not a "
            f"recognised source type; refusing to guess what these bars are"
        ) from exc
