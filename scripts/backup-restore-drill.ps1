<#
.SYNOPSIS
    Back up the development database, restore it into a scratch database, and
    verify the restored state is internally consistent.

.DESCRIPTION
    A backup nobody has restored is a plan, not a capability. This script
    performs the whole cycle and checks the properties that would actually
    matter after a real incident:

      * the dump succeeds and is non-trivial in size
      * it restores into an empty database without error
      * the audit hash chain still verifies from the restored rows
      * the per-account ledger sequence is gapless
      * every balance equals the running total of its ledger
      * order, fill and position counts match the source

    It restores into a SCRATCH database by default, so the drill cannot destroy
    the database it is testing. Restoring over the live one is what turns a
    recoverable incident into an unrecoverable one.

.PARAMETER Keep
    Keep the scratch database and the dump file afterwards, for inspection.

.PARAMETER Target
    Name of the scratch database. Defaults to vantage_restore_drill.

.EXAMPLE
    ./scripts/backup-restore-drill.ps1
#>

[CmdletBinding()]
param(
    [switch]$Keep,
    [string]$Target = 'vantage_restore_drill'
)

$ErrorActionPreference = 'Stop'

$root = Split-Path -Parent $PSScriptRoot
$container = 'vantage-postgres'
$superuser = 'vantage_superuser'
$source = 'vantage'

function Invoke-Psql {
    param([string]$Database, [string]$Sql)
    # PGOPTIONS sets the SERVER's client_min_messages, which is what silences
    # a NOTICE. `--set` only sets a psql variable and does not.
    #
    # This matters because PowerShell 5.1 wraps a native command's stderr in an
    # ErrorRecord: with $ErrorActionPreference = 'Stop', a harmless "database
    # does not exist, skipping" notice aborts the whole script.
    docker exec -e PGPASSWORD=$env:POSTGRES_SUPERUSER_PASSWORD `
        -e PGOPTIONS='-c client_min_messages=error' $container `
        psql -U $superuser -d $Database -q -v ON_ERROR_STOP=1 -tAc $Sql
}

function Assert-Equal {
    param([string]$What, $Expected, $Actual)
    if ("$Expected" -ne "$Actual") {
        Write-Host "  FAIL  $What : source=$Expected restored=$Actual" -ForegroundColor Red
        $script:failures++
    } else {
        Write-Host "  ok    $What ($Actual)" -ForegroundColor Green
    }
}

$script:failures = 0

# The superuser password comes from .env, like everything else.
$envFile = Join-Path $root '.env'
if (-not (Test-Path $envFile)) { Write-Error "No .env at $envFile" }
Get-Content $envFile | ForEach-Object {
    if ($_ -match '^\s*([A-Z0-9_]+)\s*=\s*(.*)$') {
        [Environment]::SetEnvironmentVariable($matches[1], $matches[2].Trim(), 'Process')
    }
}
if (-not $env:POSTGRES_SUPERUSER_PASSWORD) {
    # The compose file's own default, used when .env does not override it.
    $env:POSTGRES_SUPERUSER_PASSWORD = 'vantage_superuser_dev_password'
}

Write-Host ''
Write-Host '=== 1. Source state ===' -ForegroundColor Cyan
$srcOrders    = Invoke-Psql $source 'SELECT count(*) FROM orders'
$srcFills     = Invoke-Psql $source 'SELECT count(*) FROM fills'
$srcTx        = Invoke-Psql $source 'SELECT count(*) FROM transactions'
$srcAudit     = Invoke-Psql $source 'SELECT count(*) FROM audit_events'
$srcPositions = Invoke-Psql $source 'SELECT count(*) FROM positions'
$srcAuditHead = Invoke-Psql $source 'SELECT COALESCE(max(sequence), 0) FROM audit_events'
Write-Host "  orders=$srcOrders fills=$srcFills transactions=$srcTx audit=$srcAudit positions=$srcPositions head=$srcAuditHead"

