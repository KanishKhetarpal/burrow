# 02 — Architecture

## 1. Overview

```
                         (internet — nothing inbound, ever)
                                        │
   iPhone + laptop ─ Tailscale (WireGuard, end-to-end encrypted, ACL: 2 devices) ─────┐
   │  • Burrow PWA (home screen)                                                     │
   │  • Termius (SSH)                                                                │
   │  • Files app / mapped drives (SMB)    • laptop: SSH, VS Code, RDP or Moonlight  │
   │  • Claude app ──── HTTPS ──── Anthropic Remote Control ──── HTTPS (outbound) ─┐ │
   │  • Shortcuts "PC On"/"PC Off" ── plug vendor cloud ──┐                        │ │
   │                                                       │                        │ │
   │                       HOME LAN (router, no port forwards, UPnP off)            │ │
   │   ┌──────────────────────────────┐        ┌──────────────────────────────────▼─▼──┐
   │   │ Wi-Fi smart plug             │  230V  │ Windows PC  (BIOS: AC restore = On)    │
   │   │ wall/UPS → plug → PC         │──────►│  ├─ tailscaled        (tag:pc)  :22 :445 │
   │   └──────────────────────────────┘        │  ├─ burrow agent      (tag:burrow-agent) │
   └──────────────────────────────────────────►│  │   Windows service, tsnet node, :443 │
                                               │  │   ├─ PWA + REST + WebSocket          │
                                               │  │   ├─ Session manager (ConPTY)        │
                                               │  │   │    └─ claude --remote-control …  │
                                               │  │   ├─ File service                    │
                                               │  │   ├─ Power / keep-awake              │
                                               │  │   └─ Auth (WhoIs + passkey) + audit  │
                                               │  ├─ OpenSSH Server (key-only)    → :22  │
                                               │  ├─ SMB share(s) (burrowfs user)  → :445│
                                               │  └─ RDP or Sunshine (optional, laptop) │
                                               └────────────────────────────────────────┘
```

Key ideas:

1. **Tailscale is the only door.** The agent and waker embed Tailscale (`tsnet`), so they listen on the tailnet only. They do not bind to any Windows or Linux network interface, so the LAN can't reach them either.
2. **Claude runs locally; the phone steers it through Anthropic's Remote Control.** Burrow doesn't rebuild a Claude UI. It launches Claude Code processes with Remote Control on and hands the phone over to the Claude app. The raw terminal is also viewable in Burrow as a fallback.
3. **Defense in depth.** Network ACL (iPhone + laptop only) → app-level device allowlist via Tailscale WhoIs → passkey (Face ID / Windows Hello / Touch ID) → step-up auth for dangerous actions → audit log.

## 2. Components

### 2.1 `burrow` (Windows agent + CLI), Go, single binary

| Module | Responsibility |
|---|---|
| `internal/tsnode` | Starts `tsnet.Server` (random hostname, state dir under `C:\ProgramData\Burrow\state\tsnet`), `ListenTLS("tcp", ":443")`, exposes `WhoIs`. Logs upload disabled (`TS_NO_LOGS_NO_SUPPORT=true`). |
| `internal/auth` | Identity middleware (WhoIs → node StableID + user login allowlist), WebAuthn passkeys (`github.com/go-webauthn/webauthn`), sessions, step-up, pairing window, rate limiting, lockdown state. |
| `internal/sessions` | Claude session lifecycle: spawn in ConPTY, track state, scrollback ring buffer (2 MB/session, memory only), prompt detection, Remote Control URL detection, watchdog for server mode, resume. |
| `internal/pty` | Windows ConPTY wrapper (`golang.org/x/sys/windows` CreatePseudoConsole) + `CreateProcessAsUser` with a **restricted token** (see §5). |
| `internal/files` | Root-scoped file ops, traversal/reparse-point protection, denylist, streaming upload/download, preview, Recycle Bin delete. |
| `internal/power` | Sleep/hibernate/shutdown/restart/lock, `SetThreadExecutionState` keep-awake, idle auto-shutdown (no sessions + no app/SSH/SMB activity for N min; off by default). |
| `internal/system` | CPU/RAM/disk/uptime, Tailscale status, version. |
| `internal/audit` | Append-only JSONL with SHA-256 hash chain; verify command. |
| `internal/store` | SQLite (`modernc.org/sqlite`, CGO-free): credentials, devices, sessions, settings. |
| `internal/config` | Loads/validates `C:\ProgramData\Burrow\burrow.yaml`. |
| `internal/web` | HTTP router, security headers, CSP, embedded PWA (`go:embed web/dist`). |
| `internal/svc` | Windows service wrapper (`golang.org/x/sys/windows/svc`), install/uninstall helpers. |
| `cmd/burrow` | Subcommands: `run` (service entry), `install`, `uninstall`, `pair`, `unlock`, `status`, `audit verify`, `devices list|revoke`, `selftest`, `attach` (P1). |

