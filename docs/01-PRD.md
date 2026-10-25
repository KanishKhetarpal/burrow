# 01 — Product Requirements: Burrow

**Codename:** Burrow (a private tunnel from your iPhone into your Windows PC)
**Owner:** one person (single user, single phone)
**Status:** Ready to build
**Builder:** Claude Code, running on the target Windows PC, in one session

---

## 1. Problem

You want to use your Windows PC from your iPhone, mainly to run Claude Code, from anywhere. The PC may be off. Today that means a mix of tools with no single, secure, one-tap flow.

## 2. Goal

One home-screen app on the iPhone that can:

1. **Wake the PC** from fully off.
2. **Start a new Claude Code chat** in a chosen project with one tap, then continue it in the Claude iOS app.
3. **Reach the PC's files** (browse, preview, download, upload).
4. **Open a terminal** on the PC (Termius) for anything else.
5. **Control power** (sleep, shut down, restart, lock, keep awake).

All of it is reachable **only from your iPhone and your laptop**. From the laptop you also get SSH/VS Code Remote, mapped drives, and an optional full remote desktop. The PC exposes nothing to the internet or the home LAN.

## 3. Fixed environment

| Item | Value |
|---|---|
| PC OS | Windows 10/11 (64-bit) |
| PC network | Ethernet to home router |
| Phone | iPhone (Safari, home-screen web app) |
| Laptop | your laptop, Windows or macOS (asked at setup Phase 0) |
| Always-on at home | Router only. Wake via Wi-Fi smart plug (§7). |
| Claude plan | Max (Remote Control and Trusted Devices supported) |
| Private network | Tailscale |
| Terminal app | Termius (iOS) |

## 4. Users and access

- Exactly **one human**: the owner.
- Exactly **two client devices**: your iPhone and your laptop. Every other device, including your own other devices, is denied by default.
- The PC's owner account is your Windows account.

## 5. User stories (P0 = must ship in the first build)

