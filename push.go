// push.go — Phase 2: Web Push delivery so an Available agent is woken and rung
// even when the console tab is backgrounded, minimised, or closed.
//
// A WebSocket can't survive a throttled/suspended background tab, so call
// delivery rides the OS push service instead. Push is *additive*: the existing
// inbox-over-WS relay stays as the instant path when the tab is open; when it
// isn't, the push wakes the service worker and a pending-invite lookup
// re-hydrates the call on reopen. If VAPID isn't configured or the agent hasn't
// granted push, everything behaves exactly as before (no regression).
package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"
)

// ───────────────────────── VAPID configuration ─────────────────────────────

type vapidConfig struct {
	publicKey  string
	privateKey string
	subject    string // "mailto:ops@example.com"
}

var vapid vapidConfig

func initPush(publicKey, privateKey, subject string) {
	// webpush-go prepends "mailto:" itself (unless the subscriber is an https
	// URL), so a subject configured WITH the scheme goes out as
	// "mailto:mailto:…" in the JWT sub claim. Apple validates the claim and
	// rejects every push with 403 BadJwtToken; FCM doesn't validate sub, which
	// is how this stayed invisible from Chrome's side. Store the bare address.
	subject = strings.TrimPrefix(strings.TrimSpace(subject), "mailto:")
	vapid = vapidConfig{publicKey: publicKey, privateKey: privateKey, subject: subject}
	if subject == "" {
		vapid.subject = "admin@localhost"
	}
	if pushEnabled() {
		log.Printf("[Push] Web Push enabled (VAPID public key %s…)", safePrefix(publicKey, 12))
	} else {
		log.Printf("[Push] Web Push disabled (set VAPID_PUBLIC_KEY + VAPID_PRIVATE_KEY to enable)")
	}
}

func pushEnabled() bool { return vapid.publicKey != "" && vapid.privateKey != "" }

// printVAPIDKeys generates a fresh VAPID keypair and prints it as env lines, for
// `live-support -genvapid`. Paste the output into the appliance .env.
func printVAPIDKeys() error {
	priv, pub, err := webpush.GenerateVAPIDKeys()
	if err != nil {
		return err
	}
	// fmt, not the println builtin — the builtin writes to stderr, which
	// breaks `live-support -genvapid > keys.env` captures.
	fmt.Println("VAPID_PUBLIC_KEY=" + pub)
	fmt.Println("VAPID_PRIVATE_KEY=" + priv)
	return nil
}

func safePrefix(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// ───────────────────────── pending call invites ────────────────────────────
// In-memory, short-lived (a ringing call). Keyed by callId; looked up by the
// authenticated (ref,userID) so a push-woken agent that reopened with a *new*
// presence session_id still finds the call it was rung for.

type pendingInvite struct {
	ref       string
	userID    int64
	callID    string
	fromName  string
	callType  string
	expiresAt time.Time
}

var (
	invitesMu sync.Mutex
	invites   = map[string]pendingInvite{} // callID -> invite
)

const inviteTTL = 30 * time.Second

func putInvite(inv pendingInvite) {
	inv.expiresAt = time.Now().Add(inviteTTL)
	invitesMu.Lock()
	pruneInvitesLocked()
	invites[inv.callID] = inv
	invitesMu.Unlock()
}

func dropInvite(callID string) {
	invitesMu.Lock()
	delete(invites, callID)
	invitesMu.Unlock()
}

func invitesForUser(ref string, userID int64) []pendingInvite {
	now := time.Now()
	invitesMu.Lock()
	defer invitesMu.Unlock()
	pruneInvitesLocked()
	out := []pendingInvite{}
	for _, inv := range invites {
		if inv.ref == ref && inv.userID == userID && inv.expiresAt.After(now) {
			out = append(out, inv)
		}
	}
	return out
}

func pruneInvitesLocked() {
	now := time.Now()
	for id, inv := range invites {
		if !inv.expiresAt.After(now) {
			delete(invites, id)
		}
	}
}

// ───────────────────────── subscription storage ────────────────────────────

type pushSub struct {
	userID   int64
	endpoint string
	p256dh   string
	auth     string
}

func upsertSubscription(db *sql.DB, ref string, userID int64, sessionID string, s pushSub) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := db.Exec(
		`INSERT INTO push_subscriptions (ref, user_id, session_id, endpoint, p256dh, auth, created_at, last_seen_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(endpoint) DO UPDATE SET
		   ref = excluded.ref,
		   user_id = excluded.user_id,
		   session_id = excluded.session_id,
		   p256dh = excluded.p256dh,
		   auth = excluded.auth,
		   last_seen_at = excluded.last_seen_at`,
		ref, userID, sessionID, s.endpoint, s.p256dh, s.auth, now, now)
	return err
}

