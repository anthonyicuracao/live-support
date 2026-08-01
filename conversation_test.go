package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
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
			"ref": testRef, "callType": callType,
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
		"ref": testRef, "callType": callTypeChat,
		"guestSession": "guest-sess", "guestName": "Guest",
	})
	if status == 200 {
		t.Error("started a conversation with a paused agent")
	}
}

// Capacity SORTS, it never gates. A visitor is never refused because of an
// agent-side accounting number — that produced a 429 which made an agent with
// three abandoned test chats permanently unreachable.
func TestCapacityRoutesNeverRefuses(t *testing.T) {
	t.Setenv("MAX_CONCURRENT_CHATS", "2")
	srv, db := newServer(t)
	c := newClient(t)

	uid := mustUser(t, db, "busy-agent")
	if err := upsertAvailability(db, testRef, uid, true, "sess", "Busy", false, "", "",
		modes{Chat: true, Audio: true}); err != nil {
		t.Fatalf("upsertAvailability: %v", err)
	}

	// A DISTINCT visitor per call. Reusing one session id would (correctly) now
	// resume that visitor's open conversation rather than create another —
	// which is the invariant, not a capacity question.
	n := 0
	start := func() (int, string) {
		n++
		return postJSON(t, c, srv.URL+"/api/conversation/start", map[string]any{
			"ref": testRef, "callType": callTypeChat,
			"guestSession": fmt.Sprintf("guest-%d", n), "guestName": "Guest",
		})
	}

	// Every chat is accepted, including past the cap.
	for i := 1; i <= 3; i++ {
		if st, _ := start(); st != 200 {
			t.Fatalf("chat %d = %d, want 200 — capacity must not refuse a visitor", i, st)
		}
	}
	// ...but the visitor is told honestly once the agent is over capacity, so
	// the UI can set expectations instead of pretending the agent is idle.
	_, body := start()
	if !strings.Contains(body, `"waiting":true`) {
		t.Errorf("4th chat past capacity should report waiting:true, got %s", body)
	}

	// Audio is a different modality and must NOT be affected by the chat load:
	// falling off one modality must never look like going offline.
	stAudio, bodyAudio := postJSON(t, c, srv.URL+"/api/conversation/start", map[string]any{
		"ref": testRef, "callType": callTypeAudio,
		"guestSession": "guest-audio", "guestName": "Guest",
	})
	if stAudio != 200 {
		t.Errorf("audio start = %d while chat-loaded, want 200", stAudio)
	}
	if strings.Contains(bodyAudio, `"waiting":true`) {
		t.Errorf("audio should not be waiting because of chat load: %s", bodyAudio)
	}
}

// A call is exclusive — one voice, one pair of ears — so an agent already on a
// call has no spare capacity for another, whatever the chat setting says.
func TestCallCapacityIsExclusive(t *testing.T) {
	_, db := newServer(t)
	uid := mustUser(t, db, "call-agent")
	if err := upsertAvailability(db, testRef, uid, true, "sess", "On Call", true, "", "",
		modes{Chat: true, Audio: true, Video: true}); err != nil {
		t.Fatalf("upsertAvailability: %v", err)
	}
	if got := capacityFor(callTypeAudio); got != 1 {
		t.Errorf("audio capacity = %d, want 1", got)
	}
	if got := capacityFor(callTypeVideo); got != 1 {
		t.Errorf("video capacity = %d, want 1", got)
	}
	if capacityFor(callTypeChat) < 2 {
		t.Error("chat capacity should allow more than one — it is multiplexable")
	}

	// First call: spare.
	if _, spare := routeAgent(db, testRef, callTypeAudio); !spare {
		t.Fatal("a free agent should have spare call capacity")
	}
	cid, _ := newConvID()
	if err := createConversation(db, conversation{
		CID: cid, Ref: testRef, GuestSession: "g1", AgentUserID: uid,
		CallType: callTypeAudio, CreatedAt: time.Now().Unix(),
	}); err != nil {
		t.Fatalf("createConversation: %v", err)
	}
	// Second: still ROUTED to them (never refused), but reported as waiting.
	id, spare := routeAgent(db, testRef, callTypeAudio)
	if id != uid {
		t.Errorf("routed to %d, want %d — the only agent offering audio", id, uid)
	}
	if spare {
		t.Error("an agent already on a call must not report spare call capacity")
	}
	// Chat is unaffected: chat alongside a call is normal work.
	if _, chatSpare := routeAgent(db, testRef, callTypeChat); !chatSpare {
		t.Error("being on a call must not consume chat capacity")
	}
}

