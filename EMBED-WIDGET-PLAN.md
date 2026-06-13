# In-Widget Live Support (iframe embed) — Implementation Plan

> Drafted overnight for review. Replaces the new-tab handoff with an iframe of the
> live-support guest page overlaid on the chat widget's message area — a correctly
> sized, centered, form-styled experience that keeps the visitor in context.
> New-tab stays as the graceful fallback.

## Decisions already locked (from discussion)
- **Framing is locked, not allow-any.** The guest route is framable **only by the
  tenant's own domain**; the agent dashboard stays frame-blocked.
- **Same-domain gate on the widget.** The widget only *attempts* the iframe when it's
  running on the tenant's domain (the ~99% case). Cross-origin **Curaçao POS**
  deployments fall back to a **new tab** and never frame — so the lock can't break POS.
- **Iframe-first, new-tab fallback** (covers POS, mobile, and browsers that block
  third-party `getUserMedia`).

## Current state (verified in code)
- **Widget** (`hy-rag/public/chat-widget.js`): connect button → `window.open({connect.url}/?ref={ref})`.
- **Guest page** (`live-support/static/index.html`): `<main class="guest-user">` with
  display-toggled sections `.greeting`, `.call` (audio/video buttons + no-agents msg),
  `.call-active` (avatars, remote/local `<video>`, end-call), `.im` (chat dock). Name
  comes from `?name=` (now via the vendored `getInput`).
- **Headers**: live-support sets **no** `X-Frame-Options`/CSP today (static handler
  ~`main.go:1240` sets only Content-Type/ETag/Cache-Control) → framable by default.
- **`ref` (= `config.dbPrefix`) IS the tenant domain** — verified: `instantaiguru.com`,
  `myaccount.icuracao.com`. So `frame-ancestors` derives from `ref`.

## Part A — live-support: `?embed=1` guest mode
**A1. Compact layout.** When `embed=1`, add a body class (`embed`) and an embed
stylesheet that: centers a single card (max ~400px, responsive 100% w/h), removes
full-page chrome, stacks the sections vertically, and sizes the call video for a small
frame. Reuse the existing `.greeting`/`.call`/`.call-active`/`.im` sections — this is
where the broader "odd top-left" UI cleanup folds in.

**A2. Inline name.** In embed mode, render the name as a first-step inline field inside
the card (the card *is* the form) instead of the `getInput` modal. `getInput` stays for
the standalone (non-embed) page.

**A3. Framing headers (route-aware, in the static handler).**
- Guest route w/ `embed=1`: `Content-Security-Policy: frame-ancestors https://<ref> https://www.<ref>` (locked to tenant domain; `<ref>` from `?ref=`).
- Agent dashboard (`auth.html`) + all `/api/*` agent routes: `X-Frame-Options: DENY` and
  `Content-Security-Policy: frame-ancestors 'none'`.
- Modern browsers honor CSP `frame-ancestors`; `X-Frame-Options` kept on the agent route
  as belt-and-suspenders.

## Part B — widget: iframe host (`chat-widget.js`)
**B1. Same-domain gate.** On connect click: if `window.location.hostname` matches the
tenant domain → iframe path; else → new-tab fallback. (Tenant domain = `settings.connect.ref`
since ref=domain; consider an explicit `settings.connect.domain` from getSettings for
robustness — see Open items.)

**B2. Overlay.** Create `<iframe allow="camera; microphone; autoplay" src="{connect.url}/?ref={ref}&embed=1">`,
positioned absolute to fill the chat message area (over `.iAIgW-chat-messages`/the chat
body), with a back/✕ control to dismiss and return to the AI chat. Size the iframe to the
chat body; the embed page is responsive to it.

**B3. Fallback.** New-tab `window.open` when: hostname≠tenant domain, `navigator.mediaDevices`
absent, or the iframe fails to load (onerror / load timeout).

## Flow
1. Visitor chats with the AI → clicks 🎧.
2. Same-domain? → iframe overlays the chat area, loads the guest embed card.
3. Card: enter name → Connect → audio/video/chat with an agent, in-frame.
4. ✕ → back to the AI chat. (POS / blocked → the same flow in a new tab.)

## Phasing (each independently testable)
1. **A1** compact layout + **A3** headers — test by opening
   `https://connect.instantaiguru.com/?ref=instantaiguru.com&embed=1` in a ~360px window;
   confirm a centered card and that it only frames from the tenant domain.
2. **A2** inline name field.
3. **B1+B2+B3** widget iframe host, gate, fallback.
4. Real audio/video call in-frame (camera/mic permission via `allow`).
5. Polish — broader guest UI cleanup (rides A1) on both repos.

## File-by-file
- `live-support/static/index.html` — `embed` body class hook + inline name field.
- `live-support/static/css/*` (or `<style>`) — embed stylesheet.
- `live-support/static/js/guest.js` — read `embed`, compact behavior, inline name.
- `live-support/main.go` — route-aware frame headers (guest: ancestors=ref domain; agent: DENY).
- `hy-rag/public/chat-widget.js` — iframe host + same-domain gate + fallback; deploy via
  `deploy.sh` + `?v=` bump.
- `hy-rag-lambdas` `getSettings` — optional `connect.domain` for the widget's same-domain
  check if `ref` proves unreliable as a hostname.

## Open items
- **Same-domain check source:** `ref`=domain works today; an explicit `connect.domain` from
  getSettings is more robust against ref≠hostname edge cases. Decide in B1.
- **Deploy hops:** live-support image + appliance pull (`docker compose pull && up -d`);
  widget via `deploy.sh prod` + `?v=` bump.
- **CSP nuance:** the guest page may load assets/STUN/TURN; ensure the added CSP only sets
  `frame-ancestors` (not a full default-src) so nothing else breaks.

## Related context
See memory `widget-domain-deployment` for the same-domain-99% / POS-cross-origin fact that
drives the locked-framing + new-tab-fallback design.
</content>
