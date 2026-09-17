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

`research/historical.py` is the seam for real bars, described in full under
**Historical data** below. Until a dataset is placed there by hand,
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

### The taxonomy, and the overload that was removed from it

`MORE_DATA_REQUIRED` used to cover two situations that call for opposite
actions: too little evidence, and enough evidence showing no usable
relationship. `donchian_breakout` carried it with 409 independent episodes
behind it; `atr_volatility_regime` carried it with 20. It is now split.

| Verdict | Means |
|---|---|
| `READY_FOR_CALIBRATION` | Permission to try. Nothing is fitted by this. |
| `INSUFFICIENT_DATA` | Too few observations. Collect more bars. |
| `INSUFFICIENT_INDEPENDENT_EPISODES` | Plenty of signals, too few independent ones. More bars help only if they contain new episodes rather than longer ones. |
| `NON_INFORMATIVE_SCORE` | The score barely moves. No quantity of data fixes a constant. |
| `NON_MONOTONIC_SCORE` | Measured with adequate power; outcome does not move consistently with score. |
| `ORDERING_NOT_ESTABLISHED` | Measured, and still cannot be resolved from zero. The score's *distribution* is the obstacle, not its quantity. |
| `NEGATIVE_ORDERING` | Higher score, worse outcome. Informative, and the opposite of what the platform renders. |
| `COST_NEGATIVE` | Ordering exists, net outcome does not survive costs. |
| `RESEARCH_ONLY` | Not evaluated as directional alpha by role. |
| `NOT_APPLICABLE` | A veto emits no directional opinion, so there is no question to be short of. |

Two of those — `INSUFFICIENT_DATA` and `INSUFFICIENT_INDEPENDENT_EPISODES` —
are the only ones where waiting for more data is the right response. The set
is exported as `NEEDS_MORE_DATA`, and its counterpart `SCORE_IS_THE_PROBLEM`
names the verdicts where waiting would spend a milestone re-learning what is
already known.

### The score and the signal rule are flagged separately

A bad score is not a bad trading hypothesis, so a second, independent flag
records which one to revisit:

- `SCORE_REDESIGN_CANDIDATE` — the entry rule produces a net outcome whose
  interval excludes zero while its score fails to rank those same signals.
- `SIGNAL_RESEARCH_REQUIRED` — neither the outcome nor the ordering is
  established, so the hypothesis itself is the open question.

Nothing is redesigned on the strength of the flag; it marks where to look.

### Prior runs are translated, not rewritten

Milestone E's records keep their original labels — they are the record of what
was concluded at the time. `verdict.map_legacy` translates them on the way out,
and where the old label is genuinely ambiguous and no counts accompany it, it
returns nothing and says why. A migration that guesses is indistinguishable
from one that knows.

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

Run `signal-research-2-expanded` under `signal-research-analysis/v1`
(classification v2), generator version 1, 19 200 bars across 16 conditions,
9 600 of them TRAIN, ~15 minutes on 12 workers. Source type
`SYNTHETIC_CONTROLLED`; `REAL_MARKET_VALIDATION_PENDING`.

Milestone E's original record is preserved unchanged as its own run. This is a
re-run under the corrected taxonomy, not an edit of that one.

| Strategy | Role | Raw N | Effective N | Signal rate | Span used | Verdict | Flag |
|---|---|---|---|---|---|---|---|
| `macd_momentum` | momentum | 4 580 | 750 | 53.06% | 0.959 | `NON_MONOTONIC_SCORE` | signal research |
| `donchian_breakout` | breakout | 2 211 | 409 | 25.61% | 0.999 | `ORDERING_NOT_ESTABLISHED` | **score redesign** |
| `session_london_breakout` | breakout | 1 214 | 404 | 14.06% | 0.999 | `ORDERING_NOT_ESTABLISHED` | **score redesign** |
| `bollinger_zscore_reversion` | mean reversion | 1 213 | 376 | 13.80% | 1.000 | `COST_NEGATIVE` | signal research |
| `rsi_mean_reversion` | mean reversion | 1 096 | 110 | 12.70% | 0.999 | `INSUFFICIENT_INDEPENDENT_EPISODES` | — |
| `multi_timeframe_trend` | directional | 205 | 201 | 2.44% | 0.465 | `INSUFFICIENT_DATA` | — |
| `ma_trend_crossover` | directional | 113 | 113 | 1.31% | 0.440 | `INSUFFICIENT_DATA` | — |
| `atr_volatility_regime` | breakout | 102 | 20 | 1.22% | 0.777 | `INSUFFICIENT_DATA` | — |
| `grid_martingale_research` | high-risk | 6 539 | 441 | 71.76% | 0.000 | `RESEARCH_ONLY` | — |
| `ensemble_weighted_vote` | ensemble | 0 | 0 | 0% | — | `INSUFFICIENT_DATA` | — |
| `ml_direction_filter` | model filter | 0 | 0 | 0% | — | `INSUFFICIENT_DATA` | — |
| `pre_event_blackout` | risk filter | 0 | 0 | 0% | — | `NOT_APPLICABLE` | — |

