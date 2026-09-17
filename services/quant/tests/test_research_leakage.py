"""The information boundary, proved rather than intended.

Every one of these guards the same thing from a different side: a strategy must
never see what happened after the bar it spoke on. A leak here would not look
like a bug -- it would look like a strategy that works, which is the most
expensive kind of mistake a research pipeline can make.
"""

from __future__ import annotations

import ast
from datetime import UTC, datetime
from pathlib import Path

import pandas as pd
import pytest

from vantage_quant import strategies
from vantage_quant.research import generate, horizons, outcome, partition, run
from vantage_quant.research.observation import SignalObservation


def _module_imports(path: Path) -> set[str]:
    """Every module a file imports, from its AST rather than by running it."""
    tree = ast.parse(path.read_text(encoding="utf-8"))
    found: set[str] = set()
    for node in ast.walk(tree):
        if isinstance(node, ast.Import):
            found.update(alias.name for alias in node.names)
        elif isinstance(node, ast.ImportFrom) and node.module:
            found.add(node.module)
            # A relative `from . import outcome` names the module in `names`.
            if node.level:
                found.update(f".{alias.name}" for alias in node.names)
    return found


def _research_dir() -> Path:
    import vantage_quant.research as pkg

    return Path(pkg.__file__).parent


def test_strategy_code_cannot_import_the_research_package() -> None:
    """The hard boundary.

    If strategies.py could import research code it could read a forward
    outcome, and a strategy that fits against its own future is not a strategy.
    """
    strategy_file = Path(strategies.__file__)
    imported = _module_imports(strategy_file)
    leaked = {m for m in imported if "research" in m or "outcome" in m}
    assert not leaked, (
        f"strategies.py imports {sorted(leaked)}. Strategy code must not be able "
        f"to reach forward outcomes: a signal fitted against its own future "
        f"looks like skill and is arithmetic."
    )


def test_observation_does_not_import_outcome() -> None:
    """The boundary inside the research package itself.

    An observation carries what was knowable when the signal was produced. If
    its module could reach the outcome module, a future field would eventually
    be added to the observation and nothing would notice.
    """
    imported = _module_imports(_research_dir() / "observation.py")
    leaked = {m for m in imported if "outcome" in m}
    assert not leaked, (
        f"observation.py imports {sorted(leaked)}. The observation type is the "
        f"point-in-time record; reaching the outcome module from it is how a "
        f"forward field gets added by accident."
    )


def test_no_observation_field_is_derived_from_the_future() -> None:
    """Every field on the observation must be knowable at the signal bar.

    Enumerated rather than inferred, so that adding a field is a decision
    somebody has to justify here.
    """
    allowed = {
        "observation_id", "strategy_id", "strategy_version", "strategy_family",
        "instrument", "direction", "bar_time", "raw_score", "confidence_kind",
        "score_components", "regime", "session", "spread_fraction",
        "trailing_volatility", "event_risk", "dataset_id", "dataset_hash",
        "replay_run_id", "code_sha", "evaluation_phase", "entry_reference",
    }
    actual = set(SignalObservation.__dataclass_fields__)
    added = actual - allowed
    assert not added, (
        f"SignalObservation gained field(s) {sorted(added)}. Every field must be "
        f"knowable at the signal's own bar. If it describes what happened next, "
        f"it belongs on SignalOutcome."
    )


def _observation(entry: float = 100.0, direction: str = "buy") -> SignalObservation:
    return SignalObservation(
        observation_id="probe",
        strategy_id="probe",
        strategy_version=1,
        strategy_family="trend_following",
        instrument="XAUUSD.m",
        direction=direction,  # type: ignore[arg-type]
        bar_time=datetime(2027, 3, 2, 4, tzinfo=UTC),
        raw_score=0.5,
        confidence_kind="raw_score",
        entry_reference=entry,
    )


def _bars(closes: list[float]) -> pd.DataFrame:
    return pd.DataFrame(
        {
            "open": closes,
            "high": [c * 1.001 for c in closes],
            "low": [c * 0.999 for c in closes],
            "close": closes,
            "volume": [1000.0] * len(closes),
        }
    )


