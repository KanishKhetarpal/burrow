<#
.SYNOPSIS
  Shared helpers for the Burrow setup phases.

.DESCRIPTION
  Logging, the setup-state file, backups, confirmations and the manual-step
  pattern. Every phase script dot-sources this so they behave the same way and
  so a phase can be re-run safely.

  Conventions these helpers enforce:
   - Nothing destructive happens without a backup recorded in setup-state.json
     together with how to revert it.
   - Every script supports -WhatIf and prints its plan before touching anything.
   - A secret never reaches the transcript: Write-BurrowSecretWindow opens a
     separate window for anything that must be read by a human once.
#>

Set-StrictMode -Version Latest

$script:StateDir = 'C:\ProgramData\Burrow'
$script:StatePath = Join-Path $script:StateDir 'setup-state.json'
$script:BackupDir = Join-Path $script:StateDir 'backup'
$script:LogPath = Join-Path $script:StateDir 'logs\setup.log'

# ---------------------------------------------------------------- logging ---

function Write-BurrowLog {
    <#
    .SYNOPSIS
      Write one line to the console and the setup log.
    #>
    [CmdletBinding()]
    param(
        [Parameter(Mandatory, Position = 0)][string]$Message,
        [ValidateSet('Info', 'Step', 'Ok', 'Warn', 'Fail', 'Manual', 'Confirm')]
        [string]$Level = 'Info'
    )

    $prefix, $colour = switch ($Level) {
        'Step'    { '==>', 'Cyan' }
        'Ok'      { '  ok', 'Green' }
        'Warn'    { '  !!', 'Yellow' }
        'Fail'    { 'FAIL', 'Red' }
        'Manual'  { ' YOU', 'Magenta' }
        'Confirm' { ' ???', 'Yellow' }
        default   { '   .', 'Gray' }
    }

    Write-Host "$prefix $Message" -ForegroundColor $colour

    try {
        $dir = Split-Path $script:LogPath -Parent
        if (-not (Test-Path $dir)) { $null = New-Item -ItemType Directory -Force -Path $dir }
        $stamp = (Get-Date).ToUniversalTime().ToString('yyyy-MM-ddTHH:mm:ssZ')
        Add-Content -Path $script:LogPath -Value "$stamp [$Level] $Message" -Encoding utf8
    } catch {
        # Logging must never be the reason a phase fails.
    }
}

function Write-BurrowBanner {
    [CmdletBinding()]
    param([Parameter(Mandatory)][string]$Phase, [string]$Title)
    Write-Host ''
    Write-Host ('─' * 64) -ForegroundColor DarkGray
    Write-Host " Phase $Phase — $Title" -ForegroundColor White
    Write-Host ('─' * 64) -ForegroundColor DarkGray
    Write-BurrowLog "phase $Phase start: $Title" -Level Info
}

# ------------------------------------------------------------------ state ---

function Get-BurrowState {
    <#
    .SYNOPSIS
      Read setup-state.json, or an empty object if there is none yet.
    #>
    [CmdletBinding()]
    param()

    if (-not (Test-Path $script:StatePath)) {
        return [pscustomobject]@{
            version   = 1
            startedAt = (Get-Date).ToUniversalTime().ToString('o')
            phases    = [pscustomobject]@{}
        }
    }
    Get-Content $script:StatePath -Raw | ConvertFrom-Json
}

function Save-BurrowState {
    [CmdletBinding(SupportsShouldProcess)]
    param([Parameter(Mandatory)]$State)

    if (-not $PSCmdlet.ShouldProcess($script:StatePath, 'write setup state')) { return }
    if (-not (Test-Path $script:StateDir)) {
        $null = New-Item -ItemType Directory -Force -Path $script:StateDir
    }
    $State | ConvertTo-Json -Depth 12 | Set-Content -Path $script:StatePath -Encoding utf8
}

