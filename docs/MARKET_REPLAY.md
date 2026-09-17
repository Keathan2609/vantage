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

Fourteen, 170 KB in total, generated by
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
| `conflicting-signals` | G. a trend that splits the strategy set |
| `event-window` | D. a tradable trend, used with releases scheduled inside it |
| `capacity-exhausted` | I. a tradable trend, used with risk capacity removed |
| `news-agreement` | N. a tradable trend, used with news published across it |
| `test-partial-fill` | P support. the synthetic instrument, so a split fill is representable |

**Each needs its OWN date range, and they are spaced two weeks apart.** A
strategy is evaluated once per completed bar for ever — that is recorded in
`strategy_runs` — so two datasets covering the same hours cannot both be
replayed into one account: the second finds every bar already evaluated.

**`correlated-pair` and `day-boundary` were lengthened to 180 for the same
reason.** At 120 and 90 they produced, respectively, decisions with no
`portfolio_correlation` check recorded and no decision at all — the correlation
matrix needs paired history of its own, and thirty instants past the warm-up is
not enough for a strategy to act. The market shapes are unchanged; there is
more of each.

**The condition fixtures are 180 instants, not 120, and that was measured.**
The first sixty are warm-up, and the seeded strategy set produces nothing
actionable until roughly 120 bars of history exist: a 120-instant version of
the same trend yielded 288 `no_trade` signals and ONE sell, while the 140-bar
`trend-clean` yields 46 decisions. A condition applied to a market that never
traded tests nothing.

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

**Proven, and measured on a canonical digest rather than on a list of
assertions.** `GET /api/v1/replay/digest/{accountID}` hashes the account's
financial output — decisions, orders, fills, ledger, positions, attribution and
reconciliation — ordered by BUSINESS keys only, never by a generated id or a
wall-clock timestamp. Three runs of one dataset from a byte-identical database
(`pg_dump`/`pg_restore` between them) produce the same digest, and the
per-section components name WHICH part diverged when they do not.

**Re-measured at the milestone's final code state**, after decisions started
recording the bar they were taken on — a richer canonical form that could have
exposed non-determinism the poorer one hid, and did not:

| Dataset | Digest | Runs |
| --- | --- | --- |
| `trend-clean` | `45f2cf72fc4a0397` | 3, identical |
| `range-bound` | `ca00376e75f72e42` | 3 identical, and **SKIPPED** — it produced no decision of its own, and agreeing about nothing is not evidence |
| `correlated-pair` | `1638d574c83ea41d` | 3, identical |

**A digest is only comparable within one code version.** It is a fingerprint of
what this build produces, so a change to what is recorded — a column added to
the canonical form, or a field that started being populated — moves every
digest. That is why a run record carries `code_sha`, and why the digests quoted
in this document are tied to the commit that measured them rather than being
golden values to assert against.

A digest proves REPRODUCIBILITY and nothing else. It says a result can be
re-derived; it says nothing about whether the result is good, and two runs of a
losing strategy agree on the same digest.

Getting there found the non-determinism. The first attempt produced identical
*decisions* — 665 signals, 250 orders, 189 fills, 2.51 lots — and different
closing balances, 574.81 against 575.38. The venue was seeding its slippage
jitter from `time.Now().UnixNano()`, so fill prices differed.

The venue is now **seeded** in replay mode, not switched to `Deterministic`:
that flag disables jitter and random rejection entirely, which would remove the
reason the mock venue exists. It stays awkward, and awkward the same way twice.

### Speed invariance

**Measured.** Four modes over the same 140-instant dataset from one snapshot:

| Mode | How it is driven | Wall clock | Digest |
| --- | --- | --- | --- |
| STEP | one instant per request | 4m32s | `f986a6b0790bb1fc` |
| 1x | background `advance`, polled | 6m07s | `f986a6b0790bb1fc` |
| 10x | background `advance`, polled | 5m39s | `f986a6b0790bb1fc` |
| MAX | 20 instants per request | 54s | `f986a6b0790bb1fc` |

Identical in every section — 46 decisions, 46 orders, 42 fills, 21 ledger
entries, 6 positions, 4 attribution rows — while wall time varies by a factor
of six. Speed alters pacing and nothing else.

