# Final engineering report

Every claim below is either something that was executed and observed, or is
labelled as not verified. Nothing is asserted from reading the code alone.

Commits: the reconciliation milestone is `e0a7f3b`, `bf8e151`, `16f02ab`. The
intelligence milestone is `0a6d8a3`, `4bc1aaa`, `ee32862`, `339b74e`,
`31f6159`. The market-replay milestone is `0cb491b`, `bccd954` and the commit
that carries this revision of the report. Appendix A is the summary; this body
is the evidence behind it.

**Section 0 is the requirement matrix: the fastest answer to "what is
actually known about this platform right now."** It is maintained as a standing
inventory rather than per milestone, so it is the section to read first and the
section to update when an answer changes. The body below is the evidence
behind it and is organised by milestone.

The current revision audited the market-data layer and gave its acquisition
path its first tests. Those tests found a defect no amount of reading had:
a forming bar and a finished bar collide on the same primary key, and the
unconditional upsert let the incomplete one win. Section 30m records it and
five others, including a leaked API key.

The revision before this one covered the market-replay milestone. Its
predecessor's largest recorded gap was that the replay provider drove the
decision layer but not the application; that gap is closed, and section 19b
states exactly what a replay now drives and what it assumes.

The headline result is that one deterministic dataset now runs the real
pipeline end to end -- ingestion, bars, strategies, orchestration, risk,
Autopilot, OMS, mock venue, fills, ledger, audit -- and two runs from a
byte-identical database produce byte-identical financial output. Reaching that
took five defects, four of them in code the previous milestones had passed.

---

## 0. Requirement matrix -- current state

This section is a standing inventory of what the platform is required to do and
what is actually known about each requirement. It is written from the
repository rather than from an earlier report, and it is the first thing to
update when a milestone changes an answer.

The classifications mean exactly this:

| Class | Meaning |
|---|---|
| **VERIFIED** | Something was executed and observed: a test that ran, output that was read, a probe that was made. |
| **PARTIAL** | The requirement holds over part of its range, or holds against a stand-in rather than the real thing. The limit is stated in the row. |
| **UNVERIFIED** | The code is there and is believed correct, but nothing has been run that would show it. This is not a claim of correctness. |
| **BROKEN** | Executed, and the result was wrong or absent. |
| **MISSING** | Not built. |

A row citing a test names a test that exists in this tree. A row citing a
result names a run recorded in §0.12.

### 0.1 Market data

| # | Requirement | Class | Evidence |
|---|---|---|---|
| MD-1 | One provider seam, several implementations behind it | VERIFIED | `internal/marketdata/provider.go`, `replay.go`, `twelvedata.go`, registered in `internal/app/app.go`. 127 tests in the package, all run. |
| MD-2 | Bars keep advancing; live quotes fold into 15m, 1h and 4h | VERIFIED | `internal/marketdata/aggregate.go`, with `Aggregator.Seed` wired in `internal/app/app.go`. Observed resuming 18 forming bars across a restart. |
| MD-3 | Quality gating: stale or absent data refuses rather than guesses | VERIFIED | `/health/ready` and `/market/health`; smoke asserts the refusal on a stale feed. Observed again this session -- the venue's maintenance break produces `no_data`, not a fabricated quote. |
| MD-4 | Historical acquisition: backfill, sync and repair through one path | PARTIAL | `internal/marketdata/sync.go`. `internal/marketdata/acquisition_integration_test.go` (16 tests) drives the real `Syncer` against a fake server implementing Twelve Data's **documented** contract. Never run against the provider itself -- see MD-9. |
| MD-5 | A large range is chunked and fully covered | VERIFIED | `TestALargeRangeIsChunkedAndFullyCovered` -- 9 000 bars over three requests, with no hole at the seams. |
| MD-6 | Re-acquisition is idempotent | VERIFIED | `TestReacquiringTheSameRangeChangesNothing`; `market_bars` is keyed on `(instrument_id, timeframe, open_time)` with `ON CONFLICT DO UPDATE` in `internal/store/market.go`. |
| MD-7 | A forming bar never overwrites a finished one | VERIFIED | The `WHERE NOT (market_bars.complete AND NOT EXCLUDED.complete)` guard in `internal/store/market.go`; `TestAFormingBarNeverOverwritesAFinishedOne`, `TestAProviderMayStillCorrectItsOwnFinishedBar`. **This was BROKEN at the start of this session**; it is the defect the new integration suite found. |
| MD-8 | Repair never fabricates a bar the provider does not have | VERIFIED | `TestRepairNeverFabricatesABarTheProviderDoesNotHave`. A closed-market range is left alone, which is what made the first version of that fixture wrong rather than the platform. |
| MD-9 | The Twelve Data provider works against Twelve Data | UNVERIFIED | `internal/marketdata/twelvedata.go`. No API key is configured on this machine and the provider has never been contacted. What is verified: the request shape against the published documentation, and the response handling against a fake that follows it. The Individual plan's licence is personal, internal and non-commercial -- recorded here because it constrains what any acquired data may later be used for. |
| MD-10 | Provenance: what was asked for, and what came back | VERIFIED | `market_data_segments`; `TestProvenanceRecordsWhatWasAskedAndWhatCameBack`. A short answer is recorded PARTIAL with a warning rather than silently accepted. |
| MD-11 | The API key never reaches a response, a log or the database | VERIFIED | `scrub` and `transportCause` in `internal/marketdata/twelvedata.go`; `TestATransportFailureDoesNotCarryTheKey`, `TestTheProviderHealthNeverCarriesTheKey`, `TestAFailedAcquisitionNeverPersistsTheKey`. This was a real leak, found by probing rather than by reading. |
| MD-12 | The provider base URL is fixed configuration, never caller-steered | VERIFIED | `NewTwelveDataProvider` requires https and takes the base URL from configuration only; no handler passes one through. |
| MD-13 | Legitimate historical XAUUSD 1H data present in the system | MISSING | Nothing has been acquired. The apparatus to acquire it is MD-4; the reason it has not run is MD-9. |

### 0.2 Research

| # | Requirement | Class | Evidence |
|---|---|---|---|
| RS-1 | The research plane cannot reach a broker | VERIFIED | `TestTheQuantBridgeCannotReachExecution` in `internal/arch/arch_test.go`; no broker client exists under `services/quant`. |
| RS-2 | No look-ahead in any indicator or backtest | VERIFIED | `services/quant/tests/test_research_leakage.py` (15 tests); `marketdata.ReplayProvider` never returns a bar past the cursor. |
| RS-3 | Backtests report honestly -- costs, slippage, out-of-sample | VERIFIED | `services/quant/tests/test_backtest.py` (25 tests). |
| RS-4 | A source type is never assumed across the process boundary | VERIFIED | `internal/marketdata/snapshot.go` writes the CSV under a temporary name and renames it only once `.manifest.json` is down; `services/quant/vantage_quant/research/historical.py` refuses an export-shaped file with no manifest, and refuses an unrecognised value. `test_research_historical.py` (72 tests). |
| RS-5 | The evidence taxonomy distinguishes "not enough data" from "the score is the problem" | VERIFIED | `research/verdict.py`, `research/requirements.py`; `test_research_expansion.py` (40 tests), `test_score_inventory.py` (9). |
| RS-6 | Regime classification is consistent across its own bands | VERIFIED | `services/quant/vantage_quant/scanner.py`; `test_regime.py` (7 tests). Corrected this milestone: a market going nowhere is a range at any ADX. |
| RS-7 | Research conclusions drawn from real market data | MISSING | Every dataset in the tree is `SYNTHETIC_CONTROLLED`. This is why MD-13 is the next thing that matters. |

### 0.3 Replay

| # | Requirement | Class | Evidence |
|---|---|---|---|
| RP-1 | Replay drives the real pipeline, not a copy of it | VERIFIED | `Scheduler.ReplayStep` calls the same job functions the interval loops call; `TestTheReplayEngineHoldsNoBrokerAdapter`. |
| RP-2 | The dataset registry is an allowlist, never a path | VERIFIED | `internal/replay/fixtures.go`, `dataset.go`; 75 tests in `internal/replay`. |
| RP-3 | Two runs from a byte-identical database produce identical financial output | VERIFIED | `tests/replay` (22 tests, stack-gated). Established in the replay and determinism milestones (§30c, §30g) and not re-executed for this revision; the finding that a stale control plane fakes non-determinism is recorded there. |
| RP-4 | A simulated clock never moves a security lifetime | VERIFIED | `TestAuthenticationUsesRealTimeNotTheTradingClock`. |
| RP-5 | A replay reports its preconditions rather than stepping through silently | VERIFIED | The engine's preflight. §30k records the run that stepped a whole dataset producing nothing because the research service was down, and what now prevents it. |

### 0.4 Strategies

| # | Requirement | Class | Evidence |
|---|---|---|---|
| ST-1 | Strategies are registered, versioned and promoted one lifecycle stage at a time | VERIFIED | `services/quant/vantage_quant/strategies.py`; `test_strategies.py` (21 tests). A seeded database carries 12 registered, 5 at PAPER. |
| ST-2 | Consensus is a pure, versioned policy: vetoes before votes, never a majority | VERIFIED | `internal/orchestrator/consensus.go`, with `aggregate.go` as its only production caller, pinned by `TestTheAutonomousLoopRoutesOnlyAnAggregatedVerdict`. 76 tests in the package. |
| ST-3 | Ten deterministic market scenarios exercise the decision layer | VERIFIED | `internal/orchestrator/scenario_test.go`. |
| ST-4 | The strategies produce an actionable signal when the market suits them | VERIFIED | A `trend-clean` replay through the real pipeline now produces **46 actionable signals** -- 26 buy (mean 0.453) and 20 sell (mean 0.700) -- beside 73 genuine abstentions. The previous revision recorded this row as BROKEN on the strength of 130 signals all at confidence 0.000; **266 of those were strategies that had been handed fewer bars than they require and could not form an opinion at all**, recorded as opinions. Section 30o. |
| ST-5 | An actionable signal that survives the policy becomes an order | **BROKEN — on the committed fixtures** | The same replay produced 39 decisions, every regime correctly TRENDING, and **no order**. On that data the only strategy clearing the 0.55 floor (`rsi_mean_reversion`, 0.700) is valid only in RANGING so the regime gate correctly discards it, and the trend-valid ones peak at 0.538 and 0.441. **That is a property of the fixtures, not of the platform:** a constructed market that classifies TRENDING and breaks its channel by 1.0x ATR gives `donchian_breakout` 0.750 — clear of the floor, clear of the 0.60 net requirement, and valid in that regime, with no threshold changed (`test_confidence_reachability.py`). What is missing is a fixture containing a decisive break. Calibrating the scale against outcomes remains research; moving a threshold would still answer it dishonestly. |

### 0.5 Risk

| # | Requirement | Class | Evidence |
|---|---|---|---|
| RK-1 | The risk engine is a pure function with no database reach | VERIFIED | `TestRiskEngineDoesNotReachTheDatabase`; `internal/risk/engine.go`. 54 tests in the package. |
| RK-2 | A check that limits exposure or loss never refuses a reducing order | VERIFIED | `internal/risk/reducing_matrix_test.go` covers both halves of the rule -- opposite side *and* quantity no greater than the open position -- and the side-flip case. |
| RK-3 | Risk may only reduce a requested size | VERIFIED | `internal/risk/sizing.go`. No path increases a request. |
| RK-4 | Trading authority is a technical control with a date range and a scope | VERIFIED | `internal/risk/authority_boundary_test.go`. Observed: a 2027 replay against a 90-day authority skipped 610 runs with "Trading authority has expired". |
| RK-5 | Fail closed -- a missing rate or a stale quote refuses | VERIFIED | Asserted by smoke; `internal/fx` returns a refusal, never a guess. |

### 0.6 OMS

| # | Requirement | Class | Evidence |
|---|---|---|---|
| OM-1 | Only the OMS holds a broker adapter | VERIFIED | `TestOnlyTheOMSCanPlaceABrokerOrder`, `TestBrokerAdapterIsHeldByAnAllowlistOfPackagesOnly`. |
| OM-2 | Every order placement goes through one method | VERIFIED | `TestEveryOrderPlacementGoesThroughTheSameOMSMethod`. |
| OM-3 | Only `internal/booking` appends a fill, and it holds no adapter | VERIFIED | `TestOnlyBookingAppendsFills`, `TestBookingHoldsNoBrokerAdapter`; 12 tests in `internal/booking`. |
| OM-4 | Idempotent under concurrency -- a duplicate request cannot double-fill | VERIFIED | `tests/race`, **15 pass 0 fail against the running stack for this revision**; `go test -race ./internal/...` clean. |
| OM-5 | The order state machine refuses illegal transitions | VERIFIED | `internal/domain` (145 tests). |
| OM-6 | Autopilot halts only the autonomous pipeline; the kill switch halts everything | VERIFIED | Both are enforced inside the order transaction. Smoke asserts that manual trading, cancel and flatten survive Autopilot OFF. |

### 0.7 Reconciliation

| # | Requirement | Class | Evidence |
|---|---|---|---|
| RC-1 | Snapshot both sides, classify with a pure function | VERIFIED | `internal/reconcile`, `domain.PolicyFor`; 34 tests. |
| RC-2 | Only provable divergence is repaired; the ambiguous halts for an operator | VERIFIED -- **and observed live** | Two orphan venue executions injected by the smoke suite were classified `EXTRA_BROKER_FILL`, severity `critical`, `OPERATOR_ACTION_REQUIRED`, and held the account in `TRADING_HALTED` for two hours until the database was reset. Automatically-safe classes beside them were repaired. That is the rule working, read out of the live database rather than out of a test. |
| RC-3 | "Not re-detected" means "fixed", never "no longer examined" | VERIFIED | `closeVanishedIssues` together with the frozen execution cursor in `reconcile.runLocked`; unit tests in `internal/reconcile`. |
| RC-4 | Reconciliation endpoints are reachable by the role permitted to use them | VERIFIED | `TestEveryReconciliationHandlerUsesTheOperationsScope`; `apps/web/tests/e2e/reconciliation.spec.ts` (21 tests). |
| RC-5 | Position quantities are never written to match the venue | VERIFIED | No such path exists. `reconcile.Resolve` exposes named actions only, and there is no generic "set order status" endpoint. |

### 0.8 Paper trading

| # | Requirement | Class | Evidence |
|---|---|---|---|
| PT-1 | Live and demo execution are impossible in this build | VERIFIED | `config.BuildAllowsLiveExecution = false` at `internal/config/config.go:33`, the validation in `config.Load`, the absence of a live adapter, and database CHECK constraints; `TestPaperOnlyGuaranteeIsStillCompiledIn`. `/health/ready` reports `"execution_mode":"paper"`. |
| PT-2 | Money never becomes a float | VERIFIED | `TestNoFloatingPointColumnsInTheSchema`, `TestMoneyHasNoFloatConstructor`; `apps/web/lib/format.ts` formats decimal strings without constructing one. |
| PT-3 | The audit chain is tamper-evident and its hashes match | VERIFIED | `internal/domain`; microsecond truncation, and `json` rather than `jsonb` so the stored bytes are the hashed bytes. |
| PT-4 | An order placed at the terminal reaches the venue, fills, books and settles | VERIFIED | Smoke, end to end against a running stack. |
| PT-5 | The mock venue misbehaves on purpose, so recovery is exercised against a venue that actually does | VERIFIED | `internal/broker/mock` fault modes (10 tests). The reconciliation evidence in RC-2 came from them. |

### 0.9 UI

| # | Requirement | Class | Evidence |
|---|---|---|---|
| UI-1 | The terminal holds no business rules; every refusal comes from the API with a machine-readable code | VERIFIED | `apps/web`; `trading.spec.ts` asserts the codes rather than the prose. |
| UI-2 | Charts render from Vantage's own bars, not a third party's feed | VERIFIED | `apps/web/components/PriceChart.tsx` uses `lightweight-charts` as a renderer only; the bars come from `/market/bars`. `chart.spec.ts` (6 tests). |
| UI-3 | Authentication, MFA and session handling in the browser | VERIFIED | `auth.spec.ts` (9 tests). The whole Playwright suite -- 57 tests -- passed against the terminal for this revision. |
| UI-4 | Reconciliation review and repair from the terminal | VERIFIED | `reconciliation.spec.ts` (21 tests). |
| UI-5 | No `Date.now()` during render | VERIFIED | The `useNow` hook in `lib/store.tsx`. |
| UI-6 | Accessibility | MISSING | There is no automated accessibility suite: no axe run, no keyboard-navigation test. Nothing in this report should be read as a claim about it. |

### 0.10 Security

| # | Requirement | Class | Evidence |
|---|---|---|---|
| SC-1 | No cryptographic primitive is implemented here | VERIFIED | `internal/crypto` composes the standard library; 11 tests. |
| SC-2 | No secrets in the repository, a log, a response, a decision snapshot or audit metadata | VERIFIED | gitleaks: 0 findings. MD-11 closed the one real leak this session. **The browser bundle is verified against the COMPILED artifact, not the source**: grepping `.next/static` and the standalone copy for env-var-shaped strings returns exactly two -- `NEXT_PUBLIC_VANTAGE_API_BASE_URL`, and `TWELVE_DATA_CONFIGURATION_REQUIRED`, which is the operator-facing notice rendered *because* no key is configured and is the inverse of a leak. A second pass for inlined credential VALUES rather than names -- the dev role passwords, the quant service token, the provider host, the literal `apikey` -- returns zero files. Source-grepping cannot show this; a key could be inlined as a bare value with no name beside it. |
| SC-3 | Authorisation is enforced server-side, per role | VERIFIED | `internal/httpapi` (29 tests), `internal/auth` (11); `auth.spec.ts`. |
| SC-4 | Authentication lifetimes read real time, never the trading clock | VERIFIED | `TestAuthenticationUsesRealTimeNotTheTradingClock`. |
| SC-5 | CSRF, CORS and security headers | VERIFIED | `internal/httpapi`. CORS allows exactly one origin. |
| SC-6 | Dependency and container scanning | VERIFIED | govulncheck 0 reachable, trivy 0, npm audit 0, semgrep 0 across 247 files. |
| SC-7 | SSRF: no outbound URL a caller could steer | VERIFIED | MD-12. |
| SC-8 | CI enforces all of the above on every push | VERIFIED | **GitHub Actions has now executed.** The repository was published on 2026-09-30 and both workflows are green: CI run `36718123184` (5 jobs -- Go, Python, web, migrations, images) and Security run `36718123066` (11 jobs -- govulncheck, npm audit, OSV-Scanner, Gitleaks, Semgrep, pip-audit, Trivy filesystem and three Trivy images, repository policy). Reaching green took two rounds of fixes for six defects that `actionlint` cannot see -- section 30q. |

### 0.11 Where three of these classifications came from

Three rows changed during this session, and how they changed is the point.

- **MD-7 was BROKEN.** Nothing in the tree tested it. The new integration
  suite acquired 9 000 bars and found 8 999 stored; the missing one was the
  hour the process happened to be in, because the live aggregator's forming
  bar and the syncer's finished bar collide on the same primary key and the
  unconditional upsert let the incomplete one win.
- **MD-11 was BROKEN.** The test that existed drove a 401, whose error is a
  static string with no URL in it, and passed. The leak was on the transport
  path.
- **MD-4 had no coverage at all.** `Backfill`, `Sync`, `Repair` and `Snapshot`
  had zero references in any test file before this session; every `Syncer`
  test exercised a pure helper.

Each was on a path whose test existed and exercised a different branch. That
is the general lesson from this audit, and it is why the UNVERIFIED rows above
say only what they say.

### 0.12 What was executed for this revision

Every command below was run on this machine on 2026-09-30 and its output read.

| Command | Result |
|---|---|
| `gofmt -l .` (services/control-api) | clean |
| `go vet ./...` | clean |
| `go test -count=1 -v ./...` | **874 pass, 0 fail, 54 skip**, exit 0 (the stack-gated suites skip) |
| `CGO_ENABLED=1 go test -race ./internal/...` | clean |
| `go test ./internal/marketdata/` with `VANTAGE_MARKETDATA_E2E=1` against the dev Postgres | **16 acquisition tests pass**, alongside the rest of the package |
| `python -m ruff check .` (services/quant) | clean |
| `python -m mypy vantage_quant` | clean, 26 source files |
| `python -m pytest` | **348 pass, 0 fail** |
| `npm run typecheck`, `npm run lint` (apps/web) | clean |
| `npm audit` | 0 vulnerabilities |
| `npx playwright test` against the terminal on :3005 | **57 pass, 0 fail, 0 skip** on Chromium, on a freshly reseeded database |
| `go test ./tests/race/` with `VANTAGE_RACE_E2E=1` | **15 pass, 0 fail, 0 skip** against the running stack, on its own fresh seed |
| `python tests/smoke/smoke.py` | **66 pass, 0 fail** |
| `python tests/smoke/smoke_research.py` | **35 pass, 0 fail** |
| `gitleaks`, `semgrep`, `trivy`, `govulncheck`, `actionlint` | 0 findings each; semgrep across 247 files |
| `./scripts/dev-up.ps1 -Reset -Seed`, four times | 20 migrations applied each time; 11 790 bars, 12 strategies, 5 at PAPER. One seed per stack-dependent suite, because the daily-loss budget and the exposure ceilings are finite fixture resources |
| `GET /health/ready` | `ready`, `paper`, quant `ok`, reconciliation `HEALTHY` |
| A full `trend-clean` replay through the real pipeline, both planes rebuilt | 140/140 rows, 81 evaluation instants, 0 errors. `strategy_runs`: 551 skipped (266 of them insufficient history, naming both numbers), 73 no_signal, **46 succeeded**. 39 decisions, all TRENDING, no order |
| `git push` to a public GitHub repository, then three CI rounds | First run: **both workflows red**, six defects `actionlint` cannot see. Third run: **both green** -- CI 5/5 jobs, Security 11/11. Section 30q |

Not executed, and therefore not claimed:

- Any request to Twelve Data. No key is configured on this machine (MD-9).
- GitHub Actions. The repository has no remote (SC-8).
- Any accessibility tooling (UI-6).
- `go test ./tests/replay/` as a single invocation. That is deliberate rather
  than an omission: several of those suites replay the same dataset into the
  same account, so one `go test` measures an exhausted fixture and reports
  agreement it has not earned. RP-3 is therefore carried from the milestone
  that ran each suite on its own seed, and is labelled as carried.
- `npm run build` was not re-run after the Playwright change, because a build
  replaces `.next` underneath the server that was serving the suite. The change
  is in test code only; `tsc --noEmit` and `eslint` were run and are clean.

---

## 1. Paper-only enforcement

Four independent mechanisms. Any one alone would prevent live execution.

`config.BuildAllowsLiveExecution` is a compile-time `false`; `config.Load`
refuses any execution mode but `paper` and any broker but `mock`; no live
adapter exists to register; and `accounts_paper_only_ck`,
`broker_connections_mock_only_ck`, order mode checks and
`strategy_versions_paper_ceiling_ck` reject non-paper rows in SQL.

**Verified:** `TestBuildRefusesLiveExecution` passes for `live`, `LIVE`,
`demo`, `real` and `production`; `TestBuildRefusesNonMockBrokers` passes for
`exness`, `mt5`, `mock,exness`, `MT5`. The built container prints
"This build executes simulated PAPER orders against a mock venue" on `--help`,
and `/version` returns `live_trading_available: false`.

**No real broker credential was requested or stored, and no external venue was
contacted at any point.**

## 2. One path to a broker

`internal/oms` is the only package holding a `broker.Adapter`. Nineteen ordered
gates, no second entry point.

**Verified:** the smoke suite drives the real pipeline over HTTP and observes
refusals with structured codes at the authority, kill-switch, risk, stop-loss,
notional and quantity gates.

## 3. Research cannot execute

The Python service has no broker client, no credential pointing back at the
control plane, and a database role granted `SELECT` on market and research
tables only -- nothing on `users`, `sessions`, `accounts`, `orders`, `fills`,
`transactions`, `trading_authorities`, `kill_switches` or
`broker_connections`.

**Verified:** the smoke suite attempts writes as the research role and confirms
Postgres refuses them.

## 4. Idempotency under concurrency

The key is claimed inside the same transaction as the order write, with
`ON CONFLICT (account_id, idempotency_key) DO NOTHING`, so a concurrent
duplicate loses the claim rather than being deduplicated afterwards. The same
key with a different payload is refused as
`idempotency_key_payload_mismatch`.

**Verified:** eight concurrent identical submissions produce exactly one order;
a replayed key returns the original outcome.

## 5. Risk engine

A pure function: 26 checks, all evaluated, all failures reported. Risk may only
reduce a requested size.

**Verified:** 22 unit tests written this session, including that concentration
does not block the first trade, that all failures are reported rather than the
first, that the approved quantity never exceeds the request, that loss ceilings
refuse rather than scale down, that a degraded feed blocks automation but not a
human, that the event blackout binds automation only, and that the same input
twice yields an identical verdict.

## 6. Money

`money.Amount` is a decimal plus a mandatory currency; the zero value has an
*empty* currency so an uninitialised field cannot join a calculation as
"0 ZAR". Cross-currency arithmetic returns `ErrCurrencyMismatch`.

**Verified:** the `money` package's tests pass. Conversion refuses rather than
guessing -- a risk evaluation with no available FX rate does not approve.

## 7. Order state machine

Eleven states with a closed transition table. `FAILED` means "the venue-side
outcome is unknown", is non-terminal, and is resolved by reconciliation, never
by an automatic retry. `CANCEL_PENDING → FILLED` is legal because a cancel
races the venue.

**Verified:** the `domain` package's tests pass, including the legal-transition
table.

## 8. Audit chain

Append-only, hash-chained, verifiable on demand; reports the first broken link
rather than a boolean. Microsecond timestamp truncation and a `json` (not
`jsonb`) metadata column are what make it re-verify.

**Verified:** the chain verified during the smoke run at sequence head 93 with
93 events checked.

## 9. Database as the last line

57 tables. Append-only enforcement is doubled: triggers plus revoked
UPDATE/DELETE grants on eight tables. The application role cannot execute DDL.

**Verified:** the smoke suite attempts twelve hostile writes and confirms
Postgres refuses each one.

## 10. Kill switch is not liquidation

A kill switch halts new orders in scope and closes nothing. Flatten is a
separate, explicitly confirmed action.

**Verified:** the smoke suite activates a switch, confirms new orders are
refused, confirms **the open position was not liquidated**, and releases it.

## 11. Authorisation

Three roles. Administration and trading are disjoint: an admin cannot place an
order.

**Verified:** a viewer cannot place an order, cannot read another user's
account, and cannot read another user's ledger; an admin cannot place orders.

## 12. Authentication

Argon2id (64 MiB, 3 passes, 4 lanes), TOTP with encrypted secrets bound to the
user id via AEAD associated data, ten recovery codes stored as HMAC hashes and
consumed atomically, session tokens stored only as SHA-256, five-failure
lockout with exponential backoff to 15 minutes.

**Verified:** the `auth` and `crypto` package tests pass. A sign-in MFA
challenge stage was added this session so enabling MFA cannot lock the operator
out -- the prior build showed a message and offered no way to complete the
challenge.

## 13. CSRF, CORS and headers

Double-submit CSRF compared in constant time; exactly one allowed origin with
no reflection; a JSON-only CSP of `default-src 'none'`; HSTS in production only.

**Verified:** the login flow sets `vantage_csrf`, and the API returned
`Access-Control-Allow-Origin` for the configured origin and nothing for
another.

## 14. Backtest honesty

Entries fill on the bar after the signal, crossing the spread with slippage;
on a bar covering both stop and target the **stop** is assumed; costs always
applied unless explicitly frictionless, and that flag is stored; unaffordable
signals are counted, not dropped.

**Verified:** the Python suite covers the fill-timing and same-bar rules.
Win rate is reported but never as the headline.

## 15. No look-ahead

Truncate the series at each index, recompute, assert the historical value is
unchanged. Applied to 13 indicators and all 14 ML features. Donchian channels
exclude the current bar; only completed bars are served.

**Verified:** part of the 196 passing Python tests.

## 16. ML methodology

Chronological 60/20/20 splits with an embargo of twice the label horizon;
unlabelled tail rows dropped; every score reported against the majority-class
baseline; Brier score reported because calibration matters more than accuracy
for a filter; inference failure produces NO TRADE.

**Verified:** on a random walk, no model beats the baseline -- the correct
result and a check that the harness is not leaking. Model versions record
algorithm, seed, dataset hash, code SHA and dependency versions.

## 17. Provider abstractions

Five seams, each with a mock: `marketdata.Provider`, `broker.Adapter`,
`econdata.CalendarProvider`, `econdata.NewsProvider`, `fx.RateSource`.

The two calendar/news interfaces were **built this session** -- previously the
data was read straight from seeded tables while the documentation claimed an
interface existed. The seed now uses the same ingestion path rather than
holding a second copy of the fixtures.

