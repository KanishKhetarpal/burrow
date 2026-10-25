<#
.SYNOPSIS
  Phase 8 — smart-plug wake: the wiring, the plug app, the three Shortcuts.

.DESCRIPTION
  Almost all of this is yours to do — a plug has to be plugged in and an iPhone
  tapped. What the script does is check the names afterwards, because the whole
  wake flow turns on three Shortcut names matching exactly.

  The wiring note is the one that bites: if the plug sits before a UPS, the UPS
  keeps the PC powered and cutting the plug does nothing (docs/01-PRD.md §7).
#>
[CmdletBinding(SupportsShouldProcess)]
param(
    [string]$InstallDir = 'C:\Program Files\Burrow',
    [string]$OnName = 'PC On',
    [string]$OffName = 'PC Off',
    [string]$CycleName = 'PC Power Cycle'
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

Write-BurrowBanner -Phase '8' -Title 'Smart plug wake'

Show-BurrowManualStep -Title 'Wire the plug (this order matters)' -Steps @(
    'wall socket -> (your UPS or inverter output, if you use one) -> SMART PLUG -> PC power cable.',
    'The plug must be the LAST thing before the PC.',
    'If the plug sits before a UPS, the UPS keeps the PC powered and wake will never work.',
    'Plug the PC back in and check it still powers on normally.'
)

Show-BurrowManualStep -Title 'Set up the plug in its own app' -Steps @(
    'Add the plug on 2.4 GHz Wi-Fi (most of these plugs do not support 5 GHz).',
    'Turn Wi-Fi off on your phone and check you can still switch the plug over mobile data. If you cannot, remote wake will not work from outside the house.',
    'Turn ON two-factor authentication for the plug app account. Whoever holds that account can cut power to your PC.',
    'If the app has a "power-on state after outage" setting, set it to Last state (or Off).',
    'Name the device exactly: PC Plug'
)

Show-BurrowManualStep -Title "Create three Shortcuts — the names must match exactly" -Steps @(
    "Shortcuts app -> + -> name it `"$OnName`" -> add the plug action: turn PC Plug ON.",
    "New shortcut `"$OffName`" -> turn PC Plug OFF.",
    "New shortcut `"$CycleName`" -> turn PC Plug OFF -> Wait 15 seconds -> turn PC Plug ON.",
    'Tapo plugs offer these actions in Shortcuts directly. For other brands, make on/off scenes in the plug app, tap "Add to Siri", then use them here.',
    'Test each one by tapping it. "Hey Siri, PC On" should also work.'
)

# The names go into the config, and the app reads them from /api/wake-config.
# A mismatch is silent: the Shortcut link simply does nothing.
$configPath = 'C:\ProgramData\Burrow\burrow.yaml'
if (-not (Test-Path $configPath)) { throw "no config at $configPath. Run Phase 7 first." }

if ($PSCmdlet.ShouldProcess($configPath, 'write the shortcut names')) {
    Backup-BurrowItem -Path $configPath -Tag 'burrow-yaml-wake' -WhatIf:$WhatIfPreference
    $yaml = Get-Content $configPath -Raw

    $yaml = $yaml -replace '(?m)^(\s*method:\s*).*$', "`${1}`"smartplug`""
    $yaml = $yaml -replace '(?m)^(\s*on:\s*).*$', "`${1}`"$OnName`""
    $yaml = $yaml -replace '(?m)^(\s*off:\s*).*$', "`${1}`"$OffName`""
    $yaml = $yaml -replace '(?m)^(\s*power_cycle:\s*).*$', "`${1}`"$CycleName`""

    Set-Content -Path $configPath -Value $yaml -Encoding utf8
    Write-BurrowLog "wake.method = smartplug, shortcuts: $OnName / $OffName / $CycleName" -Level Ok
}

if ($PSCmdlet.ShouldProcess('Burrow service', 'restart to pick up the config')) {
    Restart-Service -Name 'Burrow' -Force -ErrorAction SilentlyContinue
    Start-Sleep -Seconds 5
    $burrow = Join-Path $InstallDir 'burrow.exe'
    & $burrow status
}

Write-Host ''
Write-BurrowLog 'BIOS "Restore on AC Power Loss" is set in Phase 10. Until then the plug turning on will not boot the PC.' -Level Info

Set-BurrowStateValue -Phase '08-wake' -Name 'shortcuts' -Value ([pscustomobject]@{
        on = $OnName; off = $OffName; powerCycle = $CycleName
    }) -WhatIf:$WhatIfPreference

if (-not $WhatIfPreference) { Set-BurrowPhaseComplete -Phase '08-wake' }
