package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// Security tests for private conversations.
//
// The guarantee being pinned (Ron, 2026-08-01): no third party may join or
// listen to an existing chat or call. Caller and callee have privacy across
// every channel.
//
// Each test here corresponds to a way the previous design failed: channel names
// were the capability, and /api/agents/available published them.

const convTestSecret = "test-connect-secret" // matches newServer's CONNECT_SECRET

// A token is only good for the conversation it was minted for. Holding one
// conversation's token must not open another's.
func TestConvTokenIsScopedToOneConversation(t *testing.T) {
	tokA, err := mintConvToken(convTestSecret, "conv-a", convRoleGuest)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	parsed, err := parseConvToken(convTestSecret, tokA)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if parsed.CID != "conv-a" {
		t.Fatalf("cid = %q, want conv-a", parsed.CID)
	}
	// The channel a connection unlocks comes from the TOKEN, so a token for A
	// can only ever grant A.
	if convChannel(parsed.CID) == convChannel("conv-b") {
		t.Fatal("token for conv-a resolved to conv-b's channel")
	}
}

// Forged, corrupted and foreign-key tokens are all refused.
func TestConvTokenRejectsForgeries(t *testing.T) {
	good, _ := mintConvToken(convTestSecret, "conv-x", convRoleGuest)

	// Mutate a character in the MIDDLE of the token, not the last one.
	//
	// base64's final character carries padding bits that the decoder ignores
	// when the payload length is not a multiple of 3, so flipping it can decode
	// to byte-identical output and the token stays valid. That is a property of
	// the encoding, not a weakness in the AEAD — but it made this test flake
	// about one run in three. A middle character always changes a real
	// ciphertext byte, so the GCM tag check always rejects it.
	mid := len(good) / 2
	repl := byte('A')
	if good[mid] == 'A' {
		repl = 'B'
	}
	flipped := good[:mid] + string(repl) + good[mid+1:]

	cases := []struct {
		name  string
		token string
	}{
		{"empty", ""},
		{"garbage", "not-a-token"},
		{"truncated", good[:len(good)/2]},
		{"flipped last char", flipped},
	}
	for _, c := range cases {
		if _, err := parseConvToken(convTestSecret, c.token); err == nil {
			t.Errorf("%s: accepted an invalid token", c.name)
		}
	}

	// Signed with a different key — i.e. another tenant's or an attacker's.
	foreign, _ := mintConvToken("a-totally-different-secret", "conv-x", convRoleGuest)
	if _, err := parseConvToken(convTestSecret, foreign); err == nil {
		t.Error("accepted a token signed with a foreign key")
	}

	// No secret configured must fail closed, never open.
	if _, err := parseConvToken("", good); err == nil {
		t.Error("accepted a token with no server secret configured")
	}
}

func TestConvTokenExpires(t *testing.T) {
	b, _ := json.Marshal(convToken{CID: "conv-old", Role: convRoleGuest, Exp: time.Now().Add(-time.Minute).Unix()})
	expired, err := signRaw(convTestSecret, string(b))
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if _, err := parseConvToken(convTestSecret, expired); err == nil {
		t.Error("accepted an expired token")
	}
}

