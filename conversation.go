// conversation.go — private, capability-gated conversations.
//
// The problem this replaces
// -------------------------
// Signalling used to ride `inbox:<session_id>` channels, and the security model
// was written down in static/js/shared.js:
//
//	"the carrier is the recipient's own inbox channel keyed by their session_id.
//	 A guest therefore only ever learns an admin's session_id because that admin
//	 messaged them first — guests are never sent the admin roster and cannot
//	 enumerate or cold-message admins."
//
// That model was sound while it held. It stopped holding when
// /api/agents/available was added for durable availability: a PUBLIC endpoint,
// gated only on a guessable ref (the tenant's own domain), that returned
// session_id for every available agent. The channel name was the capability,
// and we started publishing it.
//
// Combined with a WebSocket that authorized nothing, anyone knowing a tenant
// domain could read an agent's inbound messages and call signalling, or inject
// their own.
//
// The model here
// --------------
// A conversation is a server-side record with exactly two participants. Its
// channel is keyed by a random conversation id that appears in no public
// response, and subscribing to it requires a capability TOKEN that only the two
// participants are ever issued:
//
//   - the guest gets one when the server creates the conversation, and only
//     after the server has confirmed the agent takes that kind of contact;
//   - the agent gets one from an authenticated request, and only for a
//     conversation they actually own.
//
// So a third party cannot listen and cannot join: they have no id to name, and
// naming it would not help without a token they cannot mint. Routing addresses
// and secrets are no longer the same value — which was the original mistake.
package main

