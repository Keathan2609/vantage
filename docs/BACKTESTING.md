# Backtesting

A backtest is a claim about what would have happened. Most backtests are wrong
in the same few ways, and this engine is built specifically to avoid them.

## The four honesty rules

**1. Entries fill on the bar AFTER the signal.**

A strategy that sees bar *i*'s close cannot trade at that close -- the close is
the last price of a bar that has just finished. Entries fill at the *next*
bar's open, crossing the spread, plus slippage. Filling at the signal bar's
close is the single most flattering error available, and it silently inflates
every result.

**2. On an ambiguous bar, the STOP is assumed.**

When a bar's range covers both the stop and the target, bar data cannot say
which came first. This engine always assumes the stop was hit. That is
pessimistic by construction, which is the only safe direction for an assumption
about your own losses. Sub-bar data would resolve it properly; assuming the
target is how a losing strategy looks profitable.

**3. Costs are always applied.**

Spread (half on entry, half on exit), slippage, commission per lot, and
overnight financing. A frictionless run is available for isolating a signal's
raw behaviour, but it sets a `frictionless` flag that is stored with the result
and displayed -- so it can never be read later as a realistic expectation.

**4. Unaffordable trades are counted, not dropped.**

Size comes from the account's risk fraction and the stop distance, floored onto
the instrument's quantity step. When the budget cannot buy the minimum tradable
size, the trade is skipped **and counted** as `skipped_unaffordable`. For a
R500 account that is often most of the signals, and hiding them would produce a
result describing a much larger account.

## Shared semantics with live execution

The backtester and the paper execution path share their financial semantics:
crossing the spread, flooring quantities onto the step, refusing unaffordable
trades. From the module's own docstring:

> Where the two differ, that difference is a bug in one of them.

That is why the same strategy functions are used for both. There is no separate
"backtest version" of a strategy that could drift from the live one.

## The cost model

| Component | Default | Applied |
| --- | --- | --- |
| `spread_fraction` | 0.00012 of price | Half on entry, half on exit |
| `slippage_fraction` | 0.0001 | Adverse, on market orders |
| Commission | Per lot, from the instrument | Both sides |
| Swap / financing | Per night held | Per overnight bar |

A `spread_multiplier` is available to test a strategy under a wider book, which
is the honest way to ask "does this survive a bad session".

## What is reported

Metrics are chosen so that the flattering ones cannot stand alone:

| Metric | Why it is there |
| --- | --- |
`expectancy` | Average result per trade -- the headline
`profit_factor` | Gross profit ÷ gross loss
`max_drawdown`, `max_drawdown_pct` | The number that decides whether an approach is survivable
`max_drawdown_duration_bars` | How long being wrong lasted
`max_consecutive_losses` | What the operator would have had to sit through
`payoff_ratio` | Average win ÷ average loss
`win_rate`, `loss_rate` | Reported, never the headline
`sharpe_per_bar`, `sortino_per_bar` | Risk-adjusted, per bar rather than annualised from too little data
`calmar` | Return over drawdown
`average_mae`, `average_mfe` | How far trades went against and in favour before closing
`exposure_fraction` | How much of the period was spent in the market
`total_commission`, `total_slippage`, `total_swap` | The costs, itemised
`probability_of_loss` | Share of sampled windows ending down
`skipped_unaffordable` | Signals the account could not act on

**Win rate is reported but is never the headline.** A strategy can win 90% of
the time and lose money, and optimising for win rate reliably produces exactly
that. Expectancy, profit factor and maximum drawdown decide whether an approach
is viable.

Sharpe is reported *per bar*, not annualised. Annualising a Sharpe ratio from a
few weeks of hourly bars produces an impressive number with no information in
it.

## Sample kinds, and why the label matters

| Kind | Meaning |
| --- | --- |
| `in_sample` | Measured on the data the strategy was developed on |
| `validation` | A held-out window used for tuning |
| `out_of_sample` | Data not used in development |
| `walk_forward` | Rolling train/test, re-fitted forward |
| `paper_forward` | Live paper execution, not a simulation |

The kind is stored with the result and shown on the Backtests page, with an
in-sample run flagged in the UI. An in-sample result measures fit to the data a
strategy was developed on; it is not evidence of a forward edge, and only
`out_of_sample` or `walk_forward` counts as evidence for lifecycle promotion.

## Provenance

Every stored backtest carries:

- `strategy_id` **and** `strategy_version` -- the result belongs to a version,
  so editing parameters invalidates it
