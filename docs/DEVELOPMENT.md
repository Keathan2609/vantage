# Development

## Prerequisites

| Tool | Version | Why that version |
| --- | --- | --- |
| Docker | any recent | Postgres and Redis |
| Go | 1.26+ | The `go` directive in `services/control-api/go.mod` |
| Python | 3.12 | The floor, not a preference: numpy 2.x's type stubs use `type` statements, which only parse under 3.12 |
| Node | 22 | Next.js 16 |
| PowerShell | 5.1+ | The dev scripts |

## First run

```bash
cp .env.example .env
```

The template ships **recognisable development keys** so the stack starts with
one command. They are not a security hole and they are not a hidden default
either: `config.Load` refuses to start in `staging` or `production` when a key
whose plaintext begins with `development-only` is in use, and it refuses the
example quant service token there too. A known key is no key, and the code says
so rather than trusting a README.

For anything beyond your own machine, generate real ones:

```bash
openssl rand -base64 32
```

Bring the stack up:

```powershell
./scripts/dev-up.ps1 -Seed
```

That starts Postgres and Redis, waits for the health check rather than
sleeping, applies migrations **as the schema owner**, and loads development
data. The seed prints development-only credentials; they exist solely on your
machine, and seeding refuses to run in any environment other than `local`.

Then run the three services in separate terminals:

```bash
cd services/control-api && go run ./cmd/control-api serve
```

```bash
cd services/quant && python -m uvicorn vantage_quant.main:app --host 127.0.0.1 --port 8000
```

```bash
cd apps/web && npm install && npm run dev
```

## Environments

| Environment | Meaning | Constraints |
| --- | --- | --- |
| `local` | Your machine | The only environment where `seed` will run |
| `development` | A shared dev deployment | No seed; paper only |
| `staging` | Pre-production | No seed; paper only |
| `production` | Real deployment | No seed; HSTS on; paper only in this build |

`config.Load` refuses to return a configuration whose execution mode is
anything but `paper`, in every environment. The process exits rather than
starting in an unexpected mode.

## Layout and dependency direction

```
services/control-api/internal/
  money      → nothing
  domain     → money
  risk       → domain, fx
  store      → domain, pgx
  broker     → domain
  oms        → all of the above
  httpapi    → everything
```

Dependencies point **inwards only**. `domain` cannot import `store`, so a
domain rule cannot be quietly satisfied by a database query, and the rules stay
testable without a database. A change that needs to break this direction is a
design problem, not an import problem.

## Migrations

Numbered, forward-only SQL in `services/control-api/migrations`, embedded into
the binary with `go:embed`.

```bash
go run ./cmd/control-api migrate         # apply pending
go run ./cmd/control-api migrate-status  # what is applied, what is pending
```

Rules:

- **Never edit an applied migration.** Add a new one. An edited migration means
  two databases with the same version number and different schemas.
- **Migrations run as the owner role**, via `VANTAGE_MIGRATION_DATABASE_URL`.
  The application role cannot execute DDL, which is the point.
- **Migrations must be idempotent.** CI applies them twice and fails if the
  second run errors.
- Two things live in the working `go:embed` set and are easy to get wrong:
  `migrations/embed.go` must be `package migrations` (embed cannot reach
  `../`), and `fs.ReadFile(migrationFS, e.Name())` takes an unrooted io/fs
  path, not a filesystem path.

The service refuses to serve against a schema it does not recognise. A running
service on a stale schema fails in ways that look like data corruption rather
than a deployment mistake.

## Testing

```bash
./scripts/test-all.ps1            # everything except the smoke suites
./scripts/test-all.ps1 -Smoke     # plus end-to-end, needs a running stack
```

Individually:

```bash
cd services/control-api && go test ./...
cd services/quant && python -m pytest
cd apps/web && npm run typecheck && npm run build
python tests/smoke/smoke.py
python tests/smoke/smoke_research.py
```

**What each layer covers, and what it does not.**

| Suite | Covers | Does not cover |
| --- | --- | --- |
| Go unit | Money arithmetic, the order state machine, position maths, the audit chain, the market clock, config refusals, the risk engine as a pure function | Anything touching the database |
| Python unit | Indicators (including no-look-ahead), strategies, the backtester's fill assumptions, the ML split and baseline logic | The HTTP boundary |
| Smoke: trading | The real pipeline against a real database: idempotency under concurrency, role boundaries, cross-tenant refusal, kill switches, hostile DB writes | Browser behaviour |
| Smoke: research | Strategy evaluation, backtests and training through the API | -- |
| Playwright | That every navigation entry renders content, that the PAPER marker is always present, that refusals read as refusals | Trading outcomes |