function Set-BurrowStateValue {
    <#
    .SYNOPSIS
      Record one value under a phase, so a later phase or a resume can read it.
    .EXAMPLE
      Set-BurrowStateValue -Phase '03-tailnet' -Name 'tailnetName' -Value 'tail1a2b.ts.net'
    #>
    [CmdletBinding(SupportsShouldProcess)]
    param(
        [Parameter(Mandatory)][string]$Phase,
        [Parameter(Mandatory)][string]$Name,
        [Parameter(Mandatory)][AllowNull()]$Value
    )

    $state = Get-BurrowState
    if (-not $state.phases.PSObject.Properties[$Phase]) {
        $state.phases | Add-Member -NotePropertyName $Phase -NotePropertyValue ([pscustomobject]@{})
    }
    $bucket = $state.phases.$Phase
    if ($bucket.PSObject.Properties[$Name]) { $bucket.$Name = $Value }
    else { $bucket | Add-Member -NotePropertyName $Name -NotePropertyValue $Value }

    Save-BurrowState -State $state -WhatIf:$WhatIfPreference
}

function Get-BurrowStateValue {
    [CmdletBinding()]
    param([Parameter(Mandatory)][string]$Phase, [Parameter(Mandatory)][string]$Name)

    $state = Get-BurrowState
    if (-not $state.phases.PSObject.Properties[$Phase]) { return $null }
    $bucket = $state.phases.$Phase
    if (-not $bucket.PSObject.Properties[$Name]) { return $null }
    $bucket.$Name
}

function Set-BurrowPhaseComplete {
    [CmdletBinding(SupportsShouldProcess)]
    param([Parameter(Mandatory)][string]$Phase)
    Set-BurrowStateValue -Phase $Phase -Name 'completedAt' `
        -Value ((Get-Date).ToUniversalTime().ToString('o')) -WhatIf:$WhatIfPreference
    Write-BurrowLog "phase $Phase complete" -Level Ok
}

function Test-BurrowPhaseComplete {
    [CmdletBinding()]
    param([Parameter(Mandatory)][string]$Phase)
    $null -ne (Get-BurrowStateValue -Phase $Phase -Name 'completedAt')
}

# ---------------------------------------------------------------- backups ---

function Backup-BurrowItem {
    <#
    .SYNOPSIS
      Copy a file aside before changing it, and record how to put it back.

    .DESCRIPTION
      Every reversible change goes through here. The revert hint is stored in
      setup-state.json so uninstall.ps1 — and a human reading the file — can see
      exactly what was changed and how to undo it.
    #>
    [CmdletBinding(SupportsShouldProcess)]
    param(
        [Parameter(Mandatory)][string]$Path,
        [Parameter(Mandatory)][string]$Tag,
        [string]$RevertHint
    )

    # A phase that failed part-way is re-run from the top. Its second backup
    # would capture the already-changed file, and uninstall would "restore"
    # that, so the first backup recorded for a tag is the one kept.
    if (Get-BurrowStateValue -Phase 'backups' -Name $Tag) {
        Write-BurrowLog "keeping the original backup for $Tag from the first run" -Level Info
        return
    }

    if (-not (Test-Path $Path)) {
        Write-BurrowLog "nothing to back up at $Path (it does not exist yet)" -Level Info
        Set-BurrowStateValue -Phase 'backups' -Name $Tag -Value ([pscustomobject]@{
                original   = $Path
                backup     = $null
                existed    = $false
                revertHint = "delete $Path (it did not exist before setup)"
            }) -WhatIf:$WhatIfPreference
        return
    }

    if (-not (Test-Path $script:BackupDir)) {
        if ($PSCmdlet.ShouldProcess($script:BackupDir, 'create backup directory')) {
            $null = New-Item -ItemType Directory -Force -Path $script:BackupDir
        }
    }

    $stamp = (Get-Date).ToString('yyyyMMdd-HHmmss')
    $dest = Join-Path $script:BackupDir "$Tag-$stamp$([System.IO.Path]::GetExtension($Path))"

    if ($PSCmdlet.ShouldProcess($Path, "back up to $dest")) {
        Copy-Item -Path $Path -Destination $dest -Force
        Write-BurrowLog "backed up $Path -> $dest" -Level Ok
    }

    Set-BurrowStateValue -Phase 'backups' -Name $Tag -Value ([pscustomobject]@{
            original   = $Path
            backup     = $dest
            existed    = $true
            revertHint = if ($RevertHint) { $RevertHint } else { "copy $dest back over $Path" }
        }) -WhatIf:$WhatIfPreference
}

function Register-BurrowChange {
    <#
    .SYNOPSIS
      Record a change that is not a file, with how to revert it.
    .EXAMPLE
      Register-BurrowChange -Tag 'standby-timeout-ac' -Was '30' -Now '0' `
          -Revert 'powercfg /change standby-timeout-ac 30'
    #>
    [CmdletBinding(SupportsShouldProcess)]
    param(
        [Parameter(Mandatory)][string]$Tag,
        [AllowNull()][string]$Was,
        [AllowNull()][string]$Now,
        [Parameter(Mandatory)][string]$Revert
    )
    Set-BurrowStateValue -Phase 'changes' -Name $Tag -Value ([pscustomobject]@{
            was    = $Was
            now    = $Now
            revert = $Revert
            at     = (Get-Date).ToUniversalTime().ToString('o')
        }) -WhatIf:$WhatIfPreference
    Write-BurrowLog "recorded change '$Tag' (revert: $Revert)" -Level Info
}

