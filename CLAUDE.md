# CLAUDE.md — Operating instructions for building and installing Burrow

You are Claude Code running **on the owner's Windows PC, in an elevated (Administrator) terminal**. Your job, in one session: **build Burrow from this spec, then set it up completely on this PC** so the owner can wake the PC, start Claude Code chats, reach their files, and open a terminal from their iPhone, and reach the PC from their laptop (web app, SSH/VS Code, drives, optional remote desktop) — reachable only from those two devices. Wake is via a smart plug (already decided).

## Read first (in order)
1. `docs/01-PRD.md` — what and why
2. `docs/02-ARCHITECTURE.md` — components, flows, stack, repo layout
3. `docs/03-SECURITY.md` — **non-negotiable** rules
4. `docs/04-API.md` — contracts
5. `docs/05-UI.md` — PWA screens
6. `docs/06-SETUP-RUNBOOK.md` — the phases you will execute on this PC
7. `docs/07-TEST-PLAN.md` — definition of done
8. `policy/tailscale-acl.hujson`, `config/burrow.example.yaml`

## How to work
- Create a task list covering Part A (build milestones) and Part B (setup Phases 0–12) and keep it updated. The owner watches it from their phone.
- **Ask questions only** at: Phase 0's single question batch, 🙋 MANUAL steps, and ⚠️ CONFIRM points defined in the runbook. Otherwise make the reasonable call, note it in `docs/DEVIATIONS.md`, and keep going.
- When an external API or library differs from what the spec assumes (Tailscale API endpoints, `tsnet` method names, Claude Code flags/settings), **check the current official docs**, adapt the implementation, and keep the security intent identical. Record the change in `docs/DEVIATIONS.md`.
- Prefer small, verifiable steps. After each milestone: build, run tests, commit.
- `git init` the repo at the start. Conventional commits. Never commit secrets, `burrow.yaml` with real values, state, logs, or keys (`.gitignore` them).

## Non-negotiables (stop and report rather than weaken)
1. Nothing listens on any OS network interface except what the runbook configures (sshd 22 and SMB 445 firewall-scoped to the iPhone + laptop tailnet IPs; RDP or Sunshine, if chosen, scoped to the laptop IP). Agent and waker listen **only via `tsnet`**.
2. No public exposure: no Funnel, no Serve, no port forwards, no third-party relays, no CDNs in the PWA.
3. Every agent request: Tailscale ACL → WhoIs node allowlist → passkey session → step-up where marked → audit.
4. **Secrets:** never print, echo, log, commit, or put secrets on a command line; never ask you to paste a secret into chat. Use the secret-window pattern in the runbook. `TS_API_KEY` is read from the environment only.
5. Claude sessions launched by Burrow run with a **restricted token** (no admin). `bypassPermissions` is never offered or accepted.
6. Back up anything you change on the system first, and record how to revert it in `setup-state.json`.
7. Don't change anything outside the scope of the runbook (no unrelated settings, no removing software, no touching other users).

## Part A — Build milestones
1. **Scaffold:** Go module (`cmd/burrow`, `cmd/burrow-waker`, `internal/*`), `web/` (Vite + Preact + TS, strict), `setup/` with `lib/Burrow.Setup.psm1`, `scripts/build.ps1`, `scripts/test.ps1`, `.gitignore`, `README.md`.
2. **Core:** config (+ validation), store (SQLite, modernc), audit (hash chain + verify), identity interface + fake, WoL packet builder.
3. **Network/web:** `tsnode` (tsnet, random hostname, state dir, `ListenTLS :443`, logs upload off), HTTP router, security headers, identity gate, `/healthz`, embedded PWA serving, admin named pipe for CLI.
4. **Auth:** pairing window + code, WebAuthn registration/login/step-up, sessions, CSRF, rate limits, lockout, lockdown/unlock, SSH key add.
5. **Sessions:** ConPTY wrapper, restricted-token `CreateProcessAsUser`, Job Objects, session manager (chat + server modes, watchdog), scrollback, prompt and Remote Control URL detection, terminal WebSocket, keep-awake integration.
6. **Files:** resolver (with fuzz tests), list/preview/download (Range)/upload/mkdir/rename/delete-to-Recycle-Bin.
7. **System/power:** stats, power actions, keep-awake.
8. **PWA:** all screens in `05-UI.md` (phone + wide laptop layout), smart-plug wake/shutdown state machine, service worker offline Wake flow, passkey flows, xterm.js terminal with key bar.
9. **Waker (build + tests only; not deployed in v1):** tsnet node, wake/status/CORS/rate limit/identity gate, fallback page, systemd unit, cross-compile.
10. **CLI/service:** `run`, `enroll`, `install` (credential prompt, LogonAsService, recovery), `uninstall`, `pair` (QR), `unlock`, `status`, `devices`, `audit verify`, `selftest`.
11. **Setup scripts:** `setup/00-preflight.ps1` … `12-verify.ps1`, `uninstall.ps1`, `rotate-secrets.ps1`, `secret/` window scripts, `pi/` deploy assets (future waker), and `laptop/setup-windows.ps1` + `laptop/setup-macos.sh` (run by the owner on the laptop).
12. **Green:** `scripts/test.ps1` passes fully (Go tests incl. Windows PTY/token tests, fuzz smoke, govulncheck, npm build/audit, secret scan).

## Part B — Set up this PC
Execute `docs/06-SETUP-RUNBOOK.md` Phases 0–12 in order, exactly as written. Use the scripts you wrote in milestone 11. For each phase: run with `-WhatIf` first and read the plan, then apply, then verify, then update the task list.

### Resume protocol
Phase 10 reboots the PC. Before any reboot or shutdown: commit, write progress to `C:\ProgramData\Burrow\setup-state.json`, and tell the owner exactly how to resume (`claude --continue` in this folder, elevated, then "continue setup"). On resume, read `setup-state.json` and continue from the first incomplete phase.

## Windows notes
- Use PowerShell 7 (`pwsh`) for scripts once installed; Windows PowerShell 5.1 only for bootstrapping.
- Refresh `PATH` after winget installs (`[Environment]::GetEnvironmentVariable('Path','Machine') + ';' + ...User`).
- Long-running/interactive steps (credential prompts, secret displays, `burrow pair`) run in a **new visible window** via `Start-Process`; wait on a marker in `setup-state.json`.
- The Burrow service runs in Session 0: no GUI, no browser. Claude must already be logged in for your account; `selftest` proves it.

## Definition of done
`docs/07-TEST-PLAN.md` §5. Finish with a short summary to the owner: what works, anything skipped and why, where `SETUP-REPORT.md` is, and the lost-device procedure in one line.
