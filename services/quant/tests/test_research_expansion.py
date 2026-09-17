"""Generation, independence and evidence grading.

The previous milestone's failure mode was too little data. The failure mode
this one invites is the opposite: a large count that is not a large amount of
evidence. These tests guard that boundary.
"""

from __future__ import annotations

from datetime import UTC, datetime, timedelta
from itertools import pairwise
from pathlib import Path

import pytest

from vantage_quant import strategies
from vantage_quant.research import datasets, expansion, historical

# --- generation -------------------------------------------------------------


def test_generation_is_deterministic_from_its_seed() -> None:
    """Same version, same seed, same bars -- or nothing is reproducible.

    The datasets are generated rather than committed, so determinism is what
    makes that safe: a hash recorded today must still identify the same bars
    tomorrow.
    """
    spec = datasets.research_suite(bars_per_condition=200)[0]
    first = datasets.generate(spec)
    second = datasets.generate(spec)
    assert first.dataset_hash == second.dataset_hash
    assert first.frame.equals(second.frame)


def test_a_different_seed_gives_different_bars() -> None:
    """Otherwise the seed is decorative and every condition is one dataset."""
    a = datasets.research_suite(bars_per_condition=200)[0]
    b = datasets.DatasetSpec(**{**a.__dict__, "seed": a.seed + 1})
    assert datasets.generate(a).dataset_hash != datasets.generate(b).dataset_hash


def test_the_hash_covers_generator_version_not_only_bars() -> None:
    """A version bump must invalidate old hashes even if bars coincide."""
    a = datasets.research_suite(bars_per_condition=200)[0]
    b = datasets.DatasetSpec(**{**a.__dict__, "generator_version": a.generator_version + 1})
    assert datasets.generate(a).dataset_hash != datasets.generate(b).dataset_hash


def test_every_market_condition_is_generated() -> None:
    suite = datasets.research_suite(bars_per_condition=100)
    assert {s.condition for s in suite} == set(datasets.MarketCondition)
    assert len(suite) == len(datasets.MarketCondition)


def test_conditions_do_not_overlap_in_time() -> None:
    """Rule 14: two datasets covering the same hours cannot both be replayed.

    Also what makes the chronological partition a straight split rather than a
    judgement call.
    """
    suite = sorted(datasets.research_suite(bars_per_condition=300), key=lambda s: s.start)
    for earlier, later in pairwise(suite):
        earlier_end = earlier.start + timedelta(hours=earlier.bars)
        assert later.start > earlier_end, (
            f"{later.dataset_id} starts at {later.start} before "
            f"{earlier.dataset_id} ends at {earlier_end}"
        )


def test_generated_bars_are_internally_consistent() -> None:
    """High is the highest and low is the lowest, on every bar.

    A generator that can emit high < close produces indicator values that
    cannot occur, and every result built on them is about a market that
    cannot exist.
    """
    built = datasets.generate(datasets.research_suite(bars_per_condition=400)[0])
    f = built.frame
    assert (f["high"] >= f["low"]).all()
    assert (f["high"] >= f["close"]).all()
    assert (f["high"] >= f["open"]).all()
    assert (f["low"] <= f["close"]).all()
    assert (f["low"] <= f["open"]).all()
    assert (f["close"] > 0).all()


def test_bars_are_not_a_cartoon_market() -> None:
    """Controlled realism, asserted rather than hoped for.

    A generator whose every bar has the same range makes ATR constant and
    every volatility strategy look better behaved than it is.
    """
    built = datasets.generate(datasets.research_suite(bars_per_condition=600)[0])
    f = built.frame
    ranges = (f["high"] - f["low"]) / f["close"]
    assert ranges.std() > 0, "every bar has an identical range"
    # Volatility clusters: consecutive ranges are correlated in a real market
    # and independent in a naive generator.
    autocorrelation = ranges.autocorr(lag=1)
    assert autocorrelation > 0.05, (
        f"bar-range autocorrelation is {autocorrelation:.3f}; the generator "
        f"produces no volatility clustering, so quiet and violent hours are "
        f"interchangeable"
    )