**Nothing is `READY_FOR_CALIBRATION`.** No calibration may be fitted.

### What the taxonomy split revealed

Eight strategies carried the single label `MORE_DATA_REQUIRED`. Under the
corrected taxonomy they separate into three different situations needing three
different responses:

- **five are genuinely short of data** (`INSUFFICIENT_DATA`) — collect more bars,
- **one is short of INDEPENDENCE, not volume** (`rsi_mean_reversion`: 1 096
  signals in 110 episodes) — more bars help only if they contain new episodes,
- **two have been measured and cannot be resolved** (`ORDERING_NOT_ESTABLISHED`)
  — collecting more bars would spend a milestone re-learning what is known.

The last two are also both `SCORE_REDESIGN_CANDIDATE`: their entry rules produce
a net outcome whose interval excludes zero while the score fails to rank those
same signals. That was visible in Milestone E only as prose; it is now a field.

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
  bars — which is precisely what `ORDERING_NOT_ESTABLISHED` now says.
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

---

# Historical data

Everything above describes the measurement. This describes how real bars get
into it, and the answer is: by hand, through one door, with their provenance
recorded.

## Nothing is fetched

No download, no scrape, no provider API, no broker. CLAUDE.md rule 3 and the
research plane's whole design forbid it, and a missing dataset therefore fails
loudly rather than reaching for a network. Acquiring data is a human decision
with licensing attached to it.

`python -m vantage_quant.research datasets` reports what is present and, when
nothing is, prints the exact specification needed — instrument, timeframe,
minimum bars, required and optional columns, accepted file types and the
timezone requirement. The minimum bar count is derived from the longest
registered warm-up plus the episode target, not picked.

## Three layers

```
RAW          the bytes as they arrived. Never mutated, SHA-256 on sight.
NORMALIZED   canonical schema, UTC index, every transformation recorded.
RESEARCH     a normalized dataset that passed quality and was partitioned.
```

Kept separate so that "was that in the file, or did we do it?" stays
answerable — the first question anyone asks of a surprising result. Both hashes
travel into every report.

## One door

A dataset is addressed by **name**, inside an allowlisted directory. Never by a
path: an arbitrary path parameter is a file-read primitive pointed at the host.
The name is rejected if it contains a separator, `..`, a drive letter or a
leading dot, and the resolved path is then checked for containment with
`is_relative_to` — a string prefix would accept `research-data-elsewhere`.

The file itself is untrusted input. It is bounded in size, its header is
checked for duplicate columns *before* pandas silently disambiguates them, a
column name that a spreadsheet would execute as a formula is refused, and
unparseable timestamps are an error rather than a `NaT` that flows onward.

## Canonical schema

`timestamp` (UTC index), `open`, `high`, `low`, `close`, and where the source
provides them `bid`, `ask`, `spread`, `volume`. Unrecognised columns are
dropped and the drop is recorded.

**Bid/ask are never synthesised.** A source without them yields
`cost_basis = ESTIMATED_COSTS`, and that label belongs on every result derived
from it. A backtest reporting costs it never paid is worse than one that admits
it is estimating. A `spread` column of zeroes does not count as observed.

## Timezone

