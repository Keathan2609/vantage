"""Machine-learning research layer.

The default failure mode of ML in trading is not a bad model — it is a good
score on a leaked dataset. This module is organised around preventing that,
because a model that scores 0.95 on leaked data and 0.50 in production is
worse than no model: it carries confidence it has not earned.

What is enforced here:

*   **Chronological splits with an embargo.** Train, validation and test are
    contiguous time windows in order, with a gap between them. Random
    K-fold on time-series data trains on the future and is the most common
    single cause of an unreproducible result.

*   **Labels are strictly forward-looking, and the rows that need future data
    to label are dropped.** A label built from the next N bars cannot exist for
    the last N bars, and keeping those rows with a partial label leaks.

*   **Features are available at decision time.** Every feature is computed from
    completed bars at or before the row's timestamp, and the feature builder is
    property-tested for look-ahead exactly as the indicators are.

*   **The baseline is a real baseline.** Every model is compared against
    always-predicting-the-majority-class. A model that cannot beat that is
    reported as not beating it, and most first attempts do not.

*   **Failure is closed.** Missing features, a failed load or an inference
    error produce no prediction, and the strategy that consumes predictions
    turns that into NO_TRADE.
"""

from __future__ import annotations

import hashlib
import json
import os
import platform
from dataclasses import dataclass, field
from datetime import datetime
from pathlib import Path
from typing import Any

import numpy as np
import pandas as pd

from vantage_quant import indicators as ind


class FeatureUnavailableError(RuntimeError):
    """Raised when features cannot be computed for a prediction."""


# ---------------------------------------------------------------------------
# Features
# ---------------------------------------------------------------------------

FEATURE_DEFINITION: dict[str, str] = {
    "return_1": "log return over the last completed bar",
    "return_5": "log return over the last 5 completed bars",
    "return_20": "log return over the last 20 completed bars",
    "rsi_14": "Wilder RSI over 14 bars, scaled to [0,1]",
    "atr_fraction": "ATR(14) as a fraction of close",
    "bb_position": "position within the Bollinger envelope, 0 at the lower band, 1 at the upper",
    "macd_histogram_atr": "MACD histogram normalised by ATR",
    "trend_slope": "slope of the 50-bar EMA over the last 10 bars, normalised by ATR",
    "volatility_ratio": "ATR(14) divided by its own 50-bar mean",
    "zscore_20": "close as a z-score of its 20-bar mean",
    "range_position_50": "position within the 50-bar high-low range",
    "hour_sin": "sine of the hour of day, so 23:00 and 00:00 are adjacent",
    "hour_cos": "cosine of the hour of day",
    "day_of_week": "day of week, 0 = Monday",
}

FEATURE_NAMES: tuple[str, ...] = tuple(FEATURE_DEFINITION)


def build_features(bars: pd.DataFrame) -> pd.DataFrame:
    """Build the feature matrix.

    Every column is computed from data at or before its own row. There is no
    ``shift(-1)`` anywhere in this function, and a property test asserts that
    adding future bars never changes a historical feature value.
    """
    if len(bars) < 80:
        raise FeatureUnavailableError(
            f"feature construction needs at least 80 bars, received {len(bars)}"
        )
    for column in ("open", "high", "low", "close"):
        if column not in bars.columns:
            raise FeatureUnavailableError(f"bars are missing the {column!r} column")

    close = bars["close"]
    high = bars["high"]
    low = bars["low"]

    atr = ind.atr(high, low, close, 14)
    atr_safe = atr.replace(0.0, np.nan)
    ema50 = ind.ema(close, 50)
    upper, _, lower = ind.bollinger(close, 20, 2.0)
    _, _, histogram = ind.macd(close)

    features = pd.DataFrame(index=bars.index)
    features["return_1"] = np.log(close / close.shift(1))
    features["return_5"] = np.log(close / close.shift(5))
    features["return_20"] = np.log(close / close.shift(20))
    features["rsi_14"] = ind.rsi(close, 14) / 100.0
    features["atr_fraction"] = atr / close
    band_span = (upper - lower).replace(0.0, np.nan)
    features["bb_position"] = (close - lower) / band_span
    features["macd_histogram_atr"] = histogram / atr_safe
    features["trend_slope"] = (ema50 - ema50.shift(10)) / atr_safe
    features["volatility_ratio"] = atr / atr.rolling(50, min_periods=50).mean()
    features["zscore_20"] = ind.zscore(close, 20)

    window_high = high.rolling(50, min_periods=50).max()
    window_low = low.rolling(50, min_periods=50).min()
    span = (window_high - window_low).replace(0.0, np.nan)
    features["range_position_50"] = (close - window_low) / span

    if isinstance(bars.index, pd.DatetimeIndex):
        hour = bars.index.hour.to_numpy(dtype="float64")
        features["hour_sin"] = np.sin(2 * np.pi * hour / 24.0)
        features["hour_cos"] = np.cos(2 * np.pi * hour / 24.0)
        features["day_of_week"] = bars.index.dayofweek.to_numpy(dtype="float64")
    else:
        features["hour_sin"] = 0.0
        features["hour_cos"] = 0.0
        features["day_of_week"] = 0.0

    return features[list(FEATURE_NAMES)]


