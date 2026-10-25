# Burrow — setup checklist

Follow this once, on the Windows PC you want to reach remotely. The build is automated;
what is left is the part that changes Windows and your tailnet, which you run yourself:
one elevated command that pauses at each step needing your hands or your phone.
Allow about 40 minutes.

You need: the Windows PC, your iPhone, your laptop, and a Wi-Fi smart plug (see
`README.md` → "Buy and wire the smart plug").

---

## 1. Before you start (2 minutes, on the PC)

1. Sign in at <https://login.tailscale.com> (make an account if you don't have one;
   use a Google/GitHub/Apple login that has 2FA or a passkey on).
2. Admin console → **Settings → Keys → Generate API access token**, expiry **1 day**.
   Copy it. Don't paste it anywhere except the prompt below.
3. Install Claude Code on the PC and sign in as the account whose sessions you want to
   reach from the phone.
4. Start → type **Terminal** → right-click → **Run as administrator**.

## 2. Build it

```powershell
cd C:\dev\burrow
.\scripts\build.ps1       # builds the PWA, embeds it, builds burrow.exe into dist\
.\scripts\test.ps1        # optional: full test suite
```

(Or let Claude Code do the whole build and setup — see `BUILD_PROMPT.md`.)

## 3. Run setup (PC, elevated terminal)

```powershell
cd C:\dev\burrow
$s = Read-Host "Tailscale API token" -AsSecureString
$env:TS_API_KEY = [Runtime.InteropServices.Marshal]::PtrToStringAuto([Runtime.InteropServices.Marshal]::SecureStringToBSTR($s))
.\setup\run-all.ps1 -WhatIf -RemoteDesktop rdp    # read the plan; changes nothing
.\setup\run-all.ps1 -RemoteDesktop rdp            # the real run
```

`-RemoteDesktop` options:

| Value | Use it when |
|---|---|
| `rdp` | Windows **Pro/Enterprise** — built-in Remote Desktop, laptop-only firewall rule |
| `sunshine` | Windows **Home** — RDP is unavailable; installs Sunshine (pair with Moonlight) |
| `none` | You don't want remote desktop at all |

Add `-KeepAutogroupSelf` if you already have other devices on your tailnet that need to
reach each other. Resume an interrupted run with `-From <phase>` (phases are numbered
0–12; see `docs/06-SETUP-RUNBOOK.md`).

The script stops and waits at each step below. Do the phone part, come back, press Enter.
It asks **y/N** before anything that could affect what you already use (replacing an
existing tailnet policy, disabling LAN file sharing, the account-lockout policy) — read
the two lines it shows before answering.

On the PC it will ask you for, in this order:

