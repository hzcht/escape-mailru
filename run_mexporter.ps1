#!/usr/bin/env pwsh
# Wrapper script to build and run mexporter — a Mail.ru / VK WorkSpace mailbox exporter to .eml.
# Run from: E:\proj\mexporter
# On first run it builds go\mexporter.exe if binary is missing.

# Change to script directory
$here = Split-Path -Parent -Path $MyInvocation.MyCommand.Definition
if (-not $here) { $here = "E:\proj\mexporter" }
Set-Location $here

# Build Go binary if needed
$bin = "go\mexporter.exe"
if (-not (Test-Path $bin)) {
    Write-Host "Binary not found, building..." -ForegroundColor Cyan
    & powershell -NoProfile -Command "Set-Location 'E:\proj\mexporter\go'; go build -o mexporter.exe ./cmd/mexporter"
    if ($LASTEXITCODE -ne 0) {
        Write-Error "Go build failed. Install Go 1.27+ and ensure `go` is on PATH."
        exit 1
    }
}

# Run mexporter with all passed arguments
if ($args.Count -eq 0) {
    & $bin
} else {
    & $bin $args
}

Write-Host "Done. Exit code: $LASTEXITCODE" -ForegroundColor Yellow