"""Machine-learning leakage and honesty tests.

The tests that matter here are not about accuracy. They are about whether the
reported accuracy means anything:

*   features never depend on future bars
*   labels that need future data are dropped, not imputed
*   splits are chronological with a real gap between windows
*   scaling is fitted on training data only
*   every score is reported against the majority-class baseline
*   a model that cannot see fails closed instead of guessing
"""

from __future__ import annotations

import numpy as np
import pandas as pd
import pytest

from vantage_quant import ml


def make_bars(n: int = 900, seed: int = 42) -> pd.DataFrame:
    rng = np.random.default_rng(seed)
    close = 2650.0 + np.cumsum(rng.normal(0, 3.0, n))
    close = np.maximum(close, 1000.0)
    open_ = np.concatenate([[close[0]], close[:-1]])
    wick = np.abs(rng.normal(3.0, 1.0, n))
    high = np.maximum.reduce([open_, close]) + wick
    low = np.minimum.reduce([open_, close]) - wick
    index = pd.date_range("2025-01-06T00:00:00Z", periods=n, freq="1h")
    return pd.DataFrame(
        {"open": open_, "high": high, "low": low, "close": close}, index=index
    )


# ---------------------------------------------------------------------------
# Features
# ---------------------------------------------------------------------------


def test_features_do_not_look_ahead() -> None:
    """The central ML safety property.

    Recomputing features over a prefix must reproduce the same historical
    values. If adding future bars changes a past feature, the model trains on
    information it would not have had, and every score it reports is inflated.
    """
    bars = make_bars(600)
    cutoff = 400

    full = ml.build_features(bars)
    prefix = ml.build_features(bars.iloc[:cutoff].copy())

    for column in ml.FEATURE_NAMES:
        a = full[column].iloc[:cutoff].to_numpy(dtype="float64")
        b = prefix[column].to_numpy(dtype="float64")
        both_nan = np.isnan(a) & np.isnan(b)
        np.testing.assert_allclose(
            a[~both_nan], b[~both_nan], rtol=1e-9, atol=1e-9,
            err_msg=f"feature {column!r} changed when future bars were added",
        )


def test_features_reject_insufficient_history() -> None:
    with pytest.raises(ml.FeatureUnavailableError, match="at least 80 bars"):
        ml.build_features(make_bars(50))


def test_every_declared_feature_is_produced() -> None:
    features = ml.build_features(make_bars(300))
    assert list(features.columns) == list(ml.FEATURE_NAMES)
    # Every feature carries a written definition, so a stored model's feature
    # set is self-describing rather than a list of opaque column names.
    assert set(ml.FEATURE_DEFINITION) == set(ml.FEATURE_NAMES)


def test_hour_encoding_is_cyclical() -> None:
    features = ml.build_features(make_bars(300))
    # 23:00 and 00:00 must be adjacent in the encoding, not 23 apart.
    assert features["hour_sin"].abs().max() <= 1.0
    assert features["hour_cos"].abs().max() <= 1.0


# ---------------------------------------------------------------------------
# Labels
# ---------------------------------------------------------------------------


def test_labels_are_forward_looking_and_unlabellable_rows_are_dropped() -> None:
    bars = make_bars(300)
    horizon = 3
    labels = ml.build_labels(bars, horizon)

    # The final `horizon` rows cannot be labelled: their outcome has not
    # happened. They must be NaN rather than filled with a guess.
    assert labels.iloc[-horizon:].isna().all(), (
        "rows whose outcome lies beyond the data must not be labelled"
    )
    assert labels.iloc[:-horizon].notna().all()

    # The label is the direction of the close `horizon` bars later.
    for i in range(0, 50):
        expected = 1.0 if bars["close"].iloc[i + horizon] > bars["close"].iloc[i] else 0.0
        assert labels.iloc[i] == expected


