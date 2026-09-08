# Reconciliation

The premise, stated in the package comment because it drives every decision
here:

> Vantage's state is a **cache** of the venue's, and caches go stale.

Responses get lost, processes die mid-write, brokers cancel orders on their own,
and people place trades in the broker's own terminal. Any system that assumes
its local records match the venue will eventually size a position against
exposure that does not exist.

## Where the venue wins

Where Vantage and the venue disagree, **the venue wins. It holds the money.**

This is not a tie-break rule, it is the whole design. Local state is corrected
towards the venue's; the venue is never corrected towards ours. An adapter that
"fixed" the venue to match local records would be turning a reporting problem
into a trading problem.

## When it runs

| Trigger | When |
| --- | --- |
| `startup` | Before the control plane begins accepting orders |
| `scheduled` | Every 5 minutes, under a scheduler lease |
| `manual` | An operator presses Reconcile now |
| `reconnect` | After a broker connection is re-established |
| `post_failure` | After any order whose outcome was unknown |

`post_failure` is the important one. An order in `FAILED` means Vantage does
not know what the venue did; reconciliation is the mechanism that finds out,
and until it does, automation for that account is blocked.

## What is compared

| Comparison | Source of truth |
| --- | --- |
| Open orders | `FetchOpenOrders` |
| One order's state, by broker id or client order id | `FetchOrder` / `FetchOrderByClientID` |
| Positions: existence, side, quantity, entry price | `FetchPositions` |
| Account balance and equity | `FetchAccount` |
| Executions since the last cursor | `PollExecutions` |

`FetchOrderByClientID` is what makes a lost response recoverable: the client
order id was ours, so we can ask "did you already take this?" rather than
guessing or resubmitting.

## Discrepancy classes

| Class | Example | Severity |
| --- | --- | --- |
| Missing locally | The venue has a position Vantage does not know about | **critical** |
| Missing at the venue | Vantage believes a position is open that is not | **critical** |
| Quantity mismatch | Sizes differ | **critical** |
| Side mismatch | Direction differs | **critical** |
| Order state mismatch | We say `SUBMITTED`, venue says `FILLED` | resolvable |
| Unknown order resolved | A `FAILED` order turns out to have filled | resolvable |
| Balance drift | Small differences from financing or rounding | informational |

Resolvable classes are applied automatically: the local order is transitioned
along a legal path, the fill is recorded once (`fills_broker_fill_uniq` makes a
double-record impossible), the position is recomputed and the ledger is
appended. Every correction is audited with the trigger and the run id.

## What a critical discrepancy blocks

Unresolved critical discrepancies **halt automated trading for the account**.

> Trading on a position book that is known to be wrong is worse than not
> trading.

They do not halt manual trading, and they do not close anything. An operator
can still act — they can see the discrepancy, its class and both views, and can
decide. What they cannot do is let a scheduler size a new position against
exposure the platform is not sure about.

The block is visible in three places: the reconciliation panel on the Authority
page, the `automation_blocked` field on `GET /reconciliation/{accountID}`, and
the `reconciliation_unresolved_discrepancies` metric.

## Why a pull model

`PollExecutions(since)` takes a cursor rather than registering a callback. A
push stream loses whatever arrives while the consumer is down; a cursor is
replayable, so after a crash the platform resumes from the last processed point
and re-reads rather than skipping.

The cost is latency — executions are noticed on the next poll rather than
instantly. For a platform that is not making markets, that is the right trade.

## Balance drift

Small differences between our computed balance and the venue's are expected:
overnight financing, rounding, and fees applied at times the venue chooses. They
are recorded as informational rather than treated as corruption, because
escalating every cent of swap would make the critical signal useless.

The threshold is deliberately conservative, and drift that grows rather than
oscillating is what deserves attention — which is why the runs are stored
(`reconciliation_runs`) rather than only the latest verdict.

## What reconciliation cannot do

- It cannot see a position between the moment it is opened at the venue and the
  next run. That window is up to five minutes for scheduled runs, and is why
  `post_failure` and `reconnect` triggers exist.
- It cannot reconstruct a fill the venue no longer reports. If a venue's
  history window is shorter than an outage, the ledger will have a hole that
  must be closed manually.
- It cannot decide whether a critical discrepancy was a bug or a manual trade
  in the broker's terminal. Both look identical from here, which is why
  resolution is an operator action and not an automatic overwrite.
- Against the mock venue, it is only as adversarial as the mock: the mock keeps
  its own books, so genuine discrepancies are reachable in tests, but it does
  not lie, duplicate or contradict itself the way a real venue occasionally
  will.
