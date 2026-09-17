"""Which datasets may be looked at, and which must not.

The fourteen replay fixtures occupy non-overlapping date ranges, so a
chronological partition is a straight split of an ordered list. That matters:
the assignment is a RULE applied to the ordering, not a choice made after
seeing which datasets would suit which strategy. Choosing the split to give a
family the conditions it likes is the selection bias the split exists to
prevent.

TEST is sealed. This module is the only place that knows which datasets are in
it, it refuses to hand them out for exploratory work, and a test asserts the
analysis path cannot reach them. The seal is recorded by hash so that a dataset
swapped underneath a sealed name is detectable rather than silent.
"""

from __future__ import annotations

import hashlib
from dataclasses import dataclass
from enum import StrEnum
from pathlib import Path


class Partition(StrEnum):
    TRAIN = "TRAIN"
    VALIDATION = "VALIDATION"
    TEST = "TEST"


#: Bumped if the split RULE changes. The assignment itself is derived, so a
#: new fixture lands wherever its dates put it without a decision.
PARTITION_VERSION = 1

#: Fractions of the ordered dataset list. Chronological, so the earliest half
#: is explored, the next quarter selects, and the last quarter is untouched.
TRAIN_FRACTION = 0.50
VALIDATION_FRACTION = 0.25


@dataclass(frozen=True)
class DatasetAssignment:
    dataset_id: str
    partition: Partition
    first_bar: str
    last_bar: str
    bars: int
    dataset_hash: str
    #: SEALED_FOR_OUT_OF_SAMPLE marks a dataset that may not be analysed in
    #: this milestone at all.
    seal: str = ""


class SealedDatasetError(RuntimeError):
    """Raised when sealed data is requested for exploratory analysis.

    An exception rather than an empty result: silently returning nothing would
    let an analysis believe it had looked and found no signal, which is worse
    than failing.
    """


def dataset_hash(path: Path) -> str:
    """Content hash of a fixture, so a swap under a stable name is visible."""
    return hashlib.sha256(path.read_bytes()).hexdigest()[:32]


def assign(datasets: list[tuple[str, str, str, int, Path]]) -> list[DatasetAssignment]:
    """Assign every dataset chronologically.

    ``datasets`` is (id, first_bar, last_bar, bars, path). Sorted by first bar
    here rather than trusting the caller's order, because an unsorted input
    would produce a partition that is not chronological while looking like one.
    """
    ordered = sorted(datasets, key=lambda d: d[1])
    n = len(ordered)
    train_end = int(n * TRAIN_FRACTION)
    validation_end = train_end + int(n * VALIDATION_FRACTION)

    out: list[DatasetAssignment] = []
    for i, (dataset_id, first, last, bars, path) in enumerate(ordered):
        if i < train_end:
            part = Partition.TRAIN
        elif i < validation_end:
            part = Partition.VALIDATION
        else:
            part = Partition.TEST
        out.append(
            DatasetAssignment(
                dataset_id=dataset_id,
                partition=part,
                first_bar=first,
                last_bar=last,
                bars=bars,
                dataset_hash=dataset_hash(path),
                seal="SEALED_FOR_OUT_OF_SAMPLE" if part is Partition.TEST else "",
            )
        )
    return out


def explorable(assignments: list[DatasetAssignment]) -> list[DatasetAssignment]:
    """The datasets this milestone is permitted to analyse.

    TRAIN only. VALIDATION exists for the policy milestone that selects between
    candidates, and opening it here would spend a holdout on exploration.
    """
    return [a for a in assignments if a.partition is Partition.TRAIN]


def selectable(assignments: list[DatasetAssignment]) -> list[DatasetAssignment]:
    """TRAIN plus VALIDATION, for the later milestone that selects a policy."""
    return [
        a for a in assignments if a.partition in (Partition.TRAIN, Partition.VALIDATION)
    ]


def sealed(assignments: list[DatasetAssignment]) -> list[DatasetAssignment]:
    """The untouched holdout. Returned for RECORDING, never for analysis."""
    return [a for a in assignments if a.partition is Partition.TEST]


def guard_not_sealed(assignments: list[DatasetAssignment]) -> None:
    """Refuse a set that contains sealed data.

    Called by every analysis entry point. The check is here rather than at each
    call site so that adding a new report cannot forget it.
    """
    breached = [a.dataset_id for a in assignments if a.partition is Partition.TEST]
    if breached:
        raise SealedDatasetError(
            f"{len(breached)} sealed dataset(s) reached an exploratory analysis: "
            f"{sorted(breached)}. TEST is held for the out-of-sample evaluation "
            f"in a later milestone; looking at it now would spend the holdout "
            f"and every later claim of 'out of sample' would be false."
        )