def test_sessions_and_spreads_accompany_every_bar() -> None:
    built = datasets.generate(datasets.research_suite(bars_per_condition=300)[0])
    assert len(built.frame.attrs["session"]) == len(built.frame)
    assert len(built.frame.attrs["spread_fraction"]) == len(built.frame)
    assert all(s > 0 for s in built.frame.attrs["spread_fraction"])


def test_every_dataset_declares_its_source_type() -> None:
    """Synthetic and historical must never be read as the same evidence."""
    for spec in datasets.research_suite(bars_per_condition=100):
        assert spec.source_type is datasets.SourceType.SYNTHETIC_CONTROLLED


def test_datasets_satisfy_the_longest_declared_warmup() -> None:
    """A strategy must not vanish because the data ends near its own minimum.

    The previous experiment lost four strategies entirely to 150-bar warm-ups
    against 140-bar fixtures.
    """
    longest = max(s.required_bars for s in strategies.REGISTRY.values())
    suite = datasets.research_suite(bars_per_condition=1200)
    for spec in suite:
        assert spec.bars > longest * 4, (
            f"{spec.dataset_id} has {spec.bars} bars against a longest warm-up of "
            f"{longest}; a strategy needs substantially more than its minimum to "
            f"produce a usable number of signals"
        )


# --- independence -----------------------------------------------------------


def _times(offsets: list[int]) -> list[datetime]:
    base = datetime(2028, 1, 3, tzinfo=UTC)
    return [base + timedelta(hours=o) for o in offsets]


def test_consecutive_same_direction_signals_are_one_episode() -> None:
    """The overstatement this exists to prevent.

    Ten signals from one uninterrupted trend are one opportunity observed ten
    times. Counting them as ten independent samples shrinks every confidence
    interval by roughly the square root of a lie.
    """
    summary = expansion.episodes(_times(list(range(10))), ["buy"] * 10)
    assert summary.raw_n == 10
    assert summary.episodes == 1
    assert summary.effective_n == 1
    assert summary.largest_episode == 10


def test_a_direction_change_starts_a_new_episode() -> None:
    summary = expansion.episodes(_times([0, 1, 2, 3]), ["buy", "buy", "sell", "sell"])
    assert summary.episodes == 2


def test_a_long_gap_starts_a_new_episode() -> None:
    summary = expansion.episodes(_times([0, 1, 50, 51]), ["buy"] * 4)
    assert summary.episodes == 2


def test_effective_n_is_never_above_raw_n() -> None:
    summary = expansion.episodes(_times([0, 20, 40, 60]), ["buy", "sell", "buy", "sell"])
    assert summary.effective_n <= summary.raw_n


def test_evidence_is_graded_on_effective_not_raw_count() -> None:
    """A thousand clustered signals must not read as a calibration candidate."""
    assert expansion.classify_evidence(99) is expansion.EvidenceClass.INSUFFICIENT
    assert expansion.classify_evidence(150) is expansion.EvidenceClass.EXPLORATORY_ONLY
    assert expansion.classify_evidence(350) is expansion.EvidenceClass.MODERATE_EVIDENCE
    assert expansion.classify_evidence(900) is expansion.EvidenceClass.CALIBRATION_CANDIDATE


# --- uncertainty ------------------------------------------------------------


def test_a_bootstrap_interval_is_deterministic() -> None:
    values = [0.01 * ((i * 7) % 13 - 6) for i in range(200)]
    first = expansion.bootstrap_mean(values)
    second = expansion.bootstrap_mean(values)
    assert first is not None and second is not None
    assert first.lower == second.lower and first.upper == second.upper


def test_a_bootstrap_interval_brackets_its_point_estimate() -> None:
    values = [0.02] * 60 + [-0.01] * 60
    interval = expansion.bootstrap_mean(values)
    assert interval is not None
    assert interval.lower <= interval.point <= interval.upper


def test_a_tiny_sample_gets_no_interval_rather_than_a_narrow_one() -> None:
    assert expansion.bootstrap_mean([0.01, 0.02, 0.03]) is None
    assert expansion.bootstrap_rate([True, False]) is None