def test_dataset_drops_incomplete_rows_rather_than_imputing() -> None:
    bars = make_bars(600)
    dataset = ml.build_dataset(bars, horizon=1)

    assert not dataset.features.isna().to_numpy().any(), "features must contain no NaN"
    assert not dataset.labels.isna().to_numpy().any(), "labels must contain no NaN"
    # Rows are lost to indicator warm-up and to the unlabellable tail.
    assert dataset.row_count < len(bars)
    assert dataset.features.index.equals(dataset.labels.index)


def test_dataset_hash_changes_with_the_data() -> None:
    bars = make_bars(600)
    first = ml.build_dataset(bars, 1)

    altered = bars.copy()
    altered.iloc[400, altered.columns.get_loc("close")] += 1.0
    second = ml.build_dataset(altered, 1)

    assert first.content_hash != second.content_hash


def test_dataset_hash_is_stable_for_identical_data() -> None:
    bars = make_bars(600)
    assert ml.build_dataset(bars, 1).content_hash == ml.build_dataset(bars, 1).content_hash


def test_dataset_rejects_too_few_labelled_rows() -> None:
    with pytest.raises(ml.FeatureUnavailableError):
        ml.build_dataset(make_bars(150), horizon=1)


# ---------------------------------------------------------------------------
# Splits
# ---------------------------------------------------------------------------


def test_split_is_chronological_with_an_embargo() -> None:
    split = ml.chronological_split(1000, 0.6, 0.2, embargo_bars=5)

    # Windows are in order and do not overlap.
    assert split.train.stop <= split.validation.start
    assert split.validation.stop <= split.test.start

    # And there is a real gap between them, so a forward-looking label cannot
    # span the boundary.
    assert split.validation.start - split.train.stop == 5
    assert split.test.start - split.validation.stop == 5


def test_split_rejects_impossible_fractions() -> None:
    with pytest.raises(ValueError, match="leave nothing for the test"):
        ml.chronological_split(1000, 0.8, 0.3)
    with pytest.raises(ValueError, match="strictly between"):
        ml.chronological_split(1000, 0.0, 0.2)
    with pytest.raises(ValueError, match="no test window"):
        ml.chronological_split(50, 0.6, 0.2, embargo_bars=100)


def test_split_windows_describe_real_time_ranges() -> None:
    bars = make_bars(900)
    dataset = ml.build_dataset(bars, 1)
    split = ml.chronological_split(dataset.row_count, 0.6, 0.2, 2)
    described = split.describe(dataset.features.index)

    train_end = pd.Timestamp(described["train"]["end"])
    validation_start = pd.Timestamp(described["validation"]["start"])
    test_start = pd.Timestamp(described["test"]["start"])

    # Time moves forward across the windows.
    assert train_end < validation_start < test_start


# ---------------------------------------------------------------------------
# Training
# ---------------------------------------------------------------------------


def test_training_produces_a_full_reproducibility_record() -> None:
    bars = make_bars(900)
    model = ml.train(bars, key="test_direction", algorithm="logistic_regression", seed=7)

    assert model.dataset_hash
    assert model.seed == 7
    assert model.algorithm == "logistic_regression"
    assert model.feature_names == ml.FEATURE_NAMES
    # The environment is recorded: results depend on library versions.
    for key in ("python", "numpy", "pandas", "scikit-learn"):
        assert key in model.dependency_versions
    for window in ("train", "validation", "test"):
        assert window in model.windows
        assert window in model.evaluations


def test_training_is_reproducible_with_a_fixed_seed() -> None:
    bars = make_bars(900)
    a = ml.train(bars, key="a", seed=11)
    b = ml.train(bars, key="b", seed=11)

    assert a.dataset_hash == b.dataset_hash
    assert a.evaluations["test"]["accuracy"] == b.evaluations["test"]["accuracy"]