- `dataset_hash` -- a hash of the exact bars used
- `period_start`, `period_end`, `timeframe`
- `initial_capital` and `currency`
- `frictionless`
- `sample_kind`
- `warnings` -- the methodology caveats that applied to this run

The dataset hash is what makes "run it again and see" a real option: a
different hash means the data changed, and a comparison across different hashes
is not a comparison.

## Warnings the engine emits

The engine attaches warnings to its own results rather than leaving the reader
to notice:

- too few trades for the statistics to mean anything
- the test period is short relative to the strategy's timeframe
- the result is in-sample
- the run was frictionless
- a large share of signals were skipped as unaffordable
- the period contains no adverse regime

A result with warnings is still stored. Suppressing it would just move the
judgement out of view.

## The look-ahead audit

Every rule above is only worth what its enforcement is worth, so the whole
research plane was audited for the ways future information leaks into a
backtest. What was checked, and what was found:

| Leak | Enforcement | Verified by |
| --- | --- | --- |
| A strategy reading a bar it could not have seen | The engine hands each strategy `bars.iloc[: i + 1]` -- a strict prefix, never the whole frame | `backtest.py:196`; `test_backtest.py` |
| Bars supplied out of order | Non-ascending timestamps are rejected before the run starts, so a "shuffle" cannot be smuggled in as input | `test_backtest.py` |
| An indicator reading forward | 13 indicator series are recomputed on truncated prefixes and compared value-by-value | `test_indicators.py::test_no_lookahead` (parameterised) |
| Indicator warm-up producing a value from too little data | 15 `min_periods` declarations; warm-up bars are `NaN`, not a partial average | `test_indicators.py` |
| Same-bar execution | Honesty rule 1: fills are on the *next* bar's open | `test_backtest.py` |
| Random train/test splits | `chronological_split` only; the sole permutation in the codebase is `monte_carlo_trade_order`, which reorders completed *trades* to sample sequence risk and never reorders bars | `test_evaluation.py` |
| Train/test adjacency | `embargo_bars` (default 2) drops bars either side of the boundary, so a label whose horizon straddles the split cannot be learned from | `test_evaluation.py` |
| Target leakage | Labels are built from a forward horizon and the horizon bars are excluded from the features for that row | `docs/MACHINE_LEARNING.md` |
| Holdout reuse | The test split is scored once and is not used for selection | `docs/MACHINE_LEARNING.md` |
| Revised economic data | Calendar events are keyed on their release timestamp, and a revision is a new row rather than an edit to the original | `docs/NEWS_AND_CALENDAR.md` |

**One finding, and it is a caveat rather than a bug.** `support_resistance` is
the only indicator that uses `shift(-1)`, to identify a swing point by
comparing a bar with its neighbours on both sides. It cannot mark the final bar
as a swing -- there is no bar after it -- which is correct. But it then reports
the *mean* of each cluster of swing points, so adding later bars changes
cluster membership and moves the reported level: a level found at 1844.24 over
400 bars had no counterpart within 159 points once 600 bars were supplied.

That is estimator non-stationarity, not exploitable look-ahead. Two facts
bound it:

1. Nothing calls the function. It is exported and tested; no strategy, scanner
   or model uses it today.
2. Any caller inside the backtester receives a prefix, so it could only ever
   see levels derived from bars that had already closed.

It is documented rather than silently "fixed" because the honest constraint on
a future caller is a rule, not a code change: **pass a prefix, and never
compare a level computed over one window against one computed over another.**
`test_support_resistance_levels_depend_on_the_window_given` pins the behaviour
so that a change to it is deliberate.

## What a backtest still cannot tell you

- **Slippage in the conditions that matter.** The model is a fraction; real
  slippage during a payroll release is not.
- **Gaps.** A weekend gap can fill far past a stop. The engine applies the stop
  at the gap open, which is the right arithmetic, but a strategy whose risk
  depends on stops holding is more fragile than its drawdown suggests.
- **Liquidity.** Size is assumed fillable. At retail size on gold this is
  reasonable; it is still an assumption.
- **Regime change.** The most reliable finding in this field is that an edge
  measured on one regime does not survive the next. `probability_of_loss` over
  sampled windows is a partial answer, not a solution.
- **Your own behaviour.** No backtest models the operator who turns the system
  off after four losses.

The platform's answer to all of this is `paper_forward`: run it live on
simulated funds through the real pipeline, with real spreads and real refusals,
and compare. That is what this build is for.