### 2.2 `burrow-waker`, Go, single binary (cross-compiled) — built and tested, **not deployed in v1**

v1 wakes via smart plug (see §3.1). The waker is still built so a Raspberry Pi can be added later by switching `wake.method` to `waker`.

Targets: `linux/arm64`, `linux/arm` (Pi), `android/arm64` (Termux), `linux/amd64` (testing).

- `tsnet` node with random hostname, tag `tag:burrow-waker`, `:443` TLS.
- `POST /api/wake` sends a magic packet (6×`0xFF` + 16×MAC) to UDP 9 and 7, to the LAN directed broadcast and `255.255.255.255`, 3 times, 200 ms apart.
- `GET /api/status` returns PC state: `online` (agent `/healthz` over tailnet OK), `booting` (LAN TCP probe responds but agent not up), `offline`.
- `GET /` serves a tiny fallback page (Wake button + status) for use without the PWA.
- Auth: WhoIs allowlist (phone node only, plus nothing else), rate limit (1 wake / 30 s, 20/day), audit log.
- CORS: allow exactly the agent's HTTPS origin; methods GET/POST; no credentials.
- Config: `/etc/burrow-waker/waker.yaml` (or `$HOME/.burrow-waker/` on Termux).
- systemd unit with hardening (`DynamicUser=yes`, `NoNewPrivileges=yes`, `ProtectSystem=strict`, `ProtectHome=yes`, `PrivateTmp=yes`, `StateDirectory=burrow-waker`, `RestrictAddressFamilies=AF_INET AF_INET6 AF_NETLINK AF_UNIX`, `Restart=always`).

### 2.3 PWA (`web/`), TypeScript + Vite + Preact

- Mobile-first, iPhone safe areas, light/dark; responsive wide layout for the laptop browser (installable as a desktop PWA in Chrome/Edge/Safari).
- `@simplewebauthn/browser` for passkeys, `@xterm/xterm` + `@xterm/addon-fit` for terminals.
- Hand-written service worker: cache the app shell and `/api/wake-config`; **network-first with a 4 s timeout** for navigations, falling back to the cached shell. This lets the app open and show **Wake** while the PC (and so the agent) is off.
- No third-party CDNs; everything bundled and served by the agent (strict CSP).

### 2.4 OS-level services on the PC (configured by setup, not code)