The hex is tied to the commit that measured it. Decisions have since started
recording the bar they were taken on, which changes the canonical form and so
changes every digest; what carries forward is the PROPERTY — one digest across
all four modes — not the value.

**1x and 10x take the same wall time, and that is the pacing cap rather than a
broken speed.** The per-instant sleep is capped at two seconds, and on 1h bars
every finite speed exceeds the cap (3600s/1, 3600s/10 and 3600s/100 all do), so
they pace identically. An uncapped 1x here would take 140 hours. The cap is
right; the speed NAMES overstate what they control on long timeframes.

**A paced run cannot be hand-stepped through the API.** The pacing sleep
happens inside the step request and the router gives every handler 30 seconds,
so any useful batch blows the deadline and surfaces as `context deadline
exceeded` from whichever query was in flight — a database error for what is
arithmetic. Paced runs use `advance`, which is the control an operator would
use for a paced replay anyway.

### Preconditions for a comparable run

1. **The same starting database.** Account balance, open positions and the
   ledger all feed the next decision. Snapshot and restore between runs.
2. **A trading authority covering the dataset's dates.** The dev seed grants
   three years; a 90-day window expired before a 2027 dataset even started and
   skipped 610 strategy runs with "Trading authority has expired" — a correct
   refusal for an invisible reason. A preflight check now reports this before a
   run starts.
3. **Autopilot on.** Also reported by preflight.

## Warm-up is observable

`GET /api/v1/replay` reports which phase a run is in and splits its instants by
that phase:

```
state                running
steps                96
phase                evaluation
warmup_instants      59
evaluation_instants  37
allow_warmup_trading false
warmup_start         2027-03-02T04:00:00Z
evaluation_start     2027-03-04T18:00:00Z
```

Without this, a run that produced nothing and a run that never left warm-up
look identical from the outside, and they need opposite responses. The
transitions are also written to the structured log as `WARMUP_STARTED`,
`WARMUP_COMPLETED` and `EVALUATION_STARTED` — in the log rather than in
`strategy_runs`, because a phase change is a fact about the run and writing it
as a strategy run would corrupt the per-bar watermark that decides whether a
bar has been evaluated.

**`warmup_instants` is one fewer than the declared warm-up, by construction.**
The declaration means "evaluation begins once N bars of history are complete",
and the clock sits at each bar's CLOSE, so at instant N-1 there are exactly N
complete bars and evaluation begins there. Instants 0 to N-2 produce no
executable intent. `warmup_instants + evaluation_instants` always equals
`steps`.

This reporting immediately paid for itself: every scenario in the end-to-end
matrix stepped 60 instants, which the warm-up model had silently turned into an
evaluation window of **zero**. Eight of the nine were asserting on nothing.

## A partial fill, and the instrument that makes one representable

A partial fill touches the order state machine, the position, the weighted
average price, the fee accrual and the ledger at once, and it could not be
exercised at all. Every order the seeded R500 account produces is 0.01 lots,
which on XAUUSD.m is simultaneously the minimum quantity AND the quantity step,
so 40% of one order is 0.004 lots — not a representable quantity — and the
venue correctly declined to split it.

`TEST_XAU` is a development-only synthetic instrument with a finer lot step
(4 decimal places, 0.0001 minimum and step) and a 1/100-ounce contract. On it,
0.10 lots splits into 0.04 and 0.06.

**Nothing under test was relaxed to get there.** XAUUSD.m keeps its minimum and
step, the account keeps its size, and the trading authority keeps its 0.10-lot
ceiling — which still applies, because the split happens inside it. The seed
refuses to run outside development, no strategy declares the instrument, and
the authority grants it explicitly rather than by wildcard, so nothing
autonomous can reach it.

Measured: `TEST_XAU buy 0.1000 filled=0.0400 PARTIALLY_FILLED`, with 0.0600
still working.

The order is placed by hand, and that is a real limitation rather than a
detail. It goes through the same `oms.Submit` and the same `booking` package as
an autonomous order — enforced structurally by
`TestEveryOrderPlacementGoesThroughTheSameOMSMethod` — so the fill, position,
ledger and state-machine behaviour is identical. What it does NOT exercise is
the orchestrator's front end.

## What a restart does to each piece of state

