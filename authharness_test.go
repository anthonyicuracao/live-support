package main

import (
	"database/sql"
	"html"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// Integration tests for the built-in auth / user-management area, ported from
// the csat project (internal/admin). They drive the real HTTP handlers through
// httptest, against a fresh per-test tenant database.

const (
	initialPW = "bootstrap-initial-pw"
	adminPW   = "a-brand-new-password"
	agentPW   = "agent-password-123"
	testRef   = "demo" // every request is scoped to this tenant
)

var (
	csrfRE       = regexp.MustCompile(`name="csrf" value="([^"]+)"`)
	inviteLinkRE = regexp.MustCompile(`https?://[^"<\s]+/invite\?[^"<\s]+`)
	resetLinkRE  = regexp.MustCompile(`https?://[^"<\s]+/reset\?[^"<\s]+`)
)

// newServer boots the auth app against a fresh temp data dir and returns the
// test server plus the tenant DB for testRef (for direct assertions).
func newServer(t *testing.T) (*httptest.Server, *sql.DB) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("DATA_DIR", dir)
	t.Setenv("ADMIN_USERNAME", "admin")
	t.Setenv("ADMIN_INITIAL_PASSWORD", initialPW)
	t.Setenv("CONNECT_SECRET", "test-connect-secret") // keep ensureConnectSecret off disk
	t.Setenv("SITE_NAME", "Test Co")

	pool, err := newDBPool(dir)
	if err != nil {
		t.Fatalf("db pool: %v", err)
	}
	dbs = pool       // package global the handlers resolve tenants through
	hub = newHub()   // ring fan-out broadcasts need a live (if empty) hub

	a, err := newAuthApp()
	if err != nil {
		t.Fatalf("newAuthApp: %v", err)
	}
	mux := http.NewServeMux()
	a.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	tdb, err := dbs.get(testRef)
	if err != nil {
		t.Fatalf("tenant db: %v", err)
	}
	return srv, tdb
}

// wr builds a server URL with the tenant ref appended to the query string.
func wr(srv *httptest.Server, path string) string {
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	return srv.URL + path + sep + "ref=" + testRef
}

func newClient(t *testing.T) *http.Client {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	return &http.Client{Jar: jar}
}

func getBody(t *testing.T, c *http.Client, target string) (int, string) {
	t.Helper()
	resp, err := c.Get(target)
	if err != nil {
		t.Fatalf("GET %s: %v", target, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func postForm(t *testing.T, c *http.Client, target string, form url.Values) (int, string) {
	t.Helper()
	resp, err := c.PostForm(target, form)
	if err != nil {
		t.Fatalf("POST %s: %v", target, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func csrfFrom(t *testing.T, body string) string {
	t.Helper()
	m := csrfRE.FindStringSubmatch(body)
	if len(m) < 2 {
		t.Fatalf("no csrf token in page: %s", first(body, 300))
	}
	return m[1]
}

// loginAdmin performs the bootstrap login + forced password change and returns
// an authenticated client.
func loginAdmin(t *testing.T, srv *httptest.Server) *http.Client {
	t.Helper()
	c := newClient(t)
	_, page := getBody(t, c, wr(srv, "/login")) // GET also bootstraps the tenant admin
	_, after := postForm(t, c, wr(srv, "/login"), url.Values{
		"csrf": {csrfFrom(t, page)}, "username": {"admin"}, "password": {initialPW},
	})
	postForm(t, c, wr(srv, "/account/password"), url.Values{
		"csrf": {csrfFrom(t, after)}, "current": {initialPW},
		"new": {adminPW}, "confirm": {adminPW},
	})
	return c
}

// inviteAgent creates an agent via the invite flow and redeems it.
func inviteAgent(t *testing.T, srv *httptest.Server, admin *http.Client, username string) {
	t.Helper()
	_, usersPage := getBody(t, admin, wr(srv, "/users"))
	_, body := postForm(t, admin, wr(srv, "/users/invite"), url.Values{
		"csrf": {csrfFrom(t, usersPage)}, "role": {"agent"}, "username": {username},
	})
	link := extractLink(t, inviteLinkRE, body, "invite")
	invitee := newClient(t)
	_, redeemPage := getBody(t, invitee, link)
	postForm(t, invitee, link, url.Values{
		"csrf": {csrfFrom(t, redeemPage)}, "t": {tokenOf(t, link)},
		"new": {agentPW}, "confirm": {agentPW},
	})
}

// canLogin reports whether the credentials yield an authenticated session. It
// probes /api/me (200 when signed in, 401 otherwise) and does not follow the
// post-login redirect to /auth.html.
func canLogin(t *testing.T, srv *httptest.Server, username, password string) bool {
	t.Helper()
	c := newClient(t)
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	_, page := getBody(t, c, wr(srv, "/login"))
	postForm(t, c, wr(srv, "/login"), url.Values{
		"csrf": {csrfFrom(t, page)}, "username": {username}, "password": {password},
	})
	resp, err := c.Get(wr(srv, "/api/me"))
	if err != nil {
		t.Fatalf("GET /api/me: %v", err)
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// extractLink pulls the first matching (HTML-escaped) link out of a page and
// unescapes it so the &amp; in the query string becomes a usable &.
func extractLink(t *testing.T, re *regexp.Regexp, body, kind string) string {
	t.Helper()
	m := re.FindString(body)
	if m == "" {
		t.Fatalf("no %s link in page: %s", kind, first(body, 400))
	}
	return html.UnescapeString(m)
}

func tokenOf(t *testing.T, link string) string {
	t.Helper()
	u, err := url.Parse(link)
	if err != nil {
		t.Fatalf("parse link %q: %v", link, err)
	}
	return u.Query().Get("t")
}

func userIDByName(t *testing.T, db *sql.DB, name string) string {
	t.Helper()
	var id int64
	if err := db.QueryRow(`SELECT id FROM users WHERE username = ?`, name).Scan(&id); err != nil {
		t.Fatalf("lookup user %q: %v", name, err)
	}
	return strconv.FormatInt(id, 10)
}

func countByName(t *testing.T, db *sql.DB, name string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM users WHERE username = ?`, name).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func first(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
