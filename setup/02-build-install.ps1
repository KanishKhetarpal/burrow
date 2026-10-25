<#
.SYNOPSIS
  Phase 2 — test, build, and install the binary and its data directories.

.DESCRIPTION
  Runs the full test suite first and refuses to install if it fails: an agent
  that reaches the network is not something to ship on a hope.

  Creates C:\ProgramData\Burrow with inheritance switched off, so the state
  directory, the audit log and the device database are readable only by SYSTEM,
  Administrators and the owner — not by other local accounts, and not by the
  burrowfs account Phase 5 creates (docs/02-ARCHITECTURE.md §4).

  Requires an elevated terminal.
#>
[CmdletBinding(SupportsShouldProcess)]
param(
    [string]$Version = '1.0.0',
    # Install without running the tests. For a re-run after a test pass.
    [switch]$SkipTests,
    [string]$InstallDir = 'C:\Program Files\Burrow'
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

Write-BurrowBanner -Phase '2' -Title 'Build, test and install'
Assert-BurrowElevated

$repo = Resolve-Path (Join-Path $PSScriptRoot '..')
$dataDir = 'C:\ProgramData\Burrow'

# ------------------------------------------------------------------ tests ---

if (-not $SkipTests) {
    Write-BurrowLog 'running the full test suite' -Level Step
    if ($PSCmdlet.ShouldProcess('scripts\test.ps1', 'run')) {
        & (Join-Path $repo 'scripts\test.ps1')
        if ($LASTEXITCODE -ne 0) {
            throw 'the tests failed. Fix them before installing; -SkipTests exists but using it here is a bad idea.'
        }
        Write-BurrowLog 'tests passed' -Level Ok
    }
} else {
    Write-BurrowLog 'skipping tests (-SkipTests)' -Level Warn
}

# ------------------------------------------------------------------ build ---

Write-BurrowLog "building version $Version" -Level Step
if ($PSCmdlet.ShouldProcess('scripts\build.ps1', 'run')) {
    & (Join-Path $repo 'scripts\build.ps1') -Version $Version -WakerTargets
    if ($LASTEXITCODE -ne 0) { throw 'the build failed' }
}

$built = Join-Path $repo 'dist\burrow.exe'
if (-not $WhatIfPreference -and -not (Test-Path $built)) { throw "no binary at $built" }

# ---------------------------------------------------------------- install ---

Write-BurrowLog "installing to $InstallDir" -Level Step
if ($PSCmdlet.ShouldProcess($InstallDir, 'create and copy burrow.exe')) {
    $null = New-Item -ItemType Directory -Force -Path $InstallDir

    # If the service is running it holds the binary open; stop it first rather
    # than failing halfway through a copy.
    $svc = Get-Service -Name 'Burrow' -ErrorAction SilentlyContinue
    if ($svc -and $svc.Status -eq 'Running') {
        Write-BurrowLog 'stopping the running Burrow service to replace the binary' -Level Info
        Stop-Service -Name 'Burrow' -Force
        $svc.WaitForStatus('Stopped', [TimeSpan]::FromSeconds(30))
    }

    Copy-Item $built (Join-Path $InstallDir 'burrow.exe') -Force
    Write-BurrowLog "installed $InstallDir\burrow.exe" -Level Ok
}

# Put it on the machine PATH so `burrow` works from any terminal.
$machinePath = [Environment]::GetEnvironmentVariable('Path', 'Machine')
if ($machinePath -notlike "*$InstallDir*") {
    if ($PSCmdlet.ShouldProcess('machine PATH', "add $InstallDir")) {
        Register-BurrowChange -Tag 'machine-path' -Was $machinePath -Now "$machinePath;$InstallDir" `
            -Revert "remove $InstallDir from the machine PATH" -WhatIf:$WhatIfPreference
        [Environment]::SetEnvironmentVariable('Path', "$machinePath;$InstallDir", 'Machine')
        Update-BurrowPath
        Write-BurrowLog "added $InstallDir to the machine PATH" -Level Ok
    }
}

# ------------------------------------------------------ data directories ---

Write-BurrowLog "creating $dataDir with a restrictive ACL" -Level Step
if ($PSCmdlet.ShouldProcess($dataDir, 'create directories and set ACLs')) {
    foreach ($sub in @('', 'state', 'audit', 'logs', 'backup')) {
        $p = if ($sub) { Join-Path $dataDir $sub } else { $dataDir }
        $null = New-Item -ItemType Directory -Force -Path $p
    }

    $me = "$env:USERDOMAIN\$env:USERNAME"

    # /inheritance:r drops the inherited ACEs that would otherwise let every
    # local user read this tree. Everything below is then explicit.
    & icacls $dataDir /inheritance:r /q | Out-Null
    & icacls $dataDir /grant:r 'SYSTEM:(OI)(CI)F' /q | Out-Null
    & icacls $dataDir /grant:r 'Administrators:(OI)(CI)F' /q | Out-Null
    & icacls $dataDir /grant:r "${me}:(OI)(CI)F" /q | Out-Null

    Write-BurrowLog "ACL: SYSTEM, Administrators and $me only" -Level Ok

    # Prove it rather than assume it: a typo in an icacls argument fails
    # quietly and would leave the audit log world-readable.
    $acl = & icacls $dataDir
    $unexpected = $acl | Select-String -Pattern 'Users:|Everyone:|Authenticated Users:' -SimpleMatch
    if ($unexpected) {
        Write-BurrowLog "unexpected ACL entries on ${dataDir}:" -Level Fail
        $unexpected | ForEach-Object { Write-Host "    $_" -ForegroundColor Red }
        throw 'the data directory ACL is wider than it should be'
    }
}

Set-BurrowStateValue -Phase '02-build-install' -Name 'installDir' -Value $InstallDir -WhatIf:$WhatIfPreference
Set-BurrowStateValue -Phase '02-build-install' -Name 'version' -Value $Version -WhatIf:$WhatIfPreference
Set-BurrowStateValue -Phase '02-build-install' -Name 'dataDir' -Value $dataDir -WhatIf:$WhatIfPreference

if (-not $WhatIfPreference) {
    & (Join-Path $InstallDir 'burrow.exe') version
    Set-BurrowPhaseComplete -Phase '02-build-install'
}