**A replay STOPS at a process restart and does not resume itself.** Engaging a
replay puts the whole process on dataset time, and a control plane that came
back up and silently moved its own clock to 2027 because a row said a run was
in progress would be deciding that for the operator at the moment nobody is
watching. The abandoned run is marked `interrupted` and listed at
`GET /api/v1/replay/interrupted` with a verdict on whether resuming it is safe;
continuing it takes a deliberate `resume_interrupted` naming the run, and the
cursor comes from the platform's own durable record rather than from the
caller.

Every piece of state is in exactly one of three categories. The distinction
matters because the failure modes are opposite: RESET state that should have
been persisted loses work, and PERSISTED state that should have been reset
carries a dead run's assumptions into a live one.

| State | At a restart | Where it lives, and why |
| --- | --- | --- |
| Orders, fills, positions, ledger, balance | **PERSISTED** | Database rows written inside the transaction that created them. Asserted unchanged across a real process kill by `TestScenarioS_ARestartStopsTheReplayAndDuplicatesNothing` and, for a half-filled order, by `TestAPartialFillSurvivesARestartWithoutDuplicating` |
| Decision snapshots and audit chain | **PERSISTED** | Append-only tables with mutation-rejecting triggers |
| The replay run record — cursor, dataset hash, seed, code SHA, config hash, window | **PERSISTED** | `replay_runs`. The cursor is what an operator resume reads; a client cannot supply one |
| Per-bar strategy watermark | **PERSISTED** | `strategy_runs`. Deliberately survives, so a restart cannot re-evaluate and re-trade a bar. Verified by counting duplicate (strategy, bar) pairs after the restart |
| Autopilot, kill switches, trading authority, risk limits | **PERSISTED** | Controls must not be released by a crash |
| Reconciliation issues | **PERSISTED** | An unresolved divergence outlives the process that found it |
| Replay market data (`provider = 'replay'`) | **PERSISTED**, then purged at the next `start` | Kept so the interrupted run's evidence survives; removed when a new run begins, because a dataset starting earlier makes the stored quote look like the future |
| Rate-limit buckets | **PERSISTED** | Redis, a separate process. An API restart does not refill a caller's budget |
| Sessions | **PERSISTED** in the database, but the restart invalidates the process's view — a client must sign in again | This is why the restart tests re-authenticate |
| The replay clock and whether it is engaged | **RESET** | The process comes back on real time. `GET /api/v1/replay` reports `engaged: false`, asserted after the kill |
| The replay window (the data floor and warm-up boundary) | **RESET** | An `atomic.Pointer` in the engine, nil until a run is started or resumed. A stale floor would silently hide history from a non-replay process |
| The engine's dataset and in-memory cursor | **RESET** | Rebuilt from the registry and the durable cursor on resume |
| Regime hysteresis trackers | **RESET** | `oms.regimeTrackers`, in process. A tracker carries "three confirmations to leave RISK_OFF"; inheriting one from a dead run would apply a previous market's confirmations to a new one |
| Correlation matrix | **RESET** | `oms.correlationMatrix`, in process. Also cleared at every replay `start` for the same reason |
| Mock venue jitter sequence | **RESET**, then re-seeded | In process, and re-seeded at every replay `start`. Seeding only at construction made a second run in one process continue the sequence, so two runs from a byte-identical snapshot produced identical decisions and different fill prices |
| Research-client circuit breaker | **RESET** | In process, threshold 5, cooldown 30s. A restart gives the research service a fresh chance, which is the behaviour wanted |
| Portfolio valuation — equity, margin, exposure, unrealised P&L | **RECONSTRUCTED** | Computed from persisted positions and current quotes on every read. Nothing is stored that could disagree with its inputs |
| The market regime | **RECONSTRUCTED** | Recomputed from bars at the next evaluation, after a fresh warm-up. The *classification* returns; the hysteresis state behind it does not |
| The correlation matrix's values | **RECONSTRUCTED** | Recomputed by the scheduler's correlation phase, which in a replay runs every step |
| Whether an interrupted run may be resumed | **RECONSTRUCTED** | Derived by comparing the stored dataset hash against the registry at the moment the question is asked, never stored. A fixture edited since the run would otherwise still read as resumable |

### Crash timing

