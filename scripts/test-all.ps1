<#
.SYNOPSIS
    Run every test suite in the repository.

.DESCRIPTION
    Runs, in order: Go unit tests, Python unit tests, the web typecheck and
    production build, and — with -Smoke — the end-to-end smoke suites against a
    running stack.

    Continues past a failing suite so one run reports everything that is
    broken, then exits non-zero if anything failed. Stopping at the first
    failure hides the other three.

.PARAMETER Smoke
    Also run the smoke suites. Requires the control plane, the research service
    and the database to be running.

.EXAMPLE
    ./scripts/test-all.ps1 -Smoke
#>

[CmdletBinding()]
param(
    [switch]$Smoke
)

$root = Split-Path -Parent $PSScriptRoot
$failures = [System.Collections.Generic.List[string]]::new()

function Invoke-Suite {
    param([string]$Name, [string]$WorkingDirectory, [scriptblock]$Command)

    Write-Host ''
    Write-Host "=== $Name " -ForegroundColor Cyan -NoNewline
    Write-Host ('=' * [Math]::Max(1, 60 - $Name.Length)) -ForegroundColor DarkGray

    Push-Location $WorkingDirectory
    try {
        & $Command
        if ($LASTEXITCODE -ne 0) {
            $failures.Add($Name)
            Write-Host "$Name FAILED (exit $LASTEXITCODE)" -ForegroundColor Red
        } else {
            Write-Host "$Name passed" -ForegroundColor Green
        }
    } catch {
        $failures.Add($Name)
        Write-Host "$Name FAILED: $_" -ForegroundColor Red
    } finally {
        Pop-Location
    }
}

Invoke-Suite 'go vet' (Join-Path $root 'services/control-api') { go vet ./... }
Invoke-Suite 'go test' (Join-Path $root 'services/control-api') { go test ./... }

Invoke-Suite 'gofmt' (Join-Path $root 'services/control-api') {
    $unformatted = gofmt -l .
    if ($unformatted) {
        Write-Host 'These files are not gofmt-clean:' -ForegroundColor Yellow
        $unformatted | ForEach-Object { Write-Host "  $_" }
        $global:LASTEXITCODE = 1
    } else {
        $global:LASTEXITCODE = 0
    }
}

Invoke-Suite 'ruff' (Join-Path $root 'services/quant') { python -m ruff check . }
Invoke-Suite 'pytest' (Join-Path $root 'services/quant') { python -m pytest }

Invoke-Suite 'web typecheck' (Join-Path $root 'apps/web') { npm run typecheck }
Invoke-Suite 'web build' (Join-Path $root 'apps/web') { npm run build }

if ($Smoke) {
    Invoke-Suite 'smoke: trading' $root { python tests/smoke/smoke.py }
    Invoke-Suite 'smoke: research' $root { python tests/smoke/smoke_research.py }
}

Write-Host ''
if ($failures.Count -gt 0) {
    Write-Host "FAILED suites: $($failures -join ', ')" -ForegroundColor Red
    exit 1
}
Write-Host 'All suites passed.' -ForegroundColor Green
exit 0
