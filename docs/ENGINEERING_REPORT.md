# Final engineering report

Every claim below is either something that was executed and observed, or is
labelled as not verified. Nothing is asserted from reading the code alone.

Commits: the reconciliation milestone is `e0a7f3b`, `bf8e151`, `16f02ab`. The
intelligence milestone is `0a6d8a3`, `4bc1aaa`, `ee32862`, `339b74e`,
`31f6159`. The market-replay milestone is `0cb491b`, `bccd954` and the commit
that carries this revision of the report. Appendix A is the summary; this body
is the evidence behind it.

This revision covers the market-replay milestone. The previous revision's
largest recorded gap was that the replay provider drove the decision layer but
not the application; that gap is closed, and section 19b states exactly what
a replay now drives and what it assumes.

The headline result is that one deterministic dataset now runs the real
pipeline end to end -- ingestion, bars, strategies, orchestration, risk,
Autopilot, OMS, mock venue, fills, ledger, audit -- and two runs from a
byte-identical database produce byte-identical financial output. Reaching that
took five defects, four of them in code the previous milestones had passed.

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

### The five untested packages

The previous revision named `orchestrator`, `httpapi`, `fx`, `ratelimit`,
`marketdata`, `quant` and `scheduler` as having no unit tests. Five now do:

| Package | Tests | What the cases are about |
| --- | --- | --- |
| `orchestrator` | 49 | The consensus policy and the ten scenarios |
| `marketdata` | 59 | Mock determinism, OHLC consistency, replay no-look-ahead, bar aggregation, feed alert transitions |
| `quant` | 23 | Every malformed-signal case becomes NO TRADE upstream, so a permissive validator is a trading defect. Plus the breaker: a 5xx counts, a 4xx does not |
| `fx` | 13 | A stale direct rate must not fall through to a fresher inverse; no rate must never become 1.0 |
| `scheduler` | 11 | Panic recovery, cancellation, the per-job timeout, and that a slow job is never entered twice |

`httpapi` and `ratelimit` still have none.

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

Twenty-three documents: `README.md` plus twenty-two in `docs/`.

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
- **`httpapi` and `ratelimit` still have no unit tests.** Nine packages now
  do. This list was five packages long in the previous revision and is two now.
- **Only ONE scenario is driven end to end.** `trend-clean` runs the whole
  pipeline and is the determinism proof. The infrastructure now exists for the
  rest, and nine datasets are committed, but scenarios D, F, G, I, J and the
  new N-S are still decision-layer tests only. Rewriting them onto the replay
  engine is the obvious next step and was not reached.
- **A replay run's code identity is only as good as the build.** The record
  is now persisted (`replay_runs`, migration 0013) and carries the dataset
  hash, code SHA, config hash and seed. But a `go run` binary has no commit
  identity, so `code_sha` reads `unknown` -- and two such runs COMPARE EQUAL on
  it while establishing nothing. `Start` records a durable warning saying the
  comparison cannot be made; a container build stamps a real SHA. This is
  recorded rather than fixed because the honest answer for an uncommitted
  working tree is that there is no identity to record.
- **Speed is not proven not to affect results.** It is designed to change
  pacing only, and the pacing code touches no price, size or timestamp. The
  determinism proof ran at `max` both times rather than comparing `1x` against
  `100x`, so the property is argued rather than measured.
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
- **Regime and model attribution are still missing.** P&L is attributed by
  instrument, strategy, strategy version, source, session, event context,
  transaction type, replay run, and PAPER_FORWARD-versus-REPLAY -- folded from
  the ledger so the buckets reconcile to the account exactly. No decision
  records the market regime or the model that informed it, so those two
  dimensions cannot be served and are not offered.
- **No backtest versus paper-forward comparison.** Both engines exist and both
  produce metrics; nothing compares them, so the differences caused by the OMS,
  spread, slippage, latency, the scheduler and reconciliation are unmeasured.
- **Correlation is still not measured.** Named in the previous two revisions.
  The consensus policy accepts a portfolio veto and the `correlated-pair`
  dataset exists to exercise one, but nothing computes that gold and silver
  move together, so the veto is never raised.
