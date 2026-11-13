package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

// Tests for tenant domain aliases (alias.go).
//
// The guarantee being pinned (Ron, 2026-11-12): an alias is looked up FIRST.
// Found → take the domain it points at → proceed with that, and with nothing
// else. So every test below enters through an alias and then checks that what
// was stored, signed in to, or announced belongs to the real tenant — and that
// the alias never became a tenant of its own along the way.

// testAlias points at testRef in every test that calls setAliases.
const testAlias = "my.demo"

// setAliases installs an alias table for one test and restores the previous one
// afterwards, so an alias can never leak into the rest of the suite.
func setAliases(t *testing.T, spec string) {
	t.Helper()
	prev := refAliases
	refAliases = parseRefAliases(spec)
	t.Cleanup(func() { refAliases = prev })
}

// aliasDBExists reports whether a database file was created under the alias's
// own name — the failure every one of these tests is ultimately guarding.
func aliasDBExists() bool {
	_, err := os.Stat(filepath.Join(os.Getenv("DATA_DIR"), testAlias+".db"))
	return err == nil
}

func TestParseRefAliases(t *testing.T) {
	t.Run("pairs are normalised and bad entries skipped", func(t *testing.T) {
		got := parseRefAliases(" MyAccount.Example.com = example.com , shop.acme.com=acme.com,, " +
			"no-equals, self.com=self.com, =missing-alias.com, missing-target.com= ")
		want := map[string]string{
			"myaccount.example.com": "example.com",
			"shop.acme.com":         "acme.com",
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("parsed %v, want %v", got, want)
		}
	})

	t.Run("empty spec means no aliases", func(t *testing.T) {
		if got := parseRefAliases(""); len(got) != 0 {
			t.Errorf("parsed %v from an empty spec", got)
		}
	})

	t.Run("an alias of an alias is dropped, its target kept", func(t *testing.T) {
		got := parseRefAliases("a.com=b.com,b.com=c.com")
		want := map[string]string{"b.com": "c.com"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("parsed %v, want %v", got, want)
		}
	})

	t.Run("a loop resolves to nothing rather than to either side", func(t *testing.T) {
		if got := parseRefAliases("a.com=b.com,b.com=a.com"); len(got) != 0 {
			t.Errorf("a loop survived parsing: %v", got)
		}
	})
}

