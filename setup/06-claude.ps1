<#
.SYNOPSIS
  Phase 6 — Claude Code settings, then the one-time sign-in steps only you can do.

.DESCRIPTION
  The automated half merges remoteControlAtStartup into your Claude settings so
  every interactive session — including ones started over SSH — shows up in the
  Claude app.

  The manual half matters more than it looks: the Burrow service runs in
  session 0, which has no desktop and no browser. An OAuth prompt there can
  never be answered. Claude has to be signed in, and Remote Control accepted
  once, from a normal terminal first (docs/02-ARCHITECTURE.md §5).
#>
[CmdletBinding(SupportsShouldProcess)]
param()

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest
# Import-Module has no -WhatIf in Windows PowerShell 5.1, and under -WhatIf
# the modules it pulls in preview every Set-Alias they do. Suppressed
# around the import only: importing a module changes nothing.
$prevWhatIf = $WhatIfPreference
$WhatIfPreference = $false
Import-Module (Join-Path $PSScriptRoot 'lib\Burrow.Setup.psm1') -Force
$WhatIfPreference = $prevWhatIf

Write-BurrowBanner -Phase '6' -Title 'Claude Code configuration'

$settingsPath = Join-Path $env:USERPROFILE '.claude\settings.json'

Backup-BurrowItem -Path $settingsPath -Tag 'claude-settings' `
    -RevertHint 'copy the backup back over ~/.claude/settings.json' -WhatIf:$WhatIfPreference

# Merged, not overwritten: this file is yours and may hold hooks, permissions
# and model settings that have nothing to do with Burrow.
$settings = [ordered]@{}
if (Test-Path $settingsPath) {
    try {
        $existing = Get-Content $settingsPath -Raw | ConvertFrom-Json
        foreach ($p in $existing.PSObject.Properties) { $settings[$p.Name] = $p.Value }
    } catch {
        throw "~/.claude/settings.json is not valid JSON; fix it before running this phase: $($_.Exception.Message)"
    }
}

$was = if ($settings.Contains('remoteControlAtStartup')) { $settings['remoteControlAtStartup'] } else { $null }
$settings['remoteControlAtStartup'] = $true

if ($PSCmdlet.ShouldProcess($settingsPath, 'set remoteControlAtStartup = true')) {
    $null = New-Item -ItemType Directory -Force -Path (Split-Path $settingsPath)
    ($settings | ConvertTo-Json -Depth 20) | Set-Content $settingsPath -Encoding utf8
    Register-BurrowChange -Tag 'claude-remote-control-at-startup' -Was "$was" -Now 'true' `
        -Revert 'set remoteControlAtStartup back in ~/.claude/settings.json' -WhatIf:$WhatIfPreference
    Write-BurrowLog 'remoteControlAtStartup is on' -Level Ok
}

# Blockers worth re-checking here: an API key set since Phase 0 would route
# Claude away from the subscription Remote Control needs.
$blockerVars = @('ANTHROPIC_API_KEY', 'ANTHROPIC_AUTH_TOKEN', 'CLAUDE_CODE_USE_BEDROCK',
    'CLAUDE_CODE_USE_VERTEX', 'CLAUDE_CODE_USE_FOUNDRY')
$found = @()
foreach ($v in $blockerVars) {
    foreach ($scope in @('Process', 'User', 'Machine')) {
        if ([Environment]::GetEnvironmentVariable($v, $scope)) { $found += "$v ($scope)" }
    }
}
if ($found.Count -gt 0) {
    throw "Claude Code is routed away from the claude.ai subscription by: $($found -join ', '). Remote Control needs the subscription; unset these and re-run."
}

$projects = Get-BurrowStateValue -Phase '00-preflight' -Name 'findings'
$firstProject = $null
if ($projects -and $projects.projectCandidates -and @($projects.projectCandidates).Count -gt 0) {
    $firstProject = @($projects.projectCandidates)[0].path
}
$exampleDir = if ($firstProject) { $firstProject } else { 'one of your project folders' }

Show-BurrowManualStep -Title 'Accept Remote Control once, from a normal terminal' -Steps @(
    "Open a NEW terminal (not this one) and run:  cd `"$exampleDir`"",
    'Run:  claude remote-control',
    'Answer y to "Enable Remote Control?" and trust the folder if it asks.',
    'Press space to show the QR code, scan it with your iPhone, and confirm the session opens in the Claude app.',
    'Press Ctrl+C to stop it. Burrow will start its own sessions from now on.',
    'This has to happen here because the Burrow service has no browser to answer a prompt in.'
)

Show-BurrowManualStep -Title 'Turn on push notifications' -Steps @(
    'In that same terminal run:  claude',
    'Run:  /config',
    'Turn on "Push when actions required" and "Push when Claude decides".',
    'That is what makes your phone buzz when a session is waiting on you.'
)

Show-BurrowManualStep -Title 'Turn on Claude trusted devices' -Steps @(
    'Open https://claude.ai/settings/account in a browser.',
    'Turn on "Require trusted devices".',
    'Enroll your iPhone (via the Claude app) and your laptop when prompted.',
    'This is the control that stops a stolen claude.ai session reaching your sessions.'
)

# Verified rather than assumed: the credential file is what the service will
# rely on, and it is cheap to check.
$signedIn = (Test-Path (Join-Path $env:USERPROFILE '.claude\.credentials.json')) -or
            (Test-Path (Join-Path $env:USERPROFILE '.claude.json'))
if ($signedIn) {
    Write-BurrowLog 'Claude Code credentials are present for this account' -Level Ok
} else {
    Write-BurrowLog 'no Claude Code credentials found for this account — sessions will fail to start' -Level Fail
    throw 'sign in to Claude Code in a normal terminal, then re-run this phase'
}

if (-not $WhatIfPreference) { Set-BurrowPhaseComplete -Phase '06-claude' }
