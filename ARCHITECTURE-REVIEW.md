# Live Support — architecture vs. implementation review

> Written 2026-08-01 after three rounds of symptom-chasing during Ron's manual
> testing. Each round fixed what was reported and the next round found something
> new, which is the signature of patching symptoms rather than a cause.
>
> This document states the decided architecture, then reviews what the code
> actually does, then lists what has to change.

## 1. The decided architecture

| Concern | Decision |
|---|---|
| Privacy | A conversation is private to exactly two participants. Channels are keyed by a random `cid` that appears in no public response, and using one requires a capability token. |
| Identity | Routing addresses (`user_id`, presence `session_id`) are public. Capabilities (tokens) are not. These are never the same value. |
| Routing | Capacity **sorts**, never refuses. The server picks the agent; the client never does. |
| Modality | `chat` is async and multiplexable; `audio`/`video` are synchronous and exclusive (capacity 1). |
| Chat semantics | A message arriving *is* the event. No ring, no accept/decline, no deadline. |
| Delivery | The server is the only writer to a conversation channel. Delivery is idempotent by server-assigned message id. |
| Persistence | Every message is recorded; a console opening later sees the transcript. |

## 2. What the implementation actually does

### 2.1 The defect: a conversation has no identity

**There are two creation sites and zero lookups.**

- `conversationStartHandler` (guest clicks Chat) → `newConvID()` + `createConversation`
- `conversationInviteHandler` (agent types at a visitor) → `newConvID()` + `createConversation`

Neither asks whether a conversation between these two already exists. So the
ordinary flow — visitor says "Hi", agent replies — produces **two**
conversations, each holding half the exchange.

Every symptom reported traces to this single omission:

| Symptom | Explanation |
|---|---|
| Visitor listed twice in PEOPLE | Two `cid`s for one person |
| Agent's thread shows only their own message | They are looking at the conversation *they* created |
| Guest's "Hi" disappeared when the reply arrived | The dock switched to the agent's new conversation; "Hi" is still in the first one |
| "Select someone to chat" / cannot type | `activePeerId` referenced a thread that the twin-absorb logic deleted |

The client-side twin-absorbing I added is not a fix. It is a workaround for a
missing server-side invariant, and it introduced a fourth failure by deleting
threads out from under the active selection.

### 2.2 The invariant that was never stated

The architecture table above says what a conversation *is* but never says
**when a new one should exist**. That gap is the whole bug. Stated properly:

> A visitor has **at most one open conversation per modality**. Starting a chat
> joins the open one if there is one, and creates one only if there is not.
> Reassignment to a different agent changes `agent_user_id`; it does not create
> a second conversation.

Chat and a call may coexist (a call is exclusive, chat is not), which is why the
invariant is per modality rather than per visitor.

### 2.3 Secondary findings

**a. `createConversation` is not idempotent and has no uniqueness constraint.**
Nothing at the schema level would have caught this. A partial unique index over
open chat conversations makes the invariant enforceable rather than merely
intended.

**b. Client thread identity is derived, not authoritative.** The console keys
threads by presence `session_id` *or* `cid` depending on which code path built
them. With 2.1 fixed there is exactly one `cid` per visitor conversation, so the
console can key on `cid` alone and treat presence purely as a liveness lookup.

**c. `activePeerId` can dangle.** Any code that removes or re-keys a thread must
re-point the selection. Nothing enforced that, which is why the composer went
dead ("Select someone to chat" with a thread visibly present).

**d. The agent's roster click creates rather than opens.** With find-or-create
this becomes correct automatically, but the intent should be explicit: clicking a
visitor **opens the conversation with them**, existing or new.

### 2.4 The testing gap — the real process failure

Every server-side rule has a unit test, and all of them passed through all three
rounds. **Not one test exercises a guest and an agent talking to each other.**
The bugs live exactly in that gap: they are not rule violations, they are
*wiring* failures between two clients and a server.

That is why manual testing found four defects that the suite found zero of, and
it is the strongest argument for the browser-level tests Ron asked for.

## 3. What changes

1. **Find-or-create, server-side** — one open conversation per (visitor,
   modality). Both creation sites go through it.
2. **Schema constraint** — partial unique index so the invariant cannot be
   violated by a future code path.
3. **Console keys threads by `cid` only**; presence resolves liveness. Remove
   the twin-absorbing workaround.
4. **Selection is never left dangling** — re-point `activePeerId` whenever the
   thread set changes.
5. **Playwright end-to-end tests** driving a real guest and a real agent against
   the real appliance, asserting the things manual testing caught:
   one entry per visitor, both sides see both messages, no duplicates, composer
   usable without a manual selection, transcript survives reload.

## 4. Why this was missed

The security work had a clear invariant ("only two participants may access a
conversation") and got tests that pin it. The *lifecycle* had no equivalent
invariant written down, so there was nothing to test against and nothing to
violate. Symptom-chasing followed naturally: each report described a rendering
problem, and each fix addressed the rendering.

The lesson worth keeping: an architecture table that describes what things *are*
is not sufficient. It has to state **when they are created and when they end**.