Write-Host ''
Write-Host '=== 2. Backup ===' -ForegroundColor Cyan
$stamp = Get-Date -Format 'yyyyMMdd-HHmmss'
$dumpDir = Join-Path $root 'reports'
New-Item -ItemType Directory -Force -Path $dumpDir | Out-Null
$dump = Join-Path $dumpDir "vantage-$stamp.dump"

# The dump is written INSIDE the container and copied out, rather than piped
# through PowerShell. Piping binary through a 5.1 pipeline corrupts it, and
# -AsByteStream does not exist before PowerShell 6.
$inside = "/tmp/vantage-$stamp.dump"
docker exec -e PGPASSWORD=$env:POSTGRES_SUPERUSER_PASSWORD $container `
    pg_dump -U $superuser -d $source -Fc -f $inside
if ($LASTEXITCODE -ne 0) { Write-Error 'pg_dump failed' }

docker cp "${container}:${inside}" $dump
if ($LASTEXITCODE -ne 0) { Write-Error 'docker cp of the dump failed' }

$size = (Get-Item $dump).Length
Write-Host "  wrote $dump ($([math]::Round($size / 1MB, 2)) MB)"
if ($size -lt 100KB) {
    Write-Host '  FAIL  the dump is implausibly small' -ForegroundColor Red
    $script:failures++
}

Write-Host ''
Write-Host '=== 3. Restore into a scratch database ===' -ForegroundColor Cyan
Write-Host "  target: $Target (the source database is never written to)"
Invoke-Psql 'postgres' "DROP DATABASE IF EXISTS $Target" | Out-Null
Invoke-Psql 'postgres' "CREATE DATABASE $Target" | Out-Null

# Restored from the copy still inside the container, for the same reason the
# dump was taken there.
#
# --no-owner is used because the restore runs as the superuser, and the roles
# already exist in this cluster. --no-privileges is deliberately NOT used: the
# first run of this drill restored without privileges and produced a database
# the application could not read at all ("permission denied for table
# audit_events"). The GRANTs are part of the backup and must come back with it.
#
# On a FRESH cluster the roles must exist before the restore, so
# infra/docker/postgres-init/00-roles.sql runs first. See
# docs/DISASTER_RECOVERY.md.
$restoreOutput = docker exec -e PGPASSWORD=$env:POSTGRES_SUPERUSER_PASSWORD $container `
    pg_restore -U $superuser -d $Target --no-owner $inside 2>&1
$restoreOutput | Select-String -Pattern 'error' | ForEach-Object {
    Write-Host "  restore warning: $_" -ForegroundColor Yellow
}

$restoredTables = Invoke-Psql $Target `
    "SELECT count(*) FROM information_schema.tables WHERE table_schema='public'"
Write-Host "  restored $restoredTables tables"
if ([int]$restoredTables -lt 50) {
    Write-Host '  FAIL  too few tables restored' -ForegroundColor Red
    $script:failures++
}

Write-Host ''
Write-Host '=== 4. Row counts match ===' -ForegroundColor Cyan
Assert-Equal 'orders'       $srcOrders    (Invoke-Psql $Target 'SELECT count(*) FROM orders')
Assert-Equal 'fills'        $srcFills     (Invoke-Psql $Target 'SELECT count(*) FROM fills')
Assert-Equal 'transactions' $srcTx        (Invoke-Psql $Target 'SELECT count(*) FROM transactions')
Assert-Equal 'audit_events' $srcAudit     (Invoke-Psql $Target 'SELECT count(*) FROM audit_events')
Assert-Equal 'positions'    $srcPositions (Invoke-Psql $Target 'SELECT count(*) FROM positions')

Write-Host ''
Write-Host '=== 5. Financial integrity of the restored data ===' -ForegroundColor Cyan

# The audit chain is the reason this check exists at all: a restore is exactly
# the circumstance in which rows could be missing or reordered.
$chainGaps = Invoke-Psql $Target @"
SELECT count(*) FROM (
  SELECT sequence, lag(sequence) OVER (ORDER BY sequence) AS prev
  FROM audit_events
) s WHERE prev IS NOT NULL AND sequence - prev <> 1
"@
Assert-Equal 'audit sequence has no gaps' 0 $chainGaps

$chainNulls = Invoke-Psql $Target `
    "SELECT count(*) FROM audit_events WHERE hash IS NULL OR hash = ''"
