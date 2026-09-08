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
liquidation. It cannot be enabled for execution — the API refuses it and a
database constraint refuses the row that would allow it — and the UI labels it
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
and asserts the value is unchanged — 13 indicators are covered.

Two specific traps are handled:

- **Donchian channels exclude the current bar.** A breakout compared against a
  channel that includes the current bar's own high can never break out, or
  always does, depending on the comparison — either way it is not a breakout.
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
| `VALIDATED` | An out-of-sample or walk-forward result — in-sample is not evidence of an edge |
| `PAPER` | Promotion from `VALIDATED`; this build's ceiling |
| `DEMO`, `LIVE` | Refused by the API, by the domain, and by `strategy_versions_paper_ceiling_ck` |

Promotion advances **one stage at a time** and each step needs evidence
attached to the *version*, not the strategy — editing parameters produces a new
version, and the new version has no backtest until one is run. That is what
stops a promoted strategy from being quietly changed after promotion.

Nothing self-promotes. A strategy cannot advance its own lifecycle and neither
can a model. Every change is written to `strategy_lifecycle_history`
(append-only) with the actor.

## Evaluation

Manual, from the Strategies page: pick a strategy, evaluate. Two modes —
signal-only, or evaluate and route into the pipeline. Both store a decision
snapshot; the second may produce an order.

Automated, from the scheduler: every 30 seconds, under a lease so two instances
cannot double-run. Before asking for a signal the orchestrator refuses for
account lifecycle, market session, reconciliation state, data health, authority
and kill switches — the refusals are recorded, so a quiet system can be
distinguished from a stopped one.

Each run is recorded in `strategy_runs` with its outcome; signals land in
`strategy_signals`. The Strategies page shows runs, signals and failures over
seven days per strategy, which is how a strategy that has been silently
erroring for a day becomes visible.

## The research client

`internal/quant/client.go` calls the research service with a bearer token, a
timeout, and a circuit breaker. When the breaker is open, or the call fails, or
the response does not parse, the answer is **no signal** — never "proceed
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
