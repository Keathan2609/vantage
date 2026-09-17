# Signal research

How the research plane decides whether a strategy's score means anything, and
what it is currently entitled to say.

This document covers the measurement apparatus. `STRATEGY_ENGINE.md` covers the
strategies themselves and `BACKTESTING.md` covers replaying them through the
trading pipeline.

## The question

Every strategy returns a `raw_score`. The consensus policy compares it against
a floor, and the terminal renders it as a confidence. Neither is entitled to
treat it as a probability: `score_inventory.py` records what each of the twelve
formulas actually computes, and eight of them average a real measurement with a
hard-coded constant. `donchian_breakout` is `mean(penetration, 0.5)`, so its
range is [0.250, 0.750] and it can never clear a 0.55 floor by more than a
fraction of its own span.

So before any calibration can be fitted, one question has to be answered with
evidence: **do the raw scores carry information about what happens next?**

Answering it needs three things that are easy to confuse:

1. **Signals.** A strategy that never speaks cannot be measured.
2. **Independent signals.** Three hundred signals from one uninterrupted trend
   are one opportunity observed three hundred times.
3. **A score that varies.** A constant ranks nothing, however often it fires.

## What the data is, and what it is not

Everything currently measured is `SYNTHETIC_CONTROLLED`: bars produced by
`research/datasets.py` from a seed. Sixteen market conditions, laid end to end
in time, non-overlapping, each with its own seed derived from a base.

Synthetic data can establish:

- that the pipeline computes what it claims to compute,
- that a strategy responds to the condition it was built for,
- how often a strategy signals, and over what part of its score range,
- whether coverage spans regimes and sessions.

Synthetic data **cannot** establish predictive value, calibration against real
markets, or trading edge. The generator and the strategy share a model of what
a trend is, so a trend strategy scoring well on a generated trend has partly
measured that agreement. No claim of edge may rest on it.

`research/historical.py` is the seam for real bars. It reads from an
allowlisted local directory by dataset NAME — never a caller-supplied path,
because an arbitrary path parameter is a file-read primitive pointed at the
host — and records mandatory provenance: original file hash, normalised dataset
hash, source timezone and every transformation applied. Nothing is downloaded,
scraped or fetched from a provider. Until a dataset is placed there by hand,
`historical.status()` reports `REAL_MARKET_VALIDATION_PENDING`, and that string
belongs in any report built on synthetic bars.

## Determinism, and why the data is not committed

CLAUDE.md rule 14 forbids committing large market datasets. Sixteen conditions
at 1200 bars each is megabytes, so the **generator** is committed and the bars
are produced on demand.

That is only safe because generation is deterministic. `GENERATOR_VERSION` plus
a seed plus a bar count fix the output exactly, and the dataset hash covers the
bars *and* the identity that produced them — so a version bump invalidates old
hashes even if the bars happened to coincide. A hash recorded in a report today
still identifies the same bars tomorrow.

Reproduce any run from committed code:

```bash
python -m vantage_quant.research expanded --bars 1200 --out run.json
```

## Partitioning

Datasets are ordered by their first bar and split chronologically: the earliest
50% is TRAIN, the next 25% VALIDATION, the last 25% TEST and sealed
`SEALED_FOR_OUT_OF_SAMPLE`.

The split happens in `_dataset_assignments`, **before any outcome is examined**.
Generate, hash, assign, then analyse. Looking at outcomes and then choosing
which data becomes the holdout is the precise failure a holdout exists to
prevent. `partition.guard_not_sealed` is called by every analysis entry point
rather than at each call site, so adding a new report cannot forget it.

VALIDATION is not opened either. It exists for the milestone that selects
between candidate policies; spending it on exploration would leave nothing to
select with.

## Counting evidence

### Raw N and effective N

An **episode** is a run of same-direction signals from one strategy with no
more than six bars between consecutive members. `effective_n` is the episode
count, deliberately the harsher of the available readings: treating each
episode as one observation understates evidence when signals really are
independent, and overstating it is the failure that matters.

Evidence is graded on `effective_n`, never raw:

| effective N | class |
|---|---|
| < 100 | `INSUFFICIENT` |
| < 300 | `EXPLORATORY_ONLY` |
| < 500 | `MODERATE_EVIDENCE` |
| ≥ 500 | `CALIBRATION_CANDIDATE` |