Two properties are asserted rather than assumed, because both are the kind of
thing that silently regresses:

- **No look-ahead**, by truncating a series at each index, recomputing, and
  asserting the historical value did not change. Applied to 13 indicators and
  all 14 ML features.
- **The paper-only guarantee**, asserted in CI by checking the compile-time
  constant, the absence of a live adapter, and the presence of the four
  database constraints.

`pytest` is configured with `filterwarnings = ["error::DeprecationWarning"]`. A
deprecation in a numeric library becomes a test failure rather than a silent
behaviour change in a backtest.

## Conventions

**Money never becomes a float.** Decimals in Go (`shopspring/decimal` inside
`money.Amount`), `NUMERIC` in Postgres, strings over the wire, and strings in
the browser. `apps/web/lib/format.ts` formats decimal strings without ever
constructing a float.

**Currency is mandatory.** `money.Amount`'s zero value has an *empty*
currency, so an uninitialised field cannot silently join a calculation as
"0 ZAR".

**Time comes from an injected clock.** Nothing in business logic calls
`time.Now()`. Timestamps are UTC and truncated to microseconds, because
Postgres cannot store nanoseconds and the audit chain has to re-verify.

**Errors carry a stable code.** The UI branches on the code and shows the
server's message. It never invents its own wording for a condition the server
understands better.

**Comments explain why, not what.** The interesting comments in this codebase
are the ones recording a decision -- why the stop is assumed on an ambiguous
bar, why `ON CONFLICT DO NOTHING` rather than catching a unique violation, why
concentration does not apply to the first trade.

**No TODOs in the order path.** CI fails on `TODO`, `FIXME`, `XXX` or `HACK` in
`oms`, `risk`, `broker` or `store`.

## Things that will waste your time

Each of these cost real time during development and is written down so it costs
nobody else any:

- **A unique-violation error aborts the whole Postgres transaction**
  (SQLSTATE 25P02). Catching it and continuing looks correct and turns every
  duplicate submission into a 500. Use `ON CONFLICT DO NOTHING` and check
  `RowsAffected`.
- **Windows locks a running `.exe`.** A rebuild silently fails and you test the
  old binary. Stop the process first:
  `Stop-Process -Name vantage-api -Force`.
- **PowerShell's `Out-File` writes a BOM**, which corrupts `go.mod` and any
  file a parser reads from byte zero. Write files with a tool that does not add
  one.
- **Nanosecond timestamps break the audit chain.** Postgres stores
  microseconds, so a hash computed over a nanosecond value never re-verifies.
- **`jsonb` re-serialises.** The audit metadata column is `json`, not `jsonb`,
  so key order and formatting survive the round trip and the hash still
  matches.
- **The app role cannot `CREATE TABLE`.** That is correct. Code that tried to
  create its own bookkeeping table on startup had to become a pure read that
  treats `42P01` (undefined table) as "nothing applied yet".
- **`pd.Timedelta(hours=0)` is deprecated** and, with warnings-as-errors, fails
  the suite. `pd.to_timedelta(n, unit="h")` is the replacement.

## Adding a strategy

1. Write it in `services/quant/vantage_quant/strategies.py` and register it,
   declaring its timeframe, instruments, parameters and minimum bars.
2. Add a test. If it uses a new indicator, the no-look-ahead property test must
   cover it.
3. Backtest it out-of-sample. An in-sample result is not evidence.
4. Promote one stage at a time. Each stage needs its evidence attached to that
   exact version.
5. Add it to an authority's `allowed_strategy_ids` if it should be permitted to
   act.

Nothing about this can be skipped by editing the database directly: the
lifecycle ceiling is a CHECK constraint and the authority is checked on every
order.

## Adding a provider

Implement the interface, register it in `internal/app`, and nothing else
changes. The five seams are `marketdata.Provider`, `broker.Adapter`,
`econdata.CalendarProvider`, `econdata.NewsProvider` and `fx.RateSource` (the
FX conversion seam). Each has a mock implementation in the repository, which is
the reference for what a real one must handle.