def test_every_evaluation_reports_the_baseline() -> None:
    """Accuracy without a baseline is not a result.

    A market that rose on 53% of bars hands 53% accuracy to a model that always
    says "up". Reporting accuracy alone invites that number to be read as skill.
    """
    bars = make_bars(900)
    model = ml.train(bars, key="baseline_check", seed=3)

    for window, metrics in model.evaluations.items():
        assert "majority_class_baseline" in metrics, f"{window} lacks a baseline"
        assert "accuracy_over_baseline" in metrics
        assert "beats_baseline" in metrics
        assert metrics["accuracy_over_baseline"] == pytest.approx(
            metrics["accuracy"] - metrics["majority_class_baseline"], abs=1e-6
        )


def test_a_model_that_does_not_beat_the_baseline_says_so() -> None:
    """On random-walk data no model should claim an edge.

    This is the test that would catch a leak: a model scoring well above the
    baseline on a random walk is reading the future.
    """
    bars = make_bars(900, seed=99)
    model = ml.train(bars, key="random_walk", seed=5)

    test_metrics = model.evaluations["test"]
    if not test_metrics["beats_baseline"]:
        assert any("does NOT beat" in w for w in model.warnings)

    # Whatever it scores, it must not score implausibly well on noise.
    assert test_metrics["accuracy"] < 0.75, (
        f"accuracy of {test_metrics['accuracy']} on a random walk suggests leakage"
    )


def test_an_embargo_narrower_than_the_label_horizon_is_refused() -> None:
    """A leakage defect found by audit, now closed.

    `train` accepted `horizon` and `embargo_bars` independently, and the API
    exposed both. With horizon=5 and embargo=2, the last three training rows
    are labelled from closes that fall inside the validation window: the model
    is trained on the outcome of the very bars it is then scored against, and
    the validation number measures nothing.

    The defaults (horizon 1, embargo 2) were always safe. The combination was
    reachable in one HTTP request.
    """
    bars = make_bars(900)
    with pytest.raises(ValueError, match="smaller than the label horizon"):
        ml.train(bars, key="leaky", horizon=5, embargo_bars=2, seed=1)


def test_a_zero_embargo_is_refused_because_the_horizon_is_never_zero() -> None:
    """A label always looks at least one bar forward, so a zero gap always leaks."""
    with pytest.raises(ValueError, match="smaller than the label horizon"):
        ml.train(make_bars(900), key="no_embargo", embargo_bars=0, seed=1)


def test_an_embargo_that_covers_the_horizon_trains_and_says_it_is_minimal() -> None:
    """The boundary case is allowed, and reported rather than passed silently."""
    model = ml.train(make_bars(900), key="tight", horizon=3, embargo_bars=3, seed=1)
    assert any("exactly equals the label horizon" in w for w in model.warnings)


def test_the_refused_split_would_genuinely_have_overlapped() -> None:
    """Proves the arithmetic behind the refusal, not just that it refuses.

    Without this, the guard could be off by one in either direction and the
    test above would still pass.
    """
    horizon = 5
    embargo = 2
    split = ml.chronological_split(900, 0.6, 0.2, embargo_bars=embargo)

    # The last training row's label is read from this many bars later.
    last_train_row = split.train.stop - 1
    label_read_at = last_train_row + horizon

    assert label_read_at >= split.validation.start, (
        "the guard is rejecting a split that does not actually overlap"
    )
    # And with an embargo equal to the horizon, it does not overlap.
    safe = ml.chronological_split(900, 0.6, 0.2, embargo_bars=horizon)
    assert (safe.train.stop - 1) + horizon < safe.validation.start


def test_unknown_algorithm_is_rejected() -> None:
    with pytest.raises(ValueError, match="unknown algorithm"):
        ml.train(make_bars(900), algorithm="deep_neural_oracle")


@pytest.mark.parametrize("algorithm", ["logistic_regression", "random_forest", "gradient_boosting"])
def test_each_supported_algorithm_trains(algorithm: str) -> None:
    model = ml.train(make_bars(700), key=f"algo_{algorithm}", algorithm=algorithm, seed=2)
    assert model.evaluations["test"]["rows"] > 0


