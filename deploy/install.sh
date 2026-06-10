#!/bin/sh
# Turnkey installer for the Live Support single-binary deployment.
# Run as root from inside the unpacked release directory:  sudo ./install.sh
#
# Config lives in one env file (/etc/live-support/live-support.env), read by
# systemd. It holds the Cloudflare TURN secrets, so it is kept root-private.
set -eu

BIN=/usr/local/bin/live-support
CONF=/etc/live-support
DATA=/var/lib/live-support
UNIT=/etc/systemd/system/live-support.service

if [ "$(id -u)" -ne 0 ]; then
  echo "Please run as root (sudo ./install.sh)" >&2
  exit 1
fi

# service user/group
NOLOGIN=$(command -v nologin || echo /sbin/nologin)
getent group live-support >/dev/null 2>&1 || groupadd --system live-support
id live-support >/dev/null 2>&1 || \
  useradd --system -g live-support --no-create-home --shell "$NOLOGIN" live-support

# binary
install -m 0755 ./live-support "$BIN"

# directories
mkdir -p "$CONF" "$DATA"
chmod 0755 "$CONF"
chown live-support:live-support "$DATA"
chmod 0750 "$DATA"

# env file — read by systemd (root) only; holds Cloudflare TURN secrets, so
# keep it root-private.
if [ ! -f "$CONF/live-support.env" ]; then
  [ -f ./live-support.env ] && SRC=./live-support.env || SRC=./.env.example
  install -m 0600 -o root -g root "$SRC" "$CONF/live-support.env"
  echo "wrote $CONF/live-support.env  ($SRC)"
fi

# systemd unit
install -m 0644 ./live-support.service "$UNIT"
systemctl daemon-reload
# If the service is already running (i.e. this is an update), apply the new
# binary now. No-op on a first install (service not yet started).
systemctl try-restart live-support >/dev/null 2>&1 || true

# auto-updater (when bundled): installs the updater + nightly timer
AUTOUPDATE_NOTE=""
if [ -f ./update.sh ] && [ -f ./live-support-update.service ] && [ -f ./live-support-update.timer ]; then
  install -m 0755 ./update.sh /usr/local/bin/live-support-update
  install -m 0644 ./live-support-update.service /etc/systemd/system/live-support-update.service
  install -m 0644 ./live-support-update.timer /etc/systemd/system/live-support-update.timer
  systemctl daemon-reload
  if [ "${LIVESUPPORT_AUTOUPDATE:-}" = "1" ]; then
    systemctl enable --now live-support-update.timer >/dev/null 2>&1 || true
    AUTOUPDATE_NOTE="nightly auto-update ENABLED — disable with: systemctl disable --now live-support-update.timer"
  else
    AUTOUPDATE_NOTE="update anytime with: sudo live-support-update   (automate nightly: systemctl enable --now live-support-update.timer)"
  fi
fi

echo
echo "Installed. Next:"
echo "  1. edit $CONF/live-support.env — set ADMIN_USERNAME, the Cloudflare TURN"
echo "     credentials, and SECURE_COOKIES=true (you serve over HTTPS via the proxy)"
echo "  2. systemctl enable --now live-support"
echo "  3. curl -fsS http://127.0.0.1:8000/healthz   # -> ok"
echo "  4. front this host with a TLS reverse proxy (see Caddyfile.example /"
echo "     nginx-live-support.conf.example) on your subdomain"
[ -n "$AUTOUPDATE_NOTE" ] && echo "  * $AUTOUPDATE_NOTE"
