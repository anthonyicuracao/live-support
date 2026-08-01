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

// Conversation roles. Stored in the token so the server can tell the two
// participants apart without trusting anything the client says.
const (
	convRoleGuest = "guest"
	convRoleAgent = "agent"
)

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
	if t.CID == "" || (t.Role != convRoleGuest && t.Role != convRoleAgent) {
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

func createConversation(db *sql.DB, c conversation) error {
	_, err := db.Exec(
		`INSERT INTO conversations (cid, ref, guest_session, guest_name, agent_user_id, call_type, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		c.CID, c.Ref, c.GuestSession, c.GuestName, c.AgentUserID, c.CallType, c.CreatedAt)
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

// agentOpenChatCount counts a user's live chat conversations — the input to the
// max_concurrent_chats governor.
func agentOpenChatCount(db *sql.DB, userID int64) int {
	var n int
	_ = db.QueryRow(
		`SELECT COUNT(*) FROM conversations
		  WHERE agent_user_id = ? AND call_type = ? AND ended_at IS NULL`,
		userID, callTypeChat).Scan(&n)
	return n
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
		Ref          string `json:"ref"`
		AgentUserID  int64  `json:"agentUserId"`
		CallType     string `json:"callType"`
		GuestSession string `json:"guestSession"`
		GuestName    string `json:"guestName"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		errJSON(w, 400, "bad json")
		return
	}
	if body.Ref == "" || body.AgentUserID == 0 || body.GuestSession == "" {
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
	// The agent must be available AND take this modality. Same gate the ring
	// path applies, enforced here too so a conversation is never created for a
	// contact that could not be delivered.
	if !userTakesCallType(db, body.AgentUserID, body.CallType) {
		errJSON(w, 409, "agent unavailable")
		return
	}
	// Concurrency governor: chat may run alongside a call, but not without
	// bound. Enforced server-side because a stale roster is the normal case.
	if body.CallType == callTypeChat && agentOpenChatCount(db, body.AgentUserID) >= maxConcurrentChats() {
		errJSON(w, 429, "agent at chat capacity")
		return
	}

	cid, err := newConvID()
	if err != nil {
		errJSON(w, 500, "internal error")
		return
	}
	conv := conversation{
		CID: cid, Ref: body.Ref, GuestSession: body.GuestSession,
		GuestName: strings.TrimSpace(body.GuestName), AgentUserID: body.AgentUserID,
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
	writeJSON(w, 200, map[string]any{
		"cid":     cid,
		"token":   tok,
		"channel": convChannel(cid),
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
		"cid":       cid,
		"token":     tok,
		"channel":   convChannel(cid),
		"callType":  conv.CallType,
		"guestName": conv.GuestName,
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
	id, _ := res.LastInsertId()
	writeJSON(w, 200, map[string]any{"message": map[string]any{
		"id": id, "cid": body.CID, "sender": t.Role, "body": text, "created_at": now,
	}})
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

func (a *authApp) mountConversations(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/conversation/start", a.conversationStartHandler)
	mux.Handle("GET /api/conversation/token", a.authedJSON(a.conversationTokenHandler))
	mux.HandleFunc("POST /api/conversation/end", a.conversationEndHandler)
	mux.HandleFunc("POST /api/conversation/message", a.conversationMessageHandler)
	mux.HandleFunc("GET /api/conversation/messages", a.conversationMessagesHandler)
}