| ID | Priority | Story | Acceptance |
|---|---|---|---|
| US-1 | P0 | As the owner, I tap **Wake** and my powered-off PC boots. | Wake runs the "PC On" shortcut; PC reachable by the app within 3 minutes; app shows Offline → Waking → Online. After a Burrow shutdown, the app prompts to cut plug power only once the PC is confirmed off. |
| US-2 | P0 | I tap **New Claude chat**, pick a project, and get a live Claude Code session I can continue in the Claude app. | Session appears in the Claude app's Code tab (green dot) within 30 s; app offers "Open in Claude". |
| US-3 | P0 | I can see, stop, and restart my running Claude sessions. | List shows status, project, start time; stop/restart work. |
| US-4 | P0 | I can watch a session's raw terminal and answer prompts (y/n, Esc, Ctrl-C) from the app. | Live terminal via xterm.js with a mobile key bar. |
| US-5 | P0 | I can browse, preview, download and upload files in allowed folders. | Works from the app; also works natively in the iOS Files app via SMB. |
| US-6 | P0 | I can open Termius into my PC with one tap. | "Open terminal" opens Termius on an `ssh://` link; key-only login succeeds. |
| US-7 | P0 | I can sleep, shut down, restart, or lock the PC, and keep it awake while Claude works. | Actions work; PC never idle-sleeps while a session is running. |
| US-8 | P0 | Only my iPhone and laptop can reach any of this, and every unlock needs Face ID / Windows Hello / Touch ID. | Verified by the security checks in `07-TEST-PLAN.md`. |
| US-9 | P0 | If my phone or laptop is lost, I can cut its access in under 2 minutes from any browser. | Documented and tested procedure in `06-SETUP-RUNBOOK.md`. |
| US-10 | P0 | I can see an audit log of every wake, login, session and file action. | Tamper-evident log viewable in the app. |
| US-11 | P0 | A panic button kills all sessions and locks Burrow down until I unlock it at the PC. | Lockdown verified in test plan. |
| US-17 | P0 | From my laptop I can use the same Burrow web app (wide layout) with Windows Hello/Touch ID. | Pairs as a second device; everything except plug-wake works from the laptop. |
| US-18 | P0 | From my laptop I can SSH into the PC and use VS Code Remote-SSH. | `ssh burrow-pc` with a passphrase-protected key works. |
| US-19 | P0 | From my laptop I can open the PC's folders as network drives. | Shares mounted via SMB over Tailscale. |
| US-20 | P0 (if chosen in Phase 0) | From my laptop I can open the PC's full desktop. | RDP (Windows Pro) or Sunshine + Moonlight (Windows Home); laptop-only. |
| US-21 | P0 | From my laptop I can continue any Burrow Claude session in claude.ai/code or Claude Desktop. | Sessions listed via Remote Control. |
| US-12 | P1 | I can say "Hey Siri, start Claude on Demo". ("Hey Siri, PC On" works from day one via the plug shortcut.) | iOS Shortcuts calling the API. |
| US-13 | P1 | A plain PowerShell web terminal inside the app (no Termius needed). | PTY shell page. |
| US-14 | P0 | Auto-shutdown after N idle minutes (no running sessions, no app activity, no SSH/SMB sessions). Needed because sleep is disabled (a sleeping PC can't be woken by the plug). | Configurable in app; off by default; warns 5 min ahead via the app's status. |
| US-15 | P1 | `burrow attach <session>` from an SSH shell attaches to a running session (tmux-style). | CLI attach over local named pipe. |
| US-16 | P1 | File search across allowed roots. | Search endpoint and UI. |

## 6. Non-goals

- No public URL, no Funnel, no port forwarding, no cloud relay of Burrow's own traffic.
- No multi-user support, no third device, no Android client.
- No remote desktop from the iPhone in v1 (laptop only).
- No storage of the Windows password, Tailscale API token, or any secret in the repo, logs, or chat.

## 7. Wake method — DECIDED: smart plug

A fully powered-off PC can't receive anything from the internet. **Chosen: Wi-Fi smart plug + BIOS "Restore on AC Power Loss = Power On".** When the plug turns on, power returns to the PC and the motherboard boots it by itself. No extra computer, no open ports.

**Safe flow (designed so a running PC never gets its power cut by the wake action):**

| Step | What happens |
|---|---|
| Shut down | Burrow → Shut down. Once the app confirms the PC is off, it prompts to run the **"PC Off"** shortcut (plug off). |
| Wake | Burrow → Wake runs the **"PC On"** shortcut (plug on). Turning *on* can never harm a running PC. BIOS boots it. |
| Plug was left on | If the PC doesn't come online in 90 s, the app offers **"Power-cycle"** ("PC Power Cycle" shortcut: off → wait 15 s → on), behind a warning and confirmation. |
| Power outage | Plug keeps its last state: PC was off → stays off; PC was on → boots when power returns. |

**Physical requirement:** wall → (UPS/inverter output, if any) → **smart plug** → PC. The plug must be the last thing before the PC; if it sits before a UPS, the UPS keeps the PC powered and wake won't work.

**Plug requirements:** 2.4 GHz Wi-Fi plug, rated ≥10 A, remote control through its own app, and **iPhone Shortcuts support** (TP-Link Tapo plugs have native Shortcuts actions; others work if their app offers "Add to Siri" for scenes). Nice to have: energy monitoring, a "power-on state after outage" setting.

The Wake-on-LAN settings and `burrow-waker` are still built (cheap) so a Raspberry Pi can be added later without code changes (`wake.method: waker`).

## 8. Success metrics

- Wake to "Online" in the app: ≤ 3 min (target 90 s).
- Tap "New Claude chat" to session visible in Claude app: ≤ 30 s.
- Zero listening ports reachable from LAN or internet (verified).
- Zero secrets in repo, logs, or Claude Code transcript.

## 9. Constraints for the builder

- Everything is built **and set up on the PC by Claude Code** following `06-SETUP-RUNBOOK.md`. Only steps that physically require you (BIOS, iPhone taps, browser logins, typing a password into a secure OS prompt) are manual, and Claude Code must stop and guide you through each.
- Security requirements in `03-SECURITY.md` are non-negotiable. If a requirement can't be met, stop and report rather than weakening it.
