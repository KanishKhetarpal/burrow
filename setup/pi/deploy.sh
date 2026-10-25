#!/usr/bin/env bash
#
# Deploy the Burrow waker to a Raspberry Pi.
#
# Not part of v1. Run this ON THE PI (or pipe it over ssh) after copying the
# cross-compiled binary across:
#
#   scp dist/burrow-waker-linux-arm64 pi@<host>:/tmp/burrow-waker
#   scp setup/pi/deploy.sh setup/pi/burrow-waker.service pi@<host>:/tmp/
#   ssh pi@<host> 'sudo bash /tmp/deploy.sh'
#
# The tailnet auth key is read from TS_AUTHKEY in the environment and is never
# written to disk.

set -euo pipefail

BIN_SRC="${BIN_SRC:-/tmp/burrow-waker}"
UNIT_SRC="${UNIT_SRC:-/tmp/burrow-waker.service}"
BIN_DST="/usr/local/bin/burrow-waker"
CONF_DIR="/etc/burrow-waker"
CONF="$CONF_DIR/waker.yaml"

say()  { printf '  %s\n' "$*"; }
step() { printf '\n==> %s\n' "$*"; }
die()  { printf '\n  ERROR: %s\n' "$*" >&2; exit 1; }

[[ $EUID -eq 0 ]] || die "run this with sudo"
[[ -f "$BIN_SRC" ]] || die "no binary at $BIN_SRC - copy it over first"
[[ -f "$UNIT_SRC" ]] || die "no unit file at $UNIT_SRC"

step "Installing the binary"
install -m 0755 -o root -g root "$BIN_SRC" "$BIN_DST"
say "installed $BIN_DST"

step "Configuration"
mkdir -p "$CONF_DIR"
chmod 0755 "$CONF_DIR"

if [[ -f "$CONF" ]]; then
  say "$CONF already exists; leaving it alone"
else
  # Random, non-identifying: this name appears in public Certificate
  # Transparency logs (docs/03-SECURITY.md T15).
  RANDOM_HOST="n-$(head -c4 /dev/urandom | od -An -tx1 | tr -d ' \n')"

  read -rp "  PC MAC address (aa:bb:cc:dd:ee:ff): " PC_MAC
  read -rp "  LAN prefix (e.g. 192.168.1.0/24): " LAN_PREFIX
  read -rp "  PC LAN address, for the booting probe (e.g. 192.168.1.50): " PC_IP
  read -rp "  Agent origin (https://n-xxxx.tailXXXX.ts.net): " AGENT_ORIGIN
  read -rp "  iPhone node id (from 'tailscale status --json' on the PC): " PHONE_NODE

  cat > "$CONF" <<CONF_EOF
# Burrow waker configuration.
hostname: "$RANDOM_HOST"
state_dir: "/var/lib/burrow-waker"
owner_login: ""

# Only the iPhone. The waker is the one thing reachable while the PC is off,
# so its allowlist is narrower than the agent's.
allowed_node_ids:
  - "$PHONE_NODE"

# Used for the CORS header and the health probe.
agent_origin: "$AGENT_ORIGIN"

pc:
  mac: "$PC_MAC"
  lan_prefix: "$LAN_PREFIX"
  probe_addr: "$PC_IP:135"
CONF_EOF

  chmod 0644 "$CONF"
  say "wrote $CONF"
fi

step "systemd unit"
install -m 0644 -o root -g root "$UNIT_SRC" /etc/systemd/system/burrow-waker.service
systemctl daemon-reload
say "installed and reloaded"

step "Starting"
if [[ -n "${TS_AUTHKEY:-}" ]]; then
  # Passed through a drop-in that is deleted straight after the first start,
  # so the key does not persist in the unit.
  mkdir -p /etc/systemd/system/burrow-waker.service.d
  cat > /etc/systemd/system/burrow-waker.service.d/authkey.conf <<DROPIN
[Service]
Environment=TS_AUTHKEY=$TS_AUTHKEY
DROPIN
  chmod 0600 /etc/systemd/system/burrow-waker.service.d/authkey.conf
  systemctl daemon-reload
  systemctl enable --now burrow-waker
  sleep 8
  rm -f /etc/systemd/system/burrow-waker.service.d/authkey.conf
  systemctl daemon-reload
  say "removed the auth key drop-in; the node state is saved under /var/lib/burrow-waker"
else
  say "TS_AUTHKEY is not set - starting without it."
  say "The waker will print a URL to authorise the node; watch:"
  say "  journalctl -u burrow-waker -f"
  systemctl enable --now burrow-waker
fi

step "Status"
systemctl --no-pager status burrow-waker || true

printf '\n'
say "Next:"
say "  1. Tag this node tag:burrow-waker at login.tailscale.com -> Machines."
say "  2. On the PC, set wake.method: waker and wake.waker_url in burrow.yaml,"
say "     then restart the Burrow service."
say "  3. Check:  journalctl -u burrow-waker -f"
