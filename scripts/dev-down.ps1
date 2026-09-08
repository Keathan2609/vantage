<#
.SYNOPSIS
    Stop the Vantage development stack.

.DESCRIPTION
    Stops the containers. The database volume is preserved unless -Purge is
    given, because losing a paper account's trade history to a routine stop
    would make forward testing worthless.

.PARAMETER Purge
    Also delete the volumes. Everything in the database is lost.

.EXAMPLE
    ./scripts/dev-down.ps1
#>

[CmdletBinding()]
param(
    [switch]$Purge
)

$ErrorActionPreference = 'Stop'

$root = Split-Path -Parent $PSScriptRoot
$compose = Join-Path $root 'infra/docker/docker-compose.yml'
$envFile = Join-Path $root '.env'

$composeArgs = @('-f', $compose)
if (Test-Path $envFile) { $composeArgs += @('--env-file', $envFile) }

if ($Purge) {
    Write-Host 'Stopping and deleting all volumes. Local trade history will be lost.' -ForegroundColor Yellow
    docker compose @composeArgs down -v
} else {
    docker compose @composeArgs down
}

if ($LASTEXITCODE -ne 0) { Write-Error 'docker compose down failed' }

# Local service processes started outside compose.
foreach ($name in @('vantage-api', 'vantage-control-api')) {
    $procs = Get-Process -Name $name -ErrorAction SilentlyContinue
    if ($procs) {
        Write-Host "Stopping local process: $name" -ForegroundColor Cyan
        $procs | Stop-Process -Force
    }
}

Write-Host 'Stack stopped.' -ForegroundColor Green
