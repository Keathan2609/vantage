# Broker adapters

One interface, `broker.Adapter`, is the only way an order reaches a venue. The
mock venue implements it; nothing else does in this build.

## The interface is shaped by what venues do, not by one venue's API

Four assumptions are baked in, and each of them costs something in the
interface design:

1. **Requests may be lost.** Every mutating call carries a client-supplied
   `ClientOrderID` so the venue, or the adapter, can recognise a retry.
2. **Responses may be lost.** An adapter must therefore be able to ask the
   venue what it believes the state is — `FetchOrderByClientID` exists
   precisely so a lost-response retry can ask "did you already take this?"
   instead of guessing.
3. **Fills are asynchronous, partial, out of order and replayable.**
   `PollExecutions` is a pull model with a cursor rather than a push callback,
   because a pull is replayable: after a crash the caller resumes from its last
   processed cursor instead of losing whatever arrived while it was down.
4. **The venue is the authority.** Where Vantage and the venue disagree,
   Vantage is wrong until proven otherwise.

What is deliberately **excluded** is the assumption that an error means nothing
happened:

> An error from `PlaceOrder` does NOT mean the order was not placed. It means
> the outcome is unknown, and the caller must reconcile.

That single sentence is why `FAILED` is a non-terminal order state and why
Vantage never automatically retries a placement.

## The surface

| Method | Purpose |
| --- | --- |
| `Name()` | Stable identifier stored on every order |
| `Capabilities()` | What the venue supports, so the pipeline can refuse rather than emulate |
| `Health(ctx)` | Connectivity; a degraded verdict is a normal answer, not an exception |
| `PlaceOrder` | Submit, with a client order id |
| `CancelOrder` | Request cancellation — which races fills |
| `FetchOrder`, `FetchOrderByClientID` | The venue's view of one order |
| `FetchOpenOrders`, `FetchPositions`, `FetchAccount` | Authoritative state for reconciliation |
| `PollExecutions(since)` | Replayable execution stream |

`Capabilities` is what lets the OMS refuse an unsupported order type instead of
synthesising it locally. A platform that emulates a stop order the venue does
not support ends up holding a stop that only exists while the platform is
running — which is the worst possible place for a stop to live.

## The mock venue

`internal/broker/mock` is not a stub that returns success. It is a small
simulated venue with its own books (`mock_venue_accounts`,
`mock_venue_orders`, `mock_venue_positions`, `mock_venue_fills`), and it models
the behaviour that breaks naive trading code:

| Behaviour | Why it is modelled |
| --- | --- |
| Spread crossing | A buy fills at the ask, a sell at the bid. Filling at the mid makes every strategy look better than it is |
| Slippage | Fills drift against the order, proportional to size |
| Latency | A submission is not instantaneous, so races are reachable in tests |
| Partial fills | Large orders fill in pieces, exercising `PARTIALLY_FILLED` |
| Margin refusal | An unaffordable order is *rejected by the venue*, not silently sized down |
| Resting-order triggering | Stops and limits trigger against real ingested prices, not on a timer |
| Venue-side dedupe | A repeated `ClientOrderID` returns the original order, as a real venue would |

Having its own books matters: reconciliation compares Vantage's records against
the *venue's* records, and a mock that shared Vantage's tables would make
reconciliation a tautology. Because the mock keeps its own state, a genuine
discrepancy can be created in tests and the reconciler has something real to
find.

## What the mock deliberately does not do

- It does not simulate weekend gaps or flash moves.
- It does not disconnect mid-order unless a test makes it.
- It is cooperative: it does not return malformed payloads, wrong instruments
  or duplicated fill ids with different quantities.

A real adapter faces all of these, which is the honest reason a passing mock
suite is not evidence that a live integration is safe.

## Adding a real adapter — the actual work

The interface is the small part. What a live integration needs before it should
be trusted:

**Credentials.** Encrypted at rest under the existing versioned AEAD, bound to
the connection row via associated data, never returned by any endpoint, never
logged, never placed in a decision snapshot or audit metadata. The response
types must have no field capable of carrying them.

**Symbol mapping.** Venue symbols do not match internal instrument ids
(`XAUUSD.m` here is one venue's micro-contract naming). The mapping belongs in
the adapter, with a refusal for any unmapped symbol — never a best-effort
guess.

**Contract specifications from the venue.** Contract size, tick size, minimum
and maximum quantity, quantity step, margin rate and commission must be read
from the venue rather than assumed from a seed file, and re-read when they
change. Sizing arithmetic is only as correct as these numbers.

**Reconnection with replay.** On reconnect, `PollExecutions` from the last
processed cursor and reconcile before resuming automation. A reconnect that
resumes trading first is how a stale position book gets traded against.

**Rate limits and backoff.** Venue-side limits, with jittered backoff, and no
automatic retry of a placement whose outcome is unknown.

**Clock skew.** Venue time versus local time, made explicit rather than
assumed. Session boundaries and expiry both depend on it.

**Hostile-input handling.** Malformed responses, unexpected fields, duplicated
fill ids, negative quantities and prices that fail a sanity band must all be
rejected loudly rather than written into the ledger.

## MetaTrader 5 specifically

MT5 is the intended eventual venue, and the interface fits it — but not
without work worth stating in advance:

- MT5's API is a Windows-oriented terminal integration rather than a plain
  REST service. Reaching it from a Go service means a bridge process, and that
  bridge becomes a component with its own failure modes, its own credentials
  and its own place in this threat model.
- MT5 uses *deals*, *orders* and *positions* as three distinct concepts with
  netting or hedging semantics depending on account type. Mapping them onto
  this platform's order-and-position model is the substantive part of the
  integration and must be exercised against a demo account before anything
  else.
- Position identity is venue-assigned and can change on partial closes under
  netting.

None of that is built. **No live or demo adapter exists in this repository, no
real credential has been requested or stored, and no external venue has been
contacted.** Enabling a real adapter re-opens `docs/THREAT_MODEL.md` in full;
it is not a configuration change.

## Registry and mode enforcement

`broker.Registry` is populated at start-up from validated configuration. In
this build that means exactly one entry, `mock`. `config.Load` refuses any
broker name other than `mock`, `BuildAllowsLiveExecution` is a compile-time
`false`, and `broker_connections_mock_only_ck` refuses a non-mock connection
row at the database level.

The OMS looks the adapter up by name from the account's broker. An account
whose adapter is not registered cannot trade at all — the failure is a refusal
at submission, not a nil dereference at execution time.