func TestResolveRef(t *testing.T) {
	setAliases(t, testAlias+"="+testRef)

	for in, want := range map[string]string{
		testAlias:       testRef,
		"My.Demo":       testRef, // domains are case-insensitive
		" my.demo ":     testRef,
		"my.demo.":      testRef, // every spelling that named the alias's database
		testRef:         testRef,
		"Other.Example": "Other.Example", // not an alias: returned untouched, case and all
		"":              "",
	} {
		if got := resolveRef(in); got != want {
			t.Errorf("resolveRef(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestResolveRefWithoutAliasesIsIdentity(t *testing.T) {
	setAliases(t, "")
	if got := resolveRef(testAlias); got != testAlias {
		t.Errorf("resolveRef(%q) = %q with no aliases configured", testAlias, got)
	}
}

func TestResolveChannel(t *testing.T) {
	setAliases(t, testAlias+"="+testRef)

	for in, want := range map[string]string{
		"presence:" + testAlias:           "presence:" + testRef,
		"dashboard:" + testAlias:          "dashboard:" + testRef,
		"inbox:user:" + testAlias + ":42": "inbox:user:" + testRef + ":42",
		"presence:other.example":          "presence:other.example",
		// Keyed by random ids, not refs. An id that happens to spell an alias
		// must not be rewritten into someone else's channel.
		"conv:" + testAlias:  "conv:" + testAlias,
		"guest:" + testAlias: "guest:" + testAlias,
		"":                   "",
	} {
		if got := resolveChannel(in); got != want {
			t.Errorf("resolveChannel(%q) = %q, want %q", in, got, want)
		}
	}
}

// The query string is resolved once, in front of every handler, so a handler
// reading ?ref= never sees an alias and never has to know to look.
func TestResolveAliasesRewritesQueryRef(t *testing.T) {
	setAliases(t, testAlias+"="+testRef)

	var got url.Values
	h := resolveAliases(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Query()
	}))

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/api/calls?page=2&ref="+testAlias, nil))
	if got.Get("ref") != testRef {
		t.Errorf("handler saw ref %q, want %q", got.Get("ref"), testRef)
	}
	if got.Get("page") != "2" {
		t.Errorf("rewriting ref disturbed another parameter: page = %q", got.Get("page"))
	}

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/api/calls?ref=Other.Example", nil))
	if got.Get("ref") != "Other.Example" {
		t.Errorf("a ref that is not an alias was changed to %q", got.Get("ref"))
	}
}

// The visitor's whole path, entered through the alias domain: session, routing
// to an agent of the real tenant, a message, and reading it back.
func TestAliasGuestReachesRealTenant(t *testing.T) {
	srv, db := newServer(t)
	setAliases(t, testAlias+"="+testRef)
	c := newClient(t)

	uid := mustUser(t, db, "alias-agent")
	if err := upsertAvailability(db, testRef, uid, true, "sess", "Agent", false, "", "",
		modes{Chat: true}); err != nil {
		t.Fatalf("upsertAvailability: %v", err)
	}

	if status, body := postJSON(t, c, srv.URL+"/api/guest/session",
		map[string]any{"ref": testAlias}); status != 200 {
		t.Fatalf("guest session via alias = %d: %s", status, first(body, 200))
	}

	status, body := postJSON(t, c, srv.URL+"/api/conversation/start", map[string]any{
		"ref": testAlias, "callType": callTypeChat,
		"guestSession": "guest-sess", "guestName": "Guest",
	})
	if status != 200 {
		t.Fatalf("conversation start via alias = %d: %s", status, first(body, 200))
	}
	var started struct {
		CID   string `json:"cid"`
		Token string `json:"token"`
	}
	if err := json.Unmarshal([]byte(body), &started); err != nil || started.CID == "" {
		t.Fatalf("bad start response: %s", first(body, 200))
	}

	// Stored under the REAL ref. This is the row an agent's console filters on;
	// stored under the alias it would sit in the right database and still be
	// invisible to every agent.
	var storedRef string
	if err := db.QueryRow(`SELECT ref FROM conversations WHERE cid = ?`, started.CID).Scan(&storedRef); err != nil {
		t.Fatalf("conversation not in the real tenant's database: %v", err)
	}
	if storedRef != testRef {
		t.Errorf("conversation stored with ref %q, want %q", storedRef, testRef)
	}

	if status, body := postJSON(t, c, srv.URL+"/api/conversation/message", map[string]any{
		"ref": testAlias, "cid": started.CID, "token": started.Token, "body": "hello from the alias",
	}); status != 200 {
		t.Fatalf("message via alias = %d: %s", status, first(body, 200))
	}

	q := url.Values{"ref": {testAlias}, "cid": {started.CID}, "token": {started.Token}}
	status, body = getBody(t, c, srv.URL+"/api/conversation/messages?"+q.Encode())
	if status != 200 || !strings.Contains(body, "hello from the alias") {
		t.Errorf("transcript via alias = %d: %s", status, first(body, 200))
	}

	if aliasDBExists() {
		t.Error("the alias got a database of its own")
	}
}

// An agent may arrive at the alias domain by link or by typing it. Either way
// they are signed in to the real tenant, and their session says so.
func TestAliasSignInLandsInRealTenant(t *testing.T) {
	srv, _ := newServer(t)
	loginAdmin(t, srv) // gives the real tenant's admin a known password
	setAliases(t, testAlias+"="+testRef)

	for name, loginURL := range map[string]string{
		"alias in the link":         srv.URL + "/login?ref=" + testAlias,
		"alias typed into the form": srv.URL + "/login",
	} {
		t.Run(name, func(t *testing.T) {
			c := newClient(t)
			_, page := getBody(t, c, loginURL)
			postForm(t, c, loginURL, url.Values{
				"csrf": {csrfFrom(t, page)}, "ref": {testAlias},
				"username": {"admin"}, "password": {adminPW},
			})
			status, body := getBody(t, c, srv.URL+"/api/me")
			if status != http.StatusOK {
				t.Fatalf("not signed in after logging in at the alias: /api/me = %d", status)
			}
			var me struct {
				Ref string `json:"ref"`
			}
			if err := json.Unmarshal([]byte(body), &me); err != nil {
				t.Fatalf("bad /api/me response: %s", first(body, 200))
			}
			if me.Ref != testRef {
				t.Errorf("signed in to %q, want the real tenant %q", me.Ref, testRef)
			}
		})
	}

	if aliasDBExists() {
		t.Error("the alias got a database of its own")
	}
}

// The platform may name the tenant by its alias in a signed token. /provision
// CREATES on demand, so unresolved it would mint the alias as a second tenant.
func TestAliasInPlatformTokenProvisionsRealTenant(t *testing.T) {
	srv, _ := newServer(t)
	setAliases(t, testAlias+"="+testRef)

	link := provisionInvite(t, srv, testAlias)
	u, err := url.Parse(link)
	if err != nil {
		t.Fatalf("parse invite link %q: %v", link, err)
	}
	if got := u.Query().Get("ref"); got != testRef {
		t.Errorf("invite issued for %q, want the real tenant %q", got, testRef)
	}
	if aliasDBExists() {
		t.Error("provisioning through an alias created a database for the alias")
	}
}

// Backstop for an entry point that forgets to resolve: the pool refuses to open
// an alias at all, and stops serving a database left over from before the name
// became one.
func TestAliasNeverHasItsOwnDatabase(t *testing.T) {
	newServer(t)

	// A tenant that existed under this name before it was made an alias.
	if _, err := dbs.get(testAlias); err != nil {
		t.Fatalf("creating the leftover database: %v", err)
	}
	setAliases(t, testAlias+"="+testRef)

	if dbs.exists(testAlias) {
		t.Error("exists() still reports the alias as a tenant")
	}
	if _, err := dbs.get(testAlias); err == nil {
		t.Error("get() opened a database for an alias")
	}
	if n := len(dbs.all()); n != 1 {
		t.Errorf("all() returned %d databases, want only the real tenant's", n)
	}
}

// newLegacyServer serves the routes main() registers directly, which the auth
// harness does not mount: the WebSocket and the session table.
func newLegacyServer(t *testing.T) *httptest.Server {
	t.Helper()
	newServer(t) // data dir, pool and hub
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", wsHandler)
	mux.HandleFunc("/api/sessions", sessionsHandler)
	srv := httptest.NewServer(resolveAliases(mux))
	t.Cleanup(srv.Close)
	return srv
}

// A visitor on the alias domain announces itself on presence:<alias>. Agents
// read presence:<real ref>, so that is where the announcement has to land or
// the console's waiting list stays empty.
func TestAliasPresenceLandsOnRealTenantChannel(t *testing.T) {
	srv := newLegacyServer(t)
	setAliases(t, testAlias+"="+testRef)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/ws", nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer ws.Close(websocket.StatusNormalClosure, "")

	if err := wsjson.Write(ctx, ws, ClientMsg{
		Action: "track", Channel: "presence:" + testAlias, Key: "guest-1",
		State: map[string]interface{}{"session_id": "guest-1", "role": "guest"},
	}); err != nil {
		t.Fatalf("track: %v", err)
	}

	// The server handles the frame on its own goroutine; wait for it.
	deadline := time.Now().Add(3 * time.Second)
	for len(hub.presenceUsers("presence:"+testRef)) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the visitor never appeared on the real tenant's presence channel")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if n := len(hub.presenceUsers("presence:" + testAlias)); n != 0 {
		t.Errorf("%d visitor(s) tracked on the alias's own channel", n)
	}
}

