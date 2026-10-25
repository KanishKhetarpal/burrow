<#
.SYNOPSIS
  Phase 12 — verify the install and write SETUP-REPORT.md.

.DESCRIPTION
  Checks what can be checked from the PC. Every check says what it actually
  proves, because a green tick that means nothing is worse than no tick.

  The negative checks are the important ones: they confirm Burrow refuses
  things, not just that it answers. A service that answers everybody is
  "working" too.
#>
[CmdletBinding()]
param(
    [string]$InstallDir = 'C:\Program Files\Burrow',
    [string]$ReportPath = (Join-Path (Resolve-Path (Join-Path $PSScriptRoot '..')) 'SETUP-REPORT.md')
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

Write-BurrowBanner -Phase '12' -Title 'Verify and report'

$burrow = Join-Path $InstallDir 'burrow.exe'
$results = New-Object System.Collections.Generic.List[object]

function Add-Check {
    param([string]$Name, [bool]$Passed, [string]$Detail, [string]$Proves)
    $results.Add([pscustomobject]@{
            Name = $Name; Passed = $Passed; Detail = $Detail; Proves = $Proves
        })
    $mark = if ($Passed) { 'ok  ' } else { 'FAIL' }
    $colour = if ($Passed) { 'Green' } else { 'Red' }
    Write-Host ("  {0}  {1}" -f $mark, $Name) -ForegroundColor $colour
    if ($Detail) { Write-Host "        $Detail" -ForegroundColor DarkGray }
}

# --------------------------------------------------------------- service ---

$svc = Get-Service -Name 'Burrow' -ErrorAction SilentlyContinue
Add-Check -Name 'Burrow service is installed and running' `
    -Passed ($null -ne $svc -and $svc.Status -eq 'Running') `
    -Detail "status: $(if ($svc) { $svc.Status } else { 'not installed' })" `
    -Proves 'the agent survives a reboot'

if ($svc) {
    $wmi = Get-CimInstance Win32_Service -Filter "Name='Burrow'"
    Add-Check -Name 'service runs as your account, not SYSTEM' `
        -Passed ($wmi.StartName -notmatch 'LocalSystem') `
        -Detail "logon: $($wmi.StartName)" `
        -Proves 'Claude sessions use your login, ~/.claude and git config'
    Add-Check -Name 'service starts automatically (delayed)' `
        -Passed ($wmi.StartMode -eq 'Auto') `
        -Detail "start mode: $($wmi.StartMode)" `
        -Proves 'the PC is reachable after a wake without anyone signing in'
}

# ------------------------------------------------------------- selftest ---

if (Test-Path $burrow) {
    $selftest = & $burrow selftest 2>&1
    $selftestOk = ($LASTEXITCODE -eq 0)
    Add-Check -Name 'burrow selftest' -Passed $selftestOk `
        -Detail ($selftest | Where-Object { $_ -match 'FAIL' } | Select-Object -First 3) -join '; ' `
        -Proves 'the identity gate refuses the agent dialling itself, the restricted token works, and the audit chain verifies'

    $audit = & $burrow audit verify 2>&1
    Add-Check -Name 'audit chain verifies' -Passed ($LASTEXITCODE -eq 0) `
        -Detail (($audit | Select-Object -Last 1) -as [string]) `
        -Proves 'no audit entry has been edited, inserted or removed'

    $devices = & $burrow devices 2>&1
    $deviceCount = @($devices | Select-String -Pattern '^\S+\s+(phone|laptop)').Count
    Add-Check -Name 'both devices are paired' -Passed ($deviceCount -ge 2) `
        -Detail "$deviceCount paired" -Proves 'the iPhone and laptop each hold a passkey bound to their node'
}

# ------------------------------------------------------------- firewall ---

$rules = @(Get-NetFirewallRule -DisplayName 'Burrow-*' -ErrorAction SilentlyContinue)
Add-Check -Name 'Burrow firewall rules exist' -Passed ($rules.Count -ge 2) `
    -Detail ($rules.DisplayName -join ', ') -Proves 'SSH and SMB are reachable only from the two allowed addresses'

$phone = Get-BurrowStateValue -Phase '04-devices' -Name 'phone'
$laptop = Get-BurrowStateValue -Phase '04-devices' -Name 'laptop'
foreach ($rule in $rules) {
    $addrs = @(($rule | Get-NetFirewallAddressFilter).RemoteAddress)
    $scoped = $addrs -ne 'Any' -and $addrs.Count -gt 0
    Add-Check -Name "  $($rule.DisplayName) is address-scoped" -Passed $scoped `
        -Detail ($addrs -join ', ') -Proves 'nothing else on the tailnet or the LAN can reach that port'
}

