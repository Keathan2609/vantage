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

## The global switch

There is one control that answers "may this platform decide to trade without a
human?", and it defaults to **off**.

| | Autopilot OFF | Kill switch active |
| --- | --- | --- |
| Automated orders | refused | refused |
| Manual orders | **allowed** | refused |
| Cancel / flatten | allowed | allowed |
| Strategy evaluation | does not run | runs, then refuses |
| Reads | unaffected | unaffected |

They are deliberately separate. A kill switch is "stop everything now"; this is
"stop the robot". An operator taking over by hand should not have to disable a
protection to do it, and a system that is quiet should not still be busy.

- `GET /api/v1/autopilot` — any signed-in role. "Is the robot running?" is the
  first question anyone asks, and a viewer who cannot answer it cannot
  interpret anything else on the screen.
- `POST /api/v1/autopilot` — **ADMIN only**, with a mandatory reason of at
  least ten characters. Switching it on commits the platform to placing orders
  with no human in the loop, which is a larger decision than any single order a
  trader can make. ADMIN cannot place an order at all, so the role that starts
  the machine is not the role that trades.
- `GET /api/v1/autopilot/history` — append-only. The state table holds only the
  present; without history a period of autonomous trading leaves no trace once
  the switch is flipped back, and an investigation into a trade cannot
  establish whether autopilot was even running when it was placed.

**Enforced inside the order transaction**, after the account row lock, for the
same reason the reconciliation halt is: checking beforehand leaves a window in
which the switch is flipped microseconds before an automated order commits
anyway. It is also checked in the scheduler, so switching off stops the work as
well as the orders. Refusals carry their own code, `autopilot_off` — reporting
`reconciliation_required` instead would send someone hunting a divergence that
does not exist.

Default OFF is not caution for its own sake: an installation that arrived with
autonomous trading already enabled would be making that decision on the
operator's behalf.

## How a decision is actually made

Autopilot evaluates strategies; it does not decide by itself which strategy
wins. That is `orchestrator.Decide`, a pure function, versioned as
`PolicyVersion`.

The order of the rules IS the policy:

1. **Vetoes, first and completely.** Data quality, portfolio state, a
   high-impact release, a confident RISK_OFF model, and a required model that
   could not answer. No confidence level reaches past one — a 0.99 signal into
   a news blackout is a 0.99 signal that must not be taken.
2. **Opinions are admitted or discarded individually**, and the reason is kept
   either way. An operator asking "why did nothing happen?" needs the discards
   more than the survivors.
3. **Direction is not decided by majority.** Two buys against one sell is a
   disagreement, not a 2-1 win. Past a configured tolerance the split itself is
   disqualifying, because netting opposing signals into whichever side weighs
   more is how a system trades its own indecision.
4. **HOLD and CLOSE are abstentions**, never agreement with whatever else is
   present.
5. **Net confidence** must clear a floor that is higher than the per-opinion
   floor. Agreeing weakly is not agreeing.

A strategy that declares its valid regimes is discarded outside them, and
`UNKNOWN` is never coerced into a label to let one run. The defaults are strict
and are **not tuned numbers**: they are a starting posture for a platform that
has never traded unattended, chosen so NO TRADE is the common outcome.
Loosening them is a decision to make against paper-forward evidence.

## Testing autonomous behaviour without waiting for a market

`marketdata.ReplayProvider` drives time from the data instead of the clock. A
scenario declares the bars; stepping the provider is what makes time pass.
Start, pause, step, reset, a spread override and a simulated disconnect are all
explicit.

The property that makes it trustworthy: it never returns a bar the cursor has
not reached, so a scenario cannot quietly become a look-ahead test. Stepping
past the end holds at the last bar rather than wrapping, because a wrapped
series looks like a fresh trend while a held one produces a stale feed — a real
condition the platform already handles.

Ten scenarios (`internal/orchestrator/scenario_test.go`) cover a clean trend, a
range, a volatility shock, a high-impact event, a spread spike, a data outage
and a reconnect replaying an old price, conflicting signals, a drawdown
sequence, a strong model signal with no risk budget, and a kill switch or
autopilot switched off mid-run. Each is asserted reproducible, and each fixture
is checked to actually be the market it claims — a "trend" scenario that did
not trend would prove nothing.

They exercise the decision layer, not the full live pipeline. Wiring the replay
provider through `internal/app` needs configuration plumbing that does not
exist yet.

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
`docs/RISK_ENGINE.md` too. The consensus policy accepts portfolio refusals as
vetoes and is tested against a correlated-exposure block, so the mechanism to
act on the answer exists -- but nothing yet COMPUTES that gold and silver are
correlated, so the block is never raised in practice. The plumbing is in place
and the measurement is not.

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
