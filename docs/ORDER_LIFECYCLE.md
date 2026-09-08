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
account, and waits for reconciliation to establish the truth. Resolution is an
explicit operator action.

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
open quantity, through the same nineteen gates. It
carries `source = manual`, and the event-risk blackout applies only to
automated orders. That exemption is not specific to flattening: any manual
order passes the blackout with the event surfaced in the interface, on the
grounds that the operator has been told and that blocking a human from closing
a position before a release would be worse than the risk it prevents.