import (
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// convTokenTTL bounds a leaked token's usefulness. Long enough that neither a
// long call nor a chat that idles over a lunch break is interrupted; short
// enough that a token lifted from a log is not a standing key.
const convTokenTTL = 12 * time.Hour

// Token roles. Stored in the token so the server can tell participants apart
// without trusting anything the client says. The role also decides WHICH
// channel a token unlocks, which is why the client never names one.
const (
	convRoleGuest = "guest"
	convRoleAgent = "agent"
	// A visitor's own inbox. Unlike the two above it is not tied to a
	// conversation: it is how an AGENT opens contact with a visitor who has no
	// authenticated channel of their own.
	roleGuestSession = "guestsession"
)

// guestChannel is a visitor's private inbox, keyed by a SERVER-MINTED session
// id. Server-minted matters: agents can see a visitor's session id in presence,
// so if the client chose it, anyone who saw it could ask for a token for it.
// Minting id and token together means seeing the id is not enough.
func guestChannel(sid string) string { return "guest:" + sid }

// channelForToken maps a verified token to the one channel it unlocks. The
// client never supplies a channel name — this is the whole reason naming a
// channel you hold no token for cannot talk you into it.
func channelForToken(t convToken) string {
	if t.Role == roleGuestSession {
		return guestChannel(t.CID)
	}
	return convChannel(t.CID)
}

var errBadConvToken = errors.New("invalid conversation token")

// convSecret is the key conversation capability tokens are signed with. Set
// once at startup from the same secret the appliance already manages, so there
// is no new key to provision or rotate. The WebSocket hub reads it directly:
// it has no authApp to reach through, and the alternative — passing the app
// into the hub — would let a future caller supply a different key by accident.
var convSecret string

// maxConcurrentChats bounds how many chats one agent may hold at once. Chat can
// run alongside a call (chat contends for no mic, camera or peer connection),
// but unbounded concurrency degrades every conversation at once and turns a
// queue into a silent backlog.
func maxConcurrentChats() int {
	if v := strings.TrimSpace(os.Getenv("MAX_CONCURRENT_CHATS")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return 3
}

type convToken struct {
	CID  string `json:"cid"`
	Role string `json:"role"`
	Exp  int64  `json:"exp"`
}

// convChannel is the ONLY channel a conversation's traffic flows over. Keyed by
// the conversation id, never by a participant's session id — a session id is a
// routing address that we publish, and must therefore never be a secret.
func convChannel(cid string) string { return "conv:" + cid }

func newConvID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func mintConvToken(secret, cid, role string) (string, error) {
	b, err := json.Marshal(convToken{CID: cid, Role: role, Exp: time.Now().Add(convTokenTTL).Unix()})
	if err != nil {
		return "", err
	}
	return signRaw(secret, string(b))
}

// parseConvToken verifies and decodes a capability token. Every failure mode
// returns the same error: a caller must not be able to tell "malformed" from
// "expired" from "wrong key" from a token that simply is not theirs.
func parseConvToken(secret, tok string) (convToken, error) {
	var t convToken
	if secret == "" || tok == "" {
		return t, errBadConvToken
	}
	pt, err := decryptRaw(secret, tok)
	if err != nil {
		return t, errBadConvToken
	}
	if err := json.Unmarshal([]byte(pt), &t); err != nil {
		return t, errBadConvToken
	}
	switch t.Role {
	case convRoleGuest, convRoleAgent, roleGuestSession:
	default:
		return t, errBadConvToken
	}
	if t.CID == "" {
		return t, errBadConvToken
	}
	if t.Exp != 0 && time.Now().Unix() > t.Exp {
		return t, errBadConvToken
	}
	return t, nil
}

// ───────────────────────── store ────────────────────────────────────────────

type conversation struct {
	CID          string
	Ref          string
	GuestSession string
	GuestName    string
	AgentUserID  int64
	CallType     string
	CreatedAt    int64
	EndedAt      sql.NullInt64
}

// openConversationFor returns the visitor's existing OPEN conversation for a
// modality, or false if they have none.
//
// This is the invariant the whole lifecycle rests on and which was missing: a
// visitor has at most one open conversation per modality. Without it, the guest
// clicking Chat and the agent replying from the roster each minted their own,
// so the two of them were in different conversations and each saw half the
// exchange.
func openConversationFor(db *sql.DB, ref, guestSession, callType string) (conversation, bool) {
	var c conversation
	err := db.QueryRow(
		`SELECT cid, ref, guest_session, guest_name, agent_user_id, call_type, created_at, ended_at
		   FROM conversations
		  WHERE ref = ? AND guest_session = ? AND call_type = ? AND ended_at IS NULL
		  ORDER BY created_at DESC LIMIT 1`, ref, guestSession, callType).
		Scan(&c.CID, &c.Ref, &c.GuestSession, &c.GuestName, &c.AgentUserID, &c.CallType, &c.CreatedAt, &c.EndedAt)
	if err != nil {
		return conversation{}, false
	}
	return c, true
}

func createConversation(db *sql.DB, c conversation) error {
	_, err := db.Exec(
		`INSERT INTO conversations (cid, ref, guest_session, guest_name, agent_user_id, call_type, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		c.CID, c.Ref, c.GuestSession, c.GuestName, c.AgentUserID, c.CallType, c.CreatedAt)
	if err == nil {
		// A new conversation is active by definition.
		_, _ = db.Exec(`UPDATE conversations SET last_activity_at = ? WHERE cid = ?`, c.CreatedAt, c.CID)
	}
	return err
}

func conversationByCID(db *sql.DB, cid string) (conversation, error) {
	var c conversation
	err := db.QueryRow(
		`SELECT cid, ref, guest_session, guest_name, agent_user_id, call_type, created_at, ended_at
		 FROM conversations WHERE cid = ?`, cid).
		Scan(&c.CID, &c.Ref, &c.GuestSession, &c.GuestName, &c.AgentUserID, &c.CallType, &c.CreatedAt, &c.EndedAt)
	return c, err
}

// chatInactiveAfter is how long without a visitor message before a chat stops
// consuming capacity. It does NOT end the conversation — the agent keeps it in
// their list and the visitor can resume it; it only stops the abandoned ones
// from occupying the agent forever.
//
// 10 minutes matches the default the mature platforms converged on, and like
// theirs it is configurable rather than baked in.
func chatInactiveAfter() time.Duration {
	if v := strings.TrimSpace(os.Getenv("CHAT_INACTIVE_MINUTES")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return time.Duration(n) * time.Minute
		}
	}
	return 10 * time.Minute
}

// agentLoad is one agent's current workload, by modality.
type agentLoad struct {
	userID int64
	chats  int  // ACTIVE chats only
	onCall bool // audio or video in progress
	live   bool // console open right now
}

// capacityFor reports how many of a modality an agent may hold at once.
//
// The asymmetry is physical, not a policy knob: chat is asynchronous and
// multiplexable, so several at once is normal work. A call is synchronous and
// exclusive — one voice, one pair of ears — so its capacity is 1 and no setting
// should be able to raise it. Pretending otherwise would just produce calls
// nobody can answer.
func capacityFor(callType string) int {
	if callType == callTypeChat {
		return maxConcurrentChats()
	}
	return 1
}

// agentLoads reads current workload for every candidate agent in one pass.
func agentLoads(db *sql.DB, cutoff int64) map[int64]*agentLoad {
	out := map[int64]*agentLoad{}
	rows, err := db.Query(
		`SELECT agent_user_id, call_type, last_activity_at
		   FROM conversations WHERE ended_at IS NULL`)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var uid, act int64
		var ct string
		if err := rows.Scan(&uid, &ct, &act); err != nil {
			continue
		}
		l := out[uid]
		if l == nil {
			l = &agentLoad{userID: uid}
			out[uid] = l
		}
		if ct == callTypeChat {
			// Inactive chats do not count. This is the whole reason an
			// abandoned conversation no longer takes an agent out of rotation.
			if act > cutoff {
				l.chats++
			}
			continue
		}
		// A call in progress is exclusive regardless of how long it has been
		// quiet — silence on a call means listening, not absence.
		l.onCall = true
	}
	return out
}

// routeAgent picks who should take a new conversation.
//
// Capacity SORTS, it does not gate — the visitor is never turned away because
// of an agent-side accounting number. Least-loaded first, live consoles ahead
// of push-only ones. The bool reports whether the chosen agent actually has
// spare capacity, so the caller can set the visitor's expectation honestly
// (and, for a call, offer a channel that can be served right now).
func routeAgent(db *sql.DB, ref, callType string) (int64, bool) {
	live := hub.liveAgentUserIDs(ref)
	cutoff := time.Now().Add(-chatInactiveAfter()).Unix()
	loads := agentLoads(db, cutoff)

	rows, err := db.Query(
		`SELECT a.user_id, a.chat_ok, a.audio_ok, a.video_ok
		   FROM agent_availability a
		   JOIN users u ON u.id = a.user_id AND u.active = 1
		  WHERE a.available = 1`)
	if err != nil {
		return 0, false
	}
	defer rows.Close()

	var bestID int64
	bestScore := 1 << 30
	bestSpare := false
	for rows.Next() {
		var uid int64
		var chatOK, audioOK, videoOK int
		if err := rows.Scan(&uid, &chatOK, &audioOK, &videoOK); err != nil {
			continue
		}
		m := modes{Chat: chatOK == 1, Audio: audioOK == 1, Video: videoOK == 1}
		if !m.allows(callType) {
			continue // they do not offer this modality at all
		}
		l := loads[uid]
		if l == nil {
			l = &agentLoad{userID: uid}
		}
		used := l.chats
		if callType != callTypeChat {
			if l.onCall {
				used = 1
			} else {
				used = 0
			}
		}
		spare := used < capacityFor(callType)
		// Rank: spare capacity first, then live console, then least loaded.
		score := used
		if !spare {
			score += 1000
		}
		if !live[uid] {
			score += 100
		}
		if score < bestScore {
			bestScore, bestID, bestSpare = score, uid, spare
		}
	}
	return bestID, bestSpare
}

// ───────────────────────── HTTP ─────────────────────────────────────────────

// POST /api/conversation/start (public, ref-controlled).
//
// The guest's entry point, and the ONLY way a guest obtains the ability to talk
// to an agent. Deliberately server-side: the checks below are exactly the ones
// a client could otherwise skip.
func (a *authApp) conversationStartHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	var body struct {
		Ref string `json:"ref"`
		// No agentUserId: ROUTING IS A SERVER CONCERN. The client used to pick
		// the agent, which meant trusting it to honour availability, modality
		// and load — and let it target one agent deliberately. The server picks.
		CallType     string `json:"callType"`
		GuestSession string `json:"guestSession"`
		GuestName    string `json:"guestName"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		errJSON(w, 400, "bad json")
		return
	}
	if body.Ref == "" || body.GuestSession == "" {
		errJSON(w, 400, "missing fields")
		return
	}
	// Positive whitelist — an unrecognised type must not fall through to a
	// modality the agent never enabled.
	switch body.CallType {
	case callTypeChat, callTypeAudio, callTypeVideo:
	default:
		errJSON(w, 400, "unknown call type")
		return
	}
	if !dbs.exists(body.Ref) {
		errJSON(w, 404, "unknown tenant")
		return
	}
	db, err := dbs.get(body.Ref)
	if err != nil {
		errJSON(w, 400, err.Error())
		return
	}
	// Join the visitor's existing open conversation for this modality rather
	// than minting a second one. Starting a chat you already have is resuming
	// it, not beginning another; the agent's half of the exchange lives there.
	if existing, ok := openConversationFor(db, body.Ref, body.GuestSession, body.CallType); ok {
		tok, terr := mintConvToken(a.ssoSecret, existing.CID, convRoleGuest)
		if terr != nil {
			errJSON(w, 500, "internal error")
			return
		}
		writeJSON(w, 200, map[string]any{
			"cid": existing.CID, "token": tok, "channel": convChannel(existing.CID),
			"agentId": existing.AgentUserID, "waiting": false,
			"callType": existing.CallType, "resumed": true,
		})
		return
	}

	// Route. Capacity SORTS here, it never gates: a visitor is not turned away
	// because of an agent-side accounting number. This replaced a 429 that made
	// an abandoned test chat permanently unreachable for everyone.
	agentID, hasSpare := routeAgent(db, body.Ref, body.CallType)
	if agentID == 0 {
		// Genuinely nobody offers this modality right now. That is a real fact
		// about the world, not a capacity decision, and the visitor is told so
		// they can pick another channel or leave a message.
		errJSON(w, 409, "no agent offers this channel right now")
		return
	}

	cid, err := newConvID()
	if err != nil {
		errJSON(w, 500, "internal error")
		return
	}
	conv := conversation{
		CID: cid, Ref: body.Ref, GuestSession: body.GuestSession,
		GuestName: strings.TrimSpace(body.GuestName), AgentUserID: agentID,
		CallType: body.CallType, CreatedAt: time.Now().Unix(),
	}
	if err := createConversation(db, conv); err != nil {
		errJSON(w, 500, "store failed")
		return
	}
	tok, err := mintConvToken(a.ssoSecret, cid, convRoleGuest)
	if err != nil {
		errJSON(w, 500, "internal error")
		return
	}
	// `waiting` is the honest part. For chat it is almost cosmetic: the agent
	// holds several at once and will get to it. For a CALL it is decisive —
	// calls are exclusive, so an agent already on one cannot answer, and the
	// visitor is better served by being offered chat now than by a ring nobody
	// can pick up.
	writeJSON(w, 200, map[string]any{
		"cid":      cid,
		"token":    tok,
		"channel":  convChannel(cid),
		"agentId":  agentID,
		"waiting":  !hasSpare,
		"callType": body.CallType,
	})
}

// GET /api/conversation/token?cid=… (authed): the agent's half of the
// capability. Issued only to the agent the conversation actually belongs to, so
// one agent cannot obtain a token for a colleague's conversation.
func (a *authApp) conversationTokenHandler(w http.ResponseWriter, r *http.Request) {
	info := authFrom(r.Context())
	if info == nil {
		errJSON(w, 401, "not signed in")
		return
	}
	cid := r.URL.Query().Get("cid")
	if cid == "" {
		errJSON(w, 400, "missing cid")
		return
	}
	conv, err := conversationByCID(info.db, cid)
	if err != nil {
		// Same answer as "not yours": a caller must not be able to probe which
		// conversation ids exist.
		errJSON(w, 404, "not found")
		return
	}
	if conv.AgentUserID != info.user.ID {
		errJSON(w, 404, "not found")
		return
	}
	tok, err := mintConvToken(a.ssoSecret, cid, convRoleAgent)
	if err != nil {
		errJSON(w, 500, "internal error")
		return
	}
	writeJSON(w, 200, map[string]any{
		"cid":          cid,
		"token":        tok,
		"channel":      convChannel(cid),
		"callType":     conv.CallType,
		"guestName":    conv.GuestName,
		"guestSession": conv.GuestSession,
	})
}

// POST /api/conversation/end (public, ref-controlled): mark a conversation
// finished so it stops counting against the chat governor. Requires the
// capability token — ending someone else's conversation is as much an attack as
// listening to it.
func (a *authApp) conversationEndHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	var body struct {
		Ref   string `json:"ref"`
		CID   string `json:"cid"`
		Token string `json:"token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		errJSON(w, 400, "bad json")
		return
	}
	t, err := parseConvToken(a.ssoSecret, body.Token)
	if err != nil || t.CID != body.CID || body.CID == "" {
		errJSON(w, 403, "forbidden")
		return
	}
	if !dbs.exists(body.Ref) {
		errJSON(w, 404, "unknown tenant")
		return
	}
	db, err := dbs.get(body.Ref)
	if err != nil {
		errJSON(w, 400, err.Error())
		return
	}
	_, _ = db.Exec(`UPDATE conversations SET ended_at = ? WHERE cid = ? AND ended_at IS NULL`,
		time.Now().Unix(), body.CID)
	writeJSON(w, 200, map[string]any{"ok": true})
}

// authConv resolves (and authorises) a conversation from a capability token.
// Every transcript endpoint goes through it, so there is exactly one place that
// decides whether a caller may touch a conversation's contents.
func authConv(ref, cid, token string) (*sql.DB, conversation, bool) {
	var zero conversation
	t, err := parseConvToken(convSecret, token)
	if err != nil || cid == "" || t.CID != cid {
		return nil, zero, false
	}
	if !dbs.exists(ref) {
		return nil, zero, false
	}
	db, err := dbs.get(ref)
	if err != nil {
		return nil, zero, false
	}
	conv, err := conversationByCID(db, cid)
	if err != nil || conv.Ref != ref {
		return nil, zero, false
	}
	return db, conv, true
}

// POST /api/conversation/message: append to the transcript.
//
// Persisted here rather than only broadcast, so a message the other side has
// seen is always one the record already holds. The sender is taken from the
// TOKEN's role, never from the request body — otherwise a guest could write
// messages attributed to the agent.
func (a *authApp) conversationMessageHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	var body struct {
		Ref   string `json:"ref"`
		CID   string `json:"cid"`
		Token string `json:"token"`
		Body  string `json:"body"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		errJSON(w, 400, "bad json")
		return
	}
	t, terr := parseConvToken(a.ssoSecret, body.Token)
	db, _, ok := authConv(body.Ref, body.CID, body.Token)
	if terr != nil || !ok {
		errJSON(w, 403, "forbidden")
		return
	}
	text := strings.TrimSpace(body.Body)
	if text == "" {
		errJSON(w, 400, "empty message")
		return
	}
	if len(text) > 4000 {
		text = text[:4000]
	}
	now := time.Now().Unix()
	res, err := db.Exec(
		`INSERT INTO chat_messages (cid, sender, body, created_at) VALUES (?, ?, ?, ?)`,
		body.CID, t.Role, text, now)
	if err != nil {
		errJSON(w, 500, "store failed")
		return
	}
	// Only a VISITOR message refreshes activity. An agent replying into a thread
	// the visitor has abandoned must not keep that slot occupied — otherwise an
	// agent could hold their own capacity open indefinitely without meaning to.
	if t.Role == convRoleGuest {
		_, _ = db.Exec(`UPDATE conversations SET last_activity_at = ? WHERE cid = ?`, now, body.CID)
	}

	id, _ := res.LastInsertId()
	msg := map[string]any{
		"id": id, "cid": body.CID, "sender": t.Role, "body": text, "created_at": now,
	}

	// A chat message from a visitor is not a ring — it is a message arriving.
	// Deliver it to the agent's user-keyed inbox, which every one of their live
	// consoles is already subscribed to, so a console that has not (yet) opened
	// this conversation still learns about it.
	//
	// This is why chat does not go through /api/call/ring: ringing implies an
	// accept/decline decision with a 30s deadline and a repeating alert, none of
	// which fit a message someone typed.
	// ONE broadcast, from the server, to the conversation channel. The client
	// used to publish this itself, which meant a subscribed console received the
	// message twice — once here and once over the channel — and rendered it
	// twice. The server is the only writer now.
	convMsg, _ := json.Marshal(msg)
	hub.broadcast(convChannel(body.CID), "message", convMsg)

	if t.Role == convRoleGuest {
		conv, cerr := conversationByCID(db, body.CID)
		if cerr == nil {
			// Separate notification for consoles that have NOT subscribed to
			// this conversation yet (they cannot have: they had no capability
			// for it). Carries guestSession so the console can recognise this
			// as the SAME person it already lists from presence, instead of
			// showing them twice under two different keys.
			notify, _ := json.Marshal(map[string]any{
				"type":         "chat-message",
				"cid":          body.CID,
				"guestName":    conv.GuestName,
				"guestSession": conv.GuestSession,
				"message":      msg,
			})
			hub.broadcast(userInboxChannel(conv.Ref, conv.AgentUserID), "message", notify)
			// And wake a closed console. One notification, not the ring loop.
			go pushChatMessage(conv, text)
		}
	}

	writeJSON(w, 200, map[string]any{"message": msg})
}

// pushChatMessage sends a single Web Push for an incoming chat message.
//
// Deliberately NOT the ring's re-push loop: a chat has no deadline to beat, so
// re-alerting every few seconds would be nagging rather than helping.
func pushChatMessage(conv conversation, text string) {
	if !pushEnabled() {
		return
	}
	db, err := dbs.get(conv.Ref)
	if err != nil {
		return
	}
	subs := subsForUser(db, conv.Ref, conv.AgentUserID)
	if len(subs) == 0 {
		return
	}
	who := conv.GuestName
	if who == "" {
		who = "A visitor"
	}
	payload, _ := json.Marshal(map[string]any{
		"type": "chat-message", "ref": conv.Ref, "cid": conv.CID,
		"callerName": who, "body": safePrefix(text, 120),
	})
	for _, s := range subs {
		sendWebPush(db, s, payload)
	}
}

// GET /api/conversation/messages?ref=&cid=&token=: the transcript.
//
// This is what stops a push-woken agent opening a fresh console to an empty
// thread while the guest can see everything they typed.
func (a *authApp) conversationMessagesHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	q := r.URL.Query()
	db, _, ok := authConv(q.Get("ref"), q.Get("cid"), q.Get("token"))
	if !ok {
		errJSON(w, 403, "forbidden")
		return
	}
	rows, err := db.Query(
		`SELECT id, sender, body, created_at FROM chat_messages WHERE cid = ? ORDER BY id`, q.Get("cid"))
	if err != nil {
		writeJSON(w, 200, map[string]any{"messages": []any{}})
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, created int64
		var sender, msgBody string
		if err := rows.Scan(&id, &sender, &msgBody, &created); err == nil {
			out = append(out, map[string]any{
				"id": id, "sender": sender, "body": msgBody, "created_at": created,
			})
		}
	}
	writeJSON(w, 200, map[string]any{"messages": out})
}

