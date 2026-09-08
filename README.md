# Vantage

An institutional-style trading platform for a single operator: market data, a
strategy and research engine, a risk-controlled order pipeline, and a dense
terminal interface.

**This build executes simulated paper orders against a mock venue. No live
broker adapter is compiled into the binary, no real-money venue is reachable
from any process here, and no real funds can move.** That is enforced in three
independent places in code and again in the database — see
[Paper-only enforcement](#paper-only-enforcement).

---

## What it does

| Area | Summary |
| --- | --- |
| Market data | Quote and bar ingestion behind a `marketdata.Provider` interface, with per-instrument health that the risk engine consults before any order |
| Trading | A single order pipeline: 19 ordered gates, idempotent submission, an explicit order state machine, and a paper venue that models spread, slippage, latency, partial fills and margin refusal |
| Risk | 18 deterministic checks evaluated as a pure function; every check runs, all failures are reported, and risk may only reduce a requested size |
| Control | Trading authority (a scoped technical mandate), kill switches, account trading flags, and reconciliation against the venue |
| Research | 12 registered strategies, an honest backtester, chronological-split machine learning with baseline comparison, and an opportunity scanner |
| Evidence | Append-only, hash-chained audit events; immutable decision snapshots; a full ledger of every balance change |
| Interface | An 18-page Next.js terminal: dark, dense, tabular, with the execution mode visible on every page |

## Quick start

Requires Docker, Go 1.26+, Python 3.12+ and Node 20+.

```bash
cp .env.example .env
```

Generate the two keys the `.env` asks for and paste them in:

```bash
openssl rand -base64 32
```

Start Postgres and Redis:

```bash
docker compose -f infra/docker/docker-compose.yml up -d
```

Apply migrations, then load development data:

```bash
cd services/control-api && go run ./cmd/control-api migrate && go run ./cmd/control-api seed
```

The seed prints development-only credentials. They exist solely on your
machine; seeding refuses to run in any environment other than `local`.

Run the three services in separate terminals:

```bash
cd services/control-api && go run ./cmd/control-api serve
```

```bash
cd services/quant && python -m uvicorn vantage_quant.main:app --host 127.0.0.1 --port 8000
```

```bash
cd apps/web && npm install && npm run dev
```

Then open http://localhost:3000.

## Layout

```
apps/web                  Next.js 16 terminal (App Router, TypeScript strict)
services/control-api      Go control plane — the only path to a broker
  internal/domain         Money, orders, positions, risk, audit: no I/O
  internal/oms            The order pipeline
  internal/risk           The risk engine (a pure function) and position sizing
  internal/broker         BrokerAdapter interface; broker/mock is the paper venue
  internal/store          SQL, one file per bounded context
  migrations              Numbered, forward-only SQL
services/quant            Python research service (FastAPI): strategies,
                          backtests, indicators, ML. No broker client exists here.
tests/smoke               End-to-end assertions against a running stack
infra/docker              Local Postgres and Redis, with least-privilege roles
docs                      Architecture, security and operational documentation
```

## Paper-only enforcement

Four independent mechanisms, any one of which would stop live execution on its
own:

1. **A compile-time constant.** `config.BuildAllowsLiveExecution = false`. No
   configuration value can change it, and no live adapter is linked into the
   binary.
2. **Configuration validation.** `config.Load` refuses to return a
   configuration whose execution mode is anything but `paper`, or whose broker
   list contains anything but `mock`. The process exits rather than starting in
   an unexpected mode.
3. **No live adapter exists.** The broker registry is populated only with the
   mock venue. There is no code path that could reach a real broker, because no
   such adapter is present to reach.
4. **Database constraints.** `accounts_paper_only_ck`,
   `broker_connections_mock_only_ck`, `orders` mode checks and
   `strategy_versions_paper_ceiling_ck` reject non-paper rows at the storage
   layer, whatever the application believes.

The consequences are stated where an operator will see them: the terminal
carries a PAPER marker on every page, `/version` reports
`live_trading_available: false`, and the control plane logs its mode at
start-up.

## Documentation

| Document | Covers |
| --- | --- |
| [ARCHITECTURE](docs/ARCHITECTURE.md) | Services, planes, data flow, why the boundaries sit where they do |
| [SECURITY](docs/SECURITY.md) | Authentication, authorisation, secrets, cryptography, transport, database privilege |
| [THREAT_MODEL](docs/THREAT_MODEL.md) | Assets, adversaries, attack surfaces, controls, residual risk |
| [REGULATORY_BOUNDARY](docs/REGULATORY_BOUNDARY.md) | What this software is and is not, and what architecture cannot decide |
| [COMPLIANCE_READINESS](docs/COMPLIANCE_READINESS.md) | Which capabilities support which obligations, and the gaps |
| [TRADING_AUTHORITY](docs/TRADING_AUTHORITY.md) | The mandate model, and why it is a technical control |
| [ORDER_LIFECYCLE](docs/ORDER_LIFECYCLE.md) | The pipeline, the state machine, idempotency, failure semantics |
| [RISK_ENGINE](docs/RISK_ENGINE.md) | Every check, position sizing, and what risk may not do |
| [BROKER_ADAPTERS](docs/BROKER_ADAPTERS.md) | The adapter contract, the mock venue, and what a real adapter would need |
| [MARKET_DATA](docs/MARKET_DATA.md) | Providers, freshness, health states, sessions and the market clock |
| [RECONCILIATION](docs/RECONCILIATION.md) | Comparing our records with the venue's, and what a mismatch blocks |
| [STRATEGY_ENGINE](docs/STRATEGY_ENGINE.md) | Signals, the registry, lifecycle promotion, and the boundary with execution |
| [BACKTESTING](docs/BACKTESTING.md) | Fill assumptions, costs, metrics, and the ways a backtest lies |
| [MACHINE_LEARNING](docs/MACHINE_LEARNING.md) | Features, splits, leakage tests, baselines, drift, fail-closed inference |
| [NEWS_AND_CALENDAR](docs/NEWS_AND_CALENDAR.md) | Event data, blackout windows, and provider terms |
| [AUTOPILOT](docs/AUTOPILOT.md) | The autonomous pipeline, its gates, and what is deliberately not enabled |
| [OBSERVABILITY](docs/OBSERVABILITY.md) | Logs, metrics, audit, and what to alert on |
| [DEVELOPMENT](docs/DEVELOPMENT.md) | Environments, workflows, testing, migrations, conventions |
| [DISASTER_RECOVERY](docs/DISASTER_RECOVERY.md) | Failure modes, backups, restore, and recovering a broken position view |
| [ENGINEERING_REPORT](docs/ENGINEERING_REPORT.md) | What was built, what was verified and how, and what was not |

## Tests

```bash
cd services/control-api && go test ./...
cd services/quant && python -m pytest
cd apps/web && npm run typecheck && npm run build
python tests/smoke/smoke.py            # against a running stack
python tests/smoke/smoke_research.py
```

## What this is not

- It is not a licensed financial service, and it gives no advice. See
  [REGULATORY_BOUNDARY](docs/REGULATORY_BOUNDARY.md).
- It does not hold, pool or transfer client funds, and has no structure that
  could.
- It makes no claim about profitability. The research tools are built to
  measure whether an approach works and to report honestly when it does not.