// canUseChannel is the single authorization decision for the socket. This is
// the test that says a stranger cannot listen.
func TestCanUseChannelAuthorization(t *testing.T) {
	guest := &Conn{grants: map[string]bool{}}
	agent := &Conn{grants: map[string]bool{}, agentRef: "acme.com"}

	t.Run("guest cannot read a conversation it has no token for", func(t *testing.T) {
		if guest.canUseChannel("subscribe", convChannel("someone-elses-conversation")) {
			t.Error("un-granted conversation channel was usable")
		}
	})

	t.Run("guest can use exactly the conversation it was granted", func(t *testing.T) {
		guest.grant(convChannel("mine"))
		if !guest.canUseChannel("subscribe", convChannel("mine")) {
			t.Error("granted channel was refused")
		}
		if guest.canUseChannel("subscribe", convChannel("not-mine")) {
			t.Error("a grant for one conversation leaked into another")
		}
	})

	t.Run("guest cannot READ the roster but may announce itself", func(t *testing.T) {
		// presence is asymmetric on purpose: a directory everyone writes and
		// only agents read. Reading it was the leak; writing is how a visitor
		// appears in the console's waiting list at all.
		if guest.canUseChannel("subscribe", "presence:acme.com") {
			t.Error("guest could subscribe to presence")
		}
		if !guest.canUseChannel("track", "presence:acme.com") {
			t.Error("guest could not announce itself — the console would show no visitors")
		}
		if guest.canUseChannel("subscribe", "dashboard:acme.com") {
			t.Error("guest could subscribe to the dashboard channel")
		}
		if guest.canUseChannel("track", "dashboard:acme.com") {
			t.Error("guest could write to the dashboard channel")
		}
	})

	t.Run("guest inbox needs its own capability", func(t *testing.T) {
		// How an agent opens contact with a visitor. Server-minted id, so an
		// agent seeing it in presence still cannot subscribe to it.
		if guest.canUseChannel("subscribe", guestChannel("some-visitor")) {
			t.Error("un-granted guest inbox was usable")
		}
		guest.grant(guestChannel("me"))
		if !guest.canUseChannel("subscribe", guestChannel("me")) {
			t.Error("granted guest inbox was refused")
		}
		if agent.canUseChannel("subscribe", guestChannel("me")) {
			t.Error("an agent could subscribe to a visitor's inbox without a token")
		}
	})

	t.Run("agent gets presence for their own tenant only", func(t *testing.T) {
		if !agent.canUseChannel("subscribe", "presence:acme.com") {
			t.Error("agent refused presence for their own tenant")
		}
		if agent.canUseChannel("subscribe", "presence:other-tenant.com") {
			t.Error("agent could read another tenant's presence")
		}
		if agent.canUseChannel("subscribe", "dashboard:other-tenant.com") {
			t.Error("agent could read another tenant's dashboard")
		}
	})

	t.Run("unknown namespaces are refused", func(t *testing.T) {
		// A positive whitelist: a new namespace must be an explicit decision.
		for _, ch := range []string{"inbox:abc", "", "random", "conv", "presence"} {
			for _, act := range []string{"subscribe", "broadcast", "track"} {
				if agent.canUseChannel(act, ch) {
					t.Errorf("unknown channel %q was allowed for %s", ch, act)
				}
			}
		}
	})

	t.Run("a token names its own channel", func(t *testing.T) {
		// The client never supplies a channel name, so naming one it holds no
		// token for cannot talk it in.
		if got := channelForToken(convToken{CID: "x", Role: convRoleGuest}); got != "conv:x" {
			t.Errorf("guest conversation token → %q", got)
		}
		if got := channelForToken(convToken{CID: "x", Role: roleGuestSession}); got != "guest:x" {
			t.Errorf("guest-session token → %q", got)
		}
	})
}

// An agent must not be able to obtain a capability for a colleague's
// conversation, and a probe must not reveal which conversation ids exist.
func TestConversationTokenHandlerOwnershipn(t *testing.T) {
	srv, db := newServer(t)
	c := loginAdmin(t, srv)

	other := mustUser(t, db, "other-agent")
	cid, _ := newConvID()
	if err := createConversation(db, conversation{
		CID: cid, Ref: testRef, GuestSession: "guest-1", AgentUserID: other,
		CallType: callTypeChat, CreatedAt: time.Now().Unix(),
	}); err != nil {
		t.Fatalf("createConversation: %v", err)
	}

	status, _ := getBody(t, c, wr(srv, "/api/conversation/token?cid="+cid))
	if status != http.StatusNotFound {
		t.Errorf("status = %d, want 404 for another agent's conversation", status)
	}

	// A conversation that does not exist answers identically, so the endpoint
	// cannot be used to enumerate ids.
	statusMissing, _ := getBody(t, c, wr(srv, "/api/conversation/token?cid=does-not-exist"))
	if statusMissing != status {
		t.Errorf("missing cid = %d but foreign cid = %d — the difference is an oracle",
			statusMissing, status)
	}
}

func postJSON(t *testing.T, c *http.Client, target string, body any) (int, string) {
	t.Helper()
	b, _ := json.Marshal(body)
	resp, err := c.Post(target, "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatalf("POST %s: %v", target, err)
	}
	defer resp.Body.Close()
	buf := new(bytes.Buffer)
	_, _ = buf.ReadFrom(resp.Body)
	return resp.StatusCode, buf.String()
}

// A guest cannot start a conversation an agent is not offering. This is the
// check a client could otherwise skip, so it has to live on the server.
func TestConversationStartRespectsModality(t *testing.T) {
	srv, db := newServer(t)
	c := newClient(t)

	uid := mustUser(t, db, "chat-only-agent")
	// Available, but chat only — no audio, no video.
	if err := upsertAvailability(db, testRef, uid, true, "sess", "Chat Only", false, "", "",
		modes{Chat: true}); err != nil {
		t.Fatalf("upsertAvailability: %v", err)
	}

	start := func(callType string) int {
		status, _ := postJSON(t, c, srv.URL+"/api/conversation/start", map[string]any{
			"ref": testRef, "agentUserId": uid, "callType": callType,
			"guestSession": "guest-sess", "guestName": "Guest",
		})
		return status
	}

	if got := start(callTypeChat); got != 200 {
		t.Errorf("chat start = %d, want 200", got)
	}
	if got := start(callTypeAudio); got == 200 {
		t.Error("audio conversation started against a chat-only agent")
	}
	if got := start(callTypeVideo); got == 200 {
		t.Error("video conversation started against a chat-only agent")
	}
	if got := start("telepathy"); got == 200 {
		t.Error("an unknown call type was accepted")
	}
}