// POST /api/guest/session (public, ref-controlled): mint a visitor's session id
// AND the capability for their private inbox, together.
//
// The server mints the id rather than accepting one, because an agent can see a
// visitor's session id in presence. If the client chose it, anyone who saw it
// could request a token for it and read that visitor's invitations. Minting
// both at once means seeing the id is not enough to obtain the capability.
func (a *authApp) guestSessionHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	var body struct {
		Ref string `json:"ref"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		errJSON(w, 400, "bad json")
		return
	}
	if body.Ref == "" || !dbs.exists(body.Ref) {
		errJSON(w, 404, "unknown tenant")
		return
	}
	sid, err := newConvID()
	if err != nil {
		errJSON(w, 500, "internal error")
		return
	}
	tok, err := mintConvToken(a.ssoSecret, sid, roleGuestSession)
	if err != nil {
		errJSON(w, 500, "internal error")
		return
	}
	writeJSON(w, 200, map[string]any{
		"sessionId": sid,
		"token":     tok,
		"channel":   guestChannel(sid),
	})
}

// POST /api/conversation/invite (authed): an AGENT opens contact with a
// visitor they can see in the console's waiting list.
//
// The invitation — including the visitor's capability for the new conversation
// — is delivered BY THE SERVER to that visitor's private inbox. The agent never
// holds a grant for someone else's inbox, and so cannot write into it directly:
// if they could, so could anyone else who learned a session id.
func (a *authApp) conversationInviteHandler(w http.ResponseWriter, r *http.Request) {
	info := authFrom(r.Context())
	if info == nil {
		errJSON(w, 401, "not signed in")
		return
	}
	var body struct {
		GuestSession string `json:"guestSession"`
		GuestName    string `json:"guestName"`
		CallType     string `json:"callType"`
		CallerName   string `json:"callerName"`
		CallID       string `json:"callId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		errJSON(w, 400, "bad json")
		return
	}
	if body.GuestSession == "" {
		errJSON(w, 400, "missing fields")
		return
	}
	switch body.CallType {
	case callTypeChat, callTypeAudio, callTypeVideo:
	default:
		errJSON(w, 400, "unknown call type")
		return
	}

	// Clicking a visitor OPENS the conversation with them — existing or new.
	// Creating unconditionally is what put the two of them in separate
	// conversations, each seeing only their own messages.
	cid := ""
	isNew := false
	if existing, ok := openConversationFor(info.db, info.ref, body.GuestSession, body.CallType); ok {
		cid = existing.CID
	} else {
		var err error
		cid, err = newConvID()
		if err != nil {
			errJSON(w, 500, "internal error")
			return
		}
		if err := createConversation(info.db, conversation{
			CID: cid, Ref: info.ref, GuestSession: body.GuestSession,
			GuestName: strings.TrimSpace(body.GuestName), AgentUserID: info.user.ID,
			CallType: body.CallType, CreatedAt: time.Now().Unix(),
		}); err != nil {
			errJSON(w, 500, "store failed")
			return
		}
		isNew = true
	}
	agentTok, err := mintConvToken(a.ssoSecret, cid, convRoleAgent)
	if err != nil {
		errJSON(w, 500, "internal error")
		return
	}
	guestTok, err := mintConvToken(a.ssoSecret, cid, convRoleGuest)
	if err != nil {
		errJSON(w, 500, "internal error")
		return
	}
	callID := body.CallID
	if callID == "" {
		callID = cid
	}
	// Only a NEW conversation is announced. Re-announcing one the visitor
	// already has open would make their dock switch to a thread they are
	// already in, which is how the visitor's own first message appeared to
	// vanish: the view moved, the message did not.
	if isNew {
		payload, _ := json.Marshal(map[string]any{
			"type":       "incoming-call",
			"cid":        cid,
			"token":      guestTok, // the visitor's capability for THIS conversation
			"callId":     callID,
			"callType":   body.CallType,
			"callerName": body.CallerName,
		})
		hub.broadcast(guestChannel(body.GuestSession), "message", payload)
	}

	writeJSON(w, 200, map[string]any{
		"cid": cid, "token": agentTok, "channel": convChannel(cid),
		"callId": callID, "resumed": !isNew,
	})
}

