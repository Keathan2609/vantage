# Market replay

A market replay drives Vantage's **real** pipeline from a fixed dataset, so
autonomous behaviour can be observed and reproduced instead of waited for.

**Everything a replay produces is SIMULATED.** It is not a backtest, not a
historical result, and not a claim about future performance.

## Why it exists

Before this, autonomous behaviour was tested two ways, and neither proved what
matters:

- **Unit tests of the decision layer.** Deterministic and fast, but they stop
  at the verdict. They cannot show that a decision becomes an order, a fill, a
  ledger entry and an audit row.
- **The live generated market.** Real end to end, but you cannot wait for a
  volatility shock, and the venue's 17:00–18:00 New York maintenance break
  makes any suite that waits on a tradable feed simply fail for an hour a day.
  A whole test run was lost to that once.

A replay is the third option: the actual pipeline, on a market you chose.

## The chain it drives

```
ReplayProvider
  → ingestion (Ingestor.IngestOnce)
  → bar backfill (Ingestor.BackfillBars)
  → data-quality checks (domain.EvaluateQuoteHealth)
  → regime, scanner, strategies (research plane)
  → orchestrator + consensus policy
  → portfolio context and position sizing
  → risk engine → trading authority → Autopilot
  → OMS → MockBroker → fills
  → portfolio ledger → audit → metrics
```

**There is no second trading engine.** `Scheduler.ReplayStep` calls the same
job functions the interval loops call, in the same order. What replay changes
is what *moves the clock*: a ticker in ordinary operation, the dataset here. A
harness that called ingestion and the orchestrator itself would prove the
harness worked.

Measured on one 140-bar dataset: 665 signals, 250 orders (189 filled, 61
refused for exposure limits), 189 fills, 64 ledger transactions, 2.51 lots
traded, a gapless ledger, and a stored balance exactly equal to the
ledger-derived one.

## Replay time is application time

`domain.Clock` is a one-method interface so that this substitution is possible,
and nothing in the trading path calls `time.Now()` directly. Engaging a run
therefore moves:

- quote ages, and so every data-quality verdict
- bar buckets
- market and session state, including the daily maintenance break
- strategy scheduling and the per-bar guard
- economic-calendar and news windows
- daily loss and drawdown boundaries
- order and audit timestamps

A run started on a Sunday in December produces the same decisions as one
started on a Tuesday in March. That is the point.

### The one thing replay time must never move

**Security lifetimes.** Session expiry, MFA and TOTP validation read
`s.wallClock`, which is always real time.

This was found the hard way. Engaging a replay dated 2027 instantly expired the
operator's own session, so the authenticated API could not drive the replay it
had just started — a 401 out of nowhere. The reverse is the dangerous
direction: a replay dated in the *past* would have kept an already-expired
session alive. `TestAuthenticationUsesRealTimeNotTheTradingClock` pins it.

### The trade-off, stated plainly

While a run is engaged, **every** clock reader in the process sees replay time,
including ones with nothing to do with trading. That is acceptable only because
replay mode is explicit, refused outside development and test, and never the
default.

## Enabling it

```bash
VANTAGE_MARKET_DATA_PROVIDER=replay    # default is "mock"
```

`config.Load` **refuses** this outside `development` and `test`. A deployment
that asked for replay and silently got live data would be worse than one that
failed to start, because the operator would believe they were watching a
replay.

Ordinary development is unaffected: the default is `mock`, and replay never
activates on its own.

## Controls

Admin-only, and the routes 404 with an explanation when the engine is absent —
404 rather than 403, because the route genuinely does not exist in that
process.

| | |
| --- | --- |
| `GET /api/v1/replay/datasets` | The allowlist, with hashes and spans |
| `GET /api/v1/replay` | Current run, counters, and `replay_now` |
| `POST /api/v1/replay/control` | `start`, `step`, `advance`, `pause`, `resume`, `reset`, `stop`, `speed`, `reconcile` |
| `POST /api/v1/replay/inject` | `outage`, `spread` — market-data faults the dataset cannot express |

`start` and `stop` require a written reason and are audited.

`advance` runs in the **background** and returns immediately; poll
`GET /api/v1/replay`. Held inside one request it exhausted the request deadline
and surfaced as `db: begin: timeout` — a database error for what was really a
control-flow mistake.

`speed` accepts `1x`, `10x`, `100x`, `max`. **Speed changes pacing only.** A
run at 100x produces byte-identical financial output to the same run at 1x; if
it did not, the fast mode would be a different system and the slow mode's
results would not transfer.

## Datasets

CSV, with a fixed column order:

```
instrument_id,timeframe,timestamp,open,high,low,close,volume,bid,ask,spread_fraction,session,source
```

