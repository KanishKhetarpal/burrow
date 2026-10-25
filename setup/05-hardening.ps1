<#
.SYNOPSIS
  Phase 5 — OpenSSH, SMB, power settings and optional remote desktop, all
  scoped to the two allowed devices.

.DESCRIPTION
  This is the phase that changes the machine most, so everything reversible is
  backed up first and recorded in setup-state.json with how to undo it.

  The shape of every firewall rule here is the same: allow the port only from
  the iPhone's and laptop's tailnet IPs, on the Tailscale interface. The
  built-in rules that would open the same ports to the LAN are disabled, which
  is the difference between "reachable only over Tailscale" and "reachable from
  anything on your home network" (docs/03-SECURITY.md T2).

  ⚠️ Several steps stop for confirmation. Read them: a couple can affect things
  you already use on the LAN.

  Requires an elevated terminal. PowerShell 7 recommended.
#>
[CmdletBinding(SupportsShouldProcess)]
param(
    [switch]$Yes,
    [ValidateSet('none', 'rdp', 'sunshine')][string]$RemoteDesktop = 'none',
    # Which file roots to share over SMB. Defaults to what Phase 0 recorded.
    [string[]]$ShareRoots,
    [switch]$SkipSSH,
    [switch]$SkipSMB,
    [switch]$SkipPower
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

Write-BurrowBanner -Phase '5' -Title 'Windows hardening and services'
Assert-BurrowElevated

$phone = Get-BurrowStateValue -Phase '04-devices' -Name 'phone'
$laptop = Get-BurrowStateValue -Phase '04-devices' -Name 'laptop'
if (-not $phone -or -not $laptop) { throw 'Phase 4 has not run: no device IPs recorded.' }

$phoneIP = $phone.ip
$laptopIP = $laptop.ip
$bothIPs = @($phoneIP, $laptopIP)
$me = $env:USERNAME
$backupDir = 'C:\ProgramData\Burrow\backup'
$null = New-Item -ItemType Directory -Force -Path $backupDir

Write-BurrowLog "scoping everything to phone=$phoneIP laptop=$laptopIP" -Level Info

# The Tailscale interface alias, so a rule cannot match traffic arriving on
# the LAN adapter even if an address were spoofed.
$tsAlias = (Get-NetAdapter | Where-Object { $_.InterfaceDescription -match 'Tailscale' } |
    Select-Object -First 1).Name
if (-not $tsAlias) {
    Write-BurrowLog 'no Tailscale adapter found; rules will be scoped by address only' -Level Warn
}

function New-BurrowFirewallRule {
    param(
        [Parameter(Mandatory)][string]$Name,
        [Parameter(Mandatory)][string]$Protocol,
        [Parameter(Mandatory)][string]$LocalPort,
        [Parameter(Mandatory)][string[]]$RemoteAddress,
        [string]$Description
    )
    Get-NetFirewallRule -DisplayName $Name -ErrorAction SilentlyContinue | Remove-NetFirewallRule
    $params = @{
        DisplayName   = $Name
        Direction     = 'Inbound'
        Action        = 'Allow'
        Protocol      = $Protocol
        LocalPort     = $LocalPort
        RemoteAddress = $RemoteAddress
        Profile       = 'Any'
        Description   = $Description
    }
    if ($tsAlias) { $params.InterfaceAlias = $tsAlias }
    $null = New-NetFirewallRule @params
    Write-BurrowLog "firewall: $Name $Protocol/$LocalPort from $($RemoteAddress -join ', ')" -Level Ok
}

# ------------------------------------------------------ firewall baseline ---

Write-BurrowLog 'backing up the firewall' -Level Step
$fwBackup = Join-Path $backupDir ("firewall-{0}.wfw" -f (Get-Date -Format 'yyyyMMdd-HHmmss'))
if ($PSCmdlet.ShouldProcess('Windows Firewall', 'export')) {
    & netsh advfirewall export $fwBackup | Out-Null
    Register-BurrowChange -Tag 'firewall-export' -Was $fwBackup -Now 'Burrow-* rules added' `
        -Revert "netsh advfirewall import $fwBackup" -WhatIf:$WhatIfPreference
}

$profiles = Get-NetFirewallProfile
foreach ($p in $profiles) {
    if (-not $p.Enabled -or $p.DefaultInboundAction -ne 'Block') {
        Write-BurrowLog "firewall profile $($p.Name): enabled=$($p.Enabled) inbound=$($p.DefaultInboundAction)" -Level Warn
        if ($PSCmdlet.ShouldProcess($p.Name, 'enable firewall, default-deny inbound')) {
            Set-NetFirewallProfile -Name $p.Name -Enabled True -DefaultInboundAction Block
            Write-BurrowLog "profile $($p.Name) set to on, inbound default block" -Level Ok
        }
    }
}

# ----------------------------------------------------- 5.1 OpenSSH Server ---

if (-not $SkipSSH) {
    Write-BurrowLog '5.1 OpenSSH Server' -Level Step

    $cap = Get-WindowsCapability -Online -Name 'OpenSSH.Server*' | Select-Object -First 1
    if ($cap.State -ne 'Installed') {
        if ($PSCmdlet.ShouldProcess('OpenSSH.Server', 'install Windows capability')) {
            $null = Add-WindowsCapability -Online -Name $cap.Name
            Write-BurrowLog 'OpenSSH Server installed' -Level Ok
        }
    } else {
        Write-BurrowLog 'OpenSSH Server is already installed' -Level Ok
    }

    $sshdConfig = 'C:\ProgramData\ssh\sshd_config'
    Backup-BurrowItem -Path $sshdConfig -Tag 'sshd_config' `
        -RevertHint 'copy the backup back over C:\ProgramData\ssh\sshd_config and restart sshd' `
        -WhatIf:$WhatIfPreference

    # Written whole rather than patched: an sshd_config assembled by appending
    # is hard to reason about, and the first matching directive wins.
    $pwshPath = (Get-Command pwsh.exe -ErrorAction SilentlyContinue)
    $defaultShell = if ($pwshPath) { $pwshPath.Source } else { "$env:SystemRoot\System32\WindowsPowerShell\v1.0\powershell.exe" }

    $sshdLines = @(
        '# Managed by Burrow setup (docs/03-SECURITY.md §5). Edits may be overwritten.',
        '',
        'Port 22',
        'AddressFamily any',
        '',
        '# Key-only. A password prompt over SSH is the one thing that makes a',
        '# brute force worth attempting, and both allowed devices hold keys.',
        'PubkeyAuthentication yes',
        'PasswordAuthentication no',
        'PermitEmptyPasswords no',
        'KbdInteractiveAuthentication no',
        'ChallengeResponseAuthentication no',
        'GSSAPIAuthentication no',
        '',
        "AllowUsers $me",
        'PermitRootLogin no',
        'MaxAuthTries 3',
        'MaxSessions 6',
        'LoginGraceTime 20',
        '',
        '# Nothing here needs to forward anything.',
        'AllowTcpForwarding no',
        'AllowAgentForwarding no',
        'X11Forwarding no',
        'PermitTunnel no',
        'GatewayPorts no',
        '',
        'ClientAliveInterval 60',
        'ClientAliveCountMax 3',
        '',
        '# Modern algorithms only.',
        'Ciphers chacha20-poly1305@openssh.com,aes256-gcm@openssh.com,aes128-gcm@openssh.com',
        'MACs hmac-sha2-512-etm@openssh.com,hmac-sha2-256-etm@openssh.com',
        'KexAlgorithms curve25519-sha256,curve25519-sha256@libssh.org,diffie-hellman-group16-sha512',
        'HostKeyAlgorithms ssh-ed25519,rsa-sha2-512,rsa-sha2-256',
        '',
        'SyslogFacility AUTH',
        'LogLevel INFO',
        '',
        'Subsystem sftp sftp-server.exe',
        '',
        '# Administrators are authenticated from this file, not from the user',
        '# profile; Windows OpenSSH ignores ~/.ssh/authorized_keys for them.',
        'Match Group administrators',
        '    AuthorizedKeysFile __PROGRAMDATA__/ssh/administrators_authorized_keys'
    )

    if ($PSCmdlet.ShouldProcess($sshdConfig, 'write hardened configuration')) {
        $null = New-Item -ItemType Directory -Force -Path (Split-Path $sshdConfig)
        Set-Content -Path $sshdConfig -Value ($sshdLines -join "`r`n") -Encoding utf8

        # Validated before the service is asked to use it: a bad config means
        # sshd refuses to start, and finding that out now is better.
        $sshd = Join-Path $env:SystemRoot 'System32\OpenSSH\sshd.exe'
        if (Test-Path $sshd) {
            & $sshd -t -f $sshdConfig
            if ($LASTEXITCODE -ne 0) { throw 'the generated sshd_config failed sshd -t' }
            Write-BurrowLog 'sshd_config validates' -Level Ok
        }
    }

    # The authorized_keys file, empty for now. Pairing fills it.
    $adminKeys = 'C:\ProgramData\ssh\administrators_authorized_keys'
    if ($PSCmdlet.ShouldProcess($adminKeys, 'create with a restrictive ACL')) {
        if (-not (Test-Path $adminKeys)) { $null = New-Item -ItemType File -Force -Path $adminKeys }
        # SYSTEM and Administrators only: sshd refuses to read this file if
        # anyone else can write it.
        & icacls $adminKeys /inheritance:r /q | Out-Null
        & icacls $adminKeys /grant:r 'SYSTEM:F' /q | Out-Null
        & icacls $adminKeys /grant:r 'Administrators:F' /q | Out-Null
        Write-BurrowLog 'administrators_authorized_keys created (empty until pairing)' -Level Ok
    }

    if ($PSCmdlet.ShouldProcess('OpenSSH', 'set the default shell')) {
        $null = New-Item -Path 'HKLM:\SOFTWARE\OpenSSH' -Force
        Set-ItemProperty -Path 'HKLM:\SOFTWARE\OpenSSH' -Name DefaultShell -Value $defaultShell
        Write-BurrowLog "ssh default shell: $defaultShell" -Level Ok
    }

    # The built-in rule opens 22 to the whole LAN; Burrow-SSH replaces it.
    $builtin = Get-NetFirewallRule -Name 'OpenSSH-Server-In-TCP' -ErrorAction SilentlyContinue
    if ($builtin -and $builtin.Enabled -eq 'True') {
        if ($PSCmdlet.ShouldProcess('OpenSSH-Server-In-TCP', 'disable the LAN-wide rule')) {
            Disable-NetFirewallRule -Name 'OpenSSH-Server-In-TCP'
            Register-BurrowChange -Tag 'firewall-openssh-builtin' -Was 'enabled' -Now 'disabled' `
                -Revert 'Enable-NetFirewallRule -Name OpenSSH-Server-In-TCP' -WhatIf:$WhatIfPreference
            Write-BurrowLog 'disabled the built-in LAN-wide SSH rule' -Level Ok
        }
    }

    if ($PSCmdlet.ShouldProcess('Burrow-SSH', 'add firewall rule')) {
        New-BurrowFirewallRule -Name 'Burrow-SSH' -Protocol TCP -LocalPort 22 `
            -RemoteAddress $bothIPs -Description 'Burrow: SSH from the iPhone and laptop over Tailscale only'
    }

    if ($PSCmdlet.ShouldProcess('sshd', 'set Automatic and start')) {
        Set-Service -Name sshd -StartupType Automatic
        Start-Service -Name sshd
        Write-BurrowLog 'sshd running' -Level Ok
    }
}

# -------------------------------------------------------------- 5.2 SMB ---

if (-not $SkipSMB) {
    Write-BurrowLog '5.2 SMB shares for the Files app and mapped drives' -Level Step

    if (-not $ShareRoots) {
        $roots = Get-BurrowStateValue -Phase '00-preflight' -Name 'findings'
        if ($roots -and $roots.fileRootCandidates) {
            $ShareRoots = @($roots.fileRootCandidates | ForEach-Object { $_.path })
        } else {
            $ShareRoots = @((Join-Path $env:USERPROFILE 'Documents'), (Join-Path $env:USERPROFILE 'Downloads'))
        }
    }

    # A dedicated low-privilege account, so the password stored on a phone is
    # not the owner's own (docs/02-ARCHITECTURE.md D6).
    $existing = Get-LocalUser -Name 'burrowfs' -ErrorAction SilentlyContinue
    if (-not $existing) {
        if ($PSCmdlet.ShouldProcess('burrowfs', 'create the file-share account')) {
            $pw = New-BurrowPassword -Length 32
            $secure = ConvertTo-SecureString $pw -AsPlainText -Force
            $null = New-LocalUser -Name 'burrowfs' -Password $secure `
                -FullName 'Burrow file share' `
                -Description 'Burrow file sharing only. Not for sign-in.' `
                -PasswordNeverExpires -UserMayNotChangePassword
            Write-BurrowLog 'created the burrowfs account' -Level Ok

            Write-BurrowSecretWindow -Title 'burrowfs password (Burrow Files)' -Secret $pw `
                -Instructions 'Save as "Burrow Files" in iOS Passwords, and in your laptop credential store. You will need it when connecting to smb://' `
                -WhatIf:$WhatIfPreference
            $pw = $null
            [GC]::Collect()
        }
    } else {
        Write-BurrowLog 'the burrowfs account already exists; leaving its password alone (use rotate-secrets.ps1 to change it)' -Level Ok
    }

    if ($PSCmdlet.ShouldProcess('burrowfs', 'deny every interactive logon right')) {
        # This account exists to hold a share password and nothing else. Denying
        # the logon rights means a leaked password cannot become a session.
        $inf = Join-Path $env:TEMP 'burrow-secpol.inf'
        $db = Join-Path $env:TEMP 'burrow-secpol.sdb'
        & secedit /export /cfg $inf /quiet

        $sid = (Get-LocalUser -Name 'burrowfs').SID.Value
        $content = Get-Content $inf
        $rights = @('SeDenyInteractiveLogonRight', 'SeDenyRemoteInteractiveLogonRight',
            'SeDenyBatchLogonRight', 'SeDenyServiceLogonRight')
        foreach ($right in $rights) {
            $line = $content | Where-Object { $_ -match "^$right\s*=" }
            if ($line) {
                if ($line -notmatch [regex]::Escape($sid)) {
                    $content = $content -replace [regex]::Escape($line), "$line,*$sid"
                }
            } else {
                $content = $content -replace '(?m)^\[Privilege Rights\]', "[Privilege Rights]`r`n$right = *$sid"
            }
        }
        Set-Content -Path $inf -Value $content -Encoding Unicode
        & secedit /configure /db $db /cfg $inf /areas USER_RIGHTS /quiet
        Remove-Item $inf, $db -Force -ErrorAction SilentlyContinue
        Write-BurrowLog 'burrowfs denied interactive, RDP, batch and service logon' -Level Ok
    }

    if ($PSCmdlet.ShouldProcess('burrowfs', 'hide from the sign-in screen')) {
        $key = 'HKLM:\SOFTWARE\Microsoft\Windows NT\CurrentVersion\Winlogon\SpecialAccounts\UserList'
        $null = New-Item -Path $key -Force
        Set-ItemProperty -Path $key -Name 'burrowfs' -Value 0 -Type DWord
        Register-BurrowChange -Tag 'burrowfs-hidden' -Was 'visible' -Now 'hidden' `
            -Revert "remove the burrowfs value from $key" -WhatIf:$WhatIfPreference
    }

    foreach ($path in $ShareRoots) {
        if (-not (Test-Path $path)) {
            Write-BurrowLog "skipping share for missing path $path" -Level Warn
            continue
        }
        $name = Split-Path $path -Leaf
        if ($PSCmdlet.ShouldProcess($path, "share as $name")) {
            Get-SmbShare -Name $name -ErrorAction SilentlyContinue | Remove-SmbShare -Force
            $null = New-SmbShare -Name $name -Path $path -EncryptData $true `
                -FolderEnumerationMode AccessBased -ChangeAccess 'burrowfs' `
                -Description "Burrow: $name"
            # NTFS rights on that folder only, never inherited upward.
            & icacls $path /grant "burrowfs:(OI)(CI)M" /q | Out-Null
            Write-BurrowLog "shared $name -> $path (encrypted, burrowfs modify)" -Level Ok
        }
    }

    if ($PSCmdlet.ShouldProcess('SMB server', 'require encryption and signing, disable SMB1')) {
        Set-SmbServerConfiguration -EnableSMB1Protocol $false -RequireSecuritySignature $true `
            -RejectUnencryptedAccess $true -Force
        Write-BurrowLog 'SMB1 off, signing required, unencrypted access rejected' -Level Ok
    }

    if ($PSCmdlet.ShouldProcess('Burrow-SMB', 'add firewall rule')) {
        New-BurrowFirewallRule -Name 'Burrow-SMB' -Protocol TCP -LocalPort 445 `
            -RemoteAddress $bothIPs -Description 'Burrow: SMB from the iPhone and laptop over Tailscale only'
    }

    $lanSmb = @(Get-NetFirewallRule -DisplayGroup 'File and Printer Sharing' -ErrorAction SilentlyContinue |
            Where-Object { $_.Enabled -eq 'True' -and $_.Direction -eq 'Inbound' })
    if ($lanSmb.Count -gt 0) {
        $ok = Confirm-BurrowChange -Yes:$Yes `
            -What "Disable $($lanSmb.Count) built-in File and Printer Sharing inbound rule(s)" `
            -Why 'These open SMB to your whole home network. Burrow does not need them. Say no if you share files with other machines on the LAN today.'
        if ($ok -and $PSCmdlet.ShouldProcess('File and Printer Sharing', 'disable LAN rules')) {
            $lanSmb | Disable-NetFirewallRule
            Register-BurrowChange -Tag 'firewall-smb-lan' -Was "$($lanSmb.Count) enabled" -Now 'disabled' `
                -Revert "Enable-NetFirewallRule -DisplayGroup 'File and Printer Sharing'" -WhatIf:$WhatIfPreference
            Write-BurrowLog 'LAN-wide SMB rules disabled' -Level Ok
        } else {
            Write-BurrowLog 'left the LAN SMB rules enabled at your request' -Level Warn
        }
    }
}