| Phase | What you do on the PC |
|---|---|
| 3 | Maybe one click: Tailscale admin → DNS → **Enable HTTPS** (if the API can't) |
| 5 | A window shows the **file-share password once** (big text + QR). Save it to iPhone Passwords by scanning the QR — it is never stored anywhere else |
| 6 | In a normal terminal: `cd C:\dev\burrow`, `claude remote-control`, answer **y**, press space, scan the QR with the iPhone, Ctrl+C. Then `claude` → `/config` → turn on **Push when actions required** and **Push when Claude decides** |
| 7 | The Windows credential prompt: type your **Windows password** (it goes straight to Windows) |
| 9 | A window shows a QR code + 6-digit code, once per device |
| 10 | The PC reboots into firmware setup. Find the **"restore on AC power loss"** setting and set it to **Always On / Power On / Last State** (names vary by vendor — e.g. ASUS: *APM → Restore AC Power Loss*; Gigabyte: *Settings → Platform Power → AC BACK*; MSI: *Settings → Advanced → Power Management Setup*), save. After Windows starts, resume: `.\setup\run-all.ps1 -From 10 -RemoteDesktop rdp` (set `TS_API_KEY` again first) |
| 11 | Router: check **UPnP is off** and there are **no port forwards** to the PC. Then revoke the API token (Admin console → Settings → Keys) |

---

## 4. iPhone — what to set

Do each block when the script pauses for it.

**When Phase 4 pauses ("install Tailscale on your iPhone")**
1. App Store → **Tailscale** → sign in with the **same** account as the PC.
2. Tailscale → Settings → **VPN On Demand** → on, for Wi-Fi **and** cellular.
3. Install an SSH client (**Termius** is what the docs assume), the **Claude** app, and
   your **smart plug's app**.
4. The PC asks you to confirm the new device is yours — check the name, answer y.

**When Phase 6 pauses**
5. claude.ai → Settings → Account → **Require trusted devices** → on; enroll the iPhone
   in the Claude app when asked.

**When Phase 8 pauses (smart plug)**
6. Wiring: wall → (UPS if any) → **smart plug** → PC. The plug must be the last thing
   before the PC.
7. Plug app: add the plug (2.4 GHz Wi-Fi), check it switches **on mobile data**, turn on
   **2FA** for the plug account, set *power-on after outage* to **Last state**, name it
   **PC Plug**.
8. Shortcuts app — make three, names exactly:
   - **PC On** → turn PC Plug on
   - **PC Off** → turn PC Plug off
   - **PC Power Cycle** → turn PC Plug off → Wait 15 s → turn PC Plug on

**When Phase 9 pauses (pairing) — iPhone first**
9. Termius → Keychain → **+** → Generate Key → **ED25519** → name it **Burrow** → copy the
   public key. Termius Settings → turn on **app lock with Face ID**.
10. Camera → scan the QR on the PC screen → Safari opens **Pair this device** → type the
    6-digit code → **Create passkey** (Face ID) → paste the Termius public key.
11. Safari → Share → **Add to Home Screen**. Open Burrow from the Home Screen and unlock
    with Face ID.
12. Files app → Browse → **⋯** → **Connect to Server** → `smb://<pc name the script shows>`
    → Registered User → `burrowfs` + the password you saved in Phase 5.
13. Termius → New Host → the same PC name, your Windows username, key **Burrow** →
    connect once, accept the host key.

**Phase 10 wake test (after the firmware change)**
14. Burrow → More → **Shut down** (Face ID) → wait for *PC is off — Cut plug power?* →
    **Turn plug off** → wait 15 s → Home → **Wake PC**. It should reach **Online** within
    3 minutes without anyone logging in.
15. Once more with the plug left on after shutdown: after 90 s **Power-cycle** appears;
    try it.

---

## 5. Laptop — what to set

1. Install Tailscale from <https://tailscale.com/download>, same account, start at login
   (Phase 4).
2. Phase 9: open the pairing URL the PC shows → code → **Create passkey** (Windows Hello /
   Touch ID) → install it as an app.
3. Burrow → Files → Downloads → `Burrow-Laptop` → download `README.txt` and
   `setup-windows.ps1` (or `setup-macos.sh`) and run it with the command in the README —
   the PC name and user are already filled in. It makes the SSH key, the `burrow-pc` SSH
   entry and the drive mappings, and prints a public key — add it in Burrow → More →
   Security → **Add SSH key**.
4. Test: `ssh burrow-pc`; VS Code Remote-SSH to `burrow-pc`; remote desktop to the PC
   (Remote Desktop / Windows App, or Moonlight for Sunshine).

---

## 6. Verify

```powershell
.\setup\12-verify.ps1
burrow selftest
burrow audit verify
```

`docs/07-TEST-PLAN.md` §5 is the definition of done. Setup writes `SETUP-REPORT.md` in
the repo root.

---

## Lost a device?

1. <https://login.tailscale.com> → remove that device. This cuts it off instantly; the
   other device keeps working.
2. claude.ai → Settings → Trusted devices → remove it.
3. At the PC: `burrow devices revoke <name>`, then `.\setup\rotate-secrets.ps1` (new
   file-share password, removes that device's SSH key).

## Undo everything

```powershell
.\setup\uninstall.ps1 -WhatIf
.\setup\uninstall.ps1
```

It reverses what setup changed, using the records in
`C:\ProgramData\Burrow\setup-state.json`.
