<#
.SYNOPSIS
  Phase 10 — the BIOS setting, then the real wake test.

.DESCRIPTION
  One BIOS setting makes the whole smart-plug method work: Restore on AC Power
  Loss = Power On. Without it, restoring power leaves the PC sitting there
  doing nothing.

  This phase reboots. Everything up to here is saved in setup-state.json, so
  after the reboot you resume with:  .\setup\run-all.ps1 -From 10

  ⚠️ Confirms before rebooting.
#>
[CmdletBinding(SupportsShouldProcess)]
param(
    [switch]$Yes,
    # Skip the reboot, if you would rather enter the BIOS yourself later.
    [switch]$NoReboot
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

Write-BurrowBanner -Phase '10' -Title 'BIOS and the wake test'
Assert-BurrowElevated

$findings = Get-BurrowStateValue -Phase '00-preflight' -Name 'findings'
$board = if ($findings) { $findings.motherboard } else { 'your motherboard' }
$vendor = if ($findings) { $findings.biosVendor } else { '' }
$uefi = if ($findings) { $findings.canRebootToFirmware } else { $false }

# The menu path differs by vendor, so this names the ones that are common
# rather than pretending there is one answer.
$hint = switch -Regex ("$board $vendor") {
    'Gigabyte|AMI'   { 'Settings -> Platform Power -> AC BACK  (set to "Always On")' }
    'ASUS'           { 'Advanced -> APM Configuration -> Restore AC Power Loss  (set to "Power On")' }
    'MSI'            { 'Settings -> Advanced -> Power Management Setup -> Restore after AC Power Loss  (set to "Power On")' }
    'ASRock'         { 'Advanced -> ACPI Configuration -> Restore on AC/Power Loss  (set to "Power On")' }
    'Dell'           { 'Power Management -> AC Recovery  (set to "Power On")' }
    'Hewlett|HP'     { 'Advanced -> Power-On Options -> After Power Loss  (set to "Power On")' }
    'Lenovo'         { 'Power -> After Power Loss  (set to "Power On")' }
    default          { 'Look under Power, APM, or ACPI for a setting named like "Restore on AC Power Loss" or "AC BACK"' }
}

Write-Host ''
Write-Host "  Motherboard: $board" -ForegroundColor White
Write-Host "  BIOS vendor: $vendor" -ForegroundColor White
Write-Host ''
Write-Host '  The setting you are looking for:' -ForegroundColor White
Write-Host "    $hint" -ForegroundColor Cyan
Write-Host ''
Write-Host '  It is named differently on every board. You want the option that makes' -ForegroundColor Gray
Write-Host '  the PC power ON by itself when mains power comes back — not "Last State".' -ForegroundColor Gray

if (-not $NoReboot) {
    $ok = Confirm-BurrowChange -Yes:$Yes `
        -What 'Reboot now into the BIOS/firmware settings' `
        -Why 'Everything so far is saved. Close anything you have open first; this reboots the PC in 5 seconds.'

    if ($ok) {
        Set-BurrowStateValue -Phase '10-bios' -Name 'rebootedForBiosAt' `
            -Value ((Get-Date).ToUniversalTime().ToString('o'))

        Write-Host ''
        Write-Host '  After you save the BIOS setting and Windows boots again, run:' -ForegroundColor Yellow
        Write-Host '    cd C:\dev\burrow' -ForegroundColor White
        Write-Host '    .\setup\run-all.ps1 -From 10' -ForegroundColor White
        Write-Host ''

        if ($PSCmdlet.ShouldProcess('this PC', 'reboot into firmware settings')) {
            if ($uefi) {
                & shutdown /r /fw /t 5 /c 'Burrow setup: entering firmware settings'
            } else {
                Write-BurrowLog 'this is not a UEFI system; press Del or F2 repeatedly as it boots' -Level Warn
                & shutdown /r /t 10 /c 'Burrow setup: press Del or F2 to enter BIOS'
            }
            return
        }
    }
}

# ------------------------------------------------------------ wake test ---

Write-Host ''
Write-BurrowLog 'Now the test that proves the whole chain works.' -Level Step

Show-BurrowManualStep -Title 'Wake test' -Steps @(
    'On the PC: shut Windows down normally (Start -> Power -> Shut down). Wait until the fans and lights are off.',
    'On the iPhone: run the "PC Off" shortcut. The plug clicks off.',
    'Wait 10 seconds.',
    'On the iPhone: run the "PC On" shortcut. The plug clicks on and the PC should start by itself.',
    'If it does not start, the BIOS setting did not take. Go back into the BIOS and check it.',
    'Once Windows has booted (you do NOT need to sign in), open Burrow on the iPhone. It should go Offline -> Waking -> Online within about 3 minutes.'
) -ThenPress 'Press Enter once the PC came back by itself and Burrow shows Online'

Show-BurrowManualStep -Title 'Function test from the iPhone' -Steps @(
    'Burrow -> New Claude chat -> pick a project -> Start -> Open in Claude.',
    'Send "list the files here" and check it answers.',
    'Burrow -> Files -> open a photo.',
    'Burrow -> Open terminal -> Termius connects with the key, no password.'
)

Show-BurrowManualStep -Title 'Function test from the laptop' -Steps @(
    'Open the Burrow web app and unlock with Windows Hello or Touch ID.',
    'ssh burrow-pc',
    'Open one of the mapped shares and save a file.',
    'Remote desktop, if you set it up.',
    'New Claude chat, then continue it at claude.ai/code.'
)

Set-BurrowStateValue -Phase '10-bios' -Name 'wakeTestPassed' -Value $true -WhatIf:$WhatIfPreference
if (-not $WhatIfPreference) { Set-BurrowPhaseComplete -Phase '10-bios' }
