<#
.SYNOPSIS
  Undo everything Burrow's setup changed on this PC.

.DESCRIPTION
  Works from setup-state.json, which records every change with how to revert
  it. Anything not recorded there is not touched — this will not guess at what
  the machine looked like before.

  ⚠️ Confirms before each group. -WhatIf shows the whole plan without doing
  anything, which is worth running first.

  It does NOT remove your Tailscale devices or the tailnet ACL policy: those
  are account-level and yours to decide on. It prints what to remove and where.
#>
[CmdletBinding(SupportsShouldProcess)]
param(
    [switch]$Yes,
    # Also delete C:\ProgramData\Burrow, including the audit log and backups.
    [switch]$RemoveData,
    [string]$InstallDir = 'C:\Program Files\Burrow'
)

$ErrorActionPreference = 'Continue'
Set-StrictMode -Version Latest
# Import-Module has no -WhatIf in Windows PowerShell 5.1, and under -WhatIf
# the modules it pulls in preview every Set-Alias they do. Suppressed
# around the import only: importing a module changes nothing.
$prevWhatIf = $WhatIfPreference
$WhatIfPreference = $false
Import-Module (Join-Path $PSScriptRoot 'lib\Burrow.Setup.psm1') -Force
$WhatIfPreference = $prevWhatIf

Write-BurrowBanner -Phase 'U' -Title 'Uninstall'
Assert-BurrowElevated

$state = Get-BurrowState
$dataDir = 'C:\ProgramData\Burrow'

Write-Host ''
Write-Host '  This will undo:' -ForegroundColor White
Write-Host '    - the Burrow service and binary' -ForegroundColor Gray
Write-Host '    - Burrow-* firewall rules, and re-enable the built-in rules it disabled' -ForegroundColor Gray
Write-Host '    - the SMB shares and the burrowfs account' -ForegroundColor Gray
Write-Host '    - sshd configuration (restored from backup)' -ForegroundColor Gray
Write-Host '    - power plan and Fast Startup changes' -ForegroundColor Gray
Write-Host '    - RDP changes, if setup made them' -ForegroundColor Gray
if ($RemoveData) {
    Write-Host '    - C:\ProgramData\Burrow, INCLUDING the audit log and backups' -ForegroundColor Yellow
}

