<#
.SYNOPSIS
  Builds mexporter release archives for GitHub Releases.

.DESCRIPTION
  Cross-compiles the CLI for windows/amd64 and linux/amd64 with CGO disabled,
  stages each binary together with README.md, a ready-to-use mexporter.json
  (copied from mexporter.example.json) and empty session/ + mail-export/
  folders, then packs:
    dist/mexporter-<version>-windows-x64.zip
    dist/mexporter-<version>-linux-x64.tar.gz

.EXAMPLE
  .\scripts\package-release.ps1
  .\scripts\package-release.ps1 -Version v1.2.3
#>
param(
    [string]$Version = ''
)

$ErrorActionPreference = 'Stop'

$root = Split-Path -Parent $PSScriptRoot
if ($Version -eq '') {
    $Version = git -C $root describe --tags --always
    if (-not $Version) { $Version = 'dev' }
}

$goDir   = Join-Path $root 'go'
$dist    = Join-Path $root 'dist'
$example = Join-Path $root 'mexporter.example.json'
$readme  = Join-Path $root 'README.md'

Remove-Item $dist -Recurse -Force -ErrorAction SilentlyContinue
New-Item -ItemType Directory -Force -Path $dist | Out-Null

function New-Stage([string]$name) {
    $stage = Join-Path $dist "stage/$name"
    New-Item -ItemType Directory -Force -Path $stage | Out-Null
    Copy-Item $readme $stage
    Copy-Item $example (Join-Path $stage 'mexporter.json')
    foreach ($dir in @('session', 'mail-export')) {
        $path = Join-Path $stage $dir
        New-Item -ItemType Directory -Force -Path $path | Out-Null
        Set-Content -Path (Join-Path $path '.gitkeep') -Value ''
    }
    return $stage
}

function Build-Go([string]$goos, [string]$goarch, [string]$out) {
    $env:GOOS = $goos
    $env:GOARCH = $goarch
    $env:CGO_ENABLED = '0'
    Push-Location $goDir
    try {
        go build -trimpath -ldflags '-s -w' -o $out ./cmd/mexporter
        if ($LASTEXITCODE -ne 0) { throw "go build failed for $goos/$goarch (exit $LASTEXITCODE)" }
    }
    finally {
        Pop-Location
        Remove-Item Env:GOOS, Env:GOARCH, Env:CGO_ENABLED -ErrorAction SilentlyContinue
    }
}

$winStage = New-Stage 'windows-x64'
Build-Go 'windows' 'amd64' (Join-Path $winStage 'mexporter.exe')
$winZip = Join-Path $dist "mexporter-$Version-windows-x64.zip"
Compress-Archive -Path (Join-Path $winStage '*') -DestinationPath $winZip -Force

$linStage = New-Stage 'linux-x64'
Build-Go 'linux' 'amd64' (Join-Path $linStage 'mexporter')
$linTar = Join-Path $dist "mexporter-$Version-linux-x64.tar.gz"
tar -czf $linTar -C $linStage mexporter README.md mexporter.json session mail-export
if ($LASTEXITCODE -ne 0) { throw "tar failed (exit $LASTEXITCODE)" }

Write-Host "Built release archives (version $Version):"
Write-Host "  $winZip"
Write-Host "  $linTar"
