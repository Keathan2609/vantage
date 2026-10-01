# Strategy engine

A strategy produces a **signal**, never an order.

That is the boundary the whole research plane is built around. A signal is an
opinion with no authority: an action, a confidence between 0 and 1, an
explanation, and optionally a suggested stop and target. The orchestrator
decides whether it becomes an order intent, and that intent then faces every
gate in `docs/ORDER_LIFECYCLE.md`.

Critically, a signal cannot express a size that risk must honour. Any suggested
size is ignored: the orchestrator sizes from the account's risk budget. A
strategy that "wanted" five lots gets whatever the budget allows, which on a
small account is frequently nothing.

## The registry

Twelve strategies across eleven families, registered in
`services/quant/vantage_quant/strategies.py`:

| Key | Name | Family |
| --- | --- | --- |
| `ma_trend_crossover` | Moving Average Trend Crossover | trend following |
| `donchian_breakout` | Donchian Channel Breakout | breakout |
| `rsi_mean_reversion` | RSI Mean Reversion | mean reversion |
| `macd_momentum` | MACD Momentum | momentum |
| `bollinger_zscore_reversion` | Bollinger Z-Score Reversion | statistical |
| `atr_volatility_regime` | ATR Volatility Regime | volatility |
| `session_london_breakout` | London Session Breakout | session |
| `multi_timeframe_trend` | Multi-Timeframe Trend | multi-timeframe |
| `pre_event_blackout` | Pre-Event Blackout | event driven |
| `ensemble_weighted_vote` | Ensemble Weighted Vote | ensemble |
| `ml_direction_filter` | ML Direction Filter | machine learning |
| `grid_martingale_research` | Grid / Martingale (Research Only) | **high-risk research** |

Each declares its timeframe, its instruments, its parameters and its minimum
bar requirement. A strategy asked to run with insufficient history returns NO
TRADE with that as the reason rather than computing an indicator on too few
bars.

## The high-risk one, and why it exists

`grid_martingale_research` is marked `high_risk=True`. Grid and martingale
systems produce a long, smooth equity curve followed by a single catastrophic
loss; they are the most persistently marketed and most reliably ruinous retail
approach.

It is included so its failure mode can be **measured** rather than argued
about: backtest it, look at the maximum drawdown, and see the account
liquidation. It cannot be enabled for execution -- the API refuses it and a
database constraint refuses the row that would allow it -- and the UI labels it
as high risk everywhere it appears.

## Signal shape

```python
Signal(
    action:            "buy" | "sell" | "no_trade" | "close",
    confidence:        Decimal 0..1,
    explanation:       str,          # why, in a sentence
    suggested_stop:    Decimal | None,
    suggested_target:  Decimal | None,
    indicators:        dict[str, str],   # readings at decision time
)
```

`indicators` is preserved into the decision snapshot, so a refusal or an entry
can be examined months later against the numbers that produced it rather than
against a re-run on data that has since changed.

`no_trade` is a normal, common answer. A strategy that always has an opinion is
a strategy that has none.

## Determinism

Given the same bars, the same parameters and the same event context, a strategy
returns the same signal. There is no wall-clock reading inside strategy code,
no randomness without a recorded seed, and no hidden state between calls.

Two things follow: a signal is reproducible from the snapshot, and a backtest
uses exactly the same functions as live evaluation. There is no separate
"backtest version" of a strategy that could drift from the live one, which is
the most common source of a backtest that cannot be reproduced in practice.

## No look-ahead

Every indicator is tested for the property that its value at bar *i* depends
only on bars ≤ *i*. The test truncates the series at each index, recomputes,
and asserts the value is unchanged -- 13 indicators are covered.

Two specific traps are handled:

- **Donchian channels exclude the current bar.** A breakout compared against a
  channel that includes the current bar's own high can never break out, or
  always does, depending on the comparison -- either way it is not a breakout.
- **Only completed bars are served.** The still-forming bar is excluded by the
  market-data layer, so a signal could actually have been acted on at the time
  it claims.

## Lifecycle

```
EXPERIMENTAL ──▶ BACKTESTED ──▶ VALIDATED ──▶ PAPER ──▶ (DEMO, LIVE: unreachable)
```

| Stage | Requires |
| --- | --- |
| `BACKTESTED` | A stored backtest for that exact strategy version |
| `VALIDATED` | An out-of-sample or walk-forward result -- in-sample is not evidence of an edge |
| `PAPER` | Promotion from `VALIDATED`; this build's ceiling |
| `DEMO`, `LIVE` | Refused by the API, by the domain, and by `strategy_versions_paper_ceiling_ck` |