func deleteSubscription(db *sql.DB, endpoint string) {
	_, _ = db.Exec(`DELETE FROM push_subscriptions WHERE endpoint = ?`, endpoint)
}

// subsForSession returns the push subscriptions registered against a presence
// session_id (the target the guest is calling).
func subsForSession(db *sql.DB, ref, sessionID string) []pushSub {
	return querySubs(db,
		`SELECT user_id, endpoint, p256dh, auth FROM push_subscriptions WHERE ref = ? AND session_id = ?`,
		ref, sessionID)
}

// subsForUser returns ALL of a user's push subscriptions — every browser they
// enabled push in (e.g. Safari and Chrome) rings, not just the session the
// guest happened to target.
func subsForUser(db *sql.DB, ref string, userID int64) []pushSub {
	return querySubs(db,
		`SELECT user_id, endpoint, p256dh, auth FROM push_subscriptions WHERE ref = ? AND user_id = ?`,
		ref, userID)
}

func querySubs(db *sql.DB, query string, args ...any) []pushSub {
	rows, err := db.Query(query, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []pushSub
	for rows.Next() {
		var s pushSub
		if err := rows.Scan(&s.userID, &s.endpoint, &s.p256dh, &s.auth); err == nil {
			out = append(out, s)
		}
	}
	return out
}

// sendWebPush delivers one notification; prunes the row on a dead endpoint.
func sendWebPush(db *sql.DB, s pushSub, payload []byte) {
	sub := &webpush.Subscription{
		Endpoint: s.endpoint,
		Keys:     webpush.Keys{P256dh: s.p256dh, Auth: s.auth},
	}
	resp, err := webpush.SendNotification(payload, sub, &webpush.Options{
		Subscriber:      vapid.subject,
		VAPIDPublicKey:  vapid.publicKey,
		VAPIDPrivateKey: vapid.privateKey,
		TTL:             30,
		Urgency:         webpush.UrgencyHigh,
	})
	if err != nil {
		log.Printf("[Push] send error: %v", err)
		return
	}
	defer resp.Body.Close()
	// 404/410 mean the subscription is gone — prune it so we stop trying.
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone {
		log.Printf("[Push] pruning dead endpoint (%d): %s…", resp.StatusCode, safePrefix(s.endpoint, 40))
		deleteSubscription(db, s.endpoint)
		return
	}
	// Anything else non-2xx is a rejection we must SEE (Apple in particular
	// rejects with 400/403 on VAPID problems, which used to vanish silently).
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		body := make([]byte, 256)
		n, _ := resp.Body.Read(body)
		log.Printf("[Push] rejected %d by %s…: %s", resp.StatusCode, safePrefix(s.endpoint, 40), string(body[:n]))
	}
}

// ───────────────────────── HTTP handlers ───────────────────────────────────

