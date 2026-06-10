# Deploying Live Support

A single static binary plus a systemd unit. SQLite (one file per tenant) lives
in `/var/lib/live-support`. TLS is terminated by a reverse proxy in front.

## One-line install (Linux)

```sh
curl -fsSL https://raw.githubusercontent.com/anthonyicuracao/live-support/main/install.sh | sudo sh
```

This downloads the latest stable release, verifies its SHA-256, installs the
binary to `/usr/local/bin/live-support`, creates the `live-support` service user,
and installs the systemd unit + updater. Then:

1. Edit `/etc/live-support/live-support.env` — set `ADMIN_USERNAME`, the
   Cloudflare TURN credentials, and `SECURE_COOKIES=true` (you serve over HTTPS).
2. `systemctl enable --now live-support`
3. `curl -fsS http://127.0.0.1:8000/healthz` → `ok`
4. Front the host with a TLS reverse proxy on your subdomain (below).

The app binds `127.0.0.1` by default, so only the proxy can reach it.

## Reverse proxy

**Caddy (recommended)** — automatic Let's Encrypt certs, transparent WebSocket
proxying. See [`Caddyfile.example`](Caddyfile.example). For two products on one
box, route by subdomain:

```caddy
csat.example.com         { reverse_proxy localhost:8080 }
live-support.example.com { reverse_proxy localhost:8000 }
```

**nginx** — fully supported; see
[`nginx-live-support.conf.example`](nginx-live-support.conf.example) (it includes
the WebSocket upgrade headers `/ws` needs). Get a cert with `certbot --nginx`.

Open only **80** and **443** inbound. WebRTC media is peer-to-peer / relayed via
Cloudflare TURN — no UDP port ranges on this host.

## Updating

```sh
sudo live-support-update              # latest stable within the tracked major (v1)
```

It backs up every tenant DB under `/var/lib/live-support/backups/<timestamp>/`
before swapping the binary and restarting. Enable nightly auto-update with:

```sh
systemctl enable --now live-support-update.timer
```

## Releasing (maintainers)

Tag a version; the `release` workflow builds all platforms and publishes the
GitHub Release:

```sh
git tag v1.0.0 && git push --tags          # stable → becomes "latest"
git tag v1.1.0-rc1 && git push --tags      # hyphen → prerelease, excluded from "latest"
```

The installer/updater pull `releases/latest`, so prereleases never reach
unattended installs. The updater only moves within its tracked major
(`LIVESUPPORT_TRACK`, default `v1`).
