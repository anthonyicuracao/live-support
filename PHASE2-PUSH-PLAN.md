# Phase 2 — Web Push + Service Worker (reliable backgrounded-browser call delivery)

Goal: an agent who is **Available** but whose tab is backgrounded / minimized / the
browser closed still gets *woken and rung* on an incoming call, and can answer it.
A WebSocket can't do this (background tabs are throttled/suspended); the OS push
service can. This is the browser/PWA tier; native APNs/FCM is Phase 3.

## Architecture

```
guest initiateCall ──► POST /api/call/ring {ref,toSession,callId,callType,callerName}
                          │
                          ├─ resolve toSession ─► push_subscriptions row ─► user_id + endpoint(s)
                          ├─ record pending invite (in-mem, key ref+user_id, TTL 30s)
                          └─ web-push (VAPID) ─► OS push service ─► agent SW wakes
                                                                      │
   SW push handler: show notification "Incoming <type> call from <name>" (+ vibrate)
   SW notificationclick: focus existing console OR openWindow(/auth.html?ref=…)
                                                                      │
   agent console on load ─► GET /api/call/pending ─► invite ─► show incoming UI + join call:<id>
                                                                      │
                          guest still ringing (30s) ─── accept ──► normal WebRTC flow
```

Push is **additive**: the existing inbox-over-WS relay stays as the instant path when
the tab is open. When the tab is closed, push is the delivery; the pending-invite
lookup re-hydrates the call on reopen. If push isn't granted/available, behaviour is
exactly as today (no regression).

Keying: the presence `sessions` table has no user_id, but the agent is authenticated.
The agent registers its push subscription with `POST /api/push/subscribe` (authed →
user_id from cookie) and passes its **current presence session_id** in the body. The
ring endpoint maps `toSession → subscription → user_id`, so pending invites are keyed
by user and survive the agent getting a *new* session_id on reopen.

## Backend (Go) — new `push.go`, edits to `main.go`

1. **VAPID config** from env: `VAPID_PUBLIC_KEY`, `VAPID_PRIVATE_KEY`, `VAPID_SUBJECT`
   (mailto:). If unset → push disabled gracefully (subscribe no-ops, ring skips push).
2. **Schema** (added to per-ref DB schema): `push_subscriptions(id, ref, user_id,
   session_id, endpoint UNIQUE, p256dh, auth, created_at, last_seen_at)` + indexes on
   (ref,session_id) and (ref,user_id).
3. **In-memory pending invites**: `map[callId]invite{ref,userID,fromName,callType,exp}`
   with mutex + 30s TTL; lookup by (ref,userID).
4. **Routes**:
   - `POST /api/push/subscribe`   (authedJSON) — upsert by endpoint.
   - `POST /api/push/unsubscribe` (authedJSON) — delete by endpoint.
   - `POST /api/call/ring`        (public, ref) — record invite + send push.
   - `POST /api/call/ring/clear`  (public, ref) — drop invite (guest cancel/timeout).
   - `GET  /api/call/pending`     (authedJSON) — invites for this user.
5. **web-push send** via `github.com/SherClockHolmes/webpush-go` (pure Go). 410/404
   from the push service → prune the dead subscription row.
6. Expose `vapidPublicKey` in `GET /api/connect-config`.

## Frontend — `static/sw.js`, `static/js/push.js`, edits to auth.js / index.html

7. **`sw.js`** (served at `/sw.js`, scope `/`): `push` → `showNotification`;
   `notificationclick` → focus an open client or `openWindow('/auth.html?ref=…')`.
8. **`push.js`** (`window.Push`): `registerServiceWorker()`, `enablePush(vapidKey,
   sessionId)` (permission → `pushManager.subscribe` → POST subscribe), `disablePush()`.
9. **auth.js**: on Go-Available, after notification permission, `Push.enablePush(...)`;
   on Pause, `Push.disablePush()`. On load, `GET /api/call/pending`; if an invite is
   live, render the existing incoming-call UI and join the call channel.
10. **guest.js**: after `sendToInbox`, `POST /api/call/ring`; on cancel/timeout, clear.
    Extend the ring window 10s → 30s so a push-woken agent can answer.
11. **`manifest.webmanifest`** (minimal) + link in auth.html → installable PWA
    (also the bridge to Phase 3).

## Testing on local Docker

- **Go tests** (`push_test.go`): subscription upsert/prune; ring → pending invite
  lookup by user; web-push request shape against a mock endpoint server.
- **Endpoint integration** (curl): subscribe (synthetic sub) → ring → GET pending
  returns the invite → clear removes it.
- **Playwright**: `/sw.js` registers + activates; Go-Available attempts push enable and
  degrades gracefully where the headless push service is absent; **pending-invite UI
  flow** — seed an invite via the authed subscribe + ring, reload console, assert the
  incoming-call UI appears (the answerable-after-wake path, fully testable w/o real push).
- **Real OS push delivery** (waking a closed tab) is inherently a real-browser check —
  verified post-deploy in Ron's browser, like the ring/device fixes.

## Deploy
Generate a VAPID keypair, add the 3 env vars to the appliance `.env`, release `v1.3`,
pull on the managed appliance.
