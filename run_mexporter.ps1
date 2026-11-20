#!/usr/bin/env pwsh
# Build and run mexporter from the project directory.
# If the binary is missing, it is built from ./go.

$ErrorActionPreference = 'Stop'

$root = Split-Path -Parent -Path $MyInvocation.MyCommand.Definition
if (-not $root) {
    $root = (Get-Location).Path
}
Set-Location $root

$bin = Join-Path $root 'go\mexporter.exe'

if (-not (Test-Path -LiteralPath $bin)) {
    Write-Host 'Binary not found, building...' -ForegroundColor Cyan
    Push-Location (Join-Path $root 'go')
    try {
        & go build -o mexporter.exe ./cmd/mexporter
        if ($LASTEXITCODE -ne 0) {
            throw "go build failed with exit code $LASTEXITCODE"
        }
    }
    finally {
        Pop-Location
    }
}

if ($args.Count -eq 0) {
    & $bin
} else {
    & $bin @args
}

$exitCode = $LASTEXITCODE
Write-Host "Done. Exit code: $exitCode" -ForegroundColor Yellow
exit $exitCode