# ------------------------------------------------------------- 5.3 power ---

if (-not $SkipPower) {
    Write-BurrowLog '5.3 power settings' -Level Step

    # A sleeping PC cannot be woken by the plug: turning the plug on does
    # nothing to a machine that already has power (docs/01-PRD.md §7).
    $findings = Get-BurrowStateValue -Phase '00-preflight' -Name 'findings'
    $wasStandby = if ($findings) { $findings.standbyTimeoutAcMinutes } else { $null }
    $wasHibernate = if ($findings) { $findings.hibernateTimeoutAcMinutes } else { $null }

    if ($PSCmdlet.ShouldProcess('power plan', 'AC sleep and hibernate to Never')) {
        & powercfg /change standby-timeout-ac 0
        & powercfg /change hibernate-timeout-ac 0
        Register-BurrowChange -Tag 'standby-timeout-ac' -Was "$wasStandby" -Now '0' `
            -Revert "powercfg /change standby-timeout-ac $wasStandby" -WhatIf:$WhatIfPreference
        Register-BurrowChange -Tag 'hibernate-timeout-ac' -Was "$wasHibernate" -Now '0' `
            -Revert "powercfg /change hibernate-timeout-ac $wasHibernate" -WhatIf:$WhatIfPreference
        Write-BurrowLog 'AC sleep and hibernate set to Never (the display timeout is untouched)' -Level Ok
        Write-BurrowLog "Burrow's idle auto-shutdown covers power saving; it is off by default." -Level Info
    }

    if ($PSCmdlet.ShouldProcess('Fast Startup', 'disable')) {
        $powerKey = 'HKLM:\SYSTEM\CurrentControlSet\Control\Session Manager\Power'
        $was = (Get-ItemProperty $powerKey -Name HiberbootEnabled -ErrorAction SilentlyContinue).HiberbootEnabled
        Set-ItemProperty -Path $powerKey -Name HiberbootEnabled -Value 0 -Type DWord
        Register-BurrowChange -Tag 'fast-startup' -Was "$was" -Now '0' `
            -Revert "Set-ItemProperty '$powerKey' -Name HiberbootEnabled -Value $was" -WhatIf:$WhatIfPreference
        Write-BurrowLog 'Fast Startup off (a hybrid-shutdown PC does not cleanly power-cycle)' -Level Ok
    }

    # Wake-on-LAN readiness, so a Pi waker can be added later without coming
    # back to the PC. Unused by the smart-plug method.
    $findings = Get-BurrowStateValue -Phase '00-preflight' -Name 'findings'
    $nic = if ($findings) { $findings.adapterName } else { $null }
    if ($nic -and $PSCmdlet.ShouldProcess($nic, 'enable Wake on Magic Packet')) {
        try {
            Set-NetAdapterPowerManagement -Name $nic -WakeOnMagicPacket Enabled -ErrorAction Stop
            & powercfg /deviceenablewake "$((Get-NetAdapter -Name $nic).InterfaceDescription)" 2>$null
            Write-BurrowLog "Wake on Magic Packet enabled on $nic (for a future Pi waker)" -Level Ok
        } catch {
            Write-BurrowLog "this NIC does not expose Wake on Magic Packet: $($_.Exception.Message)" -Level Warn
        }
    }
}

# ---------------------------------------------------- 5.4 remote desktop ---

if ($RemoteDesktop -eq 'rdp') {
    Write-BurrowLog '5.4 Remote Desktop, laptop only' -Level Step

    $ok = Confirm-BurrowChange -Yes:$Yes `
        -What 'Enable Remote Desktop, reachable only from the laptop over Tailscale' `
        -Why 'RDP is a real remote-desktop service. It will be firewalled to one address, require Network Level Authentication, and the built-in LAN rules will be disabled.'
    if ($ok) {
        if ($PSCmdlet.ShouldProcess('RDP', 'enable with NLA and TLS')) {
            $tsKey = 'HKLM:\System\CurrentControlSet\Control\Terminal Server'
            $wasDeny = (Get-ItemProperty $tsKey -Name fDenyTSConnections).fDenyTSConnections
            Set-ItemProperty -Path $tsKey -Name fDenyTSConnections -Value 0
            Register-BurrowChange -Tag 'rdp-enabled' -Was "$wasDeny" -Now '0' `
                -Revert "Set-ItemProperty '$tsKey' -Name fDenyTSConnections -Value 1" -WhatIf:$WhatIfPreference

            $wsKey = "$tsKey\WinStations\RDP-Tcp"
            Set-ItemProperty -Path $wsKey -Name UserAuthentication -Value 1   # NLA
            Set-ItemProperty -Path $wsKey -Name SecurityLayer -Value 2        # TLS
            Write-BurrowLog 'RDP on, NLA required, TLS security layer' -Level Ok
        }

        if ($PSCmdlet.ShouldProcess('Remote Desktop firewall group', 'disable the LAN rules')) {
            Get-NetFirewallRule -DisplayGroup 'Remote Desktop' -ErrorAction SilentlyContinue |
                Disable-NetFirewallRule
            Register-BurrowChange -Tag 'firewall-rdp-builtin' -Was 'enabled' -Now 'disabled' `
                -Revert "Enable-NetFirewallRule -DisplayGroup 'Remote Desktop'" -WhatIf:$WhatIfPreference
        }

        if ($PSCmdlet.ShouldProcess('Burrow-RDP', 'add firewall rules')) {
            New-BurrowFirewallRule -Name 'Burrow-RDP-TCP' -Protocol TCP -LocalPort 3389 `
                -RemoteAddress @($laptopIP) -Description 'Burrow: RDP from the laptop only'
            New-BurrowFirewallRule -Name 'Burrow-RDP-UDP' -Protocol UDP -LocalPort 3389 `
                -RemoteAddress @($laptopIP) -Description 'Burrow: RDP from the laptop only'
        }

        $lockoutOk = Confirm-BurrowChange -Yes:$Yes `
            -What 'Set an account lockout policy: 10 failures locks the account for 15 minutes' `
            -Why 'This applies to every sign-in on this PC, not just RDP. It is the standard mitigation for a reachable logon service.'
        if ($lockoutOk -and $PSCmdlet.ShouldProcess('account lockout', 'configure')) {
            & net accounts /lockoutthreshold:10 /lockoutduration:15 /lockoutwindow:15 | Out-Null
            Register-BurrowChange -Tag 'account-lockout' -Was 'none' -Now '10/15/15' `
                -Revert 'net accounts /lockoutthreshold:0' -WhatIf:$WhatIfPreference
            Write-BurrowLog 'account lockout configured' -Level Ok
        }
    }
} elseif ($RemoteDesktop -eq 'sunshine') {
    Write-BurrowLog '5.4 Sunshine (this edition of Windows has no RDP host)' -Level Step
    Show-BurrowManualStep -Title 'Sunshine needs its current documentation checked before it is configured' -Steps @(
        'winget install --exact --id LizardByte.Sunshine',
        'Open Sunshine''s web UI at https://localhost:47990 and set an admin username and password.',
        'In Settings, bind the web UI to localhost only.',
        'Check https://docs.lizardby.te for the current streaming ports, then re-run this phase with -RemoteDesktop sunshine.',
        'This script will not guess the ports: a wrong guess either breaks streaming or opens a port that should be closed.'
    )
    Write-BurrowLog 'remove any firewall rules the Sunshine installer added, then add Burrow-Sunshine rules scoped to the laptop IP' -Level Warn
}

Set-BurrowStateValue -Phase '05-hardening' -Name 'remoteDesktop' -Value $RemoteDesktop -WhatIf:$WhatIfPreference
Set-BurrowStateValue -Phase '05-hardening' -Name 'shareRoots' -Value $ShareRoots -WhatIf:$WhatIfPreference

Write-Host ''
Write-Host '  Burrow firewall rules now in place:' -ForegroundColor White
Get-NetFirewallRule -DisplayName 'Burrow-*' -ErrorAction SilentlyContinue | ForEach-Object {
    $f = $_ | Get-NetFirewallPortFilter
    $a = $_ | Get-NetFirewallAddressFilter
    Write-Host ('    {0,-18} {1}/{2,-6} from {3}' -f $_.DisplayName, $f.Protocol, $f.LocalPort, ($a.RemoteAddress -join ', '))
}

if (-not $WhatIfPreference) { Set-BurrowPhaseComplete -Phase '05-hardening' }
