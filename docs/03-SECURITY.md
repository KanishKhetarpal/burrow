# 03 — Security Model (non-negotiable)

Principle: **unreachable first, then authenticated, then authorized, then audited.** A random URL is not security; Burrow has no public URL at all. Allowed client devices: **your iPhone and your laptop**, nothing else.

## 1. Assets

| Asset | Why it matters |
|---|---|
| PC filesystem and code | Source code, documents, credentials on disk |
| Claude Code sessions | Can run arbitrary commands in projects |
| your Claude, Tailscale and GitHub identities | Account takeover paths |
| Burrow passkey and session cookies | Grant app access |
| SSH private keys on the iPhone and laptop | Grant shell |
| `burrowfs` SMB password on the iPhone and laptop | Grants file access |
| Remote desktop access from the laptop (RDP or Sunshine) | Grants full desktop |

## 2. Threat model

| # | Threat | Mitigation |
|---|---|---|
| T1 | Internet scanner finds the PC | No port forwards, UPnP off, nothing listens publicly. Tailscale uses outbound connections only. |
| T2 | Someone on the home LAN (guest, IoT device) | Agent/waker don't bind to LAN (tsnet). Windows Firewall allows 22/445 **only** from the iPhone's and laptop's tailnet IPs (and 3389/Sunshine only from the laptop) on the Tailscale interface. |
| T3 | Another device joins your tailnet (stolen login, malicious node) | Device approval on; Tailnet Lock (recommended); ACL default-deny with rules only for the iPhone and laptop; agent WhoIs allowlist by node StableID. |
| T4 | your Tailscale account compromised | Identity provider protected by passkey/2FA; Tailnet Lock blocks adding unsigned nodes; Burrow passkey still required. |
| T5 | Phone stolen while unlocked | Burrow requires Face ID passkey per session (12 h absolute, 30 min idle); step-up for dangerous actions; Termius app lock; lost-device kill procedure (§7). |
| T17 | Laptop stolen or infected with malware (it's an allowed device) | Burrow passkey per session via Windows Hello/Touch ID; step-up for dangerous actions; SSH key protected by a passphrase (Keychain/ssh-agent); RDP with NLA + account lockout, or Sunshine with PIN pairing; laptop full-disk encryption (BitLocker/FileVault) required; laptop Tailscale key expires every 180 days; one-click removal in Tailscale admin (§7). |
| T6 | Claude account compromised | Claude **Trusted Devices** on (device enrollment + 18 h biometric step-up); claude.ai login protected by passkey/2FA. |
| T7 | Prompt injection makes Claude run harmful commands | Restricted token (no admin); default permission mode stays `default` (no bypass by default); `bypassPermissions` not offered in UI; secrets denylisted from file API. |
| T8 | Path traversal / symlink escape in file API | Canonicalize, enforce root prefix, reject reparse points escaping roots, denylist, fuzz tests. |
| T9 | XSS/CSRF in PWA | Strict CSP (no inline, no eval, self only; waker origin added only if a waker is used), SameSite=Strict `__Host-` cookies, Origin check, custom header requirement, output encoding. |
| T10 | Brute force of pairing code or passkey | Pairing: opened only from the PC console, 10-min window, 5 attempts, single use. Login: lockout after 5 failures for 15 min, audited. |
| T11 | Malicious wake spam | Waker rate limits; wake only powers on the PC (no data access). Accepted residual risk. |
| T12 | Secrets leak via logs, repo, or Claude transcript | Secrets only via OS secure prompts or env vars set by the owner; never echoed; log scrubber; `.gitignore`; pre-commit secret scan. |
| T13 | Tampering with audit log | Hash-chained entries; `burrow audit verify`. |
| T14 | Supply-chain risk | Pinned module versions with `go.sum`/lockfile; `govulncheck` and `npm audit` in test step; winget installs from official IDs only. |
| T16 | Smart plug vendor account compromised (attacker can toggle PC power) | 2FA on the plug app account; plug is the only device on that account that matters; Burrow's wake action only turns power **on**; the "cut power" prompt appears only after a confirmed Burrow shutdown. Worst case is a forced power-off (possible unsaved-work loss), not data access. |
| T15 | Cert hostname disclosure (Certificate Transparency logs show `*.ts.net` names) | Random, non-identifying hostnames (`n-<8 random>`), random tailnet name. Names reveal nothing and the hosts stay unreachable. |

## 3. Control layers (every request to the agent passes all of them)

1. **Network:** Tailscale ACL allows only iPhone + laptop → agent `:443`; iPhone + laptop → PC `:22, :445`; laptop → PC remote-desktop ports (if enabled); iPhone → waker `:443` and waker → agent `:443` (health only), both unused until a waker is added. Everything else denied. Policy `tests` assert this (see `policy/tailscale-acl.hujson`).
2. **Device identity (app level):** `tsnet` WhoIs on `RemoteAddr`. Reject unless the node StableID is in `device.allowed` (the iPhone and the laptop) **and** the user login equals `owner.tailscale_login`. Exceptions: `/healthz` also accepts the waker node and returns `{ok,version}` only; during an open pairing window (opened only from the PC console), an owner-owned node that isn't allowlisted yet may reach the pairing endpoints only. Rejects return 404 (don't reveal the service) and are audited.
3. **Human presence:** WebAuthn passkey with `userVerification: required` (Face ID). Session cookie `__Host-burrow` (Secure, HttpOnly, SameSite=Strict, Path=/), 30 min idle / 12 h absolute, bound to node StableID. Server-side session store; logout revokes.
4. **Step-up:** fresh passkey assertion (≤5 min old) for shutdown, restart, file delete, adding SSH keys, editing projects/roots, lockdown, viewing settings that reveal paths.
5. **Authorization scope:** file API limited to configured roots minus denylist; sessions limited to configured projects; `max_sessions` cap.
6. **Audit:** every auth event, wake, session start/stop, file write/delete/download, power action, denied request.

