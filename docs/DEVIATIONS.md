# Deviations from the spec

Where the build differs from `docs/01-PRD.md` … `docs/07-TEST-PLAN.md`, or where an
external API turned out not to behave as the spec assumed. The security intent in
`docs/03-SECURITY.md` is unchanged by everything below.

---

## D-1 — `GetFinalPathNameByHandle` flags are not exported by `x/sys/windows`

**Spec:** `docs/04-API.md` §6 step 3 — resolve components with
`GetFinalPathNameByHandle`.

`golang.org/x/sys/windows` v0.49.0 exports the function but not
`FILE_NAME_NORMALIZED` or `VOLUME_NAME_DOS`. Both are `0x0` (the default form is a
normalised path with a drive letter), so they are declared locally in
`internal/files/finalpath_windows.go` rather than passing a bare `0`. Behaviour is
exactly as specified.

---

## D-2 — ConPTY children need NULL standard handles

**Spec:** `docs/02-ARCHITECTURE.md` §2.1 — spawn in a ConPTY via
`CreatePseudoConsole`.

Attaching the pseudo-console with `PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE` is not by
itself enough to capture a child's output. A console process receives its standard
handles through the PEB regardless of the `inheritHandles` argument to
`CreateProcess`, so the child ends up **correctly attached to the pseudo-console**
— `mode con` inside it reports the size passed to `CreatePseudoConsole` — while its
`stdout` still points at the parent's `stdout`. Everything it prints then bypasses
the PTY, and the output pipe carries only the console's 16-byte handshake
(`\x1b[?9001h\x1b[?1004h`). Nothing errors.

`internal/pty/conpty_windows.go` therefore sets `STARTF_USESTDHANDLES` with
`StdInput`, `StdOutput` and `StdErr` all `0`: the child has nothing to inherit and
falls back to its attached console, which is the PTY.

Two related traps, both of which produce an output pipe that is silent rather than
an error:

- `CREATE_NO_WINDOW`, `DETACHED_PROCESS` and `CREATE_NEW_CONSOLE` each stop the
  pseudo-console attaching. None is passed.
- `ClosePseudoConsole` blocks until the console host has flushed its pending
  output, so closing it while the read end is open and undrained deadlocks.
  `Close` closes the pipe ends first, which makes those writes fail immediately.

---

## D-3 — Restricted token: no `DuplicateTokenEx`, and narrower rights

**Spec:** `docs/02-ARCHITECTURE.md` §5 — `CreateRestrictedToken` with
`DISABLE_MAX_PRIVILEGE`, Administrators deny-only, Medium integrity.

All three restrictions are applied as specified. Two implementation details differ
from the obvious reading:

- Opening the process token with `TOKEN_ALL_ACCESS` fails with "access denied" on a
  normal user account. `RestrictedToken` asks for exactly
  `TOKEN_DUPLICATE | TOKEN_QUERY | TOKEN_ASSIGN_PRIMARY | TOKEN_ADJUST_DEFAULT`
  instead. The last is needed because the restricted token inherits the access
  rights of the source handle, and lowering its integrity label requires it.
- The intermediate `DuplicateTokenEx` is unnecessary: `CreateRestrictedToken`
  returns a token of the same type as its input, and a process token is already a
  primary token, which is what `CreateProcessAsUser` needs.

No restricting SIDs are added. A restricting SID would also block reads of the
owner's own files, which a Claude session legitimately needs; the deny-only
Administrators entry is what removes the privilege.

`internal/pty` exports `AdminDenyOnly` so `burrow selftest` and the PTY test can
assert the restriction took effect on the token itself, rather than relying on the
wording of `whoami /groups`.

---

## D-4 — `go vet` and the pseudo-console attribute

`PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE` takes the `HPCON` **by value** as its
`lpValue`. The typed helper in `x/sys` takes an `unsafe.Pointer`, so using it means
converting a handle into a pointer, which `go vet`'s `unsafeptr` check reports as a
possible misuse. `internal/pty` calls `UpdateProcThreadAttribute` directly with a
`uintptr` argument instead, which is both correct and vet-clean. Passing a
*pointer to* the handle, which would also satisfy vet, was tested and does not
work: the console never attaches.

---

## D-5 — The pairing window also opens `/healthz` and `/api/auth/state`

**Spec:** `docs/04-API.md` §1 — during a pairing window an unpaired owner node
"may reach only `/pair`, its static assets, and `/api/auth/pair/*`".

Built exactly to that wording, no device could pair. The PWA asks `/healthz` and
`/api/auth/state` before it renders anything, both answered 404, and the app
concluded the PC was off and showed the Wake screen instead of the pairing form.
Found by rendering `/pair` as an unpaired node in the dev harness
(`cmd/burrow-dev`); no test covered it because the gate and the PWA were each
correct on their own.

`internal/web/gate.go` `isPairingPath` now admits those two paths as well, still
only for an owner-owned node and only while a window opened from the PC console is
live. Neither says more than loading `/pair` already does: that the agent is up
and pairing is open. `TestGatePairingWindowScope` asserts both stay closed when no
window is open and that `/`, `/api/wake-config` and `/api/auth/me` stay closed
while one is.

---

## D-6 — xterm's injected styles under `style-src 'self'`

**Spec:** `docs/03-SECURITY.md` §4 — CSP `style-src 'self'`, no inline style.

xterm's DOM renderer writes its cell layout and colour theme into `<style>`
elements it creates at runtime. The CSP refuses them, so the terminal rendered
with no theme and broken cell geometry; the only symptom was a CSP report in the
console. The policy is unchanged. `web/src/csp-styles.ts` mirrors the text of
those elements into adopted constructable stylesheets, which CSP does not govern,
and keeps them in sync as xterm rewrites them on resize and font changes. The
browser still logs a report for each original element it refuses; that is
expected.

---

## D-7 — Dev-only advisories left in the vitest 3 line

`npm audit --omit=dev --audit-level=high` (the gate in `scripts/test.ps1`) is
clean: `preact` moved to 10.27.3 for GHSA-36hm-qxxp-pg3m and `vite` to 7.3.7.
A full `npm audit` still reports `tinypool` and `@vitest/mocker` advisories inside
vitest 3.2.7, the newest 3.x. They affect only the test runner, which never ships
and never serves anything; clearing them needs vitest 4, a major upgrade left for
its own change.

---

## D-8 — bypassPermissions is opt-in, not banned

**Spec:** `CLAUDE.md` non-negotiable 5 and `docs/03-SECURITY.md` T7 — "bypassPermissions
is never offered or accepted".

Changed at the owner's explicit request on 2026-10-25, after being told the risk:
anyone holding the unlocked phone could have Claude run anything the owner's account
can, with no prompts. The guard rails kept around it:

- **Off unless `claude.allow_bypass_permissions: true` is in burrow.yaml**, which only
  the PC can edit; no API sets it. `default_permission_mode` still cannot be
  bypassPermissions, so every bypass session is chosen explicitly.
- **A fresh step-up (Face ID) for every bypass session**, checked in
  `handleCreateSession` before the session manager runs.
- **The restricted token still applies**: the session has no admin rights and
  cannot change Windows itself.
- The session start is audited with its permission mode, as every start is.

Tests: `TestBypassPermissionsNeedsTheFlagAndAFreshStepup`,
`TestCreateSessionRefusesBypassPermissions` (flag off),
`TestStartRefusesBypassPermissionsUnlessEnabled`.
