package main

import (
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"testing"
)

// TestOnlineDoesNotAutoCreateTenant: /api/online is public + CORS-* and called
// from arbitrary tenant sites (the chat-widget presence poll), so an unknown ref
// must report "nobody online" WITHOUT spawning an empty tenant DB — otherwise
// anyone could create tenants by guessing refs.
func TestOnlineDoesNotAutoCreateTenant(t *testing.T) {
	newServer(t) // sets up the package-global dbs with a temp data dir
	const ghost = "never-provisioned.example"

	req := httptest.NewRequest("GET", "/api/online?ref="+url.QueryEscape(ghost), nil)
	w := httptest.NewRecorder()
	onlineHandler(w, req)

	if w.Code != 200 {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	var r struct {
		Online bool `json:"online"`
		Count  int  `json:"count"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &r); err != nil {
		t.Fatalf("bad json: %s", w.Body.String())
	}
	if r.Online || r.Count != 0 {
		t.Fatalf("unknown ref should be offline/0, got %+v", r)
	}
	// The key assertion: the presence query must NOT have created a tenant DB.
	if dbs.exists(ghost) {
		t.Fatalf("/api/online auto-created a tenant DB for unknown ref %q", ghost)
	}
}