def test_excludes_zero_reports_honestly() -> None:
    assert expansion.Interval(0.5, 0.2, 0.8, 100).excludes_zero
    assert expansion.Interval(-0.5, -0.8, -0.2, 100).excludes_zero
    assert not expansion.Interval(0.1, -0.2, 0.4, 100).excludes_zero


# --- roles and readiness ----------------------------------------------------


def test_every_registered_strategy_has_a_research_role() -> None:
    """A strategy with no role would be asked the wrong question silently."""
    for key in strategies.REGISTRY:
        assert key in expansion.STRATEGY_ROLES, (
            f"{key} has no research role. Asking 'does its score predict return?' "
            f"of a veto is a category error, and the role is what prevents it."
        )


def test_a_risk_filter_is_not_asked_a_directional_question() -> None:
    verdict, reason = expansion.readiness(
        role=expansion.StrategyRole.RISK_FILTER,
        evidence=expansion.EvidenceClass.CALIBRATION_CANDIDATE,
        score_span_used=0.9,
        monotonicity="MONOTONIC_POSITIVE",
        mean_net_return=0.05,
        spearman_interval=expansion.Interval(0.5, 0.3, 0.7, 500),
        raw_n=500,
    )
    assert verdict is expansion.CalibrationReadiness.NOT_APPLICABLE
    assert "veto" in reason


def test_a_constant_score_is_non_informative_however_large_n_is() -> None:
    """grid_martingale_research produced 313 observations of the number 0.2.

    A large count of an unchanging value ranks nothing. The span check runs
    BEFORE the count check so that N can never rescue it.
    """
    verdict, reason = expansion.readiness(
        role=expansion.StrategyRole.ALPHA_MOMENTUM,
        evidence=expansion.EvidenceClass.CALIBRATION_CANDIDATE,
        score_span_used=0.0,
        monotonicity="MONOTONIC_POSITIVE",
        mean_net_return=0.05,
        spearman_interval=expansion.Interval(0.5, 0.3, 0.7, 500),
        raw_n=500,
    )
    assert verdict is expansion.CalibrationReadiness.NON_INFORMATIVE_SCORE
    assert "ordering information" in reason


def test_an_interval_including_zero_is_not_ready() -> None:
    verdict, _ = expansion.readiness(
        role=expansion.StrategyRole.ALPHA_MOMENTUM,
        evidence=expansion.EvidenceClass.CALIBRATION_CANDIDATE,
        score_span_used=0.6,
        monotonicity="WEAK_POSITIVE",
        mean_net_return=0.01,
        spearman_interval=expansion.Interval(0.05, -0.10, 0.20, 500),
        raw_n=500,
    )
    assert verdict is expansion.CalibrationReadiness.MORE_DATA_REQUIRED


def test_a_positive_ordering_that_loses_money_is_cost_negative() -> None:
    verdict, _ = expansion.readiness(
        role=expansion.StrategyRole.ALPHA_MOMENTUM,
        evidence=expansion.EvidenceClass.CALIBRATION_CANDIDATE,
        score_span_used=0.6,
        monotonicity="MONOTONIC_POSITIVE",
        mean_net_return=-0.001,
        spearman_interval=expansion.Interval(0.3, 0.1, 0.5, 500),
        raw_n=500,
    )
    assert verdict is expansion.CalibrationReadiness.COST_NEGATIVE


def test_required_bars_follows_the_measured_rate() -> None:
    rate = expansion.SignalRate(
        strategy_id="probe", eligible_bars=1000, signals=20, buy=10, sell=10
    )
    assert rate.rate == pytest.approx(0.02)
    assert rate.bars_required_for(500) == 25000


def test_a_silent_strategy_gets_no_required_bars_estimate() -> None:
    """Dividing by a zero rate would produce an infinity dressed as a plan."""
    rate = expansion.SignalRate(
        strategy_id="silent", eligible_bars=5000, signals=0, buy=0, sell=0
    )
    assert rate.bars_required_for(500) is None


# --- historical seam --------------------------------------------------------