A fixed order rather than a flexible header map: a dataset is financial
evidence, and a column silently landing in the wrong field because someone
reordered a spreadsheet is not a failure mode worth allowing.

Rows carry both a bar and (optionally) a two-sided quote. The pipeline needs
quotes for ingestion, data quality and execution pricing, and bars for
indicators; a dataset supplying only one would force the loader to invent the
other. Where bid/ask are absent, the quote is derived from `close` and
`spread_fraction` — which is recorded on the row, so the derivation is visible
rather than implied.

### Validation

A dataset is rejected for a non-positive low, an inconsistent candle, a crossed
book, a half-specified book, a missing spread where no quote is given, or a
timestamp that does not advance. Every one of those corresponds to something
that has gone wrong somewhere in this repository, and a bad fixture produces a
test that fails for the wrong reason.

### Identity

`Dataset.Hash` is the SHA-256 of the **parsed rows**, not the file bytes: a
comment, a line ending or a reordered file should not change a dataset's
identity, and a changed price must.

### Committed fixtures

Nine, 119 KB in total, generated by
`internal/replay/fixtures/generate.py` — committed so a diff to a CSV is
explainable by a diff to the generator, and closed-form rather than seeded-
random so nobody has to reason about an RNG.

| ID | Scenario |
| --- | --- |
| `trend-clean` | A. sustained uptrend with pullbacks |
| `range-bound` | B. oscillation, no net drift |
| `volatility-shock` | C. calm, then violent |
| `spread-spike` | E. flat price, spread to 2% of mid |
| `drawdown` | H. monotonic decline |
| `trend-reversal` | K. uptrend that reverses |
| `false-breakout` | L. break of structure that immediately fails |
| `correlated-pair` | M. two instruments trending together |
| `day-boundary` | T. crosses UTC midnight and the venue's daily break |

**They are dated 2027 on purpose.** A dataset dated *behind* the seeded market
data makes every replay quote read as `timestamp_regressed` — correctly, since
it genuinely is older — and the run is refused for a reason that looks like a
broken clock.

### Larger datasets

Not in version control. A committed multi-year tick archive would dominate the
repository, be impossible to review in a diff, and slow every clone for data
nobody reads.

Real historical data belongs in object storage addressed by content hash,
referenced by that hash from a run record and fetched on demand. The registry
is an **allowlist of declarations in code** rather than a directory scan, so
adding an external source means adding a loader — not loosening a path check.

### Why a dataset is never named by path

A control endpoint that accepted a filename would be a file-read primitive
wearing a trading-system costume, and no amount of path cleaning makes that a
good idea. IDs resolve through the registry; an unknown ID is a 404 and never
reaches a filesystem call.

## ReplayRun

Every run records: id, dataset id, **dataset hash**, code SHA, **config hash**,
seed, dataset span, wall-clock start and finish, state, counters and any error.

All of it, because a replay result is only evidence if it can be tied to
exactly what produced it. Dataset hash catches an edited fixture; code SHA
catches a changed strategy or risk rule; config hash catches a changed
threshold; the seed catches deliberate randomness. Without all four, "the same
run" is a claim rather than a fact.

The config hash covers only what can change a trading decision — environment,
execution mode, brokers, provider, quant timeout. Not the whole configuration:
a log level should not invalidate a comparison, and a connection string must
never reach a stored record.

### Where it is kept

`replay_runs` (migration 0013). The row is written **before the run is
announced** and updated as the run progresses, so an interrupted replay — a
crash, a kill, a power cut — still leaves a trace. A run whose opening record
cannot be written is **refused**: a result nobody can tie to a dataset, a code
SHA and a configuration is not worth the minutes it costs to produce.

The reverse trade-off applies once the run exists. A recording failure after
the start is logged and counted, never fatal: throwing away real orders, fills
and ledger entries because a bookkeeping write failed would destroy the
evidence to protect the filing system.

Read it at `GET /api/v1/replay/runs` (newest first, `?dataset=` and `?limit=`)
and `GET /api/v1/replay/runs/{id}`. Both are admin-only, and unlike the control
endpoints they answer in **every** process: a recorded run is evidence, and the
engine being absent does not unmake it. Every payload carries `simulated: true`
and the SIMULATED — REPLAY — PAPER label.

`simulated` is a column with a `CHECK (simulated)` on it, so a replay row that
claims to be live is not representable. The control plane may insert and update
a run but **not delete one**; removing a run is the schema owner's deliberate
act.

### What a run cannot claim