## 4. Mandatory implementation rules

- Agent and waker **must not** call `net.Listen` on OS interfaces. All listeners come from `tsnet`. Exception: the local admin named pipe for CLI (`\\.\pipe\burrow-admin`), ACL'd to SYSTEM + Administrators + the owner.
- Never enable Tailscale Funnel or Serve; ACL has no `funnel` nodeAttr.
- TLS only (tsnet `ListenTLS`); HSTS `max-age=31536000`.
- Headers on every response: CSP `default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data: blob:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'` (add `https://<waker-fqdn>` to `connect-src` only when `wake.method: waker`), `X-Content-Type-Options: nosniff`, `Referrer-Policy: no-referrer`, `Permissions-Policy` (camera/mic/geo off), `Cross-Origin-Opener-Policy: same-origin`, `Cross-Origin-Resource-Policy: same-origin`.
- State-changing API calls require `X-Burrow: 1` header and a matching `Origin`.
- File downloads: `Content-Disposition: attachment` unless the preview whitelist applies (images, PDF, plain text rendered as `text/plain`); never serve HTML/SVG inline.
- WebSocket upgrade checks Origin, session cookie, and node identity; idle timeout 10 min without input.
- Uploads stream to a temp file in the target root, size-capped (`max_upload_mb`), then atomic rename; filenames sanitized (no reserved Windows names, no ADS `:`).
- **File denylist (always, case-insensitive, even inside roots):** `.ssh`, `.gnupg`, `.claude\.credentials.json`, `.claude.json`, `AppData`, `NTUSER.DAT*`, `*.kdbx`, `*.pem`, `*.key`, `id_*`, `.env*`, `C:\ProgramData\Burrow\**`, browser profile folders. Configurable additions only (no removals of defaults without editing code).
- Rate limits: auth endpoints 10/min; API 120/min; uploads 10 concurrent max.
- Lockdown mode: all endpoints except `/healthz` return 404; sessions killed; SMB share and sshd stopped; cleared only by `burrow unlock` from an admin console on the PC.
- Secrets: the Tailscale API token comes from env var `TS_API_KEY` set by you before launching Claude Code. The Windows password goes straight into an OS credential prompt and then into the Service Control Manager. The `burrowfs` password is generated (32 chars), shown **once** in a local window for you to save in iOS Passwords, and never written to disk. None of these ever appear in chat, logs, files, or command lines (`ps` visible args).
- Claude Code (the builder) must **never** print, echo, or log secret values, and must never ask you to paste a secret into chat.

