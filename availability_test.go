package main

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"
)

// TestAgentDiscovery: GET /api/agents/available lists only agents who are BOTH
// durably available AND reachable (≥1 push subscription), and the gate closes
// on Pause.
func TestAgentDiscovery(t *testing.T) {
	_, db := newServer(t)

	res, err := db.Exec(
		`INSERT INTO users(username, password_hash, role, must_change_pw, active, created_at) VALUES (?,?,?,?,?,?)`,
		"agent2", "x", "agent", 0, 1, time.Now().Unix())
	if err != nil {
		t.Fatal(err)
	}
	uid, _ := res.LastInsertId()

	list := func() []map[string]any {
		req := httptest.NewRequest("GET", "/api/agents/available?ref="+testRef, nil)
		w := httptest.NewRecorder()
		agentsAvailableHandler(w, req)
		if w.Code != 200 {
			t.Fatalf("agents: want 200, got %d: %s", w.Code, w.Body.String())
		}
		var out struct {
			Agents []map[string]any `json:"agents"`
		}
		json.Unmarshal(w.Body.Bytes(), &out)
		return out.Agents
	}

	// Available but NO push subscription → unreachable → hidden.
	if err := upsertAvailability(db, testRef, uid, true, "sess-2", "Reachable Agent", true, "", "2026-01-01T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
	if got := list(); len(got) != 0 {
		t.Fatalf("agent without push subscription should be hidden, got %v", got)
	}

	// Subscribed → discoverable, with the durable record's fields.
	if err := upsertSubscription(db, testRef, uid, "sess-2", pushSub{endpoint: "https://push.example/z", p256dh: "k", auth: "a"}); err != nil {
		t.Fatal(err)
	}
	got := list()
	if len(got) != 1 {
		t.Fatalf("want 1 discoverable agent, got %d", len(got))
	}
	if got[0]["session_id"] != "sess-2" || got[0]["name"] != "Reachable Agent" || got[0]["has_camera"] != true {
		t.Fatalf("unexpected agent record: %v", got[0])
	}

	// Pause hides them again.
	clearAvailability(db, uid)
	if got := list(); len(got) != 0 {
		t.Fatalf("paused agent should be hidden, got %v", got)
	}
}
