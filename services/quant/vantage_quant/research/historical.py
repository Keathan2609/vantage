"""The seam for real market data, and the reason it is only a seam today.

Synthetic data proves the pipeline works and the strategies respond. It cannot
establish predictive value: the generator and the strategy share a model of
what a trend is, so a trend strategy scoring well on a generated trend partly
measures that agreement. Every claim about real edge needs real bars.

None are available locally, and this milestone does not go looking. The brief
is explicit and so is CLAUDE.md rule 3: no downloads, no broker, no scraping,
no third-party host. So the adapter is built, it reads from an allowlisted
local directory, and the status it reports today is honest:
REAL_MARKET_VALIDATION_PENDING.

Provenance is mandatory rather than encouraged. A historical dataset with no
recorded source, no original file hash and no record of what was done to it
cannot support a claim, because nobody can check it later.
"""

from __future__ import annotations

import hashlib
from dataclasses import dataclass, field
from datetime import UTC, datetime
from pathlib import Path

import pandas as pd

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


class HistoricalDataUnavailableError(RuntimeError):
    """No legitimate local historical data is present.

    Raised rather than returning an empty frame: an empty frame flows onward
    and produces a report full of zeroes that reads like a measurement.
    """


class UnsafeDatasetPathError(ValueError):
    """A requested dataset is outside the allowlisted directory."""


@dataclass(frozen=True)
class Provenance:
    """Where a historical dataset came from and what was done to it."""

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
    transformations: tuple[str, ...] = field(default_factory=tuple)


@dataclass(frozen=True)
class HistoricalDataset:
    dataset_id: str
    frame: pd.DataFrame
    provenance: Provenance


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
    name: str, *, instrument: str, timeframe: str, root: Path | None = None
) -> HistoricalDataset:
    """Read and normalise one local historical dataset.

    Normalisation is recorded, not assumed: every transformation performed is
    listed on the provenance so a later reader can tell a UTC conversion from
    a timezone that was already UTC.
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

    original_hash = hashlib.sha256(path.read_bytes()).hexdigest()[:32]
    raw = (
        pd.read_parquet(path)
        if path.suffix.lower() == ".parquet"
        else pd.read_csv(path)
    )

    transformations: list[str] = []
    frame, original_tz = _normalise(raw, transformations)

    missing = [c for c in REQUIRED_COLUMNS if c not in frame.columns]
    if missing:
        raise HistoricalDataUnavailableError(
            f"{name!r} is missing required column(s) {missing}; a bar series "
            f"without them cannot be scored"
        )

    normalized_hash = hashlib.sha256(
        frame[list(REQUIRED_COLUMNS)].to_csv(float_format="%.6f").encode()
    ).hexdigest()[:32]

    return HistoricalDataset(
        dataset_id=path.stem,
        frame=frame,
        provenance=Provenance(
            source=str(path.name),
            source_type=SourceType.HISTORICAL_MARKET,
            instrument=instrument,
            timeframe=timeframe,
            start=frame.index[0].to_pydatetime(),
            end=frame.index[-1].to_pydatetime(),
            bars=len(frame),
            ingested_at=datetime.now(UTC),
            original_file_hash=original_hash,
            normalized_dataset_hash=normalized_hash,
            original_timezone=original_tz,
            transformations=tuple(transformations),
        ),
    )


def _normalise(
    raw: pd.DataFrame, transformations: list[str]
) -> tuple[pd.DataFrame, str]:
    """Lower-case columns, index by UTC timestamp, sort, de-duplicate."""
    frame = raw.copy()
    if list(frame.columns) != [c.lower() for c in frame.columns]:
        frame.columns = [str(c).lower() for c in frame.columns]
        transformations.append("lower-cased column names")

    time_column = next(
        (c for c in ("timestamp", "time", "date", "datetime") if c in frame.columns),
        None,
    )
    if time_column is None:
        raise HistoricalDataUnavailableError(
            "no timestamp column found; one of timestamp/time/date/datetime is required"
        )

    parsed = pd.to_datetime(frame[time_column], utc=False, format="mixed")
    original_tz = str(parsed.dt.tz) if parsed.dt.tz is not None else "naive"
    if parsed.dt.tz is None:
        parsed = parsed.dt.tz_localize(UTC)
        transformations.append("localised naive timestamps to UTC")
    else:
        parsed = parsed.dt.tz_convert(UTC)
        transformations.append(f"converted {original_tz} to UTC")

    frame = frame.drop(columns=[time_column])
    frame.index = pd.DatetimeIndex(parsed, name="timestamp")

    if not frame.index.is_monotonic_increasing:
        frame = frame.sort_index()
        transformations.append("sorted by timestamp")

    duplicates = int(frame.index.duplicated().sum())
    if duplicates:
        frame = frame[~frame.index.duplicated(keep="first")]
        transformations.append(f"dropped {duplicates} duplicate timestamp(s)")

    return frame, original_tz
