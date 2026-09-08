# Final engineering report

Every claim below is either something that was executed and observed, or is
labelled as not verified. Nothing is asserted from reading the code alone.

Commit: `c9bca9e`. Appendix A is the summary; this body is the evidence
behind it.

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
tables only — nothing on `users`, `sessions`, `accounts`, `orders`, `fills`,
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

A pure function: 18 checks, all evaluated, all failures reported. Risk may only
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
guessing — a risk evaluation with no available FX rate does not approve.

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
out — the prior build showed a message and offered no way to complete the
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

**Verified:** on a random walk, no model beats the baseline — the correct
result and a check that the harness is not leaking. Model versions record
algorithm, seed, dataset hash, code SHA and dependency versions.

## 17. Provider abstractions

Five seams, each with a mock: `marketdata.Provider`, `broker.Adapter`,
`econdata.CalendarProvider`, `econdata.NewsProvider`, `fx.RateSource`.

The two calendar/news interfaces were **built this session** — previously the
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
two-second window after a reseed, before the ingestor's first tick — the
platform was correct to refuse, and the **test** was fixed to wait for a
tradable feed and to assert on it.

## 19. Reconciliation

The venue wins. Unresolved critical discrepancies halt automated trading and
nothing else.

**Verified:** a clean run comparing orders and positions completed during the
smoke run.

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

## 22. Next.js upgrade — a critical vulnerability found and fixed

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
revealed **16 real typing gaps** — a `Literal` action erased to `str` in eleven
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
`dockerfile`: initially 7 findings, all false positives on inspection —
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
| Trivy config | 0 misconfigurations across three Dockerfiles |
| Trivy image | 0 HIGH/CRITICAL in all three, **under CI's `--ignore-unfixed`** |

That last row needs its qualifier stated, not buried. Re-scanned on 2026-09-08
WITHOUT `--ignore-unfixed`:

| Image | HIGH/CRITICAL | Notes |
| --- | --- | --- |
| control-api | 0 | A static Go binary on a minimal base; there is almost nothing else in it |
| web | 0 | |
| research (quant) | **54** | All from `python:3.12.12-slim-trixie` (Debian 13.6): `perl-base` (3 CRITICAL), `util-linux` and its libraries, `ncurses`, `gzip`, `libacl1`, `libsqlite3-0`, `libsystemd0` |

Every one of those 54 has an **empty fixed-version field** — Debian has not
published a fix — and the Dockerfile already runs `apt-get upgrade -y`, so
there is nothing to apply. None is added to `.trivyignore`, deliberately:
suppressing those ids would also hide them once fixes land, which is precisely
when they should reappear.

The exposure is bounded rather than absent. The research service holds no
broker client and no credentials pointing back at the control plane, its
database role is read-only, it runs non-root on a read-only root filesystem,
and nothing in uvicorn or the numeric stack executes perl, mount helpers or
ncurses. The real fix is a minimal base — about 750 MB of that 813 MB image is
not needed at runtime — and that is recorded as remaining work, not attempted
at the end of an audit.

Getting the images this far required real work: bumping stale base tags,
applying distribution security updates in the final stage, and **removing npm
from the web runtime image** — the server never installs a package, and a
package manager in a production container is a way for anything that gets a
shell to fetch its next stage.

`.trivyignore` records the two accepted application-level findings. Both are
inside pip's own `_vendor` tree — `msgpack==1.1.2` and `setuptools==70.3.0` in
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

Twenty-one documents: `README.md` plus twenty in `docs/`.

Several documentation claims were **found false against the code and corrected**
rather than left standing:

- the event blackout was documented as binding manual orders; it binds
  automated ones only, and the terminal said the wrong thing too
- the calendar and news provider interfaces were documented but did not exist
  — they were built
- "there is deliberately no default" for the encryption keys was wrong: the
  template ships recognisable development keys and the loader refuses them in
  staging and production, which is the better design and is now what the docs
  describe