A build that carries no commit identity records `code_sha` as `unknown`, which
is what `go run` produces. Two such runs **compare equal on code identity**
while establishing nothing, which is the dangerous direction, so `Start`
appends a durable warning to the run record saying the comparison cannot be
made. The container build stamps a real SHA through ldflags; a development
binary does not, and the record says so rather than implying otherwise.

The `warnings` column also carries the preflight findings — an expired trading
authority, autopilot off. A run with zero orders and a run that was never
permitted to place one are indistinguishable afterwards without them, and that
difference is the whole question a paper-forward run exists to answer.

## PAPER_FORWARD and BACKTEST/REPLAY

Every order created while a run owns the clock is tagged with that run
(`orders.replay_run_id`, migration 0014). An untagged order was decided on a
live simulated feed; a tagged one was decided against a dataset at a dataset
instant.

The tag is what makes the two separable at all. A replay writes real orders
through the real OMS into the real ledger — that is why it is evidence — and
without the tag those numbers sit in the same account as a forward session's,
indistinguishable, so every question about how the platform behaves on a live
feed gets a polluted answer.

Two attribution dimensions read it:

- `?by=run_kind` splits `paper_forward` from `replay`. This is the comparison a
  paper-forward programme rests on.
- `?by=replay_run` separates individual runs, so two replays of one dataset can
  be compared from the ledger rather than by capturing an API response.

A deposit is neither: it has no order, so it lands in the unattributed bucket
rather than being filed as one side of a comparison it is not part of.

The tag is read at order-creation time, not passed in by the caller. A MANUAL
order placed while a replay is engaged was also decided against dataset prices,
and asking each caller to remember that is how half of them forget.

The foreign key means a run with orders attributed to it cannot be deleted, and
it is deliberately not `ON DELETE CASCADE`: cascading would destroy financial
records to tidy a development artefact.

## Determinism

**Proven.** Two runs of one dataset from a byte-identical database
(`pg_dump`/`pg_restore` between them) produce byte-identical financial output:
same signals, orders, fills, quantities, transactions, closing balance and open
positions.

Getting there found the non-determinism. The first attempt produced identical
*decisions* — 665 signals, 250 orders, 189 fills, 2.51 lots — and different
closing balances, 574.81 against 575.38. The venue was seeding its slippage
jitter from `time.Now().UnixNano()`, so fill prices differed.

The venue is now **seeded** in replay mode, not switched to `Deterministic`:
that flag disables jitter and random rejection entirely, which would remove the
reason the mock venue exists. It stays awkward, and awkward the same way twice.

### Preconditions for a comparable run

1. **The same starting database.** Account balance, open positions and the
   ledger all feed the next decision. Snapshot and restore between runs.
2. **A trading authority covering the dataset's dates.** The dev seed grants
   three years; a 90-day window expired before a 2027 dataset even started and
   skipped 610 strategy runs with "Trading authority has expired" — a correct
   refusal for an invisible reason. A preflight check now reports this before a
   run starts.
3. **Autopilot on.** Also reported by preflight.

## What a replay assumes

- **FX rates are held constant.** The account is ZAR and gold is quoted in USD,
  so sizing needs a USD/ZAR rate; seeded rates are dated at seed time and a
  dataset ahead of the seed makes them months stale. Rates are re-stamped at
  each replay instant at their seeded value, so **a replay's P&L contains no
  currency movement.** An explicit assumption rather than a hidden one.
- **Bars come from the dataset, not from aggregation.** Quote-to-bar
  aggregation is detached for the duration of a run: the dataset already
  supplies complete bars, and folding one quote per bar into a bar as well
  would write a degenerate candle over a real one.
- **The interval loops are suppressed.** Both the two-second ingestion loop and
  the replay step used to ingest, so each instant produced two identical quotes
  and 75 of 685 strategy runs were skipped for degraded data the harness had
  manufactured. Reconciliation, outbox and cleanup keep running — a replay
  should exercise the real system's background behaviour, not replace it.
- **A run starts from clean replay market data.** Quotes, bars and health rows
  from a previous run are removed, because a dataset starting earlier than the
  last run's final bar makes the stored quote look like the future. Only rows
  whose provider is `replay` are touched.

## Not yet done

- **Scenarios D, F, G, I, J, N–S** exist as decision-layer tests
  (`internal/orchestrator/scenario_test.go`) but are not yet driven end to end
  through the application. The infrastructure to do so now exists; the
  scenarios have not been rewritten onto it.
- **Regime and model are still not attributed.** No decision records the
  market regime or the model that informed it, so P&L cannot be grouped by
  either. Replay run and PAPER_FORWARD-versus-REPLAY are now attributed; those
  two are what remain.
- **No paper-forward versus backtest comparison.** Both exist; nothing compares
  them.
