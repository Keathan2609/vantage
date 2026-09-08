# Reconciliation and recovery

Vantage's records are a **cache** of the venue's, and caches go stale.
Responses get lost, processes die mid-write, brokers cancel orders on their
own, and people place trades in the broker's own terminal. Any system that
assumes its local records match the venue will eventually size a position
against exposure that does not exist.

Detecting that is the easy half. This document is mostly about the hard half:
**repairing what can be proven and refusing to guess at the rest.**

## Where the venue wins, and where nothing wins

The venue holds the money, so where the two views disagree the venue is
authoritative about what happened. But "the venue is right" is not the same as
"Vantage can safely write down what the venue says", and conflating those two
is how a reconciliation subsystem starts inventing trades.

The governing rule:

> Repair automatically only where the correct repair is **provable** from the
> evidence. Not likely, not usually — provable. Everything else waits for an
> operator, and an operator who is shown the evidence rather than a summary of
> it.

The asymmetry is deliberate. An unresolved issue costs a halted account, which
is recoverable. A wrong automatic repair writes a number into an append-only
ledger, which is not.

## The shape of a run

1. take the account's reconciliation lock, or decline to run
2. capture the **venue's** snapshot
3. capture **Vantage's** snapshot
4. classify the differences — a pure function, no I/O, no repairs
5. persist each as an issue, deduplicated by fingerprint
6. apply the repairs the policy calls provable
7. close issues whose divergence has gone
8. record the run and recompute the trading verdict

Steps 2 and 3 happen **before any repair**. An earlier implementation
interleaved venue lookups, local queries and repairs, which had three problems
and the third is the serious one:

- a repair changes local state, so a later comparison in the same run sees a
  different local view and the run is not self-consistent
- the evidence behind a decision is gone once the loop moves on
- **the run is not reproducible** — given the same divergence, two runs could
  classify it differently depending on timing, which for a process that writes
  to the ledger is not acceptable

Capturing both sides first makes a run a pure function of two snapshots plus
the repair policy. That is what makes the classifier unit-testable with no
database and no venue, which is where most of its test coverage lives
(`internal/reconcile/classify_test.go`).

### One trap worth naming

`FetchOpenOrders` returns only OPEN orders, which is the correct contract — but
an order the venue **filled** is no longer open, so it is absent from that
list. Concluding "the venue never heard of this" from that absence would
release the risk budget for a position that exists.

This was not hypothetical. A lost-response order was classified
`ORDER_MISSING_AT_BROKER` while the venue held it as filled with an execution
against it; only the repair machine's terminal-state guard stopped it being
marked rejected. Every local order the open list does not cover is now resolved
by a **direct lookup on its client id**. A direct not-found is evidence;
absence from a filtered list is not.

## Issue taxonomy

Thirteen types, deliberately narrow. An earlier version recorded
`fill_quantity_mismatch` for both "the venue has an execution we can prove
belongs to order X" and "the venue has an execution we cannot attribute to
anything" — one of which is safely repairable and one of which must never be
touched automatically. Collapsing them meant neither could be handled.

| Type | Repair class | Halts | What it means |
| --- | --- | --- | --- |
| `FILL_MISSING_LOCALLY` | **automatic** | account | The venue reports an execution attributable to exactly one Vantage order. The central repairable case |
| `ORDER_MISSING_AT_BROKER` | **automatic** | account | An order with no venue id that a direct client-id lookup says the venue never had |
| `ORDER_STATUS_MISMATCH` | **automatic** | account | Both sides know the order, statuses differ, **and filled quantities agree** |
| `OUT_OF_ORDER_EXECUTION_REPORT` | **automatic** | none | An execution arrived out of sequence. Harmless by construction |
| `EXTRA_BROKER_FILL` | operator | account | An execution attributable to no order, or to several |
| `PARTIAL_FILL_MISMATCH` | operator | account | Quantities disagree and no execution explains the difference |
| `POSITION_MISMATCH` | operator | account | The position books disagree |
| `BALANCE_MISMATCH` | operator | **none** | Ledger and venue balances differ beyond tolerance |
| `ORDER_MISSING_LOCALLY` | operator | account | The venue works an order Vantage has no record of |
| `UNKNOWN_EXECUTION_STATE` | operator | account | Vantage cannot determine whether an order executed |
| `DUPLICATE_EXECUTION_REPORT` | operator | account | The venue re-reported an execution id with **different content** |
| `VENUE_ID_MISMATCH` | unresolvable | **connection** | Vantage holds a venue id the venue denies |
| `EXTERNAL_BROKER_ACTIVITY` | unresolvable | account | State at the venue that Vantage did not cause |

