package main

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestPushSubscriptionUpsert: a re-subscribe from the same browser (same
// endpoint) must update the row in place — refreshing session_id — not pile up
// duplicate rows, and subsForSession must resolve the current session.
func TestPushSubscriptionUpsert(t *testing.T) {
	_, db := newServer(t)

	const ep = "https://push.example/abc"
	if err := upsertSubscription(db, testRef, 7, "sess-1", pushSub{endpoint: ep, p256dh: "k1", auth: "a1"}); err != nil {
		t.Fatalf("upsert 1: %v", err)
	}
	// Re-subscribe with a new presence session (agent reopened the tab).
	if err := upsertSubscription(db, testRef, 7, "sess-2", pushSub{endpoint: ep, p256dh: "k2", auth: "a2"}); err != nil {
		t.Fatalf("upsert 2: %v", err)
	}

	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM push_subscriptions WHERE endpoint = ?`, ep).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("re-subscribe should upsert, got %d rows", n)
	}

	// Old session no longer resolves; new session does.
	if got := subsForSession(db, testRef, "sess-1"); len(got) != 0 {
		t.Fatalf("stale session should resolve to no subs, got %d", len(got))
	}
	got := subsForSession(db, testRef, "sess-2")
	if len(got) != 1 || got[0].p256dh != "k2" {
		t.Fatalf("current session should resolve the refreshed sub, got %+v", got)
	}

	deleteSubscription(db, ep)
	if got := subsForSession(db, testRef, "sess-2"); len(got) != 0 {
		t.Fatalf("delete should remove the sub, got %d", len(got))
	}
}

// TestPendingInviteLifecycle covers the in-memory invite store used to
// re-hydrate a call after the agent is woken and reopens the console.
func TestPendingInviteLifecycle(t *testing.T) {
	dropInvite("call-A")
	dropInvite("call-B")

	putInvite(pendingInvite{ref: "demo", userID: 5, callID: "call-A", fromName: "Ada", callType: "audio"})
	putInvite(pendingInvite{ref: "demo", userID: 5, callID: "call-B", fromName: "Bo", callType: "video"})
	putInvite(pendingInvite{ref: "demo", userID: 9, callID: "call-C", fromName: "Cy", callType: "audio"})
	putInvite(pendingInvite{ref: "other", userID: 5, callID: "call-D", fromName: "Di", callType: "audio"})

	got := invitesForUser("demo", 5)
	if len(got) != 2 {
		t.Fatalf("user 5 in demo should have 2 invites, got %d", len(got))
	}
	dropInvite("call-A")
	if len(invitesForUser("demo", 5)) != 1 {
		t.Fatalf("after drop, user 5 should have 1 invite")
	}
	// Expiry: a past-dated invite is pruned on lookup.
	invitesMu.Lock()
	invites["call-old"] = pendingInvite{ref: "demo", userID: 5, callID: "call-old", expiresAt: time.Now().Add(-time.Second)}
	invitesMu.Unlock()
	for _, inv := range invitesForUser("demo", 5) {
		if inv.callID == "call-old" {
			t.Fatal("expired invite should not be returned")
		}
	}
}

// TestCallRingFlow: ring records a pending invite for the target user and
// GET /api/call/pending returns it; clear removes it. Push send is skipped
// (no VAPID configured in tests), so this exercises the wiring without network.
func TestCallRingFlow(t *testing.T) {
	_, db := newServer(t)

	// A user + their push subscription against presence session "agent-sess".
	res, err := db.Exec(
		`INSERT INTO users(username, password_hash, role, must_change_pw, active, created_at) VALUES (?,?,?,?,?,?)`,
		"agent1", "x", "agent", 0, 1, time.Now().Unix())
	if err != nil {
		t.Fatal(err)
	}
	uid, _ := res.LastInsertId()
	if err := upsertSubscription(db, testRef, uid, "agent-sess", pushSub{endpoint: "https://push.example/x", p256dh: "k", auth: "a"}); err != nil {
		t.Fatal(err)
	}

	ring := func() int {
		body := `{"ref":"` + testRef + `","toSession":"agent-sess","callId":"call-X","callType":"audio","callerName":"Guest Q"}`
		req := httptest.NewRequest("POST", "/api/call/ring", strings.NewReader(body))
		w := httptest.NewRecorder()
		callRingHandler(w, req)
		if w.Code != 200 {
			t.Fatalf("ring: want 200, got %d: %s", w.Code, w.Body.String())
		}
		var ringResp struct {
			Pushed int `json:"pushed"`
		}
		json.Unmarshal(w.Body.Bytes(), &ringResp)
		return ringResp.Pushed
	}

	// Durable-availability gate: a user who never went Available isn't pushed.
	if got := ring(); got != 0 {
		t.Fatalf("ring before going Available should push 0, got %d", got)
	}

	// Agent goes Available → ring pushes.
	if err := upsertAvailability(db, testRef, uid, true, "agent-sess", "Agent One", false, "", ""); err != nil {
		t.Fatal(err)
	}
	if got := ring(); got != 1 {
		t.Fatalf("ring should target 1 subscription, got %d", got)
	}

	// Multi-device: a second browser's subscription also rings.
	if err := upsertSubscription(db, testRef, uid, "other-sess", pushSub{endpoint: "https://push.example/y", p256dh: "k", auth: "a"}); err != nil {
		t.Fatal(err)
	}
	if got := ring(); got != 2 {
		t.Fatalf("ring should target both of the user's subscriptions, got %d", got)
	}

	// Pause (or logout) closes the gate again.
	clearAvailability(db, uid)
	if got := ring(); got != 0 {
		t.Fatalf("ring after Pause should push 0, got %d", got)
	}
	if err := upsertAvailability(db, testRef, uid, true, "agent-sess", "Agent One", false, "", ""); err != nil {
		t.Fatal(err)
	}

	// The agent's console fetches pending invites (authed context).
	pendReq := httptest.NewRequest("GET", "/api/call/pending", nil)
	info := &authInfo{ref: testRef, db: db, user: &User{ID: uid}}
	pendReq = pendReq.WithContext(context.WithValue(pendReq.Context(), authKey, info))
	pw := httptest.NewRecorder()
	callPendingHandler(pw, pendReq)
	if pw.Code != 200 {
		t.Fatalf("pending: want 200, got %d: %s", pw.Code, pw.Body.String())
	}
	var pend struct {
		Invites []struct {
			CallID     string `json:"callId"`
			CallType   string `json:"callType"`
			CallerName string `json:"callerName"`
		} `json:"invites"`
	}
	json.Unmarshal(pw.Body.Bytes(), &pend)
	if len(pend.Invites) != 1 || pend.Invites[0].CallID != "call-X" || pend.Invites[0].CallerName != "Guest Q" {
		t.Fatalf("pending should return the ringing call, got %+v", pend.Invites)
	}

	// Guest cancels → clear removes the invite.
	clrReq := httptest.NewRequest("POST", "/api/call/ring/clear", strings.NewReader(`{"callId":"call-X"}`))
	cw := httptest.NewRecorder()
	callRingClearHandler(cw, clrReq)
	if cw.Code != 200 {
		t.Fatalf("clear: want 200, got %d", cw.Code)
	}
	if len(invitesForUser(testRef, uid)) != 0 {
		t.Fatal("clear should drop the pending invite")
	}
}
