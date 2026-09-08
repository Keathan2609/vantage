# Autopilot

Autopilot means letting Vantage choose among permitted markets rather than
requiring an instrument to be picked by hand.

**It is designed and partly built. It is not enabled as a standing mode in this
build.** What exists is every component it needs, exercised by the scheduler on
a fixed instrument list. What is not enabled is the standing "choose your own
market" loop.

That distinction is stated plainly here and on the Scanner page, because a
half-enabled autonomous trading mode described as finished is exactly the kind
of claim this project refuses to make.

## The pipeline

```
universe
   ↓  enabled instruments the authority permits
data quality
   ↓  per-instrument health; degraded or stale is refused for automation
scanner
   ↓  ranked candidates, cost-adjusted, event-risk capped
eligible strategies
   ↓  enabled, PAPER lifecycle, instrument declared, authority permits
signal aggregation
   ↓  agreement across strategies, weighted by confidence
portfolio context
   ↓  existing exposure, correlation, open position count
event risk
   ↓  blackout windows around high-impact releases
risk budget
   ↓  size from equity × risk fraction ÷ stop distance
trading authority
   ↓  instruments, order types, ceilings, automation flag
order intent
   ↓
ORDER PIPELINE  ← the same nineteen gates a manual order faces
```

Every stage above the order pipeline can only *narrow*. None of them can widen
a limit, and the last stage is the same OMS a human uses.

## The scanner

`services/quant/vantage_quant/scanner.py` ranks instruments and places nothing.

| Input | Effect on the score |
| --- | --- |
| Trend score | Directional strength |
| Momentum score | Rate of change |
| Volatility | Regime classification, and a penalty at extremes |
| Spread fraction | **Subtracted** — cost reduces the score directly |
| Event risk | Caps the score outright when high |
| Agreeing strategies | Raises confidence when independent approaches concur |

Subtracting cost is the part that matters on a small account. A wide spread
pushes an instrument down the list even when its trend looks attractive,
because the trend has to pay for the spread twice before it earns anything.

Every candidate carries a plain-language `explanation` of why it scored as it
did, shown on the Scanner page. A ranking nobody can interrogate is a ranking
nobody should act on.

## What makes it safe to enable

The constraint that makes Autopilot acceptable is that **it cannot widen its own
limits**:

- It produces order *intents*. Every intent faces the risk engine, the
  authority check and the kill switches.
- It cannot create or modify an authority: those endpoints require a session
  and a role the scheduler does not have.
- It cannot change a risk limit, for the same reason, and the database caps
  risk-per-trade at 10% regardless.
- It cannot promote a strategy or a model.
- It stops when `automation_enabled` is false on the authority, when a kill
  switch is active in scope, when reconciliation has unresolved critical
  discrepancies, when the feed is degraded, and when the market is closed.

Enabling it is therefore a **scheduling change**, not a new execution path.
That is the whole design goal: the dangerous version of this feature is one
that ships with its own order path "for efficiency".

## What is deliberately not enabled

| Not enabled | Why |
| --- | --- |
| Standing instrument selection | It has not been run long enough on paper to know how it behaves across regimes |
| Position pyramiding | Adding to a winner is where a modest system becomes a leveraged one |
| Automatic strategy weighting from live results | A weighting loop that reads its own P&L will chase noise on a sample this small |
| Automatic lifecycle promotion | Nothing self-promotes, ever |
| Overnight and weekend carry decisions | Financing and gap risk are modelled, but not well enough to automate the decision |

## What running it would require first

In order, and none of it is a formality:

1. **Paper-forward evidence.** Weeks of `paper_forward` results through the
   real pipeline, compared against the backtest that justified each strategy.
   Disagreement between the two is the signal that matters.
2. **A demo venue.** The mock is cooperative. Autonomous trading against a
   cooperative venue proves the logic, not the integration.
3. **Alerting.** Metrics exist; nothing is wired to notify anyone. An
   autonomous system nobody is told about is worse than a manual one.
4. **A rehearsed stop.** The kill switch works and is tested. What has not been
   rehearsed is the operator procedure around it.
5. **Correlation handling.** Gross exposure and concentration are
   per-instrument. Autopilot choosing gold *and* silver is two positions on the
   same trade, and the risk engine currently counts that as diversification.

Item 5 is a genuine gap rather than an enhancement, and it is named in
`docs/RISK_ENGINE.md` too.

## How the scheduler exercises it today

Every 30 seconds, under a lease so two instances cannot double-run:

1. List enabled PAPER strategy versions.
2. For each, take the instruments it declares.
3. Refuse, with a recorded reason, for account lifecycle, market session,
   reconciliation state, data health, authority or kill switch.
4. Ask the research service for a signal.
5. If actionable, size from the account's budget — ignoring any size the
   strategy suggested — and submit through the OMS.

Steps 3 and 5 are the ones worth noticing. The refusals are *recorded*, so a
quiet system can be told apart from a stopped one, and the sizing ignores the
strategy entirely, so a strategy cannot ask for more than the account's budget
allows.
