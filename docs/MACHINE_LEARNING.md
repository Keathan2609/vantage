# Machine learning

The default failure mode of ML in trading is not a bad model. It is a **good
score on a leaked dataset**. A model that scores 0.95 on leaked data and 0.50
in production is worse than no model, because it carries confidence it has not
earned.

`services/quant/vantage_quant/ml.py` is organised around preventing that.

## What models are allowed to do

Models **filter** signals. They never initiate one, they cannot size a trade,
they cannot modify a risk limit, they cannot promote themselves, and they have
no path to a broker. `ml_direction_filter` is a strategy that consults a model
and then returns an ordinary signal into the ordinary pipeline.

`PAPER` is the lifecycle ceiling for a model version in this build, enforced by
the API and by a database constraint.

## The five enforced properties

**1. Chronological splits with an embargo.**

Train, validation and test are contiguous windows in time order — 60 / 20 / 20
— with a gap between them of twice the label horizon. Random K-fold on
time-series data trains on the future and is the single most common cause of an
unreproducible result. The embargo removes the overlap where a training row's
label window reaches into the validation window.

**2. Labels are strictly forward-looking, and unlabelled rows are dropped.**

The label is `forward_direction` over N bars. A label built from the next N
bars cannot exist for the last N bars, and keeping those rows with a partial
label leaks. They are dropped.

**3. Features are available at decision time.**

Every feature is computed from completed bars at or before its own row. There
is no `shift(-1)` anywhere in `build_features`, and a property test asserts
that *adding future bars never changes a historical feature value* — the same
test applied to the indicators. All 14 features are covered.

**4. The baseline is a real baseline.**

Every model is compared against always-predicting-the-majority-class. A market
that rose on 53% of bars hands 53% accuracy to a model that always says "up".
`beats_baseline` and `accuracy_over_baseline` are reported per split, and a
model that does not beat the baseline is reported as not beating it.

Most first attempts do not. On a random walk, verified in the test suite, no
model beats the baseline — which is the correct result and a useful check that
the harness is not leaking.

**5. Failure is closed.**

Missing features, a failed model load or an inference error raise
`FeatureUnavailableError` and produce **no prediction**. The consuming strategy
turns that into NO TRADE. Without this, "the model is broken" silently becomes
"trade without the filter", which is the opposite of what a filter is for.

## The features

Fourteen, each documented in the code with what it measures:

| Feature | Meaning |
| --- | --- |
| `return_1`, `return_5`, `return_20` | Log returns over 1, 5 and 20 completed bars |
| `rsi_14` | Wilder RSI, scaled to [0,1] |
| `atr_fraction` | ATR(14) as a fraction of close |
| `bb_position` | Position in the Bollinger envelope, 0 at the lower band |
| `macd_histogram_atr` | MACD histogram normalised by ATR |
| `trend_slope` | 50-bar EMA slope over 10 bars, normalised by ATR |
| `volatility_ratio` | ATR(14) over its own 50-bar mean |
| `zscore_20` | Close as a z-score of its 20-bar mean |
| `range_position_50` | Position within the 50-bar high-low range |
| `hour_sin`, `hour_cos` | Hour of day as a circle, so 23:00 and 00:00 are adjacent |
| `day_of_week` | 0 = Monday |

Volatility features are expressed as *fractions of price or of their own
history* rather than absolute values, so a model trained at one price level
does not become useless at another. The hour is encoded as a sine/cosine pair
because a raw hour number tells a linear model that 23:00 and 00:00 are 23
units apart.

Feature construction requires at least 80 bars and refuses rather than
producing partially-warmed indicator values.

## Algorithms

Logistic regression, random forest and gradient boosting, from scikit-learn,
each behind a pipeline with imputation and scaling where relevant. Deliberately
conventional: the interesting problem here is the evaluation methodology, not
the estimator, and a small dataset does not support anything larger.

## What is recorded per model version

| Field | Purpose |
| --- | --- |
| `algorithm`, hyper-parameters | What was fitted |
| `random_seed` | Reproducibility |
| `dataset_hash` | The exact rows used |
| `code_git_sha` | The code that produced it |
| `dependency_versions` | numpy, pandas, scikit-learn, Python, platform |
| Window boundaries | Train, validation and test start and end |
| `lifecycle` | `EXPERIMENTAL` → `PAPER` ceiling |
| Evaluations per split | Accuracy, baseline, over-baseline, precision, recall, log loss, Brier score, positive rate, row count |

Dependency versions are recorded because a scikit-learn upgrade can change a
fitted result, and "we cannot reproduce it" is otherwise indistinguishable from
"the data changed".

## Metrics, and why Brier matters more than accuracy

Accuracy alone is meaningless on imbalanced data. The **Brier score** measures
calibration, which is what actually matters for a filter: a model whose "60%"
means 60% is usable, while one whose "90%" means 55% is dangerous precisely
because it is confident.

Both are reported per split, alongside precision, recall and log loss.

## Drift

`ml_drift_events` records distribution shift between the training window and
recent live data, per feature. Drift is a warning, not an automatic action — a
model is not disabled by drift detection, because a false positive that
silently stops trading is its own failure.

One development note worth keeping: an early drift test compared a random
walk's tail against its whole history and failed. The detector was right — the
tail of a random walk *does* differ from its history — and the test was wrong.
The test was fixed, not the detector.

## Artefacts are not committed

Trained model binaries and market datasets are **not** in Git. `.gitignore`
excludes `*.joblib`, `*.pkl`, `*.onnx`, `*.parquet`, `*.feather`, `/data/raw/`,
`/data/parquet/`, `/data/snapshots/` and `/artifacts/`.

Artefacts live in a configured directory outside the repository, referenced by
the model version row. What *is* committed is everything needed to reproduce
one: the code, the seed, the dataset hash and the window boundaries.

## What this layer does not claim

- No accuracy figure is presented as a forward expectation.
- Nothing is described as "AI-powered", and no model output is described as a
  prediction of profit.
- A model that beats the baseline on a test window has beaten the baseline on
  that window. That is the whole claim.
- Feature importance is not offered as an explanation of the market; on
  correlated financial features it mostly reflects which correlated feature the
  fitting process happened to reach first.
