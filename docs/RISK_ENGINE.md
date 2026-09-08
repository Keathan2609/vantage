# Risk engine

`internal/risk` is a pure function: given an intent, an account snapshot,
limits, a quote, feed health, market state and event context, it returns a
decision. It performs no I/O, holds no state, and cannot be influenced by
anything except its inputs.

Three properties follow from that, and all three matter:

1. It is exhaustively testable without a database or a venue.
2. It cannot be partially applied — there is no code path that skips it.
3. Its verdict is reproducible from the stored decision snapshot months later.

## Every check runs

The engine does not stop at the first failure. It evaluates all applicable
checks, appends each result with its limit and observed value, and returns the
complete list.

The reason is operational. An operator who fixes one breached ceiling and
resubmits, only to hit the next, then the next, learns nothing per attempt.
Seeing "your size exceeds three limits, here they are" is one interaction
instead of four.

`FirstFailure` is available for a headline message, but the UI shows the full
list, and `decision_snapshots` stores all of it.

## The checks

| Check | Limit source | Rejection code |
| --- | --- | --- |
| `account_trading_enabled` | account flag | `risk_limit_breached` |
| `instrument_enabled` | instrument row | `instrument_disabled` |
| `market_data_health` | feed health | `market_data_stale` / `market_data_invalid` |
| `market_open` | market clock and calendar | `market_closed` |
| `max_spread` | `max_spread_fraction` | `spread_too_wide` |
| `event_risk` | blackout window, automated orders only | `event_risk_blackout` |
| `max_daily_loss` | `max_daily_loss` | `daily_loss_limit_reached` |
| `max_drawdown` | `max_drawdown_fraction` | `drawdown_limit_reached` |
| `max_order_quantity` | `max_order_quantity` | `risk_limit_breached` |
| `max_order_notional` | `max_order_notional` | `exposure_limit_breached` |
| `max_risk_per_trade` | `max_risk_per_trade_fraction`, with the stop distance | `risk_limit_breached` |
| `stop_loss_required` | `require_stop_loss` | `risk_limit_breached` |
| `max_open_positions` | `max_open_positions` | `max_open_positions` |
| `max_pending_orders` | `max_pending_orders` | `max_pending_orders` |
| `max_gross_exposure` | `max_gross_exposure` | `exposure_limit_breached` |
| `max_net_exposure` | `max_net_exposure` | `exposure_limit_breached` |
| `max_instrument_exposure` | `max_per_instrument_exposure` | `exposure_limit_breached` |
| `max_concentration` | `max_concentration_fraction` | `concentration_limit_breached` |
| `max_leverage` | `max_leverage` | `leverage_limit_breached` |
| `available_margin` | computed from the snapshot | `insufficient_margin` |
| `sizeable_quantity` | instrument minimum and step | `quantity_invalid` |

### Two checks with non-obvious behaviour

**Concentration** only applies when other instruments hold exposure:

```go
concentrationApplies := distinctInstruments > 0
```

Without that condition, the *first* trade on an empty account is always 100%
concentrated in one instrument and is always refused. The check was written
first and blocked every opening trade until this was fixed — a good example of
a limit that is correct in aggregate and nonsense at the boundary.

**Event risk** applies to automated orders only. A manual order passes with the
event surfaced in the interface. The reasoning is in the code: the operator has
been told, and blocking a person from closing a position before a release
would be worse than the risk it prevents. Automation gets no such judgement, so
it gets the hard block.

### Loss ceilings are checked before sizing

An account past its daily loss limit does not get a smaller trade. It gets no
trade. Scaling down after a loss limit is breached would turn a hard stop into
a soft one.

## Position sizing

`internal/risk/sizing.go` computes size from the account's risk budget, not
from anything a strategy asked for:

```
risk amount   = equity × max_risk_per_trade_fraction
stop distance = |entry − stop|, in quote currency
value per lot = contract_size × stop_distance, converted to account currency
quantity      = floor(risk_amount / value_per_lot, to the instrument's step)
```

Four things this deliberately does:

- **Requires a stop.** Without one, "risk per trade" has no defined meaning, so
  a stop is mandatory when the account requires one and sizing refuses without
  it rather than assuming a default distance.
- **Rounds down, always.** Rounding to nearest can round *up* past the limit.
- **Refuses below the minimum.** If the budget cannot afford one minimum lot,
  the answer is NO TRADE, not "take the minimum anyway". On a R500 account with
  a 100-ounce gold contract at roughly R48,000 of notional per 0.01 lot, this
  is the common answer — which is why the platform also seeds `XAUUSD.m`, a
  one-ounce micro contract, and keeps the standard contract enabled so the
  refusal stays visible rather than hidden.
- **Converts explicitly.** With no USD/ZAR rate available, sizing fails with a
  currency error rather than treating dollars as rand.

## Risk may only reduce

The engine can approve a *smaller* quantity than requested, or refuse. There is
no path by which it returns more than was asked for. `ApprovedQuantity` is
clamped to the request, and the OMS uses the approved value, so a bug in a
limit calculation cannot inflate an order.

When the approved quantity is below the requested one, the decision records
both, and the UI shows the reduction. Silent shrinking would leave an operator
wondering why their 0.05 became 0.02.

## NO TRADE is a first-class outcome

A refusal is stored, not discarded: `risk_events` records the check, code,
severity and message; `decision_snapshots` records the whole verdict with its
inputs. The Risk page lists every refusal with its reason, and the Decisions
tab shows the rows where nothing was placed.

On a small account, NO TRADE is the *common* outcome. A system that only
recorded what it did would be unable to answer the more useful question — why
it did not act.

## Hard ceilings the application cannot raise

Application-level validation gives a readable sentence
("cannot exceed 10% of equity per trade"), and the database enforces the same
thing regardless:

```sql
risk_limits_risk_ceiling_ck  CHECK (max_risk_per_trade_fraction <= 0.10)
```

A compromised API, a bad migration or a manual UPDATE cannot store a limit
above the ceiling. Drawdown is capped at 100% for the same reason — a limit
above that is not a limit.

Every limit change is written to `risk_limit_history` with its previous value,
and audited with the actor. Widening a risk limit is one of the most
consequential non-trading actions available, so it leaves the clearest trail.

## Kill switches are not part of risk

They are checked earlier in the pipeline, and they are a different kind of
control: a kill switch halts **new orders** in its scope (global, account or
strategy) and does nothing to open positions.

Closing a position is the separate, explicitly confirmed Flatten action. The
two are kept apart because automatic liquidation on an alarm would dump
positions into exactly the conditions that raised the alarm — and because an
operator hitting "stop" in an incident must not be surprised in either
direction.

The UI states this next to the button, not in a tooltip.

## What the engine cannot do

- It cannot make a bad strategy profitable. It bounds loss per trade and per
  day; within those bounds a consistently poor edge still loses money.
- It cannot price gap risk. A stop is a request, not a guarantee; a weekend gap
  can fill far past it, and the backtester's documented assumptions say the
  same thing.
- It cannot detect correlation between instruments. Gross exposure and
  concentration are per-instrument; two highly correlated metals count as
  diversification here, which they are not.
- It cannot see positions opened directly in the broker's own terminal until
  reconciliation runs.

Each of these is a real limit, and none of them is hidden behind reassuring
wording in the interface.
