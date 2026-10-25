<#
.SYNOPSIS
  Build Burrow: the PWA, then the agent and waker binaries.

.DESCRIPTION
  The PWA is built first and copied into cmd/burrow/webdist, which the agent
  embeds with go:embed. That ordering matters: a stale or missing webdist
  produces a binary that serves the API but says the app is not bundled.

  Nothing here needs elevation or touches the system.
#>
[CmdletBinding()]
param(
    # Stamped into the binary and into the service worker's cache name.
    [string]$Version = $(if ($env:BURROW_VERSION) { $env:BURROW_VERSION } else { '1.0.0' }),

    # Skip the PWA build, for a quick Go-only iteration.
    [switch]$SkipWeb,

    # Also cross-compile the waker for a future Raspberry Pi.
    [switch]$WakerTargets,

    [string]$OutDir = (Join-Path $PSScriptRoot '..\dist')
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

$repo = Resolve-Path (Join-Path $PSScriptRoot '..')
$webDist = Join-Path $repo 'cmd\burrow\webdist'
$null = New-Item -ItemType Directory -Force -Path $OutDir

function Write-Step([string]$Text) {
    Write-Host ''
    Write-Host "==> $Text" -ForegroundColor Cyan
}

# ---------- the PWA ----------

if (-not $SkipWeb) {
    Write-Step 'Building the web app'
    Push-Location (Join-Path $repo 'web')
    try {
        if (-not (Test-Path 'node_modules')) {
            npm ci --no-fund --no-audit
            if ($LASTEXITCODE -ne 0) { throw 'npm ci failed' }
        }
        $env:BURROW_VERSION = $Version
        npm run build
        if ($LASTEXITCODE -ne 0) { throw 'the web build failed' }
    } finally {
        Pop-Location
    }

    Write-Step 'Staging the web build for embedding'
    # Cleared rather than merged: a leftover file from a previous build would
    # be embedded and served forever.
    if (Test-Path $webDist) { Remove-Item $webDist -Recurse -Force }
    $null = New-Item -ItemType Directory -Force -Path $webDist
    Copy-Item (Join-Path $repo 'web\dist\*') $webDist -Recurse -Force

    if (-not (Test-Path (Join-Path $webDist 'index.html'))) {
        throw 'the web build produced no index.html'
    }
}

# ---------- the binaries ----------

$ldflags = "-s -w -X burrow/internal/web.Version=$Version"

Write-Step "Building burrow.exe ($Version)"
Push-Location $repo
try {
    $env:CGO_ENABLED = '0'
    & go build -trimpath -ldflags $ldflags -o (Join-Path $OutDir 'burrow.exe') ./cmd/burrow
    if ($LASTEXITCODE -ne 0) { throw 'go build failed' }

    Write-Step 'Building burrow-waker.exe (local test build)'
    & go build -trimpath -ldflags $ldflags -o (Join-Path $OutDir 'burrow-waker.exe') ./cmd/burrow-waker
    if ($LASTEXITCODE -ne 0) { throw 'go build (waker) failed' }

    if ($WakerTargets) {
        # The waker is not deployed in v1; these exist so a Raspberry Pi can be
        # added later without revisiting the build (docs/02-ARCHITECTURE.md D7).
        $targets = @(
            @{ os = 'linux'; arch = 'arm64'; name = 'burrow-waker-linux-arm64' },
            @{ os = 'linux'; arch = 'arm';   name = 'burrow-waker-linux-arm' },
            @{ os = 'linux'; arch = 'amd64'; name = 'burrow-waker-linux-amd64' },
            @{ os = 'android'; arch = 'arm64'; name = 'burrow-waker-android-arm64' }
        )
        foreach ($t in $targets) {
            Write-Step "Cross-compiling the waker for $($t.os)/$($t.arch)"
            $env:GOOS = $t.os
            $env:GOARCH = $t.arch
            & go build -trimpath -ldflags $ldflags -o (Join-Path $OutDir $t.name) ./cmd/burrow-waker
            if ($LASTEXITCODE -ne 0) { Write-Warning "could not build for $($t.os)/$($t.arch)" }
        }
        Remove-Item Env:GOOS, Env:GOARCH -ErrorAction SilentlyContinue
    }
} finally {
    Pop-Location
}

Write-Step 'Done'
Get-ChildItem $OutDir | Select-Object Name, @{n = 'Size'; e = { '{0:N1} MB' -f ($_.Length / 1MB) } } | Format-Table -AutoSize
Write-Host "Version: $Version" -ForegroundColor Green