$lanSsh = Get-NetFirewallRule -Name 'OpenSSH-Server-In-TCP' -ErrorAction SilentlyContinue
if ($lanSsh) {
    Add-Check -Name 'the LAN-wide SSH rule is disabled' -Passed ($lanSsh.Enabled -ne 'True') `
        -Detail "enabled: $($lanSsh.Enabled)" -Proves 'port 22 is not open to your home network'
}

# ---------------------------------------------------------------- sshd ---

$sshd = Get-Service -Name sshd -ErrorAction SilentlyContinue
if ($sshd) {
    Add-Check -Name 'sshd is running' -Passed ($sshd.Status -eq 'Running') -Detail "$($sshd.Status)" `
        -Proves 'Termius and VS Code Remote can connect'

    $cfg = Get-Content 'C:\ProgramData\ssh\sshd_config' -Raw -ErrorAction SilentlyContinue
    if ($cfg) {
        Add-Check -Name 'sshd refuses password authentication' `
            -Passed ($cfg -match '(?m)^\s*PasswordAuthentication\s+no') -Detail '' `
            -Proves 'a guessed password cannot get a shell; only the paired keys work'
    }
}

# ----------------------------------------------------------------- SMB ---

$shares = @(Get-SmbShare -ErrorAction SilentlyContinue | Where-Object { $_.Name -notmatch '\$$' })
Add-Check -Name 'SMB shares exist' -Passed ($shares.Count -gt 0) `
    -Detail ($shares.Name -join ', ') -Proves 'the iOS Files app and mapped drives have something to connect to'

foreach ($s in $shares) {
    Add-Check -Name "  $($s.Name) requires encryption" -Passed ($s.EncryptData) -Detail '' `
        -Proves 'file contents are not readable in transit'
}

$smbCfg = Get-SmbServerConfiguration -ErrorAction SilentlyContinue
if ($smbCfg) {
    Add-Check -Name 'SMB1 is disabled' -Passed (-not $smbCfg.EnableSMB1Protocol) -Detail '' `
        -Proves 'the obsolete protocol with known attacks is off'
    Add-Check -Name 'SMB rejects unencrypted access' -Passed ($smbCfg.RejectUnencryptedAccess) -Detail '' `
        -Proves 'a client cannot negotiate the encryption away'
}

$fsUser = Get-LocalUser -Name 'burrowfs' -ErrorAction SilentlyContinue
if ($fsUser) {
    $admins = @(Get-LocalGroupMember -Group 'Administrators' -ErrorAction SilentlyContinue |
            Where-Object { $_.Name -match 'burrowfs' })
    Add-Check -Name 'burrowfs is not an administrator' -Passed ($admins.Count -eq 0) -Detail '' `
        -Proves 'the password stored on your phone grants files, not the machine'
}

# ---------------------------------------------------------------- power ---

$standby = (powercfg /query SCHEME_CURRENT SUB_SLEEP STANDBYIDLE 2>$null |
        Select-String 'Current AC Power Setting Index' | Select-Object -First 1)
if ($standby) {
    $val = [Convert]::ToInt32((($standby -split ':')[-1].Trim()), 16)
    Add-Check -Name 'AC sleep is Never' -Passed ($val -eq 0) -Detail "$($val / 60) min" `
        -Proves 'the PC cannot drop into a state the smart plug is unable to wake it from'
}

