# 07 — Test Plan and Acceptance

Build is "done" only when every **P0** item below passes and `SETUP-REPORT.md` shows all PASS.

## 1. Automated (run by `scripts/test.ps1`; must pass before install)

### Go unit tests
| Area | Cases |
|---|---|
| Identity gate | allowed node (iPhone or laptop) + owner → pass; pairing window lets an owner-owned unpaired node reach only pairing endpoints; allowed node + wrong login → 404; unknown node → 404; waker node on `/healthz` → 200, on `/api/*` → 404; WhoIs error → 404. (Interface `IdentityResolver` with a fake.) |
| Sessions/auth | cookie flags (`__Host-`, Secure, HttpOnly, SameSite=Strict); idle and absolute expiry; session bound to node (different node → 401); logout revokes. |
| Step-up | protected endpoints return `stepup_required` without fresh assertion; pass within 300 s; fail after. |
| CSRF | missing `X-Burrow` → 403; wrong `Origin` → 403. |
| Pairing | closed window → 404; wrong code ×5 → window closes; success stores node + credential; second pair attempt rejected. |
| Rate limits / lockout | 5 login failures → 15 min lock; 429s on API flood. |
| Lockdown | all routes 404 except `/healthz`; `unlock` only via admin pipe. |
| File path resolver | table tests + `go test -fuzz` (≥ 60 s in CI mode): `..`, encoded `%2e%2e`, mixed slashes, `\\?\`, UNC, drive letters, ADS `file.txt:stream`, reserved names, trailing dots, denylist hits, junction/symlink escaping root (created in temp dir), case variations. Every escape must be rejected. |
| Files ops | upload size cap, no overwrite by default, atomic rename, delete → Recycle Bin (Windows-only test), denylisted entries hidden from listing. |
| Prompt/URL detection | fixtures of real Claude Code output (with ANSI) → correct `remoteUrl`, `trust`, `enable_rc`, `permission` detection. |
| Audit | hash chain verifies; edited line detected; concurrent appends serialized. |
| Config | invalid configs rejected (drive root, whole profile, missing waker URL, nonexistent paths). |
| WoL | magic packet bytes exactly 102 bytes, correct MAC repetition; broadcast address calc from IP/prefix. |
| Waker | identity gate, CORS (exact origin only; preflight), rate limit, status state machine with fake probes. |
| PTY (Windows-only, build tag) | spawn `cmd /c echo hi` in ConPTY → output contains `hi`; resize works; job kill terminates child tree; restricted token: `whoami /groups` shows `BUILTIN\Administrators` as deny-only (skip with notice if the test user isn't admin). |
| Headers | every response has CSP, HSTS, nosniff, Referrer-Policy, COOP, CORP; downloads use `attachment`; HTML/SVG never inline. |

### Frontend
- `npm run build` clean, TypeScript strict, no `any` in API layer.
- `npm audit --omit=dev --audit-level=high` clean.
- Service worker: unit test of routing logic (navigation fallback, never caches `/api/*` except wake-config).
- Wake/shutdown state machine (pure TS, unit-tested): "cut plug power" is offered only after a Burrow-initiated shutdown **and** 30 s of continuous `/healthz` failure; Power-cycle only after 90 s without Online and an explicit confirm; laptop clients show the iPhone hint instead of running shortcuts.

### Supply chain
- `govulncheck ./...` clean (or documented, unreachable).
- No secrets in repo: run a secret scan (e.g. `gitleaks` via `go run` or a simple regex scan) over the tree.

## 2. Setup verification (Phase 12, automated) — must all PASS
Listed in `06-SETUP-RUNBOOK.md` Phase 12.

## 3. Manual acceptance on the iPhone (P0)

| # | Test | Pass criteria |
|---|---|---|
| M1 | Fresh open from Home Screen while PC on | Face ID prompt → Home in < 3 s |
| M2 | Shut down from Burrow | After `/healthz` fails continuously for 30 s, app offers "Turn plug off"; PC Off shortcut runs |
| M2b | Unexplained disconnect (e.g. unplug Ethernet) | App shows Offline but **never** offers to cut plug power |
| M3 | Tap Wake (plug off) | PC On shortcut runs; Online within 3 min, no Windows login needed |
| M3b | Tap Wake with plug left on | After 90 s, Power-cycle offered behind warning; it boots the PC |
| M4 | New Claude chat in a project | "Open in Claude" within 30 s; session visible in Claude app Code tab; a message round-trips |
| M5 | Answer a permission prompt from Burrow quick reply | Claude proceeds |
| M6 | View terminal | Live output; key bar works; read-only toggle works |
| M7 | Stop and resume a session | Stops cleanly; resume continues the conversation |
| M8 | Files: browse, preview photo, download to iPhone, upload a photo | All succeed; upload lands in chosen folder |
| M9 | Files app SMB | Shares visible; open and save a file |
| M10 | Termius | Key-only login works; password login refused |
| M11 | Power settings | PC never idle-sleeps (AC sleep = Never); keep-awake active while a session runs; Sleep button is labelled "can't be woken from the phone"; idle auto-shutdown works when enabled |
| M12 | Shutdown requires step-up Face ID | Prompted, then shuts down |
| M13 | Audit log shows all of the above | Entries present, chain verifies |
| M14 | Lockdown | App goes dark; SSH/SMB refused; `burrow unlock` at PC restores |
| M15 | Tailscale off on home Wi-Fi | App offline; SMB/SSH to LAN IP fail |
| M16 | Lost-phone drill: remove iPhone in Tailscale admin | Everything stops from the iPhone within 30 s; laptop unaffected; re-add + re-pair works |

## 3b. Manual acceptance on the laptop (P0)

| # | Test | Pass criteria |
|---|---|---|
| L1 | Burrow web app in the laptop browser | Windows Hello/Touch ID unlock; wide layout; sessions, files, power work; Wake shows "use your iPhone" hint |
| L2 | `ssh burrow-pc` | Key (with passphrase) login works; password login refused |
| L3 | VS Code Remote-SSH to `burrow-pc` | Opens a project folder on the PC |
| L4 | Mapped shares | Open, edit and save a file |
| L5 | Remote desktop (if enabled) | RDP/Moonlight session works from the laptop; same client from the iPhone's IP is refused |
| L6 | Claude session from the laptop | Burrow-started session appears in claude.ai/code or Claude Desktop; a message round-trips |
| L7 | Tailscale quit on laptop (home Wi-Fi) | SSH, SMB, remote desktop to the PC's LAN IP all fail |
| L8 | Lost-laptop drill: remove laptop in Tailscale admin | All laptop access stops within 30 s; iPhone unaffected |

## 4. Negative security tests (P0)
- From the PC itself, `burrow selftest` self-dial through tsnet (agent's own node identity) → **404** on `/`, `/api/*`.
- With a device's node removed from `device.allowed` (temporary config change) → 404 everywhere from that device; the other device still works; restore after.
- Tailscale policy `tests` block (in `policy/tailscale-acl.hujson`) passes on apply; any other device in the tailnet cannot reach 22/443/445 on Burrow nodes.
- From the laptop on the same LAN with Tailscale quit: `nmap -Pn -p 22,443,445,3389,47984-48010 <pc-lan-ip>` → all filtered/closed except anything you deliberately kept for LAN.
- RDP/Sunshine ports refused from the iPhone's tailnet IP (policy test + firewall rule scope).

## 5. Definition of done
- All automated tests pass; Phase 12 all PASS; M1–M16 and L1–L8 pass.
- `SETUP-REPORT.md` written; no secrets anywhere in the repo, logs, or the Claude Code transcript.
- README updated with daily-use instructions (wake, new chat, files, laptop access, lost device).
