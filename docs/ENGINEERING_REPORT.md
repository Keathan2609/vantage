# Final engineering report

Every claim below is either something that was executed and observed, or is
labelled as not verified. Nothing is asserted from reading the code alone.

Commits: the reconciliation milestone is `e0a7f3b`, `bf8e151` and the commit
that carries this revision of the report. Appendix A is the summary; this body
is the evidence behind it.

This revision covers the reconciliation, recovery and operator-control
milestone. The previous revision's largest recorded gap was that
reconciliation could detect divergence but not repair it; that gap is closed,
and section 19 states precisely what is repaired automatically and what is
deliberately never repaired without a human.

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

Re-scanned again on 2026-09-08 after this milestone, with no severity filter
and no ignore file at all, the picture is unchanged: control-api 1 (the
module-level openpgp advisory), web 0 at every severity, research 173 (3
CRITICAL, 51 HIGH, 57 MEDIUM, 57 LOW, 5 UNKNOWN) plus the 3 accepted
application-level findings in pip's vendored tree.

Every one of those 54 HIGH/CRITICAL has an **empty fixed-version field** —
Debian has not published a fix — and the Dockerfile already runs `apt-get upgrade -y`, so
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
recorded in `CLAUDE.md` and `docs/ORDER_LIFECYCLE.md` because the failure is
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
- **`go test -race` has never been run on this machine.** It requires cgo and
  there is no C compiler installed (`gcc: command not found`), so the detector
  cannot build. The concurrency evidence in this report is therefore
  *behavioural* -- 14 integration tests driving genuine parallel HTTP requests
  against a real database -- and not detector-verified. Those two things catch
  different bugs and neither substitutes for the other. CI is configured to run
  `-race`; CI has not run.
- **`orchestrator`, `httpapi`, `fx`, `ratelimit`, `marketdata`, `quant` and
  `scheduler` still have no unit tests.** `oms`, `store`, `reconcile` and
  `portfolio` now do, and `handleBrokerError`'s six-branch table is fully
  covered, so this list is shorter than it was -- but it is still the largest
  piece of remaining technical debt. See Appendix A.
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
| 16 | Reconciliation — detection | COMPLETE | 13-type taxonomy, snapshot-based, classified by a pure function with 34 unit tests. Fingerprint deduplication with a partial unique index; stale issues closed when they stop being detected |
| 17 | Reconciliation — repair | COMPLETE | Four provable types repaired automatically through the same accounting path an ordinary fill uses; a separate repair state machine that cannot leave a terminal state; every repair stamped and attributed to its issue. Crash-recovery, idempotence, concurrent-run and ambiguity tests all pass |
| 17a | Reconciliation — operator control | COMPLETE | Seven ADMIN-only actions with mandatory reasons, full issue history, failed attempts audited, no "set order status" endpoint. 21 Playwright tests including forged ids, cross-account ids, mass assignment and repeat resolution |
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

This was the largest piece of technical debt in the previous revision: five
high-risk packages with no unit tests at all. Four of them now have some.

- `oms` — 10 tests. `handleBrokerError` was the specific worry: a six-branch
  decision table with three branches reached by no test. It is now a pure
  function, `ClassifyBrokerError`, and all six branches are covered, including
  an explicit test that an unrecognised error fails closed to UNKNOWN.
- `reconcile` — 34 tests over the classifier, all reachable because it takes
  two snapshots and does no I/O.
- `store` — 15 tests over `mapError` and `Fingerprint`, the two pure decisions
  in the package. `mapError` decides whether every database failure becomes a
  404, a 409, a retry or an opaque 500; the deadlock branch is covered
  specifically, because it was once an unclassified 40P01 surfacing as a bare
  500 on order placement.
- `portfolio` — 7 tests, after extracting the exposure arithmetic from
  `Compute`, which read six tables before reaching it. The cases that were
  never tested were the ones nobody wants to seed: an unpriceable position must
  not count as flat, gross and net must disagree for a hedged book, and
  currency exposure must net across instruments while gross does not.