# ----------------------------------------------------------- interactions ---

function Confirm-BurrowChange {
    <#
    .SYNOPSIS
      A CONFIRM point: explain in two lines, then wait for yes.

    .DESCRIPTION
      Used for changes that are irreversible or could break something already
      in use. -Yes skips the prompt, for an unattended re-run of a phase that
      was already approved once.
    #>
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$What,
        [Parameter(Mandatory)][string]$Why,
        [switch]$Yes
    )

    Write-Host ''
    Write-BurrowLog $What -Level Confirm
    Write-Host "      $Why" -ForegroundColor Gray

    if ($Yes) {
        Write-BurrowLog 'approved by -Yes' -Level Ok
        return $true
    }
    if ($WhatIfPreference) {
        Write-BurrowLog 'would ask for confirmation here (-WhatIf)' -Level Info
        return $false
    }

    $answer = Read-Host '      Continue? [y/N]'
    $ok = $answer -match '^(y|yes)$'
    Write-BurrowLog "confirm '$What' -> $(if ($ok) { 'yes' } else { 'no' })" -Level Info
    $ok
}

function Show-BurrowManualStep {
    <#
    .SYNOPSIS
      A MANUAL step: print numbered instructions and wait.

    .DESCRIPTION
      For anything only a person can do — a phone tap, a BIOS menu, a browser
      login, typing a password into an OS prompt. The caller verifies the
      result afterwards rather than trusting that it happened.
    #>
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$Title,
        [Parameter(Mandatory)][string[]]$Steps,
        [string]$ThenPress = 'Press Enter when that is done'
    )

    Write-Host ''
    Write-BurrowLog "$Title — this one needs you" -Level Manual
    $i = 1
    foreach ($s in $Steps) {
        Write-Host ("      {0}. {1}" -f $i, $s) -ForegroundColor White
        $i++
    }
    Write-Host ''
    if ($WhatIfPreference) {
        Write-BurrowLog 'would wait for you here (-WhatIf)' -Level Info
        return
    }
    $null = Read-Host "      $ThenPress"
}

