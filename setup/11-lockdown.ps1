<#
.SYNOPSIS
  Phase 11 — the last tightening: Tailnet Lock, router check, revoke the API token.

.DESCRIPTION
  Each item here is optional on its own and each one closes a different path.
  Tailnet Lock is the strongest: it means a stolen Tailscale login cannot add a
  machine to your tailnet, because new nodes must be signed by a key that lives
  on your devices rather than in your account (docs/03-SECURITY.md T3, T4).

  Revoking the API token at the end matters more than it sounds: that token can
  rewrite your ACL policy, and setup has no further use for it.
#>
[CmdletBinding(SupportsShouldProcess)]
param([switch]$Yes)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest
# Import-Module has no -WhatIf in Windows PowerShell 5.1, and under -WhatIf
# the modules it pulls in preview every Set-Alias they do. Suppressed
# around the import only: importing a module changes nothing.
$prevWhatIf = $WhatIfPreference
$WhatIfPreference = $false
Import-Module (Join-Path $PSScriptRoot 'lib\Burrow.Setup.psm1') -Force
$WhatIfPreference = $prevWhatIf

Write-BurrowBanner -Phase '11' -Title 'Lock it down'

# ------------------------------------------------------------ BitLocker ---

$bl = 'unknown'
try { $bl = (Get-BitLockerVolume -MountPoint $env:SystemDrive).ProtectionStatus.ToString() } catch {}
if ($bl -ne 'On') {
    Write-Host ''
    Write-BurrowLog 'BitLocker is not on for the system drive.' -Level Warn
    Write-Host '      Burrow keeps device keys, the passkey database and the audit log on' -ForegroundColor Gray
    Write-Host '      this disk. Without disk encryption, anyone who takes the machine has' -ForegroundColor Gray
    Write-Host '      them. Turning it on is a few minutes in Settings -> Privacy & security.' -ForegroundColor Gray
    Write-Host '      Not automated here: it needs a recovery key you must save somewhere safe.' -ForegroundColor Gray
} else {
    Write-BurrowLog 'BitLocker is on' -Level Ok
}

# ---------------------------------------------------------- Tailnet Lock ---

$ok = Confirm-BurrowChange -Yes:$Yes `
    -What 'Set up Tailnet Lock' `
    -Why 'Recommended. It stops any new device joining your tailnet unless an existing signing node approves it, so a stolen Tailscale password is not enough to reach your PC. If you lose every signing device you need the disablement secret to recover.'

if ($ok) {
    $ts = Get-BurrowTailscaleCli
    Show-BurrowManualStep -Title 'Tailnet Lock' -Steps @(
        "On this PC, run:  & '$ts' lock init",
        'It prints a list of signing keys and a DISABLEMENT SECRET.',
        'Save the disablement secret in iOS Passwords (or a password manager) right now. Without it, losing your signing devices means rebuilding the tailnet.',
        'On the iPhone: Tailscale app -> Settings -> Tailnet Lock -> add this device as a signing node.',
        'Then approve the devices that are already on the tailnet when prompted.',
        'Check it with:  tailscale lock status'
    )
    Set-BurrowStateValue -Phase '11-lockdown' -Name 'tailnetLock' -Value 'attempted' -WhatIf:$WhatIfPreference
} else {
    Write-BurrowLog 'skipped Tailnet Lock' -Level Info
    Set-BurrowStateValue -Phase '11-lockdown' -Name 'tailnetLock' -Value 'skipped' -WhatIf:$WhatIfPreference
}

# -------------------------------------------------------- identity 2FA ---

Show-BurrowManualStep -Title 'Protect the three accounts that can undo all of this' -Steps @(
    'Tailscale: your identity provider account (Google/Microsoft/GitHub) — turn on a passkey or 2FA. Whoever holds it can rewrite the ACL.',
    'claude.ai: Settings -> Account -> passkey or 2FA, and confirm "Require trusted devices" is on from Phase 6.',
    'Your smart plug app: 2FA. Whoever holds it can cut power to your PC (but gets no data).'
)

# ------------------------------------------------------------- router ---

$findings = Get-BurrowStateValue -Phase '00-preflight' -Name 'findings'
$gw = if ($findings) { $findings.gateway } else { 'your router' }

Show-BurrowManualStep -Title 'Check the router (two minutes, and worth it)' -Steps @(
    "Open http://$gw in a browser and sign in.",
    'Find UPnP and turn it OFF. UPnP lets any program on your LAN open a port to the internet without asking.',
    'Find Port Forwarding / Virtual Servers and confirm there are NO rules pointing at this PC.',
    'Optional: give this PC a DHCP reservation so its LAN address stays put.',
    'Burrow needs no port forward and no UPnP. If you find one for this PC, remove it.'
)

# ------------------------------------------------------ the API token ---

Write-Host ''
Write-BurrowLog 'The Tailscale API token has done its job.' -Level Step
Write-Host '      It can rewrite your ACL policy and create auth keys. Setup does not' -ForegroundColor Gray
Write-Host '      need it again; Burrow itself never uses it.' -ForegroundColor Gray

Show-BurrowManualStep -Title 'Revoke the API token' -Steps @(
    'Open https://login.tailscale.com/admin/settings/keys',
    'Find the API access token you created for this setup and revoke it.',
    'If you need one later, make a new short-lived token then.'
)

if ($PSCmdlet.ShouldProcess('TS_API_KEY', 'remove from this process environment')) {
    Remove-Item Env:TS_API_KEY -ErrorAction SilentlyContinue
    Write-BurrowLog 'TS_API_KEY cleared from this process' -Level Ok
    Write-BurrowLog 'if you set it with setx or in your profile, remove it there too' -Level Warn
}

if (-not $WhatIfPreference) { Set-BurrowPhaseComplete -Phase '11-lockdown' }
