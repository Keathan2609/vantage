# Architecture

## The one decision everything else follows from

There is exactly one path from an intent to a broker, and it lives in Go.

```
                    ┌──────────────────────────────────────┐
   browser ────────▶│  control plane (Go)                  │
   (Next.js)        │                                      │
                    │   httpapi → oms → broker adapter     │
                    │              ↑                       │
                    │           risk engine                │
                    └───────┬──────────────────┬───────────┘
                            │                  │
                            ▼                  ▼
                    ┌───────────────┐   ┌──────────────────┐
                    │  PostgreSQL   │◀──│  quant (Python)  │
                    │               │   │  read-only role  │
                    └───────────────┘   └──────────────────┘
```

The quant service produces *signals, backtests, model evaluations and scanner
rankings*. It has no broker client, no credentials pointing back at the control
plane, and a database role with `SELECT` on market and research tables only. It
cannot place an order because there is nothing in its process that can.

This is the constraint the whole design serves: research code changes fastest,
is the most experimental, and is written in the language with the least type
safety. Giving it execution capability would put the least disciplined part of
the system closest to the money.

## Planes

### Control plane — `services/control-api` (Go)

Owns authentication, authorisation, accounts, orders, positions, the ledger,
risk, trading authority, kill switches, reconciliation, market-data ingestion,
the audit log and the scheduler. It is the only component that holds a
`BrokerAdapter`.

Package layout, inner to outer:

| Package | Depends on | Role |
| --- | --- | --- |
| `money` | nothing | Decimal amounts with a mandatory currency |
| `domain` | `money` | Orders, positions, risk types, the market clock, audit hashing. No I/O. |
| `risk` | `domain`, `fx` | The risk engine: a pure function from state to a decision |
| `store` | `domain`, pgx | Every SQL statement, one file per context |
| `broker` | `domain` | The adapter interface; `broker/mock` is the paper venue |
| `oms` | all of the above | The order pipeline |
| `orchestrator` | `oms`, `quant` | Turns a research signal into a reviewed order intent |
| `httpapi` | everything | HTTP, middleware, serialisation |
| `scheduler` | everything | Periodic jobs under leases |

Dependencies point inwards only. `domain` cannot import `store`, so a domain
rule cannot be quietly satisfied by a database query, and the rules are
testable without a database.

### Research plane — `services/quant` (Python, FastAPI)

Indicators, 12 strategies, the backtester, the ML pipeline and the scanner.
Stateless with respect to trading: it is given the data it needs in the request
or reads it through its read-only role, and it answers.

The control plane calls it over HTTP with a shared bearer token, behind a
circuit breaker and a timeout. When the research service is unreachable, the
control plane's answer is "no signal" — never "trade without the filter".

### Presentation — `apps/web` (Next.js 16, TypeScript strict)

A client-rendered terminal. It holds no business rule: every refusal it shows
comes from the API with a machine-readable code, and it never computes a limit,
a size or a P&L figure. Money arrives as decimal strings and stays strings;
`lib/format.ts` formats them without ever constructing a float.

### Storage — PostgreSQL

57 tables. The database is not a passive store; it is the last line of several
invariants:

- `accounts_paper_only_ck`, `broker_connections_mock_only_ck` — mode ceilings
- `orders_account_idempotency_uniq` — one order per idempotency key per account
- `fills_broker_fill_uniq` — a venue fill cannot be recorded twice
- `positions_one_open_per_instrument_uniq` — no accidental parallel positions
- `transactions_account_sequence_uniq` — a gapless per-account ledger sequence
- `kill_switches_active_global_uniq` — one active global switch, not five
- `risk_limits_risk_ceiling_ck` — no risk-per-trade above 10%, ever
- `strategy_versions_paper_ceiling_ck` — no strategy above PAPER in this build
- append-only triggers on `audit_events`, `transactions`, `fills`,
  `order_state_transitions`, `decision_snapshots` and the three history tables

