# Live Support

A self-hosted, single-binary live audio/video support server. Guests reach a live agent straight from a web page — no login, no third-party calling service — over peer-to-peer WebRTC, backed by an embedded database and a built-in realtime hub.

Live Support is **fully self-contained**: agents sign in with a built-in username/password system — argon2id password hashing, server-side sessions, CSRF protection, invite-based user creation, and a per-tenant user-management page — with no external identity provider or vendor service required. Updated — June 2026.

## Screenshots

|  |  |
| --- | --- |
| **Guest — reach a live agent** | **Agent — incoming call** |
| ![Guest page](docs/screenshots/guest.png) | ![Incoming call](docs/screenshots/incoming.png) |
| **Agent dashboard** | **User management** |
| ![Agent dashboard](docs/screenshots/agent-dashboard.png) | ![User management](docs/screenshots/user-management.png) |

## 1. Overview

### 1.1 Guest Page (index.html)

A standalone web page that guests land on when they want to reach a live agent. No login is required. The page is scoped to a specific client/tenant using a `?ref=` parameter in the URL — for example `?ref=example.com`.

- Modern, mobile-first caller modal — a centered card with the brand header; designed to drop into an in-page widget or iframe as well as a full page.
- Greeting screen with the guest's name, read from the URL.
- Live agent detection — call buttons appear only when an agent is online; the Video button enables only when a video-capable agent is available. If no agents are available the page goes straight to the message form.
- Audio and video calling — full peer-to-peer WebRTC with optional Cloudflare TURN for NAT traversal.
- Microphone / camera pickers — choose which input device to use (like Zoom/Meet), persisted per-browser and hot-swappable mid-call via `replaceTrack`.
- Incoming call support — guests can also receive calls initiated by an agent.
- Message form — name, email, and message fields. Shown when no agents are online or when a call attempt times out after 30 seconds (long enough for a push-notified agent to answer — see §1.9).

### 1.2 Agent Dashboard (auth.html)

A separate page for authenticated agents and admins. Access requires a signed-in session — visitors without one are redirected to the sign-in page (`/login`).