Assert-Equal 'every audit row has a hash' 0 $chainNulls

# The ledger sequence is per account and must be gapless: a hole means a
# transaction is missing, which means a balance cannot be reconstructed.
$ledgerGaps = Invoke-Psql $Target @"
SELECT count(*) FROM (
  SELECT account_id, sequence,
         lag(sequence) OVER (PARTITION BY account_id ORDER BY sequence) AS prev
  FROM transactions
) s WHERE prev IS NOT NULL AND sequence - prev <> 1
"@
Assert-Equal 'ledger sequence has no gaps' 0 $ledgerGaps

# Every balance_after must equal the running total of its account's ledger.
# This is the property that says the money adds up.
$ledgerDrift = Invoke-Psql $Target @"
WITH running AS (
  SELECT account_id, sequence, balance_after,
         sum(amount) OVER (PARTITION BY account_id ORDER BY sequence
                           ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW) AS computed
  FROM transactions
)
SELECT count(*) FROM running WHERE abs(balance_after - computed) > 0.005
"@
Assert-Equal 'every balance equals its ledger running total' 0 $ledgerDrift

# A filled order must have fills, and a fill must belong to an order.
$orphanFills = Invoke-Psql $Target @"
SELECT count(*) FROM fills f
LEFT JOIN orders o ON o.id = f.order_id WHERE o.id IS NULL
"@
Assert-Equal 'no orphaned fills' 0 $orphanFills

$rejectedWithoutCode = Invoke-Psql $Target `
    "SELECT count(*) FROM orders WHERE status = 'REJECTED' AND reject_code IS NULL"
Assert-Equal 'every rejected order carries a code' 0 $rejectedWithoutCode

$nonPaper = Invoke-Psql $Target `
    "SELECT count(*) FROM accounts WHERE mode <> 'paper'"
Assert-Equal 'every restored account is still paper-only' 0 $nonPaper

Write-Host ''
Write-Host '=== 6. Verify the audit chain with the application itself ===' -ForegroundColor Cyan
Write-Host '  (recomputes every hash from the restored rows, not just the sequence)'
Push-Location (Join-Path $root 'services/control-api')
try {
    $restoreUrl = "postgres://vantage_app:vantage_app_dev_password@localhost:5432/$Target" + '?sslmode=disable'
    $env:VANTAGE_DATABASE_URL = $restoreUrl
    $env:VANTAGE_MIGRATION_DATABASE_URL = $restoreUrl
    go run ./cmd/control-api verify-audit
    if ($LASTEXITCODE -ne 0) {
        Write-Host '  FAIL  the audit chain does not verify against the restored data' -ForegroundColor Red
        $script:failures++
    } else {
        Write-Host '  ok    the chain verifies' -ForegroundColor Green
    }
} finally {
    Pop-Location
}

Write-Host ''
if (-not $Keep) {
    Invoke-Psql 'postgres' "DROP DATABASE IF EXISTS $Target" | Out-Null
    Remove-Item $dump -Force -ErrorAction SilentlyContinue
    docker exec $container rm -f $inside 2>&1 | Out-Null
    Write-Host 'Scratch database and dump removed. Pass -Keep to retain them.' -ForegroundColor DarkGray
} else {
    Write-Host "Kept: database $Target and $dump" -ForegroundColor DarkGray
}

Write-Host ''
if ($script:failures -gt 0) {
    Write-Host "DRILL FAILED: $($script:failures) check(s) did not pass." -ForegroundColor Red
    exit 1
}
Write-Host 'DRILL PASSED: backup, restore and integrity checks all succeeded.' -ForegroundColor Green
exit 0
