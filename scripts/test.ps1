<#
.SYNOPSIS
  Run everything that has to pass before Burrow is installed.

.DESCRIPTION
  Go tests (including the Windows ConPTY and restricted-token tests), the path
  resolver fuzzer, govulncheck, the web typecheck, build, unit tests and npm
  audit, and a secret scan over the tree.

  Nothing here needs elevation or touches the system.
#>
[CmdletBinding()]
param(
    # Fuzz the path resolver for this long. The test plan asks for at least 60s.
    [int]$FuzzSeconds = 60,

    # Skip the slow supply-chain checks for a quick inner loop.
    [switch]$Quick
)

$ErrorActionPreference = 'Continue'
Set-StrictMode -Version Latest

$repo = Resolve-Path (Join-Path $PSScriptRoot '..')
$failures = [System.Collections.Generic.List[string]]::new()

function Invoke-Step {
    param([string]$Name, [scriptblock]$Body)
    Write-Host ''
    Write-Host "==> $Name" -ForegroundColor Cyan
    try {
        & $Body
        if ($LASTEXITCODE -ne 0 -and $null -ne $LASTEXITCODE) {
            $failures.Add($Name)
            Write-Host "    FAILED" -ForegroundColor Red
        }
    } catch {
        $failures.Add("$Name ($_)")
        Write-Host "    FAILED: $_" -ForegroundColor Red
    }
}

Push-Location $repo
try {
    # ---------- Go ----------

    Invoke-Step 'gofmt' {
        $unformatted = & gofmt -l ./cmd ./internal
        if ($unformatted) {
            Write-Host ($unformatted -join "`n")
            throw 'files are not gofmt-clean'
        }
    }

    Invoke-Step 'go vet' { & go vet ./... }

    Invoke-Step 'go test' {
        # The ConPTY tests spawn real processes and take about 40s.
        & go test ./... -timeout 600s
    }

    Invoke-Step "path resolver fuzz ($FuzzSeconds s)" {
        # The one invariant that matters: nothing the fuzzer produces may
        # resolve outside its root (docs/07-TEST-PLAN.md §1).
        & go test ./internal/files/ -run FuzzResolve -fuzz FuzzResolve -fuzztime "${FuzzSeconds}s"
    }

    if (-not $Quick) {
        Invoke-Step 'govulncheck' {
            $tool = Get-Command govulncheck -ErrorAction SilentlyContinue
            if (-not $tool) {
                Write-Host '    installing govulncheck…'
                & go install golang.org/x/vuln/cmd/govulncheck@latest
                $gobin = & go env GOPATH
                $tool = Join-Path $gobin 'bin\govulncheck.exe'
            } else {
                $tool = $tool.Source
            }
            & $tool ./...
        }
    }

    # ---------- the web app ----------

    Push-Location (Join-Path $repo 'web')
    try {
        if (-not (Test-Path 'node_modules')) {
            Invoke-Step 'npm ci' { & npm ci --no-fund --no-audit }
        }
        Invoke-Step 'web typecheck' { & npx tsc --noEmit }
        Invoke-Step 'web unit tests' { & npx vitest run }
        Invoke-Step 'web build' { & npx vite build }
        if (-not $Quick) {
            Invoke-Step 'npm audit' { & npm audit --omit=dev --audit-level=high }
        }
    } finally {
        Pop-Location
    }

    # ---------- secrets ----------

    Invoke-Step 'secret scan' {
        # A regex sweep over tracked files. The point is to catch a real key
        # pasted into a config or a test fixture before it is committed
        # (docs/03-SECURITY.md T12).
        $patterns = @(
            'tskey-[A-Za-z0-9\-]{10,}',
            'sk-[A-Za-z0-9_\-]{20,}',
            'gh[pousr]_[A-Za-z0-9]{20,}',
            '-----BEGIN [A-Z ]*PRIVATE KEY-----',
            'eyJ[A-Za-z0-9_\-]{10,}\.[A-Za-z0-9_\-]{10,}\.[A-Za-z0-9_\-]{10,}'
        )
        $tracked = & git ls-files
        $hits = @()
        foreach ($file in $tracked) {
            if (-not (Test-Path $file -PathType Leaf)) { continue }
            # Skip this script, which necessarily contains the patterns.
            if ($file -like '*scripts/test.ps1') { continue }
            $content = Get-Content $file -Raw -ErrorAction SilentlyContinue
            if (-not $content) { continue }
            foreach ($p in $patterns) {
                if ($content -match $p) { $hits += "$file : $($Matches[0].Substring(0, [Math]::Min(18, $Matches[0].Length)))…" }
            }
        }
        if ($hits.Count -gt 0) {
            Write-Host ($hits -join "`n") -ForegroundColor Red
            throw "$($hits.Count) possible secret(s) in tracked files"
        }
        Write-Host '    no secrets found in tracked files'
    }
} finally {
    Pop-Location
}

Write-Host ''
if ($failures.Count -eq 0) {
    Write-Host 'All checks passed.' -ForegroundColor Green
    exit 0
}
Write-Host "$($failures.Count) check(s) failed:" -ForegroundColor Red
$failures | ForEach-Object { Write-Host "  - $_" -ForegroundColor Red }
exit 1
