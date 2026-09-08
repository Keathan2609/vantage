# Disaster recovery

Honest framing first: **no restore drill has been performed.** The procedures
below are reasoned from the schema and the code, and the backup step is not
automated. That gap is recorded in `docs/COMPLIANCE_READINESS.md` rather than
softened here, because a recovery procedure nobody has executed is a plan, not
a capability.

## What must survive

| Data | Where | Reconstructible? |
| --- | --- | --- |
| Ledger (`transactions`) | Postgres | **No.** Append-only, gapless per account. This is the money. |
| Orders, fills, state transitions | Postgres | **No.** |
| Positions | Postgres | Partly — from the venue, by reconciliation |
| Audit chain | Postgres | **No.** Its value is that it cannot be rebuilt. |
| Decision snapshots | Postgres | No, and they are the record of *why* |
| Risk limits, authorities, their history | Postgres | No |
| Users, MFA secrets, recovery codes | Postgres | No (and MFA can be re-enrolled) |
| Market bars and quotes | Postgres | Yes, from a provider |
| Backtests, model evaluations | Postgres | Yes, by re-running — the dataset hash and seed are stored |
| Model artefacts | Filesystem | Yes, by retraining |
| Encryption keys | Environment / KMS | **No.** Lose the key and every MFA secret is unreadable. |

The two rows that matter most are the ledger and the keys. Everything else is
either reconstructible or replaceable.

## Backups

Postgres is the only stateful component that matters. Redis holds rate-limit
buckets and is disposable by design — a cold start is harmless.

```bash
# Full logical backup, custom format, compressed.
docker exec vantage-postgres pg_dump -U vantage_superuser -d vantage -Fc \
  > vantage-$(date +%Y%m%d-%H%M%S).dump
```

What a real deployment needs beyond this, and does not have:

- **A schedule.** Nightly full plus WAL archiving for point-in-time recovery.
  Without WAL, the recovery point is the last dump.
- **Off-host storage.** A backup on the same machine as the database survives
  neither disk failure nor ransomware.
- **Encryption at rest for the dump.** A `pg_dump` contains encrypted MFA
  secrets, but also every email address, IP address and the whole trade
  history.
- **A tested restore.** Untested backups have a well-earned reputation.

**Keys are backed up separately, and never in the same place as the dump.** A
backup containing both the ciphertext and the key that decrypts it is not
encrypted. In a real deployment the key lives in a KMS and the code is written
so that swapping the source changes one constructor.

## Restore

```bash
# 1. Stop everything that writes.
./scripts/dev-down.ps1

# 2. Recreate an empty database with the runtime roles.
docker compose -f infra/docker/docker-compose.yml up -d postgres
docker exec -i vantage-postgres psql -U vantage_superuser -d postgres \
  -c "DROP DATABASE IF EXISTS vantage; CREATE DATABASE vantage;"
docker exec -i vantage-postgres psql -U vantage_superuser -d vantage \
  < infra/docker/postgres-init/00-roles.sql

# 3. Restore.
docker exec -i vantage-postgres pg_restore -U vantage_superuser -d vantage --no-owner \
  < vantage-20260101-120000.dump

# 4. Bring the schema to the current version.
cd services/control-api && go run ./cmd/control-api migrate

# 5. Verify the audit chain BEFORE trading.
go run ./cmd/control-api verify-audit
```

Step 5 is not optional. A restore is exactly the circumstance in which rows
could be missing or reordered, and the chain is the only thing that will say
so. If verification fails, do not trade: establish what happened first.

Then, before resuming automation:

```bash
# 6. Reconcile against the venue. Local state is a cache; the venue holds the money.
curl -X POST http://localhost:8080/api/v1/reconciliation/{accountID}/run
```

## Failure modes and first actions

### The database is unreachable

**Symptom.** `/health/ready` fails; every request 503s.

**Effect.** No orders can be placed, which is the correct failure. Nothing is
lost, because nothing is written outside a transaction.

**First action.** Restore connectivity. Do not restart the control plane
repeatedly — it will simply fail readiness again, and the restart loop hides
the actual error in the logs.

### The control plane crashed mid-order

**Symptom.** An order sits in `FAILED`.

**Effect.** `FAILED` means the venue-side outcome is unknown, not that nothing
happened. Automation for that account is blocked.

**First action.** Run reconciliation. It resolves the order against venue state
and moves it to whatever actually happened. **Never resubmit**: retrying an
unknown outcome is how one intended trade becomes two positions.

### The venue is unreachable

**Symptom.** `broker_up` is 0; new orders fail.

**Effect.** Open positions are at the venue and unaffected by our outage. Stops
placed *at the venue* still work; stops that exist only in Vantage do not — which
is why the platform submits protective levels with the order rather than
holding them locally.

**First action.** Reconcile on reconnect, before resuming automation. A
reconnect that resumes trading first is how a stale position book gets traded
against.

### Reconciliation reports critical discrepancies

**Symptom.** `automation_blocked: true`.

**Effect.** Automated trading is halted. Manual trading is not.

**First action.** Look at both views on the Authority page. Decide whether this
was a bug, a lost response, or a trade placed directly in the broker's own
terminal — all three look identical from here, which is why resolution is a
human action rather than an automatic overwrite.

### The audit chain fails verification

**Symptom.** `verify-audit` reports a broken link, or the Security page shows
`broken`.

**Effect.** The record of what the system did has been altered. The chain names
the first broken sequence number, so everything after it is suspect and
everything before it is intact.

**First action.** **Stop trading.** Activate a global kill switch, preserve the
database (do not restore over it — that destroys the evidence), and establish
who had write access. This is the one failure that is not an operational
problem.

### The encryption key is lost

**Effect.** MFA secrets cannot be decrypted. Nothing else is affected: passwords
are hashed, not encrypted, and no other field uses the key today.

**First action.** Rotate to a new key version and have every user re-enrol MFA.
Keys are versioned precisely so this is a re-enrolment rather than a rebuild.

### The research service is down

**Symptom.** `/health/ready` reports `quant: unavailable`.

**Effect.** No new signals. Manual trading is unaffected. The circuit breaker
opens and the answer becomes "no signal" — never "trade without the filter".

**First action.** Restart it. Nothing is lost: it holds no state that matters.

### Market data has stopped

**Symptom.** `quote_age_seconds` climbing; feed health `stale`.

**Effect.** Automated orders are refused. Manual orders are refused too once
the feed is `stale` rather than `degraded`.

**First action.** Check the provider before restarting anything. The refusal is
the system working, and it is safer than the alternative.

## Recovery objectives

Stated as targets rather than achievements, because nothing here has been
measured under a real failure:

| Objective | Target | Currently |
| --- | --- | --- |
| RPO (data loss) | 0 for the ledger | Last manual dump |
| RTO (control plane) | Minutes | Minutes — it is one stateless binary |
| RTO (database) | Under an hour | Untested |
| Position-book accuracy after recovery | Exact, via reconciliation | Depends on the venue's history window |

The last row is the real constraint. If a venue's execution history is shorter
than an outage, the ledger will have a hole that reconciliation cannot close,
and it must be reconstructed by hand from the venue's own statements.

## What deliberately does not exist

- **No automatic failover.** A single-operator system that fails over
  automatically is a system with two places to be confused about the truth.
- **No automatic restore.** Restoring over live data is how a recoverable
  incident becomes an unrecoverable one.
- **No automatic liquidation on any failure.** Dumping positions into whatever
  conditions caused the incident is not risk management. Flatten is always an
  explicit, confirmed action.
