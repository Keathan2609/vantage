# CLAUDE.md — Vantage

## What this is

A single-operator algorithmic trading platform. Go control plane, Python
research plane, Next.js terminal, PostgreSQL, Redis for rate limits only.

**This build is paper-only and must stay that way.** Read
`docs/REGULATORY_BOUNDARY.md` before touching anything that could change it.

## Non-negotiable rules

1. **Never enable live or demo execution.** Four mechanisms prevent it:
   `config.BuildAllowsLiveExecution`, `config.Load`'s validation, the absence
   of a live adapter, and database CHECK constraints. Do not weaken any of
   them, and do not add a live adapter without an explicit instruction that
   says so in those words.
2. **Never request, store, or ask the user for real broker credentials.**
3. **Never scan, probe, or send traffic to** Exness, MetaQuotes, Trading
   Economics, any third-party provider, or any system not owned by this
   project. Scanning targets are `127.0.0.1` and this repository only.
4. **Never implement a cryptographic primitive.** Compose the standard library.
   `internal/crypto` is the only place crypto lives.
5. **Python must never reach a broker.** No adapter, no credential pointing at
   the control plane, no write access to trading tables. If a task seems to
   need it, the design is wrong.
6. **Never bypass the OMS.** Every order goes through `internal/oms`. There is
   no "internal" path, and adding one is not an optimisation.
   Likewise **never bypass `internal/booking`**: it is the only code that turns
   a fill into a position, ledger entries and a balance, and it is the only
   code that appends a row to `fills`. Reconciliation uses it too, which is why
   a recovered fill passes the same validations as a live one. Two arch tests
   enforce this (`TestOnlyBookingAppendsFills`,
   `TestBookingHoldsNoBrokerAdapter`); if one fails, the fix is to route
   through `booking`, not to relax the test.
7. **Never auto-repair ambiguous financial state.** Only divergence that is
   *provable* from evidence may be repaired without a human — see
   `domain.PolicyFor`. An execution that matches zero or several local orders,
   a position mismatch or a balance mismatch stays open for an operator, and
   position quantities are never written to match the venue. Do not add a
   generic "set order status" endpoint; the answer is a named action in
   `reconcile.Resolve`.
8. **A check that limits exposure or loss must never refuse a reducing
   order.** Three separate checks were found blocking a flatten and trapping
   the operator in the position. The rule is written out in
   `internal/risk/engine.go`. "Reducing" means opposite side **and** quantity
   no greater than the open position — both halves, or a side flip skips the
   exposure checks.
9. **No secrets in the repository**, in a log, in an API response, in a
   decision snapshot, or in audit metadata.
10. **Do not commit** market datasets, trained model binaries, or `.env`.

## Claims and language

- Never state that anything is "compliant", "licensed", "FSCA-approved" or
  "regulated". The strongest available claim is **compliance-supporting**.
- Never describe trading authority as permission, consent, a mandate or
  authorisation. It is a **technical control**.
- Never invent a performance, accuracy or profitability figure, and never
  describe the platform as AI-powered.
- **Never claim something was verified that was not run.** If a test was not
  executed, say so. `docs/ENGINEERING_REPORT.md` keeps a standing list of what
  has and has not been executed; update it rather than working around it.

## Conventions that matter

**Money never becomes a float.** Decimal in Go, `NUMERIC` in Postgres, strings
over the wire and in the browser. `apps/web/lib/format.ts` formats decimal
strings without constructing a float. `money.Amount` carries a mandatory
currency; its zero value has an *empty* currency on purpose, so an
uninitialised field cannot silently join a calculation as "0 ZAR".

The frontend may parse a decimal into a float **only** for a presentational
quantity that never returns to the server — a meter width, a percentage label,
a chart coordinate. Never for a value that is submitted, stored or compared.

**Time comes from an injected clock.** Nothing in business logic calls
`time.Now()`. Timestamps are UTC, truncated to microseconds — the audit chain
depends on it. In React, do not read `Date.now()` during render; use the
`useNow` hook in `lib/store.tsx`.

**Dependencies point inwards.** `domain` cannot import `store`. A rule that
needs a database query to be satisfied is in the wrong package.

**Risk may only reduce.** Never add a path that increases a requested size.

**Fail closed.** A missing rate, a stale quote, an unreachable research
service, an unresolved discrepancy: all produce a refusal, never a guess. NO
TRADE is a first-class, common outcome.

