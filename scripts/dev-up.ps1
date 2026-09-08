<#
.SYNOPSIS
    Bring up the Vantage development stack.

.DESCRIPTION
    Starts Postgres and Redis, applies migrations as the schema owner, and
    optionally loads development data. The control plane, research service and
    terminal are left to be run in their own terminals during development, so
    a code change is a restart rather than an image rebuild.

    Refuses to run without a .env file. The template ships recognisable
    development keys so the stack starts with one command; the control plane
    refuses to start with those keys in staging or production, so a known key
    cannot quietly reach a deployment.

.PARAMETER Seed
    Load deterministic development data after migrating.

.PARAMETER Reset
    Destroy the database volume first. Everything in it is lost.

.EXAMPLE
    ./scripts/dev-up.ps1 -Seed
#>

[CmdletBinding()]
param(
    [switch]$Seed,
    [switch]$Reset
)

$ErrorActionPreference = 'Stop'

$root = Split-Path -Parent $PSScriptRoot
$compose = Join-Path $root 'infra/docker/docker-compose.yml'
$envFile = Join-Path $root '.env'

if (-not (Test-Path $envFile)) {
    Write-Error @"
No .env file found at $envFile

Copy the template:

    Copy-Item .env.example .env

The template's keys are development-only and are recognised as such: the
control plane refuses to start with them in staging or production. Generate
real ones for anything beyond this machine:

    openssl rand -base64 32
"@
}

# Load .env into this process so the Go commands below see the same values the
# containers do.
Get-Content $envFile | ForEach-Object {
    if ($_ -match '^\s*([A-Z0-9_]+)\s*=\s*(.*)$') {
        [Environment]::SetEnvironmentVariable($matches[1], $matches[2].Trim(), 'Process')
    }
}

if ($env:VANTAGE_EXECUTION_MODE -and $env:VANTAGE_EXECUTION_MODE -ne 'paper') {
    Write-Error "VANTAGE_EXECUTION_MODE is '$($env:VANTAGE_EXECUTION_MODE)'. This build accepts 'paper' only."
}

if ($Reset) {
    Write-Host 'Destroying the database volume. All local data will be lost.' -ForegroundColor Yellow
    docker compose -f $compose --env-file $envFile down -v
    if ($LASTEXITCODE -ne 0) { Write-Error 'docker compose down failed' }
}

Write-Host 'Starting Postgres and Redis...' -ForegroundColor Cyan
docker compose -f $compose --env-file $envFile up -d postgres redis
if ($LASTEXITCODE -ne 0) { Write-Error 'docker compose up failed' }

# Wait for the health check rather than sleeping a fixed interval: a slow
# machine should wait longer, not fail.
Write-Host 'Waiting for Postgres to report healthy...' -NoNewline
$deadline = (Get-Date).AddMinutes(2)
do {
    Start-Sleep -Seconds 2
    Write-Host '.' -NoNewline
    $state = docker inspect --format '{{.State.Health.Status}}' vantage-postgres 2>$null
} while ($state -ne 'healthy' -and (Get-Date) -lt $deadline)
Write-Host ''

if ($state -ne 'healthy') {
    docker logs --tail 40 vantage-postgres
    Write-Error "Postgres did not become healthy (last state: $state)"
}

Push-Location (Join-Path $root 'services/control-api')
try {
    Write-Host 'Applying migrations as the schema owner...' -ForegroundColor Cyan
    go run ./cmd/control-api migrate
    if ($LASTEXITCODE -ne 0) { Write-Error 'migrations failed' }

    go run ./cmd/control-api migrate-status

    if ($Seed) {
        Write-Host 'Loading development data...' -ForegroundColor Cyan
        go run ./cmd/control-api seed
        if ($LASTEXITCODE -ne 0) { Write-Error 'seed failed' }
    }
} finally {
    Pop-Location
}

Write-Host ''
Write-Host 'Stack is up. Run these in separate terminals:' -ForegroundColor Green
Write-Host '  cd services/control-api ; go run ./cmd/control-api serve'
Write-Host '  cd services/quant       ; python -m uvicorn vantage_quant.main:app --host 127.0.0.1 --port 8000'
Write-Host '  cd apps/web             ; npm run dev'
Write-Host ''
Write-Host 'Execution mode: paper. No live broker adapter is compiled in.' -ForegroundColor DarkGray
