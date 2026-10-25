<#
.SYNOPSIS
  Phase 0 — read the machine, find blockers, propose project and file roots.

.DESCRIPTION
  Read-only. Nothing here changes a setting, installs anything or logs into
  anything, so it is safe to run as often as you like and safe to run before
  you have decided to go ahead.

  It is deliberately Windows PowerShell 5.1 compatible, because it runs before
  Phase 1 installs PowerShell 7.

.EXAMPLE
  .\setup\00-preflight.ps1
  .\setup\00-preflight.ps1 -Json      # machine-readable, for a later phase
#>
[CmdletBinding()]
param(
    # Emit the findings as JSON instead of a report.
    [switch]$Json,
    # Skip the router probe, which makes one read-only HTTP GET to the gateway.
    [switch]$NoRouterProbe
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

# This phase only reads, so there is nothing for -WhatIf to preview. Leaving
# the preference on makes every module that autoloads here - CimCmdlets,
# NetTCPIP and the rest - print a "would Set-Alias" line for each alias it
# defines, burying the report under dozens of lines about nothing.
$WhatIfPreference = $false

Import-Module (Join-Path $PSScriptRoot 'lib\Burrow.Setup.psm1') -Force

# Seeded up front because Set-StrictMode makes reading an unset key an error,
# and several of these are only filled on some paths (no router probe, no
# Tailscale installed, no active adapter).
$findings = [ordered]@{
    routerTitle           = $null
    routerMac             = $null
    tailscaleVersion      = $null
    tailscaleBackendState = $null
    tailscaleSelfName     = $null
    tailscaleSelfIPs      = $null
    tsApiKeyValid         = $false
    tailnetDeviceCount    = 0
    tailnetDevices        = @()
    adapterName           = $null
    adapterDescription    = $null
    adapterMac            = $null
    adapterIsEthernet     = $false
    lanIPv4               = $null
    lanPrefix             = $null
    lanBroadcast          = $null
    lanCidr               = $null
    networkCategory       = 'unknown'
    motherboard           = 'unknown'
    biosVendor            = $null
    biosVersion           = $null
    passwordLastSet       = $null
}
$blockers = New-Object System.Collections.Generic.List[string]
$warnings = New-Object System.Collections.Generic.List[string]

function Add-Blocker([string]$m) { $blockers.Add($m) }
function Add-Warning([string]$m) { $warnings.Add($m) }

if (-not $Json) { Write-BurrowBanner -Phase '0' -Title 'Preflight (read-only)' }

# ------------------------------------------------------- shell and rights ---

$findings.elevated = Test-BurrowElevated
$findings.psVersion = $PSVersionTable.PSVersion.ToString()
$findings.psEdition = $PSVersionTable.PSEdition
$pwsh = Get-Command pwsh.exe -ErrorAction SilentlyContinue
$findings.pwsh7 = $null
if ($pwsh) {
    try { $findings.pwsh7 = (& $pwsh.Source -NoProfile -Command '$PSVersionTable.PSVersion.ToString()') } catch {}
}

if (-not $findings.elevated) {
    Add-Warning 'Not elevated. Phase 0 is fine, but Phases 1-12 need "Run as administrator".'
}

# ------------------------------------------------------------- Windows OS ---

$os = Get-CimInstance Win32_OperatingSystem
$findings.osCaption = $os.Caption
$findings.osVersion = $os.Version
$findings.osBuild = $os.BuildNumber
$findings.osArch = $os.OSArchitecture

# Pro/Enterprise gets built-in RDP; Home does not and needs Sunshine (5.4).
$edition = (Get-CimInstance Win32_OperatingSystem).Caption
$findings.isHomeEdition = $edition -match 'Home'
$findings.remoteDesktopMethod = if ($findings.isHomeEdition) { 'Sunshine + Moonlight' } else { 'RDP (built in)' }

# UEFI vs legacy decides whether Phase 10 can reboot straight into firmware.
try {
    $findings.firmware = if ($env:firmware_type) { $env:firmware_type } else {
        if ((Get-CimInstance Win32_DiskPartition -ErrorAction SilentlyContinue | Where-Object { $_.Type -match 'GPT' })) { 'UEFI' } else { 'Unknown' }
    }
} catch { $findings.firmware = 'Unknown' }

$findings.canRebootToFirmware = $findings.firmware -eq 'UEFI'

try {
    $bb = Get-CimInstance Win32_BaseBoard
    $bios = Get-CimInstance Win32_BIOS
    $findings.motherboard = "$($bb.Manufacturer) $($bb.Product)"
    $findings.biosVendor = $bios.Manufacturer
    $findings.biosVersion = ($bios.SMBIOSBIOSVersion)
} catch {
    $findings.motherboard = 'unknown'
}

# ---------------------------------------------------------------- network ---

$adapters = Get-NetAdapter -Physical -ErrorAction SilentlyContinue |
    Where-Object { $_.Status -eq 'Up' -and $_.InterfaceDescription -notmatch 'Tailscale|Hyper-V|Loopback' }

$eth = $adapters | Where-Object { $_.MediaType -eq '802.3' -or $_.PhysicalMediaType -eq '802.3' } | Select-Object -First 1
if (-not $eth) { $eth = $adapters | Select-Object -First 1 }

if ($eth) {
    $cfg = Get-NetIPAddress -InterfaceIndex $eth.ifIndex -AddressFamily IPv4 -ErrorAction SilentlyContinue |
        Where-Object { $_.IPAddress -notmatch '^169\.254\.' } | Select-Object -First 1
    $findings.adapterName = $eth.Name
    $findings.adapterDescription = $eth.InterfaceDescription
    $findings.adapterMac = $eth.MacAddress
    $findings.adapterIsEthernet = ($eth.MediaType -eq '802.3')
    if ($cfg) {
        $findings.lanIPv4 = $cfg.IPAddress
        $findings.lanPrefix = $cfg.PrefixLength
        # Recorded so a future Pi waker can send a directed broadcast.
        $ipBytes = ([System.Net.IPAddress]::Parse($cfg.IPAddress)).GetAddressBytes()
        $maskBits = [uint32]0
        for ($i = 0; $i -lt $cfg.PrefixLength; $i++) { $maskBits = $maskBits -bor ([uint32]1 -shl (31 - $i)) }
        $maskBytes = [BitConverter]::GetBytes([uint32]$maskBits)
        [Array]::Reverse($maskBytes)
        $bcast = @(0, 1, 2, 3) | ForEach-Object { $ipBytes[$_] -bor (255 -bxor $maskBytes[$_]) }
        $findings.lanBroadcast = ($bcast -join '.')
        $findings.lanCidr = "$($cfg.IPAddress)/$($cfg.PrefixLength)"
    }
    # $profile is an automatic variable in PowerShell; use another name.
    $netProfile = Get-NetConnectionProfile -InterfaceIndex $eth.ifIndex -ErrorAction SilentlyContinue
    $findings.networkCategory = 'unknown'
    if ($netProfile) { $findings.networkCategory = $netProfile.NetworkCategory.ToString() }
    if ($findings.networkCategory -eq 'Public') {
        Add-Warning 'The network profile is Public. That is fine for Burrow, but SMB discovery on the LAN will be off.'
    }
    if (-not $findings.adapterIsEthernet) {
        Add-Warning "The active adapter ($($eth.Name)) is not Ethernet. Wake-on-LAN for a future Pi waker needs a wired link; the smart plug works either way."
    }
} else {
    Add-Blocker 'No active network adapter found.'
}

$gw = Get-NetRoute -DestinationPrefix '0.0.0.0/0' -ErrorAction SilentlyContinue |
    Sort-Object RouteMetric | Select-Object -First 1
$findings.gateway = $null
if ($gw) { $findings.gateway = $gw.NextHop }

if ($findings.gateway -and -not $NoRouterProbe) {
    # Informational only, for a possible future upgrade. Read-only GET, short
    # timeout, never a login.
    try {
        $resp = Invoke-WebRequest -Uri "http://$($findings.gateway)/" -TimeoutSec 3 -UseBasicParsing -ErrorAction Stop
        if ($resp.Content -match '<title>(.*?)</title>') { $findings.routerTitle = $Matches[1].Trim() }
    } catch {
        $findings.routerTitle = $null
    }
    $arp = Get-NetNeighbor -IPAddress $findings.gateway -ErrorAction SilentlyContinue | Select-Object -First 1
    $findings.routerMac = $null
    if ($arp) { $findings.routerMac = $arp.LinkLayerAddress }
}

# ------------------------------------------------------------------ power ---

$findings.standbyTimeoutAcMinutes = $null
$findings.hibernateTimeoutAcMinutes = $null
try {
    $scheme = (powercfg /getactivescheme) -replace '.*GUID: ([0-9a-f\-]+).*', '$1'
    $standby = powercfg /query $scheme SUB_SLEEP STANDBYIDLE 2>$null
    $acLine = ($standby | Select-String 'Current AC Power Setting Index' | Select-Object -First 1)
    if ($acLine) {
        $hex = ($acLine -split ':')[-1].Trim()
        $findings.standbyTimeoutAcMinutes = [Convert]::ToInt32($hex, 16) / 60
    }
    $hib = powercfg /query $scheme SUB_SLEEP HIBERNATEIDLE 2>$null
    $acLineH = ($hib | Select-String 'Current AC Power Setting Index' | Select-Object -First 1)
    if ($acLineH) {
        $hexH = ($acLineH -split ':')[-1].Trim()
        $findings.hibernateTimeoutAcMinutes = [Convert]::ToInt32($hexH, 16) / 60
    }
} catch {
    Add-Warning 'Could not read the current power plan timeouts.'
}

if ($findings.standbyTimeoutAcMinutes -and $findings.standbyTimeoutAcMinutes -gt 0) {
    Add-Warning "AC sleep is set to $($findings.standbyTimeoutAcMinutes) minutes. Phase 5.3 sets it to Never: a sleeping PC cannot be woken by the smart plug."
}

try {
    $hiberboot = Get-ItemProperty 'HKLM:\SYSTEM\CurrentControlSet\Control\Session Manager\Power' -Name HiberbootEnabled -ErrorAction Stop
    $findings.fastStartupEnabled = ($hiberboot.HiberbootEnabled -eq 1)
} catch { $findings.fastStartupEnabled = $null }

# ------------------------------------------------------------------ tools ---

# The parameter is not called $args: that is an automatic variable, and
# binding it silently passes nothing, so every tool reports its usage text
# instead of its version.
function Get-ToolVersion([string]$exe, [string[]]$versionArgs) {
    $cmd = Get-Command $exe -ErrorAction SilentlyContinue
    if (-not $cmd) { return $null }
    try {
        $out = & $cmd.Source @versionArgs 2>&1 | Select-Object -First 1
        if ($null -eq $out) { return 'present' }
        "$out".Trim()
    } catch { 'present' }
}

$findings.go = Get-ToolVersion 'go' @('version')
$findings.node = Get-ToolVersion 'node' @('-v')
$findings.npm = Get-ToolVersion 'npm' @('-v')
$findings.git = Get-ToolVersion 'git' @('--version')
$findings.winget = Get-ToolVersion 'winget' @('--version')
$findings.claude = Get-ToolVersion 'claude' @('--version')

$ts = Get-BurrowTailscaleCli
$findings.tailscaleCli = $ts
if ($ts) {
    try { $findings.tailscaleVersion = (& $ts version 2>&1 | Select-Object -First 1).ToString().Trim() } catch {}
    try {
        $status = & $ts status --json 2>$null | ConvertFrom-Json
        $findings.tailscaleBackendState = $status.BackendState
        if ($status.Self) {
            $findings.tailscaleSelfName = $status.Self.HostName
            $findings.tailscaleSelfIPs = $status.Self.TailscaleIPs
        }
    } catch {}
}

if (-not $findings.winget) { Add-Blocker 'winget is not available. Phase 1 needs it to install the toolchain.' }
if (-not $findings.git) { Add-Warning 'Git for Windows is not installed. Claude Code needs Git Bash; Phase 1 installs it.' }

# Git Bash, which Claude Code needs on Windows.
$gitBash = @('C:\Program Files\Git\bin\bash.exe', 'C:\Program Files (x86)\Git\bin\bash.exe') |
    Where-Object { Test-Path $_ } | Select-Object -First 1
$findings.gitBash = $gitBash

# ------------------------------------------------------------ Claude Code ---

# Any of these would route Claude Code away from the claude.ai subscription,
# which is what Remote Control needs.
$claudeBlockerVars = @(
    'ANTHROPIC_API_KEY', 'ANTHROPIC_AUTH_TOKEN', 'CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC',
    'DISABLE_GROWTHBOOK', 'CLAUDE_CODE_USE_BEDROCK', 'CLAUDE_CODE_USE_VERTEX', 'CLAUDE_CODE_USE_FOUNDRY'
)
$found = @()
foreach ($v in $claudeBlockerVars) {
    foreach ($scope in @('Process', 'User', 'Machine')) {
        $val = [Environment]::GetEnvironmentVariable($v, $scope)
        if ($val) { $found += "$v ($scope)" }
    }
}
$baseUrl = [Environment]::GetEnvironmentVariable('ANTHROPIC_BASE_URL', 'Process')
if ($baseUrl -and $baseUrl -notmatch 'api\.anthropic\.com') { $found += "ANTHROPIC_BASE_URL=$baseUrl" }

$findings.claudeEnvBlockers = $found
if ($found.Count -gt 0) {
    Add-Blocker "Claude Code is routed away from the claude.ai subscription by: $($found -join ', '). Remote Control needs the subscription."
}

$claudeSettings = Join-Path $env:USERPROFILE '.claude\settings.json'
$findings.claudeSettingsPath = $null
if (Test-Path $claudeSettings) { $findings.claudeSettingsPath = $claudeSettings }
$findings.claudeSignedIn = (Test-Path (Join-Path $env:USERPROFILE '.claude\.credentials.json')) -or
                           (Test-Path (Join-Path $env:USERPROFILE '.claude.json'))
if (-not $findings.claude) {
    Add-Blocker 'Claude Code is not installed or not on PATH.'
} elseif (-not $findings.claudeSignedIn) {
    Add-Blocker 'Claude Code does not look signed in. The Burrow service runs in session 0 with no browser, so you must sign in once in a normal terminal first.'
}

# -------------------------------------------------------------- Tailscale ---

$findings.tsApiKeyPresent = [bool]$env:TS_API_KEY
if ($env:TS_API_KEY) {
    try {
        $devices = Invoke-BurrowTailscaleApi -Path '/tailnet/-/devices'
        $findings.tsApiKeyValid = $true
        $findings.tailnetDeviceCount = @($devices.devices).Count
        $findings.tailnetDevices = @($devices.devices | ForEach-Object {
                # The API omits "tags" on untagged devices; reading a missing
                # property throws under StrictMode.
                $t = $_.PSObject.Properties['tags']
                [pscustomobject]@{ name = $_.name; os = $_.os; tags = $(if ($t) { $t.Value } else { @() }) }
            })
    } catch {
        $findings.tsApiKeyValid = $false
        Add-Blocker "TS_API_KEY is set but the API rejected it: $($_.Exception.Message)"
    }
} else {
    Add-Blocker 'TS_API_KEY is not set in this process. See docs/06-SETUP-RUNBOOK.md "Before you start".'
}

# ---------------------------------------------------------------- account ---

$findings.userName = $env:USERNAME
$findings.userDomain = $env:USERDOMAIN
try {
    $localUser = Get-LocalUser -Name $env:USERNAME -ErrorAction Stop
    $findings.accountType = 'local'
    $findings.passwordLastSet = $localUser.PasswordLastSet
} catch {
    # A Microsoft account does not appear in Get-LocalUser under that name.
    $findings.accountType = 'microsoft-or-domain'
}
try {
    $admins = Get-LocalGroupMember -Group 'Administrators' -ErrorAction Stop
    $me = "$env:USERDOMAIN\$env:USERNAME"
    $findings.userIsAdmin = [bool]($admins | Where-Object { $_.Name -eq $me })
} catch { $findings.userIsAdmin = $null }

if ($findings.accountType -eq 'microsoft-or-domain') {
    Add-Warning 'Your account looks like a Microsoft account. The Burrow service logon in Phase 7 needs your Microsoft account password, not your PIN.'
}

# ------------------------------------------------------- existing exposure ---

$findings.listeningPorts = @(
    Get-NetTCPConnection -State Listen -ErrorAction SilentlyContinue |
        Where-Object { $_.LocalAddress -in @('0.0.0.0', '::') } |
        ForEach-Object {
            $p = Get-Process -Id $_.OwningProcess -ErrorAction SilentlyContinue
            $pname = if ($p) { $p.ProcessName } else { '?' }
            [pscustomobject]@{ port = $_.LocalPort; process = $pname }
        } | Sort-Object port -Unique
)

try {
    $rdp = Get-ItemProperty 'HKLM:\System\CurrentControlSet\Control\Terminal Server' -Name fDenyTSConnections -ErrorAction Stop
    $findings.rdpEnabled = ($rdp.fDenyTSConnections -eq 0)
} catch { $findings.rdpEnabled = $null }

$findings.smbSharingRulesEnabled = @(
    Get-NetFirewallRule -DisplayGroup 'File and Printer Sharing' -ErrorAction SilentlyContinue |
        Where-Object { $_.Enabled -eq 'True' -and $_.Direction -eq 'Inbound' } | Measure-Object
).Count
$findings.existingShares = @(
    Get-SmbShare -ErrorAction SilentlyContinue |
        Where-Object { $_.Name -notmatch '^\w\$$|^IPC\$$|^ADMIN\$$' } |
        ForEach-Object { [pscustomobject]@{ name = $_.Name; path = $_.Path } }
)

try {
    $bl = Get-BitLockerVolume -MountPoint $env:SystemDrive -ErrorAction Stop
    $findings.bitlocker = $bl.ProtectionStatus.ToString()
} catch { $findings.bitlocker = 'unknown' }
if ($findings.bitlocker -ne 'On') {
    Add-Warning 'BitLocker is not on for the system drive. Not required, but recommended: Burrow stores device keys and the audit log on this disk.'
}

# -------------------------------------------------- project / root proposal ---

$candidateRoots = @(
    (Join-Path $env:USERPROFILE 'source\repos'),
    (Join-Path $env:USERPROFILE 'code'),
    (Join-Path $env:USERPROFILE 'Projects'),
    (Join-Path $env:USERPROFILE 'dev'),
    'C:\dev', 'D:\code', 'D:\dev', 'C:\code'
) | Where-Object { Test-Path $_ }

$projects = @()
foreach ($parent in $candidateRoots) {
    Get-ChildItem $parent -Directory -ErrorAction SilentlyContinue | ForEach-Object {
        if (Test-Path (Join-Path $_.FullName '.git')) {
            $projects += [pscustomobject]@{ name = $_.Name; path = $_.FullName; parent = $parent }
        }
    }
}
$findings.projectsParentCandidates = @($candidateRoots)
$findings.projectCandidates = @($projects | Select-Object -First 40)

$findings.fileRootCandidates = @(
    @{ name = 'Desktop'; path = (Join-Path $env:USERPROFILE 'Desktop'); writable = $true },
    @{ name = 'Documents'; path = (Join-Path $env:USERPROFILE 'Documents'); writable = $true },
    @{ name = 'Downloads'; path = (Join-Path $env:USERPROFILE 'Downloads'); writable = $true },
    @{ name = 'Pictures'; path = (Join-Path $env:USERPROFILE 'Pictures'); writable = $false }
) | Where-Object { Test-Path $_.path } | ForEach-Object { [pscustomobject]$_ }

# ----------------------------------------------------------------- output ---

$findings.blockers = @($blockers)
$findings.warnings = @($warnings)
$findings.checkedAt = (Get-Date).ToUniversalTime().ToString('o')

if ($Json) {
    [pscustomobject]$findings | ConvertTo-Json -Depth 8
    if ($blockers.Count -gt 0) { exit 1 } else { exit 0 }
}

function Show-Row([string]$k, $v) {
    $text = '-'
    if ($v -is [bool]) { $text = if ($v) { 'yes' } else { 'no' } }
    elseif ($null -ne $v -and "$v" -ne '') { $text = "$v" }
    Write-Host ('  {0,-26} {1}' -f $k, $text)
}

Write-Host ''
Write-Host 'Machine' -ForegroundColor White
Show-Row 'Windows' "$($findings.osCaption) build $($findings.osBuild)"
Show-Row 'Edition path' $findings.remoteDesktopMethod
$fwNote = if ($findings.canRebootToFirmware) { '(shutdown /r /fw works)' } else { '(enter BIOS with Del/F2 at boot)' }
Show-Row 'Firmware' "$($findings.firmware)  $fwNote"
Show-Row 'Motherboard' $findings.motherboard
Show-Row 'BIOS' "$($findings.biosVendor) $($findings.biosVersion)"
Show-Row 'Elevated' $findings.elevated
Show-Row 'PowerShell' "$($findings.psVersion) ($($findings.psEdition))"
Show-Row 'PowerShell 7' $findings.pwsh7

Write-Host ''
Write-Host 'Network' -ForegroundColor White
Show-Row 'Adapter' "$($findings.adapterName) — $($findings.adapterDescription)"
Show-Row 'LAN address' $findings.lanCidr
Show-Row 'MAC' $findings.adapterMac
Show-Row 'Directed broadcast' $findings.lanBroadcast
Show-Row 'Profile' $findings.networkCategory
Show-Row 'Gateway' $findings.gateway
Show-Row 'Router' $findings.routerTitle

Write-Host ''
Write-Host 'Power' -ForegroundColor White
$sleepText = if ($null -ne $findings.standbyTimeoutAcMinutes) { "$($findings.standbyTimeoutAcMinutes) min" } else { 'unknown' }
$hibText = if ($null -ne $findings.hibernateTimeoutAcMinutes) { "$($findings.hibernateTimeoutAcMinutes) min" } else { 'unknown' }
Show-Row 'AC sleep' $sleepText
Show-Row 'AC hibernate' $hibText
Show-Row 'Fast Startup' $findings.fastStartupEnabled

Write-Host ''
Write-Host 'Tools' -ForegroundColor White
Show-Row 'Go' $findings.go
Show-Row 'Node' $findings.node
Show-Row 'Git' $findings.git
Show-Row 'Git Bash' $findings.gitBash
Show-Row 'winget' $findings.winget
Show-Row 'Claude Code' $findings.claude
Show-Row 'Claude signed in' $findings.claudeSignedIn
Show-Row 'Tailscale' $findings.tailscaleVersion
Show-Row 'Tailscale state' $findings.tailscaleBackendState

Write-Host ''
Write-Host 'Account and exposure' -ForegroundColor White
Show-Row 'Account' "$($findings.userDomain)\$($findings.userName) ($($findings.accountType))"
Show-Row 'Administrator' $findings.userIsAdmin
Show-Row 'BitLocker' $findings.bitlocker
Show-Row 'RDP enabled' $findings.rdpEnabled
Show-Row 'Existing shares' $findings.existingShares.Count
Show-Row 'Ports on 0.0.0.0' $findings.listeningPorts.Count
$keyText = 'not set'
if ($findings.tsApiKeyPresent) {
    $keyText = if ($findings.tsApiKeyValid) { 'set and valid' } else { 'set but rejected' }
}
Show-Row 'TS_API_KEY' $keyText
Show-Row 'Tailnet devices' $findings.tailnetDeviceCount

if ($findings.projectCandidates.Count -gt 0) {
    Write-Host ''
    Write-Host "Projects found ($($findings.projectCandidates.Count))" -ForegroundColor White
    $findings.projectCandidates | Select-Object -First 12 | ForEach-Object {
        Write-Host ('  {0,-26} {1}' -f $_.name, $_.path)
    }
}

if ($warnings.Count -gt 0) {
    Write-Host ''
    Write-Host 'Warnings' -ForegroundColor Yellow
    $warnings | ForEach-Object { Write-Host "  ! $_" -ForegroundColor Yellow }
}

Write-Host ''
if ($blockers.Count -eq 0) {
    Write-Host 'No blockers. Phase 1 can run.' -ForegroundColor Green
} else {
    Write-Host 'Blockers — fix these before Phase 1:' -ForegroundColor Red
    $blockers | ForEach-Object { Write-Host "  x $_" -ForegroundColor Red }
}

# The findings are saved so later phases can read them without re-probing.
if (Test-BurrowElevated) {
    Set-BurrowStateValue -Phase '00-preflight' -Name 'findings' -Value ([pscustomobject]$findings)
    if ($blockers.Count -eq 0) { Set-BurrowPhaseComplete -Phase '00-preflight' }
} else {
    Write-Host ''
    Write-Host '  (not elevated, so findings were not saved to C:\ProgramData\Burrow)' -ForegroundColor DarkGray
}

if ($blockers.Count -gt 0) { exit 1 } else { exit 0 }