**Comments explain why.** Record the decision and the failure it prevents, not
what the line does.

## Before you say you are done

```bash
cd services/control-api && gofmt -l . && go vet ./... && go test ./...
cd services/quant && python -m ruff check . && python -m mypy vantage_quant && python -m pytest
cd apps/web && npm run typecheck && npm run lint && npm run build
python tests/smoke/smoke.py && python tests/smoke/smoke_research.py   # needs a running stack
cd apps/web && npm run test                                          # Playwright, needs a running stack
cd services/control-api && go test ./tests/race/                     # concurrency, needs a running stack
```

The two stack-dependent suites need credentials and a base URL in the
environment, and neither is defaulted:

```bash
VANTAGE_E2E_BASE_URL=http://localhost:3001   # ONLY if the terminal is not on 3000
VANTAGE_E2E_PASSWORD=...                     # printed by `control-api seed`
VANTAGE_E2E_VIEWER_PASSWORD=...
VANTAGE_E2E_ADMIN_PASSWORD=...               # reconciliation repair is admin-only
VANTAGE_RACE_E2E=1                           # opt-in for the race suite
```

All of it, not a subset. `./scripts/test-all.ps1 -Smoke` runs the lot and
reports every failure rather than stopping at the first.

The Python tools live in `services/quant/.venv`; on Windows invoke them as
`./.venv/Scripts/python.exe -m ruff` etc., because the system Python does not
have them.

## Things that will waste your time

- **If port 3000 is taken, Playwright silently tests the WRONG APPLICATION.**
  `playwright.config.ts` defaults `baseURL` to `http://localhost:3000`. On a
  machine running another project there, all 23 UI tests fail at `signIn` with
  "waiting for locator('.topbar')" — because the page that loaded was a
  different app's landing page, which has no Vantage markup at all. The page
  snapshot in `test-results/*/error-context.md` is what reveals it; the error
  message alone reads like a broken login. Set `VANTAGE_E2E_BASE_URL`.

- **Do not run `npm run build` while a server is serving that build.** The
  build replaces `.next` underneath the running process, which then serves a
  half-swapped app. Restart it afterwards.

- **`output: "standalone"` means `npm start` (`next start`) is unsupported.**
  Next prints a warning and serves anyway, which is worse than failing. Use
  `npm run dev` locally; the container runs
  `node .next/standalone/server.js`.

- **The daily-loss budget is a finite fixture resource.** The seeded account's
  limit is 15 ZAR and every order in a suite pays commission, so running the
  race suite twice in a row spends it. After that the risk engine correctly
  refuses every opening order and several tests fail with statuses that are all
  409 — which looks exactly like a concurrency defect. `requireRiskBudget` in
  the race suite detects it and skips with a reseed instruction. Do not widen
  the limit or edit the ledger to "fix" it; run
  `./scripts/dev-up.ps1 -Reset -Seed`.

- **Reconciliation handlers must use `accountForOperations`, not
  `accountForRequest`.** `accountForRequest` scopes through ownership, and an
  admin owns no trading account — so every admin-only reconciliation call
  returned "Account not found" and the endpoints were unreachable by the only
  role permitted to use them. The bug is silent: the route answers, plausibly,
  with the wrong thing. `TestEveryReconciliationHandlerUsesTheOperationsScope`
  guards it now.

- **`playwright.config.ts` sets a global `use.storageState`, and
  `request.newContext()` inherits it.** A test that means to be unauthenticated
  is silently the trader, so a 401 assertion sees a 403. Pass
  `storageState: undefined` explicitly — for the anonymous context *and* for
  any context that should be a different user.

- **The exposure limits accumulate across a test file.** A suite that places a
  dozen 0.01-lot orders fills the 1500 ZAR per-instrument ceiling part way
  through and every later order is correctly refused. Flatten AND cancel in
  `beforeEach`, not once in `beforeAll` — and cancel before flattening, or the
  flatten is refused for exceeding the pending-order limit.

- **A test that revokes a trading authority must restore it in a cleanup
  registered BEFORE the revoke.** Otherwise a failure anywhere after that point
  leaves the account with no authority, and every later test — in every later
  run — is refused with a 403 that looks nothing like its cause.

- **A unique-violation aborts the whole Postgres transaction** (25P02). Use
  `ON CONFLICT DO NOTHING` and check `RowsAffected`; catching the error and
  continuing turns every duplicate into a 500.