### Intervals are clustered too

Grading on the effective count while building confidence intervals on the raw
one states a precision the data has not got. Every interval therefore resamples
**episodes**, not signals — a cluster bootstrap, 2000 resamples, fixed seed. For
a strategy with 1214 signals in 404 episodes this widens the interval by
roughly √(1214/404) ≈ 1.7, and that factor is the difference between a
relationship that excludes zero and one that does not.

### The whole grid, never the best cell

Every horizon is reported for every strategy. Strategies × horizons × regimes
will produce a striking number somewhere by chance alone, so the report shows
the complete grid; a single quoted coefficient would be a selected maximum.

## Readiness

`expansion.readiness` produces one verdict per strategy. The order of its
checks is load-bearing.

1. **Role first.** A veto is asked a different question from a directional
   alpha. `pre_event_blackout` never emits a direction, so
   "does its score predict return" has no answer and it is `NOT_APPLICABLE` —
   not short of data. `grid_martingale_research` is `RESEARCH_ONLY`.
2. **Unobserved before constant.** A strategy that never signalled has a span
   of zero, and reporting `NON_INFORMATIVE_SCORE` there would be a claim about
   a distribution nobody sampled.
3. **Span before count.** A score that used under 2% of its range carries no
   ordering information, and no N rescues it. Checked before the count so a
   large sample of identical scores cannot pass.
4. **Evidence class.**
5. **Monotonicity, affirmatively.** `READY_FOR_CALIBRATION` requires a
   demonstrated ordering. `INSUFFICIENT_EVIDENCE` — fewer than three usable
   quantile bins, which happens when a score concentrates — is *not*
   monotonicity, and treating it as such once produced a false ready verdict.
6. **Interval excludes zero.**
7. **Net outcome survives costs.**

Verdicts: `READY_FOR_CALIBRATION`, `MORE_DATA_REQUIRED`,
`NON_INFORMATIVE_SCORE`, `NON_MONOTONIC`, `COST_NEGATIVE`, `RESEARCH_ONLY`,
`NOT_APPLICABLE`.

### A known gap in the taxonomy

`MORE_DATA_REQUIRED` covers two different situations: too little evidence, and
enough evidence showing no usable relationship. `donchian_breakout` has 409
effective observations and a rank-correlation interval of [-0.012, +0.072] — it
has been measured, and the answer is "no usable ordering", not "collect more
bars". `atr_volatility_regime` has 20 effective observations and carries the
same label meaning something entirely different.

Read the reason string, which always states which of the two it is, rather than
the label alone. Splitting the verdict would be the cleaner fix and is left for
the milestone that acts on these results.

## Leakage

`tests/test_research_leakage.py` holds the guards. The research package may not
be imported by strategy code: a strategy that could read a forward outcome
could fit against a future it never saw. Outcomes are computed strictly from
bars after the signal bar, and the eligible-bar denominator is measured — a
signal rate against the wrong denominator produces a required-data estimate
wrong by the same factor.

## What may be claimed

- Signal rates, coverage and score ranges on synthetic data: yes, as measured.
- Responsiveness of a strategy to a condition: yes.
- Predictive value, calibration, profitability or live readiness: **no.**
  `REAL_MARKET_VALIDATION_PENDING` stands until real bars exist, and even then
  a calibration must be fitted and validated before any of it changes.

## The current measurement

Run `signal-research-2-expanded`, generator version 1, code sha `f02e589`,
19 200 bars across 16 conditions, 9 600 of them TRAIN. 15 minutes on 12 workers.
Source type `SYNTHETIC_CONTROLLED`; `REAL_MARKET_VALIDATION_PENDING`.

