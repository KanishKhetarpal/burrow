# 04 — API and Data Contracts

Base URL (agent): `https://<agent-fqdn>` where `<agent-fqdn>` = `<agent-hostname>.<tailnet>.ts.net` (from `tsnet` `CertDomains()`).
Base URL (waker): `https://<waker-fqdn>`.

## 0. Conventions

- JSON in/out, UTF-8. Times are RFC 3339 UTC.
- Every request passes the identity gate (`03-SECURITY.md` §3): node must be an allowlisted device (iPhone or laptop) owned by the owner login. Failure → **404** with empty body (don't reveal the service).
- **Pairing exception:** while a pairing window is open (started from the PC console), nodes owned by `owner.tailscale_login` that aren't allowlisted yet may reach only `/pair`, its static assets, and `/api/auth/pair/*`. Max 3 paired devices (iPhone, laptop, one spare).
- Authenticated endpoints need cookie `__Host-burrow`. Failure → `401 {"error":{"code":"auth_required"}}`.
- State-changing requests (POST/PUT/PATCH/DELETE) need header `X-Burrow: 1` and same-origin `Origin`. Failure → `403 csrf`.
- Step-up endpoints (marked 🔐) need a passkey assertion ≤ 300 s old on the session. Failure → `403 {"error":{"code":"stepup_required"}}`; PWA then runs the step-up flow and retries.
- Error shape: `{"error":{"code":"<snake_case>","message":"<human readable>"}}`.
- Lockdown mode: everything except `/healthz` → 404.

## 1. Health and bootstrap

| Method | Path | Auth | Response |
|---|---|---|---|
| GET | `/healthz` | identity (phone or waker) | `{"ok":true,"version":"1.0.0"}` |
| GET | `/api/wake-config` | identity | `{"method":"smartplug\|waker\|router\|none","shortcuts":{"on":"PC On","off":"PC Off","powerCycle":"PC Power Cycle"},"wakeTimeoutSec":180,"powerCycleAfterSec":90,"offConfirmSec":30,"wakerUrl":null,"routerHint":""}` (cached by the service worker; v1 uses `smartplug`) |
| GET | `/manifest.webmanifest`, `/sw.js`, `/assets/*`, `/`, `/pair` | identity | static PWA |

## 2. Auth

| Method | Path | Auth | Body → Response |
|---|---|---|---|
| GET | `/api/auth/state` | identity | `{"paired":bool,"pairingOpen":bool,"authenticated":bool,"lockdown":bool}` |
| POST | `/api/auth/pair/begin` | identity, pairing window open | `{"code":"123456"}` → WebAuthn `PublicKeyCredentialCreationOptions` (residentKey required, userVerification required, attestation none, `rp.id` = agent FQDN) |
| POST | `/api/auth/pair/finish` | identity, pairing window | attestation response + `{"deviceName":"iPhone","kind":"phone\|laptop"}` → `{"ok":true}`; stores credential + node StableID; sets session cookie; closes window |
| POST | `/api/auth/login/begin` | identity | → `PublicKeyCredentialRequestOptions` (allowCredentials = stored, UV required) |
| POST | `/api/auth/login/finish` | identity | assertion → `{"ok":true}` + cookie |
| POST | `/api/auth/stepup/begin` / `finish` | session | same as login; marks session `stepup_at` |
| POST | `/api/auth/logout` | session | revokes session |
| GET | `/api/auth/me` | session | `{"login":"…","device":{"id":"…","name":"iPhone\|Laptop","kind":"phone\|laptop"},"stepupValidUntil":"…"}` |
| POST | `/api/ssh-keys` 🔐 (or during pairing) | session | `{"publicKey":"ssh-ed25519 AAAA… termius"}` → `{"ok":true,"fingerprint":"SHA256:…"}`. Validates type (ed25519, ecdsa-sha2-nistp256, sk-* only), appends to the correct authorized_keys file with correct ACL. |

## 3. System and power

| Method | Path | Auth | Notes |
|---|---|---|---|
| GET | `/api/system` | session | `{"hostname","osVersion","uptimeSec","cpuPct","ramUsedMB","ramTotalMB","disks":[{"mount","usedGB","totalGB"}],"keepAwake":{"active":bool,"reason":"sessions\|manual","until":null},"sessions":{"running":n},"tailscale":{"online":bool}}` |
| POST | `/api/power` | session (🔐 for shutdown/restart) | `{"action":"sleep\|hibernate\|shutdown\|restart\|lock","delaySec":0-600}` → `{"ok":true,"scheduledAt":"…","plugOffSafe":bool}`. `plugOffSafe` is true only for `shutdown` (the PWA may then offer "PC Off" after confirming the PC is down). Warns (409 `sessions_running`) if sessions are running unless `"force":true`. Note: with the smart plug method, **sleep/hibernate can't be woken remotely** (plug on does nothing to a sleeping PC); the PWA labels them "won't be wakeable from the phone". |
| POST | `/api/power/keep-awake` | session | `{"enabled":bool,"minutes":0-720}` (0 = until disabled) |
| POST | `/api/lockdown` 🔐 | session | Enters lockdown (kills sessions, stops sshd and removes SMB access, returns 404 to all). If the service lacks rights to stop sshd/SMB, it still locks the app and reports which parts couldn't be stopped. |

## 4. Projects

| Method | Path | Auth | Notes |
|---|---|---|---|
| GET | `/api/projects` | session | `[{"id","name","path","isGit":bool,"lastUsedAt"}]` sorted by lastUsedAt desc |
| POST | `/api/projects` 🔐 | session | `{"name","path"}`; path must exist, be a directory, not denylisted, and sit under one of the configured `projects_parents`. Persists to config. |
| DELETE | `/api/projects/{id}` 🔐 | session | Removes from config only. |

## 5. Claude sessions

### 5.1 Model

```json
{
  "id": "s_01J…",                 // ULID
  "name": "demo-app-2314",
  "projectId": "p_…",
  "cwd": "D:\\code\\demo-app",
  "mode": "chat | server",
  "permissionMode": "default | acceptEdits | plan",
  "spawn": "same-dir | worktree",  // server mode only
  "status": "starting | ready | needs_input | running | exited | failed | stopped",
  "remoteUrl": "https://claude.ai/code/…" ,
  "prompt": {"kind":"trust | enable_rc | permission | other", "text":"…", "options":["y","n"]},
  "pid": 1234,
  "startedAt": "…", "endedAt": null, "exitCode": null,
  "restarts": 0
}
```

`bypassPermissions` is **not** accepted by the API.

### 5.2 Endpoints

| Method | Path | Auth | Notes |
|---|---|---|---|
| GET | `/api/sessions` | session | list (running first) |
| POST | `/api/sessions` | session | `{"projectId","name?","mode?","permissionMode?","spawn?"}` → 201 session. 409 `max_sessions`. |
| GET | `/api/sessions/{id}` | session | one session |
| POST | `/api/sessions/{id}/input` | session | `{"text":"y\r"}` or `{"key":"esc\|ctrl_c\|enter\|up\|down\|tab"}`; used by quick-reply buttons |
| POST | `/api/sessions/{id}/stop` | session | graceful (`/exit` then Ctrl-C ×2 after 5 s, then job kill after 10 s) |
| POST | `/api/sessions/{id}/resume` | session | chat: `claude --continue --remote-control "<name>"` in same cwd; server: `claude remote-control --continue` |
| DELETE | `/api/sessions/{id}` | session | delete record (only when not running) |
| POST | `/api/sessions/kill-all` | session | immediate job kill of all |
| GET | `/api/sessions/{id}/scrollback` | session | last ≤2 MB raw PTY bytes (base64) for terminal hydration |
| WS | `/api/sessions/{id}/term` | session | see §5.3 |

### 5.3 Terminal WebSocket protocol

- Subprotocol `burrow.term.v1`. Origin + cookie + identity checked on upgrade.
- Server → client: binary frames = raw PTY output. Text frames = JSON control: `{"t":"status","status":"…"}`, `{"t":"prompt",…}`, `{"t":"exit","code":0}`.
- Client → server: binary frames = keystrokes (UTF-8). Text frames = JSON control: `{"t":"resize","cols":80,"rows":24}`, `{"t":"ping"}`.
- On connect, server first sends scrollback, then live output.
- Read-only mode (default on open, toggle in UI) drops client keystrokes server-side.
- Idle 10 min without client input or ping → close 4000.

### 5.4 Prompt and URL detection (server side)

- Strip ANSI/OSC sequences; keep a rolling 8 KB text window.
- `remoteUrl`: first match of `https://claude\.ai/code/[A-Za-z0-9_\-/?=&.]+`.
- Prompts (case-insensitive): `Trust .+\?\s*\[y/N\]` → `trust`; `Enable Remote Control\?\s*\(y/n\)` → `enable_rc`; Claude permission dialogs (e.g. lines containing `Do you want to` with numbered options) → `permission` with options parsed when possible; else `other`.
- Detection is best-effort; the raw terminal is always available.

## 6. Files

All paths are `{root, path}` where `root` is a configured root name and `path` is relative, `/`-separated.

| Method | Path | Auth | Notes |
|---|---|---|---|
| GET | `/api/files/roots` | session | `[{"name","writable":bool}]` (no absolute paths unless step-up) |
| GET | `/api/files/list?root=&path=&sort=name\|mtime\|size` | session | `{"entries":[{"name","type":"file\|dir","size","mtime","previewable":bool}]}`; denylisted entries omitted |
| GET | `/api/files/download?root=&path=` | session | streamed, `Content-Disposition: attachment`, Range supported |
| GET | `/api/files/preview?root=&path=` | session | images (jpg/png/gif/webp/heic→jpeg if feasible), pdf, text (≤1 MB, `text/plain; charset=utf-8`) |
| POST | `/api/files/upload?root=&path=` | session, writable root | multipart, streamed; `?overwrite=false` default → 409 on clash; max `max_upload_mb` |
| POST | `/api/files/mkdir` | session | `{"root","path"}` |
| POST | `/api/files/rename` | session | `{"root","from","to"}` (same root only) |
| POST | `/api/files/delete` 🔐 | session | `{"root","path"}` → moved to Recycle Bin (`SHFileOperation`/`IFileOperation` with allow-undo) |
| GET | `/api/files/search?root=&q=` (P1) | session | name match, max 200 results, 5 s budget |

Path resolution algorithm (must be unit + fuzz tested):
1. Reject if `path` contains `\0`, `:`, a drive letter, or a UNC prefix, or any segment is `..` after cleaning.
2. `full = filepath.Join(rootAbs, filepath.FromSlash(path))`; `filepath.Clean`.
3. Resolve every existing component with `GetFinalPathNameByHandle`; reject if the result isn't under `rootAbs` (case-insensitive, `\\?\` normalized) or any component is a reparse point pointing outside the root.
4. Reject if any segment matches the denylist.
5. Reject reserved names (`CON`, `PRN`, `AUX`, `NUL`, `COM1-9`, `LPT1-9`) and trailing dots/spaces on create.

## 7. Security endpoints

| Method | Path | Auth | Notes |
|---|---|---|---|
| GET | `/api/audit?limit=200&before=` | session | newest first |
| GET | `/api/devices` | session | paired devices (name, node StableID suffix, pairedAt, lastSeen) |
| DELETE | `/api/devices/{id}` 🔐 | session | revokes credential + node; if it's the current one, logs out |

## 8. Waker API (built, not deployed in v1)

| Method | Path | Notes |
|---|---|---|
| GET | `/` | minimal page: status + Wake button (same identity rules) |
| GET | `/api/status` | `{"pc":"online\|booting\|offline","checkedAt":"…","lastWakeAt":"…"}` |
| POST | `/api/wake` | `{"ok":true,"sentAt":"…"}`; 429 if rate limited |
| OPTIONS | `/api/*` | CORS preflight; `Access-Control-Allow-Origin: https://<agent-fqdn>` exactly; `Allow-Methods: GET, POST`; `Allow-Headers: Content-Type, X-Burrow`; `Max-Age: 600` |

Waker identity gate: allow only the phone node StableID (config `allowed_node_ids`). Everything else → 404.

## 9. Audit log format

One JSON object per line:

```json
{"seq":42,"ts":"2026-10-25T10:00:00Z","actor":"device:iphone","action":"session.start","target":"demo-app-2314","result":"ok","ip":"100.x.y.z","meta":{"mode":"chat"},"prev":"<sha256 of previous line>","hash":"<sha256 of this line without hash field>"}
```

Actions: `auth.pair`, `auth.login`, `auth.login_fail`, `auth.stepup`, `auth.logout`, `deny.identity`, `deny.csrf`, `deny.ratelimit`, `wake.request` (waker), `session.start|stop|resume|kill_all|input`, `file.download|upload|mkdir|rename|delete`, `power.*`, `lockdown.enter|exit`, `sshkey.add`, `device.revoke`, `config.change`.

## 10. Config schema

See `config/burrow.example.yaml`. The agent validates on start and refuses to run with:
- an empty `owner.tailscale_login` (after setup),
- a root or project path that doesn't exist,
- a root that equals a drive root (`C:\`) or `%USERPROFILE%` itself (too broad; use subfolders),
- `wake.method = waker` without `wake.waker_url`,
- `wake.method = smartplug` without all three shortcut names.