Promotion advances **one stage at a time** and each step needs evidence
attached to the *version*, not the strategy -- editing parameters produces a new
version, and the new version has no backtest until one is run. That is what
stops a promoted strategy from being quietly changed after promotion.

Nothing self-promotes. A strategy cannot advance its own lifecycle and neither
can a model. Every change is written to `strategy_lifecycle_history`
(append-only) with the actor.

## Evaluation

Manual, from the Strategies page: pick a strategy, evaluate. Two modes --
signal-only, or evaluate and route into the pipeline. Both store a decision
snapshot; the second may produce an order.

Automated, from the scheduler: every 30 seconds, under a lease so two instances
cannot double-run. Before asking for a signal the orchestrator refuses for
account lifecycle, market session, reconciliation state, data health, authority
and kill switches -- the refusals are recorded, so a quiet system can be
distinguished from a stopped one.

Each run is recorded in `strategy_runs` with its outcome; signals land in
`strategy_signals`. The Strategies page shows runs, signals and failures over
seven days per strategy, which is how a strategy that has been silently
erroring for a day becomes visible.

## The research client

`internal/quant/client.go` calls the research service with a bearer token, a
timeout, and a circuit breaker. When the breaker is open, or the call fails, or
the response does not parse, the answer is **no signal** -- never "proceed
without the filter".

The trust flows one way. The research service holds no credential pointing back
at the control plane, so a compromised research process cannot call the trading
API even if it wanted to.

## What a strategy cannot do

- Reach a broker: no adapter exists in the research process.
- Read credentials, accounts, orders, fills or balances: the research database
  role is granted nothing on those tables.
- Set its own size: the orchestrator sizes from the account budget.
- Bypass risk, authority, kill switches, idempotency, the OMS or the audit log:
  there is one pipeline and no alternative entry point.
- Widen a limit: those endpoints require a session and a role the scheduler
  does not have.
- Promote itself: lifecycle changes are an authenticated operator action.


## The regime is a gate, not a label

Every strategy version declares `valid_regimes`, and the consensus policy
DISCARDS any strategy whose declared set excludes the current regime. Nothing
downstream re-examines that: a discarded opinion never reaches the vote, never
reaches risk, and never becomes an order.

No PAPER-promoted strategy declares itself valid in `UNKNOWN`. An UNKNOWN
verdict is therefore not a soft outcome -- it is a guaranteed NO TRADE for that
bar, whatever the strategies saw. A live decision snapshot showed exactly that:
`macd_momentum` produced a `sell` at confidence 0.619, comfortably above the
policy floor, and the record read

    discarded: declares itself valid in TRENDING, and the regime is UNKNOWN

with the outcome `NO TRADE: no strategy offered an actionable opinion that
survived the policy`.

### The band that had no branch

`classify_regime` decided TRENDING (ADX above 25 **and** displacement), RANGING
(ADX below 20), and RANGING again for ADX above 25 that was going nowhere -- on
the stated reasoning that displacement, not ADX, is what separates a trend from
an oscillation.

ADX between 20 and 25 had no branch and fell through to UNKNOWN. Measured
across the sixteen generated market conditions, that fallthrough took **9.5% of
all decision points**, and the band matched ADX 20 -- 25 almost exactly (9.6%).

It was also inconsistent. The same market going nowhere was called RANGING at
ADX 30 and unclassifiable at ADX 22 -- the *less* directional reading getting
the more conservative treatment. The band's measured efficiency confirms which
population it belongs to:

| label | n | median efficiency | share ≥ 0.30 |
|---|---|---|---|
| UNKNOWN band (ADX 20 -- 25) | 248 | 0.203 | 29.4% |
| RANGING | 1188 | 0.154 | 3.7% |
| TRENDING | 1109 | 0.511 | 100% |

The same rule now applies in the band: going nowhere is a range. What remains
UNKNOWN is the genuinely ambiguous part -- price getting somewhere while ADX has
not confirmed it, which an emerging trend and a false start look like alike,
and which is exactly when a mean-reversion strategy must not be told it is safe.

Effect, measured on the same data: UNKNOWN 9.5% → 2.7%, RANGING 45.0% → 51.8%,
**TRENDING unchanged at 42.9%** -- the trend definition was not loosened.

### What this does and does not claim

It makes the classifier self-consistent and is evidenced on the classifier's
own behaviour, which is a property of the code. It is **not** evidence that
trading those bars is profitable. The strategies it newly admits are the
mean-reversion pair, and `docs/SIGNAL_RESEARCH.md` records that
`rsi_mean_reversion`'s score ordering runs backwards and
`bollinger_zscore_reversion` is cost-negative on synthetic data. Fixing a
classifier gap is not an endorsement of the strategies behind it.