| Strategy | Role | Raw N | Effective N | Signal rate | Span used | Verdict |
|---|---|---|---|---|---|---|
| `macd_momentum` | momentum | 4 580 | 750 | 53.06% | 0.959 | `NON_MONOTONIC` |
| `donchian_breakout` | breakout | 2 211 | 409 | 25.61% | 0.999 | `MORE_DATA_REQUIRED` |
| `session_london_breakout` | breakout | 1 214 | 404 | 14.06% | 0.999 | `MORE_DATA_REQUIRED` |
| `bollinger_zscore_reversion` | mean reversion | 1 213 | 376 | 13.80% | 1.000 | `COST_NEGATIVE` |
| `rsi_mean_reversion` | mean reversion | 1 096 | 110 | 12.70% | 0.999 | `MORE_DATA_REQUIRED` |
| `multi_timeframe_trend` | directional | 205 | 201 | 2.44% | 0.465 | `MORE_DATA_REQUIRED` |
| `ma_trend_crossover` | directional | 113 | 113 | 1.31% | 0.440 | `MORE_DATA_REQUIRED` |
| `atr_volatility_regime` | breakout | 102 | 20 | 1.22% | 0.777 | `MORE_DATA_REQUIRED` |
| `grid_martingale_research` | high-risk | 6 539 | 441 | 71.76% | 0.000 | `RESEARCH_ONLY` |
| `ensemble_weighted_vote` | ensemble | 0 | 0 | 0% | — | `MORE_DATA_REQUIRED` |
| `ml_direction_filter` | model filter | 0 | 0 | 0% | — | `MORE_DATA_REQUIRED` |
| `pre_event_blackout` | risk filter | 0 | 0 | 0% | — | `NOT_APPLICABLE` |

**Nothing is `READY_FOR_CALIBRATION`.** No calibration may be fitted.

### Signal scarcity was not the problem

The working hypothesis had been a signal rate near 2%. Measured, the rates
span 1.22% to 71.76%: five strategies produce over a thousand observations each
and `macd_momentum` reaches 750 *effective* observations, comfortably a
`CALIBRATION_CANDIDATE` on count alone. The shortage was never the headline
constraint. Three strategies are genuinely starved — `atr_volatility_regime`
would need about 41 000 eligible bars for 500 observations, `ma_trend_crossover`
about 38 000 — but the rest have enough data and still fail, which is a more
useful answer than "collect more".

### What actually blocks calibration

- `macd_momentum` is the strongest **negative** result. With adequate power its
  score is `FLAT` at all three horizons: ρ between 0.014 and 0.028, mean net
  return indistinguishable from zero. Measured, not unmeasured.
- `session_london_breakout` has a rank correlation that survives clustering
  (ρ ≈ 0.10–0.12, interval excluding zero) and a positive net outcome, but its
  score concentrates into **two usable quantile bins out of five**, so
  monotonicity cannot be established. What it needs is score *spread*, not more
  bars.
- `bollinger_zscore_reversion` orders outcomes weakly positively and still
  loses money after costs at every horizon.
- `rsi_mean_reversion` orders them **backwards**: higher score, worse outcome
  at all three horizons.
- `grid_martingale_research` emitted 6 539 signals of an unchanging score. A
  large count of a constant ranks nothing.
- `ml_direction_filter` is silent because no model is deployed and it
  **fails closed** rather than trading unfiltered. That is the design working,
  not a data shortage.

### Strategy edge and score quality are different things

`donchian_breakout`'s mean net return is positive and its interval excludes
zero at all three horizons (+0.19% to +0.71%), while its rank-correlation
interval includes zero at all three. On this data its signals are profitable on
average and its *score does not rank them*. Calibrating that score would add
nothing; the signal is carrying the result and the number attached to it is
decoration.

### Components often rank better than the combined score

Exploratory, unadjusted for multiple comparisons, and reported for the whole
grid rather than its best cell — but consistent with what
`score_inventory.py` predicted. `atr_volatility_regime`'s `range_position`
component reaches ρ = +0.50 where its combined score reaches −0.04.
`macd_momentum`'s `strength_atr` reaches +0.065 against a combined +0.014. In
five of the seven measurable strategies a raw component orders outcomes at
least as well as the score built from it, which is what averaging a real
measurement with a constant does.

### What the clustering correction changed

Resampling episodes rather than signals widened rank-correlation intervals by
up to 2.37× (`rsi_mean_reversion`, medium horizon: 1 096 signals but 110
episodes, its largest containing 82 signals). Where signals really are
independent the correction changes nothing: `ma_trend_crossover` has 113
signals in 113 episodes and its interval width ratio is exactly 1.00. A few
ratios fall marginally below 1.0 — bootstrap noise at 500 resamples, not a
narrowing.

Before the correction, `session_london_breakout` was reported
`READY_FOR_CALIBRATION`. It is not.