Still with none: `orchestrator`, `httpapi`, `fx`, `ratelimit`, `marketdata`,
`quant`, `scheduler`.

The reason integration coverage was not simply extended instead: it covers the
paths the tests happen to take. But the reverse is also true and worth keeping
in view — this audit's defects were found by integration tests against a real
database and a real venue simulator, and three of them were arbitrated by
PostgreSQL. A fake store would have agreed with whatever the test author
imagined. Both kinds of test earn their place; neither replaces the other.

---

## Test and scan tally

Every row below was executed against the milestone's final commit, on a freshly
reset and reseeded database. Nothing is carried over from an earlier run.

| Suite | Result |
| --- | --- |
| Go unit | **261 pass, 0 fail, 0 skip** across 15 packages; `gofmt` and `go vet` clean. By package: domain 62, risk 38, reconcile 34, store 18, arch 13, booking 12, oms 10, portfolio 7, and the rest |
| Go `-race` | **NOT RUN.** The detector requires cgo and no C compiler is installed on this machine (`gcc: command not found`). Behavioural concurrency evidence is the row below; that is not the same claim |
| Go concurrency and recovery integration | **15 pass, 0 fail** against a running stack and a real venue simulator, including the crash-recovery acceptance test, five-run idempotence, eight concurrent runs, the ambiguous-execution case, and the defect-15 regression |
| Python unit | **202 pass, 0 fail**; `ruff` clean; `mypy` clean on 7 source files |
| Web static | `tsc --noEmit` clean; `eslint` clean; production build clean, 20 routes including `/operations` |
| Playwright | **51 pass, 0 fail, 0 skip** on Chromium against the live stack, of which 21 are the new reconciliation and operations tests |
| Smoke: trading | **53 pass, 0 fail** |
| Smoke: research | **35 pass, 0 fail** |
| Gitleaks | 0 leaks — git history and the working tree (`--no-git`) |
| Semgrep | 0 findings, 394 rules, 168 files |
| govulncheck | 0 reachable (1 unreachable, in a required module) |
| npm audit | 0 vulnerabilities |
| Trivy filesystem (vuln) | 0 — `go.mod` and `package-lock.json` |
| Trivy config | 0 HIGH/CRITICAL across all three Dockerfiles |
| Trivy image — all three | 0 HIGH/CRITICAL **under CI's settings** (`--ignore-unfixed` plus `.trivyignore`); all three exit 0 |
| Trivy image — unfiltered | control-api 0, web 0, research **54 HIGH/CRITICAL** from the Debian base, every one with no upstream fix published. See `.trivyignore` for why none is suppressed and what would resolve it |
| ZAP baseline — control plane | **0 FAIL, 0 WARN, 69 PASS**, 4 URLs |
| ZAP baseline — terminal (production build) | **0 FAIL, 1 WARN, 68 PASS**, 17 URLs including `/operations` — the WARN is the documented `unsafe-inline` acceptance |
| actionlint | 0 findings, after fixing the 8 shellcheck issues it reported |
| Restore drill | PASSED — backup, restore into a scratch database, and financial-integrity verification |
| OSV-Scanner | **NOT RUN — not installed.** `osv-scanner --version` reports `command not found`. It is configured in CI, which has not run. No other scanner was substituted for it and no OSV result is claimed anywhere in this report |
| pip-audit | **NOT RUN — not installed**, in the project venv or on PATH. Configured in CI |
| GitHub Actions | **NEVER RUN.** This repository has no remote. `actionlint` passing is not CI passing, and nothing in this report should be read as if it were |

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

Both targets were `127.0.0.1` — the control plane and the terminal. **No
third-party host was scanned**, and no provider (Exness, MetaQuotes, Trading
Economics or any other) was contacted by any scanner in this audit.