Redis holds rate-limit buckets. It is never a source of truth: when Redis is
not configured or not reachable, the limiter falls back to an in-process
token bucket, which is exactly correct for a single instance and would divide
each limit by the instance count in a multi-instance deployment. That trade-off
is documented in `internal/ratelimit` rather than hidden. Financial state never
touches Redis.

## Data flow: a manual order

1. The browser mints an idempotency key when the operator opens the review
   panel — once per intent, not once per click.
2. `POST /api/v1/orders` authenticates the session cookie, checks the CSRF
   header, requires the `trader` role and confirms account ownership.
3. `oms.Submit` runs phase A: execution mode, trading authority, kill switches,
   market-data freshness, session state, instrument rules, then the risk
   engine.
4. Phase B, one transaction: claim the idempotency key, write the order and its
   decision snapshot — or write the rejection and stop.
5. Phase C: call the broker adapter with **no transaction open**.
6. Phase D, one transaction: apply fills, update the position, append ledger
   transactions, append the state transition, append the audit event.
7. The response carries the order, its fills, the risk verdict with every check
   that ran, and whether the submission was a duplicate replay.

`docs/ORDER_LIFECYCLE.md` covers the gates and the failure semantics in detail.

## Data flow: an automated evaluation

The scheduler runs enabled PAPER strategies every 30 seconds under a lease, so
two instances cannot double-run a strategy. For each candidate the orchestrator
refuses before it asks: account lifecycle, market session, reconciliation
state, data health, authority, kill switch. Only then does it call the research
service for a signal.

If a signal is actionable, the orchestrator sizes it **from the account's risk
budget, ignoring whatever size the strategy suggested**, and submits an order
intent through the same OMS pipeline a human uses. A strategy cannot skip a
gate because there is no other pipeline to skip it into.

## Messaging: a transactional outbox, not a broker

Events that must reach something outside the transaction (notifications,
downstream projections) are written to an `outbox` table in the same
transaction as the state change, and dispatched by a scheduler job every three
seconds.

This is deliberately not Kafka. A message broker introduces a second store that
can disagree with Postgres about whether a trade happened; the outbox pattern
makes "the state changed" and "the event exists" the same commit. At this scale
the operational cost of Kafka buys nothing that matters.

## Time

One `Clock` interface, injected. Nothing calls `time.Now()` directly in
business logic, which is what makes the session, blackout and expiry rules
testable.

Market sessions resolve in the venue's IANA zone (New York for the FX and
metals calendar), so the weekly open and close move correctly across
daylight-saving changes instead of drifting an hour twice a year. Timestamps
are stored in UTC and truncated to microseconds — Postgres `timestamptz`
resolution — so a value read back hashes identically to the value written,
which the audit chain depends on.

## Money

`money.Amount` is a `shopspring/decimal` value plus a mandatory currency.
`Add`, `Sub` and `Cmp` return `ErrCurrencyMismatch` across currencies rather
than doing the arithmetic. The zero value has an *empty* currency, so an
uninitialised field cannot silently join a calculation as "0 ZAR".

Conversion is explicit and refuses rather than guessing: with no USD/ZAR rate
available, a gold position in a rand account cannot be valued, and the platform
declines to trade instead of inventing a rate. `docs/RISK_ENGINE.md` covers
what that refusal looks like from the operator's side.

## Scale and what would change

The current shape suits one operator, five instruments and one venue. Three
things would change under real load:

- **Market data** would move from a 2-second poll to a streaming provider and a
  push channel to the browser. The `MarketDataProvider` interface and the
  store's shape do not change; the ingestor does.
- **The scheduler** already takes leases, so a second control-plane instance is
  safe to add. Strategy evaluation would move to a work queue rather than a
  serial loop.
- **Reconciliation** would run continuously against a venue event stream rather
  than every five minutes.

None of these require a different architecture, which was the point of putting
the interfaces where they are.