- Go and Node version floors were understated in the docs and, worse, in the
  Dockerfile, where the pinned 1.23.5 could not build a module requiring 1.26

Each document names what is missing, not only what exists.

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
(SQLSTATE 40P01)` and returned HTTP 500 — the least useful answer an
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
exactly that — `Vantage 0, venue 0.01` — so six real executions existed that
the ledger knew nothing about. The orders themselves sat in `ACCEPTED`
permanently: uncancellable (`order_not_at_venue`, because Phase D never wrote
the venue id) while still consuming the pending-order budget, until the account
reached `8 pending orders against a limit of 5` and could not trade at all.

Fixed by declaring a lock order and taking it first. `store.LockAccountTx` is
now the outermost lock for every transaction that writes anything belonging to
an account, and the comment there records the cycle so the rule is not
mistaken for style. `40P01` is classified (`store.ErrDeadlock`), Phase D
retries once, and — the part that matters most — a Phase D failure after venue
acceptance now routes to `failOrder`, marking the outcome UNKNOWN for
reconciliation instead of returning a bare error that left the order stranded.
A Prometheus counter that must stay at zero guards the regression.

### 2. An exposure limit that prevented reducing exposure

Flattening a 3866 ZAR position was refused: `Order notional 3866.77 ZAR against
a limit of 2500.00 ZAR`. A position built by several individually permitted
orders could grow past the per-order cap and then become **impossible to
close**. The control that exists to contain risk prevented shedding it, and the
larger the position the harder the exit.

The engine already had a `reducing` concept — and applied it to exactly one of
the checks. It now applies to the notional and net-exposure checks too.

Its definition was also too permissive, which was the more dangerous half:
"opposite side" alone let an account long 0.08 lots SELL 5.00 and skip the
gross-exposure, per-instrument and concentration checks entirely — 0.08 of that
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
id — so it was permanently wedged. Reconciliation now closes out an open order
that holds no venue identifier **and** that the venue does not recognise by
client id, on the same evidence the FAILED branch already used: a direct lookup,
not a gap in a paginated list. An order holding a venue id the venue denies
stays critical and manual, because that is a different and far more alarming
thing.

### 5. An unvalidated instrument reaching the database

`POST /strategies/{id}/run` passed `instrument_id` straight through. A missing
or unknown value came back as `strategy_runs_instrument_id_fkey rejected the
write` and was reported as HTTP 500 "The strategy could not be evaluated" — an
internal error for a plainly bad request. Validated up front, answered 422.

### 6. A data-leakage hole in the research plane

`ml.train` accepted `horizon` and `embargo_bars` independently, and the API
exposed both. With `horizon=5, embargo_bars=2` the last three training rows are
labelled from closes that fall **inside** the validation window: the model is
trained on the outcome of the bars it is then scored against, and the
validation number measures nothing.

The defaults (horizon 1, embargo 2) were always safe. The combination was one
HTTP request away. An embargo narrower than the horizon is now refused rather
than silently widened — a caller who asked for a 2-bar embargo on a 10-bar
label has a mistaken mental model, and quietly giving them a different split
than the one recorded in the model's provenance hides that instead of
correcting it. The "zero embargo" warning that could no longer fire was
replaced rather than left as decoration.

### And the documented setup path, which did not work on a fresh clone

`.env.example` never defined `VANTAGE_MIGRATION_DATABASE_URL`, and
`dev-up.ps1` never loaded `.env` for its own `go run` calls — so
`./scripts/dev-up.ps1 -Reset -Seed` failed with `permission denied for schema
public` on any machine where the operator had not exported the variables by
hand. `DEVELOPMENT.md` had documented the variable for a year without anything
supplying it.

The same script also aborted mid-teardown, because PowerShell 5.1 wraps a
native command's stderr in an ErrorRecord and `docker compose` reports progress
there — so `$ErrorActionPreference = 'Stop'` turned normal output into a fatal
error. `2>&1` does not fix that in 5.1; the preference has to be relaxed around
the call and restored after.

Both fixed, and verified by a full reset from an empty volume.

## 31. What is NOT verified

Stated plainly, because a report that lists only successes is not useful. This
list is shorter than it was — six items from the previous revision have since
been executed and moved into the tally — and what remains is what remains.

- **The CI workflows have never run.** There is no remote. `actionlint` passes
  with 0 findings and every command the workflows run has been executed by
  hand, which is not the same thing and must not be reported as if it were.
- **OSV-Scanner and pip-audit were not run.** Neither is installed on this
  machine. Both are configured in CI.
- **No penetration test** against a deployed instance. The ZAP baseline is a
  passive scan of localhost, which is a much weaker claim.
- **Five high-risk packages have no unit tests**: `oms`, `store`, `reconcile`,
  `portfolio`, `orchestrator`. They are covered end-to-end, but
  `handleBrokerError` has a six-branch decision table and three of those
  branches are reached by no test at all. This is the largest piece of
  remaining technical debt. See Appendix A.
- **Reconciliation cannot repair.** It detects a fill divergence and halts
  automation; it cannot ingest the missing fills, `ACCEPTED -> FILLED` is
  forbidden by the state machine, and no endpoint resolves a discrepancy by
  hand. Defect 1 above produced exactly this state, and clearing it required a
  database reset — which is not an operational procedure.
- **No scheduled backups and no WAL archiving.** The restore drill passes but
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
  fault modes — including a lost response after acceptance, which is what
  exposed defect 1's severity — but it does not contradict itself the way a
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

# Appendix A — completion matrix

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
| 2 | Single execution path (OMS) | COMPLETE | `arch_test.go` proves only `oms.go` calls `adapter.PlaceOrder`, that the adapter is held by an allowlist of packages, and that exactly two call sites reach the OMS — the HTTP handler and the orchestrator |
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
| 16 | Reconciliation — detection | COMPLETE | Order and position comparison by broker id and client id, discrepancy severities, automation blocked on critical findings, alerting wired |
| 17 | Reconciliation — repair | **PARTIAL** | It detects a fill divergence and halts automation but cannot ingest the missing fills, and `ACCEPTED -> FILLED` is forbidden by the state machine. There is also no endpoint to resolve a discrepancy by hand. This is the most important next milestone |
| 18 | Unknown-outcome handling | COMPLETE | `ErrUnknownOutcome` is distinct from rejection; FAILED means "outcome unknown" and is never retried. Proven with a deterministic lost-response fault, including that the order stays FAILED |
| 19 | Deterministic broker faults | COMPLETE | 12 modes with exact firing counts, never probabilistic. 9 unit tests; all 12 armed through the development endpoint in a Playwright test |
| 20 | Mock venue | COMPLETE | Its own tables and transaction, spread, slippage, partial fills, order-book state |
| 21 | Live broker adapter | **INTENTIONALLY DEFERRED** | Not written and not compiled in. This is the safety property, not a gap |
| 22 | Market data ingestion | COMPLETE | Provider abstraction, polling ingestor, bar aggregation |
| 23 | Market data quality gates | COMPLETE | Staleness, timestamp regression, duplicate detection, spread ceiling. Unit tested in `domain`; the refusals are asserted in smoke |
| 24 | Economic calendar | COMPLETE | Provider interface, ingestion, and an event blackout that binds automated orders. Unit tested in `econdata` |
| 25 | News feed | COMPLETE | Provider interface, ingestion, labelled as development fixtures in the API response |
| 26 | Portfolio valuation | PARTIAL | Correct and exercised by every smoke run and by equity history, but the package has **no unit tests** — margin, exposure and unrealised P&L are verified only end-to-end |
| 27 | Equity history and attribution | COMPLETE | Recorded per snapshot, exposed and rendered |
| 28 | Strategy library (12) | COMPLETE | Implemented in the research plane with tests; 5 promoted to paper by the seed |
| 29 | Strategy orchestrator | PARTIAL | Evaluates and routes through the same OMS as a manual order (asserted structurally), and concurrent runs are covered by the race suite — but the package has **no unit tests** |
| 30 | Signal to order routing | COMPLETE | `TestEveryOrderPlacementGoesThroughTheSameOMSMethod` pins the two permitted call sites |
| 31 | Backtesting engine | COMPLETE | Next-bar fills, stop assumed on an ambiguous bar, full costs, unaffordable trades counted rather than dropped. Rejects non-ascending bars |
| 32 | Look-ahead prevention | COMPLETE | 13 indicator series property-tested on truncated prefixes; the backtester feeds strict prefixes. One documented caveat: `support_resistance` is window-dependent and currently has no callers |
| 33 | ML training pipeline | COMPLETE | Chronological splits, an embargo now required to cover the label horizon, majority-class baseline, Brier score, full reproducibility record |
| 34 | ML leakage prevention | COMPLETE | Feature look-ahead property test, labels dropped rather than imputed, holdout scored once. The embargo-narrower-than-horizon hole was found by this audit and closed |
| 35 | Model drift detection | COMPLETE | Distribution-shift detection with unit tests |
| 36 | Model artefact storage | COMPLETE | Versioned on disk, deliberately not committed |
| 37 | Scanner | COMPLETE | Ranks candidates and states that it does not execute. Covered by the research smoke suite |
| 38 | Alerting | COMPLETE | All seven required sources wired, per-(kind, key) cooldowns, suppressed-repeat counts, one-shot recovery. 8 unit tests. Sinks are slog, Prometheus and a notifications row — no external SaaS dependency |
| 39 | Notifications | COMPLETE | Stored, listed, markable as read |
| 40 | Metrics | COMPLETE | Prometheus counters and gauges across the pipeline, including a deadlock counter that must stay at zero |
| 41 | Structured logging | COMPLETE | slog with request and correlation identifiers |
| 42 | Distributed tracing | **NOT IMPLEMENTED** | No OpenTelemetry. Correlation ids are propagated, which is what a single-operator system actually needs, but this is a gap rather than a decision worth defending |
| 43 | Health and readiness | COMPLETE | Live and ready endpoints with per-dependency status |
| 44 | Rate limiting | PARTIAL | Redis-backed, per-operation budgets, enforced and demonstrably effective — it refused this audit's own test traffic. The package has no unit tests |
| 45 | Database migrations | COMPLETE | Idempotent, owner-role only, with a status command. CI re-applies them to prove the second run is a no-op |
| 46 | Least-privilege DB roles | COMPLETE | Owner, application and research roles. The application role cannot perform DDL; the research role cannot write at all |
| 47 | Lock ordering | COMPLETE | Declared in `store.LockAccountTx` and taken as the outermost lock. Added by this audit after a real deadlock; a counter and a race test guard the regression |
| 48 | Backup and restore | COMPLETE | `scripts/backup-restore-drill.ps1` performs the whole cycle and verifies the financial integrity of the restored data. Executed and passing |
| 49 | Disaster-recovery procedure | COMPLETE | Documented per failure mode, including the two corrections the first drill produced |
| 50 | Scheduled backups and WAL archiving | **NOT IMPLEMENTED** | The drill is run by hand. The recovery point is the last dump |
| 51 | Terminal (19 routes) | COMPLETE | Every route renders with live data; 30 Playwright tests |
| 52 | Order ticket | COMPLETE | Review before confirm, duplicate-click protection, risk explanations rendered |
| 53 | Containers | COMPLETE | Three images, non-root, no HIGH or CRITICAL misconfigurations |
| 54 | CI workflows | PARTIAL — **cannot be verified locally** | `actionlint` passes with 0 findings and every command the workflows run has been executed by hand. GitHub Actions itself has never run: this repository has no remote. Nothing here may be read as "CI passed" |
| 55 | Regulatory and live-trading readiness | **BLOCKED** | Deliberately. No FSP licence, no client-money segregation, no live venue agreement, no independent audit. Recorded in `COMPLIANCE_READINESS.md` and `REGULATORY_BOUNDARY.md` |

## The unit-test gap, stated plainly

Five packages carry a large share of this platform's risk and have **no unit
tests at all**: `oms`, `store`, `reconcile`, `portfolio` and `orchestrator`. So
do `httpapi`, `fx`, `ratelimit`, `marketdata`, `quant` and `scheduler`.

They are not untested. Every one is driven by the 88 smoke assertions, the 30
Playwright tests and the 9 race tests, all against a real database and a real
venue simulator — which is how this audit found five defects that unit tests
built on fakes would have missed completely. Three of the five were arbitrated
by PostgreSQL, and a fake store would simply have agreed with whatever the test
author imagined.

But integration coverage has a specific weakness: it covers the paths the tests
happen to take. `handleBrokerError` has a six-branch decision table, and three
of those branches are exercised by armed faults while the other three are not
reached at all. That is the honest shape of the gap, and closing it is the
largest piece of remaining technical debt.

---

## Test and scan tally

Every row below was executed on 2026-09-08 against commit `c9bca9e`. Nothing is
carried over from an earlier run.

| Suite | Result |
| --- | --- |
| Go unit | **145 pass, 0 fail, 0 skip** across 9 packages; `gofmt` and `go vet` clean |
| Go race integration | **9 pass, 0 fail** against a running stack, on two consecutive runs |
| Python unit | **202 pass, 0 fail**; `ruff` clean; `mypy` clean on 7 source files |
| Web static | `tsc --noEmit` clean; `eslint` clean; production build clean, 19 routes |
| Playwright | **30 pass, 0 fail** on Chromium against the live stack, on two consecutive runs |
| Smoke: trading | **53 pass, 0 fail** |
| Smoke: research | **35 pass, 0 fail** |
| Gitleaks | 0 leaks — git history and the working tree (`--no-git`) |
| Semgrep | 0 findings, 388 rules, 156 files |
| govulncheck | 0 reachable (1 unreachable, in a required module) |
| npm audit | 0 vulnerabilities |
| Trivy filesystem (vuln) | 0 — `go.mod` and `package-lock.json` |
| Trivy config | 0 HIGH/CRITICAL across all three Dockerfiles |
| Trivy image — all three | 0 HIGH/CRITICAL **under CI's settings** (`--ignore-unfixed` plus `.trivyignore`); all three exit 0 |
| Trivy image — unfiltered | control-api 0, web 0, research **54 HIGH/CRITICAL** from the Debian base, every one with no upstream fix published. See `.trivyignore` for why none is suppressed and what would resolve it |
| ZAP baseline — control plane | **0 FAIL, 0 WARN, 66 PASS** |
| ZAP baseline — terminal (production build) | **0 FAIL, 1 WARN, 64 PASS** — the WARN is the documented `unsafe-inline` acceptance |
| actionlint | 0 findings, after fixing the 8 shellcheck issues it reported |
| Restore drill | PASSED — backup, restore into a scratch database, and financial-integrity verification |
| OSV-Scanner, pip-audit | **NOT RUN.** Neither is installed on this machine. They are configured in CI, which has not run |
| GitHub Actions | **NEVER RUN.** This repository has no remote. `actionlint` passing is not CI passing, and nothing in this report should be read as if it were |

### Scope of the ZAP scans

Both targets were `127.0.0.1` — the control plane and the terminal. **No
third-party host was scanned**, and no provider (Exness, MetaQuotes, Trading
Economics or any other) was contacted by any scanner in this audit.