# ---------------------------------------------------------------------------
# Labels
# ---------------------------------------------------------------------------


def label_definition(horizon: int) -> dict[str, Any]:
    return {
        "name": "forward_direction",
        "horizon_bars": horizon,
        "description": (
            f"1 when the close {horizon} completed bar(s) ahead exceeds the current "
            f"close, otherwise 0. The final {horizon} rows cannot be labelled and are "
            f"dropped rather than filled."
        ),
    }


def build_labels(bars: pd.DataFrame, horizon: int = 1) -> pd.Series:
    """Forward direction over ``horizon`` bars.

    This is the one place a future value is used, which is what a label IS. The
    rows that cannot be labelled become NaN and are dropped by ``build_dataset``;
    filling them — with zero, or with the last known direction — would train the
    model on fabricated outcomes.
    """
    if horizon < 1:
        raise ValueError(f"label horizon must be at least 1, got {horizon}")
    future = bars["close"].shift(-horizon)
    return (future > bars["close"]).astype("float64").where(future.notna())


@dataclass
class Dataset:
    features: pd.DataFrame
    labels: pd.Series
    horizon: int
    content_hash: str

    @property
    def row_count(self) -> int:
        return len(self.features)


def build_dataset(bars: pd.DataFrame, horizon: int = 1) -> Dataset:
    """Assemble aligned features and labels, dropping unusable rows."""
    features = build_features(bars)
    labels = build_labels(bars, horizon)

    frame = features.join(labels.rename("__label__"))
    # Rows with any missing feature or an unlabelled outcome are dropped.
    # Imputing a feature would invent market history; imputing a label would
    # invent an outcome.
    frame = frame.replace([np.inf, -np.inf], np.nan).dropna()

    if len(frame) < 200:
        raise FeatureUnavailableError(
            f"only {len(frame)} complete labelled rows after dropping incomplete ones; "
            f"at least 200 are needed for a chronological three-way split"
        )

    clean_features = frame[list(FEATURE_NAMES)]
    clean_labels = frame["__label__"]

    digest = hashlib.sha256()
    digest.update(json.dumps(FEATURE_DEFINITION, sort_keys=True).encode())
    digest.update(json.dumps(label_definition(horizon), sort_keys=True).encode())
    digest.update(str(clean_features.index[0]).encode())
    digest.update(str(clean_features.index[-1]).encode())
    digest.update(np.ascontiguousarray(clean_features.to_numpy(dtype="float64")).tobytes())
    digest.update(np.ascontiguousarray(clean_labels.to_numpy(dtype="float64")).tobytes())

    return Dataset(
        features=clean_features,
        labels=clean_labels,
        horizon=horizon,
        content_hash=digest.hexdigest(),
    )


# ---------------------------------------------------------------------------
# Splits
# ---------------------------------------------------------------------------


@dataclass
class Split:
    """A chronological three-way split with embargoed gaps."""

    train: slice
    validation: slice
    test: slice
    embargo_bars: int

    def describe(self, index: pd.Index) -> dict[str, dict[str, Any]]:
        def window(s: slice) -> dict[str, Any]:
            if s.start >= s.stop:
                return {"rows": 0}
            return {
                "start": pd.Timestamp(index[s.start]).isoformat(),
                "end": pd.Timestamp(index[s.stop - 1]).isoformat(),
                "rows": s.stop - s.start,
            }

        return {
            "train": window(self.train),
            "validation": window(self.validation),
            "test": window(self.test),
        }