The authoritative version is `internal/domain/reconciliation.go`, where each
entry carries a `Rationale` explaining its classification. That rationale is
served to the operator alongside the issue and published at
`GET /api/v1/reconciliation/taxonomy`, so the terminal renders the same policy
the server enforces rather than re-implementing it.

### Why each automatic repair is safe

**`FILL_MISSING_LOCALLY`** — the execution carries a venue order id that maps
to exactly one Vantage order, and its instrument and side agree with that
order. Booking is idempotent on the venue's execution id (a unique index on
`(broker_name, broker_fill_id)`), so applying it twice is impossible.
Attribution is conjunctive and strict: a venue reporting the *opposite side*
for an order id we recognise is describing a mapping error, and booking it
would move the position the wrong way, so it is refused rather than attributed.

**`ORDER_MISSING_AT_BROKER`** — safe *only* on the strength of a direct
client-id lookup returning not-found, and *only* for an order holding no venue
identifier. That combination proves the order never reached the market. An
order that **does** hold a venue id the venue denies is a `VENUE_ID_MISMATCH`
instead, and closing it out would free risk budget for a position that may
exist.

**`ORDER_STATUS_MISMATCH`** — the venue is authoritative about its own order
states, but adoption requires that the **filled quantities already agree**. A
status difference accompanied by a quantity difference is a *missing
execution*, and relabelling the order would leave it marked FILLED with no
fills behind it — breaking the invariant that position quantity equals
fill-derived quantity. That case is classified `PARTIAL_FILL_MISMATCH` and
repaired by booking the execution, not by relabelling.

### Why each operator case is not automatic

**Ambiguous executions.** An execution that cannot be attributed to exactly one
order is where automatic repair is most tempting and most dangerous: guessing
the parent order writes a real position and a real profit or loss against the
wrong one.

**Positions and balances are derived, never inputs.** A position is the
consequence of executions; a balance is the running total of an append-only
ledger. Writing either to match a snapshot would produce a value nothing
explains. The repair is to find and book the executions that account for the
difference — which is a different issue type. No action available for a
position or balance mismatch writes financial state, and a test asserts that.

**External activity.** Vantage cannot invent the risk decision, authority check
and intent that every order carries. Manufacturing them would put a fabricated
audit trail in the ledger. It is reported as external and never recorded as
though Vantage had placed it.

## Fill ingestion: one accounting path

An execution can reach Vantage two ways — returned by a `PlaceOrder` call, or
discovered later by reconciliation. **Those two paths must produce identical
accounting**, and "must" is not a strong enough guarantee when the code exists
twice.

So it exists once, in `internal/booking`, and both callers go through it. An
architecture test (`TestOnlyBookingAppendsFills`) asserts that nothing else
appends a fill, which makes the guarantee structural rather than aspirational.

The specific failure this prevents: a reconciliation importer with its own
`INSERT` that wrote a fill and a position but skipped the ledger entry. No
constraint ties a position to a transaction, so the schema would not object,
and the account would carry a position no money movement explains.

Every imported execution is validated before anything is written — identifier
present, account and instrument match, side matches, quantity and price
positive, no overfill — because a constraint violation arrives as an aborted
transaction that also rolls back the repair of every other issue in the same
unit of work.

A fill records **how it arrived** (`ingest_source`) and, when imported, the
issue that justified it. The ledger line says so too: "Realised P&L on XAUUSD.m
(recovered by reconciliation)". An operator reading a statement should not have
to join to another table to discover an entry was reconstructed after a
divergence.

## Repair transitions are distinguishable from normal ones

Reconciliation needs moves the normal state machine forbids — `ACCEPTED →
FILLED` being the important one, because that is the state a lost response
leaves behind.

The obvious shortcut is to add that transition to the normal table. That would
be a mistake: `ACCEPTED` means "persisted, not yet sent", so a fill arriving in
that state during ordinary execution is an OMS bug, and the state machine
refusing it is how that bug gets caught. Widening the table would remove the
check that makes normal execution safe in order to describe an exceptional
path.

