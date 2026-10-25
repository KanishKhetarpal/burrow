# Kickoff prompt — paste this into Claude Code

Run Claude Code from **Windows Terminal (Administrator)** in this folder, with `TS_API_KEY` loaded as shown in `docs/06-SETUP-RUNBOOK.md` → "Before you start". Then paste everything between the lines:

---

Build and install **Burrow** on this Windows PC, end to end, in this session.

1. Read `CLAUDE.md`, then every file in `docs/`, plus `policy/tailscale-acl.hujson` and `config/burrow.example.yaml`. These are the full spec. `docs/03-SECURITY.md` is non-negotiable.
2. Create a task list covering Part A (build milestones 1–12 in `CLAUDE.md`) and Part B (setup Phases 0–12 in `docs/06-SETUP-RUNBOOK.md`). Keep it updated as you go.
3. **Part A:** build the whole system (Go agent + CLI, waker, PWA, setup scripts) with tests. Don't move on until `scripts/test.ps1` passes.
4. **Part B:** do the entire PC setup yourself by running the phase scripts: toolchain installs, Tailscale tailnet config via the API, OpenSSH, SMB shares, firewall, power plan, remote desktop for my laptop (if I choose it), Claude Code Remote Control config, the Burrow Windows service, smart-plug wake config, pairing my iPhone and laptop, verification. Everything that can be automated, you automate. Write the laptop setup script too.
5. Stop only for:
   - the single Phase 0 question batch,
   - 🙋 MANUAL steps (my iPhone, my laptop, the smart plug and its shortcuts, BIOS, browser logins, typing my Windows password into the OS prompt) — give me short numbered instructions, then verify the result yourself,
   - ⚠️ CONFIRM points listed in the runbook.
6. Never show, echo, log, or ask me to paste any secret in chat. Use the secret-window pattern from the runbook. Read the Tailscale token only from `$env:TS_API_KEY`.
7. If an external API, flag, or library differs from the spec, check the current official docs, adapt while keeping the security intent, and note it in `docs/DEVIATIONS.md`.
8. Before the BIOS reboot, save progress and tell me exactly how to resume.
9. Finish when `docs/07-TEST-PLAN.md` "Definition of done" is met: write `SETUP-REPORT.md` and give me a short summary.

Start now with Phase 0 preflight checks while you scaffold the repo.

---