// GET /api/conversations (authed): this agent's open conversations.
//
// Without this a console only learns of a conversation when a message happens
// to arrive, so a reload silently emptied the agent's list while the visitor
// carried on typing into it. Persistence is not just storing the transcript —
// it is being able to come back to the conversation at all.
func (a *authApp) conversationsListHandler(w http.ResponseWriter, r *http.Request) {
	info := authFrom(r.Context())
	if info == nil {
		errJSON(w, 401, "not signed in")
		return
	}
	rows, err := info.db.Query(
		`SELECT cid, guest_session, guest_name, call_type, created_at, last_activity_at
		   FROM conversations
		  WHERE agent_user_id = ? AND ended_at IS NULL
		  ORDER BY last_activity_at DESC`, info.user.ID)
	if err != nil {
		writeJSON(w, 200, map[string]any{"conversations": []any{}})
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var cid, gs, name, ct string
		var created, act int64
		if err := rows.Scan(&cid, &gs, &name, &ct, &created, &act); err != nil {
			continue
		}
		tok, terr := mintConvToken(a.ssoSecret, cid, convRoleAgent)
		if terr != nil {
			continue
		}
		out = append(out, map[string]any{
			"cid": cid, "token": tok, "channel": convChannel(cid),
			"guestSession": gs, "guestName": name, "callType": ct,
			"createdAt": created, "lastActivityAt": act,
		})
	}
	writeJSON(w, 200, map[string]any{"conversations": out})
}

func (a *authApp) mountConversations(mux *http.ServeMux) {
	mux.Handle("GET /api/conversations", a.authedJSON(a.conversationsListHandler))
	mux.HandleFunc("POST /api/guest/session", a.guestSessionHandler)
	mux.Handle("POST /api/conversation/invite", a.authedJSON(a.conversationInviteHandler))
	mux.HandleFunc("POST /api/conversation/start", a.conversationStartHandler)
	mux.Handle("GET /api/conversation/token", a.authedJSON(a.conversationTokenHandler))
	mux.HandleFunc("POST /api/conversation/end", a.conversationEndHandler)
	mux.HandleFunc("POST /api/conversation/message", a.conversationMessageHandler)
	mux.HandleFunc("GET /api/conversation/messages", a.conversationMessagesHandler)
}