- **Tailscale client** (`tailscaled`, tagged `tag:pc`, unattended mode) for SSH and SMB reachability.
- **OpenSSH Server**: key-only, `AllowUsers <windows-user>`, firewall-scoped to the iPhone's and laptop's tailnet IPs.
- **SMB share(s)** for the iOS Files app and laptop drive mappings: dedicated local account `burrowfs`, encryption required, firewall-scoped to the iPhone's and laptop's tailnet IPs.
- **Remote desktop (optional, laptop only):** RDP with NLA on Windows Pro/Enterprise, or Sunshine (host) + Moonlight (laptop client) on Windows Home; firewall-scoped to the laptop's tailnet IP.
- **Power plan:** AC sleep/hibernate = Never (a sleeping PC can't be woken by the plug).

## 3. Key flows

### 3.1 Wake

**v1 method: `smartplug`.** BIOS *Restore on AC Power Loss = Power On* boots the PC whenever plug power comes back.

1. PWA opens (from cache if the PC is off). It calls agent `/healthz` with a 4 s timeout. On failure it shows **Offline**.
2. User taps **Wake** → PWA opens `shortcuts://run-shortcut?name=PC%20On` (plug **on** only; harmless if the PC is already running). PWA records `wakeRequestedAt` in local storage.
3. User returns to Burrow; PWA polls agent `/healthz` every 3 s for up to 3 minutes: `offline → waking → online`.
4. If not online after 90 s → PWA shows **Power-cycle** (likely cause: the plug was left on after shutdown, so no AC-restore event happened). Tapping it shows a warning ("Only if the PC is fully shut down") + confirm, then opens `shortcuts://run-shortcut?name=PC%20Power%20Cycle` (off → wait 15 s → on).
5. On `online`, PWA reloads from the agent and asks for Face ID unlock.

**Safe shutdown → plug off:**
1. User taps Shut down (step-up). Agent acknowledges with `scheduledAt` and the PWA stores `shutdownRequestedAt`.
2. PWA polls `/healthz` until it fails, then keeps polling for 30 s more (must fail continuously).
3. Only then does it show **"PC is off — cut plug power?"** → opens `shortcuts://run-shortcut?name=PC%20Off`.
4. If the agent goes unreachable **without** a Burrow shutdown request (network blip, crash), the PWA never suggests cutting power.

Other methods (future): `waker` → PWA `POST https://<waker-fqdn>/api/wake` (CORS) and polls waker `/api/status`; `router` → instructions only.

Boot timeline on the PC: plug on → BIOS AC restore → Windows boots to the lock screen (no user login needed) → `tailscaled` service up → `Burrow` service (runs as your account) starts → tsnet node up → `/healthz` OK.

### 3.2 New Claude chat

1. User picks a project, name (default `<project>-<HHmm>`), mode, permission mode.
2. Agent validates the project path (must be in `projects` config), checks `max_sessions`.
3. Agent spawns in a ConPTY with a restricted token, cwd = project path:
   - **Chat mode (default):** `claude --remote-control "<name>"` (+ `--permission-mode <m>` if not default). Interactive mode reconnects by itself after network outages.
   - **Project server mode:** `claude remote-control --name "<name>" --spawn <same-dir|worktree> --capacity <n>` for starting more chats from inside the Claude app. A watchdog restarts it with backoff (server mode exits after about 10 min offline).
4. Agent watches PTY output (ANSI stripped):
   - Detects `https://claude.ai/code/...` → stores `remote_url`, status `ready`.
   - Detects known prompts (`Trust …? [y/N]`, `Enable Remote Control? (y/n)`, permission prompts) → status `needs_input`, surfaced as quick-reply buttons.
5. PWA shows **Open in Claude** (universal link `remote_url`, which opens the Claude app) and **View terminal**. If no URL within 30 s, it shows "Open Claude app → Code tab" (session is listed by name).
6. Keep-awake turns on while ≥1 session is running.

### 3.3 Files

- **In-app:** REST endpoints scoped to configured roots (see `04-API.md`).
- **Native:** iOS Files → Connect to Server → `smb://<pc-magicdns-name>` → user `burrowfs`.
- **Termius SFTP** also works over the SSH server.

### 3.4 Pairing (one time per device: iPhone, then laptop)

1. On the PC (admin terminal) run `burrow pair`. This opens a 10-minute window and prints a QR code for `https://<agent-fqdn>/pair` plus a 6-digit code (shown only on the PC screen).
2. Phone (on tailnet; ACL already limits to the phone's IP) opens the URL, enters the code (5 tries max), registers a passkey (Face ID, resident key, user verification required).
3. Agent stores the device's Tailscale **node StableID** and the owner login. From now on, every request must come from one of the paired nodes (max 2 + 1 spare slot).
4. Optional in the same flow: paste an SSH public key (Termius on the iPhone, `~/.ssh/burrow_ed25519.pub` on the laptop), which is appended to the right `authorized_keys` file.
5. The window closes after success or timeout.

## 4. Data and file locations

| Path | Contents | ACL |
|---|---|---|
| `C:\Program Files\Burrow\burrow.exe` | Binary | Default Program Files |
| `C:\ProgramData\Burrow\burrow.yaml` | Config | SYSTEM, Administrators, owner: full; nobody else |
| `C:\ProgramData\Burrow\state\` | tsnet state, SQLite DB | SYSTEM + owner only |
| `C:\ProgramData\Burrow\audit\audit.jsonl` | Audit log | SYSTEM + owner only |
| `C:\ProgramData\Burrow\logs\` | Rotating logs (10 × 10 MB) | SYSTEM + owner only |
| `C:\ProgramData\Burrow\backup\` | Pre-change backups from setup (sshd_config, firewall export) | Administrators |

PTY scrollback is memory-only. Logs never contain PTY content, cookies, tokens, or passkey material.

## 5. Process model and privileges

- **Burrow service** runs as your Windows account (so Claude Code uses your login, `~/.claude`, git config, and projects) with start type *Automatic (Delayed)* and failure recovery *restart after 10 s / 30 s / 60 s*.
- your account is probably an administrator. A service logged on as an admin gets a **full admin token with no UAC prompt**. To avoid handing that to Claude sessions, the agent launches every child process (Claude, shells) with a **restricted token**: `CreateRestrictedToken` with `DISABLE_MAX_PRIVILEGE`, `BUILTIN\Administrators` set to deny-only, and integrity level set to Medium. Acceptance test: `whoami /groups` inside a session shows Administrators as "Group used for deny only".
- Child processes get a proper user environment (`CreateEnvironmentBlock`), `PATH` including Git for Windows, and `CLAUDE_CODE_GIT_BASH_PATH` if needed.
- Children are placed in a Job Object so stopping a session kills its whole process tree.
- Session 0 has no desktop: Claude must already be logged in (setup verifies), and browser-opening features (`--chrome`, OAuth popups) won't work in Burrow-launched sessions. Documented limitation.

## 6. Tech stack (pinned at build time to latest stable)

| Area | Choice | Why |
|---|---|---|
| Agent + waker | Go (latest stable) | Single static binaries, easy cross-compile, first-class `tsnet`, Windows service + ConPTY via `x/sys/windows`. |
| Tailnet embedding | `tailscale.com/tsnet` | Listens only on the tailnet; built-in TLS certs and WhoIs. |
| Passkeys | `github.com/go-webauthn/webauthn` | Maintained, spec-complete. |
| DB | `modernc.org/sqlite` | No CGO. |
| WebSocket | `github.com/coder/websocket` | Maintained successor of nhooyr. |
| Config | `gopkg.in/yaml.v3` | |
| Terminal QR | `github.com/mdp/qrterminal/v3` | For `burrow pair`. |
| Frontend | Vite + Preact + TypeScript | Tiny bundle, fast on phone. |
| Terminal UI | `@xterm/xterm`, `@xterm/addon-fit` | |
| WebAuthn client | `@simplewebauthn/browser` | |

## 7. Repository layout

```
burrow/
├── CLAUDE.md, README.md, BUILD_PROMPT.md
├── docs/                      # this spec
├── policy/tailscale-acl.hujson
├── config/burrow.example.yaml
├── go.mod
├── cmd/
│   ├── burrow/                # agent + CLI
│   └── burrow-waker/
├── internal/                  # modules from §2.1 (+ shared: wol, identity)
├── web/                       # PWA source → web/dist embedded by internal/web
├── laptop/                    # setup-windows.ps1, setup-macos.sh (run on the laptop)
├── setup/
│   ├── lib/Burrow.Setup.psm1  # shared helpers: logging, state, backup, confirm
│   ├── 00-preflight.ps1 … 12-verify.ps1
│   ├── secret/                # scripts that handle secrets, run in a separate visible window
│   ├── pi/                    # waker deploy (systemd unit, deploy script)
│   ├── rotate-secrets.ps1
│   └── uninstall.ps1
└── scripts/ build.ps1, test.ps1
```

## 8. Design decisions (short ADR log)

| # | Decision | Alternatives rejected | Reason |
|---|---|---|---|
| D1 | Tailscale only, no public endpoint | Random public URL; Cloudflare Tunnel + Access | Unreachable beats hidden. No inbound ports anywhere. |
| D2 | `tsnet` for agent/waker | Bind agent to the tailscaled IP | Not reachable from LAN even by mistake; self-contained identity and certs; waker runs on Termux without root. |
| D3 | Use Anthropic Remote Control for the Claude UI | Custom chat UI over the Agent SDK | Native app UX, push notifications, attachments, maintained by Anthropic. |
| D4 | Agent as Windows service under your account | Logon task + auto-login | Works at the lock screen after WoL without weakening login. |
| D5 | Restricted token for children | Run as-is | Claude sessions shouldn't silently hold admin rights. |
| D6 | SMB with dedicated low-privilege account | Share with your own credentials | Limits the blast radius if the phone's stored SMB password leaks. |
| D7 | Smart plug + BIOS AC restore for v1 wake (waker on a Pi as upgrade path) | Router port-forward of WoL (breaks "zero open ports"; usually impossible behind ISP CGNAT); Pi now | your choice; no extra computer; wake action only ever turns power *on*. |
| D8 | Go + Preact | Node + node-pty | Avoids native module builds on Windows; one binary to ship. |