def test_historical_data_is_addressed_by_name_never_by_path(tmp_path: Path) -> None:
    """An arbitrary path parameter is a file-read primitive on the host."""
    directory = historical.data_directory(tmp_path)
    directory.mkdir(parents=True)
    for hostile in ("../secrets.csv", "/etc/passwd", "a/b.csv", "..\\x.csv"):
        with pytest.raises(historical.UnsafeDatasetPathError):
            historical.load(hostile, instrument="XAUUSD.m", timeframe="1h", root=tmp_path)


def test_a_missing_directory_raises_rather_than_returning_nothing(tmp_path: Path) -> None:
    """An empty frame flows onward and produces zeroes that read as a result."""
    with pytest.raises(historical.HistoricalDataUnavailableError):
        historical.load("anything.csv", instrument="X", timeframe="1h", root=tmp_path)


def test_the_status_line_is_honest_when_nothing_is_present(tmp_path: Path) -> None:
    status = historical.status(tmp_path)
    assert "REAL_MARKET_VALIDATION_PENDING" in status
    assert "SYNTHETIC_CONTROLLED" in status


def test_a_historical_dataset_is_normalised_with_recorded_provenance(
    tmp_path: Path,
) -> None:
    """Provenance is mandatory: a claim nobody can check is not evidence."""
    directory = historical.data_directory(tmp_path)
    directory.mkdir(parents=True)
    (directory / "probe.csv").write_text(
        "Timestamp,Open,High,Low,Close\n"
        "2027-01-02 01:00,100,101,99,100.5\n"
        "2027-01-02 00:00,100,101,99,100.2\n",
        encoding="utf-8",
    )
    loaded = historical.load(
        "probe.csv", instrument="XAUUSD.m", timeframe="1h", root=tmp_path
    )
    assert loaded.provenance.source_type is datasets.SourceType.HISTORICAL_MARKET
    assert loaded.provenance.original_file_hash
    assert loaded.provenance.normalized_dataset_hash
    assert loaded.provenance.bars == 2
    assert "lower-cased column names" in loaded.provenance.transformations
    assert "sorted by timestamp" in loaded.provenance.transformations
    assert loaded.frame.index.is_monotonic_increasing
    assert str(loaded.frame.index.tz) == "UTC"


def test_an_unknown_suffix_is_refused(tmp_path: Path) -> None:
    directory = historical.data_directory(tmp_path)
    directory.mkdir(parents=True)
    (directory / "probe.exe").write_bytes(b"MZ")
    with pytest.raises(historical.UnsafeDatasetPathError):
        historical.load("probe.exe", instrument="X", timeframe="1h", root=tmp_path)


# --- the empty sample -------------------------------------------------------


def test_a_veto_that_never_signals_is_not_short_of_data() -> None:
    """The bug this file caught on its first real run.

    pre_event_blackout emits no directional signal by design. Reporting it as
    MORE_DATA_REQUIRED tells the reader that collecting more bars would help,
    and no quantity of bars will ever make a veto express a direction.
    """
    verdict, _ = expansion.readiness(
        role=expansion.StrategyRole.RISK_FILTER,
        evidence=expansion.EvidenceClass.INSUFFICIENT,
        score_span_used=0.0,
        monotonicity="INSUFFICIENT_EVIDENCE",
        mean_net_return=0.0,
        spearman_interval=None,
        raw_n=0,
    )
    assert verdict is expansion.CalibrationReadiness.NOT_APPLICABLE


def test_an_unobserved_score_is_not_called_non_informative() -> None:
    """A span of zero over zero samples is absence of evidence, not evidence.

    The span check would otherwise fire on the default 0.0 and publish a claim
    about a distribution nobody sampled.
    """
    verdict, reason = expansion.readiness(
        role=expansion.StrategyRole.ALPHA_DIRECTIONAL,
        evidence=expansion.EvidenceClass.INSUFFICIENT,
        score_span_used=0.0,
        monotonicity="INSUFFICIENT_EVIDENCE",
        mean_net_return=0.0,
        spearman_interval=None,
        raw_n=0,
    )
    assert verdict is expansion.CalibrationReadiness.MORE_DATA_REQUIRED
    assert "never observed" in reason


# --- the two defects the first real run exposed ------------------------------