**Verified:** 9 unit tests covering refresh idempotence, window filtering, and
that no fixture can claim a release happened in the future. The Connections
page reports the provider names the implementations give for themselves.

## 18. Market data quality

Five health states with a documented policy; `degraded` blocks automation but
not a human; `stale` and `invalid` block both.

**Verified:** the risk engine's tests cover the degraded/stale boundary. A
transient smoke failure this session was traced to an order placed in the
two-second window after a reseed, before the ingestor's first tick -- the
platform was correct to refuse, and the **test** was fixed to wait for a
tradable feed and to assert on it.

## 19. Reconciliation, recovery and operator control

Detection was already here. What this section now covers is **repair**, which
is the part that decides whether the platform can be left running.

The governing rule: only divergence that is *provable* is repaired without a
human. Four of thirteen issue types qualify, and each qualifies because the
evidence identifies exactly one local order -- not because the answer is
likely. Everything else halts the narrowest scope that contains it and waits.

- **A pure classifier.** `reconcile.Classify(local, broker, tolerance)` is a
  function of two snapshots with no store, adapter or clock. That is why the
  taxonomy has 34 unit tests: every branch is reachable by constructing two
  structs.
- **Snapshot-first.** Both sides are captured before any repair, so a run
  reasons about one consistent picture and re-runs against the same evidence
  identically.
- **One accounting path.** `internal/booking` is the only code that appends a
  fill, moves a position or books fill P&L, and reconciliation uses it. A
  recovered fill therefore passes the same nine validations, the same overfill
  check and the same `(broker_name, broker_fill_id)` unique index as a live
  one. Two architecture tests fail the build if a second writer appears or if
  `booking` acquires a broker adapter.
- **A separate repair state machine.** `ACCEPTED -> FILLED` is legal as a
  repair and remains illegal in ordinary execution, where it would be an OMS
  bug. Neither table permits leaving a terminal state. Every repair is stamped
  `is_repair` and names the issue that justified it.
- **Uncertainty stays uncertain.** An order whose outcome is unknown is
  `FAILED` *and* `reconciliation_required`, so it is queryable, visible in the
  operations view, counted in readiness and picked up by the next run.
- **Seven named operator actions, ADMIN only**, each requiring an open issue, a
  permitted action and a reason of at least ten characters. There is no
  endpoint that sets an order's status, and `SET_ORDER_STATUS` is asserted
  invalid by a test. `IMPORT_BROKER_FILL` accepts no quantity or price: it
  reconstructs the execution from the evidence stored at detection time.
- **Durable concurrency.** One run per account via a PostgreSQL session
  advisory lock; a second caller is told it declined. The OMS re-reads the halt
  state *inside* the order transaction after taking the same account row lock a
  repair takes, so the ordering is Postgres's decision rather than a function
  of timing.

**Verified**, against a running stack and a real venue simulator:

- The acceptance test passes: an order is submitted, the venue accepts and
  fills it, the local transaction fails before persistence, the process
  restarts, start-up reconciliation discovers the missing fill, imports it
  **exactly once**, and the order, position and ledger converge with the
  trading state returning to HEALTHY.
- Five consecutive runs against the same divergence produce one repair and
  four no-ops, with identical fill count, transaction count and balance
  compared as exact numerics.
- Eight concurrent runs produce one execution and seven 409s, with no 500 and
  no double repair.
- An ambiguous venue execution -- one attributable to zero or several local
  orders -- stays OPERATOR_ACTION_REQUIRED and is never booked. This is the
  separate acceptance criterion and it holds.
- Reconciliation racing new order submission settles without an inconsistent
  position.
- After every scenario: the ledger is gapless, the stored balance equals the
  ledger-derived balance, and positions equal their fills.

**Not repaired automatically, permanently:** `POSITION_MISMATCH` and
`BALANCE_MISMATCH`. A mismatch is a symptom; writing the quantity to match the
venue destroys the evidence of the cause and de-links the position from the
fills that built it.

## 19a. The intelligence layer

### The headline finding: bars were frozen at seed time

`UpsertBars` had exactly one caller -- the seeder. Nothing turned live quotes
into bars and nothing fetched history on a schedule, so the bar series stopped
advancing the moment the database was seeded.

The consequences compound:

1. Every strategy evaluated the same final bar forever.
2. The orchestrator's per-bar guard -- which exists so one signal is not traded
   twice -- then suppressed every later evaluation with "this bar has already
   been evaluated". A correct guard, permanently jammed.
3. The live quote walked away from the frozen bars. Measured on the running
   stack: the newest 1h bar closed at 2567.27 while the live quote was 2646.50
   -- seven hours and 79 dollars apart. A stop derived from the bar was on the
   wrong side of the live market, and the orchestrator refused the order as
   `price_invalid`, correctly, for a reason that read like a strategy bug.

Nothing reported unhealthy. Readiness was HEALTHY, reconciliation was clean,
every scan was green, and the platform could not have traded autonomously for
more than a few hours after a seed.

**Fixed** by `internal/marketdata.Aggregator`, which folds each quote into 15m,
1h and 4h bars. Only complete bars reach strategies; the mid is used rather
than the bid or ask, so no indicator inherits a spread bias; bucketing is on
the venue's timestamp; a late tick from a closed interval is dropped rather
than rewriting history a strategy may have acted on; and an incomplete bar is
continued across a restart. **Verified on the running stack:** bars now advance
to the present where they had been stuck at 02:00.

### A staleness blind spot

Quote age was measured from `IngestedAt` -- how long since THIS PROCESS
received the quote. A provider that dropped its connection and, on reconnect,
replayed a ten-minute-old price stamped `IngestedAt` with now, so the quote
passed every freshness check while describing a market that had moved on. That
is the exact moment stale data is most dangerous.

Both ages are now measured and the worse decides. `MaxSourceAge` defaults to
60s, deliberately generous, because it exists to catch a price that is old by
minutes rather than to second-guess a well-behaved feed -- and a check that is
too tight becomes its own outage. Three tests pin it, including one asserting
that sub-second provider latency is still healthy.

### Multi-strategy aggregation, which did not exist

The orchestrator evaluated exactly one strategy and routed its signal. Safe,
but it meant the platform had no opinion about the case that matters: a trend
strategy says BUY at 0.75, mean reversion says HOLD at 0.45, a regime model
says RISK_OFF at 0.82, and a high-impact release is due.

`orchestrator.Decide` is a pure, versioned policy. The order of its rules is
the policy:

- **Vetoes first and completely**, before any opinion is weighed. Data quality,
  portfolio state, a high-impact event, a confident RISK_OFF model, a required
  model that could not answer. No confidence reaches past one.
- **Direction is not a majority vote.** Two buys against one sell is a
  disagreement. Netting opposing signals into whichever side weighs more is how
  a system trades its own indecision.
- **HOLD and CLOSE abstain.** Counting them as agreement manufactures consensus
  out of silence.
- **An unavailable optional model is recorded, never read as assent.**
- **UNKNOWN is never coerced** into a regime to let a strategy run.

Thresholds are configurable, the ruleset is versioned so a stored decision
stays interpretable after the rules change, and the defaults are strict. They
are **not tuned numbers**: they are a starting posture chosen so NO TRADE is
the common outcome.

35 unit tests, including the brief's worked example, which produces NO TRADE
with both refusals recorded rather than only the first.

### Canonical regimes

Regimes existed only as bare strings inferred in Python. `domain.Regime` makes
the list authoritative on the side of the boundary that makes refusals, and
`ParseRegime` fails closed to UNKNOWN -- because an unrecognised regime is
precisely a market nobody has characterised.

Recorded rather than hidden: **RISK_OFF is nameable but no classifier infers
it.** It is reachable only from a model that reports it or from an operator.
That is a gap.

### Deterministic scenarios

`marketdata.ReplayProvider` drives time from the data instead of the clock,
with start, pause, step, reset, a spread override and a simulated disconnect.
It never returns a bar the cursor has not reached, so a scenario cannot quietly
become a look-ahead test.

Fourteen tests cover the brief's ten scenarios A-J. Each is asserted
reproducible across ten runs, and each fixture is checked to be the market it
claims: the "trend" series is asserted to drift more than 5% and NOT to be a
volatility event as well, so a pass is attributable to the trend.

**What they do not do:** they exercise the decision layer, not the full live
pipeline. Wiring the replay provider through `internal/app` needs configuration
plumbing that does not exist yet.

### The Autopilot switch

There was no single control for "stop the robot". Automation was gated by kill
switches, per-account authority and the reconciliation halt, but nothing said
whether autonomous decision-making was permitted at all.

Deliberately not a kill switch: a kill switch halts every order including an
operator's, which is the wrong control for "I am taking over by hand". Default
OFF, a mandatory reason, append-only history, ADMIN to change and any role to
read, enforced inside the order transaction with its own rejection code.

**Verified against the live stack:** anon 401, viewer 403, trader 403, admin
without a reason 422, admin with a two-character reason 422, admin omitting
`enabled` 422, admin valid 200. The migration applies twice cleanly.

### The untested packages

The previous revision named `orchestrator`, `httpapi`, `fx`, `ratelimit`,
`marketdata`, `quant` and `scheduler` as having no unit tests. All seven now
do:

| Package | Tests | What the cases are about |
| --- | --- | --- |
| `orchestrator` | 49 | The consensus policy and the ten scenarios |
| `marketdata` | 59 | Mock determinism, OHLC consistency, replay no-look-ahead, bar aggregation, feed alert transitions |
| `quant` | 23 | Every malformed-signal case becomes NO TRADE upstream, so a permissive validator is a trading defect. Plus the breaker: a 5xx counts, a 4xx does not |
| `fx` | 13 | A stale direct rate must not fall through to a fresher inverse; no rate must never become 1.0 |
| `scheduler` | 11 | Panic recovery, cancellation, the per-job timeout, and that a slow job is never entered twice |

| `ratelimit` | 16 | The failures here are all silent: a bucket that never refills locks a trader out of closing a position, one that refills too fast removes the control, and a key built from the wrong parts lets one caller spend another's budget. Also two configuration invariants -- cancelling and the kill switch can never be scarcer than placing, or the platform can be filled faster than it can be stopped |
| `httpapi` | 22 | The layer under the handlers: the error envelope every endpoint returns through, the request decoder, and the helpers whose failure modes are security-relevant. An internal error string never reaching the client; not-found and not-yours being indistinguishable so no endpoint becomes an enumeration oracle; an unknown field refused rather than silently dropped; `X-Forwarded-For` ignored unless a proxy is trusted, so a caller cannot pick a new address per request and walk past the login rate limit |

The handlers themselves still need a database, a broker and a keyring to
construct, so they are covered by the smoke and Playwright suites rather than
by unit tests. That is a real limit and is recorded as one.

## 19b. Market replay

### What it drives

`Scheduler.ReplayStep` calls the same job functions the interval loops call, in
the same order. There is no second trading engine, deliberately: a harness that
called ingestion and the orchestrator itself would prove the harness worked,
and every difference between it and the scheduler would be a difference nobody
notices until it matters. `TestTheReplayEngineHoldsNoBrokerAdapter` stops the
engine reaching into execution.

**Measured on one 140-bar dataset:** 665 signals, 250 orders (189 filled, 61
refused with `exposure_limit_breached`), 189 fills, 64 ledger transactions,
2.51 lots traded, a gapless ledger (64 of 64), and a stored balance exactly
equal to the ledger-derived balance.

### Determinism

Two runs of `trend-clean` from a byte-identical database (`pg_dump` /
`pg_restore` between them) produce byte-identical financial output: signals,
orders, fills, quantities, transactions, closing balance and open positions.

The first attempt did not. It produced identical *decisions* -- 665 signals,
250 orders, 189 fills, 2.51 lots -- and different closing balances, 574.81
against 575.38, because the mock venue seeded its slippage jitter from
`time.Now().UnixNano()`. The venue is now seeded in replay mode, and seeded
rather than switched to `Deterministic`: that flag disables jitter and random
rejection entirely, which would remove the reason the mock venue exists.

### Replay time is application time

`domain.Clock` is a one-method interface and nothing in the trading path calls
`time.Now()` directly, so substituting it moves quote ages, bar buckets,
session state, strategy scheduling, event windows, daily P&L boundaries and
order timestamps together. A run started on a Sunday in December produces the
same decisions as one started on a Tuesday in March.

The trade-off is stated rather than hidden: while a run is engaged every clock
reader in the process sees replay time. That is acceptable only because replay
is explicit, refused outside development and test, and never the default --
and because security lifetimes were carved out after the defect below.

### Datasets

Nine committed CSV fixtures, 119 KB in total, generated by a committed
closed-form script so a diff to a fixture is explainable by a diff to the
generator. Identity is the SHA-256 of the *parsed rows*, so a comment or a line
ending does not change a dataset and a changed price does.

Named by allowlisted ID and never by path: a control endpoint taking a filename
would be a file-read primitive wearing a trading-system costume. Larger
datasets are documented as belonging in content-addressed object storage rather
than in version control.

### What a replay assumes

Recorded because each one bounds what a result means:

- **FX rates are held constant**, so a replay's P&L contains no currency
  movement.
- **Bars come from the dataset**, and quote-to-bar aggregation is detached for
  the duration of a run.
- **The market-data and strategy interval loops are suppressed.**
- **A run starts from clean replay market data.**

## 20. Regulatory boundary

Non-custodial by construction: no pooled capital, no omnibus account, no
collective investment structure, no Vantage-controlled wallet, no advice, no
performance claim. Trading authority is described only as a technical control,
and the notice travels in the API payload itself.

`docs/REGULATORY_BOUNDARY.md` draws no legal conclusion and states plainly
that architecture cannot decide its own regulatory status.

## 21. Frontend

18 pages, no placeholders, no empty navigation. Money arrives as decimal
strings and is formatted without ever constructing a float. One polling loop
feeds the whole terminal.

**Verified:** typecheck, lint and production build clean; all 18 pages walked
in a browser against the live stack and observed rendering real data.

## 22. Next.js upgrade -- a critical vulnerability found and fixed

Trivy found the pinned Next.js 15.1.6 carried **CVE-2025-55182, a
pre-authentication remote code execution** via unsafe deserialization in React
Server Components, plus ten further advisories.

Upgraded to Next 16.3.4 with React 19.2.8, then to a patched ESLint 9.x
(ESLint 10 breaks `eslint-plugin-react`). Result: `npm audit` reports 0
vulnerabilities, Trivy reports 0 HIGH/CRITICAL.

**Verified:** typecheck, lint and build clean afterwards, and the production
build walked page by page in a browser.

## 23. React correctness issues found by the upgrade

Next 16's React Compiler rules surfaced four real defects: a ref written during
render, a synchronous `setState` inside an effect, and `Date.now()` read during
render in two pages.

Fixed properly rather than suppressed: the ref moved into an effect; `useAsync`
restructured so `loading` is *derived* by comparing the answer held against the
request wanted (one state write per fetch instead of three); and a `useNow`
hook introduced so the blackout badge and countdowns actually advance.

**Verified:** ESLint clean; the calendar page observed showing a live blackout
for an event 8 minutes away.

## 24. Python type safety

`mypy` was configured but had never actually run: it failed on missing pandas
stubs, so nothing was checked. Adding `pandas-stubs` and targeting 3.12
revealed **16 real typing gaps** -- a `Literal` action erased to `str` in eleven
places, and `float | None` used as `float` in the backtester's exit path.

Fixed by typing the strategy sides as `Action` and by carrying the level that
was hit alongside the fact it was hit, which narrows the type for free.

**Verified:** `mypy` reports "Success: no issues found in 7 source files";
ruff clean; 196 tests still pass.

## 25. Secret scanning

Gitleaks initially reported 14 findings. All triaged: two were the documented
development keys in `.env.example`, whose plaintext begins with
`development-only` and which `config.Load` **refuses to start with in staging
or production**; the rest were build output and vendored test data, none
tracked by Git.

`.gitleaks.toml` records each exclusion with its reason, plus a project rule
for Postgres URLs carrying a non-development password.

**Verified:** "no leaks found". `.env` is not tracked; `git ls-files` contains
no `.env`, key material, model artefact or dataset.

## 26. Static analysis

Semgrep across `security-audit`, `golang`, `python`, `typescript`, `react` and
`dockerfile`: initially 7 findings, all false positives on inspection --
`math/rand` in the *simulation* (seeded deliberately so tests assert exact
fills), `Secure` set from the environment rather than a literal, and
`HttpOnly: false` on the CSRF cookie, which the double-submit pattern requires.

Each is suppressed with the reasoning written next to the code, not globally.
Two files were also only *partially* parsed because of a bare `&` in a JSX
attribute, meaning they were not actually being scanned; that was fixed too.

**Verified:** 0 findings, 0 parse warnings, 143 files scanned.

## 27. Dependency and container scanning

| Scanner | Result |
| --- | --- |
| `govulncheck` | 0 reachable. One module-level advisory (GO-2026-5932, `x/crypto/openpgp`, unmaintained, no fix) accepted: the module is required for Argon2id and that package is never imported |
| `npm audit` | 0 vulnerabilities |
| Trivy filesystem | 0 HIGH/CRITICAL in npm and gomod |
| Trivy config | **carried -- not re-run** | 0 misconfigurations across three Dockerfiles |
| Trivy image | 0 HIGH/CRITICAL in all three, **under CI's `--ignore-unfixed`** |

That last row needs its qualifier stated, not buried. Re-scanned on 2026-09-08
WITHOUT `--ignore-unfixed`:

| Image | HIGH/CRITICAL | Notes |
| --- | --- | --- |
| control-api | 0 | A static Go binary on a minimal base; there is almost nothing else in it |
| web | 0 | |
| research (quant) | **54** | All from `python:3.12.12-slim-trixie` (Debian 13.6): `perl-base` (3 CRITICAL), `util-linux` and its libraries, `ncurses`, `gzip`, `libacl1`, `libsqlite3-0`, `libsystemd0` |

Re-scanned again on 2026-09-08 after this milestone, with no severity filter
and no ignore file at all, the picture is unchanged: control-api 1 (the
module-level openpgp advisory), web 0 at every severity, research 173 (3
CRITICAL, 51 HIGH, 57 MEDIUM, 57 LOW, 5 UNKNOWN) plus the 3 accepted
application-level findings in pip's vendored tree.

Every one of those 54 HIGH/CRITICAL has an **empty fixed-version field** --
Debian has not published a fix -- and the Dockerfile already runs `apt-get upgrade -y`, so
there is nothing to apply. None is added to `.trivyignore`, deliberately:
suppressing those ids would also hide them once fixes land, which is precisely
when they should reappear.

The exposure is bounded rather than absent. The research service holds no
broker client and no credentials pointing back at the control plane, its
database role is read-only, it runs non-root on a read-only root filesystem,
and nothing in uvicorn or the numeric stack executes perl, mount helpers or
ncurses. The real fix is a minimal base -- about 750 MB of that 813 MB image is
not needed at runtime -- and that is recorded as remaining work, not attempted
at the end of an audit.

Getting the images this far required real work: bumping stale base tags,
applying distribution security updates in the final stage, and **removing npm
from the web runtime image** -- the server never installs a package, and a
package manager in a production container is a way for anything that gets a
shell to fetch its next stage.

`.trivyignore` records the two accepted application-level findings. Both are
inside pip's own `_vendor` tree -- `msgpack==1.1.2` and `setuptools==70.3.0` in
`pip/_vendor/vendor.txt`, confirmed by reading the file in the built image.
They are not importable by the application and are reachable only while pip
itself runs, which the service never does. This is also why
`importlib.metadata` reports setuptools 84.0.0 in that image: the vendored copy
is a directory of files, not an installed distribution.

## 28. Containers

| Image | Size | User | Notes |
| --- | --- | --- | --- |
| control-api | 34.6 MB | `65532:65532` | `scratch`: no shell, no package manager, no toolchain |
| quant | 813 MB | `vantage:vantage` | numpy, pandas and scikit-learn account for most of it |
| web | 310 MB | `node` | npm removed from the runtime |

The control-api image needed the IANA database **embedded** via `time/tzdata`:
the market clock resolves New York at construction, and a `scratch` image has
no `/usr/share/zoneinfo`, so the alternative was a start-up failure.

**Verified:** all three build; the control-api image runs and identifies itself
as paper-only; the compose file's hardening (read-only root, dropped
capabilities, `no-new-privileges`, loopback-only ports) matches what the images
support.

## 29. Documentation

Twenty-three documents: `README.md` plus twenty-two in `docs/`.

Several documentation claims were **found false against the code and corrected**
rather than left standing:

- the event blackout was documented as binding manual orders; it binds
  automated ones only, and the terminal said the wrong thing too
- the calendar and news provider interfaces were documented but did not exist
  -- they were built
- "there is deliberately no default" for the encryption keys was wrong: the
  template ships recognisable development keys and the loader refuses them in
  staging and production, which is the better design and is now what the docs
  describe
- Go and Node version floors were understated in the docs and, worse, in the
  Dockerfile, where the pinned 1.23.5 could not build a module requiring 1.26
- `ORDER_LIFECYCLE.md` claimed a flatten "carries `source = manual`" and used
  that to explain why the event blackout does not block it. It carries
  `source = risk_control`, and the blackout *did* block it -- defect 7. The
  document was not merely stale: it described a safety property the code did
  not have, which is the kind of documentation error that stops anyone looking
  for the bug
- `CommissionPerLot` was documented as being "in the account currency" while
  both the venue and the booking path treat it as the quote currency. A reader
  trusting the comment would double-convert

Each document names what is missing, not only what exists.

`RECONCILIATION.md` was rewritten for this milestone and is the longest of
them: the governing rule, the run shape, the thirteen-type taxonomy with why
each automatic repair is provable and each operator case is not, the
identifier-ownership table across `VantageOrderID` / `ClientOrderID` /
`BrokerOrderID` / `BrokerExecutionID`, the external-activity policy, halt
scope, and the limits that remain.

## 30. Defects this audit found, and what it changed

The audit in commit `c9bca9e` was not a documentation pass. It found six real
defects, five of them by writing a concurrency suite that drives a running
stack rather than a fake.

That method matters, because three of the five were arbitrated by PostgreSQL
rather than by Go. A unit test with a fake store proves the fake serialises the
way the test author imagined; it cannot prove a constraint exists, that an
UPDATE names the right columns, or that two real transactions actually
conflict.

### 1. A deadlock on concurrent order placement, which silently created phantom fills

Six of eight simultaneous orders on one account died with `deadlock detected
(SQLSTATE 40P01)` and returned HTTP 500 -- the least useful answer an
order-placement API can give, because the caller cannot tell whether the order
exists.

The cycle was invisible in the source. No two statements lock in a different
order; the asymmetry came from a foreign key. Every table in an account's graph
carries `account_id REFERENCES accounts (id)`, so INSERTing a fill takes an
implicit `FOR KEY SHARE` on the account row. The explicit `FOR UPDATE` then
came **last**, with the positions lock wedged between them:

```
T1  INSERT fill      -> KEY SHARE on accounts (implicit, via the FK)
T1  SELECT position  -> FOR UPDATE on positions          [held]
T2  INSERT fill      -> KEY SHARE on accounts (granted; compatible)
T2  SELECT position  -> waits for T1
T1  SELECT account   -> FOR UPDATE, conflicts with T2's KEY SHARE -> cycle
```

The 500 was not the worst of it. The mock venue had already committed and
**filled** those orders; Phase D rolled back. Reconciliation later reported
exactly that -- `Vantage 0, venue 0.01` -- so six real executions existed that
the ledger knew nothing about. The orders themselves sat in `ACCEPTED`
permanently: uncancellable (`order_not_at_venue`, because Phase D never wrote
the venue id) while still consuming the pending-order budget, until the account
reached `8 pending orders against a limit of 5` and could not trade at all.

Fixed by declaring a lock order and taking it first. `store.LockAccountTx` is
now the outermost lock for every transaction that writes anything belonging to
an account, and the comment there records the cycle so the rule is not
mistaken for style. `40P01` is classified (`store.ErrDeadlock`), Phase D
retries once, and -- the part that matters most -- a Phase D failure after venue
acceptance now routes to `failOrder`, marking the outcome UNKNOWN for
reconciliation instead of returning a bare error that left the order stranded.
A Prometheus counter that must stay at zero guards the regression.

### 2. An exposure limit that prevented reducing exposure

Flattening a 3866 ZAR position was refused: `Order notional 3866.77 ZAR against
a limit of 2500.00 ZAR`. A position built by several individually permitted
orders could grow past the per-order cap and then become **impossible to
close**. The control that exists to contain risk prevented shedding it, and the
larger the position the harder the exit.

The engine already had a `reducing` concept -- and applied it to exactly one of
the checks. It now applies to the notional and net-exposure checks too.

Its definition was also too permissive, which was the more dangerous half:
"opposite side" alone let an account long 0.08 lots SELL 5.00 and skip the
gross-exposure, per-instrument and concentration checks entirely -- 0.08 of that
is a close and 4.92 is a large new short taken with the limits switched off.
Reducing now means opposite side **and** no larger than the position held. Four
tests pin both halves.

### 3. A normal race outcome reported as a server fault

Eight simultaneous cancels of one order produced one 200 and seven 500s.
Exactly one cancel took effect, which was correct, but the state machine's
refusal (`CANCEL_PENDING -> CANCEL_PENDING`) was unclassified and fell through
to "internal error". Losing a race is an expected outcome; reporting it as a
server fault is how a client learns to retry something that will never succeed.
It answers 409 now.

### 4. An order that never reached the venue could not be resolved

Reconciliation flagged it critical and stopped there. There is no endpoint to
resolve a discrepancy by hand, and cancellation refuses an order with no venue
id -- so it was permanently wedged. Reconciliation now closes out an open order
that holds no venue identifier **and** that the venue does not recognise by
client id, on the same evidence the FAILED branch already used: a direct lookup,
not a gap in a paginated list. An order holding a venue id the venue denies
stays critical and manual, because that is a different and far more alarming
thing.

### 5. An unvalidated instrument reaching the database

`POST /strategies/{id}/run` passed `instrument_id` straight through. A missing
or unknown value came back as `strategy_runs_instrument_id_fkey rejected the
write` and was reported as HTTP 500 "The strategy could not be evaluated" -- an
internal error for a plainly bad request. Validated up front, answered 422.

### 6. A data-leakage hole in the research plane

`ml.train` accepted `horizon` and `embargo_bars` independently, and the API
exposed both. With `horizon=5, embargo_bars=2` the last three training rows are
labelled from closes that fall **inside** the validation window: the model is
trained on the outcome of the bars it is then scored against, and the
validation number measures nothing.

The defaults (horizon 1, embargo 2) were always safe. The combination was one
HTTP request away. An embargo narrower than the horizon is now refused rather
than silently widened -- a caller who asked for a 2-bar embargo on a 10-bar
label has a mistaken mental model, and quietly giving them a different split
than the one recorded in the model's provenance hides that instead of
correcting it. The "zero embargo" warning that could no longer fire was
replaced rather than left as decoration.

### And the documented setup path, which did not work on a fresh clone

`.env.example` never defined `VANTAGE_MIGRATION_DATABASE_URL`, and
`dev-up.ps1` never loaded `.env` for its own `go run` calls -- so
`./scripts/dev-up.ps1 -Reset -Seed` failed with `permission denied for schema
public` on any machine where the operator had not exported the variables by
hand. `DEVELOPMENT.md` had documented the variable for a year without anything
supplying it.

The same script also aborted mid-teardown, because PowerShell 5.1 wraps a
native command's stderr in an ErrorRecord and `docker compose` reports progress
there -- so `$ErrorActionPreference = 'Stop'` turned normal output into a fatal
error. `2>&1` does not fix that in 5.1; the preference has to be relaxed around
the call and restored after.

Both fixed, and verified by a full reset from an empty volume.

---

## 30a. Defects the reconciliation milestone found

Building the repair path meant exercising divergence deliberately, and that
surfaced nine more defects. Three of them were in code the previous audit had
already passed as correct, and one of them -- defect 15 -- was in the repair
subsystem itself, defeating its own acceptance criterion.

### 7. Three risk checks refused to let an operator close a position

A flatten was refused with `event_risk_blackout`. Then, once that was fixed,
with `daily_loss_limit_reached`. The first was found by a Playwright fixture
that could not clean up after itself; the second by the race suite failing in a
way that looked like a concurrency bug.

The cause in both cases: a check written to *limit* exposure or loss was
applied to an order that *reduced* it. The event blackout's own comment claimed
humans could always close a position -- and it was true for `SourceManual`, but
a flatten carries `SourceRiskControl`, so the exemption never applied to the
one order type it was written for. The daily-loss and drawdown ceilings had no
exemption at all: once breached, the account was locked into the losing
position that breached it, while it went on losing.

This was the third instance of one mistake. The previous audit found the same
inversion in the per-order notional cap: a position built by several
individually permitted orders could not be closed by one order, because closing
it exceeded the per-order limit.

