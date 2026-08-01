# Live Chat as a First-Class Channel — Design

> For review. No code written yet. Companion to `PERSISTENT-LOGIN-PLAN.md`.
>
> Goal: an agent sets availability per modality (Chat / Audio / Video), and a
> guest can start a **chat** the same way they start a call today.

## Current state (verified in code)

**Modality is not modelled.** `agent_availability` carries one `available` bit
plus `has_camera` (`main.go:207`). `has_mic` is **hardcoded `true`** in the
discovery response (`availability.go:195`). So "can this agent take video" is
inferred from *device capability*, never from agent *intent*, and "can this agent
take chat" is not a question the system can answer at all.

**A guest can never start a chat.** The IM dock is strictly agent-initiated:

```js
if (!t) return; // can't message an admin who never messaged us   guest.js:1023
```

and the receive path states it outright: "Only inbound admin messages create or
update a thread. This is the sole way a guest ever learns an admin's session id."

**Chat rides live WS presence, not durable availability.** `sendIM` targets an
agent's *presence session id*. So chat does not work down the push-reachable /
closed-console path that audio and video do support: the whole Phase-3 durable
availability work simply does not apply to it.

**Guest entry points are audio and video only.** `.audio-call-button` and
`.video-call-button` (`guest.js:312`, `:322`); `?auto=` accepts `audio|video`
only (`guest.js:88`). The video button is disabled when no discovered agent has
a camera, with an explanatory hint — a good precedent to copy per modality.

**Chat is ephemeral.** Threads live in an in-memory `Map` in the page
(`guest.js:947`) and messages are WS broadcasts. Nothing persists, so a
push-woken agent opening a fresh console sees no history. The existing `messages`
table is the contact-form inbox (`name`/`contact`/`message`), not a chat
transcript.

## Part A — model modality

**A1. Schema.** Three columns on `agent_availability`, added with the repo's
existing idiom (unconditional `ALTER TABLE … ADD COLUMN`, error ignored when it
already exists — precedent at `main.go:252`), because the schema block uses
`CREATE TABLE IF NOT EXISTS` and will not alter an existing table:

```sql
ALTER TABLE agent_availability ADD COLUMN chat_ok  INTEGER NOT NULL DEFAULT 1;
ALTER TABLE agent_availability ADD COLUMN audio_ok INTEGER NOT NULL DEFAULT 1;
ALTER TABLE agent_availability ADD COLUMN video_ok INTEGER NOT NULL DEFAULT 0;
```

`available` stays as the master switch: it is the "am I working" bit, and the
three flags say what kind of work. An agent with `available = 0` is offered
nothing regardless of flags.

**Backfill semantics.** Existing rows get `audio_ok = 1` (what they do today),
`video_ok` backfilled from `has_camera` (what discovery infers today), and
`chat_ok = 1`. **Decided by Ron, 2026-08-01: backfill `chat_ok = 1`.** Every
currently-available agent therefore becomes chat-available the moment this
deploys, without opting in. That is the product intent, and chat has no hardware
precondition that could make the promise false. Recorded explicitly because it
changes what guests are offered on deploy day.

`has_camera` stays: it is a *capability* fact and remains the right input for
disabling the video toggle in the console. `video_ok` is *intent* and is what
discovery should publish. Keeping both is what stops the current conflation.

**A2. API.** `GET`/`POST /api/availability` carry the three flags.
`/api/agents/available` publishes them and **stops hardcoding `has_mic`**,
reporting `audio_ok` / `video_ok` / `chat_ok` instead. Guests filter on intent.

**A3. Ring gate.** `/api/call/ring` takes a `callType`; it must additionally
refuse a type the target agent has not enabled, alongside the existing
`userIsAvailable` check. Otherwise a stale roster lets a guest ring an agent for
video they explicitly turned off.

## Part B — console UI

Master **Available** toggle plus three modality checkboxes.

The existing deferred-permission pattern is the right shape and should be
extended, not replaced: today the agent picks whether they will take video
*before* going available, and permission is requested only at Go-Available, only
for the picked modes (`auth.js:328`). That generalises cleanly — Chat requires no
permission at all, Audio requires mic, Video requires mic + camera.

Rules:
- Chat has no hardware precondition, so it can never be blocked by a permission
  failure. An agent with no working mic can still be chat-available, which is
  precisely the case the current single-bit model cannot express.
- Video stays disabled when `has_camera` is false, reusing the existing hint.
- Turning every modality off is equivalent to Pause: the master bit clears rather
  than leaving an "available for nothing" record that discovery has to special-
  case.

## Part C — guest chat as a first-class entry

**C1. A third button.** `.chat-button` beside audio and video, gated on
`anyChatOk` exactly as video is gated on `anyHasCamera` today. `?auto=` accepts
`chat`.