def test_unestablished_monotonicity_is_not_treated_as_monotonic() -> None:
    """session_london_breakout was READY_FOR_CALIBRATION on this hole.

    Its score collapsed into two usable quantile bins out of five, so
    classify_monotonicity returned INSUFFICIENT_EVIDENCE -- and readiness only
    rejected the literal NON_MONOTONIC, so "we could not establish an ordering"
    passed as "we established one". A monotone calibration needs a monotone
    ordering to exist.
    """
    verdict, reason = expansion.readiness(
        role=expansion.StrategyRole.ALPHA_BREAKOUT,
        evidence=expansion.EvidenceClass.MODERATE_EVIDENCE,
        score_span_used=0.99,
        monotonicity="INSUFFICIENT_EVIDENCE",
        mean_net_return=0.0025,
        spearman_interval=expansion.Interval(0.10, 0.046, 0.150, 500),
        raw_n=1214,
    )
    assert verdict is not expansion.CalibrationReadiness.READY_FOR_CALIBRATION
    assert verdict is expansion.CalibrationReadiness.MORE_DATA_REQUIRED
    assert "quantile bins" in reason


def test_a_flat_ordering_is_not_ready_either() -> None:
    verdict, _ = expansion.readiness(
        role=expansion.StrategyRole.ALPHA_BREAKOUT,
        evidence=expansion.EvidenceClass.CALIBRATION_CANDIDATE,
        score_span_used=0.99,
        monotonicity="FLAT",
        mean_net_return=0.0025,
        spearman_interval=expansion.Interval(0.10, 0.046, 0.150, 500),
        raw_n=900,
    )
    assert verdict is expansion.CalibrationReadiness.NON_MONOTONIC


def test_episode_labels_agree_with_the_episode_count() -> None:
    times = _times([0, 1, 2, 30, 31, 100])
    directions = ["buy", "buy", "buy", "sell", "sell", "buy"]
    labels = expansion.episode_labels(times, directions)
    assert len(labels) == len(times)
    assert len(set(labels)) == expansion.episodes(times, directions).episodes


def test_episode_labels_are_returned_in_the_callers_order() -> None:
    """Scattered back, not sorted: the caller aligns them with its own rows."""
    times = _times([50, 0, 1])
    labels = expansion.episode_labels(times, ["buy", "buy", "buy"])
    # Positions 1 and 2 are hours 0 and 1 -- contiguous, so one episode.
    assert labels[1] == labels[2]
    # Position 0 is hour 50, far past the gap, so it is its own episode even
    # though it appears FIRST in the caller's list.
    assert labels[0] != labels[1]


def test_a_clustered_interval_is_wider_than_a_naive_one() -> None:
    """The correction, demonstrated rather than asserted in a comment.

    Ten long runs of a repeated value look like 200 independent observations
    to a naive bootstrap. Resampling the runs shows how little is really there.
    """
    values: list[float] = []
    clusters: list[int] = []
    for episode in range(10):
        level = 0.02 if episode % 2 == 0 else -0.01
        values.extend([level] * 20)
        clusters.extend([episode] * 20)

    naive = expansion.bootstrap_mean(values)
    clustered = expansion.bootstrap_mean(values, clusters=clusters)
    assert naive is not None and clustered is not None
    assert (clustered.upper - clustered.lower) > (naive.upper - naive.lower)
    assert "clustered by episode" in clustered.note


def test_too_few_episodes_gets_no_clustered_interval() -> None:
    """Nine episodes cannot support a resampled interval; say so with None."""
    values = [0.01] * 90
    clusters = [i // 10 for i in range(90)]
    assert expansion.bootstrap_mean(values, clusters=clusters) is None


def test_a_clustered_interval_is_deterministic() -> None:
    values = [0.01 * ((i * 7) % 11 - 5) for i in range(200)]
    clusters = [i // 10 for i in range(200)]
    first = expansion.bootstrap_mean(values, clusters=clusters)
    second = expansion.bootstrap_mean(values, clusters=clusters)
    assert first is not None and second is not None
    assert (first.lower, first.upper) == (second.lower, second.upper)
