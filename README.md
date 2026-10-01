# Vantage

A single operator trading platform: market data, a strategy and research
engine, a risk controlled order pipeline, and a dense terminal to drive it all
from.

**Everything here is simulated.** Orders go to a mock venue. No live broker
adapter is compiled into the binary, no real money venue is reachable from any
process in this repository, and no funds can move. Four independent mechanisms
enforce that, described under [Paper only](#paper-only).

I built it to find out what it actually takes to run an automated strategy
responsibly, rather than to make money. Most of the work turned out to be in
the parts nobody demos: reconciliation, failing closed, and being able to prove
afterwards what the system did and why.

## What is in it

**Market data.** Quote and bar ingestion behind one provider interface, with
per instrument health that the risk engine checks before any order. Live quotes
fold into 15m, 1h and 4h bars so the series keeps moving.

**Trading.** One order pipeline with no second entry point. Idempotent
submission, an explicit state machine, and a paper venue that models spread,
slippage, latency, partial fills and margin refusal, plus deliberate fault
modes so recovery is tested against a venue that misbehaves rather than a mock
that agrees.

**Risk.** A pure function, 26 checks, all of them evaluated on every order so a
refusal reports everything that failed rather than the first thing. Risk may
only reduce a requested size, never increase it, and no check that limits
exposure is allowed to refuse an order that reduces a position.

**Control.** Trading authority as a scoped technical control, kill switches,
account level trading flags, and a trading verdict derived from real state
rather than from whether the process is up.

**Recovery.** Reconciliation snapshots both sides, classifies the difference
with a pure function, and repairs only what is provable from evidence. Anything
ambiguous halts the narrowest scope that contains it and waits for a named
operator action. Position quantities are never written to match the venue.

**Replay.** A committed dataset drives the real pipeline on an injected clock:
ingestion, bars, strategies, consensus, risk, the OMS, the venue, the ledger
and the audit chain. Two runs from the same starting database produce identical
financial output. Development only, and refused anywhere else.

**Research.** Twelve registered strategies, a backtester that states its fill
assumptions, machine learning with chronological splits and a baseline to beat,
and a scanner. The research service has no broker client and cannot reach one.

**Evidence.** Append only hash chained audit events, immutable decision
snapshots, and a ledger entry behind every balance change.

**Terminal.** A dark, dense, tabular Next.js front end. It holds no business
rules. Every refusal comes from the API with a machine readable code, and the
execution mode is visible on every page.

## Running it

You need Docker, Go 1.26+, Python 3.12+ and Node 20+.

```bash
cp .env.example .env
```

The `.env` asks for two keys. Generate each with:

```bash
openssl rand -base64 32
```

Start Postgres and Redis:

```bash
docker compose -f infra/docker/docker-compose.yml up -d
```

Apply migrations and load development data:

```bash
cd services/control-api && go run ./cmd/control-api migrate && go run ./cmd/control-api seed
```

Seeding prints credentials that exist only on your machine, and it refuses to
run in any environment other than `local`.

Then run the three services, each in its own terminal:

```bash
cd services/control-api && go run ./cmd/control-api serve
```

```bash
cd services/quant && python -m uvicorn vantage_quant.main:app --host 127.0.0.1 --port 8000
```

```bash
cd apps/web && npm install && npm run dev
```

The terminal is at http://localhost:3000.

That is the local setup. To run it somewhere you can reach from a phone or
another machine, see [DEPLOYMENT](docs/DEPLOYMENT.md). If something else on your machine
already has that port, set `VANTAGE_PUBLIC_WEB_ORIGIN` to the port you actually
get and restart the control plane, or CORS will refuse the browser.

## Layout

```
apps/web                  Next.js terminal (App Router, TypeScript strict)
services/control-api      Go control plane, the only path to a broker
  internal/domain         Money, orders, positions, risk, audit. No I/O
  internal/oms            The order pipeline
  internal/risk           The risk engine and position sizing, both pure
  internal/booking        The one accounting path: fill to position, ledger, balance
  internal/reconcile      Snapshots, a pure classifier, bounded repair
  internal/replay         Deterministic market replay, development only
  internal/orchestrator   Signals to intents, and the consensus policy
  internal/broker         The adapter interface; broker/mock is the paper venue
  internal/store          SQL, one file per bounded context
  migrations              Numbered, forward only
services/quant            Python research service. No broker client exists here
tests/smoke               End to end assertions against a running stack
infra/docker              Local Postgres and Redis with least privilege roles
docs                      Architecture, security and operational documentation
```

## Paper only

Four mechanisms, any one of which would stop live execution on its own.

1. **A compile time constant.** `config.BuildAllowsLiveExecution` is false. No
   configuration value can change it.
2. **Configuration validation.** `config.Load` refuses any execution mode other
   than paper, and any broker other than the mock venue. The process exits
   rather than starting in an unexpected mode.
3. **No live adapter exists.** The broker registry holds only the mock venue.
   There is no code path to a real broker because there is no adapter to reach.
4. **Database constraints.** Check constraints on accounts, broker connections,
   orders and strategy versions reject non paper rows at the storage layer,
   whatever the application believes.

The consequences are visible rather than buried. The terminal carries a PAPER
marker on every page, `/version` reports `live_trading_available: false`, and
the control plane logs its mode at startup.

## Documentation

| Document | Covers |
| --- | --- |
| [ARCHITECTURE](docs/ARCHITECTURE.md) | Services, planes, data flow, and why the boundaries sit where they do |
| [SECURITY](docs/SECURITY.md) | Authentication, authorisation, secrets, cryptography, transport, database privilege |
| [THREAT_MODEL](docs/THREAT_MODEL.md) | Assets, adversaries, attack surfaces, controls, residual risk |
| [REGULATORY_BOUNDARY](docs/REGULATORY_BOUNDARY.md) | What this software is and is not, and what architecture cannot decide |
| [COMPLIANCE_READINESS](docs/COMPLIANCE_READINESS.md) | Which capabilities support which obligations, and where the gaps are |
| [TRADING_AUTHORITY](docs/TRADING_AUTHORITY.md) | The mandate model, and why it is a technical control |
| [ORDER_LIFECYCLE](docs/ORDER_LIFECYCLE.md) | The pipeline, the state machine, idempotency, failure semantics |
| [RISK_ENGINE](docs/RISK_ENGINE.md) | Every check, position sizing, and what risk may not do |
| [BROKER_ADAPTERS](docs/BROKER_ADAPTERS.md) | The adapter contract, the mock venue, and what a real adapter would need |
| [MARKET_DATA](docs/MARKET_DATA.md) | Providers, freshness, health states, sessions and the market clock |
| [RECONCILIATION](docs/RECONCILIATION.md) | Detection, the divergence taxonomy, automatic against operator repair, halt scope, and what stays unresolvable |
| [STRATEGY_ENGINE](docs/STRATEGY_ENGINE.md) | Signals, the registry, lifecycle promotion, and the boundary with execution |
| [BACKTESTING](docs/BACKTESTING.md) | Fill assumptions, costs, metrics, and the ways a backtest lies |
| [MACHINE_LEARNING](docs/MACHINE_LEARNING.md) | Features, splits, leakage tests, baselines, drift, fail closed inference |
| [NEWS_AND_CALENDAR](docs/NEWS_AND_CALENDAR.md) | Event data, blackout windows, and provider terms |
| [AUTOPILOT](docs/AUTOPILOT.md) | The autonomous pipeline, its gates, the on off switch, and what is deliberately not enabled |
| [MARKET_REPLAY](docs/MARKET_REPLAY.md) | Driving the real pipeline from a dataset, and the assumptions a replay makes |
| [DESIGN_SYSTEM](docs/DESIGN_SYSTEM.md) | The terminal's visual rules, and the measurements behind them |
| [GOLD_RESEARCH_PROFILE](docs/GOLD_RESEARCH_PROFILE.md) | What is specific about XAUUSD, and why none of it belongs in generic infrastructure |
| [OBSERVABILITY](docs/OBSERVABILITY.md) | Logs, metrics, audit, and what to alert on |
| [DEVELOPMENT](docs/DEVELOPMENT.md) | Environments, workflows, testing, migrations, conventions |
| [DEPLOYMENT](docs/DEPLOYMENT.md) | Running it somewhere you can reach from any device, and why serverless cannot host the control plane |
| [DISASTER_RECOVERY](docs/DISASTER_RECOVERY.md) | Failure modes, backups, restore, and recovering a broken position view |
| [ENGINEERING_REPORT](docs/ENGINEERING_REPORT.md) | What was built, what was verified and how, and what was not |

## Tests

```bash
cd services/control-api && go test ./...
cd services/quant && python -m pytest
cd apps/web && npm run typecheck && npm run build
python tests/smoke/smoke.py            # needs a running stack
python tests/smoke/smoke_research.py
cd apps/web && npm run test            # Playwright, needs a running stack
cd services/control-api && go test ./tests/race/   # concurrency and recovery
```

The last three need credentials and a base URL in the environment. Nothing is
defaulted, and `VANTAGE_RACE_E2E=1` opts into the race suite.

The race suite spends the seeded account's real daily loss budget on
commission, so after two full runs it skips with an instruction to reseed
rather than failing as though something were broken.
`./scripts/test-all.ps1 -Smoke` runs the lot and reports every failure instead
of stopping at the first.

## What this is not

It is not a licensed financial service and it gives no advice. See
[REGULATORY_BOUNDARY](docs/REGULATORY_BOUNDARY.md).

It does not hold, pool or transfer anyone's funds, and has no structure that
could.

It makes no claim about profitability. The research tools exist to measure
whether an approach works and to say so plainly when it does not, which so far
is most of the time.

## Licence

All rights reserved. See [LICENSE](LICENSE). The code is published so it can be
read and assessed, not reused. If you want to do something with it, ask.
