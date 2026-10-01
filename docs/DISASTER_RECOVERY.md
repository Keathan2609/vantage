# Disaster recovery

**A restore drill has been performed and passes.** `scripts/backup-restore-drill.ps1`
takes a dump, restores it into a scratch database, and verifies the properties
that would actually matter after an incident. Its most recent run:

```
orders=176 fills=54 transactions=18 audit=518 positions=18
restored 58 tables
ok  row counts match the source (orders, fills, transactions, audit, positions)
ok  audit sequence has no gaps
ok  every audit row has a hash
ok  ledger sequence has no gaps
ok  every balance equals its ledger running total
ok  no orphaned fills
ok  every rejected order carries a code
ok  every restored account is still paper-only
ok  the audit chain verifies (518 events, recomputed from the restored rows)
DRILL PASSED
```

What remains not automated is the *schedule*: the drill is run by hand, and
there is no WAL archiving, so the recovery point is the last dump. That gap is
recorded in `docs/COMPLIANCE_READINESS.md`.

**Two things the drill taught that reasoning had missed**, and which the
procedure below now reflects:

1. **Do not restore with `--no-privileges`.** The first run did, and produced a
   database the application could not read at all -- `permission denied for
   table audit_events`. The GRANTs are part of the backup and must come back
   with it.
2. **Restore into a scratch database, not over the live one.** The drill does
   this so that testing recovery cannot itself destroy the thing being
   recovered.

## What must survive

| Data | Where | Reconstructible? |
| --- | --- | --- |
| Ledger (`transactions`) | Postgres | **No.** Append-only, gapless per account. This is the money. |
| Orders, fills, state transitions | Postgres | **No.** |
| Positions | Postgres | Partly -- from the venue, by reconciliation |
| Audit chain | Postgres | **No.** Its value is that it cannot be rebuilt. |
| Decision snapshots | Postgres | No, and they are the record of *why* |
| Risk limits, authorities, their history | Postgres | No |
| Users, MFA secrets, recovery codes | Postgres | No (and MFA can be re-enrolled) |
| Market bars and quotes | Postgres | Yes, from a provider |
| Backtests, model evaluations | Postgres | Yes, by re-running -- the dataset hash and seed are stored |
| Model artefacts | Filesystem | Yes, by retraining |
| Encryption keys | Environment / KMS | **No.** Lose the key and every MFA secret is unreadable. |

The two rows that matter most are the ledger and the keys. Everything else is
either reconstructible or replaceable.

## Backups

Postgres is the only stateful component that matters. Redis holds rate-limit
buckets and is disposable by design -- a cold start is harmless.

```bash
# Full logical backup, custom format, compressed. Written inside the
# container and copied out: piping binary through a shell is how a dump
# arrives corrupted and nobody notices until the restore.
docker exec vantage-postgres pg_dump -U vantage_superuser -d vantage -Fc -f /tmp/vantage.dump
docker cp vantage-postgres:/tmp/vantage.dump "vantage-$(date +%Y%m%d-%H%M%S).dump"
```

What a real deployment needs beyond this, and does not have:

- **A schedule.** Nightly full plus WAL archiving for point-in-time recovery.
  Without WAL, the recovery point is the last dump.
- **Off-host storage.** A backup on the same machine as the database survives
  neither disk failure nor ransomware.
- **Encryption at rest for the dump.** A `pg_dump` contains encrypted MFA
  secrets, but also every email address, IP address and the whole trade
  history.
- **A scheduled drill.** `scripts/backup-restore-drill.ps1` exists and passes,
  but it is run by hand. Nothing runs it nightly, so nothing would notice a
  backup that silently started producing an unusable dump.

**Keys are backed up separately, and never in the same place as the dump.** A
backup containing both the ciphertext and the key that decrypts it is not
encrypted. In a real deployment the key lives in a KMS and the code is written
so that swapping the source changes one constructor.

## Restore

Run the drill first, against a copy, to confirm the dump is good:

```powershell
./scripts/backup-restore-drill.ps1
```

Then, for a real recovery:

```bash
# 1. Stop everything that writes.
./scripts/dev-down.ps1

# 2. Recreate an empty database. On a FRESH cluster the runtime roles must
#    exist BEFORE the restore, because the dump's GRANT statements reference
#    them by name and fail on a role that is not there.
docker compose -f infra/docker/docker-compose.yml up -d postgres
docker exec vantage-postgres psql -U vantage_superuser -d postgres -c "DROP DATABASE IF EXISTS vantage; CREATE DATABASE vantage;"
docker exec vantage-postgres psql -U vantage_superuser -d vantage -f /docker-entrypoint-initdb.d/00-roles.sql

# 3. Restore. --no-owner because the restore runs as the superuser.
#    NOT --no-privileges: the grants are part of the backup, and the first
#    drill run proved that without them the application cannot read its own
#    tables ("permission denied for table audit_events").
docker cp vantage-20260101-120000.dump vantage-postgres:/tmp/restore.dump
docker exec vantage-postgres pg_restore -U vantage_superuser -d vantage --no-owner /tmp/restore.dump

# 4. Bring the schema to the current version. Migrations are idempotent, so
#    this is a no-op when the dump was already current.
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
repeatedly -- it will simply fail readiness again, and the restart loop hides
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
placed *at the venue* still work; stops that exist only in Vantage do not -- which
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
terminal -- all three look identical from here, which is why resolution is a
human action rather than an automatic overwrite.

### The audit chain fails verification

**Symptom.** `verify-audit` reports a broken link, or the Security page shows
`broken`.

**Effect.** The record of what the system did has been altered. The chain names
the first broken sequence number, so everything after it is suspect and
everything before it is intact.

**First action.** **Stop trading.** Activate a global kill switch, preserve the
database (do not restore over it -- that destroys the evidence), and establish
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
opens and the answer becomes "no signal" -- never "trade without the filter".

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
| RTO (control plane) | Minutes | Minutes -- it is one stateless binary |
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
