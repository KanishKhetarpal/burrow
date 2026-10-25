# Burrow

A private remote for your Windows PC, usable only from **your iPhone and your laptop**:
wake it, start Claude Code chats, reach your files, open a terminal, and (from the laptop)
use the full desktop. Nothing is exposed to the internet or your home LAN; everything goes
over Tailscale and needs Face ID / Windows Hello / Touch ID.

This repo is the complete package: the spec, the Go agent + CLI, the PWA, and the setup
scripts that configure a PC end to end. Clone it, run the setup, and it is yours — no
values in here belong to anyone's machine, tailnet or account.

## What you get

- **Wake a powered-off PC** from the phone, via a Wi-Fi smart plug + the motherboard's
  "restore on AC power loss" setting. No open ports, no extra computer.
- **Claude Code sessions** started and driven from the phone (chat or Remote Control),
  running as your Windows user with a restricted (non-admin) token.
- **Files**: browse, preview, download, upload, rename, delete to the Recycle Bin — plus
  SMB for the iOS Files app and mapped drives on the laptop.
- **Terminal** in the app (xterm.js over a ConPTY) and real SSH from the laptop.
- **Power and system**: stats, shutdown, keep-awake while sessions run.
- **Passkey-only auth** (WebAuthn), step-up for anything destructive, device allowlist by
  Tailscale node, hash-chained audit log, lockdown switch.

## Requirements

| | |
|---|---|
| PC | Windows 10/11, x64, an account you can run a service as |
| Phone | iPhone (iOS 17+) — Tailscale, Shortcuts, Claude app, an SSH client |
| Laptop | Windows or macOS with Tailscale |
| Network | A Tailscale account (free tier is enough) |
| Wake | A Wi-Fi smart plug whose app supports iPhone Shortcuts |
| Build | Go 1.23+, Node 20+, PowerShell 7 (setup phase 1 can install these) |

Everything is reachable **only** from the two devices you pair. Every other device,
including your own, is denied by default.

## What's in here

| Path | Purpose |
|---|---|
| `SETUP.md` | **Start here** — the setup checklist you run |
| `BUILD_PROMPT.md` | The prompt to paste into Claude Code if you want it to build + set up for you |
| `CLAUDE.md` | Operating rules Claude Code follows during the build |
| `docs/01-PRD.md` | Requirements, user stories, wake decision |
| `docs/02-ARCHITECTURE.md` | Components, flows, tech stack, repo layout |
| `docs/03-SECURITY.md` | Threat model and non-negotiable controls |
| `docs/04-API.md` | API, WebSocket and data contracts |
| `docs/05-UI.md` | App screens (iPhone + laptop) |
| `docs/06-SETUP-RUNBOOK.md` | Every PC setup step, phase by phase |
| `docs/07-TEST-PLAN.md` | Tests and definition of done |
| `docs/DEVIATIONS.md` | Where the implementation departs from the spec, and why |
| `cmd/`, `internal/` | Go agent, CLI and waker |
| `web/` | The PWA (Vite + Preact + TypeScript) |
| `setup/` | PowerShell setup phases 0–12, uninstall, secret windows |
| `laptop/` | Scripts you run on the laptop |
| `policy/tailscale-acl.hujson` | Two-device network policy template with built-in tests |
| `config/burrow.example.yaml` | Agent config template |

## Buy and wire the smart plug

- **Plug:** a 2.4 GHz Wi-Fi smart plug rated **10 A or more** whose app supports **iPhone
  Shortcuts** (TP-Link Tapo plugs do natively; others work if their app has "Add to Siri"
  for scenes). Energy monitoring is a bonus.
- **Wiring:** wall → (UPS/inverter output, if you use one) → **smart plug** → PC. The plug
  must be the last thing before the PC.
- Your motherboard must have a "restore on AC power loss" (AC BACK / APM) setting. Setup
  walks you to it.

## Install

1. Clone this repo to `C:\dev\burrow` on the PC.
2. Follow `SETUP.md`.

Either build it yourself (`.\scripts\build.ps1`, then `.\setup\run-all.ps1`) or run
`claude` in the folder and paste `BUILD_PROMPT.md` to have Claude Code build it and drive
the setup, stopping only for the steps that need your hands: phone and laptop taps, the
plug app and Shortcuts, your Windows password in a Windows prompt, one firmware setting,
and the wake test.

## Daily use (after setup)

**iPhone**
- **Wake:** Burrow → Wake PC (or "Hey Siri, PC On").
- **Shut down:** Burrow → Shut down → when it says the PC is off, tap "Turn plug off".
- **New chat:** New Claude chat → pick project → Open in Claude.
- **Files:** in Burrow, or the Files app (`smb://<your PC name>`).
- **Terminal:** Open terminal (in-app, or your SSH client).

**Laptop**
- Burrow web app (installed from the browser) for sessions, files and power.
- `ssh burrow-pc`, or VS Code Remote-SSH to `burrow-pc`.
- Mapped drives for your PC folders.
- Remote desktop (Remote Desktop / Windows App, or Moonlight) if you enabled it.
- Claude sessions also show up in claude.ai/code and Claude Desktop.
- Wake still happens from the iPhone.

**Lost device:** login.tailscale.com → remove that device. Its access stops instantly; the
other device keeps working. Then `burrow devices revoke <name>` and
`.\setup\rotate-secrets.ps1` on the PC.

## Security posture in one paragraph

No public endpoint exists: the agent listens only on a `tsnet` node (Tailscale), never on
an OS interface, and Funnel/Serve are never enabled. Every request passes a Tailscale ACL,
a WhoIs node allowlist, a passkey session, step-up where marked, and the audit log. SSH
(22) and SMB (445) are firewall-scoped to the two paired tailnet IPs; remote desktop, if
enabled, to the laptop only. Claude sessions run with Administrators set to deny-only and
`bypassPermissions` is never accepted. Secrets never touch chat, logs, command lines or
the repo. Details and the threat model: `docs/03-SECURITY.md`.

## Status

v1, built and tested. The Raspberry Pi waker (`cmd/burrow-waker`) is built and tested but
not deployed — the smart plug covers wake in v1.
