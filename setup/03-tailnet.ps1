<#
.SYNOPSIS
  Phase 3 — configure the tailnet and join this PC to it.

.DESCRIPTION
  MagicDNS and HTTPS certificates on, a bootstrap ACL policy that defines the
  tags but grants no Burrow access yet, device approval and key expiry, then
  this PC joins as tag:pc with a random hostname.

  The bootstrap policy deliberately grants nothing: the real rules need the
  iPhone's and laptop's tailnet IPs, which do not exist until Phase 4. A policy
  that allowed access to devices that had not joined yet would be a window
  where an unexpected device could reach the PC.

  ⚠️ If a custom ACL policy already exists, this shows the diff and stops for
  confirmation. Replacing someone's policy without asking could cut off
  machines they rely on.

  Requires an elevated terminal and $env:TS_API_KEY.
#>
[CmdletBinding(SupportsShouldProcess)]
param(
    # Skip the confirmation when replacing an existing policy.
    [switch]$Yes,
    # Keep the owner's other personal devices able to reach each other
    # (Phase 0, Q5). Burrow's tagged nodes stay iPhone+laptop-only regardless.
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

Write-BurrowBanner -Phase '3' -Title 'Tailnet configuration'
Assert-BurrowElevated

if (-not $env:TS_API_KEY) {
    throw 'TS_API_KEY is not set in this process. See docs/06-SETUP-RUNBOOK.md "Before you start".'
}

$ts = Get-BurrowTailscaleCli
if (-not $ts) { throw 'tailscale.exe not found. Run Phase 1 first.' }

# ------------------------------------------------------- 1. DNS and certs ---

Write-BurrowLog 'reading tailnet DNS settings' -Level Step
$dns = Invoke-BurrowTailscaleApi -Path '/tailnet/-/dns/preferences'
Write-BurrowLog "MagicDNS: $($dns.magicDNS)" -Level Info

if (-not $dns.magicDNS) {
    if ($PSCmdlet.ShouldProcess('tailnet DNS', 'enable MagicDNS')) {
        $null = Invoke-BurrowTailscaleApi -Path '/tailnet/-/dns/preferences' -Method POST `
            -Body @{ magicDNS = $true }
        Write-BurrowLog 'MagicDNS enabled' -Level Ok
    }
}

$tailnetName = $null
try {
    # The tailnet's DNS name is what the agent's FQDN hangs off.
    $status = & $ts status --json 2>$null | ConvertFrom-Json
    if ($status.MagicDNSSuffix) { $tailnetName = $status.MagicDNSSuffix }
} catch {}
if ($tailnetName) { Write-BurrowLog "tailnet: $tailnetName" -Level Info }

# HTTPS certificates have no stable API across versions, so this is checked
# and handed over rather than guessed at.
Show-BurrowManualStep -Title 'Enable HTTPS certificates (one click, if it is not already on)' -Steps @(
    'Open https://login.tailscale.com/admin/dns',
    'Under "HTTPS Certificates", click Enable if it is not already enabled.',
    'Burrow needs this: the agent serves TLS with a Tailscale-issued certificate.'
)

# --------------------------------------------------- 2. bootstrap policy ---

Write-BurrowLog 'reading the current ACL policy' -Level Step
$currentPolicy = $null
try {
    $currentPolicy = Invoke-BurrowTailscaleApi -Path '/tailnet/-/acl'
} catch {
    Write-BurrowLog 'could not read the current policy; assuming the default' -Level Warn
}

# Tags only, no access rules. The real rules land in Phase 4.
$bootstrapLines = @(
    '{',
    '  "tagOwners": {',
    '    "tag:pc":           ["autogroup:admin"],',
    '    "tag:burrow-agent": ["autogroup:admin"],',
    '    "tag:burrow-waker": ["autogroup:admin"]',
    '  },',
    '  "acls": ['
)
if ($KeepAutogroupSelf) {
    $bootstrapLines += '    {"action": "accept", "src": ["autogroup:member"], "dst": ["autogroup:self:*"]}'
}
$bootstrapLines += @(
    '  ],',
    '  "ssh": []',
    '}'
)
$bootstrap = $bootstrapLines -join "`n"

$looksCustom = $false
if ($currentPolicy) {
    $asText = $currentPolicy | ConvertTo-Json -Depth 20
    # The stock policy is a single allow-all rule; anything else is someone's
    # own configuration and must not be replaced silently.
    $looksCustom = ($asText -notmatch '"autogroup:members".*"\*:\*"') -and
                   ($asText -match '"acls"') -and
                   ($asText.Length -gt 400)
}

if ($looksCustom) {
    Write-Host ''
    Write-Host '  Current policy (first 40 lines):' -ForegroundColor Yellow
    ($currentPolicy | ConvertTo-Json -Depth 20) -split "`n" | Select-Object -First 40 |
        ForEach-Object { Write-Host "    $_" -ForegroundColor DarkGray }

    $ok = Confirm-BurrowChange -Yes:$Yes `
        -What 'Replace the existing Tailscale ACL policy with the Burrow bootstrap policy' `
        -Why 'Your tailnet already has a custom policy. Replacing it may cut off machines that rely on it. A copy is saved to C:\ProgramData\Burrow\backup first.'
    if (-not $ok) { throw 'stopped: the ACL policy was not replaced' }
}

if ($PSCmdlet.ShouldProcess('tailnet ACL', 'back up and apply the bootstrap policy')) {
    $backupPath = Join-Path 'C:\ProgramData\Burrow\backup' ("tailscale-acl-{0}.json" -f (Get-Date -Format 'yyyyMMdd-HHmmss'))
    $null = New-Item -ItemType Directory -Force -Path (Split-Path $backupPath)
    if ($currentPolicy) {
        $currentPolicy | ConvertTo-Json -Depth 20 | Set-Content $backupPath -Encoding utf8
        Write-BurrowLog "previous policy saved to $backupPath" -Level Ok
        Register-BurrowChange -Tag 'tailscale-acl' -Was $backupPath -Now 'burrow bootstrap' `
            -Revert "POST the contents of $backupPath to /api/v2/tailnet/-/acl" -WhatIf:$WhatIfPreference
    }

    # Validated before applying: an invalid policy applied to a live tailnet
    # can lock everything out, and validate costs nothing.
    Write-BurrowLog 'validating the bootstrap policy' -Level Step
    $null = Invoke-BurrowTailscaleApi -Path '/tailnet/-/acl/validate' -Method POST `
        -Body $bootstrap -ContentType 'application/hujson'
    Write-BurrowLog 'policy validates' -Level Ok

    $null = Invoke-BurrowTailscaleApi -Path '/tailnet/-/acl' -Method POST `
        -Body $bootstrap -ContentType 'application/hujson'
    Write-BurrowLog 'bootstrap policy applied (tags defined, no Burrow access yet)' -Level Ok
}

# --------------------------------------------------- 3. tailnet settings ---

Write-BurrowLog 'device approval and key expiry' -Level Step
Show-BurrowManualStep -Title 'Two tailnet settings that have no stable API' -Steps @(
    'Open https://login.tailscale.com/admin/settings/general',
    'Turn ON "Device approval" — a new device then cannot reach anything until you approve it.',
    'Open https://login.tailscale.com/admin/settings/keys and set device key expiry to 180 days.',
    'Tagged nodes (the PC and the agent) do not expire; your iPhone and laptop re-auth every 180 days.'
)

# ------------------------------------------------------------ 4. this PC ---

$pcHostname = Get-BurrowStateValue -Phase '03-tailnet' -Name 'pcHostname'
if (-not $pcHostname) { $pcHostname = New-BurrowHostname }

$status = $null
try { $status = & $ts status --json 2>$null | ConvertFrom-Json } catch {}
$alreadyUp = $status -and $status.BackendState -eq 'Running'

if ($alreadyUp) {
    Write-BurrowLog "this PC is already on the tailnet as $($status.Self.HostName)" -Level Info
    if ($PSCmdlet.ShouldProcess($pcHostname, 'rename this node')) {
        # --unattended matters as much as the name: without it Tailscale on
        # Windows only runs while someone is logged in with the tray app open,
        # so after a reboot (the whole point of Burrow) SSH and SMB are gone.
        & $ts set --hostname=$pcHostname --unattended
        Write-BurrowLog "hostname set to $pcHostname, unattended mode on" -Level Ok
    }
    Show-BurrowManualStep -Title 'Tag this PC' -Steps @(
        'Open https://login.tailscale.com/admin/machines',
        "Find $pcHostname, open its menu, choose Edit ACL tags, and add tag:pc.",
        'In the same menu, choose "Disable key expiry" so the PC does not drop off the tailnet in 180 days.'
    )
} else {
    Write-BurrowLog 'joining this PC to the tailnet' -Level Step
    if ($PSCmdlet.ShouldProcess($pcHostname, 'create an auth key and run tailscale up')) {
        # Single-use, pre-authorised, one hour: the shortest-lived key that can
        # still do the job, and it never touches disk except as a temp file
        # that is deleted immediately.
        $key = Invoke-BurrowTailscaleApi -Path '/tailnet/-/keys' -Method POST -Body @{
            capabilities = @{
                devices = @{
                    create = @{
                        reusable      = $false
                        ephemeral     = $false
                        preauthorized = $true
                        tags          = @('tag:pc')
                    }
                }
            }
            expirySeconds = 3600
            description   = 'burrow setup PC node single-use'
        }

        $keyFile = Join-Path $env:TEMP ("burrow-authkey-{0}" -f ([guid]::NewGuid().ToString('N')))
        try {
            Set-Content -Path $keyFile -Value $key.key -Encoding ascii -NoNewline
            & icacls $keyFile /inheritance:r /grant:r "$($env:USERNAME):(R)" | Out-Null

            # --auth-key=file: so the key never appears in the process list.
            & $ts up --unattended --hostname=$pcHostname "--auth-key=file:$keyFile" `
                --accept-routes=false --shields-up=false
            if ($LASTEXITCODE -ne 0) { throw 'tailscale up failed' }
            Write-BurrowLog "joined as $pcHostname" -Level Ok
        } finally {
            Remove-Item $keyFile -Force -ErrorAction SilentlyContinue
        }
    }
}

# Record what Phase 4 and Phase 7 need.
if (-not $WhatIfPreference) {
    Start-Sleep -Seconds 2
    $status = & $ts status --json 2>$null | ConvertFrom-Json
    $pcIP = @($status.Self.TailscaleIPs | Where-Object { $_ -notmatch ':' })[0]
    $pcDNS = $status.Self.DNSName -replace '\.$', ''

    Set-BurrowStateValue -Phase '03-tailnet' -Name 'pcHostname' -Value $pcHostname
    Set-BurrowStateValue -Phase '03-tailnet' -Name 'pcTailnetIP' -Value $pcIP
    Set-BurrowStateValue -Phase '03-tailnet' -Name 'pcMagicDNS' -Value $pcDNS
    Set-BurrowStateValue -Phase '03-tailnet' -Name 'tailnetName' -Value $tailnetName

    Write-Host ''
    Write-Host '  This PC on the tailnet' -ForegroundColor White
    Write-Host ('    {0,-14} {1}' -f 'hostname', $pcHostname)
    Write-Host ('    {0,-14} {1}' -f 'tailnet IP', $pcIP)
    Write-Host ('    {0,-14} {1}' -f 'MagicDNS', $pcDNS)

    Set-BurrowPhaseComplete -Phase '03-tailnet'
}
