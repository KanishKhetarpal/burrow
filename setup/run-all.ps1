<#
.SYNOPSIS
  Run the setup phases in order, with the option to resume.

.DESCRIPTION
  Phase 10 reboots the PC, so setup is resumable by design: each phase records
  its completion in C:\ProgramData\Burrow\setup-state.json and this script
  skips what is already done.

  Start with -WhatIf. It walks the whole sequence without changing anything and
  shows you what each phase would do.

.EXAMPLE
  .\setup\run-all.ps1 -WhatIf          # the plan, no changes
  .\setup\run-all.ps1                  # run from the first incomplete phase
  .\setup\run-all.ps1 -From 10         # resume after the BIOS reboot
  .\setup\run-all.ps1 -Only 5          # just one phase
#>
[CmdletBinding(SupportsShouldProcess)]
param(
    [int]$From = 0,
    [int]$To = 12,
    [int]$Only = -1,
    # Answer every CONFIRM with yes. Read the runbook before using this.
    [switch]$Yes,
    # Re-run phases that are already marked complete.
    [switch]$Force,
    [ValidateSet('none', 'rdp', 'sunshine')][string]$RemoteDesktop = 'none',
    [switch]$KeepAutogroupSelf
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

$phases = @(
    @{ N = 0;  Id = '00-preflight';     Script = '00-preflight.ps1';       Needs = @() }
    @{ N = 1;  Id = '01-toolchain';     Script = '01-toolchain.ps1';       Needs = @('elevated') }
    @{ N = 2;  Id = '02-build-install'; Script = '02-build-install.ps1';   Needs = @('elevated') }
    @{ N = 3;  Id = '03-tailnet';       Script = '03-tailnet.ps1';         Needs = @('elevated', 'tsapi') }
    @{ N = 4;  Id = '04-devices';       Script = '04-devices.ps1';         Needs = @('elevated', 'tsapi') }
    @{ N = 5;  Id = '05-hardening';     Script = '05-hardening.ps1';       Needs = @('elevated') }
    @{ N = 6;  Id = '06-claude';        Script = '06-claude.ps1';          Needs = @() }
    @{ N = 7;  Id = '07-service';       Script = '07-service.ps1';         Needs = @('elevated', 'tsapi') }
    @{ N = 8;  Id = '08-wake';          Script = '08-wake.ps1';            Needs = @() }
    @{ N = 9;  Id = '09-pair';          Script = '09-pair.ps1';            Needs = @() }
    @{ N = 10; Id = '10-bios';          Script = '10-bios-waketest.ps1';   Needs = @('elevated') }
    @{ N = 11; Id = '11-lockdown';      Script = '11-lockdown.ps1';        Needs = @() }
    @{ N = 12; Id = '12-verify';        Script = '12-verify.ps1';          Needs = @() }
)

if ($Only -ge 0) { $From = $Only; $To = $Only }

Write-Host ''
Write-Host '  Burrow setup' -ForegroundColor White
Write-Host '  ────────────' -ForegroundColor DarkGray
if ($WhatIfPreference) {
    Write-Host '  -WhatIf: nothing will be changed.' -ForegroundColor Cyan
}
Write-Host ''

# Show where things stand before doing anything, so a resume is obvious.
foreach ($p in $phases) {
    $done = Test-BurrowPhaseComplete -Phase $p.Id
    $mark = if ($done) { 'done' } else { '    ' }
    $inRange = $p.N -ge $From -and $p.N -le $To
    $colour = if ($done) { 'DarkGray' } elseif ($inRange) { 'White' } else { 'DarkGray' }
    Write-Host ("   {0}  {1,2}  {2}" -f $mark, $p.N, $p.Id) -ForegroundColor $colour
}
Write-Host ''

$failed = $null

foreach ($p in $phases) {
    if ($p.N -lt $From -or $p.N -gt $To) { continue }

    if ((Test-BurrowPhaseComplete -Phase $p.Id) -and -not $Force) {
        Write-BurrowLog "phase $($p.N) ($($p.Id)) is already complete — skipping (-Force to re-run)" -Level Info
        continue
    }

    if ($p.Needs -contains 'elevated' -and -not (Test-BurrowElevated)) {
        throw "Phase $($p.N) needs an elevated terminal. Re-open PowerShell with Run as administrator."
    }
    if ($p.Needs -contains 'tsapi' -and -not $env:TS_API_KEY) {
        throw "Phase $($p.N) needs `$env:TS_API_KEY. See docs/06-SETUP-RUNBOOK.md 'Before you start'."
    }

    $script = Join-Path $PSScriptRoot $p.Script

    # Only pass the parameters a given script actually declares, so adding a
    # switch to one phase does not break the others.
    $params = @{}
    $declared = (Get-Command $script).Parameters.Keys
    if ($declared -contains 'Yes' -and $Yes) { $params.Yes = $true }
    if ($declared -contains 'RemoteDesktop') { $params.RemoteDesktop = $RemoteDesktop }
    if ($declared -contains 'KeepAutogroupSelf' -and $KeepAutogroupSelf) { $params.KeepAutogroupSelf = $true }
    if ($declared -contains 'WhatIf' -and $WhatIfPreference) { $params.WhatIf = $true }

    try {
        & $script @params
    } catch {
        $failed = "phase $($p.N) ($($p.Id)): $($_.Exception.Message)"
        Write-Host ''
        Write-BurrowLog $failed -Level Fail
        break
    }

    # Phase 10 reboots; anything after it runs in the next session.
    if ($p.N -eq 10 -and -not $WhatIfPreference) {
        $rebooted = Get-BurrowStateValue -Phase '10-bios' -Name 'rebootedForBiosAt'
        $tested = Get-BurrowStateValue -Phase '10-bios' -Name 'wakeTestPassed'
        if ($rebooted -and -not $tested) {
            Write-Host ''
            Write-BurrowLog 'the PC is about to reboot into the BIOS. Resume afterwards with:' -Level Info
            Write-Host '    .\setup\run-all.ps1 -From 10' -ForegroundColor White
            return
        }
    }
}

Write-Host ''
if ($failed) {
    Write-Host "  Stopped: $failed" -ForegroundColor Red
    Write-Host '  Fix it, then re-run. Completed phases are skipped automatically.' -ForegroundColor Gray
    exit 1
}

Write-Host '  All requested phases finished.' -ForegroundColor Green
if ($To -ge 12 -and -not $WhatIfPreference) {
    Write-Host '  Read SETUP-REPORT.md, then do the manual tests it lists.' -ForegroundColor White
}