def chronological_split(
    n_rows: int,
    train_fraction: float = 0.6,
    validation_fraction: float = 0.2,
    embargo_bars: int = 2,
) -> Split:
    """Split by time, with a gap between each window.

    The embargo exists because a label computed over a forward horizon overlaps
    the rows that follow it. Without a gap, the last training rows share their
    outcome window with the first validation rows, and the validation score is
    contaminated by information the model saw in training.
    """
    if not 0 < train_fraction < 1 or not 0 < validation_fraction < 1:
        raise ValueError("split fractions must lie strictly between 0 and 1")
    if train_fraction + validation_fraction >= 1:
        raise ValueError(
            f"train ({train_fraction}) and validation ({validation_fraction}) fractions "
            f"leave nothing for the test window"
        )
    if embargo_bars < 0:
        raise ValueError("embargo cannot be negative")

    train_end = int(n_rows * train_fraction)
    validation_start = train_end + embargo_bars
    validation_end = validation_start + int(n_rows * validation_fraction)
    test_start = validation_end + embargo_bars

    if test_start >= n_rows:
        raise ValueError(
            f"{n_rows} rows with a {embargo_bars}-bar embargo leaves no test window; "
            f"use more data or a smaller embargo"
        )

    return Split(
        train=slice(0, train_end),
        validation=slice(validation_start, validation_end),
        test=slice(test_start, n_rows),
        embargo_bars=embargo_bars,
    )


# ---------------------------------------------------------------------------
# Training
# ---------------------------------------------------------------------------

ALGORITHMS = {
    "logistic_regression": "L2-regularised logistic regression",
    "random_forest": "random forest classifier",
    "gradient_boosting": "gradient boosting classifier",
}


@dataclass
class TrainedModel:
    """A trained model with the record needed to reproduce it."""

    key: str
    version: int
    algorithm: str
    pipeline: Any
    feature_names: tuple[str, ...]
    dataset_hash: str
    horizon: int
    seed: int
    hyperparameters: dict[str, Any]
    dependency_versions: dict[str, str]
    windows: dict[str, dict[str, Any]]
    evaluations: dict[str, dict[str, Any]]
    warnings: list[str] = field(default_factory=list)
    artifact_path: str | None = None
    artifact_hash: str | None = None

    def predict_up_probability(self, bars: pd.DataFrame) -> float:
        """Probability that the next bar closes higher.

        Raises FeatureUnavailableError rather than returning a guess. The consuming
        strategy turns that into NO_TRADE, which is the only safe response to
        "the model cannot see".
        """
        features = build_features(bars)
        latest = features.iloc[[-1]]
        if latest.isna().to_numpy().any():
            missing = [c for c in latest.columns if bool(latest[c].isna().iloc[0])]
            raise FeatureUnavailableError(f"missing feature value(s): {missing}")
        if latest.replace([np.inf, -np.inf], np.nan).isna().to_numpy().any():
            raise FeatureUnavailableError("feature values are not finite")

        probabilities = self.pipeline.predict_proba(latest.to_numpy(dtype="float64"))
        classes = list(self.pipeline.classes_)
        if 1.0 not in classes and 1 not in classes:
            raise FeatureUnavailableError("model does not expose an upward class")
        index = classes.index(1.0) if 1.0 in classes else classes.index(1)
        return float(probabilities[0][index])


def dependency_versions() -> dict[str, str]:
    """Record the environment, since results depend on it."""
    import sklearn

    return {
        "python": platform.python_version(),
        "numpy": np.__version__,
        "pandas": pd.__version__,
        "scikit-learn": sklearn.__version__,
        "platform": platform.platform(),
    }


