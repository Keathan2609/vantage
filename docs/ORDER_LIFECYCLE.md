# Order lifecycle

Every order — manual, strategy-generated or autopilot — passes through the same
ordered sequence of gates in `internal/oms`. There is no second entry point and
no internal bypass, because nothing outside that package holds a
`BrokerAdapter`.

## The nineteen gates

| # | Gate | Refusal code |
| --- | --- | --- |
| 1 | Schema validation | `schema_invalid` |
| 2 | Authentication | `unauthenticated` |
| 3 | Authorisation (role) | `forbidden` |
| 4 | Account ownership | `account_not_owned` |
| 5 | Execution mode | `execution_mode_not_permitted` |
| 6 | Trading authority | `no_trading_authority`, `trading_authority_expired`, `trading_authority_revoked`, `instrument_not_permitted_by_authority`, `strategy_not_permitted_by_authority`, `order_type_not_permitted`, `automation_disabled` |
| 7 | Kill switch | `kill_switch_active` |
| 8 | Market-data freshness | `market_data_stale`, `market_data_invalid` |
| 9 | Market / session state | `market_closed` |
| 10 | Instrument rules | `instrument_disabled`, `quantity_invalid`, `price_invalid` |
| 11 | Risk checks | `risk_limit_breached` and the specific codes in `docs/RISK_ENGINE.md` |
| 12 | Idempotency claim | `duplicate_command`, `idempotency_key_payload_mismatch` |
| 13 | Optimistic version check | `stale_object_version` |
| 14 | Persistence | — |
| 15 | Broker adapter | `broker_rejected`, `broker_unavailable` |
| 16 | Result normalisation | — |
| 17 | Portfolio and ledger update | — |
| 18 | Audit event | — |
| 19 | Metrics and observability | — |

Gates 1–11 are read-only. Nothing is written until gate 12, so a refused order
costs one transaction and produces a stored rejection with its reason rather
than a silent 4xx.

## Transaction boundaries

```
Phase A   read-only validation and the risk decision
Phase B   ONE transaction: claim the idempotency key; persist the order and its
          decision snapshot — or persist the rejection and stop
Phase C   the broker call, with NO database transaction open
Phase D   ONE transaction: apply the venue's answer to orders, fills,
          positions, the ledger and the audit log
```

The split is the point. Holding a transaction open across the broker call would
put a network round trip inside a lock on financial rows: one slow venue and
every other order queues behind it. Splitting them means a crash between C and
D leaves the order in a known, recoverable state — `FAILED`, awaiting
reconciliation — rather than a lock nobody can clear.

## Idempotency

The browser mints one key when the operator opens the review panel. Not per
click, not per render: per *intent*. A double-click, an impatient retry and a
network-level replay therefore all carry the same key.

`RegisterCommand` claims it inside phase B:

```go
tag, err := tx.Exec(ctx, `INSERT INTO command_idempotency (...) VALUES (...)
    ON CONFLICT (account_id, idempotency_key) DO NOTHING`, ...)
if tag.RowsAffected() == 1 {
    return nil, nil // freshly claimed; this request owns the order
}
// otherwise: someone else claimed it — load and replay their result
```

`ON CONFLICT DO NOTHING` rather than catching a unique-violation error, because
in Postgres a constraint violation aborts the surrounding transaction
(SQLSTATE 25P02): every subsequent statement in it fails with "current
transaction is aborted". Catching the error and continuing looks correct and
turns every duplicate into a 500. This was a real defect during development,
found by the smoke suite.

Payload hashing closes the other half. The same key with a *different* payload
is `idempotency_key_payload_mismatch`, not a replay of the first order — a
client that changed the size but reused the key gets an error rather than a
surprise.

**Verified:** eight concurrent identical submissions produce exactly one order
(`tests/smoke/smoke.py`).

## The state machine

Eleven states, with a closed transition table. Any transition not in the table
is impossible: the OMS refuses it and raises an audit event rather than
coercing the order into the requested state.

```
CREATED ──▶ VALIDATING ──▶ ACCEPTED ──▶ SUBMITTED ──▶ PARTIALLY_FILLED ──▶ FILLED
   │            │              │            │                │
   │            │              │            ├──▶ CANCEL_PENDING ──▶ CANCELLED
   │            │              │            ├──▶ EXPIRED
   └────────────┴──────────────┴────────────┴──▶ REJECTED
                                            └──▶ FAILED
```

Two transitions deserve explanation, and both are in the code's own comments:

**`SUBMITTED → FAILED`.** `FAILED` does not mean "did not happen". It means
**Vantage does not know the broker-side outcome** — the request went out and
the answer was lost. It is therefore *not terminal*: reconciliation resolves it
against venue state, and the order can move to `FILLED` if that is what
actually happened.

**`CANCEL_PENDING → FILLED`.** A cancel request races the venue. An order can
fill after the operator asks to cancel it, and pretending otherwise would
desynchronise Vantage from the broker. The UI shows `CANCEL_PENDING` as a
pending state, not a completed one, for exactly this reason.

Every transition is appended to `order_state_transitions` with the actor and
reason. That table is append-only.

## Broker error handling

`handleBrokerError` distinguishes two categories, and the distinction decides
whether a retry is ever safe:

| Category | Meaning | Result | Retry? |
| --- | --- | --- | --- |
| Definitive rejection | The venue said no (bad symbol, insufficient margin, market closed) | `REJECTED` with the venue's reason | Safe — nothing happened |
| Unknown outcome | Timeout, connection reset, malformed response | `FAILED` | **Never automatically** |