Killing the process faster does not reach the two moments that matter. They are
reached by making the venue behave exactly as it would at that instant — which
is what the deterministic fault modes are for — and then killing the process
for real. Both are covered:

- **The outcome was unknown.** `timeout` leaves the order FAILED with nothing
  written at the venue, and the platform unable to know that. Across a restart
  the order must gain no fill and must not become FILLED: resolving an
  uncertainty without evidence invents a position either way.
- **The venue executed and the answer was lost.** `lost_response` records the
  order at the venue and drops the reply, so the execution exists and the
  platform has no record of it. Across a restart, exactly two outcomes are
  defensible — reconciliation imports it exactly once, or it stays an open
  issue for an operator. Importing it twice, or closing the issue without
  re-reading the evidence, is not. **Measured:** the venue held 13 executions
  against 12 local fills; after a real process kill exactly one was imported,
  none twice, and the balance reconciled against the ledger-derived total.

Plus a half-filled order carried across a restart, on the synthetic instrument
that makes a partial fill representable at all.

**What is still not covered:** killing the process at a chosen instruction
boundary *inside* the OMS transaction. That needs a fault that blocks at a
named point, which does not exist. The gap it leaves is narrow — the order
write and the venue call are not in one transaction, and the two cases above
bracket the window between them — but it is a gap.

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

- **Seeded history reaching a replay's classification: FIXED and PROVED.** The
  floor is demonstrated by planting 400 bars of the opposite market shape
  immediately before it, under a provider the replay cannot purge: the
  `range-bound` dataset still classified RANGING 77/77 with 400 trending bars
  planted, and `trend-clean` still classified TRENDING 77/77 with 400 ranging
  bars planted. See `tests/replay/isolation_test.go`.
- **A replay run cannot tell "input gathering failed" from "the account was
  flat".** `starting_positions` is `NOT NULL DEFAULT 0` and the input gatherer
  is deliberately non-fatal, so both record 0. The honest fix is a separate
  flag saying whether the inputs were gathered at all.

- **Nothing autonomous trades under the default consensus policy.** This is
  the consequence of wiring `orchestrator.Decide` in, and it is a finding, not
  a defect. `DefaultConsensusPolicy` discards an opinion below 0.55 confidence
  and one whose strategy declares itself invalid in the prevailing regime.
  **Measured on a freshly seeded stack, every fixture:** the only actionable
  opinions the five PAPER strategies produce are `macd_momentum` buys at
  0.51-0.54, `donchian_breakout` buys at 0.31-0.44 — all below the floor — and
  `rsi_mean_reversion` sells at 0.66-0.73, which clear the floor and are
  discarded because the strategy declares RANGING/LOW_VOLATILITY and the regime
  is TRENDING. `ma_trend_crossover` and `bollinger_zscore_reversion` produced no
  actionable signal at all. On `trend-clean`, which placed 33 orders before the
  wiring, the result is 77 no-trade verdicts and **0 orders**.

  The thresholds are NOT tuned to fix this. Their own comment says they are "a
  starting posture for a platform that has never traded unattended, chosen so
  that the common outcome is NO TRADE", and that loosening them "should be made
  against paper-forward evidence, not to make a demo trade". Changing them to
  make a fixture trade is exactly that. The open question is whether the
  strategies' confidence is calibrated to a scale the policy was written
  against, and it is a research question rather than a wiring one.

  Consequence for the scenarios: D and I now SKIP rather than assert, because
  each measures how an order was refused and no order is placed. Their skip
  messages say so and print the verdicts.
- **Replay scenarios J and Q do not exist.** The letters in this repository are
  A B C D E G H I K L M N T. A, D, G, I, M and N have been run against the
  wired consensus; J and Q were asked for and there is nothing to run.
- **A decision is recorded, but a no-trade verdict is not attributable to one
  strategy.** The `consensus` column carries every contribution, so the
  individual opinions are recoverable; `strategy_id` on the snapshot is null,
  because the verdict belongs to the set. Any query that groups decisions by
  strategy therefore sees order-bearing decisions only.
- **The model is still not attributed.** A decision now records the market
  regime, the regime policy version and the evidence behind it, along with the
  replay run; the model that informed it is still not recorded, so P&L cannot
  be grouped by model.
- **No paper-forward versus backtest comparison.** Both exist; nothing compares
  them.