def train(
    bars: pd.DataFrame,
    *,
    key: str = "direction",
    algorithm: str = "logistic_regression",
    horizon: int = 1,
    train_fraction: float = 0.6,
    validation_fraction: float = 0.2,
    embargo_bars: int = 2,
    seed: int = 0,
    hyperparameters: dict[str, Any] | None = None,
) -> TrainedModel:
    """Train a directional classifier with chronological validation."""
    from sklearn.ensemble import GradientBoostingClassifier, RandomForestClassifier
    from sklearn.linear_model import LogisticRegression
    from sklearn.pipeline import Pipeline
    from sklearn.preprocessing import StandardScaler

    if algorithm not in ALGORITHMS:
        raise ValueError(f"unknown algorithm {algorithm!r}; choose from {sorted(ALGORITHMS)}")

    dataset = build_dataset(bars, horizon)
    split = chronological_split(
        dataset.row_count, train_fraction, validation_fraction, embargo_bars
    )

    x = dataset.features.to_numpy(dtype="float64")
    y = dataset.labels.to_numpy(dtype="float64")

    hyper = dict(hyperparameters or {})
    if algorithm == "logistic_regression":
        estimator: Any = LogisticRegression(
            C=float(hyper.get("C", 1.0)),
            max_iter=int(hyper.get("max_iter", 2000)),
            random_state=seed,
        )
    elif algorithm == "random_forest":
        estimator = RandomForestClassifier(
            n_estimators=int(hyper.get("n_estimators", 200)),
            max_depth=int(hyper.get("max_depth", 6)),
            min_samples_leaf=int(hyper.get("min_samples_leaf", 20)),
            random_state=seed,
            n_jobs=1,
        )
    else:
        estimator = GradientBoostingClassifier(
            n_estimators=int(hyper.get("n_estimators", 150)),
            max_depth=int(hyper.get("max_depth", 3)),
            learning_rate=float(hyper.get("learning_rate", 0.05)),
            random_state=seed,
        )

    # Scaling is fitted inside the pipeline on the TRAINING window only.
    # Fitting a scaler on the whole series before splitting leaks the test
    # window's distribution into training — a subtle, very common leak.
    pipeline = Pipeline([("scaler", StandardScaler()), ("model", estimator)])
    pipeline.fit(x[split.train], y[split.train])

    evaluations = {
        name: evaluate(pipeline, x[window], y[window])
        for name, window in (
            ("train", split.train),
            ("validation", split.validation),
            ("test", split.test),
        )
    }

    warnings = _training_warnings(evaluations, y, split)

    return TrainedModel(
        key=key,
        version=1,
        algorithm=algorithm,
        pipeline=pipeline,
        feature_names=FEATURE_NAMES,
        dataset_hash=dataset.content_hash,
        horizon=horizon,
        seed=seed,
        hyperparameters=hyper,
        dependency_versions=dependency_versions(),
        windows=split.describe(dataset.features.index),
        evaluations=evaluations,
        warnings=warnings,
    )


def evaluate(pipeline: Any, x: np.ndarray, y: np.ndarray) -> dict[str, Any]:
    """Score a fitted model against a data window.

    Accuracy is reported alongside the MAJORITY-CLASS baseline, because
    accuracy alone is meaningless on imbalanced data: a market that rose on 53%
    of bars gives 53% accuracy to a model that always says "up".
    """
    from sklearn.metrics import (
        accuracy_score,
        brier_score_loss,
        log_loss,
        precision_score,
        recall_score,
        roc_auc_score,
    )

    if len(y) == 0:
        return {"rows": 0, "note": "empty window"}

    predictions = pipeline.predict(x)
    try:
        probabilities = pipeline.predict_proba(x)[:, list(pipeline.classes_).index(1.0)]
    except (ValueError, IndexError):
        probabilities = predictions.astype("float64")

    majority = float(max(np.mean(y), 1.0 - np.mean(y)))
    accuracy = float(accuracy_score(y, predictions))

    metrics: dict[str, Any] = {
        "rows": len(y),
        "positive_rate": round(float(np.mean(y)), 4),
        "accuracy": round(accuracy, 4),
        "majority_class_baseline": round(majority, 4),
        # The number that matters: does the model beat always guessing the
        # more common class?
        "accuracy_over_baseline": round(accuracy - majority, 4),
        "beats_baseline": bool(accuracy > majority),
        "precision": round(float(precision_score(y, predictions, zero_division=0)), 4),
        "recall": round(float(recall_score(y, predictions, zero_division=0)), 4),
    }

    if len(np.unique(y)) > 1:
        try:
            metrics["roc_auc"] = round(float(roc_auc_score(y, probabilities)), 4)
            metrics["log_loss"] = round(float(log_loss(y, probabilities, labels=[0.0, 1.0])), 4)
            # Calibration matters more than discrimination for a filter: a
            # model whose "60%" means 60% is usable, one whose "90%" means 55%
            # is dangerous.
            metrics["brier_score"] = round(float(brier_score_loss(y, probabilities)), 4)
        except ValueError:
            pass
    return metrics