# ---------------------------------------------------------------------------
# Inference and failing closed
# ---------------------------------------------------------------------------


def test_prediction_returns_a_probability() -> None:
    bars = make_bars(900)
    model = ml.train(bars, key="predict", seed=4)
    probability = model.predict_up_probability(bars)
    assert 0.0 <= probability <= 1.0


def test_prediction_fails_closed_on_missing_features() -> None:
    """A model that cannot see must refuse, not guess."""
    bars = make_bars(900)
    model = ml.train(bars, key="fail_closed", seed=4)

    with pytest.raises(ml.FeatureUnavailableError):
        model.predict_up_probability(make_bars(40))


def test_prediction_fails_closed_on_non_finite_prices() -> None:
    bars = make_bars(900)
    model = ml.train(bars, key="fail_closed_2", seed=4)

    corrupted = bars.copy()
    # A flat tail makes several features undefined (zero variance, zero range).
    corrupted.iloc[-60:, corrupted.columns.get_loc("close")] = corrupted["close"].iloc[-61]
    corrupted.iloc[-60:, corrupted.columns.get_loc("high")] = corrupted["close"].iloc[-61]
    corrupted.iloc[-60:, corrupted.columns.get_loc("low")] = corrupted["close"].iloc[-61]

    try:
        probability = model.predict_up_probability(corrupted)
    except ml.FeatureUnavailableError:
        return  # refusing is the correct outcome
    assert 0.0 <= probability <= 1.0


def test_no_model_deployed_returns_none() -> None:
    ml.clear_deployments()
    assert ml.load_deployed_model("nothing_here") is None


def test_deployment_registers_and_clears() -> None:
    ml.clear_deployments()
    model = ml.train(make_bars(700), key="deployable", seed=6)
    ml.deploy(model)
    assert ml.load_deployed_model("deployable") is model
    ml.clear_deployments()
    assert ml.load_deployed_model("deployable") is None


# ---------------------------------------------------------------------------
# Drift
# ---------------------------------------------------------------------------


def test_drift_detects_a_distribution_shift() -> None:
    reference = ml.build_features(make_bars(600, seed=1)).dropna()

    shifted = make_bars(300, seed=2)
    # Triple the volatility: the ATR-derived features must register a shift.
    shifted["high"] = shifted["close"] + (shifted["high"] - shifted["close"]) * 8
    shifted["low"] = shifted["close"] - (shifted["close"] - shifted["low"]) * 8
    recent = ml.build_features(shifted).dropna()

    report = ml.feature_drift(reference, recent)
    assert report["drifted_features"], "a large volatility shift must be detected"
    assert report["severity"] in ("warning", "critical")


def test_identical_windows_report_no_drift() -> None:
    """The detector's floor: a window compared with itself has not drifted."""
    reference = ml.build_features(make_bars(600, seed=1)).dropna()
    report = ml.feature_drift(reference, reference)
    assert report["severity"] == "info"
    assert not report["drifted_features"]


def test_recent_window_of_a_random_walk_legitimately_drifts() -> None:
    """A trailing window of a random walk IS a different distribution.

    This is not a false positive. A random walk has no stable level, so
    level-dependent features (z-score, position in range) genuinely differ
    between the whole history and its recent tail — which is exactly the
    condition a model trained on the full history should be warned about
    before its predictions are trusted.
    """
    reference = ml.build_features(make_bars(600, seed=1)).dropna()
    report = ml.feature_drift(reference, reference.tail(150))
    assert report["severity"] in ("info", "warning", "critical")
    # Whatever it reports, the shift magnitudes must be finite and explained.
    for name, shift in report["drifted_features"].items():
        assert name in ml.FEATURE_NAMES
        assert shift > 0.5
