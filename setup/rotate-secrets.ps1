<#
.SYNOPSIS
  Change the burrowfs share password.

.DESCRIPTION
  Run this after losing a device: that password is stored on the iPhone and the
  laptop, so a lost device means someone may hold it (docs/03-SECURITY.md §7).

  The new password is shown once, in its own window.

  Removing the lost device at login.tailscale.com is the more important step
  and is not done here — without tailnet access the password is useless, but
  rotate it anyway.
#>
[CmdletBinding(SupportsShouldProcess)]
param([int]$Length = 32)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest
# Import-Module has no -WhatIf in Windows PowerShell 5.1, and under -WhatIf
# the modules it pulls in preview every Set-Alias they do. Suppressed
# around the import only: importing a module changes nothing.
$prevWhatIf = $WhatIfPreference
$WhatIfPreference = $false
Import-Module (Join-Path $PSScriptRoot 'lib\Burrow.Setup.psm1') -Force
$WhatIfPreference = $prevWhatIf

Write-BurrowBanner -Phase 'R' -Title 'Rotate the burrowfs password'
Assert-BurrowElevated

$user = Get-LocalUser -Name 'burrowfs' -ErrorAction SilentlyContinue
if (-not $user) { throw 'there is no burrowfs account on this PC.' }

Write-Host ''
Write-Host '  After this, both devices will need the new password the next time they' -ForegroundColor Gray
Write-Host '  connect to a share. Nothing else breaks: Burrow''s own file browsing does' -ForegroundColor Gray
Write-Host '  not use this account.' -ForegroundColor Gray

if (-not $PSCmdlet.ShouldProcess('burrowfs', 'set a new password')) { return }

$pw = New-BurrowPassword -Length $Length
$secure = ConvertTo-SecureString $pw -AsPlainText -Force
Set-LocalUser -Name 'burrowfs' -Password $secure
Write-BurrowLog 'password changed' -Level Ok

Write-BurrowSecretWindow -Title 'New burrowfs password (Burrow Files)' -Secret $pw `
    -Instructions 'Update "Burrow Files" in iOS Passwords and in your laptop credential store.'

$pw = $null
$secure.Dispose()
[GC]::Collect()

Set-BurrowStateValue -Phase 'rotate-secrets' -Name 'lastRotatedAt' `
    -Value ((Get-Date).ToUniversalTime().ToString('o'))

Write-Host ''
Write-Host '  On each device, re-connect to the share and save the new password:' -ForegroundColor White
Write-Host '    iPhone  Files app -> Connect to Server -> smb://<your PC name> -> user burrowfs' -ForegroundColor Gray
Write-Host '    Laptop  re-map the drives, or run laptop/setup-windows.ps1 again' -ForegroundColor Gray
Write-Host ''
Write-Host '  If you are here because a device was lost, also:' -ForegroundColor Yellow
Write-Host '    1. login.tailscale.com -> Machines -> remove it (this is the one that matters)' -ForegroundColor Gray
Write-Host '    2. claude.ai -> Settings -> Account -> Trusted devices -> remove it' -ForegroundColor Gray
Write-Host '    3. burrow devices revoke <name>' -ForegroundColor Gray
