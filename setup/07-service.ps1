<#
.SYNOPSIS
  Phase 7 — write the config, enroll the agent on the tailnet, install and
  start the Windows service.

.DESCRIPTION
  The service logs on as your own account rather than SYSTEM, so Claude Code
  uses your login, your ~/.claude and your git config. That is why it needs a
  password, and why that password is typed into a console prompt by you — it is
  never read by this script, never logged, and never placed on a command line
  where another local process could read it out of the process list
  (docs/03-SECURITY.md §4).

  Requires an elevated terminal.
#>
[CmdletBinding(SupportsShouldProcess)]
param(
    [string]$InstallDir = 'C:\Program Files\Burrow',
    # Regenerate burrow.yaml even if one exists.
    [switch]$RewriteConfig
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

Write-BurrowBanner -Phase '7' -Title 'Burrow agent service'
Assert-BurrowElevated

$burrow = Join-Path $InstallDir 'burrow.exe'
if (-not (Test-Path $burrow)) { throw "burrow.exe is not installed at $burrow. Run Phase 2 first." }

$configPath = 'C:\ProgramData\Burrow\burrow.yaml'
$phone = Get-BurrowStateValue -Phase '04-devices' -Name 'phone'
$laptop = Get-BurrowStateValue -Phase '04-devices' -Name 'laptop'
$ownerLogin = Get-BurrowStateValue -Phase '04-devices' -Name 'ownerLogin'
$pcMagicDNS = Get-BurrowStateValue -Phase '03-tailnet' -Name 'pcMagicDNS'
$findings = Get-BurrowStateValue -Phase '00-preflight' -Name 'findings'

if (-not $ownerLogin) { throw 'Phase 4 has not run: no owner login recorded.' }

# --------------------------------------------------------- 1. the config ---

if ((Test-Path $configPath) -and -not $RewriteConfig) {
    Write-BurrowLog "$configPath already exists; leaving it alone (-RewriteConfig to regenerate)" -Level Ok
} else {
    $agentHostname = Get-BurrowStateValue -Phase '07-service' -Name 'agentHostname'
    if (-not $agentHostname) { $agentHostname = New-BurrowHostname }

    $roots = @()
    if ($findings -and $findings.fileRootCandidates) {
        foreach ($r in $findings.fileRootCandidates) {
            $w = if ($r.writable) { 'true' } else { 'false' }
            $roots += "    - { name: `"$($r.name)`", path: '$($r.path)', writable: $w }"
        }
    }
    if ($roots.Count -eq 0) {
        $roots = @("    - { name: `"Documents`", path: '$(Join-Path $env:USERPROFILE 'Documents')', writable: true }")
    }

    $projects = @()
    $parents = @()
    if ($findings -and $findings.projectCandidates) {
        foreach ($p in @($findings.projectCandidates | Select-Object -First 12)) {
            $projects += "  - name: `"$($p.name)`""
            $projects += "    path: '$($p.path)'"
        }
        $parents = @($findings.projectCandidates | ForEach-Object { $_.parent } | Select-Object -Unique)
    }
    $parentLines = @()
    foreach ($p in $parents) { $parentLines += "  - '$p'" }
    if ($parentLines.Count -eq 0) { $parentLines = @("  - '$(Join-Path $env:USERPROFILE 'code')'") }

    $gitBash = if ($findings -and $findings.gitBash) { $findings.gitBash } else { 'C:\Program Files\Git\bin\bash.exe' }
    $sshHost = if ($pcMagicDNS) { $pcMagicDNS } else { $env:COMPUTERNAME }

    $yaml = @(
        '# Burrow agent configuration. Written by setup/07-service.ps1.',
        '# The annotated reference is config/burrow.example.yaml in the repo.',
        '# The agent refuses to start if anything here is invalid.',
        '',
        'owner:',
        "  tailscale_login: `"$ownerLogin`"",
        '',
        'device:',
        '  # Filled in by pairing (Phase 9). Only these nodes may reach Burrow.',
        '  allowed:',
        '  max_devices: 3',
        '',
        'agent:',
        "  hostname: `"$agentHostname`"",
        "  state_dir: 'C:\ProgramData\Burrow\state'",
        '  tsnet_logs_upload: false',
        '  auth:',
        '    session_idle_minutes: 30',
        '    session_absolute_hours: 12',
        '    stepup_seconds: 300',
        '    login_max_failures: 5',
        '    lockout_minutes: 15',
        '    pairing_window_minutes: 10',
        '',
        'claude:',
        '  binary: "claude"',
        "  git_bash_path: '$gitBash'",
        '  default_mode: "chat"',
        '  default_permission_mode: "default"',
        '  server_spawn: "same-dir"',
        '  server_capacity: 8',
        '  max_sessions: 6',
        '  restricted_token: true',
        '  ready_timeout_seconds: 30',
        '  scrollback_bytes: 2097152',
        '',
        'projects_parents:'
    ) + $parentLines + @(
        '',
        'projects:'
    ) + $projects + @(
        '',
        'files:',
        '  roots:'
    ) + $roots + @(
        '  upload_default_root: "Downloads"',
        '  max_upload_mb: 2048',
        '  extra_denylist: []',
        '',
        'wake:',
        '  method: "smartplug"',
        '  shortcuts:',
        '    on: "PC On"',
        '    off: "PC Off"',
        '    power_cycle: "PC Power Cycle"',
        '  wake_timeout_seconds: 180',
        '  power_cycle_after_seconds: 90',
        '  off_confirm_seconds: 30',
        '  waker_url: ""',
        '  router_hint: ""',
        '',
        'power:',
        '  keep_awake_while_sessions: true',
        '  auto_shutdown_idle_minutes: 0',
        '',
        'ssh:',
        "  user: `"$env:USERNAME`"",
        "  host: `"$sshHost`"",
        '',
        'audit:',
        "  path: 'C:\ProgramData\Burrow\audit\audit.jsonl'",
        '',
        'logging:',
        "  dir: 'C:\ProgramData\Burrow\logs'",
        '  level: "info"',
        '  max_files: 10',
        '  max_size_mb: 10'
    )

    if ($PSCmdlet.ShouldProcess($configPath, 'write burrow.yaml')) {
        Backup-BurrowItem -Path $configPath -Tag 'burrow-yaml' -WhatIf:$WhatIfPreference
        Set-Content -Path $configPath -Value ($yaml -join "`r`n") -Encoding utf8
        Set-BurrowStateValue -Phase '07-service' -Name 'agentHostname' -Value $agentHostname
        Write-BurrowLog "wrote $configPath (agent hostname $agentHostname)" -Level Ok
    }
}

# ------------------------------------------------- 2. enroll on the tailnet ---

$tsnetState = 'C:\ProgramData\Burrow\state\tsnet'
if (Test-Path (Join-Path $tsnetState 'tailscaled.state')) {
    Write-BurrowLog 'the agent is already enrolled on the tailnet' -Level Ok
} else {
    Write-BurrowLog 'enrolling the agent as its own tailnet node' -Level Step
    if ($PSCmdlet.ShouldProcess('burrow-agent', 'create an auth key and start once to enroll')) {
        $key = Invoke-BurrowTailscaleApi -Path '/tailnet/-/keys' -Method POST -Body @{
            capabilities  = @{ devices = @{ create = @{
                        reusable = $false; ephemeral = $false; preauthorized = $true
                        tags     = @('tag:burrow-agent')
                    } } }
            expirySeconds = 3600
            description   = 'burrow setup agent node single-use'
        }

        # The key reaches the agent through the process environment only, so it
        # never lands on disk or in the process list.
        $env:TS_AUTHKEY = $key.key
        try {
            Write-BurrowLog 'starting the agent once to register the node (about 60s for the first certificate)' -Level Info
            $proc = Start-Process -FilePath $burrow -ArgumentList @('run', '-config', $configPath) `
                -PassThru -NoNewWindow
            $deadline = (Get-Date).AddMinutes(3)
            $enrolled = $false
            while ((Get-Date) -lt $deadline) {
                Start-Sleep -Seconds 5
                if (Test-Path (Join-Path $tsnetState 'tailscaled.state')) { $enrolled = $true; break }
                if ($proc.HasExited) { break }
            }
            if (-not $proc.HasExited) { Stop-Process -Id $proc.Id -Force -ErrorAction SilentlyContinue }
            if (-not $enrolled) { throw 'the agent did not register a tailnet node; check C:\ProgramData\Burrow\logs\burrow.log' }
            Write-BurrowLog 'agent enrolled' -Level Ok
        } finally {
            Remove-Item Env:TS_AUTHKEY -ErrorAction SilentlyContinue
        }
    }
}

# ------------------------------------------ 3. log on as a service right ---

if ($PSCmdlet.ShouldProcess($env:USERNAME, 'grant "Log on as a service"')) {
    $sid = (New-Object System.Security.Principal.NTAccount("$env:USERDOMAIN\$env:USERNAME")).
        Translate([System.Security.Principal.SecurityIdentifier]).Value

    $inf = Join-Path $env:TEMP 'burrow-svc-right.inf'
    $db = Join-Path $env:TEMP 'burrow-svc-right.sdb'
    & secedit /export /cfg $inf /quiet

    $content = Get-Content $inf
    $line = $content | Where-Object { $_ -match '^SeServiceLogonRight\s*=' }
    if ($line) {
        if ($line -notmatch [regex]::Escape($sid)) {
            $content = $content -replace [regex]::Escape($line), "$line,*$sid"
        } else {
            Write-BurrowLog 'the account already has "Log on as a service"' -Level Ok
        }
    } else {
        $content = $content -replace '(?m)^\[Privilege Rights\]', "[Privilege Rights]`r`nSeServiceLogonRight = *$sid"
    }
    Set-Content -Path $inf -Value $content -Encoding Unicode
    & secedit /configure /db $db /cfg $inf /areas USER_RIGHTS /quiet
    Remove-Item $inf, $db -Force -ErrorAction SilentlyContinue
    Write-BurrowLog 'granted "Log on as a service"' -Level Ok
}

# ------------------------------------------------- 4. install the service ---

$svc = Get-Service -Name 'Burrow' -ErrorAction SilentlyContinue
if ($svc -and $svc.Status -ne 'Running') {
    # Left behind by an earlier run whose password Windows rejected: it exists
    # but can never start. Removing it lets the install below ask again.
    try {
        Start-Service -Name 'Burrow' -ErrorAction Stop
    } catch {
        Write-BurrowLog 'the Burrow service exists but will not start (usually a mistyped password); reinstalling it' -Level Warn
        if ($PSCmdlet.ShouldProcess('Burrow service', 'remove the broken install')) {
            & $burrow uninstall
            Start-Sleep -Seconds 2
            $svc = Get-Service -Name 'Burrow' -ErrorAction SilentlyContinue
        }
    }
}
if ($svc) {
    Write-BurrowLog 'the Burrow service already exists' -Level Ok
} else {
    Write-Host ''
    Write-BurrowLog 'The next step asks for your Windows password.' -Level Manual
    Write-Host '      It is typed into a console prompt with echo off, handed straight to' -ForegroundColor Gray
    Write-Host '      the Service Control Manager, and never logged or stored by Burrow.' -ForegroundColor Gray
    Write-Host '      If you sign in with a Microsoft account, use that account password,' -ForegroundColor Gray
    Write-Host '      not your PIN. A PIN is device-local and cannot authenticate a service.' -ForegroundColor Gray
    Write-Host ''

    if ($PSCmdlet.ShouldProcess('Burrow service', 'install')) {
        # Run in a visible window of its own so the password prompt is clearly
        # separate from this transcript.
        $p = Start-Process -FilePath $burrow `
            -ArgumentList @('install', '-config', $configPath) -Wait -PassThru -NoNewWindow
        if ($p.ExitCode -ne 0) { throw 'burrow install failed' }
        Write-BurrowLog 'service installed' -Level Ok
    }
}

# ---------------------------------------------------------- 5. start it ---

if ($PSCmdlet.ShouldProcess('Burrow service', 'start and wait for health')) {
    Start-Service -Name 'Burrow' -ErrorAction Stop
    Write-BurrowLog 'waiting for the agent (the first TLS certificate can take a minute)' -Level Step

    $deadline = (Get-Date).AddMinutes(3)
    $healthy = $false
    while ((Get-Date) -lt $deadline) {
        Start-Sleep -Seconds 5
        & $burrow status 2>&1 | Out-Null
        if ($LASTEXITCODE -eq 0) { $healthy = $true; break }
    }
    if (-not $healthy) {
        throw 'the service did not become healthy. Check C:\ProgramData\Burrow\logs\burrow.log'
    }
    Write-BurrowLog 'the agent is up' -Level Ok
    & $burrow status
}

# ------------------------------------------------------------ 6. selftest ---

if ($PSCmdlet.ShouldProcess('burrow selftest', 'run')) {
    Write-Host ''
    Write-BurrowLog 'running selftest' -Level Step
    & $burrow selftest
    if ($LASTEXITCODE -ne 0) {
        Write-BurrowLog 'some selftest checks failed — read them above before continuing' -Level Warn
    }
}

if (-not $WhatIfPreference) { Set-BurrowPhaseComplete -Phase '07-service' }
