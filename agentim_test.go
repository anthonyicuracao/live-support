package main

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

func postJSONObj(t *testing.T, c *http.Client, target string, body any) (int, map[string]any) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	resp, err := c.Post(target, "application/json", strings.NewReader(string(raw)))
	if err != nil {
		t.Fatalf("POST %s: %v", target, err)
	}
	defer resp.Body.Close()
	out := map[string]any{}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func TestAgentIMReachesTheRecipientsInbox(t *testing.T) {
	srv, db := newServer(t)
	admin := loginAdmin(t, srv)
	inviteAgent(t, srv, admin, "gaby")

	gabyID, err := strconv.ParseInt(userIDByName(t, db, "gaby"), 10, 64)
	if err != nil {
		t.Fatalf("gaby id: %v", err)
	}

	console := &Conn{send: make(chan ServerMsg, 4), subs: map[string]bool{}, tracked: map[string]string{}}
	hub.subscribe(console, userInboxChannel(testRef, gabyID))

	status, out := postJSONObj(t, admin, wr(srv, "/api/im/send"), map[string]any{
		"toUserId": gabyID,
		"body":     "can you take the chat in the queue?",
	})
	if status != 200 {
		t.Fatalf("send: got %d, want 200 (%v)", status, out)
	}
	if out["delivered"] != true {
		t.Fatalf("send reported delivered=%v, want true — a live console was subscribed", out["delivered"])
	}

	select {
	case msg := <-console.send:
		var got map[string]any
		if err := json.Unmarshal(msg.Payload, &got); err != nil {
			t.Fatalf("payload: %v", err)
		}
		if got["type"] != "im" {
			t.Errorf("type = %v, want im", got["type"])
		}
		if got["body"] != "can you take the chat in the queue?" {
			t.Errorf("body = %v", got["body"])
		}
		if got["fromName"] != "admin" {
			t.Errorf("fromName = %v, want admin", got["fromName"])
		}
	default:
		t.Fatal("nothing reached the recipient's inbox — this is the bug")
	}
}

func TestAgentIMReportsUndelivered(t *testing.T) {
	srv, db := newServer(t)
	admin := loginAdmin(t, srv)
	inviteAgent(t, srv, admin, "gaby")
	gabyID, _ := strconv.ParseInt(userIDByName(t, db, "gaby"), 10, 64)

	status, out := postJSONObj(t, admin, wr(srv, "/api/im/send"), map[string]any{
		"toUserId": gabyID, "body": "anyone there?",
	})
	if status != 200 {
		t.Fatalf("send: got %d, want 200", status)
	}
	if out["delivered"] != false {
		t.Errorf("delivered = %v, want false — no console was subscribed", out["delivered"])
	}
}

func TestAgentIMRejectsUnknownRecipient(t *testing.T) {
	srv, _ := newServer(t)
	admin := loginAdmin(t, srv)

	if status, _ := postJSONObj(t, admin, wr(srv, "/api/im/send"), map[string]any{
		"toUserId": 999999, "body": "hello?",
	}); status != 404 {
		t.Errorf("unknown recipient: got %d, want 404", status)
	}
	if status, _ := postJSONObj(t, admin, wr(srv, "/api/im/send"), map[string]any{
		"toUserId": 0, "body": "hello?",
	}); status != 400 {
		t.Errorf("missing recipient: got %d, want 400", status)
	}
}

func TestAgentIMRequiresAuth(t *testing.T) {
	srv, _ := newServer(t)
	if status, _ := postJSONObj(t, newClient(t), wr(srv, "/api/im/send"), map[string]any{
		"toUserId": 1, "body": "hello?",
	}); status != 401 {
		t.Errorf("anonymous send: got %d, want 401", status)
	}
}

func TestLegacyInboxChannelIsRefused(t *testing.T) {
	agent := &Conn{agentRef: testRef, agentUserID: 7, subs: map[string]bool{}, grants: map[string]bool{}}

	for _, action := range []string{"subscribe", "broadcast"} {
		if agent.canUseChannel(action, "inbox:some-session-id") {
			t.Errorf("%s on the legacy inbox channel was allowed; it must be refused", action)
		}
	}
	if !agent.canUseChannel("subscribe", "inbox:user:"+testRef+":7") {
		t.Error("an agent cannot subscribe to their own user-keyed inbox")
	}
	if agent.canUseChannel("subscribe", "inbox:user:"+testRef+":8") {
		t.Error("an agent can subscribe to a colleague's inbox")
	}
}

func TestConversationStartNamesTheRoutedAgent(t *testing.T) {
	srv, db := newServer(t)
	admin := loginAdmin(t, srv)
	inviteAgent(t, srv, admin, "gaby")
	gabyID, _ := strconv.ParseInt(userIDByName(t, db, "gaby"), 10, 64)

	if _, err := db.Exec(
		`INSERT INTO agent_availability
		   (user_id, ref, available, session_id, display_name, has_camera, picture,
		    online_since, updated_at, chat_ok, audio_ok, video_ok)
		 VALUES (?, ?, 1, 's1', 'Gaby', 0, '', '2026-08-01T00:00:00Z', '2026-08-01T00:00:00Z', 1, 0, 0)`,
		gabyID, testRef); err != nil {
		t.Fatalf("availability: %v", err)
	}

	status, out := postJSONObj(t, newClient(t), srv.URL+"/api/conversation/start", map[string]any{
		"ref": testRef, "callType": callTypeChat,
		"guestSession": "guest-session-1", "guestName": "Visitor",
	})
	if status != 200 {
		t.Fatalf("start: got %d, want 200 (%v)", status, out)
	}
	if out["agentName"] != "Gaby" {
		t.Errorf("agentName = %v, want Gaby — the visitor must be shown the agent who was actually routed", out["agentName"])
	}
	if int64(out["agentId"].(float64)) != gabyID {
		t.Errorf("agentId = %v, want %d", out["agentId"], gabyID)
	}

	_, resumed := postJSONObj(t, newClient(t), srv.URL+"/api/conversation/start", map[string]any{
		"ref": testRef, "callType": callTypeChat,
		"guestSession": "guest-session-1", "guestName": "Visitor",
	})
	if resumed["resumed"] != true {
		t.Fatalf("second start did not resume: %v", resumed)
	}
	if resumed["agentName"] != "Gaby" {
		t.Errorf("resumed agentName = %v, want Gaby", resumed["agentName"])
	}
}