// An abandoned chat must stop consuming capacity, or one test session takes an
// agent out of rotation forever. This is the bug the 429 was a symptom of.
func TestInactiveChatsReleaseCapacity(t *testing.T) {
	t.Setenv("MAX_CONCURRENT_CHATS", "1")
	_, db := newServer(t)
	uid := mustUser(t, db, "loaded-agent")
	if err := upsertAvailability(db, testRef, uid, true, "sess", "Loaded", false, "", "",
		modes{Chat: true}); err != nil {
		t.Fatalf("upsertAvailability: %v", err)
	}
	cid, _ := newConvID()
	if err := createConversation(db, conversation{
		CID: cid, Ref: testRef, GuestSession: "g1", AgentUserID: uid,
		CallType: callTypeChat, CreatedAt: time.Now().Unix(),
	}); err != nil {
		t.Fatalf("createConversation: %v", err)
	}
	if _, spare := routeAgent(db, testRef, callTypeChat); spare {
		t.Fatal("precondition: one active chat should exhaust a cap of 1")
	}

	// Age it past the inactivity window. It is NOT ended — the visitor could
	// still come back — it simply stops occupying the agent.
	stale := time.Now().Add(-2 * chatInactiveAfter()).Unix()
	if _, err := db.Exec(`UPDATE conversations SET last_activity_at = ? WHERE cid = ?`, stale, cid); err != nil {
		t.Fatalf("age conversation: %v", err)
	}
	if _, spare := routeAgent(db, testRef, callTypeChat); !spare {
		t.Error("an inactive chat must not keep consuming capacity")
	}
	var ended sql.NullInt64
	if err := db.QueryRow(`SELECT ended_at FROM conversations WHERE cid = ?`, cid).Scan(&ended); err != nil {
		t.Fatalf("read ended_at: %v", err)
	}
	if ended.Valid {
		t.Error("going inactive must not CLOSE the conversation — the visitor may return")
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

// The lifecycle invariant that was missing entirely, and whose absence produced
// four separate UI symptoms: a visitor has AT MOST ONE open conversation per
// modality. Both creation paths must join it rather than mint a rival.
func TestOneOpenConversationPerVisitorPerModality(t *testing.T) {
	srv, db := newServer(t)
	agent := loginAdmin(t, srv)

	uid := mustUser(t, db, "chat-agent")
	if err := upsertAvailability(db, testRef, uid, true, "sess", "Agent", false, "", "",
		modes{Chat: true, Audio: true}); err != nil {
		t.Fatalf("upsertAvailability: %v", err)
	}

	guest := newClient(t)
	startChat := func() map[string]any {
		_, body := postJSON(t, guest, srv.URL+"/api/conversation/start", map[string]any{
			"ref": testRef, "callType": callTypeChat,
			"guestSession": "visitor-1", "guestName": "Visitor",
		})
		var out map[string]any
		if err := json.Unmarshal([]byte(body), &out); err != nil {
			t.Fatalf("decode: %v (%s)", err, body)
		}
		return out
	}

	first := startChat()
	cid, _ := first["cid"].(string)
	if cid == "" {
		t.Fatalf("no cid in %v", first)
	}

	t.Run("the visitor clicking Chat again resumes", func(t *testing.T) {
		again := startChat()
		if again["cid"] != cid {
			t.Errorf("second start made a new conversation %v, want %s", again["cid"], cid)
		}
		if again["resumed"] != true {
			t.Errorf("second start should report resumed:true, got %v", again["resumed"])
		}
	})

	t.Run("the agent replying joins the SAME conversation", func(t *testing.T) {
		// This is the one that broke it: the console created its own, so the
		// two of them held half the exchange each.
		_, page := getBody(t, agent, wr(srv, "/users"))
		_ = page
		_, body := postJSON(t, agent, wr(srv, "/api/conversation/invite"), map[string]any{
			"guestSession": "visitor-1", "guestName": "Visitor",
			"callType": callTypeChat, "callerName": "Agent",
		})
		var out map[string]any
		if err := json.Unmarshal([]byte(body), &out); err != nil {
			t.Fatalf("decode: %v (%s)", err, body)
		}
		if out["cid"] != cid {
			t.Errorf("agent invite made conversation %v, want the visitor's %s", out["cid"], cid)
		}
		if out["resumed"] != true {
			t.Errorf("agent invite into an open conversation should report resumed:true, got %v", out["resumed"])
		}
	})

	t.Run("exactly one open chat row exists", func(t *testing.T) {
		var n int
		if err := db.QueryRow(
			`SELECT COUNT(*) FROM conversations
			  WHERE ref = ? AND guest_session = ? AND call_type = ? AND ended_at IS NULL`,
			testRef, "visitor-1", callTypeChat).Scan(&n); err != nil {
			t.Fatalf("count: %v", err)
		}
		if n != 1 {
			t.Errorf("%d open chat conversations for one visitor, want 1", n)
		}
	})

	t.Run("a call is a separate conversation from a chat", func(t *testing.T) {
		// Modality-scoped, not visitor-scoped: a call may run alongside a chat.
		_, body := postJSON(t, guest, srv.URL+"/api/conversation/start", map[string]any{
			"ref": testRef, "callType": callTypeAudio,
			"guestSession": "visitor-1", "guestName": "Visitor",
		})
		var out map[string]any
		_ = json.Unmarshal([]byte(body), &out)
		if out["cid"] == cid {
			t.Error("an audio call reused the chat conversation")
		}
		if out["cid"] == nil || out["cid"] == "" {
			t.Errorf("no conversation created for the call: %s", body)
		}
	})
}

// A tenant whose data predates the one-open-conversation invariant must still
// OPEN. The constraint first lived in the always-run schema block, so an
// existing tenant with duplicates failed CREATE UNIQUE INDEX, failed openDB,
// and became entirely unreachable — every sign-in rejected, cause invisible.
func TestTenantWithPreExistingDuplicatesStillOpens(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "legacy.db")

	// A tenant as it was BEFORE the invariant: two open chats for one visitor.
	seed, err := openDB(path)
	if err != nil {
		t.Fatalf("initial open: %v", err)
	}
	// Drop the index to reproduce a database created BEFORE the invariant
	// existed. Without this the first open already constrains the table and the
	// duplicates cannot be seeded at all.
	if _, err := seed.Exec(`DROP INDEX IF EXISTS idx_conversations_open_unique`); err != nil {
		t.Fatalf("drop index: %v", err)
	}
	now := time.Now().Unix()
	for i, cid := range []string{"dupe-a", "dupe-b", "dupe-c"} {
		if _, err := seed.Exec(
			`INSERT INTO conversations (cid, ref, guest_session, guest_name, agent_user_id, call_type, created_at, last_activity_at)
			 VALUES (?, 'legacy.test', 'visitor-1', 'V', 1, 'chat', ?, ?)`,
			cid, now+int64(i), now+int64(i)); err != nil {
			t.Fatalf("seed %s: %v", cid, err)
		}
	}
	seed.Close()

	// Reopening must succeed, not fail on the constraint.
	db, err := openDB(path)
	if err != nil {
		t.Fatalf("a tenant with pre-existing duplicates failed to open: %v", err)
	}
	defer db.Close()

	// And the duplicates are reconciled rather than left to break it again:
	// the most recent survives, the rest are closed (not deleted — they hold
	// real transcript).
	var open int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM conversations WHERE ended_at IS NULL AND guest_session = 'visitor-1'`).
		Scan(&open); err != nil {
		t.Fatalf("count: %v", err)
	}
	if open != 1 {
		t.Errorf("%d open conversations after reconciliation, want 1", open)
	}
	var survivor string
	_ = db.QueryRow(`SELECT cid FROM conversations WHERE ended_at IS NULL AND guest_session = 'visitor-1'`).Scan(&survivor)
	if survivor != "dupe-c" {
		t.Errorf("survivor is %q, want the most recently active (dupe-c)", survivor)
	}
	var total int
	_ = db.QueryRow(`SELECT COUNT(*) FROM conversations`).Scan(&total)
	if total != 3 {
		t.Errorf("%d conversations remain, want 3 — reconciliation must CLOSE, never delete", total)
	}
}

// Read receipts are a per-tenant privacy setting. Default ON (Ron: live support
// is the primary use case and the visitor is the one waiting), and when a tenant
// turns them off it must hold on BOTH paths — refusing to record new ones is not
// enough if the transcript still hands back every timestamp taken while it was on.
func TestReadReceiptsAreAPerTenantSetting(t *testing.T) {
	srv, db := newServer(t)
	c := newClient(t)

	uid := mustUser(t, db, "receipt-agent")
	if err := upsertAvailability(db, testRef, uid, true, "sess", "Agent", false, "", "",
		modes{Chat: true}); err != nil {
		t.Fatalf("upsertAvailability: %v", err)
	}

	if !readReceiptsEnabled(db) {
		t.Fatal("read receipts must default to ON — transparency is the documented default")
	}

	// A conversation with one visitor message for the agent to acknowledge.
	_, startBody := postJSON(t, c, srv.URL+"/api/conversation/start", map[string]any{
		"ref": testRef, "callType": callTypeChat,
		"guestSession": "visitor-r", "guestName": "V",
	})
	var started map[string]any
	if err := json.Unmarshal([]byte(startBody), &started); err != nil {
		t.Fatalf("decode start: %v (%s)", err, startBody)
	}
	cid, _ := started["cid"].(string)
	guestTok, _ := started["token"].(string)
	agentTok, err := mintConvToken(convTestSecret, cid, convRoleAgent)
	if err != nil {
		t.Fatalf("mint agent token: %v", err)
	}

	_, msgBody := postJSON(t, c, srv.URL+"/api/conversation/message", map[string]any{
		"ref": testRef, "cid": cid, "token": guestTok, "body": "hello",
	})
	var sent struct {
		Message struct{ ID int64 } `json:"message"`
	}
	if err := json.Unmarshal([]byte(msgBody), &sent); err != nil || sent.Message.ID == 0 {
		t.Fatalf("decode message: %v (%s)", err, msgBody)
	}

	ack := func(kind string) {
		postJSON(t, c, srv.URL+"/api/conversation/receipt", map[string]any{
			"ref": testRef, "cid": cid, "token": agentTok,
			"upToId": sent.Message.ID, "kind": kind,
		})
	}
	transcriptReadAt := func(tok string) float64 {
		qs := "ref=" + testRef + "&cid=" + cid + "&token=" + url.QueryEscape(tok)
		_, body := getBody(t, c, srv.URL+"/api/conversation/messages?"+qs)
		var out struct {
			Messages []map[string]any `json:"messages"`
		}
		_ = json.Unmarshal([]byte(body), &out)
		if len(out.Messages) == 0 {
			t.Fatalf("no messages in transcript: %s", body)
		}
		v, _ := out.Messages[0]["read_at"].(float64)
		return v
	}

	t.Run("on by default: a read is recorded and visible", func(t *testing.T) {
		ack("read")
		if transcriptReadAt(guestTok) == 0 {
			t.Error("read receipt was not recorded while the setting is on")
		}
	})

	t.Run("turning it off hides receipts already taken", func(t *testing.T) {
		if err := setTenantSetting(db, settingReadReceipts, "off"); err != nil {
			t.Fatalf("set: %v", err)
		}
		if transcriptReadAt(guestTok) != 0 {
			t.Error("a read time recorded earlier is still exposed after opting out")
		}
	})

	t.Run("and stops new ones being recorded", func(t *testing.T) {
		// Wipe the stored value so a fresh ack would have to write it again.
		if _, err := db.Exec(`UPDATE chat_messages SET read_at = NULL WHERE cid = ?`, cid); err != nil {
			t.Fatalf("clear: %v", err)
		}
		ack("read")
		var stored sql.NullInt64
		if err := db.QueryRow(`SELECT read_at FROM chat_messages WHERE cid = ?`, cid).Scan(&stored); err != nil {
			t.Fatalf("read back: %v", err)
		}
		if stored.Valid {
			t.Error("a read receipt was recorded while the tenant has them off")
		}
	})

	t.Run("delivery is unaffected — it is not the same claim", func(t *testing.T) {
		ack("delivered")
		var delivered sql.NullInt64
		if err := db.QueryRow(`SELECT delivered_at FROM chat_messages WHERE cid = ?`, cid).Scan(&delivered); err != nil {
			t.Fatalf("read back: %v", err)
		}
		if !delivered.Valid {
			t.Error("delivery receipts must keep working — they say it reached a device, not that a person read it")
		}
	})

	t.Run("an unexpected value falls back to ON", func(t *testing.T) {
		// Positive check for the disabling value: a corrupted setting must not
		// silently switch a feature off for a tenant who never asked.
		_ = setTenantSetting(db, settingReadReceipts, "banana")
		if !readReceiptsEnabled(db) {
			t.Error("an unrecognised setting disabled read receipts")
		}
	})
}