function Write-BurrowSecretWindow {
    <#
    .SYNOPSIS
      Show a secret in a separate window, never in this transcript.

    .DESCRIPTION
      A generated password has to reach a human exactly once. Printing it here
      would put it in the console scrollback and in any transcript of this
      session, so it goes to a new window that the person closes themselves.
      The value is passed to that window through a temp file with a restrictive
      ACL, which is deleted by the window itself.
    #>
    [CmdletBinding(SupportsShouldProcess)]
    param(
        [Parameter(Mandatory)][string]$Title,
        [Parameter(Mandatory)][string]$Secret,
        [string]$Instructions = 'Save this now. It will not be shown again.'
    )

    if (-not $PSCmdlet.ShouldProcess($Title, 'show a secret in a separate window')) { return }

    $tmp = Join-Path $env:TEMP ("burrow-secret-{0}.txt" -f ([guid]::NewGuid().ToString('N')))
    Set-Content -Path $tmp -Value $Secret -Encoding utf8 -NoNewline

    # Only this account may read it, and only until the window deletes it.
    & icacls $tmp /inheritance:r /grant:r "$($env:USERNAME):(R)" | Out-Null

    $viewer = Join-Path $PSScriptRoot '..\secret\show-secret.ps1'
    if (-not (Test-Path $viewer)) {
        Write-BurrowLog 'the secret viewer script is missing; not showing the secret' -Level Fail
        Remove-Item $tmp -Force -ErrorAction SilentlyContinue
        throw 'setup/secret/show-secret.ps1 not found'
    }

    Start-Process -FilePath 'powershell.exe' -ArgumentList @(
        '-NoProfile', '-ExecutionPolicy', 'Bypass', '-File', $viewer,
        '-Path', $tmp, '-Title', $Title, '-Instructions', $Instructions
    ) -Wait

    Remove-Item $tmp -Force -ErrorAction SilentlyContinue
    Write-BurrowLog "showed '$Title' in a separate window" -Level Ok
}

function New-BurrowPassword {
    <#
    .SYNOPSIS
      Generate a password with a cryptographic RNG.
    #>
    [CmdletBinding()]
    param([int]$Length = 32)

    # Ambiguous characters are left out: this gets read off a screen and typed
    # into a phone. Quotes, backslashes and backticks are out too, because the
    # value passes through net.exe and PowerShell quoting.
    $alphabet = 'ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz23456789-_=+@#%^'
    $bytes = New-Object byte[] ($Length * 4)
    $rng = [System.Security.Cryptography.RandomNumberGenerator]::Create()
    try { $rng.GetBytes($bytes) } finally { $rng.Dispose() }

    $sb = New-Object System.Text.StringBuilder
    $i = 0
    while ($sb.Length -lt $Length) {
        # Rejection sampling, so every character is equally likely.
        $b = $bytes[$i % $bytes.Length]
        $i++
        if ($b -ge (256 - (256 % $alphabet.Length))) { continue }
        $null = $sb.Append($alphabet[$b % $alphabet.Length])
    }
    $sb.ToString()
}

function New-BurrowHostname {
    <#
    .SYNOPSIS
      A random, non-identifying tailnet hostname: n-<8 hex>.

    .DESCRIPTION
      These names appear in public Certificate Transparency logs as
      <hostname>.<tailnet>.ts.net, so they must say nothing about the machine
      or its owner (docs/03-SECURITY.md T15).
    #>
    [CmdletBinding()]
    param()
    $bytes = New-Object byte[] 4
    $rng = [System.Security.Cryptography.RandomNumberGenerator]::Create()
    try { $rng.GetBytes($bytes) } finally { $rng.Dispose() }
    'n-' + (($bytes | ForEach-Object { $_.ToString('x2') }) -join '')
}

# ------------------------------------------------------------ environment ---

function Test-BurrowElevated {
    [CmdletBinding()]
    param()
    $id = [Security.Principal.WindowsIdentity]::GetCurrent()
    ([Security.Principal.WindowsPrincipal]$id).IsInRole(
        [Security.Principal.WindowsBuiltInRole]::Administrator)
}

function Assert-BurrowElevated {
    [CmdletBinding()]
    param()
    if (-not (Test-BurrowElevated)) {
        throw 'This phase changes system settings and must run from an elevated terminal (Run as administrator).'
    }
}

function Update-BurrowPath {
    <#
    .SYNOPSIS
      Refresh PATH in this process after an install.

    .DESCRIPTION
      winget updates the stored environment but not the running shell, so a
      tool installed a moment ago is not on PATH until this runs.
    #>
    [CmdletBinding()]
    param()
    $machine = [Environment]::GetEnvironmentVariable('Path', 'Machine')
    $user = [Environment]::GetEnvironmentVariable('Path', 'User')
    $env:Path = "$machine;$user"
    Write-BurrowLog 'refreshed PATH for this process' -Level Info
}

