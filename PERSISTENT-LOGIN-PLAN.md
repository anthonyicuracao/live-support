# Persistent Agent Login — Design

> **Status: implemented 2026-08-01.** Parts A-D are in; the "Current state"
> section below describes the code as it was BEFORE this change and is kept as
> the record of why each piece exists. Tests in `session_persist_test.go`.
>
> Goal: an agent stays logged in and available indefinitely, including the
> owner-agent who takes one call a week or one a month. Before this they were
> silently logged out, and the console kept claiming otherwise.

## What shipped

| Part | Change |
|---|---|
| A1 | `envHours` accepts `0` (was `n > 0`, which silently swallowed it) |
| A2 | `sessionNever = 0` + `sessionExpired()` guard; create/lookup/sweep all honour it |
| A3 | `SESSION_TTL_HOURS` default `168` → `0` (never) |
| A4 | `cookieMaxAgeCap` (400d) + `refreshSessionCookie` on both auth middlewares |
| A5 | CSRF cookie `MaxAge` 1h → 24h |
| B1 | `guardAuth()` in `auth.js`; `postAvailability` no longer ignores a 401 |
| B2 | `/api/me` revalidation every 5 min, plus on focus and `visibilitychange` |
| B3 | `wsSessionToken` + `watchWSSession` — an agent socket closes when its session dies; guests untouched |
| B4 | `lookupAuthSession` clears availability when it reaps an expired session; `deleteUserAuthSessions` does the same |
| C | `Signed in` column + `Sign out everywhere` (`POST /users/signout`), `loginAgo` helper |
| D | `userIsAvailable` now JOINs `users.active`, matching discovery |

Not done, deliberately: the unauthorized WS `subscribe`/`broadcast` noted in Part
D stays open for its own decision. B3 stops presence outliving a session, which
is the part that belongs to this change; it does not restrict what an
already-connected socket may subscribe to.

## The decision that shapes everything

**Sessions do not expire.** Ron, 2026-08-01: the "stolen cookie" threat barely
applies here, because a live session's only power is *being available to answer
calls* while simultaneously showing on the admin screen as logged in, and the
admin can revoke it in one click. Weighed against that, an owner-agent who is
rung once a month must not be logged out between calls. No idle window is
acceptable, which rules out sliding renewal with any cap.

**Revocation already works, verified in code**, and is what this design leans on:

| Path | Mechanism | Where |
|---|---|---|
| Delete user | `DELETE FROM auth_sessions WHERE user_id = ?` + FK `ON DELETE CASCADE` | `auth.go:254`, `main.go:162` |
| Password reset | deletes all that user's sessions | `auth.go:547` |
| Invite reclaim | deletes all that user's sessions | `auth.go:430` |
| Deactivate | `lookupAuthSession` rejects when `!u.Active` | `auth.go:322` |

Deactivate takes effect on the very next request; there is no cached identity to
outlive it.

## Current state (verified in code)

**Layer 1 — a 7-day wall with no renewal.**
`sessionTTL` defaults to 168h (`auth.go:819`); `createAuthSession` stamps
`expires_at = now + ttl` once. There is **no** `UPDATE auth_sessions` anywhere in
the repo, so the expiry is absolute: an agent who works every day is still cut
off on day 7. `envHours` rejects `0` (`n > 0`, `auth.go:779`), so
`SESSION_TTL_HOURS=0` silently falls back to 168 and there is currently **no way
to express "never".**

**Layer 2 — expiry is invisible to an open console.** This is the actual defect:

- `/api/me` is fetched **once, at page load** (`auth.js:156`). Never re-checked.
- `postAvailability()` (`auth.js:308`) awaits the fetch inside a try/catch but
  never inspects `resp.ok`. A 401 is a *successful* fetch and does not throw, so
  it produces no signal at all. Its catch comment reads "Best effort — live WS
  presence still covers open-tab callability."
- `wsHandler` (`main.go:630`) does **no authentication**: `websocket.Accept` with
  `OriginPatterns: ["*"]` and no session lookup. Presence therefore outlives the
  session indefinitely.

**Layer 3 — the silent rot.** With the availability touch 401ing, `updated_at`
freezes. `discoveryFreshness` (24h, `availability.go:33`) then ages the agent out
of REST discovery. About a day after the invisible logout the agent has stopped
being discoverable while the console still displays **Available**. They learn on
the next reload, when they are bounced to `/login`.

**Layer 4 — expired is not paused.** Expiry is passive, so `clearAvailability`
never runs and the row stays `available = 1`. Push ring gates on that DB flag, so
the agent is still rung, taps the notification, and lands on a login page with
the call already gone.

## Part A — non-expiring sessions

**A1. Let `0` mean never.** `envHours` currently treats `0` as invalid and
returns the default; it must accept `0` and return `0`. This is the whole reason
the knob cannot express the desired behaviour today.

**A2. Represent "never" as `expires_at = 0`.** Keeps the column and its index,
so no table rebuild:

- `createAuthSession`: `ttl == 0` stores `expires_at = 0`.
- `lookupAuthSession`: expired only when `s.ExpiresAt != 0 && now >= s.ExpiresAt`.
- `sweepAuthSessions`: `WHERE expires_at != 0 AND expires_at < ?`, so sweeping
  never reaps a permanent session.

