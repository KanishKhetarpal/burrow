#!/usr/bin/env bash
#
# Set up the laptop side of Burrow, on macOS.
#
# Run this ON YOUR LAPTOP, not on the PC. It writes the SSH config entry,
# mounts the shares and points you at the web app.
#
# Nothing here needs sudo.
#
# Usage:
#   ./laptop/setup-macos.sh <pc-magicdns-name> <pc-username>

set -euo pipefail

PC_NAME="${1:-}"
PC_USER="${2:-}"
KEY_PATH="${KEY_PATH:-$HOME/.ssh/burrow_ed25519}"
SHARES=("Documents" "Downloads")

say()  { printf '  %s\n' "$*"; }
step() { printf '\n==> %s\n' "$*"; }
warn() { printf '  \033[33m%s\033[0m\n' "$*"; }
ok()   { printf '  \033[32m%s\033[0m\n' "$*"; }

if [[ -z "$PC_NAME" || -z "$PC_USER" ]]; then
  cat <<USAGE
Usage: $0 <pc-magicdns-name> <pc-username>

  pc-magicdns-name   from \`burrow status\` on the PC, e.g. mypc.tail1a2b.ts.net
  pc-username        your Windows username on the PC
USAGE
  exit 2
fi

printf '\n  Burrow - laptop setup (macOS)\n'
printf '  -----------------------------\n'

# ------------------------------------------------------------- Tailscale ---

step "Tailscale"
TS=""
for candidate in /Applications/Tailscale.app/Contents/MacOS/Tailscale "$(command -v tailscale || true)"; do
  if [[ -n "$candidate" && -x "$candidate" ]]; then TS="$candidate"; break; fi
done

if [[ -z "$TS" ]]; then
  warn "Tailscale is not installed."
  warn "Install it from the App Store or https://tailscale.com/download,"
  warn "sign in with the same account as the PC, then re-run this."
  exit 1
fi
ok "found $TS"

step "Checking the PC is reachable at $PC_NAME"
if ping -c 1 -W 2000 "$PC_NAME" >/dev/null 2>&1; then
  ok "reachable"
else
  warn "cannot reach $PC_NAME - check Tailscale is connected and the PC is on."
  warn "Continuing anyway; the config below is still worth writing."
fi

# --------------------------------------------------------------- SSH key ---

step "SSH key"
if [[ -f "$KEY_PATH" ]]; then
  ok "already have $KEY_PATH"
else
  say "Generating an ed25519 key. Give it a passphrase - it is what protects"
  say "shell access to your PC if this laptop is taken."
  mkdir -p "$(dirname "$KEY_PATH")"
  ssh-keygen -t ed25519 -f "$KEY_PATH" -C "burrow-laptop-$(scutil --get ComputerName 2>/dev/null || hostname)"
fi

# The passphrase goes into the Keychain, so it is typed once rather than on
# every connection.
if [[ -f "$KEY_PATH" ]]; then
  ssh-add --apple-use-keychain "$KEY_PATH" 2>/dev/null \
    || ssh-add -K "$KEY_PATH" 2>/dev/null \
    || warn "could not add the key to the Keychain; you will be asked for the passphrase each time"
fi

if [[ -f "$KEY_PATH.pub" ]]; then
  printf '\n'
  say "Your public key - paste this into Burrow (More -> Security -> Add SSH key):"
  printf '\n    \033[32m%s\033[0m\n\n' "$(cat "$KEY_PATH.pub")"
  if command -v pbcopy >/dev/null; then
    tr -d '\n' < "$KEY_PATH.pub" | pbcopy
    ok "copied to the clipboard"
  fi
fi

# ------------------------------------------------------------ ssh config ---

step "SSH config entry"
SSH_CONFIG="$HOME/.ssh/config"
mkdir -p "$(dirname "$SSH_CONFIG")"
touch "$SSH_CONFIG"
chmod 600 "$SSH_CONFIG"

if grep -qE '^Host[[:space:]]+burrow-pc[[:space:]]*$' "$SSH_CONFIG"; then
  ok "a burrow-pc entry already exists; leaving it alone"
else
  cat >> "$SSH_CONFIG" <<ENTRY

# Added by Burrow laptop setup
Host burrow-pc
    HostName $PC_NAME
    User $PC_USER
    IdentityFile $KEY_PATH
    IdentitiesOnly yes
    PreferredAuthentications publickey
    ServerAliveInterval 60
    UseKeychain yes
    AddKeysToAgent yes
ENTRY
  ok "added 'burrow-pc' to $SSH_CONFIG"
fi

printf '\n'
say "You can now use:"
say "    ssh burrow-pc"
say "    VS Code -> Remote-SSH: Connect to Host -> burrow-pc"

# ---------------------------------------------------------------- shares ---

step "Mounting the shares"
say "These ask for the burrowfs password - the one shown once during PC setup"
say "and saved as \"Burrow Files\". Tick \"Remember this password\" in the dialog."

for share in "${SHARES[@]}"; do
  mount_point="/Volumes/$share"
  if mount | grep -q " on $mount_point "; then
    ok "$share is already mounted"
    continue
  fi
  say "opening smb://burrowfs@$PC_NAME/$share"
  # Finder's own mount dialog handles the Keychain prompt properly, which a
  # command-line mount_smbfs does not.
  open "smb://burrowfs@$PC_NAME/$share" || warn "could not open $share"
  sleep 2
done

printf '\n'
say "To mount these automatically at login:"
say "  System Settings -> General -> Login Items -> + -> pick the mounted volume"

# ------------------------------------------------------------- the app ---

step "The Burrow web app"
printf '  Agent address from `burrow status` on the PC (blank to skip): '
read -r AGENT_FQDN
if [[ -n "$AGENT_FQDN" ]]; then
  AGENT_FQDN="${AGENT_FQDN#https://}"
  open "https://$AGENT_FQDN"
  printf '\n'
  say "In Safari: unlock with Touch ID, then File -> Add to Dock."
fi

# --------------------------------------------------------- remote desktop ---

step "Remote desktop (only if you enabled it on the PC)"
say "Windows App (RDP):   install from the Mac App Store, add PC $PC_NAME"
say "Moonlight (Sunshine): https://moonlight-stream.org, add PC by $PC_NAME"

printf '\n\033[32m  Done.\033[0m\n'
say "Wake still comes from your iPhone - the laptop cannot run the Shortcut."
