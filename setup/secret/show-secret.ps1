<#
.SYNOPSIS
  Show one secret in its own window, then delete the file that carried it.

.DESCRIPTION
  Called by Write-BurrowSecretWindow. A generated password has to reach a human
  exactly once; printing it in the setup console would put it in the scrollback
  and in any transcript of that session, so it is shown here instead and this
  window is closed by the person who read it.

  The value arrives in a temp file with a restrictive ACL rather than on the
  command line, where any local process could read it out of the process list.
#>
[CmdletBinding()]
param(
    [Parameter(Mandatory)][string]$Path,
    [Parameter(Mandatory)][string]$Title,
    [string]$Instructions = 'Save this now. It will not be shown again.'
)

$ErrorActionPreference = 'Stop'

try {
    if (-not (Test-Path $Path)) { throw "the secret file is gone: $Path" }
    $secret = (Get-Content -Path $Path -Raw).Trim()

    $host.UI.RawUI.WindowTitle = "Burrow — $Title"
    Clear-Host

    Write-Host ''
    Write-Host '  ┌────────────────────────────────────────────────────────┐' -ForegroundColor DarkGray
    Write-Host ('  │ {0,-54} │' -f $Title) -ForegroundColor White
    Write-Host '  └────────────────────────────────────────────────────────┘' -ForegroundColor DarkGray
    Write-Host ''
    Write-Host '   ' -NoNewline
    Write-Host $secret -ForegroundColor Green
    Write-Host ''
    Write-Host "  $Instructions" -ForegroundColor Yellow
    Write-Host ''
    Write-Host '  This window is the only place it is shown. It is not in the' -ForegroundColor DarkGray
    Write-Host '  setup log, not in the repo, and not on any command line.' -ForegroundColor DarkGray
    Write-Host ''

    # Offered rather than automatic: a clipboard is readable by other
    # applications, so it should be a deliberate choice.
    $copy = Read-Host '  Copy to the clipboard? [y/N]'
    if ($copy -match '^(y|yes)$') {
        Set-Clipboard -Value $secret
        Write-Host '  Copied. Paste it where it is needed, then copy something else.' -ForegroundColor Gray
    }

    Write-Host ''
    $null = Read-Host '  Press Enter once you have saved it'
} finally {
    # Always, even if the display failed: the file must not outlive this window.
    Remove-Item -Path $Path -Force -ErrorAction SilentlyContinue
    Clear-Host
}
