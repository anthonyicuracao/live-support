#!/bin/sh
# Live Support auto-updater (installed as /usr/local/bin/live-support-update,
# run by the live-support-update.timer). Updates the binary to the latest
# release within the tracked major version, backing up the databases first.
#
#   LIVESUPPORT_TRACK=v1   only update within this major line (default v1)
set -eu

REPO="anthonyicuracao/live-support"
TRACK="${LIVESUPPORT_TRACK:-v1}"
BIN="/usr/local/bin/live-support"
DATA="/var/lib/live-support"

log() { printf '[live-support-update] %s\n' "$*"; }
die() { printf '[live-support-update] error: %s\n' "$*" >&2; exit 1; }

case "$(uname -m)" in
  x86_64 | amd64)  goarch=amd64 ;;
  aarch64 | arm64) goarch=arm64 ;;
  *) die "unsupported architecture $(uname -m)" ;;
esac
asset="live-support-linux-${goarch}.tar.gz"

current=$("$BIN" -version 2>/dev/null || echo none)

# latest stable tag (GitHub excludes pre-releases from releases/latest)
latest=$(curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" \
  | sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' | head -1)
[ -n "$latest" ] || die "could not determine latest release"

# only auto-update within the tracked major (e.g. v1.*)
case "$latest" in
  "${TRACK}".*) ;;
  *) log "latest $latest is outside track $TRACK — skipping (upgrade manually)"; exit 0 ;;
esac

if [ "$latest" = "$current" ]; then
  log "already up to date ($current)"
  exit 0
fi
log "updating $current -> $latest"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
base="https://github.com/${REPO}/releases/download/${latest}"
curl -fsSL "${base}/${asset}" -o "$tmp/$asset" || die "download failed"
curl -fsSL "${base}/${asset}.sha256" -o "$tmp/$asset.sha256" || die "checksum download failed"
expected=$(awk '{print $1}' "$tmp/$asset.sha256")
actual=$(sha256sum "$tmp/$asset" | awk '{print $1}')
[ "$expected" = "$actual" ] || die "checksum mismatch"
tar xzf "$tmp/$asset" -C "$tmp"
newbin="$tmp/live-support-linux-${goarch}/live-support"
[ -x "$newbin" ] || die "binary missing in archive"

# back up the per-tenant databases before swapping (one SQLite file per ref)
if [ -d "$DATA" ]; then
  stamp=$(date +%Y%m%d-%H%M%S)
  dest="$DATA/backups/$stamp"
  found=0
  for db in "$DATA"/*.db; do
    [ -e "$db" ] || continue
    found=1
    mkdir -p "$dest"
    name=$(basename "$db")
    if command -v sqlite3 >/dev/null 2>&1; then
      sqlite3 "$db" ".backup '$dest/$name'" || cp "$db" "$dest/$name"
    else
      cp "$db" "$dest/$name"
    fi
  done
  [ "$found" = 1 ] && log "backed up tenant DBs to $dest"
fi

# atomic swap + restart (schema is created/migrated on boot)
install -m 0755 "$newbin" "${BIN}.new"
mv "${BIN}.new" "$BIN"
systemctl restart live-support
log "updated to $latest and restarted"