// A paused or deactivated agent cannot be reached at all.
func TestConversationStartRequiresAvailability(t *testing.T) {
	srv, db := newServer(t)
	c := newClient(t)

	uid := mustUser(t, db, "paused-agent")
	if err := upsertAvailability(db, testRef, uid, false, "sess", "Paused", false, "", "",
		modes{Chat: true, Audio: true}); err != nil {
		t.Fatalf("upsertAvailability: %v", err)
	}
	status, _ := postJSON(t, c, srv.URL+"/api/conversation/start", map[string]any{
		"ref": testRef, "agentUserId": uid, "callType": callTypeChat,
		"guestSession": "guest-sess", "guestName": "Guest",
	})
	if status == 200 {
		t.Error("started a conversation with a paused agent")
	}
}

// The concurrency governor: chat may run alongside a call, but not without
// bound, and the bound is enforced server-side.
func TestChatConcurrencyGovernor(t *testing.T) {
	t.Setenv("MAX_CONCURRENT_CHATS", "2")
	srv, db := newServer(t)
	c := newClient(t)

	uid := mustUser(t, db, "busy-agent")
	if err := upsertAvailability(db, testRef, uid, true, "sess", "Busy", false, "", "",
		modes{Chat: true, Audio: true}); err != nil {
		t.Fatalf("upsertAvailability: %v", err)
	}

	start := func() int {
		status, _ := postJSON(t, c, srv.URL+"/api/conversation/start", map[string]any{
			"ref": testRef, "agentUserId": uid, "callType": callTypeChat,
			"guestSession": "guest-sess", "guestName": "Guest",
		})
		return status
	}

	if got := start(); got != 200 {
		t.Fatalf("first chat = %d, want 200", got)
	}
	if got := start(); got != 200 {
		t.Fatalf("second chat = %d, want 200", got)
	}
	if got := start(); got != http.StatusTooManyRequests {
		t.Errorf("third chat = %d, want 429 — the cap was not enforced", got)
	}

	// Audio is a different modality and must NOT be blocked by the chat cap:
	// falling off one modality must never look like going offline.
	statusAudio, _ := postJSON(t, c, srv.URL+"/api/conversation/start", map[string]any{
		"ref": testRef, "agentUserId": uid, "callType": callTypeAudio,
		"guestSession": "guest-sess", "guestName": "Guest",
	})
	if statusAudio != 200 {
		t.Errorf("audio start = %d while at chat capacity, want 200", statusAudio)
	}
}

// The public roster must no longer hand out anything that can be used to
// address an agent directly. This is the leak that broke the original model.
func TestPublicRosterDoesNotExposeSessionID(t *testing.T) {
	srv, db := newServer(t)
	c := newClient(t)

	uid := mustUser(t, db, "listed-agent")
	if err := upsertAvailability(db, testRef, uid, true, "secret-session-id", "Listed", true, "", "",
		modes{Chat: true, Audio: true, Video: true}); err != nil {
		t.Fatalf("upsertAvailability: %v", err)
	}
	// A push subscription is required for an agent to appear in discovery.
	if err := upsertSubscription(db, testRef, uid, "secret-session-id", pushSub{
		endpoint: "https://push.example/endpoint", p256dh: "k", auth: "a",
	}); err != nil {
		t.Fatalf("upsertSubscription: %v", err)
	}

	// agentsAvailableHandler is mounted by mountAvailability, which the shared
	// harness does not call — drive it directly rather than reshaping the
	// harness for one test.
	_ = srv
	_ = c
	rec := httptest.NewRecorder()
	agentsAvailableHandler(rec, httptest.NewRequest("GET", "/api/agents/available?ref="+testRef, nil))
	body := rec.Body.String()

	if bytes.Contains([]byte(body), []byte("secret-session-id")) {
		t.Error("public roster still exposes session_id — it can be used as a channel key")
	}
	if !bytes.Contains([]byte(body), []byte("Listed")) {
		t.Fatalf("precondition: the agent should appear in the roster at all; got %s", body)
	}
}
