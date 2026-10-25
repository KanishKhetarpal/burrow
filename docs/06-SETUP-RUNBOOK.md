# 06 — Setup Runbook (Claude Code performs this on the PC)

**Claude Code does every step on the PC itself.** It writes each phase as an idempotent PowerShell script in `setup/`, runs it, verifies it, and records progress in `C:\ProgramData\Burrow\setup-state.json`. It stops only at:

- 🙋 **MANUAL**: something only you can physically do (iPhone taps, BIOS, a browser login, typing a password into an OS prompt). Claude Code gives exact, numbered instructions, then waits and verifies the result itself.
- ⚠️ **CONFIRM**: a change that's irreversible or could break something you already use. Claude Code explains the change in two lines and waits for "yes".

Everything else runs without asking.

---

## Before you start (owner, ~5 minutes)

1. Put this build pack in an empty folder, e.g. `C:\dev\burrow`.
2. Make sure Claude Code is installed and signed in with your **Max** account (`claude` → `/status` shows your claude.ai plan, not an API key).
3. Tailscale account: sign up at tailscale.com if you don't have one (use Google/GitHub/Apple login that has a passkey or 2FA on). Then **Admin console → Settings → Keys → Generate API access token**, expiry **1 day**. Copy it.
4. Open **Windows Terminal as Administrator** (Start → type "Terminal" → right-click → Run as administrator).
5. In that window, load the token without it landing in your shell history:
   ```powershell
   $s = Read-Host "Tailscale API token" -AsSecureString
   $env:TS_API_KEY = [Runtime.InteropServices.Marshal]::PtrToStringAuto([Runtime.InteropServices.Marshal]::SecureStringToBSTR($s))
   cd C:\dev\burrow
   claude
   ```
6. Paste the prompt from `BUILD_PROMPT.md`.

Keep your iPhone and laptop nearby. Total hands-on time for you: about 30–40 minutes spread over the run.

---

## Secret-handling pattern (Claude Code must follow)

Claude Code never sees, prints, or stores secret values. Any step that handles a secret runs in a **separate visible console window** launched with `Start-Process pwsh -ArgumentList '-NoProfile','-File',<script>` so its output never enters Claude Code's tool output. The script writes only a non-secret success marker into `setup-state.json`. Claude Code waits for the marker. Secrets covered:

| Secret | Handling |
|---|---|
| `TS_API_KEY` | Env var set by the owner. Scripts read `$env:TS_API_KEY`; never echo it. |
| Tailscale auth keys (tagged, single-use, 1 h) | Created and consumed inside one script run; passed via `--auth-key=file:<tmp>` (ACL'd temp file deleted immediately) or process env for `burrow enroll`. Never on a command line, never logged. |
| Windows password (service logon) | Typed by the owner into the Windows credential prompt shown by `burrow install`. Goes straight to the Service Control Manager. |
| `burrowfs` SMB password | Generated in a secret window, set on the account, shown once (large text + QR code) for you to save in iOS Passwords. Not written anywhere. |
| Pairing code | Shown only in the `burrow pair` window. |
| Tailnet Lock disablement secrets | Shown once in a secret window. |
| Sunshine admin credentials (Windows Home + remote desktop only) | Set in a secret window; you save them in your password manager. |
| Pi deploy SSH key (future waker only) | Generated into `%USERPROFILE%\.ssh\burrow_pi_deploy` (user-only ACL). Public half only is ever displayed. |

---

## Phase 0 — Preflight and decisions (automated + one question batch)

Checks (report as a table; fail fast on blockers):
- Running elevated; Windows edition/build (**Pro/Enterprise vs Home** decides the remote-desktop method in 5.4); `winget` present; PowerShell versions.
- Ethernet adapter: name, status Up, MAC, LAN IPv4, prefix, directed broadcast (recorded for a future Pi waker).
- Motherboard and BIOS vendor (`Win32_BaseBoard`, `Win32_BIOS`), UEFI vs legacy (for `shutdown /r /fw`), so Phase 10 can give the exact *Restore on AC Power Loss* menu path.
- Current power plan: AC sleep/hibernate timeouts (both must become *Never*, see 5.3).
- Router (informational only, for a future upgrade): gateway IP, MAC vendor (OUI), HTTP `<title>` at the gateway (read-only GET, 3 s timeout). Never log into the router.
- Claude Code: version (run `claude update` if behind), signed in with claude.ai subscription. Blockers if any of these are set anywhere (shell, user/machine env, `~/.claude/settings.json` `env`): `ANTHROPIC_API_KEY`, `ANTHROPIC_AUTH_TOKEN`, `ANTHROPIC_BASE_URL` (non-default), `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC`, `DISABLE_GROWTHBOOK`, `CLAUDE_CODE_USE_BEDROCK/VERTEX/FOUNDRY`.
- Git for Windows present (Claude Code needs Git Bash).
- `$env:TS_API_KEY` present and valid (`GET https://api.tailscale.com/api/v2/tailnet/-/devices` → 200). List existing tailnet devices (name, OS, tags) and whether a custom ACL policy already exists.
- your Windows account: local or Microsoft account; admin or not; whether password sign-in is possible (if "passwordless" Windows Hello-only is enforced, flag: the service needs a password logon).
- Current listening TCP ports and owning processes; RDP enabled?; built-in SMB/RDP firewall rules state; network profile of the Ethernet adapter; BitLocker status.

**Already decided (don't ask):** wake method = **smart plug** (`docs/01-PRD.md` §7). Allowed devices = **your iPhone + your laptop**.

Then ask the owner **one batch of questions** (AskUserQuestion):
1. **Laptop OS:** Windows / macOS. And **remote desktop from the laptop?** Yes (recommended) / No. Method is picked by the PC's edition in 5.4.
2. **Project folders** for "New Claude chat": Claude Code scans common places (`%USERPROFILE%\source\repos`, `%USERPROFILE%\code`, `%USERPROFILE%\Projects`, `C:\dev`, `D:\code`, `D:\dev`, any dir with `.git` two levels down) and proposes a list plus a **projects parent** folder.
3. **File roots** (app + SMB): default Desktop, Documents, Downloads, Pictures, plus the projects parent. Mark each read-only or read/write.
4. **Default Claude permission mode:** Default (recommended) / Accept edits / Plan.
5. If other tailnet devices exist: **keep your other personal devices able to reach each other?** (adds an `autogroup:self` rule; Burrow nodes stay iPhone+laptop-only either way).

Save answers to `setup-state.json`.

## Phase 1 — Toolchain (automated)

`winget install --exact --id <id> --accept-source-agreements --accept-package-agreements --silent` for any missing:
`GoLang.Go`, `OpenJS.NodeJS.LTS`, `Git.Git`, `Microsoft.PowerShell`, `Tailscale.Tailscale`.
Refresh `PATH` in-process. Print versions. Run `go install golang.org/x/vuln/cmd/govulncheck@latest`.

## Phase 2 — Build, test, install binaries (automated)

1. `scripts/test.ps1`: `go vet ./...`, `go test ./... -race` (where supported), `govulncheck ./...`, `npm ci && npm run build && npm audit --omit=dev --audit-level=high` in `web/`.
2. `scripts/build.ps1`: build PWA → embed → `go build -trimpath -ldflags "-s -w -X main.version=<git describe or 1.0.0>"` for `burrow.exe`; cross-compile `burrow-waker` for `linux/arm64`, `linux/arm` (GOARM=7), `android/arm64`.
3. Install `burrow.exe` to `C:\Program Files\Burrow\`. Create `C:\ProgramData\Burrow\{state,audit,logs,backup}` and set ACLs with `icacls` (inheritance off; SYSTEM + Administrators + the owner full; nothing else).

## Phase 3 — Tailnet configuration (automated, via Tailscale API)

Use `https://api.tailscale.com/api/v2` with `Authorization: Bearer $env:TS_API_KEY`. Check current Tailscale API docs for exact endpoints before calling; validate every policy before applying.

1. Read tailnet DNS settings. Turn on MagicDNS if off. HTTPS certificates: enable via API if supported; otherwise 🙋 **MANUAL:** Admin console → DNS → *Enable HTTPS* (one click). Read the tailnet DNS name (e.g. `tail1a2b3c.ts.net`).
2. Build the **bootstrap policy** from `policy/tailscale-acl.hujson`: `tagOwners` for `tag:pc`, `tag:burrow-agent`, `tag:burrow-waker`; no Burrow access rules yet; optional `autogroup:self` rule from Phase 0 Q5. If a custom policy exists, show the diff and ⚠️ **CONFIRM** before replacing. Validate, then apply.
3. Tailnet settings: device approval ON; device key expiry 180 days.
4. **PC Tailscale node:**
   - If the PC isn't on the tailnet: create a single-use, pre-authorized auth key tagged `tag:pc` (1 h expiry) and run, in the same script, `tailscale up --unattended --hostname=<n-random8> --auth-key=file:<tmp> --accept-routes=false --shields-up=false`, then delete the temp file.
   - If already logged in: set hostname with `tailscale set --hostname=<n-random8>`, apply tag `tag:pc` to the device via API, disable its key expiry via API, and ensure `--unattended` and `--shields-up=false`.
   - Record the PC's tailnet IP and MagicDNS name.

## Phase 4 — iPhone and laptop join the tailnet (🙋 MANUAL, ~5 min, then automated)

Claude Code shows these steps, then polls the API every 5 s for new devices:

**iPhone**
1. Install **Tailscale** from the App Store, open it, sign in with the **same** account.
2. In Tailscale → Settings → **VPN On Demand**: turn it on for Wi-Fi and cellular so it's always connected.
3. Install **Termius**, the **Claude** app (sign in with your Max account), and your **smart plug's app**.

**Laptop**
1. Install **Tailscale** (tailscale.com/download), sign in with the **same** account, and set it to start at login.
2. Install the **Claude Desktop app** or just use claude.ai/code in the browser for Remote Control sessions.

For each new device, Claude Code shows its name/OS and asks you to confirm it's yours (⚠️ CONFIRM), approves it via API if pending, and records `phone_ip`/`phone_node_id` and `laptop_ip`/`laptop_node_id`, plus your login. The laptop's key expiry stays on (re-auth every 180 days). Then it renders the **final policy** (both host aliases + all Burrow rules + tests), validates (`/acl/validate`), and applies.

## Phase 5 — Windows hardening and services (automated, with CONFIRMs noted)

Back up first: `netsh advfirewall export C:\ProgramData\Burrow\backup\firewall-<ts>.wfw`; copy `C:\ProgramData\ssh\sshd_config` if it exists.

**5.1 OpenSSH Server**
- `Add-WindowsCapability -Online -Name OpenSSH.Server~~~~0.0.1.0` if missing.
- Write `sshd_config` per `03-SECURITY.md` §5; `AllowUsers <windows-user>`; validate with `sshd -t`.
- Default shell: `HKLM:\SOFTWARE\OpenSSH` → `DefaultShell` = `C:\Program Files\PowerShell\7\pwsh.exe`.
- Create `C:\ProgramData\ssh\administrators_authorized_keys` (if your account is an administrator) or `%USERPROFILE%\.ssh\authorized_keys`, with correct ACLs (empty for now; filled at pairing).
- Disable the default `OpenSSH-Server-In-TCP` firewall rule; add `Burrow-SSH` (TCP 22, inbound, RemoteAddress = phone IP + laptop IP, InterfaceAlias = Tailscale, all profiles).
- `sshd` service: Automatic, start.

**5.2 SMB for the iOS Files app and the laptop**
- Create local user `burrowfs` in a **secret window** (password generated, shown once with QR; you save it in iOS Passwords as "Burrow Files"; the laptop uses the same account, saved in its credential store at Phase 9).
- Deny `burrowfs` interactive, RDP, batch and service logon (`secedit` export → edit → import); hide from the sign-in screen (`Winlogon\SpecialAccounts\UserList`).
- For each file root: `New-SmbShare -Name <RootName> -Path <path> -EncryptData $true -FolderEnumerationMode AccessBased -ChangeAccess|ReadAccess burrowfs`; NTFS grant `burrowfs` Modify or ReadAndExecute `(OI)(CI)` on that folder only. Never share a drive root or the whole profile.
- `Set-SmbServerConfiguration -EnableSMB1Protocol $false -RequireSecuritySignature $true -RejectUnencryptedAccess $true -Force`; disable the SMB1 optional feature if present (may need reboot; defer to Phase 10 reboot).
- Add `Burrow-SMB` firewall rule (TCP 445, phone IP + laptop IP, Tailscale interface).
- If built-in "File and Printer Sharing (SMB-In)" rules are enabled for the LAN: report it. ⚠️ **CONFIRM** before disabling (you may share files on the LAN today).

**5.3 Power settings (required for smart-plug wake) and Wake-on-LAN readiness (future Pi)**
- A sleeping or hibernating PC **can't** be woken by the plug, so set AC sleep and hibernate to *Never*: `powercfg /change standby-timeout-ac 0`, `powercfg /change hibernate-timeout-ac 0`. Display-off timeout stays as you have it. Record previous values for uninstall. Tell the owner in one line; Burrow's idle auto-shutdown (off by default) covers power saving.
- WoL (kept ready so a Pi can be added later without touching the PC again):
- `Set-NetAdapterPowerManagement -Name <eth> -WakeOnMagicPacket Enabled`.
- Set advanced properties that exist on this NIC: *Wake on Magic Packet* = Enabled, *Shutdown Wake-On-Lan / Shutdown Wake Up* = Enabled, *Wake on pattern match* = Disabled, *Energy Efficient Ethernet / Green Ethernet* = Disabled, *WOL & Shutdown Link Speed* = 10 Mbps First (Realtek).
- `powercfg /deviceenablewake "<NIC friendly name>"`.
- Turn off Fast Startup: `HKLM\SYSTEM\CurrentControlSet\Control\Session Manager\Power\HiberbootEnabled = 0`.
- Save MAC and broadcast to `setup-state.json`.

**5.4 Remote desktop for the laptop (only if the owner said yes in Phase 0)** — reachable only from the laptop's tailnet IP.
- **PC is Windows Pro/Enterprise → built-in Remote Desktop (RDP):**
  - Enable RDP (`fDenyTSConnections = 0`), require Network Level Authentication (`UserAuthentication = 1`), TLS security layer (`SecurityLayer = 2`).
  - Disable the built-in "Remote Desktop" firewall rule group; add `Burrow-RDP` (TCP + UDP 3389, RemoteAddress = laptop IP, InterfaceAlias = Tailscale).
  - Add only the owner's account to "Remote Desktop Users" (admins are allowed by default; no one else).
  - ⚠️ **CONFIRM** account lockout policy (`net accounts /lockoutthreshold:10 /lockoutduration:15 /lockoutwindow:15`); it applies to all sign-ins on the PC.
  - Laptop client: Windows → Remote Desktop Connection (`mstsc`) to `<pc-magicdns-name>`; macOS → Microsoft's **Windows App** from the App Store. Sign in with your Windows account (Microsoft account password, not PIN).
- **PC is Windows Home (no RDP host) → Sunshine + Moonlight** (open source, low latency):
  - `winget install --exact --id LizardByte.Sunshine`. Check current Sunshine docs for config keys and ports before applying.
  - Restrict Sunshine's web admin UI to localhost only; set its admin username/password in a **secret window**.
  - Remove any firewall rules Sunshine's installer added; add `Burrow-Sunshine` rules for its streaming ports (TCP and UDP per current docs) with RemoteAddress = laptop IP, InterfaceAlias = Tailscale. The web UI port is never opened.
  - Laptop client: **Moonlight** (Windows/macOS) → add PC by `<pc-magicdns-name>` → pair with the PIN shown in Moonlight, entered in Sunshine's local web UI on the PC (Phase 9).
- Update the Tailscale policy with the matching laptop → `tag:pc` ports (RDP or Sunshine) and extend the policy tests.

## Phase 6 — Claude Code configuration (automated + 🙋 ~3 min)

1. Back up and merge `%USERPROFILE%\.claude\settings.json`: `"remoteControlAtStartup": true` (every interactive Claude session, including ones started over SSH, shows up in the Claude app).
2. Folder trust: check current Claude Code docs for a supported way to pre-trust the configured project folders; use it if one exists. Otherwise the trust prompt will appear in Burrow's terminal and the app shows a **Trust folder** button.
3. 🙋 **MANUAL (one time):** in a normal terminal, `cd` into one project and run `claude remote-control`. Answer `y` to *Enable Remote Control?* (and trust the folder if asked). Press space, scan the QR with the iPhone, confirm the session opens in the Claude app, then Ctrl+C.
4. 🙋 **MANUAL:** in that session or a new `claude`, run `/config` and turn on **Push when actions required** and **Push when Claude decides**.
5. 🙋 **MANUAL:** claude.ai → Settings → Account → turn on **Require trusted devices**; enroll the iPhone (Claude app) and the laptop (browser or Claude Desktop) when prompted.

## Phase 7 — Burrow agent service (automated + 🙋 password prompt)

1. Write `C:\ProgramData\Burrow\burrow.yaml` from Phase 0 answers and recorded values (random agent hostname `n-<8>`, owner login, allowed device list left empty until pairing, roots, projects, smart-plug wake settings).
2. Create a single-use pre-authorized auth key tagged `tag:burrow-agent` and, in the same script, run `burrow enroll` (key via process env only). This registers the tsnet node and writes its state under `C:\ProgramData\Burrow\state\tsnet`. Record the agent FQDN from `CertDomains()` into config.
3. Grant the owner's account **Log on as a service** (`SeServiceLogonRight`).
4. 🙋 **MANUAL (secret window):** `burrow install` shows the Windows credential prompt. Type your **Windows password** (for a Microsoft account, your Microsoft account password, not your PIN). This creates service **Burrow** running as your account: Automatic (Delayed Start), restart on failure (10 s / 30 s / 60 s).
5. Start the service; wait for `burrow status` = healthy (tsnet up, TLS cert issued, listening). First cert issuance can take ~1 min.
6. `burrow selftest`: child process runs with Administrators deny-only (`whoami /groups`), `claude --version` and Remote Control eligibility pass from the service context, identity gate denies a non-allowlisted node (self-dial returns 404), security headers present, audit chain verifies.

## Phase 8 — Smart plug wake (🙋 ~10 min, then automated)

**Wiring (owner):** wall socket → (UPS/inverter output, if you use one) → **smart plug** → PC power cable. The plug must be the **last** thing before the PC. If it sits before a UPS, the UPS keeps the PC powered and wake won't work.

**Plug app (owner, on the iPhone):**
1. Add the plug in its app on 2.4 GHz Wi-Fi and confirm you can switch it **from mobile data** (remote control works).
2. Turn on **2FA** for the plug app account.
3. If the app has a *power-on state after outage* setting, set it to **Last state** (or Off).
4. Name the device **"PC Plug"**.

**iPhone Shortcuts (owner)** — names must match exactly:
| Shortcut | Actions |
|---|---|
| **PC On** | plug app action: turn **PC Plug** on |
| **PC Off** | plug app action: turn **PC Plug** off |
| **PC Power Cycle** | turn **PC Plug** off → *Wait* 15 seconds → turn **PC Plug** on |

(Tapo plugs offer these actions directly in Shortcuts. For other brands, create the on/off scenes in the plug app, tap **Add to Siri**, then use them in Shortcuts.)

**Claude Code (automated):** write `wake.method: smartplug` and the three shortcut names into `burrow.yaml`, restart Burrow, confirm `/api/wake-config` returns them. BIOS *Restore on AC Power Loss* is set in Phase 10.

**Usage rule built into the app:** Wake only ever turns power **on**. Cutting power is offered only after a confirmed Burrow shutdown, and Power-cycle sits behind a warning.

## Phase 9 — Pair the iPhone and the laptop (🙋 ~10 min)

Claude Code runs `burrow pair` in a visible window for **each** device (QR code + URL `https://<agent-fqdn>/pair` + 6-digit code, 10-minute window, single use).

**iPhone**
1. Termius: Keychain → **+** → Generate Key → **ED25519** → name "Burrow" → copy the public key. Settings → turn on **app lock with Face ID**.
2. Scan the QR (Camera) → Safari → enter the code → **Create passkey** (Face ID) → paste the Termius public key → finish.
3. Safari **Share → Add to Home Screen** → open Burrow from the Home Screen, unlock with Face ID.
4. **Files** app → ⋯ → **Connect to Server** → `smb://<pc-magicdns-name>` → Registered User → `burrowfs` + saved password. Your roots appear as shares.
5. Termius: New Host → `<pc-magicdns-name>`, username `<windows username>`, key "Burrow" → connect once, accept the host key.

**Laptop** (Claude Code writes `laptop/setup-windows.ps1` and `laptop/setup-macos.sh` and copies them into a `Burrow-Laptop` folder in the Downloads root, so you can download the right one from Burrow → Files → Downloads after pairing)
1. In the laptop browser open `https://<agent-fqdn>/pair` → enter the code → **Create passkey** (Windows Hello or Touch ID). Install it as an app (Chrome/Edge: *Install Burrow*; Safari on macOS: *File → Add to Dock*).
2. Run the laptop script. It:
   - checks Tailscale is connected and the PC answers,
   - generates `~/.ssh/burrow_ed25519` **with a passphrase** (macOS: stored in Keychain; Windows: `ssh-agent` service enabled),
   - adds a `Host burrow-pc` entry to `~/.ssh/config` (HostName `<pc-magicdns-name>`, User `<windows username>`, IdentityFile, `IdentitiesOnly yes`),
   - prints the **public** key for you to add in Burrow → More → Security → **Add SSH key** (Face ID/Windows Hello step-up),
   - maps the shares (Windows: persistent drive letters with credentials prompted by Windows; macOS: opens `smb://<pc-magicdns-name>` in Finder, save to Keychain),
   - points you to the remote-desktop client from 5.4 (Remote Desktop/Windows App, or Moonlight).
3. Test: `ssh burrow-pc` works; VS Code **Remote-SSH** to `burrow-pc` works (great for coding on the PC from the laptop).
4. Remote desktop: connect once. For Sunshine, pair Moonlight with its PIN (Claude Code opens Sunshine's local web UI on the PC for you to type the PIN).
5. Claude from the laptop: open claude.ai/code or Claude Desktop → your Burrow sessions are listed (Remote Control). Burrow's **New Claude chat** works from the laptop too.

**Wake from the laptop:** the plug is controlled from the iPhone. Burrow on the laptop shows "Wake from your iPhone (PC On shortcut, or 'Hey Siri, PC On')". If you have a Mac and the plug's Shortcuts action runs there, the same shortcut works on the Mac.

Claude Code verifies: audit shows two `auth.pair` events and the SSH key additions; `burrow devices list` shows the iPhone and the laptop with their node IDs.

## Phase 10 — BIOS and the real wake test (🙋, needs reboots)

1. Claude Code prints the exact BIOS path for the detected board. The setting that matters is **Restore on AC Power Loss = Power On** (names vary by vendor and BIOS version; Claude Code adapts to the detected board):
   - **ASUS:** Advanced → APM Configuration → *Restore AC Power Loss* = **Power On**
   - **MSI:** Settings → Advanced → Power Management Setup → *Restore after AC Power Loss* = **Power On**
   - **Gigabyte:** Settings → Platform Power → *AC BACK* = **Always On**
   - **ASRock:** Advanced → Chipset Configuration → *Restore on AC/Power Loss* = **Power On**
   - **Dell:** Power Management → *AC Recovery* = **Power On**
   - **HP:** Advanced → Power-On Options → *After Power Loss* = **Power On**
   - **Lenovo:** Power → *After Power Loss* = **Power On**
   - Optional while you're there (for a future Pi): Wake on LAN / Power On By PCI-E = Enabled; ErP / Deep Sleep = Disabled.
   - Do **not** pick "Last State": after a Burrow shutdown the last state is "off", so the PC wouldn't boot.
2. Claude Code saves progress and tells you how to resume: after the reboot, open Terminal as Administrator, reload `TS_API_KEY` if Phase 11 isn't done, `cd C:\dev\burrow`, `claude --continue`, and say **"continue setup"**.
3. ⚠️ **CONFIRM**, then `shutdown /r /fw /t 5` (UEFI) to reboot straight into firmware settings. Legacy BIOS: press Del/F2 at boot.
4. 🙋 Wake test (after BIOS is saved and Windows has booted once):
   1. iPhone → Burrow → More → Power → **Shut down** (Face ID).
   2. Wait for "PC is off. Cut plug power?" → **Turn plug off** (runs PC Off).
   3. Wait 15 s → Home → **Wake PC** (runs PC On).
   4. Burrow reaches **Online** within 3 minutes without anyone logging into Windows.
   5. Repeat once with the plug **left on** after shutdown to check the **Power-cycle** path appears after 90 s and works.
5. 🙋 Function test from the **iPhone**: New Claude chat → Open in Claude → send "list files here"; Files → open a photo; Open terminal → Termius connects.
6. 🙋 Function test from the **laptop**: Burrow web app unlock (Windows Hello/Touch ID); `ssh burrow-pc`; open a mapped share; remote desktop session (if set up); New Claude chat → continue it in claude.ai/code.
7. Resume Claude Code; it re-runs `burrow selftest` and continues.

## Phase 11 — Lock it down (each ⚠️ CONFIRM)

- **Tailnet Lock** (recommended): follow current Tailscale docs; PC + iPhone (and the laptop, optional) as signing nodes; sign the agent node (and a waker node if added later); disablement secrets shown once in a secret window for iOS Passwords. Skip if you decline.
- **RDP**: if remote desktop uses RDP (5.4), confirm the built-in RDP rules stay disabled (only `Burrow-RDP` allowed); if it doesn't, make sure RDP is off.
- **LAN SMB rules**: disable built-in LAN file sharing rules if the owner doesn't need them.
- **BitLocker**: recommend turning on (not automated).
- 🙋 **Router:** check UPnP is **off** and there are **no port forwards** to the PC. Optional DHCP reservation for the PC.
- 🙋 **Revoke the Tailscale API token** (Admin console → Settings → Keys → Revoke). Claude Code removes `$env:TS_API_KEY` from its process environment.

## Phase 12 — Verify and report (automated + 2 min manual)

`setup/12-verify.ps1` prints PASS/FAIL for:
- Burrow process owns no OS-level listening sockets (only the admin named pipe).
- For every OS listening port: no enabled inbound allow rule with RemoteAddress *Any* (except loopback-only services). 22 and 445 allowed only from the iPhone and laptop IPs on the Tailscale interface; 3389 or Sunshine ports (if set up) only from the laptop IP.
- `sshd -T`: password auth off, AllowUsers correct.
- SMB1 off, encryption required, signing required, shares scoped correctly, `burrowfs` logon rights denied.
- Tailscale: policy validates and tests pass, device approval on, no Funnel attr, PC and agent tagged; the iPhone and laptop are the only allowed sources.
- Burrow service runs as the owner's account; child token restricted; audit chain verifies; selftest passes.
- AC sleep/hibernate = Never; Fast Startup off; WoL settings applied (future-ready).
- Burrow `/api/wake-config` returns `smartplug` with the three shortcut names.

🙋 Negative tests (on home Wi-Fi):
1. iPhone: turn **Tailscale off** → Burrow shows Offline; Files → `smb://<PC LAN IP>` fails; Termius to the LAN IP fails. Turn it back on → everything works.
2. Laptop: quit Tailscale → `ssh <user>@<PC LAN IP>`, `\\<PC LAN IP>\Documents` and remote desktop to the LAN IP all fail. Start Tailscale → everything works.

Write `SETUP-REPORT.md` (no secrets): what was installed/changed, PASS/FAIL table, where backups are, how to uninstall.

---

## Lost or stolen iPhone or laptop

See `03-SECURITY.md` §7. Short version: **login.tailscale.com → remove that device** (cuts everything from it instantly; the other device keeps working), then **claude.ai → Trusted devices → remove it**, then at the PC `burrow devices revoke <device>` and `setup\rotate-secrets.ps1` (new `burrowfs` password; remove that device's SSH key; re-pair Moonlight if it was the laptop).

## Uninstall

`setup\uninstall.ps1` (⚠️ CONFIRM): stops and deletes the Burrow service, removes `Burrow-*` firewall rules, restores firewall/sshd backups, removes the shares and `burrowfs`, reverts `remoteControlAtStartup`, the power plan, and RDP/Sunshine changes, optionally removes Burrow nodes from the tailnet via API, deletes `C:\ProgramData\Burrow`.

## Script conventions (for Claude Code)

- One script per phase: `setup/NN-<name>.ps1`, shared helpers in `setup/lib/Burrow.Setup.psm1`.
- Each script: `#Requires -RunAsAdministrator`, `Set-StrictMode -Version Latest`, `$ErrorActionPreference = 'Stop'`, supports `-WhatIf`, is idempotent (check → plan → apply → verify), logs to `C:\ProgramData\Burrow\logs\setup-<date>.log` with a secret scrubber, and ends by writing one JSON summary line to stdout for Claude Code to read.
- Every system change is recorded in `setup-state.json` with what to revert.
- If a step fails, stop that phase, show the error and the fix, and don't continue to dependent phases.

## Appendix A — Future upgrade: Raspberry Pi waker (not part of v1)

Switch to this later for wake without cutting power (Wake-on-LAN). The PC side is already prepared in Phase 5.3.

1. Generate the deploy key (`ssh-keygen -t ed25519 -f %USERPROFILE%\.ssh\burrow_pi_deploy -N ""`, user-only ACL). Show the **public** key.
2. 🙋 **MANUAL (~10 min):** Raspberry Pi Imager → Raspberry Pi OS **Lite (64-bit)** → Edit settings: hostname = the random `n-<8>` Claude Code gives you; username of your choice; Wi-Fi (Pi Zero 2 W) or plug in Ethernet (Pi 4/5); locale; Services → **Enable SSH → Allow public-key authentication only** → paste the public key. Write the card, insert, power on.
3. Automated: find the Pi (`<hostname>.local`, else scan ARP for Raspberry Pi OUIs and confirm with the owner), SSH in with the deploy key, then:
   - `apt-get update && apt-get -y full-upgrade && apt-get -y install unattended-upgrades ufw` and enable unattended upgrades.
   - `ufw default deny incoming; ufw allow from <pc-lan-ip> to any port 22 proto tcp; ufw enable`.
   - sshd: `PasswordAuthentication no`, `AllowUsers <user>@<pc-lan-ip>`.
   - Copy the matching `burrow-waker` build (`uname -m`), write `/etc/burrow-waker/waker.yaml` (PC MAC, broadcast, PC LAN IP, agent URL, phone node ID, CORS origin), enroll with a single-use `tag:burrow-waker` key sent over SSH **stdin**, install the hardened systemd unit, enable and start.
4. Put the waker FQDN into `burrow.yaml` (`wake.waker_url`); restart Burrow; verify `GET /api/status` from the agent's perspective returns `online`.
5. Set `wake.method: waker`, add the waker origin to the CSP `connect-src`, and restart Burrow.
