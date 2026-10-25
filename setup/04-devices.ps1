<#
.SYNOPSIS
  Phase 4 — bring the iPhone and laptop onto the tailnet, then apply the real
  ACL policy.

.DESCRIPTION
  Waits for each device to appear, asks you to confirm it is yours before
  approving it, records its tailnet IP and node id, then renders the final
  policy from policy/tailscale-acl.hujson with those two hosts filled in and
  the policy tests that assert nothing else can reach Burrow.

  Confirming each device matters: device approval is the control that stops a
  stolen Tailscale login adding a machine, and approving whatever turns up
  would throw that away (docs/03-SECURITY.md T3).

  Requires an elevated terminal and $env:TS_API_KEY.
#>
[CmdletBinding(SupportsShouldProcess)]
param(
    [switch]$Yes,
    [switch]$KeepAutogroupSelf,
    # Remote desktop from the laptop, decided in Phase 0.
    [ValidateSet('none', 'rdp', 'sunshine')][string]$RemoteDesktop = 'none',
    [int]$WaitMinutes = 15
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

Write-BurrowBanner -Phase '4' -Title 'iPhone and laptop join the tailnet'
Assert-BurrowElevated

$repo = Resolve-Path (Join-Path $PSScriptRoot '..')
$pcHostname = Get-BurrowStateValue -Phase '03-tailnet' -Name 'pcHostname'
if (-not $pcHostname) { throw 'Phase 3 has not run: no PC hostname recorded.' }

function Get-TailnetDevices {
    (Invoke-BurrowTailscaleApi -Path '/tailnet/-/devices').devices
}

function Test-Tagged($d) {
    # The API omits "tags" on untagged devices.
    $t = $d.PSObject.Properties['tags']
    return [bool]($t -and @($t.Value).Count -gt 0)
}

$before = @(Get-TailnetDevices)
# Never offer the PC itself or any tagged machine as the iPhone or laptop. The
# PC is matched by its own Tailscale addresses, not its hostname: a laptop can
# share the PC's Windows name (the tailnet then calls it "<name>-1").
# Devices that joined before this phase are still offered: the iPhone is often
# already signed in by the time setup runs.
$ts = Get-BurrowTailscaleCli
if (-not $ts) { throw 'tailscale.exe not found. Run Phase 1 first.' }
$pcAddresses = @(& $ts ip 2>$null | ForEach-Object { $_.Trim() } | Where-Object { $_ })
$beforeIds = @($before | Where-Object {
        (Test-Tagged $_) -or @($_.addresses | Where-Object { $_ -in $pcAddresses }).Count -gt 0
    } | ForEach-Object { $_.id })
Write-BurrowLog "$($before.Count) device(s) on the tailnet now" -Level Info

# ----------------------------------------------------------- join devices ---

Show-BurrowManualStep -Title 'Install Tailscale on your iPhone and laptop' -Steps @(
    'iPhone: App Store -> Tailscale -> sign in with the SAME account as this PC.',
    'Laptop: https://tailscale.com/download -> install -> sign in with the same account.',
    'Leave both connected. Do not turn on "Exit node" or "Shields up".',
    'This script is watching for them to appear.'
) -ThenPress 'Press Enter once both are signed in'

function Wait-ForNewDevice {
    param([string]$Label, [string[]]$KnownIds, [int]$TimeoutMinutes)

    $wantOS = if ($Label -eq 'iPhone') { @('iOS') } else { @('windows', 'macOS', 'linux') }
    Write-BurrowLog "looking for the $Label…" -Level Step
    $deadline = (Get-Date).AddMinutes($TimeoutMinutes)
    while ((Get-Date) -lt $deadline) {
        $now = @(Get-TailnetDevices)
        $new = @($now | Where-Object {
                $_.id -notin $KnownIds -and -not (Test-Tagged $_) -and $_.os -in $wantOS
            })
        if ($new.Count -gt 0) { return $new }
        Start-Sleep -Seconds 5
    }
    throw "the $Label did not appear within $TimeoutMinutes minutes"
}

$recorded = @{}

foreach ($label in @('iPhone', 'laptop')) {
    if ($WhatIfPreference) {
        Write-BurrowLog "would wait for the $label here (-WhatIf)" -Level Info
        continue
    }

    # @() because PowerShell unrolls a one-element array on return, and a
    # single device has no .Count under StrictMode.
    $new = @(Wait-ForNewDevice -Label $label -KnownIds $beforeIds -TimeoutMinutes $WaitMinutes)

    $device = $new[0]
    if ($new.Count -gt 1) {
        Write-Host ''
        Write-Host "  More than one new device appeared. Which is your $label?" -ForegroundColor Yellow
        for ($i = 0; $i -lt $new.Count; $i++) {
            Write-Host ("    [{0}] {1}  ({2})  {3}" -f $i, $new[$i].name, $new[$i].os, $new[$i].addresses[0])
        }
        $pick = Read-Host '      Number'
        $device = $new[[int]$pick]
    }

    Write-Host ''
    Write-Host ('  Device: {0}' -f $device.name) -ForegroundColor White
    Write-Host ('    OS       {0}' -f $device.os)
    Write-Host ('    Address  {0}' -f ($device.addresses -join ', '))
    Write-Host ('    Node ID  {0}' -f $device.nodeId)

    $ok = Confirm-BurrowChange -Yes:$Yes `
        -What "Approve this device as your $label and give it access to Burrow" `
        -Why 'Only approve a device you just signed in yourself. Approving an unexpected device would hand it your PC.'
    if (-not $ok) { throw "stopped: the $label was not approved" }

    if ($PSCmdlet.ShouldProcess($device.name, 'approve on the tailnet')) {
        try {
            $null = Invoke-BurrowTailscaleApi -Path "/device/$($device.id)/authorized" -Method POST `
                -Body @{ authorized = $true }
            Write-BurrowLog "$label approved" -Level Ok
        } catch {
            Write-BurrowLog "could not approve automatically ($($_.Exception.Message)); approve it in the admin console" -Level Warn
        }
    }

    $ipv4 = @($device.addresses | Where-Object { $_ -notmatch ':' })[0]
    $recorded[$label] = [pscustomobject]@{
        name   = $device.name
        id     = $device.id
        nodeId = $device.nodeId
        ip     = $ipv4
        os     = $device.os
        user   = $device.user
    }
    $beforeIds += $device.id
}

if ($WhatIfPreference) {
    Write-BurrowLog 'would render and apply the final policy here (-WhatIf)' -Level Info
    return
}

Set-BurrowStateValue -Phase '04-devices' -Name 'phone' -Value $recorded['iPhone']
Set-BurrowStateValue -Phase '04-devices' -Name 'laptop' -Value $recorded['laptop']
Set-BurrowStateValue -Phase '04-devices' -Name 'ownerLogin' -Value $recorded['iPhone'].user

# ----------------------------------------------------- the final policy ---

$phoneIP = $recorded['iPhone'].ip
$laptopIP = $recorded['laptop'].ip

Write-BurrowLog "rendering the policy for phone=$phoneIP laptop=$laptopIP" -Level Step

# Read as UTF-8 explicitly (5.1 would decode it as ANSI), then fold the
# non-ASCII characters to ASCII. They only appear in comments (dashes,
# arrows), and the API refuses the policy outright over a mis-encoded one.
$template = Get-Content (Join-Path $repo 'policy\tailscale-acl.hujson') -Raw -Encoding UTF8
$template = $template -replace '[^\x00-\x7F]', '-'
$policy = $template.
    Replace('<PHONE_TAILNET_IP>', $phoneIP).
    Replace('<LAPTOP_TAILNET_IP>', $laptopIP)

# Remote desktop: exactly one of these, or neither. Both lines are commented
# out in the template, so the chosen one is uncommented.
switch ($RemoteDesktop) {
    'rdp' {
        $policy = $policy -replace '(?m)^\s*//\s*<RDP_RULE>\s*', '    '
        # The template's tests already deny 3389 to the iPhone; the laptop's
        # accept list gains it so the policy proves RDP works from there only.
        $policy = $policy -replace '("src":\s*"owner-laptop",\s*"accept":\s*\[[^\]]*)\]', '$1, "tag:pc:3389"]'
    }
    'sunshine' {
        # Sunshine's ports change between releases; the runbook has you confirm
        # them against current docs before this runs.
        $ports = Get-BurrowStateValue -Phase '05-hardening' -Name 'sunshinePorts'
        if (-not $ports) { $ports = '47984,47989,48010' }
        $policy = $policy -replace '(?m)^\s*//\s*<SUNSHINE_RULE>\s*', '    '
        $policy = $policy.Replace('<SUNSHINE_PORTS>', $ports)
    }
}

if ($KeepAutogroupSelf) {
    $policy = $policy -replace '(?m)^\s*//\s*<AUTOGROUP_SELF_RULE>\s*', '    '
}

# Any placeholder left behind means a substitution did not happen, and the
# policy would be applied with a literal "<...>" in it.
# Comment lines are skipped: the template's own comments mention <PLACEHOLDERS>,
# and the optional rules that were not chosen stay commented out.
$live = ($policy -split "`n" | Where-Object { $_ -notmatch '^\s*//' }) -join "`n"
if ($live -match '<[A-Z_]+>') {
    throw "the rendered policy still contains a placeholder: $($Matches[0])"
}

$rendered = Join-Path 'C:\ProgramData\Burrow\backup' ("tailscale-acl-rendered-{0}.hujson" -f (Get-Date -Format 'yyyyMMdd-HHmmss'))
$null = New-Item -ItemType Directory -Force -Path (Split-Path $rendered)
Set-Content -Path $rendered -Value $policy -Encoding utf8
Write-BurrowLog "rendered policy saved to $rendered" -Level Info

Write-BurrowLog 'validating' -Level Step
$null = Invoke-BurrowTailscaleApi -Path '/tailnet/-/acl/validate' -Method POST `
    -Body $policy -ContentType 'application/hujson'
Write-BurrowLog 'policy validates, including its own tests' -Level Ok

if ($PSCmdlet.ShouldProcess('tailnet ACL', 'apply the final Burrow policy')) {
    $null = Invoke-BurrowTailscaleApi -Path '/tailnet/-/acl' -Method POST `
        -Body $policy -ContentType 'application/hujson'
    Write-BurrowLog 'final policy applied' -Level Ok
}

Write-Host ''
Write-Host '  Access is now limited to these two devices:' -ForegroundColor White
Write-Host ('    iPhone  {0,-18} {1}' -f $phoneIP, $recorded['iPhone'].name)
Write-Host ('    Laptop  {0,-18} {1}' -f $laptopIP, $recorded['laptop'].name)
Write-Host '  Every other device on your tailnet is denied by the policy tests.' -ForegroundColor Gray

Set-BurrowPhaseComplete -Phase '04-devices'