Normalised to UTC, and **never inferred from this machine**. If the source
states a timezone it is converted and recorded; if a timezone is supplied
explicitly it is applied and recorded; if nobody said, the timestamps are taken
as UTC, the provenance reads `UNDECLARED` and a `TIMEZONE_ASSUMED` warning is
attached. Gold and FX histories are commonly exported in broker server time,
and a silent offset moves every session boundary in the analysis. Daylight
saving is handled by converting through the named zone rather than a fixed
offset, which is tested across the March transition.

## Quality, recorded rather than reduced to a boolean

`ERROR` findings mean the data cannot support research: duplicate timestamps,
out-of-order rows, non-finite or non-positive prices, impossible OHLC, crossed
quotes. A dataset with any of these is `REJECTED` and cannot enter a run.

`WARNING` findings mean it can, with the reader told what they are working
with: assumed timezone, absent spread, irregular spacing, large gaps, abnormal
ranges or spreads, suspected flatlining, Saturday bars, low coverage. This is
the common and correct outcome for real market data — a validator that refused
everything imperfect would refuse every real market file in existence.

Status: `IMPORTED` → `VALIDATING` → `VALID` / `VALID_WITH_WARNINGS` /
`REJECTED`. **Nothing is repaired.** A validator that silently fixes its input
destroys the evidence that the input was broken.

## Coverage

A market clock modelled on `fx_metals_24x5` distinguishes a closed venue from a
provider that lost data — counting the weekend as missing makes a complete
dataset look broken and hides the one that is. Coverage is measured between the
first and last bar present, so truncation at either end is invisible by
construction; nothing in a file can say the series ought to have continued.

## Resampling

Only ever coarser, with a recorded aggregation version. Going finer would
invent bars, and empty periods are dropped rather than forward-filled for the
same reason: an interpolated OHLC bar asserts that trades happened at prices
nobody quoted.

## Partitioning and the seal

Identical to the synthetic path and for the same reason: ordered by first bar,
split 50/25/25 chronologically, TEST sealed `SEALED_FOR_OUT_OF_SAMPLE`, and the
assignment made **before** any outcome is examined. `guard_not_sealed` is
called by the entry point rather than at each call site.

## One analysis, two data sources

Historical and synthetic runs both call `expanded.observe_frame` and
`expanded.build_report`. There is no `HistoricalResearchV2` with its own
statistical rules, because then a difference between the two results could come
from either the market or the code, and the comparison would mean nothing.

Analysis methodology is versioned as a whole (`signal-research-analysis/v1`),
carrying horizon registry, cost model, clustering rule, bootstrap,
classification, generator, partition and normalization versions. `compare`
refuses two runs whose versions differ rather than producing a table whose
differences are partly methodological.

## Disagreement is the finding

Synthetic and historical results are never averaged. A number averaged across
the two describes no market that exists, and it would destroy the single most
useful thing this work can produce — the discovery that a relationship was an
artefact of the generator. Disagreements are classified instead:
`SYNTHETIC_ONLY_RELATIONSHIP`, `HISTORICAL_ONLY_RELATIONSHIP`,
`DIRECTION_REVERSAL`, `SCORE_RANGE_MISMATCH`, `COST_SURVIVAL_MISMATCH`,
`NOT_COMPARABLE`, `AGREES`.

`SCORE_RANGE_MISMATCH` is checked first: if the two sources exercised different
parts of the score function they are not measuring the same population, and any
apparent agreement downstream is between different things.

## Throughput

Measured on constructed fixture bars, because throughput is a property of the
code rather than the market: **41.3 bars/sec** and 110 observations/sec across
12 workers, against 9.4 bars/sec single-threaded before the run was
parallelised. That is roughly 2.5 minutes per year of hourly TRAIN data, or
about 12 minutes for five years — multi-year work is practical.

Workers re-import each dataset by name inside the worker rather than receiving
a pickled frame: the re-read costs milliseconds against seconds of strategy
evaluation, and addressing by name keeps the allowlist check inside the worker
instead of trusting whatever arrived over the process boundary. A test asserts
the serial and parallel paths produce identical counts, spans and verdicts.

## Current status

**`WAITING_FOR_HISTORICAL_DATA`.** No legitimate historical dataset exists
locally. The importer, validator, partitioner, runner and comparison are built
and tested; what is missing is the data, and it was not invented or downloaded.