Writing `0` rather than a sentinel far-future timestamp is deliberate: a
far-future value is indistinguishable from a real one, so any later reader has to
guess whether it means "never" or "a very long session".

**A3. Default to never.** `SESSION_TTL_HOURS` default changes `168` → `0`. This
is a behaviour change for existing deployments and is the point of the exercise;
operators who want expiry set the hours explicitly.

**A4. The cookie cannot actually be permanent, and we should stop pretending
otherwise.** Chrome caps cookie `Max-Age` at **400 days** regardless of what we
send. So a permanent server session paired with a write-once cookie still evicts
the agent at ~13 months. Fix: **re-set the session cookie on activity**, so its
400-day clock restarts. Cheap and idempotent, done at most once per day per
session by comparing against a `last_cookie_refresh` we already have a natural
home for. This is the one place where a sliding mechanism is genuinely required,
and it is a browser constraint rather than a security policy.

**A5. CSRF cookie lifetime.** `MaxAge: 3600` (`auth.go:680`) against a session
that now never expires. It guards four form POSTs, not the JSON API, so the blast
radius is small, but a login page left open for an hour fails on submit for no
good reason. Raise it and keep re-issuing it per render, which `issueCSRF`
already does.

## Part B — make session loss impossible to miss

Ron did not rule on this list, so this is my recommendation rather than a
decision he made. All four are cheap, and the second is the one that turns a
silent failure into a visible one.

**B1. Inspect `resp.ok`.** `postAvailability` and the other authed console
fetches treat `401` as what it is. On 401 the console shows an explicit
signed-out state rather than continuing to render **Available**.

**B2. Revalidate `/api/me`.** On a timer (~5 min) and on `visibilitychange` /
`focus`, so a laptop reopened after a week finds out immediately rather than at
the next hard reload.

**B3. Authenticate the agent WebSocket.** Presence must not be able to outlive
the session. This also closes part of the exposure noted below.

**B4. Clear availability when a session dies.** Whatever reaps a session
(sweeper, expiry-on-lookup) also clears the availability bit, so an agent who
cannot answer is not rung. With Part A this becomes rare, which is exactly why it
should be correct rather than relied upon.

## Part C — the admin visibility this security model assumes

Ron's reasoning is that a logged-in agent "clearly appear[s] on the admin screen
as logged-in (working)". **Today it does not.** `templates/users.tmpl:36` renders
only Username / Role / Status, where Status is Active|Inactive plus pending-first-
login and reset-requested flags. Nothing shows whether a user currently holds a
session. The live Agents view in the console shows who is *online*, which is a
different question from who is *logged in*.

Since permanent sessions rest on the admin being able to see and revoke them,
this needs closing in the same change:

- A **Signed in** column on the users table: session count and last-seen.
- A per-user **Sign out everywhere** action (`DELETE FROM auth_sessions WHERE
  user_id = ?`), so revoking a session does not require deleting the account or
  resetting the password.

## Part D — smaller findings from the same pass

- **`userIsAvailable` does not check `users.active`** (`availability.go:69`),
  while `/api/agents/available` does (`JOIN users u ON … u.active = 1`). Exposure
  is small because the roster is what a guest rings from, but the ring gate is
  the security-relevant one and should not be the laxer of the two.
- **WS `subscribe`/`broadcast` are unauthorized** (`main.go:492`). Any client may
  subscribe to `presence:<ref>` (guessable, since `ref` is the tenant domain) and
  read the agent roster, or broadcast into a channel whose session id it knows.
  Guests are unauthenticated by design, so this is a design tension rather than a
  clear bug, and the sharp edge is unrestricted `broadcast` rather than
  `subscribe`. Flagging for a separate decision, not folding it into this change.

## Migration and compatibility

- No schema change. `expires_at = 0` uses the existing column; the existing index
  is unaffected.
- Existing sessions keep their real `expires_at` and expire once, normally. Only
  sessions minted after the change are permanent. No forced re-login on deploy.
- Env override remains: `SESSION_TTL_HOURS=168` restores today's behaviour.

## Tests

`go test ./...` is green today and is the baseline. To add:

1. `ttl = 0` mints `expires_at = 0`; `lookupAuthSession` still resolves it well
   past any plausible clock skew.
2. The sweeper deletes a genuinely expired session and leaves a permanent one.
3. `envHours("…", 168)` returns `0` when the env var is `"0"`, and still returns
   the default for `""`, `"-1"`, `"abc"`.
4. Deactivate, delete, and password reset each kill a permanent session.
5. A 401 from `/api/availability` leaves the console in a signed-out state rather
   than reporting available (harness-level, mirroring `authharness_test.go`).

## Open items

- Should `SESSION_TTL_HOURS=0` also disable the sweeper's hourly tick entirely,
  or keep it running for invites and resets? (Leaning: keep it, it also sweeps
  those tables.)
- Does the once-per-day cookie refresh in A4 belong on every authed request, or
  only on the ones the console makes anyway? (Leaning: middleware, so it cannot
  be missed by a new endpoint.)