def _training_warnings(
    evaluations: dict[str, dict[str, Any]], y: np.ndarray, split: Split
) -> list[str]:
    warnings: list[str] = []

    test = evaluations.get("test", {})
    train_eval = evaluations.get("train", {})

    if not test.get("beats_baseline", False):
        warnings.append(
            f"The model does NOT beat the majority-class baseline out of sample "
            f"({test.get('accuracy')} vs {test.get('majority_class_baseline')}). "
            f"It carries no demonstrated edge and should not filter live signals."
        )
    train_acc = train_eval.get("accuracy", 0.0)
    test_acc = test.get("accuracy", 0.0)
    if train_acc - test_acc > 0.1:
        warnings.append(
            f"Training accuracy ({train_acc}) exceeds test accuracy ({test_acc}) by "
            f"more than 10 points, which is the signature of overfitting."
        )
    if test_acc > 0.65:
        warnings.append(
            f"Out-of-sample accuracy of {test_acc} on next-bar direction is implausibly "
            f"high for a liquid market. Check the feature set for look-ahead before "
            f"believing it."
        )
    if split.embargo_bars == 0:
        warnings.append(
            "No embargo between windows. With a forward-looking label the last training "
            "rows share their outcome window with the first validation rows."
        )
    positive_rate = float(np.mean(y))
    if positive_rate < 0.4 or positive_rate > 0.6:
        warnings.append(
            f"Class balance is skewed ({positive_rate:.1%} upward). Accuracy is a poor "
            f"guide here; read the baseline comparison and the Brier score instead."
        )
    return warnings


# ---------------------------------------------------------------------------
# Persistence and deployment
# ---------------------------------------------------------------------------


def artifact_dir() -> Path:
    return Path(os.getenv("VANTAGE_QUANT_ARTIFACT_DIR", "./data/artifacts"))


def save(model: TrainedModel) -> TrainedModel:
    """Persist a trained model and hash the artefact.

    Artefacts are never committed to git; they are large, binary and
    reproducible from the recorded metadata.
    """
    import joblib

    directory = artifact_dir()
    directory.mkdir(parents=True, exist_ok=True)
    stamp = datetime.utcnow().strftime("%Y%m%dT%H%M%SZ")
    path = directory / f"{model.key}-v{model.version}-{stamp}.joblib"

    joblib.dump(
        {
            "pipeline": model.pipeline,
            "feature_names": list(model.feature_names),
            "key": model.key,
            "version": model.version,
            "algorithm": model.algorithm,
            "horizon": model.horizon,
            "dataset_hash": model.dataset_hash,
            "seed": model.seed,
            "dependency_versions": model.dependency_versions,
        },
        path,
    )

    digest = hashlib.sha256(path.read_bytes()).hexdigest()
    model.artifact_path = str(path)
    model.artifact_hash = digest
    return model


_DEPLOYED: dict[str, TrainedModel] = {}


def deploy(model: TrainedModel) -> None:
    """Register a model for use by the ML-filtered strategy.

    Deployment here means "available to the research process for PAPER
    signals". It confers nothing else: the control plane holds the lifecycle
    state, caps it at PAPER, and a model cannot promote itself.
    """
    _DEPLOYED[model.key] = model


def load_deployed_model(key: str) -> TrainedModel | None:
    """Return the deployed model for a key, or None.

    None is a normal answer, and the consuming strategy must fail closed on it.
    """
    return _DEPLOYED.get(key)


def clear_deployments() -> None:
    _DEPLOYED.clear()


# ---------------------------------------------------------------------------
# Drift monitoring
# ---------------------------------------------------------------------------


def feature_drift(reference: pd.DataFrame, recent: pd.DataFrame) -> dict[str, Any]:
    """Compare recent feature distributions against the training window.

    Uses a standardised mean shift per feature. It is a smoke alarm, not a
    diagnosis: a drifting feature means the model is being asked about a market
    it was not trained on, which is worth knowing before its predictions are
    trusted.
    """
    drifted: dict[str, float] = {}
    for column in reference.columns:
        if column not in recent.columns:
            continue
        ref = reference[column].dropna()
        cur = recent[column].dropna()
        if len(ref) < 30 or len(cur) < 10:
            continue
        spread = float(ref.std(ddof=0))
        if spread <= 0:
            continue
        shift = abs(float(cur.mean()) - float(ref.mean())) / spread
        if shift > 0.5:
            drifted[column] = round(shift, 3)

    return {
        "drifted_features": drifted,
        "severity": (
            "critical" if any(v > 2.0 for v in drifted.values())
            else "warning" if drifted
            else "info"
        ),
        "note": (
            "Standardised mean shift per feature against the training distribution. "
            "A shift above 2 standard deviations means the model is being asked about "
            "conditions it has not seen."
        ),
    }