**C2. Guest-initiated threads.** This is the substantive change. The guest
already receives a roster carrying `session_id` per agent, so the target is
available without waiting for an inbound message; the current restriction is a
consequence of chat having been built as a call-side channel, not a security
boundary. The guest picks a target the same way the call buttons do, and the
"can't message an admin who never messaged us" guard is replaced by a real
target-selection path.

**C3. Chat rings.** A chat request goes through `/api/call/ring` with
`callType: "chat"`, so a closed console is woken by Web Push exactly as a call
is. This is what makes chat a peer of audio and video rather than a lesser
channel, and it is why C2 cannot simply reuse the WS-presence path.

**C4. Notification wording.** The service worker currently renders
`"${callType} call from ${callerName}"` (`auth.js:1295`). "chat call" is wrong;
the type needs a proper label per modality.

**C5. No WebRTC for chat.** The chat path must not construct a peer connection or
request media. Today `initiateCall` is the only entry and it does both.

## Part D — persistence

Chat as a first-class channel implies a transcript. A push-woken agent opening a
fresh console must see what the guest already said, and today they would see an
empty dock.

Proposal: a `chat_messages` table keyed by conversation (guest session +
agent user), with the WS broadcast kept as the live transport and the table as
the record. Deliberately **not** reusing the `messages` table, which is the
contact-form inbox and has an unrelated shape.

This is the largest single piece of the work and would have been separable.
**Decided by Ron, 2026-08-01: Part D ships in the same pass.** Chat without a
transcript is not a first-class channel — a push-woken agent opening a fresh
console to an empty dock, while the guest can see everything they typed, is
precisely the kind of silent asymmetry this whole effort exists to remove.

## Part E — concurrency: chat while on a call

**Decision (mine, deferred to me by Ron 2026-08-01): allow it, with a governor.**

Allowing it is the straightforward part. Chat contends for none of the resources
a call does — no mic, no camera, no peer connection, negligible bandwidth — and
blended handling (a voice agent answering chats between or during calls) is
ordinary contact-centre behaviour. Forbidding it would mean an agent on a
five-minute call is invisible to every chat-seeking guest for those five minutes,
which for a one-owner tenant means the channel is simply down.

The reliability half is a limit, because "unlimited concurrent chats" degrades
every conversation at once and is how a queue silently becomes a backlog:

- **`max_concurrent_chats`**, configurable, default **3**. Enforced **server-side
  at the ring gate**, not in the console, so a stale roster cannot push an agent
  past it.
- Discovery **prefers less-loaded agents** when several are chat-available, so
  load spreads rather than piling onto whoever happens to sort first.
- At the limit the agent stops being offered *for chat* while remaining available
  for audio and video. Falling off one modality must never look like going
  offline.
- **The existing mid-call block on availability flips stays** (`auth.js:605`).
  That guard protects live WebRTC state from being yanked, which is a different
  concern from receiving a chat, and it is correct as written. Nothing in this
  part relaxes it.

The distinction worth holding onto: *taking a chat* during a call is safe;
*changing what you are available for* during a call is not.

## Interaction with `PERSISTENT-LOGIN-PLAN.md`

These reinforce each other. Chat is the modality an owner-agent is most likely to
be woken for a month after logging in, so permanent sessions matter more once
chat exists. Conversely, the silent-401 rot is worse with chat: a guest typing
into a dead thread gets no error at all. Recommend landing persistent login
first, since it is smaller and its detection work (Part B there) is what makes
chat failures visible.

## Tests

1. Availability round-trip preserves all three flags.
2. `/api/agents/available` reports intent, not device capability, and no longer
   hardcodes `has_mic`.
3. Ring refuses a modality the agent has disabled, and refuses everything when
   `available = 0`.
4. Backfill: an existing row with `has_camera = 1` becomes `video_ok = 1`; with
   `has_camera = 0`, `video_ok = 0`.
5. Clearing the last modality clears the master bit.
6. A chat ring wakes a closed console via push without constructing a peer
   connection.

## Decisions (2026-08-01)

All three open items are now settled and folded into the parts above:

| Question | Decision | Where |
|---|---|---|
| Backfill `chat_ok` | **1** — every available agent is chat-available on deploy | Part A1 |
| Does persistence ship in this pass | **Yes** — Part D is in scope | Part D |
| Chat while on a call | **Allowed, capped at `max_concurrent_chats` (default 3), enforced at the ring gate** | Part E |

## Open items

None blocking. Two things to settle during implementation rather than before it:

- Where `max_concurrent_chats` lives: env-wide, or per-tenant in the availability
  row so a busy tenant can raise it without a redeploy. (Leaning: env default,
  per-tenant override.)
- Whether "less-loaded agents preferred" belongs in the discovery SQL or in the
  guest's target selection. (Leaning: discovery, so every caller inherits it.)
