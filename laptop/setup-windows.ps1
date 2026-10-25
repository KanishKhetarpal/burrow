<#
.SYNOPSIS
  Set up the laptop side of Burrow, on Windows.

.DESCRIPTION
  Run this ON YOUR LAPTOP, not on the PC. It sets up the SSH config entry, maps
  the shares and points you at the web app.

  Nothing here needs administrator rights except mapping drives for all users,
  which it does not do — the mappings are for your account only.

.EXAMPLE
  .\laptop\setup-windows.ps1 -PcName mypc.tail1a2b.ts.net -PcUser pcuser
#>
[CmdletBinding(SupportsShouldProcess)]
param(
    # The PC's MagicDNS name, from `burrow status` on the PC.
    [Parameter(Mandatory)][string]$PcName,
    # Your Windows username on the PC.
    [Parameter(Mandatory)][string]$PcUser,
    # Shares to map, and the drive letters to use.
    [hashtable]$Drives = @{ 'B' = 'Documents'; 'V' = 'Downloads' },
    [string]$KeyPath = (Join-Path $env:USERPROFILE '.ssh\burrow_ed25519'),
    [switch]$SkipDrives
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

function Say($m, $c = 'Gray') { Write-Host "  $m" -ForegroundColor $c }
function Step($m) { Write-Host ''; Write-Host "==> $m" -ForegroundColor Cyan }

Write-Host ''
Write-Host '  Burrow — laptop setup (Windows)' -ForegroundColor White
Write-Host '  ───────────────────────────────' -ForegroundColor DarkGray

# -------------------------------------------------------------- Tailscale ---

Step 'Tailscale'
$ts = @(
    (Get-Command tailscale.exe -ErrorAction SilentlyContinue).Source,
    'C:\Program Files\Tailscale\tailscale.exe'
) | Where-Object { $_ -and (Test-Path $_) } | Select-Object -First 1

if (-not $ts) {
    Say 'Tailscale is not installed.' 'Yellow'
    Say 'Install it from https://tailscale.com/download and sign in with the same account as the PC, then re-run.' 'Yellow'
    return
}
Say "found $ts" 'Green'

# Reachability first: everything below fails confusingly if the tailnet is down.
Step "Checking the PC is reachable at $PcName"
if (-not (Test-Connection -TargetName $PcName -Count 1 -Quiet -ErrorAction SilentlyContinue)) {
    Say "cannot reach $PcName." 'Yellow'
    Say 'Check Tailscale is connected here and the PC is on. Continuing anyway — the config below is still worth writing.' 'Yellow'
} else {
    Say 'reachable' 'Green'
}

# -------------------------------------------------------------- SSH key ---

Step 'SSH key'
if (Test-Path $KeyPath) {
    Say "already have $KeyPath" 'Green'
} else {
    Say 'Generating an ed25519 key. Give it a passphrase — it is the thing that'
    Say 'protects shell access to your PC if this laptop is taken.'
    if ($PSCmdlet.ShouldProcess($KeyPath, 'generate an SSH key')) {
        $null = New-Item -ItemType Directory -Force -Path (Split-Path $KeyPath)
        & ssh-keygen -t ed25519 -f $KeyPath -C "burrow-laptop-$env:COMPUTERNAME"
    }
}

if (Test-Path "$KeyPath.pub") {
    Write-Host ''
    Say 'Your public key — paste this into Burrow (More -> Security -> Add SSH key):' 'White'
    Write-Host ''
    Write-Host "    $(Get-Content "$KeyPath.pub" -Raw)" -ForegroundColor Green
    if ($PSCmdlet.ShouldProcess('clipboard', 'copy the public key')) {
        Set-Clipboard -Value (Get-Content "$KeyPath.pub" -Raw).Trim()
        Say 'copied to the clipboard' 'Green'
    }
}

# ------------------------------------------------------------ ssh config ---

Step 'SSH config entry'
$sshConfig = Join-Path $env:USERPROFILE '.ssh\config'
$entry = @(
    '',
    '# Added by Burrow laptop setup',
    'Host burrow-pc',
    "    HostName $PcName",
    "    User $PcUser",
    "    IdentityFile $KeyPath",
    '    IdentitiesOnly yes',
    # The PC refuses password auth, so failing fast on a missing key is
    # clearer than a prompt that can never succeed.
    '    PreferredAuthentications publickey',
    '    ServerAliveInterval 60'
) -join "`r`n"

$existing = if (Test-Path $sshConfig) { Get-Content $sshConfig -Raw } else { '' }
if ($existing -match '(?m)^Host\s+burrow-pc\s*$') {
    Say 'a burrow-pc entry already exists; leaving it alone' 'Green'
} elseif ($PSCmdlet.ShouldProcess($sshConfig, 'add the burrow-pc host entry')) {
    $null = New-Item -ItemType Directory -Force -Path (Split-Path $sshConfig)
    Add-Content -Path $sshConfig -Value $entry -Encoding utf8
    Say "added 'burrow-pc' to $sshConfig" 'Green'
}

Write-Host ''
Say 'You can now use:' 'White'
Say '    ssh burrow-pc'
Say '    VS Code -> Remote-SSH: Connect to Host -> burrow-pc'

# ------------------------------------------------------------- drives ---

if (-not $SkipDrives) {
    Step 'Mapped drives'
    Say 'These ask for the burrowfs password — the one shown once during PC setup' 'Gray'
    Say 'and saved as "Burrow Files".' 'Gray'

    foreach ($letter in $Drives.Keys) {
        $share = $Drives[$letter]
        $unc = "\\$PcName\$share"

        if (Get-PSDrive -Name $letter -ErrorAction SilentlyContinue) {
            Say "${letter}: is already in use; skipping $share" 'Yellow'
            continue
        }
        if ($PSCmdlet.ShouldProcess($unc, "map to ${letter}:")) {
            Say "mapping ${letter}: -> $unc"
            # /persistent:yes so it survives a reboot; the credential goes into
            # Windows Credential Manager when you tick "remember".
            & net use "${letter}:" $unc /user:burrowfs /persistent:yes
            if ($LASTEXITCODE -ne 0) {
                Say "could not map $unc — check the PC is on and the password is right" 'Yellow'
            }
        }
    }
}

# ------------------------------------------------------------ the app ---

Step 'The Burrow web app'
$agentFqdn = Read-Host '  Agent address from `burrow status` on the PC (blank to skip)'
if ($agentFqdn) {
    $url = "https://$($agentFqdn -replace '^https://', '')"
    Say "opening $url" 'Gray'
    if ($PSCmdlet.ShouldProcess($url, 'open in the default browser')) {
        Start-Process $url
    }
    Write-Host ''
    Say 'In the browser: unlock with Windows Hello, then install it' 'White'
    Say '(Chrome/Edge: the install icon in the address bar).'
}

Write-Host ''
Write-Host '  Done.' -ForegroundColor Green
Write-Host '  Wake still comes from your iPhone — the laptop cannot run the Shortcut.' -ForegroundColor Gray