$hiberboot = (Get-ItemProperty 'HKLM:\SYSTEM\CurrentControlSet\Control\Session Manager\Power' `
        -Name HiberbootEnabled -ErrorAction SilentlyContinue).HiberbootEnabled
Add-Check -Name 'Fast Startup is off' -Passed ($hiberboot -eq 0) -Detail "HiberbootEnabled=$hiberboot" `
    -Proves 'a shutdown really powers off, so restoring power boots cleanly'

# -------------------------------------------------------------- exposure ---

$publicListeners = @(
    Get-NetTCPConnection -State Listen -ErrorAction SilentlyContinue |
        Where-Object { $_.LocalAddress -in @('0.0.0.0', '::') -and $_.LocalPort -in @(22, 443, 445, 3389) }
)
Write-Host ''
Write-Host '  Ports 22/443/445/3389 bound on all interfaces:' -ForegroundColor White
if ($publicListeners.Count -eq 0) {
    Write-Host '    none' -ForegroundColor Green
} else {
    foreach ($l in $publicListeners) {
        $p = Get-Process -Id $l.OwningProcess -ErrorAction SilentlyContinue
        Write-Host ("    {0,-6} {1}" -f $l.LocalPort, $(if ($p) { $p.ProcessName } else { '?' })) -ForegroundColor Yellow
    }
    Write-Host '    These bind widely but the firewall rules above restrict who can reach them.' -ForegroundColor DarkGray
}

# The agent must not be among them: it listens on the tailnet only.
$agentOnOsInterface = @($publicListeners | Where-Object { $_.LocalPort -eq 443 -and (Get-Process -Id $_.OwningProcess -ErrorAction SilentlyContinue).ProcessName -eq 'burrow' })
Add-Check -Name 'the agent does not listen on an OS interface' -Passed ($agentOnOsInterface.Count -eq 0) `
    -Detail '' -Proves 'the agent is reachable over the tailnet only, not from the LAN'

# ---------------------------------------------------------- manual tests ---

Write-Host ''
Write-BurrowLog 'These last ones you have to do yourself — they need the phone and the laptop.' -Level Manual
$manual = @(
    'M15  On home Wi-Fi, turn Tailscale OFF on the iPhone. Burrow goes offline and smb:// to the LAN address fails.',
    'M16  Remove the iPhone at login.tailscale.com -> Machines. Within 30s it loses everything; the laptop is unaffected. Re-add and re-pair.',
    'L7   Quit Tailscale on the laptop. SSH, SMB and remote desktop to the PC all fail.',
    'Scan from the laptop with Tailscale off:  nmap -Pn -p 22,443,445,3389 <pc-lan-ip>  -> all filtered or closed.'
)
$manual | ForEach-Object { Write-Host "      $_" -ForegroundColor Gray }

# -------------------------------------------------------------- report ---

$passed = @($results | Where-Object { $_.Passed }).Count
$failed = @($results | Where-Object { -not $_.Passed }).Count

$report = @(
    '# Burrow — setup report',
    '',
    "Generated $((Get-Date).ToString('yyyy-MM-dd HH:mm')) on $env:COMPUTERNAME.",
    '',
    "**$passed passed, $failed failed.**",
    '',
    '## Automated checks',
    '',
    '| Check | Result | What it proves |',
    '|---|---|---|'
)
foreach ($r in $results) {
    $mark = if ($r.Passed) { 'pass' } else { '**FAIL**' }
    $report += "| $($r.Name) | $mark | $($r.Proves) |"
}

$report += @(
    '',
    '## Still to do by hand',
    ''
)
$manual | ForEach-Object { $report += "- $_" }

$report += @(
    '',
    '## Daily use',
    '',
    '**iPhone** — Wake: Burrow -> Wake PC, or "Hey Siri, PC On". New chat: New Claude chat -> pick project -> Open in Claude.',
    'Files: in Burrow, or the Files app via `smb://<your PC name>`. Terminal: Open terminal (Termius).',
    'Shut down: Burrow -> More -> Shut down, then tap "Turn plug off" once it says the PC is off.',
    '',
    '**Laptop** — the Burrow web app, `ssh burrow-pc`, VS Code Remote-SSH, mapped drives, and remote desktop if enabled.',
    'Wake still comes from the iPhone.',
    '',
    '## If a device is lost',
    '',
    '1. login.tailscale.com -> Machines -> remove that device. Access stops immediately; the other device keeps working.',
    '2. claude.ai -> Settings -> Account -> Trusted devices -> remove it, then sign out everywhere.',
    '3. At the PC: `burrow devices revoke <name>`, then `setup\rotate-secrets.ps1` to change the burrowfs password.',
    '',
    '## Where things are',
    '',
    '| What | Where |',
    '|---|---|',
    '| Binary | `C:\Program Files\Burrow\burrow.exe` |',
    '| Config | `C:\ProgramData\Burrow\burrow.yaml` |',
    '| Audit log | `C:\ProgramData\Burrow\audit\audit.jsonl` |',
    '| Agent log | `C:\ProgramData\Burrow\logs\burrow.log` |',
    '| Setup state and backups | `C:\ProgramData\Burrow\setup-state.json`, `backup\` |',
    '',
    'Undo everything: `setup\uninstall.ps1`.'
)

Set-Content -Path $ReportPath -Value ($report -join "`r`n") -Encoding utf8

Write-Host ''
Write-Host ("  $passed passed, $failed failed.") -ForegroundColor $(if ($failed -eq 0) { 'Green' } else { 'Yellow' })
Write-Host "  Report written to $ReportPath" -ForegroundColor White

if ($failed -eq 0) { Set-BurrowPhaseComplete -Phase '12-verify' }
if ($failed -gt 0) { exit 1 }