The fix is a rule rather than a third patch, written out in
`internal/risk/engine.go`: **a check whose purpose is to limit exposure or loss
must never refuse an order that strictly reduces exposure.** "Strictly
reducing" is opposite side AND quantity no greater than the open position --
both halves, because "opposite side" alone let an account long 0.08 lots sell
5.00 and skip the exposure family entirely. Seven tests, and the rule is also
recorded in `ENGINEERING_GUIDE.md` and `docs/ORDER_LIFECYCLE.md` because the failure is
invisible in normal operation: everything looks correct until the day someone
needs out.

### 8. The admin-only repair endpoints were unreachable by admins

Every operator action returned "Account not found". `accountForRequest` resolves
an account **through the authenticated user**, which is exactly right for a
trading route and exactly wrong here: an admin owns no trading account, and
repair is admin-only by design. So the only role permitted to use the endpoints
was the only role that could not.

It was found because seven Playwright security tests silently *skipped* rather
than failed -- they could not construct the precondition. A skip that reads as
"nothing to test here" is worse than a failure.

Fixed with `accountForOperations`, which widens to any account for ADMIN only.
The same defect was then found in two more handlers. It is now locked by
`TestEveryReconciliationHandlerUsesTheOperationsScope`, because the failure
mode is silent: the route answers, plausibly, with the wrong thing.

### 9. A filled order was reported as missing from the venue

`FetchOpenOrders` returns open orders. A filled order is not open, so every
locally-filled order looked like it had vanished from the broker, and the
proposed repair was to mark it rejected. Only the terminal-state guard in the
repair transition table stopped it.

Fixed by capturing the local snapshot **first** and resolving every order the
open list does not cover individually by client id, plus an independent guard
that refuses to call an order missing when the venue is simultaneously
reporting an execution against it. Two tests.

### 10. One problem produced two contradictory issues