So recovery has its **own** table (`domain.CanRepairTransition`). Every normal
transition is also a legal repair; the converse does not hold. What stays
forbidden either way: **leaving a terminal state.** A FILLED order does not
become CANCELLED because a snapshot disagreed — that is a contradiction to
investigate, not a state to overwrite.

Every repair records the previous state, the new state, the issue id, the
evidence, the actor, the timestamp and the correlation id, and is stamped
`is_repair` in `order_state_transitions`. A schema constraint refuses a repair
that names no issue. Reading `SUBMITTED → FILLED` without that stamp cannot
tell "the venue told us at the time" from "we reconstructed this three hours
later from a snapshot".

## Unknown execution state

`FAILED` means "Vantage does not know the venue-side outcome". That is
correct, and it is why FAILED is not terminal — but the name reads as a closed
failure, and an operator needs to see uncertainty **as** uncertainty.

So orders carry `reconciliation_required`, set in the same transaction that
moves an order to FAILED. It is deliberately a flag and not a new order status:
adding a status would mean widening the transition table for every ordinary
execution in order to describe an exceptional condition.

An order in that state:

- blocks nothing from being retried, because it is **never** retried — a retry
  of an unknown outcome is how one intended trade becomes two positions
- appears in the operations view and in `GET /reconciliation/{id}/issues`
- counts toward the account's readiness verdict
- halts automated trading for the account
- raises an alert
- is picked up by the next reconciliation run even though the state machine no
  longer considers it open

It is **not** collapsed into a tidy state. Recording it as rejected would free
the risk budget for a position that may exist; recording it as filled would
invent one that may not.

## Halt and resume

The verdict is **derived** from open issues and the account's last successful
run, every time it is asked for. Storing it would be cheaper and is exactly the
design to avoid: a stored verdict can disagree with the issues it summarises,
and the failure mode is an account that trades automatically because a flag was
not cleared.

| State | Automation | Meaning |
| --- | --- | --- |
| `HEALTHY` | permitted | Reconciled recently, nothing unresolved |
| `DEGRADED` | permitted | A warning-level divergence, or the last run is stale |
| `RECONCILIATION_REQUIRED` | **stopped** | Never reconciled, or the last run failed |
| `TRADING_HALTED` | **stopped** | An unresolved divergence that halts |

`RECONCILIATION_REQUIRED` is the state worth explaining. "No open issues" is
true of an account nobody has ever checked, and that is not the same as
agreement with the venue. Nothing is known to be wrong, but nothing is known to
be right either, and that is not a state to trade automatically from.

**Blast radius is the minimum necessary.** An uncertain gold execution on one
account halts that account; it does not stop an unrelated one. The single
exception is `VENUE_ID_MISMATCH`, which halts the whole broker connection —
every other repair on that connection relies on the identifier mapping being
sound, so while it is in doubt none of them can be trusted. No issue type halts
all trading globally; a global stop is a kill switch, applied deliberately.

**What a halt does not stop:** manual trading, and every read path. An operator
can see the warning and decide; an algorithm cannot. This is stated on the
operations page too, because it is the most common misreading of a halted
system.

In the multi-user architecture the scope stays per-account by construction: the
verdict is computed per account from that account's issues, and the
connection-scoped case consults only accounts on the same broker connection.

### The halt is durable, not timing-dependent

Checking the halt before opening a transaction leaves a real window: an
automated order can pass the check microseconds before a repair writes a
critical issue, and still commit. The window is small, and "small" is not a
property to rely on when the consequence is an automated order placed against a
book known to be wrong.

So the OMS re-reads the halt **inside** the transaction that persists the
order, after taking the account row lock that a repair also takes. Either the
order commits before the issue exists, or it sees the issue — decided by
PostgreSQL's serialisation rather than by luck.

## Start-up and periodic runs

On start-up every account is reconciled before automated trading is permitted:
an account whose state is unproven reports `RECONCILIATION_REQUIRED` and the
orchestrator will not run a strategy on it.

Periodic runs are bounded three ways, and each covers a case the others do not:

- a **scheduler lease** stops two control-plane instances reconciling at once
- a **per-account advisory lock** stops a scheduled run overlapping a manual
  one, or overlapping the previous tick
- a **timeout** (4 minutes, inside a 5-minute interval) stops a venue that
  accepts a connection and never answers from holding the lease until restart