- **RISK_OFF is still never inferred**, and no regime-transition or hysteresis
  tests exist.
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
| 29 | Strategy orchestrator | COMPLETE | Evaluates and routes through the same OMS as a manual order (asserted structurally), concurrent runs covered by the race suite, and 49 unit tests over the consensus policy and the ten scenarios |
| 29a | Multi-strategy consensus | COMPLETE | A pure, versioned aggregation policy: vetoes before votes, no majority resolution of a disagreement, abstentions not counted as agreement, an unavailable model never read as assent. 35 tests |
| 29b | Market regimes | COMPLETE | Seven canonical regimes in `domain`, fail-closed parsing, UNKNOWN never coerced. **RISK_OFF is nameable but no classifier infers it** — recorded as a gap |
| 29c | Bar aggregation from the live feed | COMPLETE | 15m/1h/4h from the quote stream; complete bars only to strategies, mid not bid/ask, venue-timestamp bucketing, late ticks dropped, restart continuation. Was entirely MISSING and is the milestone's headline defect |
| 29d | Deterministic market replay | COMPLETE for the decision layer, **PARTIAL** end to end | Provider plus five series generators, no look-ahead by construction, ten scenarios A-J reproducible. Does not yet drive ingestion, the OMS and the ledger — that needs config plumbing in `internal/app` |
| 29f | Market replay — end-to-end pipeline | COMPLETE for one dataset | `Scheduler.ReplayStep` calls the real jobs; no second trading engine, enforced by an architecture test. Measured: 665 signals, 250 orders, 189 fills, 64 ledger rows, gapless ledger, stored balance equal to the derived one |
| 29g | Market replay — determinism | COMPLETE | Two runs from a byte-identical database produce byte-identical financial output. Required seeding the mock venue, which was seeding slippage from the wall clock |
| 29h | Market replay — datasets and controls | COMPLETE | Nine committed fixtures with parsed-row hashes, an allowlist registry (never a path), admin-only start/step/advance/pause/resume/reset/stop/speed/reconcile, reason required and audited for start and stop |
| 29i | Market replay — scenario coverage | **PARTIAL** | One of twenty scenarios (A) is driven end to end. The rest remain decision-layer tests; the infrastructure to move them exists |
| 29j | ReplayRun persistence | COMPLETE | `replay_runs` (0013). Written before the run is announced and updated as it plays, so an interrupted replay still leaves a trace; a run whose opening record cannot be written is refused, while a later recording failure never destroys a finished run. `simulated` cannot be false, and the app role cannot DELETE |
| 29k | Replay run history API | COMPLETE | Admin-only `GET /replay/runs` and `/replay/runs/{id}`, answering in every process because a recorded run is evidence whether or not the engine is present |
| 29n | PAPER_FORWARD separated from BACKTEST/REPLAY | COMPLETE | `orders.replay_run_id` (0014) tags every order created while a replay owned the clock, including a manual one. `?by=run_kind` and `?by=replay_run` read it. Without the tag a replay's numbers and a forward session's sit in one account indistinguishably |
| 29l | P&L attribution across dimensions | COMPLETE for nine of eleven | Instrument, strategy, strategy version, source, session, event context and transaction type. Folded from the ledger, so every entry is counted exactly once; the report compares itself against the account's own totals and reports itself unreconciled rather than presenting a truncated window as the account. Regime and replay run are NOT attributed |
| 29m | Attribution reconciliation | COMPLETE | Measured on a replay: 25 of 25 ledger entries in every one of the seven dimensions, discrepancy 0.00, and 500.00 funding plus -20.66 trading equal to the ledger's 479.34 |
| 29e | Autopilot global switch | COMPLETE | Default OFF, ADMIN to change with a mandatory reason, any role to read, append-only history, enforced in the order transaction with its own rejection code, distinct from the kill switch |
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
| 53a | Walk-forward, sensitivity, Monte Carlo | **PARTIAL** | Implemented in `vantage_quant.backtest` with Python tests, but exposed by no HTTP route — the control plane cannot run one and no result is stored or displayed |
| 53b | ML validation depth | **PARTIAL** | Precision, recall, ROC-AUC, Brier and a majority-class baseline, with a warning when the model does not beat it. No F1, PR-AUC, confusion matrix or calibration curve, so a 0.80 output is a score rather than a demonstrated probability |
| 53c | P&L attribution | **PARTIAL** | By instrument only. Strategy, model, session, regime and event context are not attributed, so "which strategy made the money?" is unanswerable from the platform |
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
| Go unit | **419 pass, 0 fail, 0 skip** across 20 packages; `gofmt` and `go vet` clean. By package: domain 65, marketdata 59, orchestrator 49, risk 38, reconcile 34, quant 23, store 18, arch 13, fx 13, booking 12, scheduler 11, oms 10, econdata 9, portfolio 7, and the rest |
| Go `-race` | **0 races across all 20 packages.** MinGW-w64 16.1.0 was installed at user scope via winget (no administrator interaction needed), which is what made the detector buildable for the first time |
| Go concurrency and recovery integration | **15 pass, 0 fail** against a running stack and a real venue simulator, including the crash-recovery acceptance test, five-run idempotence, eight concurrent runs, the ambiguous-execution case, and the defect-15 regression |
| Python unit | **202 pass, 0 fail**; `ruff` clean; `mypy` clean on 7 source files |
| Web static | `tsc --noEmit` clean; `eslint` clean; production build clean, 20 routes including `/operations` |
| Playwright | **51 pass, 0 fail, 0 skip** on Chromium against the live stack, of which 21 are the new reconciliation and operations tests |
| Smoke: trading | **55 pass, 0 fail** |
| Smoke: research | **35 pass, 0 fail** |
| Gitleaks | 0 leaks — git history and the working tree (`--no-git`) |
| Semgrep | 0 findings, 394 rules, 177 files |
| govulncheck | 0 reachable (1 unreachable, in a required module) |
| npm audit | 0 vulnerabilities |
| Trivy filesystem (vuln) | 0 — `go.mod` and `package-lock.json` |
| Trivy config | 0 HIGH/CRITICAL across all three Dockerfiles |
| Trivy image — all three | 0 HIGH/CRITICAL **under CI's settings** (`--ignore-unfixed` plus `.trivyignore`); all three exit 0 |
| Trivy image — unfiltered | control-api 0, web 0, research **54 HIGH/CRITICAL** from the Debian base, every one with no upstream fix published. See `.trivyignore` for why none is suppressed and what would resolve it |
| ZAP baseline — control plane | **0 FAIL, 0 WARN, 60 PASS**, 4 URLs |
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
