"""A replay fixture must be long enough for what it is used to prove.

The strategies declare how much history they need. A replay warms up for 60
instants before any executable intent is permitted. A fixture therefore carries
strategy evidence only if it has enough instants to reach whichever of the
warm-up and the history requirement binds, plus a margin beyond it, and four of
the fourteen do not -- on those, no PAPER strategy
can produce a signal at any point in the dataset.

That is fine. They exist for execution and recovery scenarios: a partial fill,
a spread spike, a drawdown halt, a false breakout at the venue. It stops being
fine the moment somebody reads one of their runs as evidence about strategies,
which is exactly the mistake that produced "no strategy fires on a clean
trend" -- a conclusion drawn from 280 of 405 signals that could not have been
anything else.

So each fixture is classified here, with its reason, and a new one that is too
short must be named or the suite fails. The requirement is read from the
registry rather than written down, because a second copy of 120 is a number
that drifts from the code it describes.
"""

from __future__ import annotations

import csv
from pathlib import Path

import pytest

from vantage_quant import strategies as strat

#: replay.DefaultWarmupInstants. Declared on the Go side; if it changes there
#: and not here, `test_the_warmup_assumption_is_stated` says so in prose rather
#: than letting this suite quietly measure the wrong window.
WARMUP_INSTANTS = 60

#: The strategies the development seed promotes to PAPER -- the only ones an
#: autonomous replay can act on. Kept in step with internal/seed/seed.go.
PAPER_STRATEGIES = (
    "ma_trend_crossover",
    "donchian_breakout",
    "rsi_mean_reversion",
    "macd_momentum",
    "bollinger_zscore_reversion",
)

#: How many evaluable instants a fixture needs beyond the point where the
#: hungriest strategy can first speak, before its output means anything.
#:
#: Twenty is not a statistical claim. It is the smallest window in which a
#: crossover, a channel break and a histogram expansion can all plausibly
#: occur, so a fixture that clears it MIGHT show something. One that does not
#: clear it definitely cannot.
MIN_USABLE_INSTANTS = 20

#: Fixtures that deliberately carry no strategy evidence, and why.
#:
#: Each is here because its scenario is about what happens AFTER an order
#: exists -- the venue's behaviour, the account's limits, recovery from a
#: crash. Lengthening them would not improve those scenarios and would slow
#: every suite that plays them.
EXECUTION_ONLY: dict[str, str] = {
    "drawdown": "Scenario about the account halting on loss, driven by injected orders.",
    "false_breakout": "Scenario about a venue-side reversal after entry, not about entry.",
    "spread_spike": "Scenario about refusing to trade on a widened spread.",
    "test_partial_fill": "Scenario about booking one partial fill exactly once.",
}


def fixtures_dir() -> Path:
    here = Path(__file__).resolve()
    for parent in here.parents:
        candidate = parent / "services" / "control-api" / "internal" / "replay" / "fixtures"
        if candidate.is_dir():
            return candidate
    pytest.skip("the replay fixtures are not present in this checkout")


def instants(path: Path) -> int:
    """Distinct timestamps. A multi-instrument fixture has several rows each."""
    with path.open(newline="") as handle:
        return len({row["timestamp"] for row in csv.DictReader(handle)})


def hungriest() -> int:
    return max(strat.get(key).required_bars for key in PAPER_STRATEGIES)


def first_speakable_instant() -> int:
    """The earliest instant at which the hungriest PAPER strategy can answer.

    The warm-up and the history requirement OVERLAP rather than adding: bars
    accumulate throughout the warm-up, so at instant 60 there are sixty bars of
    history already. The binding constraint is whichever is larger.

    Written out because the first version of this suite added them, decided
    every fixture was too short, and would have had someone lengthening
    fourteen files to fix arithmetic.
    """
    return max(WARMUP_INSTANTS, hungriest())


def all_fixtures() -> list[Path]:
    return sorted(fixtures_dir().glob("*.csv"))


def test_the_fixtures_are_present() -> None:
    """Guards the guard: a path change must not turn this suite into a no-op
    that passes by finding nothing to check."""
    found = all_fixtures()
    assert len(found) >= 10, f"only {len(found)} fixtures found; the path is probably wrong"


@pytest.mark.parametrize("path", all_fixtures(), ids=lambda p: p.stem)
def test_a_fixture_is_long_enough_or_declared_execution_only(path: Path) -> None:
    usable = instants(path) - first_speakable_instant()

    if path.stem in EXECUTION_ONLY:
        # Declared short. Assert it really IS short, so an entry cannot outlive
        # the fixture it describes and quietly exempt a usable one.
        assert usable < MIN_USABLE_INSTANTS, (
            f"{path.stem} is declared execution-only but has {usable} usable "
            f"instants. Remove it from EXECUTION_ONLY: a fixture that can carry "
            f"strategy evidence should not be marked as unable to."
        )
        return

    assert usable >= MIN_USABLE_INSTANTS, (
        f"{path.stem} has {instants(path)} instants. The hungriest PAPER strategy "
        f"cannot answer before instant {first_speakable_instant()} -- the larger of "
        f"the {WARMUP_INSTANTS}-instant warm-up and its {hungriest()}-bar history "
        f"requirement, which overlap rather than add -- leaving {usable} usable, "
        f"under the {MIN_USABLE_INSTANTS} needed for its output to mean anything "
        f"about a strategy.\n\n"
        f"Either lengthen it -- each fixture needs its OWN date range, because a "
        f"strategy is evaluated once per completed bar for ever -- or add it to "
        f"EXECUTION_ONLY with the reason it carries no strategy evidence. What it "
        f"must not be is a short fixture whose empty run is read as a finding."
    )


def test_every_declared_exception_still_exists() -> None:
    """A stale entry is a silent exemption for a fixture that may since have
    been lengthened, or for one that no longer exists at all."""
    present = {p.stem for p in all_fixtures()}
    stale = sorted(set(EXECUTION_ONLY) - present)
    assert not stale, f"EXECUTION_ONLY names fixtures that are gone: {stale}"


def test_the_warmup_assumption_is_stated() -> None:
    """This suite measures a window defined by a constant that lives in Go.

    It cannot import it, so it asserts the shape of what it assumes: a warm-up
    that no longer left room for the hungriest strategy would make every
    threshold here meaningless, and the failure should say so rather than
    appearing as an unrelated fixture being too short.
    """
    assert WARMUP_INSTANTS > 0
    assert hungriest() > 0, "no PAPER strategy declares a history requirement"
    assert first_speakable_instant() + MIN_USABLE_INSTANTS < 400, (
        "the window this suite requires has grown past any fixture that could "
        "reasonably be committed; revisit MIN_USABLE_INSTANTS or the warm-up"
    )