Without the timeout the first two are worthless: a run blocked on a socket
holds both indefinitely, and reconciliation silently stops happening while
reporting no error at all.

The lock is **session-scoped**, because a run spans several transactions by
design — it reads a snapshot with no transaction open, then repairs each issue
in its own unit of work so one failure does not roll back the others. A
transaction-scoped lock would be released in exactly the window a second run
must not enter. It is an advisory lock rather than a row so that PostgreSQL
enforces it across processes and releases it automatically if the process dies;
a `locked_until` column would leave a stale lock needing a timeout nobody can
choose correctly.

Overlapping runs **decline** rather than queue, and the API answers `409`. A
run that waited would eventually reconcile from a stale snapshot, and a caller
that read an empty report from a skipped run would conclude the account was
clean.

## Idempotence

Reconciliation runs every five minutes, so "converges once" is not enough — it
has to converge and then stop.

- an **issue** is identified by the problem, not the run that found it. A
  fingerprint plus a partial unique index on unresolved rows means a repeated
  detection touches the existing issue and increments a sighting count, rather
  than inserting a row per detection
- a **fill** is deduplicated by the venue's execution id at the database level
- a **repair transition** is a no-op when the order is already in the target
  state
- an issue whose divergence has **gone** is closed by the next run. Without
  this every transient divergence would halt an account permanently: a snapshot
  taken mid-execution raises a mismatch, the next run finds the quantities
  agree, and nothing would ever revisit the original issue

An ordinary execution **replay** is not recorded at all. The execution cursor
overlaps by a minute on purpose, so every poll re-sees recent executions — that
is the expected steady state, not an event. An earlier version raised an info
issue for each one, which produced dozens an hour that said nothing and made
the run report's repair count meaningless. What *is* recorded is the venue
re-reporting an execution id with **different content**, which means one of the
two records is wrong about a trade that happened.

## Operator actions

`POST /api/v1/reconciliation/{accountID}/issues/{issueID}/resolve`, **admin
only**. This is the most dangerous route in the API: it can book an execution
into an append-only ledger.

| Action | Writes financial state | What it does |
| --- | --- | --- |
| `ACKNOWLEDGE` | no | Records that a human judged it benign |
| `RECHECK` | no | Re-runs detection; closes the issue only if the divergence is genuinely gone |
| `RESOLVE_MANUALLY` | no | Records that it was resolved outside Vantage |
| `IMPORT_BROKER_FILL` | **yes** | Books the execution the venue reported |
| `MARK_BROKER_REJECTED` | **yes** | Records a definitive venue refusal |
| `MARK_NOT_EXECUTED` | **yes** | Records that the order never reached the market |
| `LINK_BROKER_ORDER` | **yes** | Attaches a venue order id to a Vantage order |

**There is deliberately no "set order status" operation.** An endpoint
accepting an arbitrary target state would let anyone with the admin role write
any number into the ledger, with an audit trail whose only content is that
somebody asked for it. That is not a control; it is a control-shaped hole. The
API refuses the action by name and says why.

The caller chooses **which remedy, never what the numbers are.**
`IMPORT_BROKER_FILL` books the execution reconstructed from the issue's own
stored evidence — not one the caller supplies. A caller able to name the
quantity and price would be writing arbitrary values into the ledger with an
operator's authority attached.

Every action:

- requires the admin role, enforced at the route rather than inside the handler
- must be in the issue type's `allowed_actions`, so a fill cannot be imported
  against a balance mismatch
- requires a reason of at least ten characters — the only durable record of
  **why** a financial repair was applied
- is refused if the issue is already resolved (`409`), so two operators
  clicking at once cannot both act
- writes an audit event **and** an issue-history row, on failure as well as
  success: an audit trail that records only successes cannot answer "did anyone
  try"
- loads the issue scoped to the account, so a forged id from another account is
  not-found rather than data — and not-found rather than forbidden, because
  distinguishing them would confirm the id exists

`LINK_BROKER_ORDER` is refused if the venue id already belongs to a different
Vantage order. Two orders claiming one venue execution is the ambiguity this
subsystem exists to avoid creating.

### Why admin, when admin cannot trade

Separation of duties runs in both directions. A trader may place orders and
flatten positions but may not resolve a divergence; an admin may resolve a
divergence but may not trade. Resolving can write to the ledger, so it belongs
to the role that is barred from trading.