Retrying an unknown outcome is how one intended trade becomes two positions.
Vantage does not do it. It records `FAILED`, blocks automated trading for the
account, and waits for reconciliation to establish the truth.

The decision is a pure function, `oms.ClassifyBrokerError`, separated from the
write so every branch is reachable without a database or a venue. Its default
case is the important one: **anything not explicitly recognised as definitive is
UNKNOWN.** A new sentinel added to the adapter package therefore fails closed,
where an `errors.Is` chain ending in "assume rejected" would silently release
the risk budget for live positions.

### The order is marked uncertain, not merely failed

In the same transaction that moves an order to `FAILED`, Vantage sets
`orders.reconciliation_required`. `FAILED` already means "the outcome is
unknown", but the name reads as a closed failure and an operator needs to see
uncertainty as uncertainty. The flag makes it queryable, so the order appears in
the operations view, counts toward the readiness verdict, and is picked up by
the next reconciliation run even though the state machine no longer considers
it open.

It is deliberately a flag rather than a new status: a status would mean
widening the transition table for every ordinary execution in order to describe
an exceptional condition.

### Recovery transitions are a separate state machine

Reconciliation needs `ACCEPTED → FILLED`, which is the state a lost response
leaves behind. That transition is **not** added to the table above: `ACCEPTED`
means "persisted, not yet sent", so a fill arriving there during ordinary
execution is an OMS bug, and the state machine refusing it is how that bug gets
caught.

Recovery therefore has its own table, `domain.CanRepairTransition`. Every
normal transition is also a legal repair; the converse does not hold. Leaving a
terminal state stays forbidden either way — a FILLED order does not become
CANCELLED because a snapshot disagreed.

Every repair is stamped `is_repair` in `order_state_transitions` and names the
issue that justified it, so reading `SUBMITTED → FILLED` can distinguish "the
venue told us at the time" from "we reconstructed this afterwards". See
`docs/RECONCILIATION.md`.

### Automated orders are refused while an account is halted

The OMS re-reads the reconciliation halt **inside** the transaction that
persists the order, after taking the account row lock a repair also takes. An
automated order is then refused with `reconciliation_required` — a temporary
refusal, not a verdict on the order: nothing is wrong with what was asked for,
and the same request is accepted once the account's records are known to agree
with the venue. Its idempotency key stays free for that retry.

Manual orders are not gated. An operator can see the warning and decide.

## Fills and the ledger

A fill is recorded once. `fills_broker_fill_uniq` makes the venue's fill id
unique per order, so a duplicated callback or a re-read during reconciliation
cannot double-count.

Each fill produces ledger entries in `transactions` — the fill itself,
commission, and swap where applicable — each with a per-account sequence number
enforced gapless by `transactions_account_sequence_uniq`. Balance is the
ledger's running total, never a separately maintained field that could drift.

## Position effects

`Position.ApplyFill` handles four cases explicitly rather than letting sign
arithmetic decide:

| Case | Effect |
| --- | --- |
| Open | New position at the fill price |
| Increase | Weighted-average entry price recalculated |
| Reduce | Realised P&L booked for the closed portion; entry price unchanged |
| Flip through zero | The old position is closed and realised, then a new one opens with the remainder |

The flip case is the one that quietly corrupts naive implementations: a sell of
0.03 against a long 0.01 is not "short 0.02 at the fill price", it is a
realised close of 0.01 plus a new short of 0.02, and the realised P&L belongs
to the first part only.

Unrealised P&L marks at the **closing side**: a long is valued at the bid,
because that is where it could actually be exited. Marking at the mid overstates
every open position by half the spread.

## Cancellation

`POST /orders/{id}/cancel` moves an eligible order to `CANCEL_PENDING` and asks
the venue. The order stays pending until the venue answers, and may still fill.
Cancelling an order that is already terminal returns the order unchanged rather
than an error, so a duplicate click is harmless.

## Flatten

Closing a position is a separate, explicitly confirmed action
(`POST /positions/{id}/flatten`, with `confirm: true`), and it is *not* what a
kill switch does. It submits a market order in the opposite direction for the
open quantity, through the same nineteen gates. It carries
`source = risk_control`.

### A risk control must never prevent reducing risk

This paragraph used to say a flatten carries `source = manual`, and used that
to explain why the event-risk blackout does not block it. That was **wrong** —
`risk_control` is not `manual` — and the error was not academic: during a
high-impact release an operator could not close a position, refused with
`event_risk_blackout`, in exactly the window when they most want out.

It was the third instance of one mistake. The other two:

- the per-order **notional cap** refused a flatten of a position larger than
  the cap, so a position built by several individually permitted orders became
  impossible to close
- the **daily-loss ceiling** refused a flatten once breached, locking the
  account into the losing position that caused the breach while it went on
  losing

The general rule, now stated in `internal/risk/engine.go` and asserted by
tests: **a check whose purpose is to LIMIT exposure or loss must never refuse
an order that strictly reduces exposure.** Checks about whether trading is
possible at all — account enabled, instrument tradable, market open, feed
healthy, kill switch, authority — still apply, because without them there is
no price to close at.

"Strictly reducing" means the order is on the opposite side of an existing
position **and** no larger than it. Both halves matter: "opposite side" alone
let an account long 0.08 lots sell 5.00 and skip the exposure checks entirely,
because 0.08 of that is a close and 4.92 is a large new short. A side flip is
not a reduction and is measured like any other new exposure.

Manual orders remain exempt from the blackout for the original reason — the
operator has been told and is deciding anyway — and an automated strategy still
cannot **open** a position through a release, which is what the blackout is
for.