- Modern centered hub — a clean, mobile-first card layout. On a desktop viewport the History tables spread to the full width (no horizontal scroll); on mobile / embedded sizes they scroll inside the card. Admins land on the user-management page (`/users`) after signing in; agents land on the console.
- Session authentication on load — the page asks the server for the signed-in identity (`/api/me`), normalized as `{ ref, name, email, isAdmin }`. The tenant (`ref`) and admin status come from the server-side session, never from URL parameters (except the explicit, server-gated dev bypass). See [§1.7 Authentication](#17-authentication-built-in).
- Availability toggle — agents start **Paused** and explicitly go **Available** to take calls. Browser microphone/camera permission is **deferred** to that moment, and only for the chosen modes: mic always, camera only when "Accept video calls" is on. Pausing releases the devices.
- Microphone / camera pickers — pick the input devices to use; the stream acquired at Go-Available is reused by the call (no second permission prompt), and a mid-call device change hot-swaps the track into the live connection.
- User management (admin only) — admins can open `/users` to invite new users (single-use invite links), issue password-reset links, and deactivate or delete accounts. See §1.7.
- Live guest list — all guests currently online for this tenant, with a real-time wait-time counter and camera availability indicator.
- Click-to-call — agents start an audio or video call directly from the guest list.
- Incoming call handling — if a guest calls while the agent is busy, the system automatically declines and routes the guest to the message form.
- Calls log — paginated table of every call: caller, callee, duration, and outcome.
- Messages log — paginated table of every message left by a guest: name, contact info, message body, and timestamp.
- Sessions log (admin only) — full history of all connected sessions, split into guest sessions and admin sessions tables.
- Admin controls — admins see delete buttons on every log entry and a Delete All option per table (including the admin sessions table). Non-admins see logs but cannot delete.

### 1.3 Opening the Agent Dashboard

Agents sign in at the tenant's login page and are taken to the dashboard:

```
https://your-connect-host/login?ref=example.com
```

(Visiting `/auth.html` without a session redirects there automatically; `/login` without a `?ref=` shows a domain field.) For local development, a server-gated dev bypass lets you open the dashboard with identity supplied directly in the URL — no sign-in required:

```
auth.html?dev=true&ref=example.com&name=Agent&admin=true
```

The dev bypass only works when the server is started with `DEV_MODE=true`.

### 1.4 Real-Time Infrastructure

All real-time communication between guests and agents runs through a WebSocket hub built directly into the connect server — no polling, no external realtime service, no third-party calling service. The hub replicates channel, presence, and broadcast semantics in-process.

| | |
|---|---|
| Presence | Every connected user joins a shared presence channel scoped to the tenant's ref. Status updates instantly when anyone connects or disconnects, and clients automatically re-register their presence after a reconnect. |
| Call signaling | WebRTC offer/answer and ICE candidates are exchanged through per-call broadcast channels. Call initiation (ring, accept, decline, busy, hangup) uses point-to-point inbox channels. |
| Instant messaging | Lightweight text chat over the same point-to-point inbox channels. See §1.8. |
| Dashboard updates | The server broadcasts a refresh event to the agent dashboard after every database write, so logs stay live without polling. |
| Heartbeat | Every session sends a keep-alive every 30 seconds. Sessions that stop heartbeating are marked offline automatically. |
| TURN server | Optional Cloudflare TURN credentials are fetched server-side and provided to the WebRTC peer connection, enabling calls through NAT and firewalls. Without credentials the app falls back to public STUN servers. |

### 1.5 Database

All persistent data lives in embedded SQLite — no database server to install or manage. **Storage is multi-tenant: each tenant (ref) gets its own SQLite database file**, created automatically in the `data/` directory the first time that ref is used (e.g. `data/example.com.db`). Tenant isolation is physical — a tenant's data lives in its own file, so data can never be shared or leaked between tenants, and deleting, exporting, or backing up a single tenant is a one-file operation.

Each tenant database contains the app tables plus the auth tables:

| Table | Purpose | Key Fields |
|---|---|---|
| sessions | One row per connected user session. | session_id, ref, name, role, status, has_mic, has_camera, logged_in_at, last_seen_at |
| calls | One row per call attempt. | call_id, ref, caller, callee, type (audio/video), started_at, duration, status |
| messages | One row per guest message. | message_id, ref, name, contact, message, created_at |
| users | One row per agent/admin account (per-tenant logins). | username, password_hash (argon2id), role (admin/agent), must_change_pw, active |
| auth_sessions | Server-side login sessions (hashed tokens at rest). | id (sha256 of token), user_id, csrf_token, expires_at |
| invites | Single-use invitation links for creating accounts. | token_hash, role, username, created_by, expires_at, redeemed_at |
| password_resets | One-time, admin-issued password-reset links. | token_hash, user_id, created_by, expires_at, used_at |
| push_subscriptions | Web Push endpoints for backgrounded-tab call delivery (§1.9). | user_id, session_id, endpoint, p256dh, auth |

### 1.6 Server

The entire application — web server, database, realtime hub, and frontend — ships as a single self-contained Go binary. The guest and agent pages are embedded inside the executable, the per-tenant SQLite databases are created next to it on first use, and optional Cloudflare TURN credential requests are proxied so the API secret stays on the server. Installing the application means copying one file to the server and running it.

- No runtime dependencies — no Node.js, no npm packages, no database server, no external realtime service, and no third-party accounts other than optional Cloudflare TURN.
- Cross-platform — the binary compiles for Linux, macOS, and Windows from the same source.
- REST API — sessions, calls, and messages are managed through a small JSON API used by both pages; an `/api/online` endpoint lets external pages check agent availability; `/api/me` reports the signed-in identity; `/api/connect-config` exposes deployment configuration (including the Web Push public key) to the frontend; and the Web Push endpoints (§1.9) deliver calls to backgrounded agents.

### 1.7 Authentication (Built-In)

Agents authenticate against the server itself — no external identity provider, JWT issuer, or vendor API. The implementation lives in `auth.go` and follows standard practice throughout:

- **Password storage** — argon2id (64 MiB, t=3, p=4) with a constant-time verify; unknown usernames are verified against a dummy hash so login timing can't enumerate accounts. Minimum password length is 12 characters.
- **Sessions** — opaque 256-bit cookie tokens, stored sha256-hashed in the tenant's `auth_sessions` table with a configurable TTL (`SESSION_TTL_HOURS`). Cookies are `HttpOnly`, `SameSite=Lax`, and `Secure` when `SECURE_COOKIES=true`.
- **CSRF** — double-submit cookie on pre-session forms (login, invite, forgot/reset) and a per-session synchronizer token on every authenticated form.
- **Throttling** — five failed logins per username locks that username out for five minutes.
- **Per-tenant users** — accounts live inside each tenant's own database file, so logins are scoped to the tenant (`ref`) and isolation stays physical.

**Sign-in flow** — agents visit `/login?ref=<tenant>` (or plain `/login` and type the domain). On success the dashboard loads and `auth.js` reads the identity from `/api/me`: `{ ref, name, email, isAdmin }`. Roles are `admin` (full dashboard, user management) and `agent`.

**First run (per tenant)** — the first time a tenant's login page is used, an initial admin account is created from `ADMIN_USERNAME` / `ADMIN_INITIAL_PASSWORD`. If no password is configured, a temporary one is generated and printed to the server log; it must be changed on first login.

**Platform SSO (optional)** — an external platform that knows the deployment's `CONNECT_SECRET` can mint short-lived signed links that sign a user straight into the dashboard, no second login:

```
https://your-connect-host/sso?t=<token>
```

The token is `base64url( nonce(12) + AES-256-GCM(key, "ref|unixSeconds|username|role") )` with `key = SHA-256(CONNECT_SECRET)` — the same opaque-token recipe the csat project uses for its survey links. Links expire after 10 minutes. Unknown usernames are auto-provisioned in the tenant (password-less — they can only enter via SSO until an admin issues a reset link). Canonical link-builder (Node.js, for the platform side):

```js
const crypto = require("crypto");

function buildSSOLink(connectUrl, secret, ref, username, isAdmin) {
  const key = crypto.createHash("sha256").update(secret).digest();
  const nonce = crypto.randomBytes(12);
  const cipher = crypto.createCipheriv("aes-256-gcm", key, nonce);
  const payload = `${ref}|${Math.floor(Date.now() / 1000)}|${username}|${isAdmin ? "admin" : "agent"}`;
  const ct = Buffer.concat([cipher.update(payload, "utf8"), cipher.final(), cipher.getAuthTag()]);
  return `${connectUrl}/sso?t=${Buffer.concat([nonce, ct]).toString("base64url")}`;
}
```

`CONNECT_SECRET` is generated on first run (appended to `.env` and printed to the log) if not set. None of the fields may contain `|`.

**User management (`/users`, admin only)** — the dashboard's user page lets admins:

- **Invite users** — mints a single-use, expiring invite link (`INVITE_TTL_HOURS`) for the `agent` or `admin` role, optionally pre-locking the username. The invitee opens the link and chooses their own password; the password never passes through the admin.
- **Reset passwords** — mints a one-time reset link (`RESET_TTL_HOURS`). Redeeming it sets the new password, reactivates the account if needed, and revokes all of the user's live sessions. Users can also file a "forgot password" request from the login page, which flags their row on the users page.
- **Deactivate / delete accounts** — with guard rails: you can't deactivate or delete yourself, or the last admin.

### 1.8 Instant Messaging

Agents and guests who are online at the same time can exchange short text messages, independent of audio/video calls. IM is **ephemeral** — threads live only in the browser while both parties are connected and are cleared on refresh. Nothing is written to the database (it is intentionally not one of the logged tables), and there is no schema change: messages ride the same in-process point-to-point inbox channels used for call invitations.

The visibility model is deliberately asymmetric:

- **Agents initiate.** The agent dashboard shows a roster of everyone online for the tenant — both guests and other agents — and an agent can open a separate conversation with any of them, holding multiple threads at once.
- **Guests only reply.** The guest page has no roster and cannot see or message agents on its own. A guest's chat panel stays hidden until an agent messages them first, and a guest can only ever reply to an agent who has already messaged them.

This asymmetry is enforced by the transport itself: a chat message is delivered to the recipient's own inbox channel (keyed by their session id), so a guest only ever learns an agent's address because that agent messaged them — the guest is never sent the agent roster and cannot enumerate or cold-message agents.

### 1.9 Web Push (backgrounded-browser call delivery)

A WebSocket can't ring an agent whose console tab is backgrounded, minimised, or closed — browsers throttle and suspend background tabs. **Web Push** closes that gap: when an agent goes Available the console subscribes to push (VAPID); when a guest calls, the server sends an encrypted push that **wakes the agent's service worker** and rings them with an OS notification even when no tab is in front. Clicking the notification focuses (or re-opens) the console, which re-hydrates the still-ringing call from a short-lived pending-invite lookup so the agent can answer.

Push is **additive**: the in-process inbox-over-WebSocket relay stays the instant path when the tab is open; push is the fallback when it isn't. With no VAPID keys configured, or notification permission not granted, the app behaves exactly as before. Mechanics:

- **VAPID** — set `VAPID_PUBLIC_KEY` / `VAPID_PRIVATE_KEY` (+ optional `VAPID_SUBJECT`); generate a keypair with `live-support -genvapid`. The public key is exposed to the client via `/api/connect-config`; the private key never leaves the server.
- **Service worker + PWA** — `static/sw.js` (scope `/`) shows the notification and handles the click; `manifest.webmanifest` makes the console installable.
- **Subscriptions** — stored per-tenant in `push_subscriptions`, tied to the authenticated user; dead endpoints (HTTP 404/410) are pruned automatically.
- **Endpoints** — `POST /api/push/{subscribe,unsubscribe}` (authenticated), `POST /api/call/ring` + `/api/call/ring/clear` (called by the guest on ring/cancel), `GET /api/call/pending` (the console's re-hydration check).

> This desktop/PWA tier covers a backgrounded or closed *browser*. A fully asleep phone still needs a native app with APNs/FCM — a later phase that the push model here de-risks.

## 2. Deployment

Deployment is a copy-and-run operation: build (or download) the binary, place it on any host, and start it. A $5/month VPS, an on-premise machine, or a container platform all work — anything that can run a single Linux process.

| | |
|---|---|
| Distribution | One executable (live-support). Frontend files — including any auth-provider modules — are embedded; the `data/` directory of per-tenant SQLite databases is created automatically on first start. |
| Configuration | A systemd `EnvironmentFile` in production, or a `.env` file beside the binary in development. See §2.1. |
| HTTPS | Browsers require HTTPS for microphone and camera access on any non-localhost address. Run the binary behind a TLS-terminating reverse proxy such as Caddy (automatic Let's Encrypt certificates) or nginx. WebSocket traffic upgrades automatically to wss: on HTTPS pages. |
| Topology | Single instance by design. Presence and call signaling live in process memory and each SQLite database uses one writer, so the application must run as exactly one replica. This matches the product's scale and keeps operations trivial. |
| Backups | Backing up the deployment means copying the `data/` folder. Backing up (or removing) a single tenant means copying (or deleting) that tenant's one database file. No dump tooling or managed-database export is involved. |
| Updating | `sudo live-support-update` (latest stable within the tracked major), or stop / replace-binary / start. Schema is created and migrated automatically at startup. See §2.4. |

### 2.1 Configuration

All settings come from environment variables or a `.env` file beside the binary.

| Variable | Default | Purpose |
|---|---|---|
| `PORT` | `8000` | HTTP listen port. |
| `BIND_ADDR` | `127.0.0.1` | Address to bind. Loopback-only by default so only the reverse proxy reaches the app; set `0.0.0.0` for direct access. |
| `DATA_DIR` | `data` | Directory holding one SQLite database per tenant ref (the systemd unit sets it to `/var/lib/live-support`). |
| `ADMIN_USERNAME` | `admin` | Username of the initial admin account created the first time a tenant is used. See §1.7. |
| `ADMIN_INITIAL_PASSWORD` | *(empty)* | Password for that initial admin. Empty = a temporary password is generated and printed to the server log (must be changed on first login). |
| `SESSION_TTL_HOURS` | `168` | Login session lifetime. |
| `INVITE_TTL_HOURS` | `72` | Invite-link lifetime. |
| `RESET_TTL_HOURS` | `24` | Password-reset-link lifetime. |
| `SECURE_COOKIES` | `false` | Set `true` when serving over HTTPS so cookies are marked `Secure`. |
| `CONNECT_SECRET` | *(generated)* | Shared secret for platform sign-in SSO links (`/sso`). Auto-generated, persisted under `DATA_DIR` (`data/connect_secret`), and logged on first run if unset; the config file is never modified. See §1.7. |
| `SITE_NAME` | `Live Support` | Name shown on the sign-in and user-management pages. |
| `PRIMARY_COLOR` | *(empty)* | Optional brand color applied to the UI at runtime (overrides the CSS `--primary` variable). Accepts any CSS color — hex (`#7646b9`), `rgb()/rgba()`, `hsl()/hsla()`, or a named color. Invalid or empty values fall back to the stylesheet default. |
| `FAVICON_URL` | *(empty)* | Optional favicon override applied at runtime. Accepts an absolute `http(s)://` URL or a same-origin path (`/...`). Invalid or empty values fall back to the bundled `/public/favicon.svg`. |
| `AGENT_PASSCODE` | *(empty)* | Optional one-time passcode for the agent dashboard (`auth.html`). When set, agents must enter this code once per browser before the dashboard loads; it is verified server-side and never sent to the page. Empty/unset disables the gate entirely. |
| `DEV_MODE` | `false` | Enables the agent-dashboard dev bypass (`?dev=true`). Never enable in production. |
| `CLOUDFLARE_TURN_TOKEN_ID` | *(empty)* | Optional Cloudflare TURN token ID. Without TURN credentials the app falls back to public STUN servers. |
| `CLOUDFLARE_API_TOKEN` | *(empty)* | Optional Cloudflare API token for TURN credential generation. |
| `VAPID_PUBLIC_KEY` | *(empty)* | Web Push public key (§1.9). Set together with `VAPID_PRIVATE_KEY` to enable push; both empty disables it. Generate a keypair with `live-support -genvapid`. |
| `VAPID_PRIVATE_KEY` | *(empty)* | Web Push private key — signs push requests; never sent to the browser. |
| `VAPID_SUBJECT` | `mailto:admin@localhost` | Contact (`mailto:`) included in push requests, per the Web Push spec. |

### 2.2 Install (Linux, one line)

A one-line installer downloads a verified release, installs the binary plus a hardened systemd unit, and sets up the auto-updater:

```bash
curl -fsSL https://raw.githubusercontent.com/anthonyicuracao/live-support/main/install.sh | sudo sh
```

It detects your OS/arch, verifies the release SHA-256, installs `/usr/local/bin/live-support`, creates the `live-support` service user, and writes the systemd unit + updater. Then:

```bash
sudoedit /etc/live-support/live-support.env   # set ADMIN_USERNAME, Cloudflare TURN creds, SECURE_COOKIES=true
sudo systemctl enable --now live-support
curl -fsS http://127.0.0.1:8000/healthz       # -> ok
```

On an EC2 instance (or any VPS): Amazon Linux 2023 / Ubuntu 24.04, a `t3.micro` (or `t4g.micro` for ARM), with inbound **22** (your IP only), **80**, and **443**. WebRTC media is peer-to-peer / relayed via Cloudflare TURN, so no UDP ports are needed — signaling rides the WebSocket on 443. Full reference: [`deploy/README.md`](deploy/README.md).

### 2.3 HTTPS reverse proxy (required — getUserMedia/WebRTC needs a secure origin)

Point a DNS A record for your subdomain at the host's public IP.

**Caddy (recommended)** — automatic Let's Encrypt certificates, transparent WebSocket proxying:

```bash
sudo dnf install caddy        # Ubuntu: sudo apt install caddy
```

`/etc/caddy/Caddyfile` (see [`deploy/Caddyfile.example`](deploy/Caddyfile.example)):

```
live-support.example.com {
    reverse_proxy localhost:8000
}
```

```bash
sudo systemctl enable --now caddy
```

Co-hosting with CSAT on the same box? Add a second block routing `csat.example.com` to its port — Caddy gets a certificate per subdomain automatically.

**nginx** is fully supported too — use [`deploy/nginx-live-support.conf.example`](deploy/nginx-live-support.conf.example) (it includes the WebSocket upgrade headers `/ws` requires) and `certbot --nginx`.

### 2.4 Updating

```bash
sudo live-support-update              # latest stable within the tracked major (v1)
```

It backs up every tenant DB to `/var/lib/live-support/backups/<timestamp>/` before swapping the binary and restarting. Automate nightly with `systemctl enable --now live-support-update.timer`.

### 2.5 Build from source

```bash
go build -o live-support .                                   # local
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o live-support .   # static Linux binary
```

The pure-Go SQLite driver makes `CGO_ENABLED=0` work — one fully static binary. `make package` produces a release tarball identical to the CI build.

### 2.6 Deployment checklist

- [ ] Host launched, firewall / security group: 22 (your IP), 80, 443
- [ ] `curl … install.sh | sudo sh` succeeded
- [ ] `/etc/live-support/live-support.env`: Cloudflare TURN creds + `SECURE_COOKIES=true`
- [ ] `systemctl enable --now live-support`, `/healthz` returns `ok`
- [ ] DNS A record → host IP
- [ ] Caddy (or nginx) running, HTTPS works
- [ ] Test: guest page loads, agent sign-in works (`/login?ref=...`), call connects

## 3. Integrating the Guest Page

To send guests to the live agent page, link to the guest page with the tenant's identifier as the `?ref=` parameter — for example from a website button, a help center, or a chatbot widget:

```
https://your-connect-host/?ref=example.com
```

If the guest's name is already known, pass it via `?name=` so the page greets them by name; otherwise a default is used:

```
https://your-connect-host/?ref=example.com&name=Jane
```

No changes to the guest page itself are required — it is ready to receive these links as-is. If the page is launched from a chatbot widget or other embedded integration, drop the tenant's domain into `?ref=` from wherever that integration exposes it.

## 4. Status Summary

| Component | Status | Notes |
|---|---|---|
| Guest page — calls, message form, full UI | ✓ Done | index.html — complete. |
| Agent dashboard — session auth, guest list, logs | ✓ Done | auth.html + auth.js — complete. |
| Built-in authentication + user management | ✓ Done | `/login`, `/users`, invites, password resets, per-tenant users (auth.go). |
| Audio and video calling (WebRTC + optional Cloudflare TURN) | ✓ Done | Complete. |
| Instant messaging (agent ↔ guest text chat) | ✓ Done | Ephemeral, over inbox channels. Agents initiate and see the full roster; guests can only reply. |
| Real-time presence and signaling (built-in WebSocket hub) | ✓ Done | Complete. |
| Web Push — backgrounded/closed-tab call delivery (§1.9) | ✓ Done | VAPID + service worker + PWA; pending-invite re-hydration on reopen. Falls back to the WS path when push is off. |
| Media UX — caller modal, Available/Pause, mic/camera pickers, mid-call switch | ✓ Done | Deferred, mode-aware permission; stream reuse (no re-prompt on accept); device pickers on both pages. |
| Database — embedded SQLite, multi-tenant | ✓ Done | One database file per tenant (ref) in `data/`, created automatically. App + auth tables per tenant (see §1.5). |
| Go server — single binary, embedded frontend, TURN proxy | ✓ Done | Complete. |
| Guest-page link from a chatbot/site | Integration | Link to `/?ref=<tenant>` from wherever guests start. |

## License

MIT — see [LICENSE](LICENSE). © 2026 Anthony Elshafei and Ron Pinkas.