// POST /api/push/subscribe (authed): register/refresh this browser's push
// subscription against the current presence session.
func pushSubscribeHandler(w http.ResponseWriter, r *http.Request) {
	info := authFrom(r.Context())
	if info == nil {
		errJSON(w, 401, "not signed in")
		return
	}
	var body struct {
		SessionID    string `json:"sessionId"`
		Subscription struct {
			Endpoint string `json:"endpoint"`
			Keys     struct {
				P256dh string `json:"p256dh"`
				Auth   string `json:"auth"`
			} `json:"keys"`
		} `json:"subscription"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		errJSON(w, 400, "bad json")
		return
	}
	if body.Subscription.Endpoint == "" || body.Subscription.Keys.P256dh == "" || body.Subscription.Keys.Auth == "" {
		errJSON(w, 400, "incomplete subscription")
		return
	}
	if err := upsertSubscription(info.db, info.ref, info.user.ID, body.SessionID, pushSub{
		endpoint: body.Subscription.Endpoint,
		p256dh:   body.Subscription.Keys.P256dh,
		auth:     body.Subscription.Keys.Auth,
	}); err != nil {
		errJSON(w, 500, "store failed")
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// POST /api/push/unsubscribe (authed): drop a subscription by endpoint.
func pushUnsubscribeHandler(w http.ResponseWriter, r *http.Request) {
	info := authFrom(r.Context())
	if info == nil {
		errJSON(w, 401, "not signed in")
		return
	}
	var body struct {
		Endpoint string `json:"endpoint"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Endpoint == "" {
		errJSON(w, 400, "bad json")
		return
	}
	deleteSubscription(info.db, body.Endpoint)
	writeJSON(w, 200, map[string]any{"ok": true})
}

// POST /api/call/ring (public, ref-controlled): record a pending invite and push
// to the target agent's subscriptions. Called by the guest when initiating.
func callRingHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	var body struct {
		Ref        string `json:"ref"`
		ToSession  string `json:"toSession"`
		CallID     string `json:"callId"`
		CallType   string `json:"callType"`
		CallerName string `json:"callerName"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		errJSON(w, 400, "bad json")
		return
	}
	if body.Ref == "" || body.ToSession == "" || body.CallID == "" {
		errJSON(w, 400, "missing fields")
		return
	}
	if !dbs.exists(body.Ref) {
		writeJSON(w, 200, map[string]any{"pushed": 0})
		return
	}
	db, err := dbs.get(body.Ref)
	if err != nil {
		errJSON(w, 400, err.Error())
		return
	}
	subs := subsForSession(db, body.Ref, body.ToSession)
	log.Printf("[Ring] ref=%s to=%s… call=%s… subs=%d",
		body.Ref, safePrefix(body.ToSession, 8), safePrefix(body.CallID, 8), len(subs))
	if len(subs) == 0 {
		// No push subscription for this agent — instant inbox relay handles it.
		writeJSON(w, 200, map[string]any{"pushed": 0})
		return
	}
	// Durable-availability gate: a Paused or logged-out agent must never be
	// pushed, even when a guest holds a stale roster naming their session.
	userID := subs[0].userID
	if !userIsAvailable(db, userID) {
		log.Printf("[Ring] dropped: user %d not available", userID)
		writeJSON(w, 200, map[string]any{"pushed": 0})
		return
	}
	// Ring every browser this agent enabled push in, not just the targeted
	// session — the agent answers wherever they see it first.
	if all := subsForUser(db, body.Ref, userID); len(all) > 0 {
		subs = all
	}
	callType := body.CallType
	if callType != "video" {
		callType = "audio"
	}
	callerName := body.CallerName
	if callerName == "" {
		callerName = "Someone"
	}
	// Record the invite against the target user so a reopened console finds it.
	putInvite(pendingInvite{
		ref:      body.Ref,
		userID:   subs[0].userID,
		callID:   body.CallID,
		fromName: callerName,
		callType: callType,
	})
	if pushEnabled() {
		payload, _ := json.Marshal(map[string]any{
			"type":       "incoming-call",
			"ref":        body.Ref,
			"callId":     body.CallID,
			"callType":   callType,
			"callerName": callerName,
		})
		// Send in the background so a slow/dead push endpoint never delays the
		// guest's ring request (which is waiting on this response).
		go func(subs []pushSub) {
			for _, s := range subs {
				sendWebPush(db, s, payload)
			}
		}(subs)
	}
	writeJSON(w, 200, map[string]any{"pushed": len(subs)})
}

// POST /api/call/ring/clear (public, ref-controlled): drop a pending invite when
// the guest cancels or the call times out.
func callRingClearHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	var body struct {
		CallID string `json:"callId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.CallID == "" {
		errJSON(w, 400, "bad json")
		return
	}
	dropInvite(body.CallID)
	writeJSON(w, 200, map[string]any{"ok": true})
}

// GET /api/call/pending (authed): pending invites for this user — used by the
// console on load (e.g. after the agent clicked a push notification).
func callPendingHandler(w http.ResponseWriter, r *http.Request) {
	info := authFrom(r.Context())
	if info == nil {
		errJSON(w, 401, "not signed in")
		return
	}
	list := invitesForUser(info.ref, info.user.ID)
	out := make([]map[string]any, 0, len(list))
	for _, inv := range list {
		out = append(out, map[string]any{
			"callId":     inv.callID,
			"callType":   inv.callType,
			"callerName": inv.fromName,
		})
	}
	writeJSON(w, 200, map[string]any{"invites": out})
}

// mountPush registers the push routes. Authed routes go through the authApp
// middleware so they resolve the tenant + user from the session cookie.
func mountPush(mux *http.ServeMux, a *authApp) {
	mux.Handle("POST /api/push/subscribe", a.authedJSON(pushSubscribeHandler))
	mux.Handle("POST /api/push/unsubscribe", a.authedJSON(pushUnsubscribeHandler))
	mux.Handle("GET /api/call/pending", a.authedJSON(callPendingHandler))
	mux.HandleFunc("POST /api/call/ring", callRingHandler)
	mux.HandleFunc("POST /api/call/ring/clear", callRingClearHandler)
}
