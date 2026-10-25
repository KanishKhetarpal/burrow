<#
.SYNOPSIS
  Phase 9 — pair the iPhone and the laptop.

.DESCRIPTION
  Runs `burrow pair` in a visible window for each device. That opens a
  ten-minute, single-use window and prints a QR code and a six-digit code on
  this screen.

  The code is safe precisely because the window can only be opened here: an
  attacker on the tailnet cannot start one, it lasts minutes, and five wrong
  guesses close it (docs/03-SECURITY.md T10). So do not send the code anywhere
  — read it off this screen and type it on the device.
#>
[CmdletBinding(SupportsShouldProcess)]
param(
    [string]$InstallDir = 'C:\Program Files\Burrow',
    # Pair only one device, when re-pairing a replacement.
    [ValidateSet('both', 'phone', 'laptop')][string]$Device = 'both'
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

Write-BurrowBanner -Phase '9' -Title 'Pair the iPhone and the laptop'

$burrow = Join-Path $InstallDir 'burrow.exe'
if (-not (Test-Path $burrow)) { throw "burrow.exe is not installed at $burrow." }

& $burrow status
if ($LASTEXITCODE -ne 0) { throw 'the Burrow service is not answering. Run Phase 7 first.' }

# The laptop fetches its setup script through Burrow -> Files -> Downloads once
# it is paired (docs/06-SETUP-RUNBOOK.md Phase 9), so it has to be there first,
# with the PC name and user filled in — the laptop cannot look those up itself.
$pcMagicDNS = Get-BurrowStateValue -Phase '03-tailnet' -Name 'pcMagicDNS'
$laptopDir = Join-Path $env:USERPROFILE 'Downloads\Burrow-Laptop'
if ($PSCmdlet.ShouldProcess($laptopDir, 'copy the laptop setup scripts')) {
    New-Item -ItemType Directory -Force -Path $laptopDir | Out-Null
    Copy-Item -Path (Join-Path $PSScriptRoot '..\laptop\*') -Destination $laptopDir -Force
    $pcName = if ($pcMagicDNS) { $pcMagicDNS } else { '<the PC name from Phase 3>' }
    @(
        'Run the one for your laptop:',
        '',
        "  Windows:  powershell -ExecutionPolicy Bypass -File .\setup-windows.ps1 -PcName $pcName -PcUser $env:USERNAME",
        "  macOS:    bash ./setup-macos.sh $pcName $env:USERNAME",
        '',
        'It prints a public key at the end: add it in Burrow -> More -> Security -> Add SSH key.'
    ) | Set-Content -Path (Join-Path $laptopDir 'README.txt') -Encoding UTF8
    Write-BurrowLog "laptop scripts copied to $laptopDir" -Level Ok
}

function Invoke-Pairing {
    param([string]$Label, [string[]]$DeviceSteps)

    Write-Host ''
    Write-BurrowLog "pairing the $Label" -Level Step

    if (-not $PSCmdlet.ShouldProcess($Label, 'open a pairing window')) { return }

    # A separate window: the QR code needs room, and keeping the code out of
    # this transcript is the point.
    Start-Process -FilePath 'powershell.exe' -ArgumentList @(
        '-NoProfile', '-NoExit', '-Command',
        "& '$burrow' pair"
    )

    Start-Sleep -Seconds 2
    Show-BurrowManualStep -Title "On the $Label" -Steps $DeviceSteps `
        -ThenPress "Press Enter once the $Label says it is paired"

    $devices = & $burrow devices 2>&1
    Write-Host ''
    Write-Host '  Paired devices now:' -ForegroundColor White
    $devices | ForEach-Object { Write-Host "    $_" }
}

if ($Device -in @('both', 'phone')) {
    Invoke-Pairing -Label 'iPhone' -DeviceSteps @(
        'Termius: Keychain -> + -> Generate Key -> ED25519 -> name it "Burrow" -> copy the public key.',
        'Termius: Settings -> turn ON app lock with Face ID.',
        'Safari: open the URL shown in the pairing window (or scan the QR code).',
        'Type the 6-digit code from that window. You get five tries.',
        'Create the passkey when Safari asks — that is the Face ID prompt.',
        'Paste the Termius public key when Burrow offers, then Finish.',
        'Share -> Add to Home Screen, so Burrow opens full-screen and works while the PC is off.'
    )
}

if ($Device -in @('both', 'laptop')) {
    Invoke-Pairing -Label 'laptop' -DeviceSteps @(
        'Generate an SSH key if you do not have one:  ssh-keygen -t ed25519 -f ~/.ssh/burrow_ed25519',
        'Use a passphrase. Copy the contents of ~/.ssh/burrow_ed25519.pub.',
        'Open the pairing URL in your laptop browser.',
        'Type the 6-digit code, then create the passkey (Windows Hello or Touch ID).',
        'Paste the public key, then Finish.',
        'Install the app: Chrome/Edge -> Install app, or Safari -> Add to Dock.',
        'Then Burrow -> Files -> Downloads -> Burrow-Laptop: download README.txt and your script, and run it as README.txt says.'
    )
}

Write-Host ''
Write-BurrowLog 'verifying that pairing wrote the device allowlist' -Level Step
$configPath = 'C:\ProgramData\Burrow\burrow.yaml'
$config = Get-Content $configPath -Raw
# The agent rewrites the file in block style ("- name: ..."), not the
# flow style ("- { name: ... }") setup writes, so match either.
if ($config -match '(?m)^\s*-\s*(\{\s*)?name:') {
    Write-BurrowLog 'the allowlist in burrow.yaml has at least one device' -Level Ok
} else {
    Write-BurrowLog 'burrow.yaml still has an empty device allowlist — pairing did not complete' -Level Fail
    throw 'no device is paired'
}

& $burrow selftest

if (-not $WhatIfPreference) { Set-BurrowPhaseComplete -Phase '09-pair' }
