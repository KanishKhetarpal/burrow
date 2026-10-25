<#
.SYNOPSIS
  Phase 1 — install the toolchain Burrow needs to build and run.

.DESCRIPTION
  Installs only what is missing, from official winget package IDs. Nothing is
  upgraded that is already present: an existing Go or Node is left exactly as
  it is, because this machine is someone's working PC, not a build agent.

  Requires an elevated terminal.

.EXAMPLE
  .\setup\01-toolchain.ps1 -WhatIf     # show what would be installed
  .\setup\01-toolchain.ps1
#>
[CmdletBinding(SupportsShouldProcess)]
param(
    # Install Tailscale too. Separate because it is the one package that adds
    # a service and a network interface.
    [switch]$SkipTailscale
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest
# Import-Module has no -WhatIf in Windows PowerShell 5.1, and under -WhatIf
# the modules it pulls in preview every Set-Alias they do. Suppressed
# around the import only: importing a module changes nothing.
$prevWhatIf = $WhatIfPreference
$WhatIfPreference = $false
Import-Module (Join-Path $PSScriptRoot 'lib\Burrow.Setup.psm1') -Force
$WhatIfPreference = $prevWhatIf

Write-BurrowBanner -Phase '1' -Title 'Toolchain'
Assert-BurrowElevated

if (-not (Get-Command winget -ErrorAction SilentlyContinue)) {
    throw 'winget is not available. Install "App Installer" from the Microsoft Store, then re-run.'
}

# Official IDs only (docs/03-SECURITY.md T14). `go install` for govulncheck is
# handled after Go is on PATH.
$packages = @(
    @{ Id = 'GoLang.Go'; Probe = 'go'; Why = 'builds the agent' }
    @{ Id = 'OpenJS.NodeJS.LTS'; Probe = 'node'; Why = 'builds the web app' }
    @{ Id = 'Git.Git'; Probe = 'git'; Why = 'Claude Code needs Git Bash on Windows' }
    @{ Id = 'Microsoft.PowerShell'; Probe = 'pwsh'; Why = 'the setup scripts and the SSH default shell' }
)
if (-not $SkipTailscale) {
    $packages += @{ Id = 'Tailscale.Tailscale'; Probe = $null; Why = 'the only way in' }
}

$installed = @()
$skipped = @()

foreach ($pkg in $packages) {
    $present = $false
    if ($pkg.Probe) {
        $present = [bool](Get-Command $pkg.Probe -ErrorAction SilentlyContinue)
    } elseif ($pkg.Id -eq 'Tailscale.Tailscale') {
        $present = [bool](Get-BurrowTailscaleCli)
    }

    if ($present) {
        Write-BurrowLog "$($pkg.Id) is already installed — leaving it alone" -Level Ok
        $skipped += $pkg.Id
        continue
    }

    Write-BurrowLog "installing $($pkg.Id) ($($pkg.Why))" -Level Step
    if (-not $PSCmdlet.ShouldProcess($pkg.Id, 'winget install')) { continue }

    & winget install --exact --id $pkg.Id `
        --accept-source-agreements --accept-package-agreements --silent
    if ($LASTEXITCODE -ne 0) {
        # winget returns a non-zero code when a package is already installed by
        # another means, which is not a failure worth stopping setup for.
        Write-BurrowLog "winget exited $LASTEXITCODE for $($pkg.Id); check it by hand if the version below is blank" -Level Warn
    } else {
        $installed += $pkg.Id
    }
    Update-BurrowPath
}

Update-BurrowPath

# govulncheck is a Go tool, so it can only be installed once Go is on PATH.
if (Get-Command go -ErrorAction SilentlyContinue) {
    $gopath = & go env GOPATH
    $gvc = Join-Path $gopath 'bin\govulncheck.exe'
    if (-not (Test-Path $gvc)) {
        Write-BurrowLog 'installing govulncheck' -Level Step
        if ($PSCmdlet.ShouldProcess('govulncheck', 'go install')) {
            & go install golang.org/x/vuln/cmd/govulncheck@latest
            if ($LASTEXITCODE -ne 0) {
                Write-BurrowLog 'govulncheck did not install; scripts\test.ps1 will try again' -Level Warn
            }
        }
    } else {
        Write-BurrowLog 'govulncheck is already installed' -Level Ok
    }
}

Write-Host ''
Write-Host 'Versions' -ForegroundColor White
foreach ($probe in @('go', 'node', 'npm', 'git', 'pwsh')) {
    $cmd = Get-Command $probe -ErrorAction SilentlyContinue
    $ver = '— not found'
    if ($cmd) {
        $flag = if ($probe -eq 'go') { 'version' } else { '--version' }
        try { $ver = (& $cmd.Source $flag 2>&1 | Select-Object -First 1) } catch { $ver = 'present' }
    }
    Write-Host ('  {0,-8} {1}' -f $probe, $ver)
}
$ts = Get-BurrowTailscaleCli
if ($ts) { Write-Host ('  {0,-8} {1}' -f 'tailscale', (& $ts version 2>&1 | Select-Object -First 1)) }

Set-BurrowStateValue -Phase '01-toolchain' -Name 'installed' -Value $installed -WhatIf:$WhatIfPreference
Set-BurrowStateValue -Phase '01-toolchain' -Name 'alreadyPresent' -Value $skipped -WhatIf:$WhatIfPreference

if (-not $WhatIfPreference) { Set-BurrowPhaseComplete -Phase '01-toolchain' }

Write-Host ''
Write-BurrowLog 'PATH was refreshed in this process. A terminal opened before now still has the old PATH.' -Level Info