- **Windows locks a running `.exe`** — rebuilds silently fail and you test the
  old binary. `Stop-Process -Name control-api -Force` first. Note that
  `go run` produces a process named `control-api`, not `vantage-api`.
- **PowerShell's `Out-File` writes a BOM** and corrupts `go.mod`. Use the file
  tools, and heredocs rather than `Out-File`.
- **Nanosecond timestamps break the audit chain.** Postgres stores
  microseconds.
- **`jsonb` re-serialises**; audit metadata is `json` so the hash still matches.
- **The app role cannot `CREATE TABLE`.** That is correct — treat `42P01` as
  "nothing applied yet".
- **Port 3000 may be taken** by another project on this machine. The API's CORS
  allows exactly one origin, so `VANTAGE_PUBLIC_WEB_ORIGIN` must match whatever
  port the terminal actually gets, and the API must be restarted after changing
  it. A stale API process still holding 8080 will serve the old origin and the
  browser will report a CORS failure that looks like an application bug.
- **A newly seeded database has no fresh quote for ~2 seconds.** An order
  placed in that window is correctly refused for a stale feed. Wait for feed
  health rather than "fixing" the refusal.

- **The venue closes for an hour every weekday at 17:00 New York.**
  `fx_metals_24x5` models a daily maintenance break (17:00-18:00 NY, Mon-Thu),
  during which ingestion correctly stores nothing and every instrument reports
  `no_data`. Any suite whose setup calls `waitForTradableFeed` then fails after
  its timeout with "market data for X never became tradable (last state:
  no_data)" -- which reads like broken market data and is the platform being
  right. Check the New York clock before debugging it. The Go and smoke suites
  are unaffected only because they do not wait on the feed the same way.

- **`node .next/standalone/server.js` needs `.next/static` copied in.**
  Next's standalone output omits the static assets, so the server answers 200
  for the HTML and 404s every chunk, serving them as `text/plain`. The page
  then renders "Connecting to the control plane..." forever and around 20
  Playwright tests fail on missing selectors, looking exactly like a broken
  API. The Dockerfile copies `.next/static` into the standalone tree; do the
  same when running it by hand, or use `npm run dev`.

## Repository map

```
apps/web                Terminal. No business rules; every refusal comes from
                        the API with a machine-readable code.
  tests/e2e             Playwright. Needs a running stack and
                        VANTAGE_E2E_PASSWORD; there is no default.
services/control-api
  internal/domain       Money, orders, positions, risk, audit. No I/O.
  internal/oms          The order pipeline. The only holder of a BrokerAdapter.
  internal/risk         Pure function. 18 checks. Position sizing.
  internal/booking      The ONLY writer of fills, positions and fill P&L.
                        Shared by execution and reconciliation on purpose.
  internal/reconcile    Snapshot both sides, classify with a pure function,
                        repair only what is provable, halt the rest.
  internal/broker       Adapter interface; broker/mock is the paper venue,
                        with deterministic fault-injection modes.
  internal/econdata     Calendar and news providers.
  internal/notify       Development alerting: notifications + structured logs.
  internal/store        Every SQL statement, parameterised.
  migrations            Numbered, forward-only. Never edit an applied one.
services/quant          Research. No broker client exists here.
tests/smoke             End-to-end assertions against a running stack.
scripts                 Dev up/down, test-all, backup/restore drill.
docs                    Twenty documents. Read the relevant one first.
```

## Adding things

**A strategy:** register it, test it (including no-look-ahead if it uses a new
indicator), backtest it out of sample, promote one lifecycle stage at a time.
In-sample results are not evidence.

**A provider:** implement the interface, register it in `internal/app`. The
five seams are `marketdata.Provider`, `broker.Adapter`,
`econdata.CalendarProvider`, `econdata.NewsProvider`, `fx.RateSource`.

**A migration:** add a new numbered file. It must be idempotent — CI applies
migrations twice.

**A broker fault case:** add a mode to `broker/mock` rather than mocking the
adapter in a test. The point of the fault modes is that OMS and reconciliation
behaviour is exercised against a venue that actually misbehaves.

## When a scanner reports something

Fix it, or record why not *next to the code* (`nosemgrep` with a reason, and
the full rule id including its repeated last segment) or in `.gitleaks.toml` /
`.trivyignore` with the reason and what would change it. Never suppress a
finding globally, and never suppress one you have not understood.