## 5. Windows hardening (applied by setup)

- Windows Firewall on for all profiles; default inbound block.
- Rules: `Burrow-SSH` TCP 22 and `Burrow-SMB` TCP 445, inbound, remote address = iPhone + laptop tailnet IPs, interface alias = Tailscale. `Burrow-RDP` (TCP/UDP 3389) or `Burrow-Sunshine` (its streaming ports): laptop IP only. Existing built-in File and Printer Sharing / RDP rules: report state; change only with your confirmation.
- SMB1 disabled; share-level `EncryptData $true`; server signing required; guest access off.
- `burrowfs`: standard local user, password never expires off (rotated manually), not in any group but Users; denied "log on locally", "log on through Remote Desktop", "log on as batch/service".
- OpenSSH `sshd_config`: `PasswordAuthentication no`, `PubkeyAuthentication yes`, `PermitEmptyPasswords no`, `KbdInteractiveAuthentication no`, `AllowUsers <windows-user>`, `MaxAuthTries 3`, `LoginGraceTime 20`, `AllowTcpForwarding no` (config flag to allow), `X11Forwarding no`, `PermitTunnel no`, `ClientAliveInterval 60`, `ClientAliveCountMax 3`, modern ciphers/KEX only. Admin users' keys go in `C:\ProgramData\ssh\administrators_authorized_keys` with ACL SYSTEM + Administrators only.
- Remote desktop (laptop only, runbook 5.4): RDP with NLA + TLS security layer + account lockout, built-in RDP firewall rules disabled, only `Burrow-RDP` (laptop IP). On Windows Home: Sunshine with its web UI bound to localhost, PIN pairing, only `Burrow-Sunshine` rules (laptop IP). If you decline remote desktop, RDP stays off.
- Power: AC sleep and hibernate set to Never (a sleeping PC can't be woken by the smart plug); idle auto-shutdown available in Burrow.
- BitLocker: report status; recommend enabling (not automated).
- Fast Startup off (needed for reliable WoL; also avoids stale sessions).

## 6. Tailscale hardening

- Tailnet ACL from `policy/tailscale-acl.hujson` with tests.
- Device approval on; key expiry on for the iPhone and laptop (re-auth every 180 days); tagged nodes don't expire.
- PC tailscaled tagged `tag:pc`; agent `tag:burrow-agent`; waker `tag:burrow-waker`. Auth keys for tags: single-use, pre-approved, 1-hour expiry, created via API, never written to disk (passed through the environment to first start only).
- MagicDNS + HTTPS certs on.
- **Tailnet Lock** (recommended, opt-in at Phase 11): PC and iPhone (laptop optional) as signing nodes; disablement secrets shown once for you to store in iOS Passwords.
- Tailscale login identity protected with passkey/2FA (manual check).
- API token: short expiry (≤1 day) and revoked at the end of setup.

## 7. Lost or stolen iPhone or laptop (≤2 minutes, any browser)

1. **login.tailscale.com → Machines → that device → Remove.** This cuts Burrow, SSH, SMB and remote desktop from it instantly; the other device keeps working.
2. **claude.ai → Settings → Account → Trusted devices → remove that device**, then sign out everywhere.
3. When back at the PC: `burrow devices revoke <device>`, rotate the `burrowfs` password (`setup/rotate-secrets.ps1`), remove that device's SSH key, unpair it from Moonlight if used, then pair the replacement. If the iPhone is lost, also change the smart plug app password.

## 8. Accepted residual risks

- Waking the PC (plug on) needs only the plug app's login on the phone, not Face ID in Burrow. Booting the PC grants no access by itself.
- The smart plug depends on its vendor's cloud. If the vendor is down, remote wake is unavailable (everything else still works while the PC is on).
- SSH logins by an admin user get an elevated token (Windows OpenSSH behavior). Mitigated by key-only auth, two-device ACL and firewall scope.
- Remote Control transcripts are stored by Anthropic per its data policy (inherent to the feature).
- A Burrow-launched Claude session can still do anything your non-admin user can do inside your profile. Permission prompts stay on by default.