$ok = Confirm-BurrowChange -Yes:$Yes -What 'Uninstall Burrow from this PC' `
    -Why 'Reversible for the most part, but the audit log and paired passkeys are gone once removed.'
if (-not $ok) { Write-BurrowLog 'nothing was changed' -Level Info; return }

# ---------------------------------------------------------------- service ---

$svc = Get-Service -Name 'Burrow' -ErrorAction SilentlyContinue
if ($svc) {
    if ($PSCmdlet.ShouldProcess('Burrow service', 'stop and remove')) {
        $burrow = Join-Path $InstallDir 'burrow.exe'
        if (Test-Path $burrow) {
            & $burrow uninstall
        } else {
            Stop-Service -Name 'Burrow' -Force -ErrorAction SilentlyContinue
            & sc.exe delete Burrow | Out-Null
        }
        Write-BurrowLog 'service removed' -Level Ok
    }
}

# The "Open on PC" buttons are scheduled tasks under \Burrow\.
$burrowTasks = @(Get-ScheduledTask -TaskPath '\Burrow\' -ErrorAction SilentlyContinue)
if ($burrowTasks.Count -gt 0 -and $PSCmdlet.ShouldProcess('\Burrow\ scheduled tasks', 'remove')) {
    $burrowTasks | Unregister-ScheduledTask -Confirm:$false
    Write-BurrowLog "removed $($burrowTasks.Count) Open-on-PC task(s)" -Level Ok
}

if (Test-Path $InstallDir) {
    if ($PSCmdlet.ShouldProcess($InstallDir, 'remove')) {
        Remove-Item $InstallDir -Recurse -Force -ErrorAction SilentlyContinue
        Write-BurrowLog "removed $InstallDir" -Level Ok
    }
}

# --------------------------------------------------------------- firewall ---

$rules = @(Get-NetFirewallRule -DisplayName 'Burrow-*' -ErrorAction SilentlyContinue)
if ($rules.Count -gt 0 -and $PSCmdlet.ShouldProcess("$($rules.Count) Burrow-* rules", 'remove')) {
    $rules | Remove-NetFirewallRule
    Write-BurrowLog "removed $($rules.Count) Burrow firewall rule(s)" -Level Ok
}

# Re-enable what setup disabled, from the recorded changes.
if ($state.phases.PSObject.Properties['changes']) {
    foreach ($prop in $state.phases.changes.PSObject.Properties) {
        $change = $prop.Value
        if ($prop.Name -notmatch '^firewall-') { continue }
        if ($PSCmdlet.ShouldProcess($prop.Name, "revert: $($change.revert)")) {
            try {
                Invoke-Expression $change.revert
                Write-BurrowLog "reverted $($prop.Name)" -Level Ok
            } catch {
                Write-BurrowLog "could not revert $($prop.Name): $($_.Exception.Message)" -Level Warn
            }
        }
    }
}

# -------------------------------------------------------------------- SMB ---

$shareRoots = Get-BurrowStateValue -Phase '05-hardening' -Name 'shareRoots'
$shares = @(Get-SmbShare -ErrorAction SilentlyContinue |
        Where-Object { $_.Description -match '^Burrow' })
foreach ($s in $shares) {
    if ($PSCmdlet.ShouldProcess($s.Name, 'remove SMB share')) {
        Remove-SmbShare -Name $s.Name -Force
        Write-BurrowLog "removed share $($s.Name)" -Level Ok
    }
}

if ($shareRoots) {
    foreach ($path in $shareRoots) {
        if ((Test-Path $path) -and $PSCmdlet.ShouldProcess($path, 'remove the burrowfs NTFS grant')) {
            & icacls $path /remove 'burrowfs' /q 2>&1 | Out-Null
        }
    }
}

$fsUser = Get-LocalUser -Name 'burrowfs' -ErrorAction SilentlyContinue
if ($fsUser -and $PSCmdlet.ShouldProcess('burrowfs', 'remove the local account')) {
    Remove-LocalUser -Name 'burrowfs'
    $key = 'HKLM:\SOFTWARE\Microsoft\Windows NT\CurrentVersion\Winlogon\SpecialAccounts\UserList'
    Remove-ItemProperty -Path $key -Name 'burrowfs' -ErrorAction SilentlyContinue
    Write-BurrowLog 'removed the burrowfs account' -Level Ok
}

# ------------------------------------------------------------------- sshd ---

if ($state.phases.PSObject.Properties['backups']) {
    foreach ($prop in $state.phases.backups.PSObject.Properties) {
        $b = $prop.Value
        if (-not $b.existed) {
            # It did not exist before setup, so removing it is the revert.
            if ((Test-Path $b.original) -and $PSCmdlet.ShouldProcess($b.original, 'remove (created by setup)')) {
                Remove-Item $b.original -Force -ErrorAction SilentlyContinue
                Write-BurrowLog "removed $($b.original)" -Level Ok
            }
            continue
        }
        if ($b.backup -and (Test-Path $b.backup)) {
            if ($PSCmdlet.ShouldProcess($b.original, "restore from $($b.backup)")) {
                Copy-Item $b.backup $b.original -Force
                Write-BurrowLog "restored $($b.original)" -Level Ok
            }
        }
    }
}

$sshd = Get-Service -Name sshd -ErrorAction SilentlyContinue
if ($sshd -and $PSCmdlet.ShouldProcess('sshd', 'restart with the restored config')) {
    Restart-Service -Name sshd -ErrorAction SilentlyContinue
}

Write-BurrowLog 'OpenSSH Server itself was left installed; remove it in Settings > Optional features if you want it gone' -Level Info

# ------------------------------------------------- power, RDP and the rest ---

if ($state.phases.PSObject.Properties['changes']) {
    foreach ($prop in $state.phases.changes.PSObject.Properties) {
        $change = $prop.Value
        if ($prop.Name -match '^firewall-') { continue }  # done above
        if ($prop.Name -eq 'tailscale-acl') { continue }  # account-level, see below
        if (-not $change.revert) { continue }
        if ($PSCmdlet.ShouldProcess($prop.Name, "revert: $($change.revert)")) {
            try {
                Invoke-Expression $change.revert
                Write-BurrowLog "reverted $($prop.Name)" -Level Ok
            } catch {
                Write-BurrowLog "could not revert $($prop.Name): $($_.Exception.Message)" -Level Warn
                Write-Host "        do it by hand: $($change.revert)" -ForegroundColor Gray
            }
        }
    }
}

# --------------------------------------------------------------- the data ---

if ($RemoveData) {
    $ok2 = Confirm-BurrowChange -Yes:$Yes -What "Delete $dataDir" `
        -Why 'This removes the audit log, the paired passkeys, the tsnet node state and every backup setup made. It cannot be undone.'
    if ($ok2 -and $PSCmdlet.ShouldProcess($dataDir, 'delete')) {
        Remove-Item $dataDir -Recurse -Force -ErrorAction SilentlyContinue
        Write-BurrowLog "deleted $dataDir" -Level Ok
    }
} else {
    Write-Host ''
    Write-BurrowLog "$dataDir was left in place (audit log, backups, setup state)." -Level Info
    Write-Host '      Re-run with -RemoveData to delete it.' -ForegroundColor Gray
}

# --------------------------------------------------- what is left for you ---

Write-Host ''
Write-Host '  Account-level things this script will not touch:' -ForegroundColor White
Write-Host '    1. login.tailscale.com -> Machines: remove the PC, the agent node, and the' -ForegroundColor Gray
Write-Host '       iPhone/laptop if you want them off the tailnet.' -ForegroundColor Gray
Write-Host '    2. login.tailscale.com -> Access controls: your ACL policy still has the' -ForegroundColor Gray
Write-Host '       Burrow rules. A backup of the policy from before setup is in' -ForegroundColor Gray
Write-Host "       $dataDir\backup\." -ForegroundColor Gray
Write-Host '    3. Tailnet Lock, if you enabled it, stays on.' -ForegroundColor Gray
Write-Host '    4. The iPhone Shortcuts and the smart plug are yours to remove.' -ForegroundColor Gray
Write-Host ''
Write-BurrowLog 'uninstall finished' -Level Ok