def test_an_outcome_is_unavailable_before_its_horizon_elapses() -> None:
    """A truncated window is missing data, never a zero.

    Scoring what exists would silently shorten the horizon for every signal
    near a dataset's end, and signals near the end are not a random sample.
    """
    result = outcome.compute_outcome(
        _observation(),
        future_bars=_bars([101.0, 102.0]),
        horizon_id="short",
        horizon_bars=5,
        suggested_stop=None,
        suggested_target=None,
    )
    assert result is None, (
        "an outcome was produced from 2 bars over a 5-bar horizon. An "
        "incomplete window must be absent rather than scored short."
    )


def test_an_outcome_uses_only_bars_after_the_signal() -> None:
    """The window is the caller's slice, and it starts after the signal bar.

    Proved by construction: an outcome computed over bars that RISE must be
    positive for a buy, and the same bars offset by the signal's own bar would
    give a different answer. The run path slices `iloc[idx + 1:]`, and this
    asserts the function honours exactly what it is handed.
    """
    rising = _bars([101.0, 102.0, 103.0, 104.0, 105.0])
    result = outcome.compute_outcome(
        _observation(entry=100.0),
        future_bars=rising,
        horizon_id="short",
        horizon_bars=5,
        suggested_stop=None,
        suggested_target=None,
    )
    assert result is not None
    # 5% up, minus a round trip's spread and slippage.
    assert result.forward_raw_return == pytest.approx(0.05, abs=1e-3)
    assert result.forward_net_return < result.forward_raw_return, (
        "the net return is not below the raw return, so costs were not charged"
    )
    assert result.bars_observed == 5


def test_a_sell_outcome_is_positive_when_price_falls() -> None:
    """Direction normalisation, or a mixed sample cancels itself out."""
    falling = _bars([99.0, 98.0, 97.0, 96.0, 95.0])
    result = outcome.compute_outcome(
        _observation(entry=100.0, direction="sell"),
        future_bars=falling,
        horizon_id="short",
        horizon_bars=5,
        suggested_stop=None,
        suggested_target=None,
    )
    assert result is not None
    assert result.forward_raw_return > 0, (
        "a SELL followed by a fall produced a negative outcome; higher must "
        "mean better for both directions or the two sides cancel"
    )


def test_excursions_are_bounded_to_the_horizon() -> None:
    """MFE and MAE may not see past the window they describe."""
    inside = _bars([101.0, 102.0, 103.0, 104.0, 105.0])
    beyond = _bars([101.0, 102.0, 103.0, 104.0, 105.0, 200.0, 5.0])

    a = outcome.compute_outcome(
        _observation(), future_bars=inside, horizon_id="short", horizon_bars=5,
        suggested_stop=None, suggested_target=None,
    )
    b = outcome.compute_outcome(
        _observation(), future_bars=beyond, horizon_id="short", horizon_bars=5,
        suggested_stop=None, suggested_target=None,
    )
    assert a is not None and b is not None
    assert a.maximum_favorable_excursion == pytest.approx(
        b.maximum_favorable_excursion
    ), "a bar beyond the horizon changed the MFE, so the excursion is unbounded"
    assert a.maximum_adverse_excursion == pytest.approx(b.maximum_adverse_excursion)


def test_an_ambiguous_bar_resolves_to_the_stop() -> None:
    """Conservative, because intrabar order is unknowable from OHLC.

    Assuming the favourable leg on an ambiguous bar is the single easiest way
    to manufacture an edge that does not exist.
    """
    both = pd.DataFrame(
        {
            "open": [100.0], "high": [110.0], "low": [90.0],
            "close": [100.0], "volume": [1000.0],
        }
    )
    result = outcome.compute_outcome(
        _observation(entry=100.0),
        future_bars=both,
        horizon_id="short",
        horizon_bars=1,
        suggested_stop=95.0,
        suggested_target=105.0,
    )
    assert result is not None
    assert result.stop_hit is True and result.target_hit is False, (
        "a bar whose range contains both the stop and the target resolved to "
        "the target. Intrabar order is unknowable, so the conservative reading "
        "is the only honest one."
    )


def test_warmup_observations_are_excluded_from_analysis() -> None:
    warm = SignalObservation(
        **{**_observation().__dict__, "evaluation_phase": "warmup"}  # type: ignore[arg-type]
    )
    assert not warm.is_analysable(), "a warm-up observation entered the analysable set"
    assert _observation().is_analysable()