// The session table is written from a JSON body and read back by query string —
// two different entry points that must agree on the ref, or the row is written
// where the list never looks.
func TestAliasSessionRowsUseRealRef(t *testing.T) {
	srv := newLegacyServer(t)
	setAliases(t, testAlias+"="+testRef)
	c := newClient(t)

	if status, body := postJSON(t, c, srv.URL+"/api/sessions", map[string]any{
		"session_id": "guest-1", "ref": testAlias, "name": "Guest", "role": "guest",
	}); status != 200 {
		t.Fatalf("session upsert via alias = %d: %s", status, first(body, 200))
	}

	for _, ref := range []string{testRef, testAlias} {
		status, body := getBody(t, c, srv.URL+"/api/sessions?ref="+ref)
		var res struct {
			Count int `json:"count"`
			Data  []struct {
				Ref string `json:"ref"`
			} `json:"data"`
		}
		if err := json.Unmarshal([]byte(body), &res); err != nil || status != 200 {
			t.Fatalf("list ?ref=%s = %d: %s", ref, status, first(body, 200))
		}
		if res.Count != 1 || len(res.Data) != 1 {
			t.Fatalf("list ?ref=%s returned %d session(s), want 1", ref, res.Count)
		}
		if res.Data[0].Ref != testRef {
			t.Errorf("session stored with ref %q, want %q", res.Data[0].Ref, testRef)
		}
	}

	if aliasDBExists() {
		t.Error("the alias got a database of its own")
	}
}