A single lost fill raised both `FILL_MISSING_LOCALLY` ("safe to import
automatically") and `PARTIAL_FILL_MISMATCH` ("an operator must review this").
Worse, the second survived the first's repair and kept the order flagged, so a
successful automatic recovery still left the account halted.

Fixed by threading an `explained` set from the execution pass into the order
pass: an order whose discrepancy is already accounted for by an execution
finding does not get a second, contradictory one.

### 11. Transient divergence halted an account permanently

Issues were opened and never closed unless an operator closed them. A
divergence that resolved itself -- a fill arriving between two runs -- left an
open issue and a halted account with nothing left to look at.

Fixed by closing open issues whose fingerprint the current run did not
re-detect and which predate that run.

### 12. Ordinary replays were reported as duplicate executions

The execution cursor deliberately overlaps by a minute, so every poll re-sees
recent executions. Each re-sighting raised a `DUPLICATE_EXECUTION_REPORT` and
counted as a repair, which made the repair count meaningless -- the exact
notification storm the brief warns against.

Redefined: a duplicate report is only a finding when the venue re-reports the
same execution id with *different* quantity, price or side. That is a real
contradiction, it is critical, and it requires an operator. An identical
re-report is the steady state and is silent.

### 13. Migration 0010 halted the account with evidence nobody could act on

The migration imported the old `reconciliation_discrepancies` rows as OPEN
issues. Fifty-four of the resulting sixty-three open issues were legacy rows
carrying only `{"value": "..."}` as evidence and a `migrated:<id>` fingerprint,
which no operator action and no RECHECK could resolve.

Fixed by migration 0011, which closes them as RESOLVED with a written reason
and clears the order flags they left behind. The lesson is narrow but real: a
migration that creates *work items* has to consider whether the work is
actionable.

### 14. The mock venue's own balance was in the wrong currency

The new balance check reported a `BALANCE_MISMATCH` that grew with every trade:
ledger 486.12, venue 499.24, and the ratio of their movements was 18.25 -- the
USD/ZAR rate.

The venue was crediting realised P&L, denominated in the instrument's quote
currency, straight into a rand-denominated balance, and it reported a
commission on every fill that it never deducted. Both are the venue's own
bookkeeping, not Vantage's, so no operator action could ever have repaired it.

That made the check worse than useless: a permanent warning nobody can clear is
how people learn to ignore warnings. Fixed with a `RateSource` seam on the mock
venue that converts realised P&L, commission and swap into the account's
currency, failing closed when no rate is available. After the fix, on a fresh
seed and a full race suite, the venue reads 492.0302 and the ledger 492.0200 --
a rounding difference, well inside tolerance.

It is worth being clear about what this was: reconciliation found a real
accounting bug in a component that had passed every previous test, because no
previous test compared the two books at all.

### 15. An unresolved ambiguous execution closed itself half an hour later

The most serious defect of the milestone, because it defeated the milestone's
own acceptance criterion by a side door.

`closeVanishedIssues` closes an open issue the current run did not re-detect,
reasoning that a run captures and classifies both views completely, so a
fingerprint absent from the result no longer exists. For executions that
reasoning holds only inside the stored cursor window. Once the cursor advanced
past an unattributable execution, later runs stopped fetching it -- and "not
re-detected" silently changed meaning from **fixed** to **no longer
examined**.

Observed on a live stack: three orphan venue executions, still present and
still unbooked in `mock_venue_fills`, whose `EXTRA_BROKER_FILL` issues were
resolved by a scheduled run about thirty minutes after detection with the
reason "The divergence is gone". Readiness went back to HEALTHY and automation
was permitted again. Nothing had been fixed and no operator had seen anything.

This is precisely what the brief forbids: an ambiguous broker-side execution
must remain unresolved and require operator review. It did, for half an hour,
and then quietly did not.

The fix **freezes the execution cursor** while any execution-derived issue is
unresolved, so a run cannot stop looking at evidence nothing has resolved.
Position and balance mismatches need no such treatment: they are computed from
a full position snapshot on every run and are therefore always re-examined.

The first attempt at the fix was to widen the window back to the issue's
detection time instead, and it had the same hole in miniature: an execution
detected later than the cursor overlap would fall outside the widened window
too, and the issue would close itself again. A frozen cursor is still the
window that saw the execution in the first place, so it cannot stop seeing
it.

Two things about how it was found are worth recording:

- **The existing acceptance test could not catch it.** That test runs
  reconciliation *once*. Only a second run, after the cursor has moved,
  exposes the behaviour. `TestAnUnresolvedIssueSurvivesLaterRuns` now runs four
  and asserts the issue is still open after each.
- **It was found by looking at the operations page, not by a test.** The page
  said HEALTHY with zero open issues on a database that demonstrably still
  contained three unbooked venue executions. A test suite that had just
  reported 14/14 green did not disagree with it.

---

## 30b. Defects the intelligence milestone found

### 16. Bars stopped advancing after the seed

Covered in full in section 19a. The most serious defect found in three
milestones, because it made autonomous operation structurally impossible while
every health signal read green.

Worth recording how it surfaced: not from a test, but from running a strategy
by hand and reading the refusal. The orchestrator said `price_invalid` -- "the
strategy suggested a stop at 2585.68 for a sell at 2646" -- which reads like a
strategy bug. Comparing the newest bar against the live quote is what showed
that the two series were seven hours apart. A test suite reporting 51/51
Playwright and 15/15 race did not disagree.

### 17. A reconnecting provider's stale price read as fresh

Quote age was measured only from arrival, so a replayed old price passed every
freshness check. Fixed by measuring the venue's own timestamp too and taking
the worse. Found while building the replay provider, because the provider
deliberately stamps the bar's time as the source time -- and the test that
asserted the platform would notice failed.

### 18. There was no way to stop the robot

Automation could be halted per-account by reconciliation, per-scope by a kill
switch, and per-account by authority -- but there was no single control for
"autonomous decision-making is not permitted", and no record of when it had
been. Added, with the kill-switch distinction made explicit because conflating
them would force an operator to disable a protection in order to take over by
hand.

### 19. Distroless would have hidden CVEs rather than removed them

Not a defect in the code -- a defect avoided in the fix. The research image
carries 173 raw findings (3 CRITICAL, 51 HIGH) from its Debian base, and
distroless was attempted to reduce them.

Measuring what the numeric stack actually needs showed that scipy and
scikit-learn bundle their own libgomp and OpenBLAS, so the hard part was easier
than assumed: 29 system libraries. But four of them -- sqlite3, ncurses, krb5
and openssl -- would have been copied in as bare files with no package database
behind them. Trivy would have stopped reporting their CVEs while the vulnerable
code was still in the image, taking the count from 173 to nearly zero with
almost none of that improvement being real.

Not adopted. The measurement and the recommended alternative (trim the
`lib-dynload` modules the service never imports, which removes the libraries
rather than hiding them) are recorded in `.trivyignore` so the next attempt
starts from the finding rather than repeating the work.

### 20. The standalone build needed an undocumented manual step

`output: "standalone"` omits static assets, so running the standalone server
answered 200 for the HTML and 404 for every chunk as `text/plain`. The app
never hydrated and around twenty Playwright tests failed on missing selectors,
looking exactly like a broken API. Fixed with a `postbuild` assembly step; a
build now always produces something runnable.

---

## 30d. The attribution defect, and what attribution found

### The defect

`AttributionByInstrument` grouped CLOSED POSITIONS and converted each one's P&L
into the account's currency, with `if err != nil { continue }` on the
conversion. A missing or stale FX rate therefore removed that position's money
from the report silently, and the total was quietly not the account's total.
Nothing failed; the number was just wrong, in the direction of looking tidier.

Attribution now folds the **ledger**. Every movement of money is exactly one
transaction row, already denominated in the account's currency because booking
converted it once, at the time, with the rate that actually applied. Each unit
of money is therefore counted exactly once by construction: double counting
would need a duplicate ledger row, which the sequence constraint forbids.

Two reads, not one. The entries are folded into buckets; the account's own
entry count and net movement come from a separate query; the report compares
them and reports itself **unreconciled** when they disagree. A fold that
reported its own sum as the account's total would be internally consistent and
wrong, which is the worst kind of financial number.

Every entry lands in exactly one bucket, including the ones that cannot be
attributed. A deposit has no order and therefore no strategy, and dropping it
would make the buckets sum to less than the account -- so it lands in a bucket
that says why. Deposits are also held apart from performance entirely: `Other`
never enters `Net`, because funding an account is not a profit.

### What it found immediately

On one replay of `trend-clean`, reconciled at 25 of 25 ledger entries in all
seven dimensions:

| Strategy | Net | Trades | Win rate |
|---|---|---|---|
| Donchian Channel Breakout v1 | -26.17 | 13 | 0.00 |
| Bollinger Z-Score Reversion v1 | -0.11 | 3 | 0.33 |
| RSI Mean Reversion v1 | +5.62 | 8 | 0.75 |

A promoted strategy losing thirteen trades out of thirteen was invisible before
this existed: the account's -20.66 was the only number available, and it looked
like mild underperformance rather than one strategy losing money on every
attempt while another paid some of it back. **This is one 140-bar simulated
replay and is not a claim about any strategy's merit.**

By session, the same run: tokyo -11.50 over 9 trades at an 11% win rate,
new_york -4.80 over 4 at 0%, london -4.66 over 8 at 63%, and the London/New
York overlap +0.30 over 3.

### The split that matters most

A replay writes real orders through the real OMS into the real ledger, which is
why it is evidence. It also means a replay's P&L and a paper-forward session's
sit in the same account, in the same tables, indistinguishable -- and a
replay's orders were decided against dataset prices at a dataset instant, so
reading them as evidence about live behaviour is the expensive mistake.

`orders.replay_run_id` closes that. An untagged order was decided on a live
simulated feed; a tagged one was not. `?by=run_kind` splits the two and
`?by=replay_run` separates individual runs, so two replays of one dataset can
be compared from the ledger rather than by capturing an API response and
trusting nothing changed in between.

The tag is read at order-creation time rather than supplied by the caller,
because a MANUAL order placed while a replay is engaged was also decided
against dataset prices. A deposit is neither kind and goes to the unattributed
bucket: filing funding as one side of the comparison would corrupt the
comparison.

### What the session dimension needed

Nothing recorded the liquidity session a decision was taken in. The decision
snapshot held `market_status` -- open or closed -- which is not a session.

Sessions are now recorded on every decision, both as the full active set and as
a single `session` label reduced from it, and attribution groups by the
recorded label rather than recomputing a calendar over the order's timestamp.
Recomputing would judge an old trade against a calendar that has since been
amended, so a holiday correction would silently reattribute historical P&L.

Costs deserve one caveat: the mock venue charges no commission on `XAUUSD.m` in
this configuration, so all 48 fills in that run had zero commission and zero
slippage and the cost column read 0.00. The routing of costs is proven by unit
test rather than by that run.

---

## 30c. Defects the replay milestone found

Four of these were in code that three previous milestones had passed as
correct. All were found by making the platform do something it had never been
asked to do.

### 21. Session expiry was denominated in simulated time

Engaging a replay dated 2027 instantly expired the operator's own session, so
the authenticated API could not drive the replay it had just started. The
symptom was a 401 out of nowhere.

The direction it happened in was the harmless one. A replay dated in the
**past** would have kept an already-expired session alive, which is a security
failure rather than an inconvenience. Authentication now reads a wall clock
that no replay can move, and an architecture test pins it -- this is the kind
of guarantee that decays silently.

### 22. An off-by-one made every quote arrive from the future

The provider was stepped before the clock was advanced, so it served bar N+1
while the clock said bar N. Every quote was two hours in the future, the
data-quality policy correctly rejected the entire run, and the symptom looked
like a broken clock. The cause was the order of two lines.

### 23. Two ingestors ran at once

Both the two-second ingestion loop and the replay step ingested, so each replay
instant produced two identical quotes. 75 of 685 strategy runs were skipped for
`duplicate_tick` -- degraded data the harness itself had manufactured. The
interval loops are now suppressed when a replay owns the pipeline;
reconciliation, outbox and cleanup keep running, because a replay should
exercise the real system's background behaviour rather than replace it.

### 24. Bars had no ingestion path from a provider

`UpsertBars` reached the store from the seeder or the quote aggregator and
nowhere else, so a provider that supplies real bars had no way to deliver them.
`Ingestor.BackfillBars` is that path. It is bounded twice against look-ahead --
by the clock and by the provider's own cursor -- because relying on one
guarantee in one implementation is how look-ahead gets in.

### 25. Sizing failed on stale FX rates

`fx: conversion rate is stale: USD/ZAR is 4310h39m10s old`. The account is ZAR
and gold is quoted in USD, so sizing cannot be computed without a rate, and
seeded rates are dated at seed time -- which a dataset deliberately ahead of
the seed makes months old. The converter refused, correctly, and every strategy
run failed.

Rates are now re-stamped at each replay instant at their seeded value. That
makes the assumption explicit: a replay's P&L contains no currency movement.

### 26. A replay could not run twice without lying

The one defect that was genuinely subtle. Quotes are upserted unconditionally
and the upsert moves the outgoing quote into `prev`, so starting a dataset
dated 2 March while the database still held a quote from a previous run at 10
March produced two effects at once: the incoming quote read as
`timestamp_regressed`, and for the rest of the run the orchestrator saw a
stored quote eight days in the **future** and refused with `future_timestamp`.
In one measured pass that was 415 of 1082 strategy runs skipped.

Every one of those refusals was correct. That is what made it hard to see: the
platform was behaving properly and the data was wrong. Bars were worse, being
upserted by `(instrument, timeframe, open_time)`, so a different dataset's bars
would silently feed this one's indicators.

A run now clears replay-provided quotes, bars and health rows first. Verified:
two consecutive runs in the same process and database, with no restore between
them, produce 665 strategy runs with zero skips -- identical to a run from a
freshly restored snapshot.

### And the dev seed's authority window

Ninety days, which expires before a 2027 dataset even starts: 610 strategy runs
skipped with "Trading authority has expired". A correct refusal for an
invisible reason. Now three years, and the engine's preflight reports a missing
authority or a disabled Autopilot **before** a run begins rather than leaving
it to be inferred from an empty result.

---

## 30f. ReplayRun: the verified state, and why the record contradicted itself

A previous revision of this report said `ReplayRun` was not persisted, and a
later one said it was. Both were true when written -- the table was added in
between -- but the two statements outlived the change and read as a
contradiction.

**Verified against the repository, not from memory:**

| Claim | Evidence |
|---|---|
| The table exists | `migrations/0013_replay_runs.sql`, applied; `\d replay_runs` shows 17 columns |
| It is written | `internal/store/replay.go` → `RecordRun`, wired in `internal/app/app.go` via `Engine.SetRecorder` |
| It is read | `GET /api/v1/replay/runs` and `/replay/runs/{id}`, admin-only |
| Orders carry it | `orders.replay_run_id` (0014), with a foreign key that is deliberately not `ON DELETE CASCADE` |
| It records identity | dataset id and hash, code SHA, config hash, seed, dataset span, wall-clock start and finish, state, counters, warnings |
| It cannot claim to be live | `CHECK (simulated)`; the app role has INSERT and UPDATE but not DELETE |

Measured on the scenario matrix: nine runs recorded, every one carrying a
dataset hash and a code SHA, none claiming to be unsimulated.

The lesson is about the report rather than the code. A standing document that
describes a moving system needs its claims dated or derived, and this one had
two undated claims about the same fact. Sections that state a capability now
say what was run to establish it.

---

## 30e. Two broken workflow references, found by pinning the actions

Pinning every GitHub Action to a commit SHA was meant to be a supply-chain
change: `actions/checkout@v4` is a MUTABLE tag, so a retagged or compromised
action would run with the workflow's permissions and nothing in the repository
would look different. Thirty-six references now name a 40-character commit,
each resolved through the GitHub API and verified twice -- that it is a real
commit in that repository, and that the named tag still points at it.

Resolving them turned up two references that do not exist.

### `aquasecurity/trivy-action@0.28.0`

There is no such tag. The repository tags its releases `v0.28.0`, with the
`v`. Both Trivy jobs -- the filesystem scan and the image scan -- would have
failed at "resolve action" before scanning anything.

Pinning the commit that `v0.28.0` points at fixes the reference and keeps the
version the workflow always meant, so this is not a version change.

### `google/osv-scanner-action@v1`

There has never been a v1 tag in that repository; its releases start at v2.2.x.
The dependency-scan job could not resolve its action either.

Here the version could NOT be kept, because the version does not exist. The
reference now points at v2.5.1, which is interface-compatible with what the
step already passes -- the nested `osv-scanner-action/` path still exists and
still takes a newline-separated `scan-args`. That is a deliberate two-major
change, recorded in a comment beside the step rather than left to be inferred
from a diff.

### Why neither was noticed

Because these workflows have never run. There is no remote, so GitHub has
never executed them, and `actionlint` validates syntax and expressions rather
than checking that a ref resolves. Three security jobs -- OSV and both Trivy
scans -- were configuration that could not have worked, and the report's claim
that OSV was "configured in CI" was true only in the narrowest sense. That
claim is now corrected in section 33.

The general lesson is the uncomfortable one: a scanner job nobody has watched
succeed is not coverage, it is an intention. The same applies to the rest of
`security.yml` until CI runs once.

### The maintenance cost this creates

A pinned SHA receives nothing. `actions/checkout@v4` silently absorbed every
v4 patch including security fixes; `actions/checkout@11d5960` will absorb none.
`.github/dependabot.yml` exists to close that: weekly, grouped, with a
`cooldown` so a freshly published release is not adopted the hour it appears --
which is the one window where a SHA pin offers no protection, since the updater
would faithfully pin the compromised commit. It has never been exercised
either, and says so.

---

## 30g. Defects the restart and determinism milestone found

Thirteen, of which five would have made a suite report a pass while proving
nothing. Those are the worst kind, and they are listed first.

Findings 6 and 7 were recorded here as NOT fixed, because each changes the
trading path and needed asking for in those words. Both have since been asked
for and both are fixed; their entries are rewritten in place rather than moved,
so the measurement that found them stays next to the measurement that closed
them. Finding 12 is what fixing 6 revealed, and it is open.

**1. Four speed modes agreed on four empty accounts.** The research service was
not running. Every strategy evaluation failed with "research service
unavailable", the circuit breaker opened, the replay stepped happily to the end
of the dataset, and the account finished exactly as it started. The digests
matched perfectly and the suite reported speed invariance. Two things were
wrong: there was no precondition on the research plane, and the vacuity guard
read the account's TOTAL decision count rather than what the run added -- so the
restored snapshot's own rows satisfied it. Both fixed: `/health/ready` must
report `quant: ok` before any scenario runs, and every vacuity guard now
measures against a baseline taken from the restored snapshot.

**2. The determinism suite skipped the preflight the scenario matrix makes.**
It started runs through `control` directly rather than through the harness's
`start`, so it never saw the warning that says a run cannot trade. On a freshly
seeded database -- where Autopilot is correctly OFF by default -- this reproduced
defect 1 exactly, with the research service perfectly healthy. The check is now
extracted and every path that starts a run calls it.

**3. Every end-to-end scenario had an evaluation window of zero.** The warm-up
model introduced by the isolation milestone suppresses executable intent for
the first 60 instants. Eight of the nine market scenarios stepped exactly 60.
They were asserting on a phase that cannot trade, and passing. Found by adding
the phase counters, which is the entire argument for having them. Every
scenario now steps its dataset to the end.

**4. The mock venue's jitter was seeded once, at construction.** Three runs of
one dataset from a byte-identical snapshot agreed exactly on the DECISIONS
section of the result digest and disagreed on orders, fills, positions and
attribution. A second run in the same process continued the random sequence
instead of repeating it. This is the hardest shape of non-determinism to find,
because everything upstream agrees and only the money differs. `Broker.Reseed`
is now called when a replay starts, alongside the regime trackers and the
correlation matrix it belongs with.

**5. A partial fill was not representable at all.** Every order the R500
account produces is 0.01 lots, which on XAUUSD.m is simultaneously the minimum
quantity AND the quantity step, so 40% of one order is 0.004 lots and the venue
correctly declined to split it. The path that books a split execution -- order
state machine, position, weighted average price, fee accrual, ledger -- was
therefore untested. Fixed with a development-only synthetic instrument whose
finer step makes 0.10 split into 0.04 and 0.06, inside the authority's existing
0.10-lot ceiling. Nothing under test was relaxed.

**6. The consensus policy had no production caller, and the platform
demonstrably traded both sides of one instrument at one instant.** FIXED.

`orchestrator.Decide` was implemented, versioned, documented as the
multi-strategy aggregation and unit-tested by ten decision-layer scenarios, and
nothing called it. The scheduler called `EvaluateAndRoute` once per (strategy,
instrument) and each call routed its own signal into the OMS independently.

This was not only structural. **Measured on one run of the condition matrix:**
the strategy set produced opposing actions on the same bar at 38 instants per
dataset, and on two of those datasets the platform FILLED both a buy and a sell
on XAUUSD.m at the same bar -- 19 instants on one, 31 on the other. Sample:
`2027-07-27 08:00`, `buy:FILLED, sell:FILLED`. On the fixture built to provoke
disagreement it was 38 of 38.

In that fixture it happened to net +7.34 ZAR across 32 ledger entries, because
commission on the seeded instrument is zero and the trend was rising. That is
luck, not design: on any venue charging commission, opening and closing the
same instrument at one instant is a guaranteed cost, and it is precisely what
`consensus.go`'s own comment says must not happen -- "netting opposing signals
into whichever side has more weight is how a system ends up trading its own
indecision". It did not even net; it took both sides.

**The fix.** The scheduler's loop is inverted to instrument-then-strategy and
calls `orchestrator.EvaluateInstrument`, which evaluates every applicable
strategy with `Execute:false`, aggregates them with `Decide`, and places at
most ONE order. **Re-measured on a freshly seeded stack in replay mode: the
same 38 instants split the set, and 0 produced orders on both sides.**

Four properties were designed in rather than discovered afterwards:

- Aggregation never increases a size. The verdict chooses a direction; sizing
  is still against the account's own budget, and a cap can only lower it.
- Aggregation never traps a position. A no-trade verdict still lets through a
  COUNTED opinion that opposes an open position, capped at that position's size
  so it is strictly reducing. Without this, the change meant to stop the
  account hedging itself would have stopped it UNWINDING -- the same inversion
  rule 8 has already had to be applied to three separate checks. A veto is no
  exception: a release inside the blackout window is an argument for being able
  to close.
- It abstains rather than deciding over a partial set. If one strategy's
  opinion for a bar was recorded elsewhere, the missing opinion may be the one
  that would have produced the disagreement.
- It down-weights a crowded family. Two trend strategies are one read of the
  market counted twice.

The verdict is recorded on the decision snapshot -- a new `consensus` column,
migration 0018 -- with the action, confidence, policy version, vetoes, rationale
and every contribution INCLUDING the discarded ones. A no-trade verdict now
writes its own snapshot; the table has always permitted `outcome='no_trade'`
with null strategy columns, because a verdict is not attributable to one
strategy. `internal/arch` pins that the scheduler cannot go back to routing a
single signal.

**What the fix revealed, and what is NOT fixed.** See finding 12.

**7. Four of the trading authority's five numeric ceilings did not bind.**
FIXED.

A trading authority is described throughout this repository as a technical
control, and it carries `MaxOrderQuantity`, `MaxOrderNotional`,
`MaxPositionExposure`, `MaxLeverage` and `MaxDailyLoss`. Only `MaxLeverage` was
compared against anything -- `risk.Evaluate` takes `decimal.Min` of it and the
account's limit. The other four were stored, returned by the API, and written
into every decision snapshot's `authority_state`, and were never read by the
risk engine, the OMS or the orchestrator. An operator who narrowed
`MaxOrderQuantity` to 0.10 saw it accepted, saw it echoed back, saw it recorded
on every decision, and was not protected by it. That is worse than not having
the field, because it creates confidence that nothing supports.

**The fix.** Each now reports its OWN named risk check --
`authority_max_order_quantity`, `authority_max_order_notional`,
`authority_max_position_exposure`, `authority_max_daily_loss` -- rather than
being folded into the account's with a `min()`. Two checks that must both pass
are arithmetically the same bound and they say which one was hit, which a fold
cannot: "Order quantity 0.50 against a limit of 0.10" does not tell an operator
whether to widen the account limit or the authority, and those have different
fixes and different audit trails.

All four carry the reducing exemption. For notional, position exposure and
daily loss that mirrors the account-level check beside them. For order quantity
it does NOT -- **the account's own `max_order_quantity` check has no reducing
exemption** -- and the difference is deliberate: a per-order size cap limits
exposure, so rule 8 applies, and without it narrowing the authority would make
a position opened while it was wider impossible to close in one order. That is
the flatten trap the notional cap, the event blackout and the daily-loss limit
each had to be rescued from, and adding a fifth place for it to appear is not
an acceptable price for enforcement.

An unset ceiling REFUSES. It cannot reach the engine from the database -- every
column is `NOT NULL` with a `> 0` CHECK and the create handler rejects a
non-positive value -- so a zero means the authority was not loaded, and "no
bound" is the most permissive reading available.

The decision snapshot now records all five rather than one, because a refusal
has to be reconstructible from the snapshot alone: the authority is versioned
and may have been narrowed since the decision was taken.

`TestTheAuthoritysNumericCeilingsAreDocumentedAsEnforcedOrNot` was UPDATED, not
deleted. Its purpose was never to describe the gap -- it is there so that adding
a sixth ceiling without enforcing it is a deliberate act with a failing test in
front of it, and a record thrown away once it reads "all enforced" protects
nothing.

**Measured on the dev fixture: no change in outcome, as predicted.** The seeded
authority is `MaxOrderQuantity` 0.10, `MaxOrderNotional` 2500 ZAR,
`MaxPositionExposure` 2500 ZAR, `MaxDailyLoss` 15 ZAR, and the seeded
strategies place 0.01-lot orders. 23 Go packages pass, `-race` clean.

**Left open, recorded rather than fixed in passing:** the ACCOUNT-level
`max_order_quantity` check still has no reducing exemption. It is a
pre-existing gap on a limit that is looser than the authority's here, so it is
not reachable through this change, but it is a fourth place rule 8 would apply
and closing it changes a bound that was not in scope.

**8. The seed refused to complete, correctly.** Adding the synthetic instrument
broke `control-api seed`: it generates historical bars for every enabled
instrument and had no starting price for the new one. It refused rather than
skipping, which is right -- an instrument in the tradable universe with no
history is a trap -- and it caught the omission on the first run.

**9. A decision never recorded which bar it was taken on.**
`decision_snapshots.bar_time` has existed since the schema was written and
nothing ever set it: 168 snapshots, none with a value, while all 1488 strategy
signals carried one. So a decision could not be joined to the signal that
produced it, "which bar did this trade come from" was unanswerable, and the
result digest coalesced every decision's bar to `'none'` -- two decisions
differing only in their bar were indistinguishable in it. Found by scenario G,
which returned nothing because grouping by `bar_time` put every row in one NULL
bucket. Fixed: the OMS request carries the bar, nil for a manual order.

**10. A partial risk-limits update silently disabled three protections.**
`require_stop_loss`, `block_on_high_impact_events` and both blackout windows
were plain bools and ints assigned unconditionally from the decoded request,
while every other field falls back to the stored value. A PUT that meant to
tighten one exposure ceiling and said nothing else therefore set
`require_stop_loss` to false, `block_on_high_impact_events` to false and both
blackout minutes to zero -- which is what a zero-valued bool and a zero int
decode to. That is the opposite of what a risk endpoint should do with silence.
Fixed: they are pointers, so omitting one leaves it alone and turning a
protection off has to be said out loud. Nothing in the terminal sends this
request; it only reads the fields.

**11. Two of this milestone's own scenarios asserted the wrong thing, and a
third tested nothing.** Recorded because they are the same class of error as
the first three, and because a test that passes for the wrong reason is the
defect this whole exercise is built to catch:

- Scenarios D and I each treated any accepted order under their condition as a
  failure. A REDUCING order is exempt from both the event blackout and the
  exposure ceilings by design, and must be: refusing a flatten during a
  high-impact release, or because the ceiling is tight, traps the position in
  exactly the conditions the control exists to avoid. Both now assert what the
  controls actually guarantee -- for D, that no decision whose `event_risk`
  check FAILED was accepted and that nothing accepted had zero open positions
  to reduce; for I, that every accepted order opposed the net exposure it saw,
  so exposure never grew.
- Scenario G's fixture was designed to overextend on every impulse, on the
  theory that this would divide a trend reading from a mean-reversion one. It
  split the seeded strategy set **zero** times in 180 instants, while the plain
  trend used by D, I and N split it on 38 instants each. The disagreement comes
  from strategies flipping at different points in one move, not from price
  stretching away from its mean. The fixture that provokes it is the one that
  looks least provocative.

Two further findings are properties rather than defects, recorded because both
cost time to diagnose:

- **One seeded database serves one replay SUITE, not one invocation.** Running
  the recovery scenarios and then the market matrix in a single `go test` left
  the matrix measuring an exhausted account: 285 strategy runs, all skipped, no
  decision, and scenario B reporting "every decision recorded the same regime
  (map[])" -- which is what an empty set looks like rather than a broken
  classifier. The obvious suspect is wrong: the per-bar watermark is not the
  cause, because a replay's `start` already purges `strategy_runs` for the
  window it is about to play. The shared account's 15 ZAR daily-loss limit and
  its exposure ceilings are.

- **The pacing cap makes every finite speed identical on long timeframes.** The
  engine caps the per-instant sleep at two seconds, so on 1h bars 1x, 10x and
  100x all pace identically; only `max` differs. An uncapped 1x on this dataset
  would take 140 hours, so the cap is right -- but the speed NAMES overstate
  what they control.
- **A paced replay cannot be hand-stepped through the API.** The pacing sleep
  happens inside the step request and the router gives every handler 30
  seconds, so any batch large enough to be useful blows the deadline and
  surfaces as `context deadline exceeded` from whichever query was in flight --
  a database error for what is arithmetic. Paced runs use the background
  `advance` control, which is what an operator would use anyway.

**12. Nothing autonomous trades under the default consensus policy.** NOT
fixed, deliberately, and it is the most important thing this milestone found.

Wiring `Decide` in (finding 6) applied two gates for the first time: a 0.55
confidence floor, and the strategy's own declared regime validity.
`StrategyVersion.ValidRegimes` had exactly the same shape as the authority
ceilings -- stored, served, and read by nothing. Verified by grep: before this
change the only non-plumbing reader of `ValidRegimes` did not exist.

**Measured on a freshly seeded stack, across every fixture played:**

| strategy | actionable signals | confidence | outcome |
| --- | --- | --- | --- |
| `rsi_mean_reversion` | sells | 0.66-0.73 | discarded: declares RANGING/LOW_VOLATILITY, regime is TRENDING |
| `macd_momentum` | buys | 0.51-0.54 | discarded: below the 0.55 floor |
| `donchian_breakout` | buys | 0.31-0.44 | discarded: below the 0.55 floor |
| `ma_trend_crossover` | none | -- | produced no actionable signal |
| `bollinger_zscore_reversion` | none | -- | produced no actionable signal |

Every verdict is therefore "no strategy offered an actionable opinion that
survived the policy". On `trend-clean`, which placed 33 orders before the
wiring, the result is 77 no-trade verdicts and **0 orders**.

The thresholds are NOT tuned to fix this, and that is a decision rather than an
omission. `DefaultConsensusPolicy`'s own comment says they are "a starting
posture for a platform that has never traded unattended, chosen so that the
common outcome is NO TRADE", and that loosening them "should be made against
paper-forward evidence, not to make a demo trade". Loosening them so a fixture
trades is precisely what it forbids.

The open question is whether the research plane's confidence is calibrated to
the scale the policy was written against, or whether the seeded strategies are
genuinely this unconvinced. That is a research question and needs evidence, not
a constant.

**Consequence for the scenarios, stated plainly:** scenario G PASSES -- 38
instants split the strategy set, 0 produced orders on both sides -- but it
passes with ZERO orders, which is weaker evidence than passing with orders, and
the test logs that in those words. D and I now SKIP: each measures how an order
was refused, and no order is placed. Three scenario guards had to be updated
because they read evidence the fix deliberately stops producing; none of the
invariants changed.

**13. The determinism suite's vacuity guard stopped guarding, because of a
change somewhere else.** FIXED, and it is the same defect as finding 1 arriving
by a different route.

The guard required `decisions_produced > 0`, and that used to imply the
financial path had run: a decision snapshot was written only by the OMS, so a
decision meant an order was attempted. Multi-strategy aggregation (finding 6)
writes a no-trade snapshot BEFORE the OMS is reached, so the guard became
satisfiable by a run that moved no money at all -- and five of the digest's
seven sections then agree because all five are empty.

**Measured before the fix:** three datasets, three runs each, all nine digests
identical, suite PASSES -- over zero orders. Exactly the shape of finding 1.

Fixed by measuring the thing the proxy stood in for. `orders_produced > 0` is
now required separately, and its absence is a SKIP naming what was and was not
shown. **After the fix:** `trend-clean` 77 decisions / 0 orders,
`range-bound` 77 / 0, `correlated-pair` 58 / 0 -- all three SKIP with
"evidence of decision determinism and not of financial determinism". The
digests still agree three times over, which IS decision determinism and is
logged as such.

The speed suite carried the same proxy and now carries the same skip.

The general lesson, stated once: a vacuity guard must measure the thing it
stands in for, never a proxy that merely happened to imply it. A proxy holds
until someone changes the code that made it true, and nothing then fails.

### The market matrix, isolated on its own seed

Nine scenarios, each stepping its dataset to the end:

| | Result |
| --- | --- |
**Re-run after the corrections: all nine PASS.** The table below records what
each one was before, because the corrections are the finding.

| A clean trend | PASS |
| B range | was FAIL -- corrected. It asserted "decisions > 0", which on a range asserts the opposite of the point: the registry describes this dataset as the market "on which a trend strategy should not churn". It now asserts the strategies were REACHED |
| C volatility shock | was FAIL -- corrected, same reason. A shock suppressing every actionable signal is a defensible response, not a defect |
| E spread spike | PASS |
| H drawdown | PASS |
| K trend reversal | PASS |
| L false breakout | PASS |
| M correlated opportunities | was FAIL -- no decision recorded the `portfolio_correlation` check. Fixture lengthened to 180 instants: the correlation matrix needs paired history of its own |
| T day and session boundary | was FAIL -- no decision at all. At 90 instants, 30 remain past the warm-up, which is not enough for a strategy to act under the data floor. Fixture lengthened to 180 |

**All four failures trace to one cause, and it is the price of the isolation
milestone's data floor.** A strategy now sees only the replay's own bars; before
the floor it could see the seeded 8730-bar history and signalled from the first
instant. Every dataset must now build that history itself, and the seeded
strategy set produces nothing actionable until roughly 120 bars exist. That is
correct behaviour and a real constraint on fixture design, and it was invisible
until the scenarios were stepped past the warm-up.

### Restart safety, measured

A real `Stop-Process` and a real restart, not an in-process reset. Four cases:

| Case | Result |
| --- | --- |
| Restart mid-run (scenario S) | PASS. 33 decisions, 33 orders, 28 fills, 15 ledger entries and the balance all unchanged. The run is `interrupted`, the clock is disengaged, `active` and `engaged` are both false, and no (strategy, bar) pair was evaluated twice. A run started afterwards reports `phase=warmup` with 3 of 3 instants in warm-up and 0 in evaluation, so the engine inherited no window or cursor from the process that died |
| Unknown outcome (`timeout`) | PASS. The order was FAILED with 0 fills before and REJECTED with 0 fills after. No fill was invented and it did not become FILLED -- the platform did not resolve an uncertainty it had no evidence for, in either direction |
| Lost execution (`lost_response`) | PASS. The venue held 13 executions against 12 local fills; after the restart exactly **1** was imported, none twice, no reconciliation issue left open, and the balance reconciled at 497.95 against the ledger-derived total |
| Interrupted runs are discoverable | PASS. Three abandoned runs listed with a resume verdict each, naming the cursor, the total, and the fact that the cursor is persisted at most every two seconds so a resume may re-play a few instants the per-bar guard will produce nothing for |

### No test in this repository fails on purpose any more

`TestConditionScenariosThroughTheRealPipeline/G_conflicting_strategy_signals`
was left red for a whole milestone, and the failure was the finding: the
platform ended single instants holding non-rejected orders on both sides of one
instrument, because `orchestrator.Decide` had no production caller. It was kept
red rather than skipped, softened or deleted -- a skip would have reported "not
exercised" for something that was exercised and failed, and a softer assertion
would have reported a pass for a platform trading its own indecision.

It PASSES now: 38 instants split the strategy set and 0 produced orders on both
sides. Read the qualification in finding 12 before treating that as strong
evidence -- it passes with zero orders placed, and the test says so in its own
log rather than leaving a reader to assume otherwise.

Anyone running the replay suite should now expect no failures. Several subtests
SKIP -- D and I in the condition matrix, and all three classes in the
determinism suite -- and every one of them says in its own message what was
shown and what was not.

## 30h. Defects the authorisation milestone found

Six. The milestone itself was authorising two known defects -- the unwired
consensus policy and the four unenforced authority ceilings -- so these are
what closing them turned up.

**1. Every authority refusal reported under an account-level code.** FIXED.
The ceilings bound, but a breach came back as `risk_limit_breached`,
`exposure_limit_breached` or `daily_loss_limit_reached` -- the account's codes.
An operator told "exposure_limit_breached" cannot tell whether to widen the
account's limit or the authority, and those are different controls with
different owners, different change procedures and different audit trails.
Research reading the codes could not separate them either. Five codes now, one
per ceiling, and no generic authority refusal: adding a sixth ceiling means
adding a sixth code. `MaxLeverage` was split out of its `decimal.Min` fold for
the same reason; the instrument's own `Spec.MaxLeverage` stays folded with the
ACCOUNT's, because a venue property and an operator setting belong together
and neither is a statement about permitted scope.

**2. The one-decision-per-bar invariant was not durable.** FIXED, and this is
the one that mattered. `command_idempotency` is keyed on
`(account_id, idempotency_key)`, which is the right guard -- an in-memory flag
would not survive the restart this platform is built to survive. But the
consensus key was `strat-<leader>-v<version>-<instrument>-<bar>`, and the
leader is whichever counted opinion was strongest on the routed side. That can
differ between two evaluations of one bar: a confidence that moved, an opinion
the policy discarded the second time, or the reducing rescue attributing to a
different strategy entirely. Each would mint a different key and let a SECOND
orchestrated order through for a bar that already had one. The key is now
`consensus-<instrument>-<bar>` and names no strategy.

**3. Three market-matrix scenarios asserted against evidence the fix stops
producing.** FIXED -- two at source, one as a skip.

T counted distinct sessions across ORDER-BEARING decisions. Session
attribution is decision-time information: reading only traded instants
describes the instants that traded, not the day. The no-trade snapshot now
carries `market_data_health` in the OMS's own shape and T reads the run.

B asserted only that the regime classifier was "not PINNED to one label". That
weakening was recorded honestly at the time -- the sample was twelve
order-bearing decisions, all at instants where a trend strategy fired, all
TRENDING on a range-bound dataset. The sample is now every evaluated instant
and the classifier reports RANGING on 77 of 77, so the assertion is
STRENGTHENED: the dominant label on a range-bound dataset must be RANGING.
"Not pinned" would now fail on a correct classifier.

M needs `portfolio_correlation`, which lives in `risk_state` and is written by
the OMS alone. With no order it did not run, which is a fact about the
consensus rather than about correlation, so it skips saying that.

**4. Reseeding without restarting the control plane fakes non-determinism.**
DOCUMENTED, not a code defect. `dev-up.ps1 -Reset` drops the database volume,
and a control plane left running keeps a pool to a database that no longer
exists plus in-process state the reset cannot reach: the quant circuit breaker,
the regime trackers, the correlation matrix and the mock venue's RNG.
`Replay.SetOnStart` resets the last three at the start of every run, which is
why this mostly looks fine and then does not.

Measured: a determinism suite on a stale process gave `range-bound` a real
digest on run 1 and the EMPTY-account digest on runs 2 and 3, while
`trend-clean` and `correlated-pair` produced nothing at all. On a freshly
started process: three identical digests per class, and run 1 of `range-bound`
produced the SAME digest as before -- so the code was deterministic throughout
and the suite was measuring a broken process. In ENGINEERING_GUIDE.md now.

**5. A Playwright test hard-coded the API port.** FIXED. `auth.spec.ts` fetched
`http://localhost:8080` inside `page.evaluate`, so on a machine where the
control plane runs anywhere else the test failed with "TypeError: Failed to
fetch" -- which reads as a broken authorisation check and is a broken URL. The
base is passed in now. Every other spec already read
`VANTAGE_E2E_API_BASE_URL`.

**6. The brief asked for replay scenarios J and Q, which do not exist.** The
letters in this repository are A B C D E G H I K L M N T. Reported rather than
invented; A, D, G, I, M and N were run.

## 30i. Replay isolation, the scenario matrix, and the score that is not a probability

This milestone was asked for nine priorities in order, with the instruction not
to jump ahead while a more fundamental invariant is still broken. Priorities 1
to 3 were reached. One fundamental invariant turned out to be broken, is
diagnosed below, and is the reason the rest was not started.

### Replay isolation: FIXED and PROVED

The standing finding was that "the regime is classified over a window that
mixes seeded history with dataset bars". The control existed --
`ReplayWindow.Floor()` bounds every historical read at the run's declared
`warmup_start`, and the store expresses it as `open_time >= $4` -- and nothing
demonstrated that it bound.

The test plants 400 bars of the OPPOSITE market shape immediately before the
floor, under their own provider and AFTER the run has started, so
`PurgeReplayMarketData` cannot remove them. That is the seeded-history case
exactly: data the replay does not own and cannot delete. 400 is more than the
300-bar window a strategy reads, so a floor that leaked at all would fill the
window entirely with contamination.

| planted before the floor | dataset | classified |
| --- | --- | --- |
| 400 trending bars | `range-bound` | 77 RANGING, **0** TRENDING |
| 400 ranging bars | `trend-clean` | 77 TRENDING, **0** RANGING |

With 235 short-window skips per run as an independent second witness: early in
a run the window holds only the few replay bars produced so far, so strategies
skip for insufficient history and say how many bars they could see. Had the
planted bars leaked, the window would have been full from the first instant.
That witness is a statement about what the QUERY returned, so it cannot be
satisfied by a classifier that happens to be right.

The warm-up / evaluation model the brief asks for already existed:
`ReplayWindow{WarmupStart, EvaluationStart, EvaluationEnd, AllowWarmupTrading}`,
with warm-up suppressing executable intents unless explicitly configured, and
`replay_runs` recording dataset id and hash, code SHA, config hash, seed, all
three window timestamps, starting balance and currency, risk/authority config
hashes, correlation and regime policy, and strategy versions. A test now
asserts that record is complete.

One gap is recorded rather than tested around: `starting_positions` is
`NOT NULL DEFAULT 0`, so a run whose input gathering FAILED is
indistinguishable from a run on a flat account. The gatherer is deliberately
non-fatal, so the honest fix is a separate "inputs gathered" flag.

### The scenario matrix, A-T

Every letter exists and is driven through the real pipeline. The brief listed
D F G I J N O P Q R S as "not yet driven through the complete application
pipeline"; that list was out of date -- all of them were built in earlier
milestones, and this milestone ran them.

| | scenario | status | result |
| --- | --- | --- | --- |
| A | clean trend | FULL_E2E | PASS |
| B | range | FULL_E2E | PASS -- RANGING on 77 of 77 |
| C | volatility shock | FULL_E2E | PASS |
| D | high-impact event | FULL_E2E | SKIP: no order-bearing decision. 58 event-risk vetoes recorded through the consensus path |
| E | spread spike | FULL_E2E | PASS |
| F | market-data outage | FULL_E2E | PASS -- 480 runs before, 95 during with 0 orders, 95 after recovery |
| G | conflicting strategies | FULL_E2E | PASS -- 38 splits, 0 both-sided |
| H | drawdown sequence | FULL_E2E | PASS |
| I | no risk capacity | FULL_E2E | SKIP: no order reached the risk engine |
| J | kill switch | FULL_E2E | PASS, PARTIAL: 0 orders existed before activation, so "orders before activation behave normally" is undemonstrated |
| K | trend reversal | FULL_E2E | PASS |
| L | false breakout | FULL_E2E | PASS |
| M | correlated opportunities | FULL_E2E | SKIP: `portfolio_correlation` is written by the OMS alone and no order reached it |
| N | news + agreement | FULL_E2E | PASS |
| O | stale provider recovery | FULL_E2E | PASS -- 0 orders, 0 stale refusals through the stale phase |
| P | partial fill | FULL_E2E (manual) | PASS via a manual order; the AUTONOMOUS path skips for want of an order |
| Q | lost response | FULL_E2E (manual) | PASS via a manual order; the AUTONOMOUS path skips for want of an order |
| R | duplicate tick | FULL_E2E | PASS |
| S | application restart | FULL_E2E | PASS (measured in the restart milestone; not re-run here) |
| T | day and session boundary | FULL_E2E | PASS |

Five of the twenty skip, and every one of them skips for the SAME reason.

### The fundamental invariant that is broken: a raw score is not a probability

`_confidence` in `services/quant` averages hand-chosen 0-1 components, and its
own docstring says the result "is a ranking input, never a probability of
profit". Three strategies average a REAL component with a hard-coded constant:
`donchian_breakout` with 0.5, `rsi_mean_reversion` with 0.45, and one more with
0.6. The value is bounded in [0, 1], is not comparable BETWEEN strategies, and
has never been fitted against realised outcomes.

It was consumed as though it were a probability. `DefaultConsensusPolicy`
discards an opinion below 0.55 and requires 0.60 net -- thresholds that read as
confidence levels -- applied uniformly to those scores. Half of
`donchian_breakout`'s score is a constant, so its whole range is [0.250, 0.750]
and a strong break is compressed towards a mediocre one.

**Correction.** An earlier revision of this section said it "reports 0.31-0.44
whatever the market does and can never clear the floor". That was wrong. The
blend is a mean, so the score is `(min(1, penetration) + 0.5) / 2`, which
crosses 0.55 at a 0.6 ATR penetration. Measured in
`services/quant/tests/test_confidence_reachability.py`: a 0.74x ATR break
scores 0.618 and a 0.97x break scores 0.733. The 0.31-0.44 was what the seeded
data produced, not a ceiling. The real cost of the constant is the top of the
range -- nothing above 0.75 is reachable -- which is a calibration defect and
not an impossibility. `docs/SIGNAL_RESEARCH.md` had this right ("its range is
[0.250, 0.750]"); this section overstated it.

That is a category error, and it was invisible. It is now named: signals carry
`confidence_kind` (`raw_score` or `calibrated_probability`) end to end, and
every consensus verdict records the kind it was compared against -- per
contribution, and as one label for the decision including "mixed" for the
moment one model is calibrated and the rest are not. An empty kind reads as
raw, because assuming calibration from silence assumes the stronger claim.

**The thresholds were NOT adjusted.** Moving a number until trades appear is
how a platform talks itself into a result. The fix is calibration against
realised outcomes, which is research, needs evidence, and is the next
milestone's work rather than this one's.

### What this blocked

Priorities 4 to 9 -- determinism re-proof after the isolation changes, speed
invariance, walk-forward persistence/API/UI, parameter and cost sensitivity,
Monte Carlo, backtest-versus-PAPER_FORWARD comparison, strategy and model
promotion gates, the remaining ML metrics, baselines, the explainability view,
NO TRADE analytics, attribution reconciliation, and correlation/RISK_OFF replay
validation -- were NOT started.

That is not a scheduling accident. Walk-forward results, a backtest-versus-
forward comparison and an evidence-based promotion gate all need a platform
that produces trades, and the only autonomous trades available today would come
from moving a threshold against an uncalibrated score. Building those
measurements on that foundation would produce numbers that look like evidence
and are not.

## 30j. Signal coverage, statistical power, and two defects in the measurement itself

The previous milestone could not answer its own question: 460 observations, no
strategy past 100, four strategies never reaching their own warm-up because the
TRAIN fixtures were shorter than their `required_bars`. This milestone gave the
pipeline enough data and then measured how much of that data was actually
evidence.

No strategy formula, threshold, consensus policy or horizon was changed. More
data, not looser rules.

### The apparatus

`research/datasets.py` generates sixteen market conditions from a seed, laid end
to end and non-overlapping, at a declared `GENERATOR_VERSION`. The bars are NOT
committed -- rule 15 -- so determinism is what makes that safe, and the dataset
hash covers the generator identity as well as the bars. `research/expansion.py`
holds the statistics; `research/historical.py` is the allowlisted seam for real
bars, which reports `REAL_MARKET_VALIDATION_PENDING` because there are none and
this milestone did not go looking for any. `python -m vantage_quant.research
expanded` reproduces the run from committed code rather than a scratch script.

### The measurement

19 200 bars, 9 600 TRAIN, twelve strategies, 15 minutes on 12 workers.
Full results in `docs/SIGNAL_RESEARCH.md`.

**The working hypothesis was wrong in a useful direction.** Signal rates were
expected near 2%; measured they span 1.22% to 71.76%. Five strategies produce
over a thousand observations each. `macd_momentum` reaches 750 *effective*
observations -- comfortably a calibration candidate on count alone -- and still
fails, which is a more useful answer than "collect more".

**Nothing is `READY_FOR_CALIBRATION`.** No calibration was fitted.

### Two defects, both mine, both inflating confidence

The first run reported `session_london_breakout` as `READY_FOR_CALIBRATION`.
Both of the following were found by interrogating that single positive result
rather than by a test failing.

1. **`readiness` blocked only on the literal `NON_MONOTONIC`.** The session
   strategy's score concentrates into two usable quantile bins out of five, so
   `classify_monotonicity` returned `INSUFFICIENT_EVIDENCE` -- and that sailed
   through the gate as though monotonicity had been demonstrated. A monotone
   calibration map requires a monotone ordering to exist; "could not establish
   one" is not that. The gate now requires an affirmative verdict.

2. **Evidence was graded on effective N while every interval was built on raw
   N.** `EpisodeSummary`'s own docstring warns that counting clustered signals
   as independent shrinks intervals "by roughly the square root of a lie", and
   the bootstrap was doing exactly that. Every interval now resamples
   **episodes**. The correction widened rank-correlation intervals by up to
   2.37× (`rsi_mean_reversion`: 1 096 signals, 110 episodes, largest episode 82)
   and by exactly 1.00× for `ma_trend_crossover`, whose 113 signals really are
   113 independent events.

After both fixes, the one ready verdict disappeared. That is the correct
outcome: it was an artefact of the measurement, not a property of the strategy.

### What actually blocks calibration

- `macd_momentum` is the strongest **negative** result. Adequate power, score
  `FLAT` at all three horizons, mean net return indistinguishable from zero.
- `session_london_breakout` has a rank correlation surviving clustering and a
  positive net outcome, but needs score *spread*, not more bars.
- `bollinger_zscore_reversion` orders outcomes and still loses to costs.
- `rsi_mean_reversion` orders them backwards at every horizon.
- `grid_martingale_research` emitted 6 539 signals of a constant score.
- `ml_direction_filter` and `ensemble_weighted_vote` emitted nothing at all.
  The former **fails closed** with no model deployed, which is the design
  working rather than a shortage.

### Strategy edge is not score quality

`donchian_breakout`'s mean net return is positive with an interval excluding
zero at all three horizons, while its rank-correlation interval includes zero at
all three. Its signals are profitable on this synthetic data and its score does
not rank them. Calibrating that score would add nothing.

### Every number above is synthetic

`SYNTHETIC_CONTROLLED`, and the generator and the strategies share a model of
what a trend is, so a trend strategy scoring well on a generated trend has
partly measured that agreement. None of this is evidence of predictive value,
calibration or edge, and none of it may be quoted as if it were.
`REAL_MARKET_VALIDATION_PENDING` stands.

## 30k. Real historical data: the apparatus, and why it has nothing to chew on

The synthetic experiment answered a narrower question than it appeared to. A
trend strategy scoring well on a generated trend has demonstrated that it
agrees with the generator about what a trend is. Only real bars separate that
agreement from an edge, so this milestone built the path for real bars and
asked what the local machine actually has.

**It has none.** `research-data/` is empty, `Downloads`, `Desktop` and
`Documents` contain no market file, and the only CSVs in the repository are the
fourteen generated replay fixtures. Nothing was downloaded, scraped or fetched:
ENGINEERING_GUIDE.md rule 3 forbids it and the brief forbade it twice. The status is
`WAITING_FOR_HISTORICAL_DATA`, and the specification of what would unblock the
next run is printed by `python -m vantage_quant.research datasets`.

### The evidence taxonomy was fixed first, deliberately

Milestone E shipped a verdict called `MORE_DATA_REQUIRED` that meant two
incompatible things: too little evidence to say anything, and enough evidence
to say there is no usable relationship. Those call for opposite actions.
`donchian_breakout` carried that label with 409 independent episodes behind it,
`atr_volatility_regime` with 20.

Importing real market results into an ambiguous category would have baked the
ambiguity into the one comparison that matters most, so the split came first.
Ten verdicts now name one cause each, and two frozen sets -- `NEEDS_MORE_DATA`
and `SCORE_IS_THE_PROBLEM` -- make the distinction machine-readable rather than
a matter of reading prose. `ORDERING_NOT_ESTABLISHED` is the new one that
carries the weight: measured, adequate sample, and still not resolvable from
zero, which points at the score's distribution rather than its quantity.

A separate flag records whether the SCORE or the SIGNAL RULE is the thing to
revisit, because a bad score is not a bad trading hypothesis and Milestone E
found exactly that split in `donchian_breakout`.

Prior runs are NOT rewritten. `verdict.map_legacy` translates old labels for
display, and where an old label is genuinely ambiguous and no counts accompany
it, it returns nothing and says why -- a migration that guesses is
indistinguishable from one that knows.

### What was built

- **`quality.py`** -- a data-quality report rather than a boolean. Duplicate
  timestamps, out-of-order rows, non-finite or non-positive prices, impossible
  OHLC and crossed quotes are ERRORS that reject a dataset. Assumed timezone,
  absent spread, irregular spacing, large gaps, abnormal ranges and spreads,
  suspected flatlining, Saturday bars and low coverage are WARNINGS that travel
  with it. Nothing is repaired: a validator that silently fixes its input
  destroys the evidence that the input was broken.
- **A market clock** modelled on `fx_metals_24x5`, so a closed venue is not
  counted as missing data. Counting the weekend as missing makes a complete
  dataset look broken and hides the one that is.
- **`historical.py`** -- RAW, NORMALIZED and RESEARCH as distinct layers, so
  "was that in the file, or did we do it?" stays answerable. Both SHA-256
  hashes travel into every record.
- **`historical_run.py`** -- imports, partitions chronologically BEFORE any
  outcome is examined, seals TEST, and evaluates TRAIN through the SAME
  `observe_frame` and `build_report` the synthetic run uses.
- **`compare.py`** -- synthetic against historical, never averaged, with
  disagreements classified. It refuses two runs whose analysis versions differ.
- **`requirements.py`** -- the exact specification of the missing data, derived
  from the longest registered warm-up plus the episode floor rather than picked.

### One analysis, two data sources

There is no `HistoricalResearchV2`. Both paths call the same observer and the
same report builder, so a difference between the two results comes from the
market rather than from two implementations that drifted. The methodology is
versioned as a whole (`signal-research-analysis/v1`) carrying horizon registry,
cost model, clustering rule, bootstrap, classification, generator, partition and
normalization versions, and a comparison across mismatched versions is refused
rather than produced with a caveat.

### Costs are an assumption unless the source says otherwise

A dataset without bid/ask gets `cost_basis = ESTIMATED_COSTS`, and that label
belongs on every result derived from it. Bid/ask are never synthesised, and a
`spread` column of zeroes does not count as observed -- a backtest reporting
costs it never paid is worse than one that admits it is estimating.

### Timezone is never inferred from this machine

Gold and FX histories are commonly exported in broker server time, and a silent
offset moves every session boundary in the analysis. A declared timezone is
converted and recorded; an undeclared one is taken as UTC, recorded as
`UNDECLARED`, and raises a `TIMEZONE_ASSUMED` warning. Daylight saving is handled
by converting through the named zone, tested across the March 2024 transition in
both directions.

### The file is untrusted input

Bounded in size before it is opened. Addressed by NAME inside an allowlisted
directory -- never a path, because an arbitrary path parameter is a file-read
primitive pointed at the host -- with separators, `..`, drive letters and
leading dots refused and the resolved path then checked with `is_relative_to`,
since a string prefix would accept `research-data-elsewhere`. Duplicate columns
are refused before pandas can silently disambiguate them and let one win.
A column name a spreadsheet would execute as a formula is refused. Unparseable
timestamps are an error rather than a `NaT` flowing onward.

### What this milestone did NOT produce

No historical results, because there is no historical data. No calibration was
fitted, no strategy formula changed, no threshold moved, `consensus-policy/v1`
is untouched, and both sealed TEST partitions -- synthetic and historical --
were never read. The synthetic baseline from Milestone E is preserved as its
own run; the re-run under the corrected taxonomy carries a new run id rather
than overwriting it.

## 30l. Vantage owns its market data

The platform could not acquire a price series. It generated one, replayed one
from a fixture, or waited for somebody to drop a CSV in a directory. This
milestone gives it a provider, a store, provenance, a coverage catalog and a
chart that reads its own data.

No API key is configured on this machine, so **no real bars were acquired**.
Status: `TWELVE_DATA_CONFIGURATION_REQUIRED`. Everything else was built and
tested against a mocked provider; see `docs/MARKET_DATA_PLATFORM.md`.

### What exists now

`TwelveDataProvider` behind the existing `marketdata.Provider` seam, so
strategies cannot tell which provider supplied their bars. A symbol-mapping
layer translating `XAUUSD` to `XAU/USD` once, at the boundary. A `Syncer` with
deterministic chunking, incremental sync, gap repair, bounded retry and a fixed
rate window. `market_data_segments` recording what was requested against what
arrived. `research_datasets` giving research an immutable identity to cite.
Read and admin API routes. A Lightweight Charts terminal page with decision
markers and a provenance panel.

### Three defects, all the same defect

The first working version hard-coded `HISTORICAL_MARKET` on every research
snapshot, and cheerfully labelled bars from the `mock` generator as real market
evidence. Two milestones exist to keep generated and observed data apart; one
constant walked past all of it. Source type is now DERIVED from the providers
that supplied the bars, and a range mixing generated with real bars is REFUSED
rather than labelled -- neither answer would be true.

The same defect then reappeared one process boundary away: the Python importer
assumed everything in the research directory was `HISTORICAL_MARKET`, which is
right for a file an operator placed by hand and wrong for a snapshot Vantage
had just exported from generated bars. Each export now carries a manifest
stating what it is, and an unrecognised source type is refused rather than
defaulted.

And the coverage endpoint reported every weekend and every daily maintenance
break as a gap -- precisely what `FindGaps`' own comment warns makes a complete
dataset look broken. The store's query is structural by design; the handler
has a market clock and was not using it. Measured after the fix on development
data: 0 real gaps, 27 closed-market absences.

All three were found by running the thing rather than by a test failing, which
is the argument for exercising a new path against a live stack before believing
it works.

### A fourth, found by a test

The provider requested ascending order and then unconditionally REVERSED the
result -- correct only when the provider ignores the parameter. The first test
passed because its fixture happened to be descending; the second caught it. It
sorts now, which is right either way.

### Secrets

The key is server-side only. It is not in the configuration digest that travels
into stored replay records, not in provider health, not in an error path, and
not in the browser bundle -- a Playwright test watches the network and fails if
the browser requests a provider host at all. Redirects are refused, because
following one would carry the key to wherever the provider pointed.

### What this does NOT do

No strategy formula, threshold or `required_bars` changed. No calibration.
`consensus-policy/v1` untouched. No sealed TEST partition read. PAPER remains
the only executable mode and no broker was contacted. Twelve Data does not feed
PAPER_FORWARD in this milestone: the provider has no live quote at all, and
returns an error rather than a synthesised price, because a provider that
invents a quote to satisfy an interface is how a strategy ends up trading a
number nobody published.

## 30m. An audit of the market-data work, and why the platform does not trade

The previous milestone shipped a market-data layer. This session audited it and
then watched the platform run, which found things reading the code had not.

### A leaked credential

Twelve Data takes its API key as a query parameter. `http.Client.Do` returns a
`*url.Error` carrying the whole request URL, and Go's own redaction strips
userinfo passwords while leaving the query untouched. Wrapping that error
verbatim put the key into provider health -- which the market-data status
endpoint serves -- into the persisted `market_data_segments.failure_reason`,
and into a log line. A probe against a closed port printed it three times.

`TestTheProviderHealthNeverCarriesTheKey` passed throughout, because it drives
a 401 whose error is a static string with no URL in it. It tested the path that
had been thought about rather than the path that leaks. That is the general
lesson from this audit: every one of the serious findings below was on a path
whose test existed and exercised a different branch.

Fixed twice over: at the source, where only the transport CAUSE is kept, and at
the boundary with a scrub that removes the raw and percent-encoded forms. Two
locks, because the first already failed once. ENGINEERING_GUIDE.md rule 13.

### Five more defects in the same layer

- **`Aggregator.Seed` had no production caller.** It exists to carry a forming
  bar across a restart. Without it the first quote after a restart reopened the
  current interval from a single tick, the unconditional upsert replaced the
  accumulated candle, and the boundary marked that truncated bar COMPLETE. A 4h
  bar restarted three hours in reported the last hour's range as the whole
  interval's, and every stop or ATR derived from it was wrong by the part
  discarded. Restarts are routine here -- the documented reseed procedure
  requires one. Now wired; on the first run it resumed 18 bars. It also reset
  the tick count, which would have shown a restart in the data as a volume
  collapse.
- **`.Add(time.Nanosecond)` is a no-op against Postgres**, which stores
  microseconds -- a fact ENGINEERING_GUIDE.md already records. Two half-open ranges used it
  to mean "inclusive" and silently dropped their last row. In the coverage
  endpoint that meant the MOST RECENT gap was never reported: a feed that
  stopped for three days and resumed showed no gaps at all.
- **The completeness guard tested `OpenTime`**, which the forming bar satisfies,
  so it never fired. The seeder wrote one unfinished candle per instrument per
  timeframe on every fresh database, labelled complete.
- **The sync watermark was not provider-scoped**, so the live quote
  aggregator's synthetic bars set it. A real backfill would have reported
  "already up to date" while the history behind that point was never acquired
  -- failure that looks like success.
- **The snapshot wrote its CSV before its manifest**, leaving a window in which
  generated bars sat in the research directory with no record of what they
  were, and the importer's default for an unaccompanied file is
  HISTORICAL_MARKET. Rule 14 exactly. The CSV is now written under a temporary
  name and renamed into place only after the manifest is down, and the Python
  importer refuses a file in the export shape that has lost its manifest.

### Watching it decide

With the stack up and autopilot on, the first autonomous decision recorded:

    macd_momentum  sell  confidence 0.618962
    discarded: declares itself valid in TRENDING, and the regime is UNKNOWN
    NO TRADE: no strategy offered an actionable opinion that survived the policy

A signal well above the confidence floor, discarded by the regime gate. No
PAPER-promoted strategy declares itself valid in UNKNOWN, so an UNKNOWN verdict
is a guaranteed NO TRADE whatever the strategies saw.

`classify_regime` had no branch for ADX between 20 and 25; it fell through to
UNKNOWN. A census over the sixteen generated market conditions -- 3 648
classified points -- put that fallthrough at **9.5% of all decision points**,
matching the band almost exactly (9.6%).

I first read the single live decision as a total trading blackout. The census
says one bar in ten. The smaller number is the correct one and the earlier
framing was an overstatement from a sample of one.

The band was also inconsistent with the branch above it: a market going nowhere
was RANGING at ADX 30 and unclassifiable at ADX 22, the less directional
reading getting the more conservative treatment. Measured efficiency says which
population it belongs to -- median 0.203, against 0.154 for RANGING and 0.511
for TRENDING. The same rule now applies in the band. UNKNOWN fell to 2.7%,
RANGING rose to 51.8%, and **TRENDING was unchanged at 42.9%**: the trend
definition was not loosened.

### So why does it not trade?

Not the regime gate, and not the consensus policy. A `trend-clean` replay
through the real pipeline produced 26 decisions, every one with the regime
correctly resolved to **TRENDING** -- and 130 strategy signals of which
**every single one was `no_trade`, mean confidence 0.000**. On the project's own
clean-trend fixture, not one of the five PAPER-promoted strategies fired.

That is consistent with what the research milestones already found and is the
open question this platform still has: the strategies, not the plumbing around
them. The plumbing now works and can be shown to work.

## 30n. The blocker to real historical data, and the smallest thing that removes it

The task was to find the highest-priority blocker to bringing legitimate
historical XAUUSD 1H data into Vantage, and to build the smallest coherent
thing that removes it.

### The blocker is not the provider

The obvious answer is "there is no API key", and it is the wrong one. A key is
a five-minute change to `.env`, and the moment it exists the platform would
start writing real bars into `market_bars` through a code path **that had never
been executed**. Grepping the tree for callers of `Backfill`, `Sync`, `Repair`
and `Snapshot` inside `internal/marketdata/*_test.go` returned nothing at all.
Every `Syncer` test exercised a pure helper: chunk arithmetic, symbol mapping,
gap detection on a slice. The acquisition path itself -- provider to store to
provenance record -- had no test.

That is the blocker, because of what the data is for. Bars acquired here become
the evidence a strategy is judged on. A chunking bug that drops the bar at each
seam, an upsert that lets a forming bar overwrite a finished one, a repair that
invents a candle to fill a hole: none of these announce themselves. They
produce a slightly wrong history that every later measurement treats as
ground truth, and nothing downstream re-checks it. Acquiring real data before
the path that acquires it can be shown to work is the expensive order to do
this in.

### The decision

Build a fake Twelve Data server that implements the provider's **published**
contract, and drive the real `Syncer` and the real store against it.

The alternatives were worse. Mocking the `Provider` interface tests the
`Syncer`'s view of a provider rather than the provider, and the provider is
half the risk. Calling Twelve Data from a test is not available -- no key -- and
would be the wrong shape anyway, since a test that needs the internet is a test
that is sometimes skipped. So: a real HTTP server, in the test, answering the
way the documentation says Twelve Data answers.

The documented behaviours the fake reproduces, each of which the implementation
has to get right:

- values come back **newest first** unless `order=ASC` is sent;
- `outputsize` is capped at 5 000, and a request for more silently returns
  fewer;
- errors arrive as `{"code","message","status":"error"}` with **HTTP 200**, so
  a client that checks the status code sees success;
- `datetime` is `2006-01-02 15:04:05`, in the timezone the request asked for.

The fake also parses the **request** dates strictly as `2006-01-02T15:04:05`
and refuses any other form. That is deliberate: the implementation was sending
a space separator, which the documentation does not list. A provider that
silently reinterprets a date is worse than one that rejects it, because the
response still looks like a success and the window is quietly not the window
that was asked for. The format is now the documented one, and the fake fails
the test if it regresses.

`internal/marketdata/acquisition_integration_test.go` holds sixteen tests. They
are gated on `VANTAGE_MARKETDATA_E2E=1` plus a database URL and skip cleanly
without them, because they need a real Postgres: the thing being tested is
partly the SQL.

### What the tests found

**A real defect, and it was the one that mattered.** Acquiring a 9 000-bar
range reported `stored 8999 of 9000`. The missing bar was the hour the process
happened to be in. The live quote aggregator writes that hour as an
**incomplete** bar; the syncer writes it as a **complete** one from the
provider; they collide on `(instrument_id, timeframe, open_time)`; and the
unconditional `ON CONFLICT DO UPDATE` let whichever arrived last win. In a
development database the aggregator always arrives last, so a real backfill
would have left one truncated bar per instrument per timeframe at the boundary
between history and live data -- exactly where a strategy's newest bar is.

The fix is one clause in `internal/store/market.go`:

    WHERE NOT (market_bars.complete AND NOT EXCLUDED.complete)

A complete bar is never replaced by an incomplete one. A provider may still
correct its own finished bar, which providers do, and there is a test for each
half.

**Two of my own fixtures were wrong, and the platform was right.** A repair
test left one gap behind; the hole I had punched was on a Sunday, and `Repair`
correctly declines to invent bars for a closed market. A second fixture
collided with an existing test helper name. Both were my errors. They are
recorded because the first one is the shape of mistake that turns into a
platform "bug report" if nobody checks the calendar.

### What this does not establish

Nothing here has spoken to Twelve Data. The request shape is verified against
the published documentation and pinned by a test; the response handling is
verified against a fake that follows it. The first real response may differ,
and if it does the failure will be in the parsing, not in the pipeline --
which is the point of doing it in this order.

Two things to carry forward when a key is configured: the provider's Individual
plans are licensed for personal, internal and non-commercial use, which
constrains what acquired data may later be used for; and the free tier's limits
(8 requests a minute, 800 a day, 5 000 data points a request) are what the
syncer's 4 000-bar chunking already assumes.

### And then the Playwright suite tested a different application

This was found while gathering the evidence for §0.12 rather than while
building anything, and it is worth writing down because the failure mode is
the same one as everything else in this section: a check that exists, passes,
and is looking at the wrong thing.

`playwright.config.ts` defaults `baseURL` to `http://localhost:3000`. On this
machine another project -- FORGE, a workflow orchestrator -- now holds that
port. The suite loaded FORGE's landing page and then failed every test at
sign-in, waiting for a `.topbar` that page has never had:

    31 failed, 26 passed (12.2m)

Twelve minutes, thirty-one failures, every one of them reading as a defect in
this application's own auth, chart, terminal and trading code. Nothing in the
output named the port, the other application, or the fact that the Vantage
terminal had never been reached; the only place that information existed was
the page snapshot in `test-results/*/error-context.md`. ENGINEERING_GUIDE.md has warned
about exactly this since the milestone it first happened in, and the warning
did not help, because by the time you go looking for a warning you have already
spent the twelve minutes and have thirty-one plausible bug reports in hand.

The fix is a preflight in `tests/e2e/global-setup.ts`: fetch `baseURL` once and
refuse to run if the server-rendered HTML is not this terminal's. It costs one
request and turns the whole failure into its first line of output:

    Error: http://localhost:3000 is not the Vantage terminal. It answered 200
    with a page titled "FORGE -- distributed workflow orchestration". Another
    application is almost certainly holding that port -- set
    VANTAGE_E2E_BASE_URL to the port the terminal actually got, and set
    VANTAGE_PUBLIC_WEB_ORIGIN to match before restarting the API.

Both branches were verified rather than assumed: against :3000 it names FORGE,
and against a port with nothing on it, it says the terminal could not be
reached rather than letting 57 tests time out one at a time.

The check is a title match, which is a weak signal deliberately: anything
stronger would need a marker in the application bundle, and a rebuild to add
one, and the failure this prevents does not need a strong signal. Serving a
completely different product is not a subtle condition.

With the terminal started on :3005, `VANTAGE_PUBLIC_WEB_ORIGIN` set to match
and the control plane restarted so CORS agreed: **57 passed, 0 failed, in 1.6
minutes**. The same suite, the same code, against the right application.

One consequence worth stating plainly: the smoke suite sends an `Origin` header
that must equal `VANTAGE_PUBLIC_WEB_ORIGIN`, so moving the terminal off :3000
made `smoke.py` fail its first order with `csrf_origin_mismatch` -- the CSRF
control working exactly as designed. `VANTAGE_SMOKE_WEB_ORIGIN` is how that is
told where the terminal went, and with it the suite reports 66 passed, 0 failed.

## 30o. "No strategy fires" was the measurement, not the strategies

The previous revision recorded the platform's largest open question as ST-4:
a `trend-clean` replay produced 130 strategy signals, every one `no_trade` at
a mean confidence of 0.000, and the conclusion drawn was that the strategies
themselves were the problem. That conclusion was wrong, and the way it was
wrong is the same shape as every other finding in this report.

### The one number that should have been suspicious

Five strategies, five different families, five unrelated formulas -- a moving
average crossover, a channel breakout, an RSI fade, a MACD histogram and a
z-score -- and every one of them returned exactly 0.000. Independent methods do
not agree to three decimal places. A uniform answer across unrelated code is
the signature of a shared precondition failing, not of five judgements.

### Where it actually broke

The strategies declare how much history they need. Four of the five PAPER
strategies require **120 completed bars**, the fifth 100.

The orchestrator loads up to 300 bars and refuses below **50** of its own.
Between 50 and 120 it did everything: loaded the bars, resolved the session and
event risk, called the research service, received an answer, recorded it, and
handed it to the consensus policy. The answer was
`no_trade("... requires 120 bars and received 63")` at confidence 0 -- a refusal
to answer, arriving on the wire in exactly the shape of an abstention.

So the control plane wrote it into `strategy_runs` as `no_signal`, which is
what it writes when a strategy has looked at the market and declined. And
`opinionsFrom` passed it to `Decide` as a **fresh opinion**, where the policy
weights each opinion by one over the number of strategies in its family -- so a
strategy that never formed a view halved the weight of the one that did, and
the decision snapshot recorded five contributors when one had contributed.

The replay warm-up is 60 instants, chosen for the 50-bar volatility baseline
and the research plane's 60-bar regime floor. Nothing connected that number to
the 120 the strategies ask for, and nothing had to: the two live in different
processes and neither ever compared them.

### How much of the evidence this was

Measured over every replay fixture, at the standard 60-instant warm-up:

| Fixture | Bars | Signals recorded | Guaranteed `no_trade` |
| --- | ---: | ---: | ---: |
| `trend_clean` | 140 | 405 | **280 (69%)** |
| `range_bound` | 140 | 405 | 280 (69%) |
| `drawdown` | 120 | 305 | **280 (92%)** |
| `false_breakout` | 120 | 305 | 280 (92%) |

Four fixtures -- `drawdown`, `false_breakout`, `spread_spike`,
`test_partial_fill` -- are shorter than 120 bars plus their warm-up, so no PAPER
strategy could produce a signal on them **at any point in the dataset**. Not a
weak signal: a structurally impossible one.

### The fix

The requirement belongs to the strategy, so the strategy reports it. `evaluate`
now stamps `required_bars` on every answer and sets `insufficient_history` when
it refused for want of history; both cross the wire; and the orchestrator
records such an answer as a **SKIP with its numbers** rather than as an
opinion, before `valid` is set and therefore before it can reach the policy.

No threshold was moved, no formula changed, no fixture lengthened and no
requirement lowered. The number is not duplicated on the Go side either -- a
second copy of "120" in the control plane would be a constant that could drift
from the code it describes.

### What the same replay says now

Same dataset, same strategies, same parameters, through the real pipeline:

    strategy_runs   skipped 551 · no_signal 73 · succeeded 46
    of the skips    266 are "insufficient history", naming both numbers
    signals         buy  26  mean 0.453  max 0.538
                    sell 20  mean 0.700  max 0.725
                    no_trade 73 -- genuine abstentions
    decisions       39, every regime TRENDING
    orders          none

The strategies fire. Forty-six actionable signals on the fixture that was
reported as producing none.

### And the real reason nothing trades, which is now visible

No order was placed, and the numbers above say exactly why -- two causes, and
neither is "the strategies do not work":

- **The only strategy clearing the confidence floor is not allowed to trade a
  trend.** `rsi_mean_reversion` scores 0.700 and declares itself valid in
  RANGING and LOW_VOLATILITY, so the regime gate correctly discards it on a
  clean trend. That is the policy working.
- **The strategies that ARE valid in a trend did not clear the floor.**
  `macd_momentum` peaked at 0.538 and `donchian_breakout` at 0.441 ON THIS
  DATASET, against a policy that discards below 0.55 and requires 0.60 net.
  The score is a raw average of hand-chosen components and three strategies
  average a real component with a hard-coded constant, so half the dynamic
  range is thrown away and a strong signal is compressed towards a mediocre
  one.

  An earlier revision added "so its ceiling is arithmetic, not market; it could
  not clear 0.55 however clean the trend". That was wrong and is corrected
  above: `donchian_breakout` clears 0.55 at a 0.6 ATR penetration and reaches
  0.733 at 0.97x, measured. What this dataset showed is that its breaks were
  weak, not that strong ones are impossible -- which is a different finding and
  points at different work.

The honest statement of ST-4 is therefore not "no strategy fires". It is that
**the confidence scale and the policy's thresholds were never calibrated
against each other**, and on these fixtures the set of strategies that can
clear the floor and the set permitted to act did not intersect.

**On these fixtures, and not in general.** A later revision tested whether the
two gates can be open at once, because "do not intersect" read as a structural
claim and a structural claim would mean the platform can never trade a trend.
It is not structural. A constructed market that `classify_regime` calls
TRENDING, ending in a break of 1.0x ATR through its own channel high, gives
`donchian_breakout` 0.750 -- above the 0.55 floor, above the 0.60 net
requirement, and valid in that regime -- with no policy changed. The control in
the same test, an identical trend with a 0.5x break, scores 0.512 and is
correctly refused, so the floor still discriminates on break size.

What the committed fixtures lack is a decisive breakout. That is fixed with a
dataset, not with a threshold, and it is a smaller and much more honest piece
of work than recalibrating the scale.

That is a research question with a defined shape, and moving the threshold to
make trades appear would answer it dishonestly. What has changed is that it can
now be measured: the 266 fabricated abstentions that used to bury it are gone.

### A postscript on how the wrong conclusion survived

Appendix A, row 53h, already said it: "under the default policy nothing clears
the 0.55 confidence floor, so the platform places no autonomous orders at all".
That was written two milestones earlier and it was right.

A later milestone then observed 130 signals at confidence 0.000, concluded the
strategies were the problem, and wrote that into the body of this report --
where it sat alongside the correct explanation without either noticing the
other. The newer, louder, wronger statement is the one that got repeated.

The lesson is not about strategies. **A report that contradicts itself in two
places is worse than one that admits it does not know**, and the only defence
is the one section 0 now exists to be: a single place where each requirement
has exactly one current answer, updated rather than appended to.

## 30p. What an independent review of this milestone found

The two commits above were reviewed before publication, and the review found
six things. They are recorded because three of them are the same failure this
milestone is about -- a check that exists, passes, and is looking at the wrong
thing -- committed by the same hand that had just written the warning.

**A regression introduced by the fix itself.** Recording a history refusal as a
skip routed it through `skip()`, which records with a NIL bar time. Both
per-bar unique indexes are partial -- `WHERE bar_time IS NOT NULL` -- so
`ON CONFLICT DO NOTHING` never matched and every 30-second tick inserted a
fresh row. An hourly bar is 120 ticks; five strategies across three instruments
starved for twenty hours would write 36 000 rows where the previous code wrote
300. Nothing prunes `strategy_runs`, and `PurgeStrategyRunsInRange` only
deletes rows that HAVE a bar time, so a replay reset could not clear them
either. Fixed with `skipAtBar`: a refusal that knows which bar it refused says
so, and the per-bar index deduplicates it. The pre-flight refusals keep the nil
because they genuinely happen before the bars are loaded.

**A test for the API-key leak that could not fail.** It drove an HTTP 500,
which returns a static error string with no URL in it. The only path that can
carry the credential is the transport error from `client.Do`. This is the exact
trap this report records in section 30m, and ENGINEERING_GUIDE.md names it in
so many words -- repeated three weeks later with 500 in place of 401. The test
now points the provider at a closed port. Verified by removing both scrubs: the
segment then records `...?apikey=test-key-not-a-real-credential&...` and the
test fails. With either scrub in place it passes, which is correct -- the
guarantee is that the key is never persisted, not that a particular lock works,
and the provider's lock has its own unit test.

**Nothing connected the fix to the thing it fixed.** `historyRefusal` was
tested in isolation and the consensus behaviour was tested with a hand-built
`Outcome`. Delete the call site and `historyRefusal` becomes dead code with
every test still green. A unit test cannot catch that, so
`TestAStarvedStrategyIsSkippedBeforeItCanBecomeAnOpinion` asserts the call site
itself: that `EvaluateAndRoute` consults it, BEFORE the routing context is
built with `valid: true`, and that the result leaves through a skip. Verified
by deleting the early return; the test fails with "EvaluateAndRoute never
consults historyRefusal".

**A test that passed when its subject did nothing.** `TestSyncFetchesOnlyWhatIsNewer`
iterated over the requests `Sync` had made and asserted none re-downloaded old
history. With zero requests the loop body never ran. A `Sync` that regressed to
fetching nothing at all reported success -- the passing lie this report
documents elsewhere, in a test written to guard against it. It now asserts a
request was made and that bars were stored, before checking what was asked for.

**Two smaller ones.** The fake provider kept `srv.Config` and discarded the
`*httptest.Server`, so `Close()` was a no-op and sixteen tests leaked a
listener each. And the cleanup deleted only `provider = 'twelvedata'` rows,
while the forming-bar test writes one attributed to `mock` -- so if the
`UpsertBars` guard ever regressed, the stray row would survive teardown and the
NEXT run would fail in an unrelated test that counts rows over the same range.
One real defect would have become a permanent, misattributed failure.

The review confirmed the three production changes themselves: the `UpsertBars`
truth table is exactly as intended and `complete` is `NOT NULL` so there is no
three-valued hole; the early return skips nothing mandatory; `dataclasses.replace`
preserves the mutable defaults and cannot bypass `__post_init__`; and the new
field pair's names match on both sides of the process boundary, with the zero
value meaning "not starved" for an older research service.

## 30q. CI ran for the first time, and failed

Every revision of this report has carried the same warning: `actionlint` passes
with 0 findings, every command the workflows run has been executed by hand, and
that is not the same thing as CI passing. The repository was published on
2026-09-30. The first run failed **both** workflows.

`actionlint` had reported 0 findings before, during and after. It still does.
It validates workflow syntax, expressions and shell -- it cannot know that an
environment value will be rejected at run time, that a pinned action's own
dependency has been deleted upstream, or that a CLI has dropped a flag.

### What it found

**`VANTAGE_ENV: local` is not an environment.** Both migration jobs set it.
`config.Load` accepts `development`, `test`, `staging`, `production` and
nothing else, so every migration job died at start-up with "not a recognised
environment". A value that exists nowhere in the codebase had been sitting in
the workflow since it was written, and no local run of the migration command
uses those env vars.

**mypy found four errors that pass locally.** `np.nanmean` returns
`np.floating`, not `float`, and `indicators.py` passed the result straight into
a function annotated `float`. Not the Python version -- `[tool.mypy]` pins 3.12
-- and not numpy, which is 2.5.3 on both sides. **The venv on the development
machine holds mypy 2.3.1, outside the declared `>=1.13,<2.0`.** CI honours the
constraint and installs 1.x, which is stricter here. So every "mypy clean" in
this report's history was produced by a tool the project does not declare.

The fix is `float(...)` at the boundary. It is exact -- `np.float64` *is* an
IEEE-754 double -- so no indicator value moves, which matters because altering
an indicator alters every strategy that reads it.

**Every Trivy job died in "Set up job".** `trivy-action@v0.28.0`, correctly
pinned by SHA, internally resolves `setup-trivy@v0.2.1` -- and that tag no
longer exists upstream; its tags now start at v0.2.6. Pinning your own
dependency does not pin its dependencies. The error names neither the nested
action nor the deleted tag. Moved to v0.36.0.

**OSV-Scanner exited 127 printing its usage text.** v2 dropped `--skip-git`.
The previous revision recorded that this job had referenced a tag which never
existed and that pinning it to a real commit was a version change; this is the
rest of that bill. Repository scanning is the default now, so the flag is gone.

**pip-audit failed on the repository itself, twice.** `vantage-quant` is
installed editable so its dependency tree resolves, and it is not on PyPI, so
it cannot be audited -- correctly, and not a vulnerability. `--skip-editable`
failed differently: under `--strict`, pip-audit turns a skip into an error.
Dropping `--strict` would have gone green and would have been the wrong fix,
because it silences an unauditable *third-party* package too -- the one thing
the job exists to catch. The job now freezes the resolved dependency set
without editable installs and audits that.

### Where it stands

Both workflows are green: CI run `36718123184`, five jobs -- control plane,
research plane, terminal, migrations applied twice, images built. Security run
`36718123066`, eleven jobs -- govulncheck, npm audit, OSV-Scanner, Gitleaks,
Semgrep, pip-audit, Trivy filesystem and three Trivy images, repository policy.
OSV-Scanner and pip-audit have produced a result for the first time; both are 0
findings, and neither has ever been run locally, so the CI job is the only
evidence for them and is cited as such.

### The honest summary

Six defects, in workflows that had been reviewed, pinned by SHA, linted clean
and described in this report as ready. Four of them are in the same category as
everything else this milestone found: a check that existed, passed, and was
looking at the wrong thing. Two are the specific failure mode of a config file
nothing executes -- it cannot be wrong until something runs it, and then it is
wrong all at once.

There is a temptation to record this as "CI configuration issues, now fixed".
It is more useful recorded as what it is: **the gap between a green local
checklist and a green pipeline was six defects wide, and nothing available
before publication could measure it.**

## 30r. Does it survive a week? No -- and the reason was not what anyone was looking for

The question asked was whether this platform runs unattended for a week or
quietly stops. Two audits were run against it: one for resource exhaustion and
drift over time, one for authorization and lifetime bugs that a scanner cannot
see. Four of the findings are fixed here. The first is the answer to the
question and it is worse than "it stops".

### It does not survive one day, and it does not stop

**Nothing in the running system ever refreshes an FX rate.** `UpsertFXRate`
has exactly two callers outside the store: the seeder, once, and a hook that
only fires inside a market replay. There is no scheduler job, and
`fx.StoreRateSource` only reads what is there. The converter's maximum age is
24 hours (`internal/app/app.go`).

The account is ZAR and every instrument is USD-quoted, so every valuation
needs a conversion. Twenty-four hours after a seed, they all fail.

Half of what follows is correct. `risk.Size` returns `ErrSizingUnavailable`,
so no autonomous order can be placed -- that is failing closed, and it is the
behaviour the rules ask for.

The other half is not. `portfolio.valuePosition` does **not** fail closed: it
sets a `ValuationNote` and returns the view with `Valued = false` and no
error. `portfolio.aggregate` then counts that position in `Unvalued` and
`continue`s -- so it contributes **nothing** to unrealised P&L, nothing to
margin used, nothing to gross or net exposure. Equity becomes balance plus
zero. Drawdown is measured against it. Free margin looks full.

The book does not report an error. It reports a smaller, healthier book than
the one that exists, and every ceiling in the risk engine passes more easily
because of it.

`Snapshot.UnvaluedPositions` has carried the warning since it was written. Its
comment in `handlers_read.go` says, in so many words, that a value above zero
means the figures beside it understate risk. Searching the tree for every
consumer of that field returns: the struct, the counter that sets it, and one
JSON response field. **Nothing reads it.** Not a risk check, not the OMS, not
readiness. `/health/ready` reports `ready`, `HEALTHY`, `quant: ok`, and the
only truthful signal in the entire system is an ERROR log line about a stale
rate, twice a minute.

So the shape of the failure is: day one, it trades; day two, it silently stops
trading autonomously while reporting health, and any manual order is assessed
against a book that is missing its open positions.

**Fixed** by making the engine refuse: `CheckBookIsValued` fails when
`UnvaluedPositions > 0`, with the code `book_not_valued`. It is the fail-closed
rule applied to the one input every other check depends on.

It exempts reducing orders, and that exemption is the point rather than a
softening. Refusing to let an operator **close** a position because the
platform cannot price it would hold them in the position precisely while the
system admits it does not know what that position is worth -- the trap the
reducing-order rule exists to prevent, and which three separate checks were
once found setting.

What is NOT fixed here: the rates still do not refresh. A real FX source is a
provider behind the `fx.RateSource` seam and this build may not contact a third
party, so the honest position is that the platform now refuses rather than
guesses, and the operator is told why. Re-stamping a held rate with a fresh
timestamp would have made the symptom disappear by fabricating data, which is
the one thing the market-data rules forbid outright.

### A halt that releases itself after seven days

Rule 7's corollary -- "not re-detected" must mean "fixed", never "no longer
examined" -- is implemented by freezing the execution cursor while any
execution-derived issue is open. The freeze is correct, and the comment above
it explains at length why freezing beats widening.

The lookback underneath it undid the whole thing:

    since := now - 7d
    if cursor != nil && cursor.After(since) { since = cursor - 1m }

While the frozen cursor is newer than seven days, it anchors the window. Once
wall time passes it, `cursor.After(since)` is false, `since` snaps **forward**
past the frozen cursor, the venue capture stops fetching the execution that is
being argued about, `Classify` cannot produce its fingerprint, and
`closeVanishedIssues` resolves the issue with *"the divergence is gone: this
run re-examined the same evidence and did not find it."*

It had re-examined nothing. An unbooked venue execution would have closed its
own issue and the account would have released its own halt -- the exact incident
the freeze was written for, rebuilt out of a lookback constant, firing seven
days in.

**Fixed:** the seven days are now `executionFirstRunLookback` and apply only
when there is no cursor. Once a cursor exists it anchors the window however old
it has become. The cost is a fetch that grows while something is unresolved,
which the surrounding comment had already accepted as the price of the freeze.

### A trader could lift a halt an administrator placed

`handleDeactivateKillSwitch` performed **no** scope or role check: it parsed a
UUID and called the store, which has no predicate either. The route sits in the
trader-or-admin group, while activating a *global* switch is admin-only. So the
role that cannot create the control could remove it, and the ids are handed out
by `GET /kill-switches`, which any authenticated user may call.

Activation was incomplete in the same way. Its comment claims *"only an admin
may halt globally or halt someone else"*; the code checked `global` and
`account` and let `user`, `broker` and `strategy` through unexamined. A trader
could halt another operator by user id, or halt every account on a broker --
which in this build is every account there is, making it a global halt reached
without the admin check that guards the global scope.

**Fixed** with a single `authoriseKillSwitchScope` used by both paths, written
as a `switch` over the scope type so a new scope cannot be added without the
compiler pointing at it. That is the real repair: both bugs were the
predictable result of an authorization rule written as an `if` ladder inside a
handler, covering the branches someone happened to think of.

Nothing a trader could do to their own account is removed. Account-scope halt
over an owned account, cancel and flatten are all untouched -- an operator
stopping their own trading is never impeded.

### The login rate limit was bypassable with a header

`middleware.RealIP` was the **first** middleware on the router. chi ships it
marked `Deprecated: RealIP is vulnerable to IP spoofing` (GHSA-3fxj-6jh8-hvhx,
GHSA-rjr7-jggh-pgcp, GHSA-9g5q-2w5x-hmxf): it rewrites `r.RemoteAddr` from
`True-Client-IP`, `X-Real-IP` or the leftmost `X-Forwarded-For`,
unconditionally, with no trusted-proxy list.

The defence was downstream in `clientIP`, which honours `X-Forwarded-For` only
behind a configured trusted proxy. It could not work: `RealIP` had already
poisoned `RemoteAddr` before `clientIP` read it. The trusted-proxy branch is
also dead -- nothing in production sets the flag.

The login limiter keys on the client address, so a caller presenting a fresh
`X-Forwarded-For` per request got a fresh full token bucket every time. That
removed the only cost control on an endpoint that runs Argon2id at **64 MiB**
per attempt -- including on the unknown-user path, which hashes a dummy to
equalise timing. A hundred concurrent requests is about 6.4 GB. It also let an
attacker write any address they chose into `audit_events.ip_address` and
`sessions.ip_address`; the hash chain still verifies, because it signs whatever
it is given.

**Fixed** by deleting one line. `clientIP` was always correct.

The guard test is the interesting part. `TestAClientCannotSpoofItsAddressUnlessAProxyIsTrusted`
passed throughout, because it calls `clientIP` **directly on a bare
httptest request** -- no middleware ever runs. The helper was right; the stack
above it was not, and a test of the helper could not see that. There are now
two tests through the router, one behavioural across all three headers and one
asserting the middleware stays out of `routes()`. Verified by putting it back:
the second fails with *"middleware.RealIP is back in the router"*.

### And the disk

Docker's default `json-file` driver is unbounded and no service declared any
rotation, while Postgres runs with `log_connections`, `log_disconnections` and
`log_min_duration_statement=500` against a pool that recycles every 30 minutes.
On Windows that grows inside the Docker Desktop VHDX and is never reclaimed.
All five services now cap at 50 MB × 5.

### What is recorded but not fixed

The longevity audit found more than was repaired here, and the rest is listed
in section 31 rather than quietly dropped. The largest is retention:
`market_quotes` gains roughly 179,000 rows a day -- about 27 MB, 10 GB a year --
and the only scheduled deletion anywhere in the module is the session sweep.
Several tables carry append-only triggers and cannot be pruned at all by
design, which makes an archive strategy a real design question rather than a
cleanup job.

Also unrepaired: `strategy_runs` still grows unbounded under any *persistent*
pre-flight refusal, because only the history-refusal path was given a bar time.
A halted account writes about 23,900 rows a day through the remaining nil-bar
skips. And `vantage_marketdata_latest_bar_age_seconds` is defined and never
set -- the one gauge that would catch rule 9's frozen-bar failure recurring is
dead.

### The common shape

Every one of the four is the same thing in a different place: **a control that
the repository's own documentation and tests describe as present, which is not
present, because the check sits one layer away from where the decision is
made.** `UnvaluedPositions` documents the risk and is read by nobody. The
cursor freeze is reasoned about at length and overridden by a constant above
it. The kill-switch comment states a rule the code does not enforce. `clientIP`
implements the spoofing defence and runs after the middleware that defeats it.

In three of the four, a test existed and passed.

## 30s. Bounding the growth, and setting the gauge that was never set

Continuing from the longevity audit in section 30r. Four of its findings are
closed here; what remains is listed in section 31.

### `market_quotes` is write-only, and nothing had ever noticed

The tree contains exactly one `INSERT INTO market_quotes`, one replay-scoped
`DELETE`, and **no `SELECT` at all**. It gains roughly 179 000 rows a day -- six
instruments at the 2-second ingest interval -- about 27 MB a day and 10 GB a
year, and no decision has ever read a row of it. The hot path for a price is
`market_quotes_latest`, which is a different table.

That was verified rather than inferred. On a development database holding
39 714 quote rows, the retention statement removed all of them while
`market_quotes_latest` stayed at six, and the platform did not notice: ingestion
refilled 150 rows within the next few minutes, `/health/ready` reported
`database: ok` and `quant: ok` throughout, and both smoke suites pass afterwards
(66/0 and 35/0). Deleting the entire quote history had no operational effect,
which is the strongest available statement of what "write-only" means.

The hourly `cleanup` job -- which already held a lease and already pruned
sessions -- now also trims `market_quotes` and `market_data_health` at 30 days,
`strategy_runs` at 14, and **published** outbox rows at 7.

Each window is a separate constant with its reason beside it, because the
consequence of being wrong differs per table. The outbox sweep touches
`published_at IS NOT NULL` only: an unpublished row is still owed to a
consumer, and the dispatcher selects exactly those. That guard was proved
empirically rather than read -- two rows inserted, one published thirty days
ago and one never, the sweep run, the delivered row gone and the undelivered
one still there.

Every sweep is capped at 50 000 rows per run. The first pass against a database
that has been up for months would otherwise issue a single DELETE over millions
of rows and hold locks for as long as it took; the job returns and takes the
next slice an hour later. A sweep that hits its cap every hour logs its count,
so a backlog that is not shrinking is visible rather than silent.

**The append-only tables are deliberately absent.** Ten of them carry a trigger
that raises on DELETE -- `transactions`, `fills`, `order_state_transitions`,
`audit_events`, `decision_snapshots`, `reconciliation_issue_events` and the four
`*_history` tables. They are the financial and evidentiary record and the
database itself refuses to let a cleanup job touch them. They need an archive
design, which is a decision rather than a line in a function, and it is not
made here. `command_idempotency` is also left alone: it is a correctness guard
against double execution, and the growth is per command rather than per tick.

### The gauge that would have caught rule 9

`vantage_marketdata_latest_bar_age_seconds` was declared in `internal/metrics`
and **set by nothing**. Grepping the tree for a setter returned the declaration
and no other line.

Rule 9 exists because the bar series froze at seed time once: every strategy
re-evaluated a single bar for ever, the per-bar guard jammed shut, the live
quote drifted 79 dollars away from the newest bar, and nothing reported
unhealthy. A frozen series is invisible in every other signal -- ingestion
succeeds, the quote is fresh, the feed is `ok` -- and shows up in this one
measurement immediately, as an age that climbs and never resets.

It is now set in `Aggregator.AggregateInto`, from the bar's close against the
quote's own source time rather than `time.Now()`, so a replay measures dataset
time like everything else on that path.

### Finishing the bar-keying

Section 30p fixed one refusal path and left the rest. The per-bar unique
indexes on `strategy_runs` are partial -- `WHERE bar_time IS NOT NULL` -- so a
refusal recorded without a bar is inserted afresh on every 30-second tick, and
`PurgeStrategyRunsInRange` cannot delete it either, because that only matches
rows which have one.

The three refusals that happen *after* the bars are loaded now key on the
newest bar in hand: too little history, replay warm-up, and a research-service
failure. The last matters most -- the quant circuit breaker opens for 30 seconds
and the scheduler ticks every 30 seconds, so a research service that is down
used to record a failure per strategy per instrument indefinitely.

The genuine pre-flight refusals keep their nil. They run before any bar is
fetched and inventing a key for them would be worse than the duplication; the
retention sweep above is what bounds those.

### And a reordering

The hourly cleanup computed a portfolio snapshot for its metrics and then
rolled the trading day, with a `continue` on the snapshot's error in between.
Any persistent failure to compute would therefore have frozen the daily-loss
reference point indefinitely while the risk engine kept measuring against it.
The roll needs the account and the clock and nothing else, so it now happens
first.

## 30t. Three silences, and a dependency nobody can upgrade

Closing the rest of the longevity list from section 30r.

### Audit verification stopped at ten thousand events and still said "verified"

`GET /admin/audit/verify` fetched sequences 1 to 10 000 and reported a verdict
for the chain. Past that length it verified the OLDEST ten thousand events,
never examined the recent end -- which is where tampering would be -- and still
answered `verified: true`. `head_sequence` was in the response beside it, so
the contradiction was visible to anyone who compared two numbers, and nothing
said the check had been partial.

A partial check reported as a complete one is worse than no check, because it
is believed.

It now walks the whole chain in pages, each page verified against the previous
page's final hash so the link across a boundary is checked like any other.
`VerifyChainFrom` is the new entry point; `VerifyChain` delegates to it with the
genesis hash and keeps its meaning.

Paging only helps if the boundary is checked, and that is what the tests pin.
`TestASeveredLinkAtAPageBoundaryIsCaught` builds two separately valid chains and
splices them: each half verifies from genesis on its own, and only the boundary
check sees that the second does not follow the first. A pager that verified
each page independently would accept a cut-and-respliced history.

The response now carries three fields rather than one. `links_intact` is what
the hashes said; `fully_verified` is whether the whole chain was reached; and
`verified` is true only when both hold. A walk that hits the 500 000-event cap
says so in `note_partial` and does not claim a verdict it did not earn.

### Nothing warned before a trading authority expired

The development seed grants three years, so the condition is far away and
completely invisible until the day it arrives. On that day:
`ActiveAuthorityForAccount` still returns the row, because it does not filter on
`valid_until`; `Effective` refuses; and the scheduler's strategy loop treats the
refusal as an ordinary skip and `continue`s **without a log line**. The platform
stops placing orders, and the symptom -- every strategy skipping -- is
indistinguishable from a quiet market.

There is now a warning at thirty days, seven days and one day, and a critical
alert once it has lapsed that names the expiry as the reason nothing is
trading. It runs in the hourly cleanup, so a horizon is crossed many times
inside the day it names; the alerter's cooldown, keyed on the account, is what
stops that becoming a notification an hour.

The horizons are sorted at the point of use rather than trusted in declaration
order, and a test pins it. Declared out of order, a thirty-day horizon would
match first with one day remaining and the operator would be told "expires in
30 days" on the day it expires.

**The architecture test caught the first attempt.** The warning was written with
`*notify.Alerter` on the scheduler's dependencies, and
`TestNoPackageDependsOnNotify` failed the build: `internal/notify` is a leaf by
rule, and a consumer declares its own narrow interface for `internal/app` to
wire. That is the rule doing exactly its job, on the same day as a section about
rules that were documented and not enforced.

### A dependency with no upgrade path

Trivy failed the quant image on two newly published HIGH findings in urllib3
2.7.0 -- CVE-2026-97687 and CVE-2026-97689, both fixed in 2.8.0.

urllib3 is not a dependency of this service. It is not in `pyproject.toml`, it
is not installed as a package, and it exists in the image at exactly two paths,
both inside pip's own vendor directory. Verified in the built image rather than
assumed:

    $ docker run --rm --entrypoint sh <image> -c "python -c 'import urllib3'"
    ModuleNotFoundError: No module named 'urllib3'

It is not importable. The research service cannot execute it under any code
path; the only program that can is pip, which runs at image build time.

Nor can it be upgraded away. The Dockerfile already runs
`pip install --upgrade pip`, the image carries **pip 26.2.1**, and
`pip index versions pip` confirms that is the newest release in existence. It
still vendors `urllib3==2.7.0`.

So it is recorded in `.trivyignore` with that evidence, scoped to those two ids,
and with what would change it: a pip release vendoring urllib3 >= 2.8.0, at
which point the existing upgrade line picks it up and the entry should be
deleted. This is the same shape as the setuptools 70.3.0 finding already
recorded there -- a scanner reading vendored metadata as installed code -- and is
kept separate so either can be removed without reasoning about the other.

### What CI is now worth

Two of this session's findings came from CI and could not have come from
anywhere else: a critical RCE in Next published between a clean local `npm
audit` and the push, and these two urllib3 advisories. Both were hours old. The
local checklist was green for both.

## 30u. It could not be deployed at all

The platform was asked to run somewhere reachable from any device. The first
thing that turned up is that it could not have been started anywhere but a
development machine.

### No bootstrap path existed

`seed` is the only caller of `CreateUser`, and `seed` refuses outside
development and test, correctly, because the accounts it writes have passwords
published in this repository. There is no HTTP route to create the first user
either: `/admin/users` requires an existing admin session.

So the guard was right and left nothing behind it. Deploy to production, run the
migrations, and then discover there is no way to log in and no command that can
make one.

`create-admin` fills it. It reads the password from the operator rather than
generating one, because a command that prints a password it chose puts that
password into a terminal scrollback, a shell history and any log capturing the
output, and leaves the operator holding a credential they did not pick. It reads
without echo from a TTY, or from stdin when piped, so the password is never an
argv entry that `ps` can read. It applies the same policy the HTTP layer applies,
because a bootstrap path that accepted a weaker password than the application
would be the weakest link on the account that matters most.

It refuses once any administrator exists. Left able to mint administrators on a
running system it would be a privilege escalation path for anyone who reaches
the host; further administrators are created through the audited HTTP route.

Verified against a scratch database rather than argued: create it empty, migrate,
bootstrap, and the account appears with an Argon2id hash and no plaintext
anywhere. Run it again and it refuses. Feed it a short password and the policy
rejects it before any database work happens.

### Serverless cannot host the control plane

Vercel was the first suggestion and it cannot run this, which is worth recording
because the reason is structural rather than a configuration problem.

The control plane ingests market data every 2 seconds, evaluates strategies
every 30, dispatches the outbox every 3, reconciles every 5 minutes, and holds
state in memory between those ticks: the bar aggregator's forming bars, a regime
tracker per instrument, the correlation matrix, the rate-limit buckets, the
research circuit breaker.

Serverless functions are request-scoped and cold-start. The 2-second loop has
nowhere to live, and the forming bars would be discarded on every invocation,
which is rule 9's frozen-series failure reached by design instead of by
accident. Vercel can host the terminal. It cannot host the thing the terminal
talks to.

### The shape chosen

A Cloudflare tunnel, with Cloudflare Access in front of both hostnames. The
tunnel dials out, so the host needs no inbound port, no public IP and no
certificate of its own. Every port in the compose file stays bound to
`127.0.0.1`, so nothing is reachable even with the host firewall open.

Access matters more than it sounds. The login endpoint runs Argon2id at 64 MB
per attempt and is the one place an unauthenticated stranger can make the
machine do real work. Access means they never reach it. Both are free.

### Three findings from the project's own secret scanner

The production overlay was written first with connection strings assembled
inline, `postgres://role:${PASSWORD}@host/db`. The repository's own
`vantage-postgres-url` rule flagged all three.

That is the rule working. The shape is exactly what a real credential looks
like, and the next person to edit the line might paste one in. So the overlay
was restructured to take each URL whole from the environment, which removes the
shape from the file entirely rather than suppressing the finding.

The scanner then flagged the comment explaining the change, because it wrote the
pattern out. Reworded.

It then flagged the deployment guide, where the documented command is the one
that builds the URL from a variable, so the shape is unavoidable. That is the
one place an allowlist entry was added, and it is narrow enough to be provably
safe: it matches only when the password position is exactly a `${UPPER_CASE}`
reference, which a literal password cannot be. Verified by dropping a file
containing a real literal password into the tree and watching the scanner catch
it with the allowlist in place.

## 30v. Backups, and proving one restores

The deployment work left one thing flagged and not done: nothing backed the
database up. `scripts/backup-restore-drill.ps1` existed and passed, but a drill
is a rehearsal, not a schedule.

### What runs now

A `backup` container in the production overlay, built from the same postgres
image as the server so pg_dump's version always matches it. Daily by default,
keeping fourteen copies.

Two details that are the difference between a backup and a file:

**Each dump is read back before it counts.** It is written under a `.partial`
name, then `pg_restore --list` parses the whole archive's table of contents,
and only then is it renamed into place. A truncated or corrupt archive fails at
the moment it is made rather than months later when something depends on it. A
file in the directory is one that has been read successfully at least once.

**Retention counts only verified dumps.** A run of failures leaves `.partial`
files, which are never counted towards the copies being kept, so a week of
broken backups cannot quietly age out the last good one.

### Proving it, rather than asserting it

The script was run against the live development database, and then one of its
dumps was restored into a scratch database and checked. Six steps, all observed:

| Step | Result |
| --- | --- |
| Dumps written | 4, each 814 850 bytes, each verified by reading the archive |
| Retention at `BACKUP_KEEP=2` | pruned to exactly 2, oldest first |
| `pg_restore` into a scratch database | exit 0 |
| Data present | 3 users, 6 instruments, 11 898 bars, 110 audit events, 10 transactions |
| The APP role can read it | yes, 110 audit events |
| `verify-audit` on the restored copy | **verified: yes** |

The fifth row is the one the drill already warned about. Its first run used
`--no-privileges` and produced a database the application could not read at
all, failing with "permission denied for table audit_events". The GRANTs are
part of the backup and have to come back with it, so the documented restore
command uses `--no-owner` and deliberately does not use `--no-privileges`.

The sixth is the strongest single check available. If the audit hash chain still
verifies after a dump and restore, the log came back byte for byte and the
tamper evidence survived the round trip. Running the platform's own verifier
against the restored copy is a better answer than counting rows.

### What this still is not

It is not off site. A dump in a volume on the same machine survives a dropped
table, a bad migration and a careless DELETE. It does not survive losing the
machine, the disk or the volume. The deployment guide gives a command to copy
the newest dump out and says plainly that nothing in the stack does it for you,
because a backup that lives only beside the thing it protects is a convenience
rather than a recovery plan.

## 30w. Off-site backups, and the one asymmetry worth explaining

Section 30v left the obvious gap: the backups lived in a volume on the same
machine as the database they protected. That survives a dropped table and not a
dead disk.

Each verified dump is now encrypted and copied off, when a recipient key and a
remote are both configured. Neither set, and it is a local backup as before.

### Encrypted away from home, readable at home

The local copy stays plaintext and the off-site copy is encrypted. That
asymmetry is deliberate and is the only interesting decision here.

The off-site copy sits somewhere the operator does not control, so it is
encrypted to a public key whose **private half never exists on the server**.
Compromising this machine therefore yields the live database, which it was
always going to yield, and not one archived backup. That property is the entire
reason to encrypt, and it is lost the moment the private key is kept beside the
thing it protects.

The local copy stays readable because this machine already holds the live
database in the clear. Encrypting it adds no secrecy against an attacker who is
already here, and it adds a new way to lose everything: a single operator who
mislays one key would own a shelf of backups nobody can open. Plaintext locally
means a restore is always possible with what is on the box.

`age` rather than gpg: one file format, one flag, no keyring, no agent, and no
way to encrypt to the wrong key because a keyring held a stale entry. This
project does not implement cryptography and does not intend to. `rclone` for
transport, because it reaches Backblaze, R2, S3, SFTP and most other things
through one configuration file that is mounted read only.

### Proved rather than asserted

The full chain was run end to end:

| Step | Result |
| --- | --- |
| Dump, verified by reading the archive back | 814 850 bytes |
| Encrypted with `age` to a public key | 815 242 bytes |
| Copied to a remote with `rclone` | delivered |
| Decrypted with the private key, which the backup container never held | 814 850 bytes, byte identical |
| `pg_restore` into a scratch database | exit 0 |
| `verify-audit` on the result | **110 of 110 events, verified: yes** |

The last row is the one that matters. If the audit hash chain still verifies
after a dump, an encryption, a network hop, a decryption and a restore, the log
came back byte for byte and the tamper evidence survived the whole journey.

That row also incidentally confirms the audit paging fix from section 30t:
`events checked` equals `head sequence`, so the verdict covers the whole chain
rather than its first page.

### A build that failed for the wrong-looking reason

The sidecar refused to start with:

    /backup.sh: set: line 41: illegal option -

Line 41 is `set -eu`. The file on disk was fine; the file in the IMAGE had CRLF
line endings, so the shell read the flag as `-eu`. The error names neither the
line ending nor the file that carried it.

`.gitattributes` already forces LF on checkout, so a clone was never at risk.
A working tree is a different matter: an editor, a generated patch, or a tool
writing in text mode on Windows can leave CRLF behind, and building locally
before deploying is exactly when someone would hit it. It happened twice during
this work, both times from a script that rewrote the file in Python's text mode.

The Dockerfile now strips carriage returns and runs `sh -n` on the result at
build time, so a malformed script fails the build rather than the backup
schedule, with no dependence on anyone's git configuration.

The first version of that strip was a `sed` expression, and it had to contain a
literal carriage return BYTE in order to match one. It worked. It was also an
invisible character in a build file, where a careless paste turns it into a
no-op that still builds green and ships CRLF into the image again. It is `tr -d
'\r'` now, which says the same thing in characters a reviewer can see.

### The hardening that silently broke the thing it protected

The sidecar ran as root. Semgrep's `missing-user-entrypoint` and Trivy's
DS-0002 both said so, and both were right. The fix is one line, `USER postgres`,
plus creating `/backups` with its ownership in the image so the named volume
inherits it rather than arriving owned by root.

Adding that line broke the off-site copy, and broke it in the worst available
way. `rclone` reads `$HOME/.config/rclone/rclone.conf`; the config was mounted
at `/root/.config/rclone/rclone.conf`, which was correct while the process was
root. `/root` is mode 0700. As uid 70 the config is simply unreadable, so every
upload fails -- while the local dump keeps being written, verified and logged as
a success.

That is the failure shape this project keeps running into and keeps writing
down: not a crash, but a component going quiet while the surrounding system
reports health. A backup that exists on the machine and nowhere else looks
identical in the logs to one that is also off-site, right up until the machine
is the thing that is lost.

The config path is now `/etc/rclone/rclone.conf`, set in the image through
`RCLONE_CONFIG` so it does not depend on which user runs the process, and
mounted read only there.

### What was actually executed

| Step | Result |
| --- | --- |
| Image built with `USER postgres` | `id` reports `uid=70(postgres)`, `/backups` owned `postgres:postgres` |
| Container run with `--read-only`, `--tmpfs /tmp`, `--user 70`, no capabilities | dump written and verified |
| `rclone` config read from the read-only mount | `rclone listremotes` reports `offsite:`, and the upload addressed a named remote rather than a bare path, so the config was genuinely used |
| Dump, encrypt, upload, decrypt with a key the container never held | **byte identical** to the local dump (`cmp`, exit 0) |
| `pg_restore --list` on the decrypted archive | exit 0 |
| Trivy filesystem, CI's flags, after the fix | DS-0002 cleared |

The byte-identity row took two attempts and the first one is worth recording,
because it produced a `MISMATCH` that was a defect in the test rather than in
the backup. The comparison took the FIRST dump in a reused volume and compared
it against the decrypted copy of the LATEST one -- two different dumps, equal in
size because they contain the same database, differing in bytes because
`pg_dump` stamps a creation time into the archive. A test that reuses state
across runs can manufacture exactly the failure it was written to detect. Rerun
on a fresh volume with one dump and one sealed file, it is identical.

## 30x. A credential that nothing could use, and the test that was missing

Checking the deployment path produced a question worth asking of any
configuration: **is every variable this sets actually read by something?** The
answer for one of them was no.

Both compose files handed the research container
`VANTAGE_QUANT_READONLY_DATABASE_URL`, `.env.production.example` asked the
operator to construct it, and `DEPLOYMENT.md` gave them a command to generate
it. The research service has never had a database driver. Not disabled, not
unused: absent.

| Evidence | Result |
| --- | --- |
| Declared dependencies in `pyproject.toml` | fastapi, uvicorn, pydantic, numpy, pandas, scikit-learn, joblib. No driver |
| Installed in `services/quant/.venv` | no psycopg, psycopg2, asyncpg, pg8000, sqlalchemy |
| Any module reading the variable | none; no `BaseSettings`, no `env_prefix`, nothing that would bind it without naming it |
| The package's own docstring | "Nothing here reaches a network, a broker or a database" |

### Why remove it rather than leave it

Nothing was broken. Nothing read the value, so nothing could misuse it, and it
would have been easy to call this cosmetic and move on.

The reason not to is rule 5, which says the research plane never reaches a
broker, the control plane or the trading tables. A credential sitting unused in
a container's environment is not a breach of that rule; it is the thing that
makes breaching it effortless and invisible. Someone adds `psycopg` next year
for an entirely good reason -- a notebook, a feature store, a one-off
investigation -- and the connection string is already there, already correct,
already pointing at the production database. No review happens, because nothing
about that change looks like it touches a security boundary.

So the credential is gone from both compose files and from both example
environments. The read-only `vantage_research` role stays, with its `SELECT`
grants, its `default_transaction_read_only`, and its statement timeouts. It is
for an analyst who chooses to connect, which is a human decision each time.

### The boundary had no test on the Python side

The Go side enforces its equivalent boundary with two arch tests. The Python
side enforced its boundary with a sentence in a guide, which is how a dead
credential survived in two compose files and a deployment document without
anyone noticing.

`services/quant/tests/test_research_plane_boundary.py` now asserts three
things: no database driver is importable, no module in the package imports one,
and no module reads a connection string or database password.

**The guard was verified to fire rather than assumed to work.** A probe module
importing `sqlalchemy` and reading `VANTAGE_QUANT_READONLY_DATABASE_URL` was
dropped into the package; both source-scanning tests failed on it by name, and
the suite went back to passing when it was removed. `find_spec` was separately
confirmed to distinguish present from absent (`numpy` true, `sqlalchemy`
false), so the driver test would fire on a real installation rather than
passing because the check never looks.

A failure of this test is not automatically a defect. It means the boundary is
being moved, and that this should happen in the open rather than as a side
effect of installing a package.

## 30y. The deployment bootstrap, executed rather than described

`docs/DEPLOYMENT.md` tells an operator to run `create-admin` as the first thing
they do on a new deployment, and states that `seed` refuses outside development
and that the control plane refuses the published development keys in
production. All three were written in this milestone. None had been run.

A document that tells someone their first command and is wrong about it is
worse than no document, so the whole path was executed against a scratch
database (`vantage_bootstrap_check`, created and dropped), with
`VANTAGE_ENV=production` and freshly generated keys.

| Claim in the guide | Executed | Result |
| --- | --- | --- |
| Migrations apply to an empty production database | `migrate` | applied, exit 0 |
| The bootstrap enforces the application's password policy | piped `password` | **refused**: "password does not meet policy: passwords must be at least 12 characters" |
| It creates one administrator | piped a 24-character password | created; `users` holds `ops@example.com`, role `admin`, an `$argon2id$v=19` hash, `mfa_enabled` false |
| It cannot mint further administrators on a running system | ran it a second time | **refused**: "1 administrator account(s) already exist" |
| It never echoes the password | inspected the output | the password appears nowhere; the command prints email, name and id only |
| `seed` refuses outside development | `seed` with `VANTAGE_ENV=production` | **refused**: "seed data contains development-only credentials" |
| The platform refuses the development encryption key in production | `serve`, production, key copied from `.env.example` | **refused** at config load: "the example development keys are in use outside development" |
| The platform refuses the development research token in production | `serve`, production, token copied from `.env.example` | **refused** at config load: "VANTAGE_QUANT_SERVICE_TOKEN is unset or still the development value" |

### The negative control, which is the row that makes the others mean anything

A refusal proves nothing on its own: a process that refuses to start for some
unrelated reason produces the same output. So the same development key was run
once more with `VANTAGE_ENV=development`, where it is legitimate. Configuration
loaded, and the process failed later and elsewhere -- at a deliberately wrong
database password, `SQLSTATE 28P01`.

That is the evidence that the check is gated on the environment rather than
firing unconditionally, and it cost one extra command.

### One correction to make here

The first attempt at the key test used `healthcheck`, which does not load
configuration -- it dials `VANTAGE_HTTP_ADDR` and reports what answers. It
failed at the dial, and that failure says nothing about key validation either
way. Read quickly it looks like a result. It is the shape of evidence this
report exists to refuse: a command that failed, for a reason nobody checked,
being counted as the check passing.

## 30z. Will it still be running in a month? Auditing growth, table by table

The question behind "I don't want the bot to be fine for a week and then die
down" is not really about strategies. A platform that ingests a quote every two
seconds dies of disk long before it dies of logic, and it dies quietly: the
retention sweep runs inside the hourly cleanup lease and **its failures are
logged and swallowed by design**, because a housekeeping sweep must not abandon
the daily roll it shares that lease with. A prune that deleted nothing would
look exactly like a prune that worked.

So every table was checked for whether it grows with uptime, and whether
anything bounds it.

| Class | Tables | Bounded by |
| --- | --- | --- |
| Pruned on a window | `market_quotes` + `market_data_health` (30d), `strategy_runs` (14d), published `outbox` (7d) | `pruneTelemetry`, hourly |
| Bounded by a cascade | `strategy_signals` | `ON DELETE CASCADE` on `run_id`, and nothing else |
| Grows with activity, not uptime | `risk_events`, `orders`, `mock_venue_*`, `command_idempotency`, `notifications` | trading volume; a single operator cannot outrun these |
| Append-only evidence, by design | `audit_events`, `fills`, `transactions`, `decision_snapshots`, `order_state_transitions`, `reconciliation_issue_events`, the four `*_history` tables | a trigger that raises on DELETE. These need an archive design, which is a separate decision |
| Grows with uptime and is deliberately NOT pruned | `reconciliation_runs` | nothing — see below |

### The table that looks like the obvious next sweep, and must not be

`reconciliation_runs` gets a row every five minutes whether or not anything
happened. It grows with uptime exactly like the three pruned tables, and adding
it to the sweep looks like finishing the job.

The foreign keys are why it is not:

	reconciliation_issues       -> reconciliation_runs    ON DELETE CASCADE
	reconciliation_issue_events -> reconciliation_issues  ON DELETE CASCADE
	fills                       -> reconciliation_issues  ON DELETE SET NULL

Deleting an old run deletes **the issues that run raised**. An unresolved issue
is an open halt and an operator's work queue. Rule 7's corollary says "not
re-detected" must mean "fixed", never "no longer examined" — and a retention
sweep that removed an unresolved issue would release an account's halt as a
side effect of housekeeping, which is the exact failure that corollary was
written after.

Confirmed rather than inferred: the DELETE was run inside a transaction and
rolled back. It fails with

    ERROR: table reconciliation_issue_events is append-only: DELETE is not permitted

so the database refuses it today. That trigger is a backstop catching the
second-order effect; the thing that makes the sweep wrong is the first-order
one. Bounding this table means first deciding what happens to a run's issues,
which is an archive design and not a line in a cleanup function. The reasoning
is recorded in `internal/store/retention.go` where the next person will look.

### The sweeps now have tests, and the tests were made to fail

`internal/store/retention_integration_test.go`, gated on `VANTAGE_STORE_E2E=1`,
runs as the **app role** — the role the scheduler uses, so a missing `DELETE`
grant fails here rather than hourly in production. The fixtures sit in 1999 and
2000 with the cutoff between them, far from any real row, because a prune is
not scoped to its caller's own data.

| Property | Executed |
| --- | --- |
| Old quotes go, recent quotes stay | pass |
| Old PUBLISHED outbox rows go | pass |
| **Old UNDELIVERED outbox rows stay** | pass |
| Recent published rows stay | pass |
| Pruning a run takes its signals | pass |
| A recent run and its signal stay | pass |

The two properties with consequences were then broken on purpose to confirm the
assertions are load-bearing:

- Removing `published_at IS NOT NULL` from the outbox sweep: **failed** with
  "an UNDELIVERED event was deleted. It is owed to a consumer and nothing will
  ever ask for it again".
- Relaxing the cascade to `NO ACTION`: **failed** with the constraint rejecting
  the write — which is precisely the production symptom, an hourly foreign-key
  violation logged and swallowed while `strategy_signals` grows for ever.

Both mutations were reverted and the cascade re-checked against the live
database before moving on. The full Go suite passes.

## 30aa. The fixture that was missing, and what it now demonstrates

Section 30z corrected ST-5 from "the two gates never intersect" to "they did
not intersect on these fixtures", and said the fix was a dataset rather than a
threshold. This is that dataset.

### What the other fourteen had in common

All of them. Every committed fixture produces breaks that penetrate the
Donchian channel by a small fraction of ATR, so `donchian_breakout` scored
0.31-0.44 on each and cleared the 0.55 floor on none. Fourteen datasets
agreeing looked like a property of the strategy. It was a property of the
generator: nothing in it ever produced a decisive move.

Measured, walking each fixture bar by bar through the real strategy and
`classify_regime`:

| Fixture | Actionable signals | Best confidence | Clear 0.55 AND regime-valid |
| --- | --- | --- | --- |
| `trend-clean` | 14 | 0.4413 | **0** |
| `false-breakout` | 0 | — | 0 |
| `decisive-breakout` (new) | 28 | **0.7500** | **3** |

`trend-clean` reproducing 0.441 independently is worth noting: it is the same
number the replay runs recorded, arrived at by a different route, which is what
makes the comparison in the third row mean something.

### The strongest instant, and the consensus verdict it produces

At bar 119 the five PAPER strategies say:

| Strategy | Action | Confidence | Regime gate |
| --- | --- | --- | --- |
| `macd_momentum` | buy | 0.5991 | valid in TRENDING |
| `donchian_breakout` | buy | 0.7500 | valid in TRENDING |
| `rsi_mean_reversion` | no_trade | — | RANGING only, gated out |
| `ma_trend_crossover` | no_trade | — | valid, no opinion |
| `bollinger_zscore_reversion` | sell | 0.6467 | RANGING only, **gated out** |

Two agreeing opinions above the floor, both valid in the prevailing regime, and
the one opposing opinion — which at 0.6467 would otherwise be disqualifying,
since the policy allows at most 20% of weight on the other side — excluded
because it declares itself valid only in RANGING.

Those exact numbers are pinned in
`internal/orchestrator/decisive_breakout_test.go` and fed to the real `Decide`
with `DefaultConsensusPolicy()` unchanged. **The verdict is an actionable buy,
with no vetoes.** A second test asserts the opposing sell appears in the
contributions as discarded rather than being silently absent, and that its
weight never reached the tally — because an operator asking "why did it trade?"
needs the discards as much as the survivors.

### What this does not show

An order. Position sizing, the eighteen risk checks, trading authority,
Autopilot, exposure ceilings and the daily-loss budget all sit downstream of
the verdict, and only a replay through the real scheduler exercises those. That
replay needs a running stack, a reseed and the four preconditions in section
30-something; it has not been run, and this section claims the decision and
nothing past it.

The fixture is 15.7 KB, generated by the committed script with no RNG, in the
`week(28)` slot so it overlaps no other dataset's dates. Regenerating produced
byte-identical output for the other fourteen, which is the check that the
generator is still deterministic.

### A stale number found on the way

Rule 15 described "the fourteen replay fixtures ... 170 KB total". The
directory held fourteen fixtures totalling 212 KB. The figure had drifted
before this work started, and nobody recomputes a number in a rule. It now
reads fifteen and 227 KB, both measured.

## 31. What is NOT verified

Stated plainly, because a report that lists only successes is not useful.

- **No strategy has been validated against real market data.** Every measured
  result in this repository rests on `SYNTHETIC_CONTROLLED` bars, where the
  generator and the strategies share a model of what a trend is. The
  acquisition path, ingestion, quality validation, partitioning, snapshots and
  comparison are built and tested; the data is absent because no provider API
  key is configured. Status: `TWELVE_DATA_CONFIGURATION_REQUIRED`.
- **The Twelve Data provider has never made a real request.** Every test runs
  against a local `httptest` server. Parsing, chunking, rate limiting, retry
  and error handling are exercised; the live API's behaviour, plan limits and
  earliest available XAUUSD bar are unknown and are not guessed at anywhere in
  this repository. What changed this revision is the *shape* of that gap: the
  acquisition path now has sixteen integration tests driving the real `Syncer`
  and the real store against a fake built to the published contract, so a first
  real response will fail in the parsing if it fails at all, rather than
  somewhere deeper. Section 30n.

- **The confidence scale and the policy's thresholds have never been
  calibrated against each other, and on the committed fixtures they do not
  intersect.** Corrected twice. The first revision recorded this as "no
  strategy fires": 266 of those 130-odd signals were strategies handed fewer
  bars than they require, recorded as opinions (section 30o). With that fixed
  the same replay produces 46 actionable signals -- and still no order, because
  the only strategy clearing the 0.55 floor is regime-gated out of a trend and
  the trend-valid ones peak at 0.538 and 0.441 against it.

  The second correction is that this is a fixture property and was being
  written as a structural one. A constructed TRENDING market with a 1.0x ATR
  break gives `donchian_breakout` 0.750, above both thresholds and valid in
  that regime, with nothing in the policy touched. The committed datasets do
  not contain a decisive break, which is a gap in the data rather than in the
  decision layer.

  What remains true: the score is a raw average with hard-coded constants and
  is not a probability of anything, so calibrating it against realised outcomes
  is still research and still needs evidence. Moving the threshold would
  manufacture trades rather than earn them, and no threshold has been moved.

- **Four replay fixtures are too short for the strategies they exercise, and
  are now declared so.** `drawdown`, `false_breakout`, `spread_spike` and
  `test_partial_fill` do not reach the 120 bars the hungriest PAPER strategy
  requires, so no strategy can signal on them at any point. They remain the
  right fixtures for the execution and recovery scenarios they were built for
  and are NOT evidence about strategy behaviour.
  `services/quant/tests/test_fixture_evidence.py` enforces the distinction: a
  fixture is either long enough or named with its reason, and the declaration
  is checked in both directions so an entry cannot outlive the fixture it
  describes. Lengthening any of them stays a separate decision with its own
  constraint -- each fixture needs its own date range.

- **Nothing refreshes an FX rate, so a rate goes stale 24 hours after a
  seed.** The platform now REFUSES rather than valuing a position at zero
  (section 30r), which closes the dangerous half. The rate still does not
  refresh: a real source is a provider behind the `fx.RateSource` seam and this
  build may not contact a third party. Re-stamping a held rate would make the
  symptom vanish by fabricating data.

- ~~**There is no retention anywhere except the session sweep.**~~ **Closed
  for the prunable tables** (section 30s): `market_quotes` and
  `market_data_health` at 30 days, `strategy_runs` at 14, published outbox rows
  at 7, batched at 50 000 per run. What REMAINS open is the append-only set -- `market_quotes`
  gains roughly 179,000 rows a day -- about 27 MB, 10 GB a year -- at the 2-second
  ingest interval across six instruments. `reconciliation_runs` gains 288 a day
  per account. `outbox` and `command_idempotency` are never pruned, the latter
  carrying an index on `created_at` that was plainly added for a job nobody
  wrote. Several tables (`audit_events`, `decision_snapshots`, `transactions`,
  `fills`) carry append-only triggers and cannot be deleted from at all, so they
  need an archive design rather than a cleanup job. `docs/COMPLIANCE_READINESS.md`
  already records retention as Not built; this quantifies it.

- ~~**`strategy_runs` still grows unbounded under a persistent refusal.**~~
  **Closed** (section 30s): the three post-bar-load refusals now key on a bar,
  and the retention sweep bounds the pre-flight ones that genuinely cannot.
  Formerly: Only
  the history-refusal path was given a bar time, so the per-bar unique index
  deduplicates that one. Every other pre-flight refusal -- reconciliation
  blocked, feed unhealthy, authority expired, kill switch active, research
  service down -- still records with a nil bar time, which the partial index
  cannot dedupe and `PurgeStrategyRunsInRange` cannot delete. A halted account
  writes about 23,900 rows a day.

- ~~**`vantage_marketdata_latest_bar_age_seconds` is defined and never
  set.**~~ **Closed** (section 30s): set in `Aggregator.AggregateInto` from the
  bar's close against the quote's source time. Still open from the same entry:
  Rule 9 exists because the bar series silently froze once; the one gauge that
  would catch it recurring has no setter in production code. There is also no
  gauge for FX rate age and no heartbeat for "last successful strategy pass",
  and readiness checks database, brokers, quant and the reconciliation verdict
  but not market-data freshness.

- ~~**Nothing warns before a trading authority expires.**~~ **Closed**
  (section 30t): warnings at 30, 7 and 1 day, and a critical alert once lapsed
  that names the expiry as the reason nothing is trading. Formerly: The dev seed grants
  three years. On the day it lapses, `ActiveAuthorityForAccount` still returns
  the row, `Effective` refuses, the scheduler's strategy loop silently
  `continue`s without logging, and the platform stops trading with no alert and
  no readiness signal.

- ~~**`GET /admin/audit/verify` stops verifying past 10,000 events.**~~
  **Closed** (section 30t): it pages the whole chain, checks the link across
  each page boundary, and reports `verified` only when the walk was complete.
  Formerly: It checks
  sequences 1…10000 and reports `verified: true` whatever `head_sequence` says,
  so the most recent history -- where tampering would be -- is never examined.

- **Accessibility is not tested at all.** No axe run, no keyboard-navigation
  test, no screen-reader pass. The terminal is a single-operator tool, which is
  a reason it has not been prioritised and not a reason to claim anything about
  it.

This list is shorter than it was -- six items from the previous revision have since
been executed and moved into the tally -- and what remains is what remains.

- ~~**The CI workflows have never run.**~~ **Closed on 2026-09-30.** The
  repository was published and both workflows are green. What the entry always
  warned about turned out to be exactly right: `actionlint` passed with 0
  findings throughout, and the first real run failed with six defects it cannot
  see -- an unrecognised environment value, a stricter mypy, a deleted upstream
  tag, a removed CLI flag, and two ways of mis-auditing a local package.
  Section 30q.
- **OSV-Scanner and pip-audit were not run.** Neither is installed on this
  machine. Both are configured in CI, and CI has never run -- see the note
  below on what pinning the workflows revealed about that configuration.
- **No penetration test** against a deployed instance. The ZAP baseline is a
  passive scan of localhost, which is a much weaker claim.
- **`httpapi` HANDLERS are still only covered end to end.** The layer beneath
  them now has direct tests -- the error envelope, the request decoder, the
  address and trace-id helpers, the configuration digest -- and `ratelimit`
  has its own. But a handler needs a database, a broker and a keyring to
  construct, so the handlers are exercised by the smoke and Playwright suites.
  Every package named in the previous revision's untested list now has tests.
- **The scenario matrix A-T is now driven end to end**, with business
  invariants rather than a PASS. The market shapes (A clean trend, B range, C
  volatility shock, E spread spike, H drawdown, K trend reversal, L false
  breakout, M correlated opportunities, T day boundary) come from committed
  fixtures; the failure and recovery cases (F outage, J kill switch, O stale
  quote after reconnect, P partial fill, Q lost broker response, R duplicate
  tick, S restart) drive the mock venue's fault modes through the real
  pipeline; and the CONDITION cases (D high-impact release, G conflicting
  strategies, I strong signal with no risk capacity, N news alongside
  agreement) apply a condition to an ordinary market.

  **Every scenario previously stepped 60 instants, which the warm-up model
  silently turned into an evaluation window of ZERO.** Warm-up is 60 instants
  and produces no executable intent by design, so eight of the nine market
  scenarios were asserting on nothing. They now step their dataset to the end.
  This was introduced by the isolation work and found by this milestone.

- **The four condition scenarios, measured.** Each on its own freshly seeded
  fixture, because the 15 ZAR daily-loss limit is spent long before a
  thirteen-dataset matrix finishes and every later refusal then has nothing to
  do with what the scenario is testing.

  | | Result | What it measured |
  | --- | --- | --- |
  | D high-impact release | PASS | 67 decisions taken inside a blackout and 67 clear of one. 39 refused naming `event_risk_blackout`; 28 accepted, every one a reducing order against an open position. No decision whose `event_risk` check FAILED was accepted |
  | G conflicting strategies | **FAIL, deliberately** | 38 instants split the strategy set and **all 38** produced filled orders on both sides of XAUUSD.m. See the consensus finding below |
  | I no risk capacity | PASS | 134 decisions, 132 exposure refusals, 1 accepted -- and that one opposed the net exposure it saw, so exposure never grew |
  | N news alongside agreement | PASS | 134 decisions with 79 news items published across the window, ledger gapless and balance reconciling |

  The contrast between G and I is the useful part: on `capacity-exhausted` the
  strategy set also split 38 times and produced **zero** both-sided fills,
  because the tightened ceilings refused the opening leg. The controls that
  exist do bind; the one that does not exist does not.

- **The consensus policy is not on the execution path.**
  `orchestrator.Decide` -- which this repository's own map describes as "the
  multi-strategy aggregation policy: a PURE function, versioned, vetoes before
  votes, and never a majority vote" -- is implemented, unit-tested by the ten
  decision-layer scenarios, and **has no production caller**. The scheduler
  calls `EvaluateAndRoute` once per (strategy, instrument) and each call routes
  its own signal into the OMS independently, so two strategies disagreeing at
  one instant are not aggregated at all. Scenario G therefore asserts the
  invariant that holds under any policy -- the platform must not end one
  instant holding orders on both sides of one instrument -- rather than
  assuming a policy that does not run.

- **News is stored and displayed but is not an input to any decision.**
  Nothing in the orchestrator, the OMS or the risk engine reads `news_items`.
  Scenario N asserts consistency rather than influence, and says so in its own
  failure text.
- **A replay run's code identity is only as good as the build.** The record
  is now persisted (`replay_runs`, migration 0013) and carries the dataset
  hash, code SHA, config hash and seed. But a `go run` binary has no commit
  identity, so `code_sha` reads `unknown` -- and two such runs COMPARE EQUAL on
  it while establishing nothing. `Start` records a durable warning saying the
  comparison cannot be made; a container build stamps a real SHA. This is
  recorded rather than fixed because the honest answer for an uncommitted
  working tree is that there is no identity to record.
- **Speed invariance is now MEASURED, not argued.** STEP, 1x, 10x and MAX over
  the same 140-instant dataset from one snapshot all produce digest
  `f986a6b0790bb1fc` -- identical in all seven sections, 46 decisions, 46
  orders, 42 fills, 21 ledger entries -- while wall time ranges from 54s to
  6m07s. 1x and 10x take the same wall time because the per-instant pacing
  sleep is capped at two seconds and every finite speed exceeds the cap on 1h
  bars; that is the cap working, and it means the speed names overstate what
  they control on long timeframes. The hex is tied to the commit that measured
  it -- decisions have since started recording their bar, which changes the
  canonical form -- so what carries forward is the property, one digest across
  all four modes, not the value.
- **Walk-forward, sensitivity and Monte Carlo are library-only.**
  `vantage_quant.backtest` implements `walk_forward`, `sensitivity`,
  `cost_sensitivity` and `monte_carlo_trade_order`, and they have Python tests
  -- but none is exposed as an HTTP route, so the control plane cannot run one
  and no result is stored or displayed. Usable from a notebook, invisible to
  the platform.
- **ML validation stops short of calibration.** Precision, recall, ROC-AUC,
  Brier and a majority-class baseline are computed and a model that does not
  beat the baseline is flagged. F1, PR-AUC, a confusion matrix and an actual
  calibration curve are not, so "0.80" is documented as a score rather than
  demonstrated to be a probability.
- **Model attribution is still missing.** P&L is attributed by instrument,
  strategy, strategy version, source, session, event context, transaction
  type, replay run, PAPER_FORWARD-versus-REPLAY and now REGIME -- folded from
  the ledger so the buckets reconcile to the account exactly. Measured across
  the scenario matrix: 23 of 23 ledger entries in every dimension, discrepancy
  0.00. No decision records the MODEL that informed it, so that one dimension
  cannot be served and is not offered.
- **No backtest versus paper-forward comparison.** Both engines exist and both
  produce metrics; nothing compares them, so the differences caused by the OMS,
  spread, slippage, latency, the scheduler and reconciliation are unmeasured.
- **Correlation is measured and feeds risk.** Rolling Pearson over bar-to-bar
  returns, paired by timestamp, with four states rather than a number: KNOWN,
  STALE, INSUFFICIENT_DATA, UNDEFINED. `CheckPortfolioCorrelation` reduces
  above 0.60 and refuses above 0.90 on the strongest single relationship. An
  unmeasurable pair reduces to 0.75 and never refuses, and says the reduction
  rests on an absence of evidence. Verified live: the matrix refreshes each
  replay step and the check is recorded on every decision.
- **RISK_OFF is inferred and the regime is recorded.** `domain.InferRegime`
  reasons over feed health, spread absolute and relative, event proximity,
  drawdown and provider stability; every verdict records every check including
  the ones that passed. Hysteresis is asymmetric -- immediate into RISK_OFF,
  three confirmations out -- and both named transitions are tested with a
  noisy boundary. The regime is stored on the decision (0015) and never
  recomputed.
- **ML evaluation is unchanged.** No F1, PR-AUC, confusion matrix or
  calibration curve, so a 0.80 output remains a score rather than a
  demonstrated probability. Promotion gates are not formalised beyond the
  existing lifecycle states.
- **Walk-forward, sensitivity and Monte Carlo are still library-only.** No HTTP
  route, so the control plane cannot run one and no result is persisted.
- **RISK_OFF is nameable but never inferred.** No classifier produces it.
- **Correlation is still not measured.** The consensus policy accepts a
  portfolio veto and is tested against a correlated-exposure block, so the
  mechanism to act exists -- but nothing computes that gold and silver are
  correlated, so the block is never raised. The plumbing is in place and the
  measurement is not.
- **Reconciliation cannot repair everything, by design.** Nine of thirteen
  issue types require an operator, and two of those -- position and balance
  mismatch -- can never be repaired automatically at all. That is the intended
  behaviour, not a gap. The real remaining limits are narrower: a venue that
  reports a plausible but false execution matching exactly one local order
  would be repaired automatically, since no design can distinguish a correct
  venue from a convincingly wrong one without a second source; and the
  automatic repair set was chosen against MockBroker's semantics, so a real
  provider with different identifier guarantees would need it re-derived
  rather than inherited.
- ~~**No scheduled backups**~~ **Closed for scheduled logical backups**
  (section 30v): a daily `pg_dump` with verification, retention, and a restore
  proven end to end including an audit-chain check on the restored copy. What
  remains open from this entry is WAL archiving, so the recovery point is the
  last daily dump rather than the last transaction. Off-site copying is closed
  in section 30w: each verified dump is encrypted to a key whose private half
  never reaches the server, and copied to any rclone remote. Formerly:
- **No WAL archiving.** The restore drill passes but
  is run by hand, so the recovery point is the last manual dump and nothing
  would notice a backup that silently began producing an unusable file.
- **No distributed tracing.** Correlation ids are propagated, which is what a
  single-operator system needs day to day, but this is a gap rather than a
  decision worth defending.
- **The research image carries 54 unfixable HIGH/CRITICAL OS findings.** No
  Debian fix exists for any of them; a minimal base is the real answer. See
  section 27.
- **`docker compose up` for the whole stack** was not exercised end to end; the
  images were built and scanned individually, and the services were run
  directly.
- **The mock venue is now adversarial but still finite.** Twelve deterministic
  fault modes -- including a lost response after acceptance, which is what
  exposed defect 1's severity -- but it does not contradict itself the way a
  real venue occasionally will. A passing mock suite is not evidence that a
  live integration is safe.
- **Correlation between instruments is not modelled.** Gross exposure and
  concentration are per-instrument, so gold and silver count as
  diversification, which they are not. Named here and in
  `docs/RISK_ENGINE.md`.
- **No claim is made about profitability.** The research tools measure whether
  an approach works and report honestly when it does not; on a R500 account the
  common outcome is NO TRADE.

---

# Appendix A -- completion matrix

Read the status column strictly. A feature is **COMPLETE** only where the
usable behaviour exists AND has tests that would catch its removal. An
interface, a route, a type or a placeholder does not count, and neither does
"the code looks right".

Where a subsystem is exercised only end-to-end, it says so. That is real
coverage, but it is not the same as a unit test, and a reader deciding what to
trust needs the difference.

| # | Subsystem | Status | Evidence, and what is missing |
| --- | --- | --- | --- |
| 1 | Paper-only enforcement | COMPLETE | Four independent gates: compile-time constant, config validation, no live adapter compiled in, DB CHECK constraints. `arch_test.go` asserts the constant is still compiled in; `config` tests cover the refusal; smoke asserts `execution_mode: paper` |
| 2 | Single execution path (OMS) | COMPLETE | `arch_test.go` proves only `oms.go` calls `adapter.PlaceOrder`, that the adapter is held by an allowlist of packages, and that exactly two call sites reach the OMS -- the HTTP handler and the orchestrator |
| 3 | Research cannot reach a broker | COMPLETE | Asserted from the `go list` dependency graph, not from convention: `TestTheQuantBridgeCannotReachExecution`. The research DB role is additionally `default_transaction_read_only = on` |
| 4 | Order idempotency | COMPLETE | `PRIMARY KEY (account_id, idempotency_key)` plus a unique index on orders. 16 simultaneous identical submissions yield exactly one row, verified in SQL rather than from the API response |
| 5 | Order state machine | COMPLETE | Transition table in `domain`, 5 test files. Illegal transitions now answer 409 instead of 500 |
| 6 | Risk engine (20 checks) | COMPLETE | 31 unit tests. Every failing check is reported, not just the first. Reducing orders are exempt from the exposure family; an over-closing side flip is not treated as reducing |
| 7 | Money arithmetic | COMPLETE | `decimal` throughout, no float constructor (`TestMoneyHasNoFloatConstructor`), no float columns in the schema (`TestNoFloatingPointColumnsInTheSchema`). Frontend floats are presentational only and never round-trip into stored state |
| 8 | Ledger (append-only, gapless) | COMPLETE | Append-only triggers, per-account sequence. Gaplessness and balance-equals-running-total asserted in SQL after deliberate concurrency, and again after a restore |
| 9 | Audit hash chain | COMPLETE | Advisory-locked single writer, `verify-audit` command, verified against a restored database as well as a live one |
| 10 | Kill switch | COMPLETE | Scoped switches. Smoke asserts it halts new orders and does NOT liquidate; the race suite asserts the settled state refuses everything after activation |
| 11 | Trading authority | COMPLETE | Scoped mandate, revocation, restoration. The race suite covers revocation during in-flight submission |
| 12 | Authentication | COMPLETE | Argon2id, session cookies, MFA enrol/verify/disable, recovery codes, rate limits. 9 Playwright tests plus unit tests |
| 13 | Authorisation (RBAC) | COMPLETE | Viewer/trader/admin with separation of duties (an admin cannot trade). Asserted against the API directly, not only through the UI |
| 14 | CSRF, CORS and headers | COMPLETE | Double-submit CSRF, strict origin allowlist, COEP/COOP/CORP. ZAP baseline 0 FAIL on both surfaces |
| 15 | Content-Security-Policy | PARTIAL | Complete except `script-src 'unsafe-inline'`. A nonce policy was implemented and reverted: Next 16's Turbopack emits chunk tags without the nonce, so `strict-dynamic` blocked every script and the terminal rendered nothing. Documented with the exact change to make when that lands |
| 16 | Reconciliation -- detection | COMPLETE | 13-type taxonomy, snapshot-based, classified by a pure function with 34 unit tests. Fingerprint deduplication with a partial unique index; stale issues closed when they stop being detected |
| 17 | Reconciliation -- repair | COMPLETE | Four provable types repaired automatically through the same accounting path an ordinary fill uses; a separate repair state machine that cannot leave a terminal state; every repair stamped and attributed to its issue. Crash-recovery, idempotence, concurrent-run and ambiguity tests all pass |
| 17a | Reconciliation -- operator control | COMPLETE | Seven ADMIN-only actions with mandatory reasons, full issue history, failed attempts audited, no "set order status" endpoint. 21 Playwright tests including forged ids, cross-account ids, mass assignment and repeat resolution |
| 17b | Trading verdict and halt scope | COMPLETE | HEALTHY / DEGRADED / TRADING_HALTED / RECONCILIATION_REQUIRED derived from real state and surfaced in readiness; halt scope per issue type with the narrowest blast radius that contains it; manual read-only use unaffected during an incident |
| 18 | Unknown-outcome handling | COMPLETE | `ErrUnknownOutcome` is distinct from rejection; FAILED means "outcome unknown" and is never retried. Proven with a deterministic lost-response fault, including that the order stays FAILED |
| 19 | Deterministic broker faults | COMPLETE | 12 modes with exact firing counts, never probabilistic. 9 unit tests; all 12 armed through the development endpoint in a Playwright test |
| 20 | Mock venue | COMPLETE | Its own tables and transaction, spread, slippage, partial fills, order-book state |
| 21 | Live broker adapter | **INTENTIONALLY DEFERRED** | Not written and not compiled in. This is the safety property, not a gap |
| 22 | Market data ingestion | COMPLETE | Provider abstraction, polling ingestor, bar aggregation |
| 23 | Market data quality gates | COMPLETE | Staleness, timestamp regression, duplicate detection, spread ceiling. Unit tested in `domain`; the refusals are asserted in smoke |
| 24 | Economic calendar | COMPLETE | Provider interface, ingestion, and an event blackout that binds automated orders. Unit tested in `econdata` |
| 25 | News feed | COMPLETE | Provider interface, ingestion, labelled as development fixtures in the API response |
| 26 | Portfolio valuation | COMPLETE | Exercised by every smoke run and by equity history, and the exposure arithmetic is now extracted into a pure function with 7 unit tests covering an unpriceable position, a hedged book, cross-instrument currency netting and the empty-currency zero |
| 27 | Equity history and attribution | COMPLETE | Recorded per snapshot, exposed and rendered |
| 28 | Strategy library (12) | COMPLETE | Implemented in the research plane with tests; 5 promoted to paper by the seed |
| 29 | Strategy orchestrator | COMPLETE | Evaluates and routes through the same OMS as a manual order (asserted structurally), concurrent runs covered by the race suite, and 49 unit tests over the consensus policy and the ten scenarios |
| 29a | Multi-strategy consensus | COMPLETE | A pure, versioned aggregation policy: vetoes before votes, no majority resolution of a disagreement, abstentions not counted as agreement, an unavailable model never read as assent. 35 tests |
| 29b | Market regimes | COMPLETE | Seven canonical regimes in `domain`, fail-closed parsing, UNKNOWN never coerced. **RISK_OFF is nameable but no classifier infers it** -- recorded as a gap |
| 29c | Bar aggregation from the live feed | COMPLETE | 15m/1h/4h from the quote stream; complete bars only to strategies, mid not bid/ask, venue-timestamp bucketing, late ticks dropped, restart continuation. Was entirely MISSING and is the milestone's headline defect |
| 29d | Deterministic market replay | COMPLETE for the decision layer, **PARTIAL** end to end | Provider plus five series generators, no look-ahead by construction, ten scenarios A-J reproducible. Does not yet drive ingestion, the OMS and the ledger -- that needs config plumbing in `internal/app` |
| 29f | Market replay -- end-to-end pipeline | COMPLETE for one dataset | `Scheduler.ReplayStep` calls the real jobs; no second trading engine, enforced by an architecture test. Measured: 665 signals, 250 orders, 189 fills, 64 ledger rows, gapless ledger, stored balance equal to the derived one |
| 29g | Market replay -- determinism | COMPLETE | Two runs from a byte-identical database produce byte-identical financial output. Required seeding the mock venue, which was seeding slippage from the wall clock |
| 29h | Market replay -- datasets and controls | COMPLETE | Nine committed fixtures with parsed-row hashes, an allowlist registry (never a path), admin-only start/step/advance/pause/resume/reset/stop/speed/reconcile, reason required and audited for start and stop |
| 29i | Market replay -- scenario coverage | **PARTIAL** | One of twenty scenarios (A) is driven end to end. The rest remain decision-layer tests; the infrastructure to move them exists |
| 29j | ReplayRun persistence | COMPLETE | `replay_runs` (0013). Written before the run is announced and updated as it plays, so an interrupted replay still leaves a trace; a run whose opening record cannot be written is refused, while a later recording failure never destroys a finished run. `simulated` cannot be false, and the app role cannot DELETE |
| 29k | Replay run history API | COMPLETE | Admin-only `GET /replay/runs` and `/replay/runs/{id}`, answering in every process because a recorded run is evidence whether or not the engine is present |
| 29n | PAPER_FORWARD separated from BACKTEST/REPLAY | COMPLETE | `orders.replay_run_id` (0014) tags every order created while a replay owned the clock, including a manual one. `?by=run_kind` and `?by=replay_run` read it. Without the tag a replay's numbers and a forward session's sit in one account indistinguishably |
| 29l | P&L attribution across dimensions | COMPLETE for nine of eleven | Instrument, strategy, strategy version, source, session, event context and transaction type. Folded from the ledger, so every entry is counted exactly once; the report compares itself against the account's own totals and reports itself unreconciled rather than presenting a truncated window as the account. Regime and replay run are NOT attributed |
| 29m | Attribution reconciliation | COMPLETE | Measured on a replay: 25 of 25 ledger entries in every one of the seven dimensions, discrepancy 0.00, and 500.00 funding plus -20.66 trading equal to the ledger's 479.34 |
| 30a | RISK_OFF inference with reasons | COMPLETE | Six components, every one recorded whether it fired or not. Two ordinary reasons make RISK_OFF; an invalid feed or a spread beyond five times expected is decisive alone |
| 30b | Regime hysteresis | COMPLETE | Asymmetric by design: immediate in, three confirmations out. A contradicting observation resets the count, so an oscillation never accumulates |
| 30c | Regime recorded at decision time | COMPLETE | `decision_snapshots.regime` + policy version + reasons (0015). Never recomputed: moved thresholds would reattribute historical P&L to conditions the platform never acted in |
| 30d | Rolling correlation | COMPLETE | Four states, never a silent zero. 29 tests covering +1, -1, near zero, insufficient samples, non-overlapping series, a flat series, changing correlation and stale data |
| 30e | Correlation-aware risk | COMPLETE | REDUCE before REJECT, strongest relationship rather than an average, absolute correlation by default |
| 30f | Scenario matrix through the real pipeline | COMPLETE for 9 of 20 | A, B, C, E, H, K, L, M, T drive ingestion → bars → strategies → orchestration → risk → OMS → venue → booking → ledger → audit, each with business invariants. D, F, G, I, J, N-S are not yet driven |
| 29e | Autopilot global switch | COMPLETE | Default OFF, ADMIN to change with a mandatory reason, any role to read, append-only history, enforced in the order transaction with its own rejection code, distinct from the kill switch |
| 30 | Signal to order routing | COMPLETE | `TestEveryOrderPlacementGoesThroughTheSameOMSMethod` pins the two permitted call sites |
| 31 | Backtesting engine | COMPLETE | Next-bar fills, stop assumed on an ambiguous bar, full costs, unaffordable trades counted rather than dropped. Rejects non-ascending bars |
| 32 | Look-ahead prevention | COMPLETE | 13 indicator series property-tested on truncated prefixes; the backtester feeds strict prefixes. One documented caveat: `support_resistance` is window-dependent and currently has no callers |
| 33 | ML training pipeline | COMPLETE | Chronological splits, an embargo now required to cover the label horizon, majority-class baseline, Brier score, full reproducibility record |
| 34 | ML leakage prevention | COMPLETE | Feature look-ahead property test, labels dropped rather than imputed, holdout scored once. The embargo-narrower-than-horizon hole was found by this audit and closed |
| 35 | Model drift detection | COMPLETE | Distribution-shift detection with unit tests |
| 36 | Model artefact storage | COMPLETE | Versioned on disk, deliberately not committed |
| 37 | Scanner | COMPLETE | Ranks candidates and states that it does not execute. Covered by the research smoke suite |
| 38 | Alerting | COMPLETE | All seven required sources wired, per-(kind, key) cooldowns, suppressed-repeat counts, one-shot recovery. 8 unit tests. Sinks are slog, Prometheus and a notifications row -- no external SaaS dependency |
| 39 | Notifications | COMPLETE | Stored, listed, markable as read |
| 40 | Metrics | COMPLETE | Prometheus counters and gauges across the pipeline, including a deadlock counter that must stay at zero |
| 41 | Structured logging | COMPLETE | slog with request and correlation identifiers |
| 42 | Distributed tracing | **NOT IMPLEMENTED** | No OpenTelemetry. Correlation ids are propagated, which is what a single-operator system actually needs, but this is a gap rather than a decision worth defending |
| 43 | Health and readiness | COMPLETE | Live and ready endpoints with per-dependency status |
| 44 | Rate limiting | PARTIAL | Redis-backed, per-operation budgets, enforced and demonstrably effective -- it refused this audit's own test traffic. The package has no unit tests |
| 45 | Database migrations | COMPLETE | Idempotent, owner-role only, with a status command. CI re-applies them to prove the second run is a no-op |
| 46 | Least-privilege DB roles | COMPLETE | Owner, application and research roles. The application role cannot perform DDL; the research role cannot write at all |
| 47 | Lock ordering | COMPLETE | Declared in `store.LockAccountTx` and taken as the outermost lock. Added by this audit after a real deadlock; a counter and a race test guard the regression |
| 48 | Backup and restore | COMPLETE | `scripts/backup-restore-drill.ps1` performs the whole cycle and verifies the financial integrity of the restored data. Executed and passing |
| 49 | Disaster-recovery procedure | COMPLETE | Documented per failure mode, including the two corrections the first drill produced |
| 50 | Scheduled backups | COMPLETE | Daily `pg_dump` in the production overlay, each archive read back with `pg_restore --list` before it counts, retention over verified dumps only. Restore proven end to end, including `verify-audit` reporting yes on the restored copy. NOT off site, and no WAL archiving: the recovery point is the last daily dump |
| 51 | Terminal (19 routes) | COMPLETE | Every route renders with live data; 30 Playwright tests |
| 52 | Order ticket | COMPLETE | Review before confirm, duplicate-click protection, risk explanations rendered |
| 53 | Containers | COMPLETE | Three images, non-root, no HIGH or CRITICAL misconfigurations |
| 53a | Walk-forward, sensitivity, Monte Carlo | **PARTIAL** | Implemented in `vantage_quant.backtest` with Python tests, but exposed by no HTTP route -- the control plane cannot run one and no result is stored or displayed |
| 53b | ML validation depth | **PARTIAL** | Precision, recall, ROC-AUC, Brier and a majority-class baseline, with a warning when the model does not beat it. No F1, PR-AUC, confusion matrix or calibration curve, so a 0.80 output is a score rather than a demonstrated probability |
| 53c | P&L attribution | PARTIAL | Eight dimensions -- instrument, strategy, strategy version, source, session, event context, replay run and regime -- folded from the LEDGER so each unit of money is counted exactly once, with an explicit UNATTRIBUTED bucket rather than a silent drop. **Model is still not a dimension**, so "which model made the money?" remains unanswerable |
| 53d | Replay determinism | COMPLETE | A canonical result digest over seven sections, ordered by business keys only. Three runs of one dataset from a `pg_dump`/`pg_restore` snapshot produce one digest; the suite SKIPS rather than passes when a dataset produced no decisions of its own. Found and fixed the mock venue's unreseeded jitter. Re-measured at the final code state after decisions began recording their bar -- a richer canonical form that could have exposed hidden non-determinism and did not: `trend-clean` `45f2cf72fc4a0397` ×3, `correlated-pair` `1638d574c83ea41d` ×3, `range-bound` skipped as vacuous |
| 53e | Replay speed invariance | COMPLETE | STEP, 1x, 10x and MAX over the same dataset produce one digest. Paced modes run through the background `advance` control, because the pacing sleep happens inside a step request and no batch size stays under the 30s handler deadline |
| 53f | Restart safety | COMPLETE | A real process kill, not an in-process reset. A replay STOPS and requires an explicit operator resume from a verified durable cursor; nothing financial is created or destroyed; no bar is evaluated twice; a half-filled order is not re-booked. Crash TIMING is covered through the venue's deterministic fault modes plus a real kill: an unknown outcome (`timeout`) must gain no fill and must not become FILLED, and a lost execution (`lost_response`) must be imported exactly once or left as an open issue. Killing inside the OMS transaction itself is still **not** covered -- that needs a fault that blocks at a named point |
| 53g | Partial-fill coverage | COMPLETE -- measured `TEST_XAU buy 0.1000 filled=0.0400 PARTIALLY_FILLED`, and `PARTIALLY_FILLED` with 0.0400 filled again after a real process kill | Was impossible: every order the R500 account produces is 0.01 lots, which is XAUUSD.m's minimum AND its step, so 40% of one order is not a representable quantity. A development-only synthetic instrument with a finer step makes 0.10 split into 0.04 and 0.06, within the authority's existing ceiling. Nothing under test was relaxed |
| 53h | Multi-strategy consensus | **WIRED** | `orchestrator.EvaluateInstrument` is `Decide`'s production caller: the scheduler groups by instrument, evaluates every applicable strategy with `Execute:false`, aggregates, and places at most ONE order, guarded durably by a `consensus-<instrument>-<bar>` idempotency key that names no strategy. Measured: 38 instants split the strategy set, 0 produced orders on both sides, against 38 of 38 before. The verdict, its vetoes, every contribution including the discarded ones, and the attribution policy are recorded on the decision snapshot. NOTE: under the default policy nothing clears the 0.55 confidence floor, so the platform places no autonomous orders at all -- see 30g finding 12 |
| 53i | Trading authority ceilings | **ENFORCED** | All five bind as their own pre-trade checks with their own rejection codes (`authority_max_order_quantity`, `authority_max_order_notional`, `authority_max_position_exposure`, `authority_daily_loss`, `authority_max_leverage`). The tighter of the authority and the account's own limit binds; the authority can only narrow. A strictly reducing order is exempt from all five, an over-closing position flip is not. Boundary-tested below, at and above each ceiling; 66-check smoke suite green |
| 54 | CI workflows | PARTIAL -- **cannot be verified locally** | `actionlint` passes with 0 findings and every command the workflows run has been executed by hand. GitHub Actions itself has never run: this repository has no remote. Nothing here may be read as "CI passed" |
| 55 | Regulatory and live-trading readiness | **BLOCKED** | Deliberately. No FSP licence, no client-money segregation, no live venue agreement, no independent audit. Recorded in `COMPLIANCE_READINESS.md` and `REGULATORY_BOUNDARY.md` |

## The unit-test gap, stated plainly

This was the largest piece of technical debt in the previous revision: five
high-risk packages with no unit tests at all. Four of them now have some.

- `oms` -- 10 tests. `handleBrokerError` was the specific worry: a six-branch
  decision table with three branches reached by no test. It is now a pure
  function, `ClassifyBrokerError`, and all six branches are covered, including
  an explicit test that an unrecognised error fails closed to UNKNOWN.
- `reconcile` -- 34 tests over the classifier, all reachable because it takes
  two snapshots and does no I/O.
- `store` -- 15 tests over `mapError` and `Fingerprint`, the two pure decisions
  in the package. `mapError` decides whether every database failure becomes a
  404, a 409, a retry or an opaque 500; the deadlock branch is covered
  specifically, because it was once an unclassified 40P01 surfacing as a bare
  500 on order placement.
- `portfolio` -- 7 tests, after extracting the exposure arithmetic from
  `Compute`, which read six tables before reaching it. The cases that were
  never tested were the ones nobody wants to seed: an unpriceable position must
  not count as flat, gross and net must disagree for a hedged book, and
  currency exposure must net across instruments while gross does not.

Still with none at the time of that revision: `orchestrator`, `httpapi`,
`fx`, `ratelimit`, `marketdata`, `quant`, `scheduler`. All seven have tests
now -- see section 19a.

The reason integration coverage was not simply extended instead: it covers the
paths the tests happen to take. But the reverse is also true and worth keeping
in view -- this audit's defects were found by integration tests against a real
database and a real venue simulator, and three of them were arbitrated by
PostgreSQL. A fake store would have agreed with whatever the test author
imagined. Both kinds of test earn their place; neither replaces the other.

---

## Test and scan tally

**Read the "Run" column.** Rows marked *this milestone* were executed against
this milestone's code. Rows marked *carried* were executed in an earlier
milestone and have NOT been re-run here -- they are reproduced because they were
true when measured, not because they were measured again, and a reader deciding
what to trust needs that difference. The stack-dependent suites are the ones
most affected: the development stack spent this milestone in REPLAY mode, which
puts the whole process on dataset time, and the smoke and Playwright suites
need real time.

| Suite | Run | Result |
| --- | --- | --- |
| Go unit | this revision | **874 pass, 0 fail, 54 skip**, exit 0, from `go test -count=1 -v ./...` across the whole module; `gofmt` and `go vet` clean. That count includes subtests, so it is not comparable with the 723 top-level tests reported for the previous milestone -- it is a different measurement of the same tree, not growth. The skips are the suites that require a running stack |
| Go `-race` | this milestone | **0 races across `./internal/...`.** MinGW-w64 16.1.0 was installed at user scope via winget (no administrator interaction needed), which is what made the detector buildable for the first time |
| Go concurrency and recovery integration | this revision | **15 pass, 0 fail, 0 skip** against a running stack and a real venue simulator, on its own fresh seed: the crash-recovery acceptance test, five-run idempotence, eight concurrent runs, the ambiguous-execution case, and the defect-15 regression |
| Market-data acquisition integration | this revision | **16 pass, 0 fail** with `VANTAGE_MARKETDATA_E2E=1` against the development Postgres, driving the real `Syncer` and the real store against a fake Twelve Data server built to the published contract. These are the first tests this project has had for `Backfill`, `Sync`, `Repair` and `Snapshot`; two of them failed on first run and one of those was a real defect |
| Python unit | this revision | **390 pass, 0 fail**; `ruff` clean; `mypy` clean on 26 source files. Of the 42 new ones, 25 pin that a strategy starved of history says so machine-readably at exactly its declared boundary and that a fed strategy never does, and 17 pin that every replay fixture is either long enough to carry strategy evidence or declared as carrying none. The second suite was written with the warm-up and the history requirement ADDED rather than overlapping, declared all fourteen fixtures too short, and was corrected before it could send anyone to lengthen them |
| Web static | this revision | `tsc --noEmit` clean; `eslint` clean. `npm run build` was NOT re-run after the `global-setup.ts` change, because a build replaces `.next` underneath the running server; the change is test-only. The build was clean earlier in this session, 20 routes, with `lightweight-charts@5.2.1` |
| Playwright | this revision | **57 pass, 0 fail, 0 skip** on Chromium against the live stack, on a freshly reseeded database, with the terminal on :3005. The run before it reported 31 failed / 26 passed because port 3000 is now held by a different project and the suite silently tested THAT application; `global-setup.ts` now refuses to start against anything that is not this terminal. Section 30n |
| Smoke: trading | this revision | **66 pass, 0 fail** against the live stack on its own fresh seed. It first failed at the first order with `csrf_origin_mismatch`, which is the CSRF control working: moving the terminal to :3005 moved `VANTAGE_PUBLIC_WEB_ORIGIN` with it, and the suite's `Origin` header follows through `VANTAGE_SMOKE_WEB_ORIGIN` |
| Smoke: research | this milestone | **35 pass, 0 fail**. It first reported 34/1 on a seven-hour-old database whose earlier suites had left unresolved reconciliation discrepancies pausing automation; a reseed restored 35/35, which is the documented behaviour rather than a regression |
| Replay: `trend-clean` end to end | this revision | 140/140 rows, 81 evaluation instants, 0 errors, on a freshly seeded database with both planes rebuilt and Autopilot on. **46 actionable signals** where the previous revision measured none, 266 starved evaluations now recorded as skips rather than opinions, 39 decisions all TRENDING, no order placed. Section 30o |
| Gitleaks | this milestone | 0 leaks -- git history and the working tree (`--no-git`) |
| Semgrep | this milestone | 0 findings across 246 tracked files with 394 rules, plus a separate 0-finding pass over the 23 new market-data and chart files because Semgrep skips untracked ones, using CI's ruleset list (`p/security-audit`, `p/secrets`, `p/golang`, `p/python`, `p/typescript`, `p/react`, `p/dockerfile`, `p/sql-injection`). The new untracked research modules were scanned separately -- Semgrep scans only git-tracked files by default, so an untracked module is silently skipped. **`--config auto` no longer works with `--metrics off`** and exits 0 after printing an error, which reads exactly like a clean scan |
| govulncheck | this milestone | 0 reachable. "Your code is affected by 0 vulnerabilities"; 1 vulnerability in a required module that nothing calls |
| npm audit | this milestone | 0 vulnerabilities |
| Trivy filesystem (vuln) | this milestone | 0 across `go.mod` and `package-lock.json`, CI's settings (`CRITICAL,HIGH`, `--ignore-unfixed`), exit 0. `.venv`, `node_modules` and `.next` skipped, as they do not exist in CI's fresh checkout -- scanning them made Trivy die with a FATAL walk error that `--exit-code 0` reported as success |
| Trivy config | this milestone | 0 HIGH/CRITICAL misconfigurations across all three Dockerfiles |
| Trivy image -- all three | **carried -- not re-run** | 0 HIGH/CRITICAL **under CI's settings** (`--ignore-unfixed` plus `.trivyignore`); all three exit 0 |
| Trivy image -- unfiltered | this milestone | Research image rebuilt with `--no-cache --pull` so the base is today's: **150 OS findings** (0 CRITICAL, 44 HIGH, 48 MEDIUM, 57 LOW, 1 UNKNOWN) plus **3 Python findings**, every OS one with no upstream fix published. See `.trivyignore` for why none is suppressed and what would resolve it |
| Trivy image -- the 3 fixable findings, named | this milestone | `msgpack` 1.1.2 → 1.2.1 (GHSA-6v7p-g79w-8964, HIGH), `setuptools` 70.3.0 → 78.1.1 (CVE-2025-47273, HIGH) and → 83.0.0 (CVE-2026-59890, MEDIUM). **Verified inside the image rather than accepted on trust:** the importable setuptools is 84.0.0, above both fixes; there is no setuptools 70.3.0 on disk at all -- the version Trivy reports comes from `setuptools==70.3.0` in `pip/_vendor/vendor.txt`, a record of what pip vendored rather than installed code; msgpack 1.1.2 exists only as `pip/_vendor/msgpack` |
| ZAP baseline -- control plane | this milestone | **0 FAIL, 1 WARN, 66 PASS** (`-I -s`, unauthenticated, `127.0.0.1` only). The WARN is Non-Storable Content, informational |
| ZAP baseline -- terminal (production build) | this milestone | **0 FAIL, 3 WARN, 64 PASS** (`-I -s`, unauthenticated). The substantive WARN is the documented `script-src 'unsafe-inline'` acceptance; the others are Non-Storable Content and Modern Web Application, both informational |
| actionlint | this milestone | 0 findings, after fixing the 8 shellcheck issues it reported |
| Restore drill | **carried -- not re-run** | PASSED -- backup, restore into a scratch database, and financial-integrity verification |
| OSV-Scanner | this revision | **RUN IN CI, 0 findings.** Never runnable before: the job referenced `google/osv-scanner-action@v1`, a tag that has never existed there, and pinning the workflows found that. Pinned to v2.5.1 -- and the first real run then exited 127 printing its usage text, because v2 dropped `--skip-git`. Removed; repository scanning is the default now. It has never been run locally (`osv-scanner --version` still reports `command not found`), so the CI job is the only evidence and is cited as such |
| pip-audit | this revision | **RUN IN CI, 0 findings.** Never run before, and it took two attempts. It first failed on `vantage-quant` itself -- the local package, installed editable so its dependencies resolve, is not on PyPI. `--skip-editable` then failed differently, because `--strict` turns a skip into an error. Dropping `--strict` would have passed and would have been worse: it silences an unauditable THIRD PARTY package too. The job now freezes the resolved dependency set without editable installs and audits that, so `--strict` still means what it says |
| GitHub Actions | this revision | **RUN, AND GREEN.** Published 2026-09-30; CI run `36718123184` 5/5 jobs and Security run `36718123066` 11/11 jobs. The first run failed both workflows; six defects and two rounds of fixes later, both pass. `actionlint` reported 0 findings before, during and after, which is the point |

### On the number of reseeds behind that table

Four of those rows needed a freshly reset database, and it is worth saying why
rather than leaving it as an oddity. The seeded account's daily-loss limit is
15 ZAR, every order in a suite pays commission, and the risk engine correctly
refuses to open a position once the limit is reached. Running the race suite
and the Playwright suite back to back spends it, after which later tests are
refused for a reason that has nothing to do with what they are testing.

The suite detects that and **skips with an explicit reseed instruction** rather
than failing as though the platform were broken -- which it did during this
session, and correctly. Each suite above was then run on its own fresh seed.
The alternative, widening the limit for the tests, would have meant the tests
no longer exercised the risk engine they run through.

### Scope of the ZAP scans

Both targets were `127.0.0.1` -- the control plane and the terminal. **No
third-party host was scanned**, and no provider (Exness, MetaQuotes, Trading
Economics or any other) was contacted by any scanner in this audit.