function Get-BurrowTailscaleCli {
    <#
    .SYNOPSIS
      Locate tailscale.exe, which is not on PATH by default.
    #>
    [CmdletBinding()]
    param()
    # No null-conditional operator here: this module is imported by
    # 00-preflight, which runs on Windows PowerShell 5.1 before Phase 1
    # installs PowerShell 7.
    $onPath = Get-Command tailscale.exe -ErrorAction SilentlyContinue
    $candidates = @(
        $(if ($onPath) { $onPath.Source }),
        'C:\Program Files\Tailscale\tailscale.exe',
        'C:\Program Files (x86)\Tailscale IPN\tailscale.exe'
    ) | Where-Object { $_ -and (Test-Path $_) }
    $candidates | Select-Object -First 1
}

function Invoke-BurrowTailscaleApi {
    <#
    .SYNOPSIS
      Call the Tailscale API with the token from the environment.

    .DESCRIPTION
      The token is read from $env:TS_API_KEY only — never a parameter, never a
      file, never a command line, where another local process could read it out
      of the process list (docs/03-SECURITY.md §4).
    #>
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$Path,
        [ValidateSet('GET', 'POST', 'PATCH', 'PUT', 'DELETE')][string]$Method = 'GET',
        $Body,
        [string]$ContentType = 'application/json'
    )

    if (-not $env:TS_API_KEY) {
        throw 'TS_API_KEY is not set in this process. See docs/06-SETUP-RUNBOOK.md "Before you start".'
    }

    $uri = "https://api.tailscale.com/api/v2$Path"
    $headers = @{ Authorization = "Bearer $env:TS_API_KEY" }

    $params = @{
        Uri             = $uri
        Method          = $Method
        Headers         = $headers
        ErrorAction     = 'Stop'
        UseBasicParsing = $true
    }
    if ($null -ne $Body) {
        $params.ContentType = $ContentType
        $params.Body = if ($Body -is [string]) { $Body } else { $Body | ConvertTo-Json -Depth 12 }
    }

    try {
        Invoke-RestMethod @params
    } catch {
        # The response body carries the useful detail; the status line alone
        # rarely says what was wrong with a policy.
        $detail = ''
        try { $detail = $_.ErrorDetails.Message } catch {}
        throw "Tailscale API $Method $Path failed: $($_.Exception.Message) $detail"
    }
}

function Test-BurrowPort {
    <#
    .SYNOPSIS
      Is anything listening on this TCP port, and what owns it?
    #>
    [CmdletBinding()]
    param([Parameter(Mandatory)][int]$Port)

    $conns = Get-NetTCPConnection -State Listen -LocalPort $Port -ErrorAction SilentlyContinue
    if (-not $conns) { return $null }
    $conns | ForEach-Object {
        $proc = Get-Process -Id $_.OwningProcess -ErrorAction SilentlyContinue
        [pscustomobject]@{
            Port    = $_.LocalPort
            Address = $_.LocalAddress
            Pid     = $_.OwningProcess
            Process = $proc.ProcessName
        }
    }
}

Export-ModuleMember -Function @(
    'Write-BurrowLog', 'Write-BurrowBanner',
    'Get-BurrowState', 'Save-BurrowState', 'Set-BurrowStateValue', 'Get-BurrowStateValue',
    'Set-BurrowPhaseComplete', 'Test-BurrowPhaseComplete',
    'Backup-BurrowItem', 'Register-BurrowChange',
    'Confirm-BurrowChange', 'Show-BurrowManualStep', 'Write-BurrowSecretWindow',
    'New-BurrowPassword', 'New-BurrowHostname',
    'Test-BurrowElevated', 'Assert-BurrowElevated', 'Update-BurrowPath',
    'Get-BurrowTailscaleCli', 'Invoke-BurrowTailscaleApi', 'Test-BurrowPort'
)