def test_the_sealed_test_partition_cannot_reach_an_exploratory_report() -> None:
    """The seal, enforced.

    An empty result would let an analysis believe it had looked and found
    nothing, which is worse than failing.
    """
    assignments = run.discover_datasets()
    sealed = partition.sealed(assignments)
    assert sealed, "no dataset is sealed, so there is no holdout at all"

    with pytest.raises(partition.SealedDatasetError):
        partition.guard_not_sealed(assignments)

    # And the explorable set really excludes them.
    explorable = partition.explorable(assignments)
    partition.guard_not_sealed(explorable)  # must not raise
    sealed_ids = {a.dataset_id for a in sealed}
    assert not ({a.dataset_id for a in explorable} & sealed_ids)


def test_the_run_only_reads_train_datasets() -> None:
    """End to end: no observation may carry a sealed or validation dataset."""
    result = run.run()
    train = {
        a.dataset_id for a in result.assignments if a.partition is partition.Partition.TRAIN
    }
    for report in result.reports:
        for dataset_id in report.by_dataset:
            assert dataset_id in train, (
                f"{report.strategy_id} produced an observation from {dataset_id!r}, "
                f"which is not in TRAIN. Exploratory analysis must not touch "
                f"VALIDATION or the sealed TEST set."
            )


def test_every_registered_strategy_appears_in_the_report() -> None:
    """No strategy silently disappears.

    A strategy that produced nothing must say so with a reason; absence from
    the report and 'produced nothing' are different facts.
    """
    result = run.run()
    reported = {r.strategy_id for r in result.reports}
    registered = set(strategies.REGISTRY)
    assert reported == registered, (
        f"missing from the report: {sorted(registered - reported)}; "
        f"unexpected: {sorted(reported - registered)}"
    )
    for report in result.reports:
        if report.observations == 0:
            assert report.note, (
                f"{report.strategy_id} produced no observations and gave no "
                f"reason. A silent zero is indistinguishable from a bug."
            )


def test_every_family_has_declared_horizons() -> None:
    """A family with no horizon has not been thought about."""
    for key, spec in sorted(strategies.REGISTRY.items()):
        declared = horizons.horizons_for(spec.family)
        assert declared, f"{key}: family {spec.family} declares no horizon"
        assert all(h.bars > 0 for h in declared)
        assert all(h.rationale for h in declared), (
            f"{key}: a horizon carries no rationale, so its length is arbitrary"
        )


def test_the_entry_reference_crosses_the_spread() -> None:
    """A fill is not the close, and pretending it is flatters every result."""
    costs = outcome.research_cost_model()
    buy = outcome.entry_fill_price(direction="buy", next_open=100.0, costs=costs)
    sell = outcome.entry_fill_price(direction="sell", next_open=100.0, costs=costs)
    assert buy > 100.0, "a buy filled at or below the mid, paying no spread"
    assert sell < 100.0, "a sell filled at or above the mid, paying no spread"


def test_generated_observations_never_see_their_own_future() -> None:
    """The generator's slice, checked directly.

    A strategy is handed `frame.iloc[.. : i + 1]`, so the last bar it sees is
    its own. This asserts the slice by construction: truncating the frame
    immediately after the signal bar must not change the signal.
    """
    spec = strategies.REGISTRY["macd_momentum"]
    assignments = {a.dataset_id: a for a in run.discover_datasets()}
    bars = generate.load_fixture(
        run.fixture_dir() / "trend_clean.csv", assignments["trend_clean"]
    )
    full = generate.observe_dataset(spec, bars, sha="test")
    assert full, "the fixture produced no signal, so this proves nothing"

    first = full[0].observation
    idx = bars.frame.index.get_indexer([first.bar_time])[0]

    truncated = generate.DatasetBars(
        dataset_id=bars.dataset_id,
        dataset_hash=bars.dataset_hash,
        frame=bars.frame.iloc[: idx + 2],  # the signal bar plus the fill bar
    )
    truncated.frame.attrs["session"] = bars.frame.attrs["session"][: idx + 2]
    truncated.frame.attrs["spread_fraction"] = bars.frame.attrs["spread_fraction"][: idx + 2]

    again = generate.observe_dataset(spec, truncated, sha="test")
    assert again, "truncating removed the signal entirely"
    assert again[0].observation.raw_score == pytest.approx(first.raw_score), (
        "removing every bar after the signal changed the signal's score, so the "
        "strategy is reading its own future"
    )