One consequence had to be handled explicitly: an admin owns no trading account
in this build, so scoping the reconciliation routes by ownership made them
unreachable for the only role permitted to use them — every call answered
"Account not found". Those routes resolve the account with an
operations-scoped lookup that widens to any account for an admin, and an
architecture test asserts that every reconciliation handler uses it. Every use
is audited with the actor, the account, the action and the reason.

## Alerting

Raised for: an issue detected, an automatic repair applied, a reconciliation
failure (escalating to critical after three in a row), trading halted, trading
resumed, and the per-run mismatch summary.

Automatic repairs are announced even though they succeeded. Software writing to
an append-only ledger on the strength of a venue snapshot is worth knowing
about *especially* when it is correct: an operator who never hears about
repairs cannot notice that they have started happening every day.

Storms are avoided by keying each alert on the thing it is about — an issue
type per account, a repair per action — and by a per-kind cooldown. A
reconciliation *failure* is rate-limited because a venue that is down fails
every scheduled run; the suppressed count still reports how many, so "it has
been failing for an hour" stays visible.

## Identifiers

| Identifier | Generated by | Unique across | Used for |
| --- | --- | --- | --- |
| `orders.id` | Vantage | all orders | Vantage's own reference |
| `orders.command_id` | Vantage | all commands | **The client order id** sent to the venue |
| `orders.idempotency_key` | the **client** | per account | Refusing a duplicate submission |
| `broker_order_id` | the **venue** | per venue | Correlating an order with the venue |
| `broker_fill_id` | the **venue** | per venue | Deduplicating an execution |

Vantage supplies `command_id` as the client order id on every order it places.
That is what makes a venue order carrying no client order id recognisable as
**external** activity rather than a lost record.

A future adapter may weaken these guarantees — a venue that does not echo a
client order id, or reuses execution ids across days. `Capabilities`
(`SupportsClientOrderID`) exists to say so, and an adapter whose guarantee is
weaker must implement its own deduplication and declare it. Nothing in this
subsystem assumes every provider behaves like the mock.

## External and manual broker activity

Vantage cannot yet reach a real venue, so this cannot happen today. The policy
exists because it will:

- a position opened or closed in the broker's own terminal
- an order cancelled there
- a stop or target moved there

Such state is reported as `EXTERNAL_BROKER_ACTIVITY`, classified unresolvable,
and **never** recorded as though Vantage had originated it. Every downstream
record — decision snapshot, risk check, authority — would be a fabrication. It
is acknowledged by an operator, and the venue's position book is treated as
authoritative for risk purposes without claiming Vantage placed the trade.

## What reconciliation still cannot do

- **Repair a position or balance mismatch.** It detects one and halts
  automation; the repair is to find the executions that explain it. Where they
  cannot be attributed, an operator must establish the truth from the venue's
  own statements.
- **Reconstruct beyond the venue's history window.** If a venue's execution
  history is shorter than an outage, the ledger has a hole reconciliation
  cannot close.
- **Resolve a contradiction.** When the venue re-reports an execution with
  different content, or denies an id Vantage holds, there is no safe write in
  either direction.
- **Prove a negative across a partial snapshot.** Every venue fetch failure
  fails the whole run, precisely because a partial snapshot compared against a
  complete local view manufactures divergence.
- **Run without a venue.** A venue that cannot be reached is not agreement:
  the run is recorded failed and the account reports
  `RECONCILIATION_REQUIRED`.

## Trying it

The deterministic fault modes in `internal/broker/mock` reproduce each scenario
exactly, and the development endpoint arms them:

```bash
curl -X POST http://localhost:8080/api/v1/dev/broker-faults \
  -H "X-Vantage-CSRF: $CSRF" -H 'Content-Type: application/json' \
  -d '{"fault":"lost_response","times":1}'
```

`lost_response` is the important one: the mock venue commits the order and its
fills, then returns `ErrUnknownOutcome` instead of the acknowledgement. From
Vantage's side that is indistinguishable from a process that died between the
broker call and its own persistence.

The end-to-end proof is
`tests/race/recovery_test.go::TestALostFillIsDiscoveredImportedOnceAndConverges`:
order submitted, venue fills, response lost, reconciliation runs, execution
discovered and attributed, imported exactly once, order and position and ledger
converge, automation resumes.
