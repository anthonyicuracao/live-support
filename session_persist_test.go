package main

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

// mustUser inserts a bare active agent directly, so these tests exercise the
// session layer without dragging the invite/redeem flow in behind them.
func mustUser(t *testing.T, db *sql.DB, username string) int64 {
	t.Helper()
	res, err := db.Exec(
		`INSERT INTO users(username, password_hash, role, must_change_pw, active, created_at)
		 VALUES(?, '', ?, 0, 1, ?)`, username, RoleAgent, time.Now().Unix())
	if err != nil {
		t.Fatalf("insert user %q: %v", username, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("last insert id: %v", err)
	}
	return id
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// Tests for permanent agent sessions (PERSISTENT-LOGIN-PLAN.md).
//
// The behaviour being pinned: an agent who is rung once a month must still be
// signed in when it happens, and an admin must be able to SEE that standing
// session and revoke it. Every test here fails against the pre-change code.

// ttl 0 means "never": the row stores 0 and lookup keeps resolving it long past
// any window the old 168-hour default would have allowed.
func TestCreateAuthSessionNeverExpires(t *testing.T) {
	_, db := newServer(t)

	uid := mustUser(t, db, "owner-agent")
	raw, _, err := createAuthSession(db, uid, 0)
	if err != nil {
		t.Fatalf("createAuthSession: %v", err)
	}

	var expires int64
	if err := db.QueryRow(`SELECT expires_at FROM auth_sessions WHERE id = ?`, hashToken(raw)).
		Scan(&expires); err != nil {
		t.Fatalf("read expires_at: %v", err)
	}
	if expires != sessionNever {
		t.Fatalf("expires_at = %d, want %d (never)", expires, sessionNever)
	}

	if _, _, err := lookupAuthSession(db, raw); err != nil {
		t.Fatalf("permanent session did not resolve: %v", err)
	}
}

// sessionExpired is the single guard the whole design rests on, so it is worth
// pinning directly rather than only through its callers.
func TestSessionExpiredGuard(t *testing.T) {
	now := time.Now().Unix()
	cases := []struct {
		name      string
		expiresAt int64
		want      bool
	}{
		{"never is never expired, even far in the future", sessionNever, false},
		{"past deadline is expired", now - 1, true},
		{"future deadline is live", now + 3600, false},
		{"exactly now is expired", now, true},
	}
	for _, c := range cases {
		if got := sessionExpired(c.expiresAt, now); got != c.want {
			t.Errorf("%s: sessionExpired(%d, %d) = %v, want %v", c.name, c.expiresAt, now, got, c.want)
		}
	}
	// The one that would break everything: a permanent session must survive a
	// clock far beyond any real deadline.
	if sessionExpired(sessionNever, now+100*365*24*3600) {
		t.Error("permanent session expired against a century-later clock")
	}
}

// The sweeper must reap genuinely expired rows and leave permanent ones. Before
// the expires_at != 0 guard this deleted every permanent session on the first
// hourly tick, because 0 is less than any current timestamp.
func TestSweepKeepsPermanentSessions(t *testing.T) {
	_, db := newServer(t)

	permanentUser := mustUser(t, db, "permanent")
	expiringUser := mustUser(t, db, "expiring")

	permanent, _, err := createAuthSession(db, permanentUser, 0)
	if err != nil {
		t.Fatalf("permanent session: %v", err)
	}
	expiring, _, err := createAuthSession(db, expiringUser, time.Hour)
	if err != nil {
		t.Fatalf("expiring session: %v", err)
	}
	// Backdate the expiring one rather than sleeping.
	if _, err := db.Exec(`UPDATE auth_sessions SET expires_at = ? WHERE id = ?`,
		time.Now().Add(-time.Hour).Unix(), hashToken(expiring)); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	sweepAuthSessions(db)

	if _, _, err := lookupAuthSession(db, permanent); err != nil {
		t.Errorf("sweeper reaped the permanent session: %v", err)
	}
	if _, _, err := lookupAuthSession(db, expiring); err == nil {
		t.Error("sweeper left an expired session resolvable")
	}
}

// 0 must survive envHours. The old `n > 0` guard swallowed it and returned the
// default, which is why "never" could not be configured at all.
func TestEnvHoursAcceptsZero(t *testing.T) {
	const key = "TEST_TTL_HOURS"
	cases := []struct {
		set  string
		want time.Duration
	}{
		{"0", 0},                 // the whole point: 0 means never
		{"24", 24 * time.Hour},   // ordinary value
		{"", 168 * time.Hour},    // unset falls back to the default
		{"-1", 168 * time.Hour},  // negative has no meaning
		{"abc", 168 * time.Hour}, // unparseable has no meaning
	}
	for _, c := range cases {
		t.Setenv(key, c.set)
		if got := envHours(key, 168); got != c.want {
			t.Errorf("envHours(%q) = %v, want %v", c.set, got, c.want)
		}
	}
}

// Revocation is the entire security model for a session that never expires, so
// each path gets pinned against a PERMANENT session specifically.
func TestRevocationEndsPermanentSessions(t *testing.T) {
	t.Run("deactivate", func(t *testing.T) {
		_, db := newServer(t)
		uid := mustUser(t, db, "to-deactivate")
		raw, _, _ := createAuthSession(db, uid, 0)
		if err := deactivateUser(db, uid); err != nil {
			t.Fatalf("deactivate: %v", err)
		}
		if _, _, err := lookupAuthSession(db, raw); err == nil {
			t.Error("deactivated user still resolves a permanent session")
		}
	})

	t.Run("sign out everywhere", func(t *testing.T) {
		_, db := newServer(t)
		uid := mustUser(t, db, "to-signout")
		a, _, _ := createAuthSession(db, uid, 0)
		b, _, _ := createAuthSession(db, uid, 0) // a second browser
		deleteUserAuthSessions(db, uid)
		for i, raw := range []string{a, b} {
			if _, _, err := lookupAuthSession(db, raw); err == nil {
				t.Errorf("session %d survived sign-out-everywhere", i)
			}
		}
	})

	t.Run("sign out clears availability", func(t *testing.T) {
		_, db := newServer(t)
		uid := mustUser(t, db, "available-agent")
		_, _, _ = createAuthSession(db, uid, 0)
		if err := upsertAvailability(db, testRef, uid, true, "sess", "Agent", false, "", "", modes{Chat: true, Audio: true}); err != nil {
			t.Fatalf("upsertAvailability: %v", err)
		}
		if !userIsAvailable(db, uid) {
			t.Fatal("precondition: agent should be available")
		}
		deleteUserAuthSessions(db, uid)
		if userIsAvailable(db, uid) {
			t.Error("agent still available after being signed out — they would still be rung")
		}
	})
}

// A deactivated agent must not be rung. Discovery already filtered them; the
// ring gate did not, and the ring gate is the one that wakes a phone.
func TestUserIsAvailableRequiresActiveUser(t *testing.T) {
	_, db := newServer(t)
	uid := mustUser(t, db, "deactivated-but-available")
	if err := upsertAvailability(db, testRef, uid, true, "sess", "Agent", false, "", "", modes{Chat: true, Audio: true}); err != nil {
		t.Fatalf("upsertAvailability: %v", err)
	}
	if !userIsAvailable(db, uid) {
		t.Fatal("precondition: agent should be available")
	}
	if err := deactivateUser(db, uid); err != nil {
		t.Fatalf("deactivate: %v", err)
	}
	if userIsAvailable(db, uid) {
		t.Error("deactivated user still passes the ring gate")
	}
}

// The admin users page must show that a session exists — the visibility the
// permanent-session security model depends on.
func TestUsersPageShowsSignedIn(t *testing.T) {
	srv, db := newServer(t)
	c := loginAdmin(t, srv)

	_, page := getBody(t, c, wr(srv, "/users"))
	if !strings.Contains(page, "Signed in") {
		t.Fatal(`users page has no "Signed in" column`)
	}
	if !strings.Contains(page, "Sign out everywhere") {
		t.Error(`users page offers no "Sign out everywhere" action for a signed-in user`)
	}

	// The admin is signed in, so listUsers must count their session.
	users, err := listUsers(db)
	if err != nil {
		t.Fatalf("listUsers: %v", err)
	}
	var found bool
	for _, u := range users {
		if u.Username == "admin" {
			found = true
			if u.Sessions < 1 {
				t.Errorf("admin shows %d sessions while logged in", u.Sessions)
			}
		}
	}
	if !found {
		t.Fatal("admin user missing from listUsers")
	}
}

// Signing another user out from the admin screen actually ends their session.
func TestSignOutEverywhereHandler(t *testing.T) {
	srv, db := newServer(t)
	c := loginAdmin(t, srv)

	uid := mustUser(t, db, "agent-with-session")
	raw, _, _ := createAuthSession(db, uid, 0)

	_, page := getBody(t, c, wr(srv, "/users"))
	postForm(t, c, wr(srv, "/users/signout"), url.Values{
		"csrf":    {csrfFrom(t, page)},
		"user_id": {itoa(uid)},
	})

	if _, _, err := lookupAuthSession(db, raw); err == nil {
		t.Error("session survived POST /users/signout")
	}
}

// loginAgo is rendered into the users table, so its edge cases matter.
func TestLoginAgo(t *testing.T) {
	if got := loginAgo(0); got != "never" {
		t.Errorf("loginAgo(0) = %q, want %q", got, "never")
	}
	if got := loginAgo(time.Now().Add(-30 * time.Second).Unix()); got != "just now" {
		t.Errorf("30s ago = %q, want %q", got, "just now")
	}
	if got := loginAgo(time.Now().Add(-3 * time.Hour).Unix()); got != "3h ago" {
		t.Errorf("3h ago = %q, want %q", got, "3h ago")
	}
	if got := loginAgo(time.Now().Add(-5 * 24 * time.Hour).Unix()); got != "5d ago" {
		t.Errorf("5d ago = %q, want %q", got, "5d ago")
	}
}

// wsSessionToken is what stops WS presence outliving a session. Guests must
// keep working unauthenticated, so both directions are pinned.
func TestWSSessionToken(t *testing.T) {
	_, db := newServer(t)

	t.Run("guest has no session and stays unauthenticated", func(t *testing.T) {
		r := httptest.NewRequest("GET", "/ws", nil)
		if _, _, _, ok := wsSessionToken(r); ok {
			t.Error("a cookieless request resolved a session")
		}
	})

	t.Run("agent with a live session resolves", func(t *testing.T) {
		uid := mustUser(t, db, "ws-agent")
		raw, _, _ := createAuthSession(db, uid, 0)
		r := httptest.NewRequest("GET", "/ws", nil)
		r.AddCookie(&http.Cookie{Name: sessionCookieName, Value: encodeSessionCookie(testRef, raw)})
		gotRef, gotRaw, gotUID, ok := wsSessionToken(r)
		if !ok {
			t.Fatal("agent with a valid session did not resolve")
		}
		if gotRef != testRef || gotRaw != raw {
			t.Errorf("resolved (%q, …), want (%q, …)", gotRef, testRef)
		}
		// The user id binds the ring inbox to its owner, so a connection cannot
		// name a colleague's inbox and listen to their calls.
		if gotUID != uid {
			t.Errorf("resolved user %d, want %d", gotUID, uid)
		}
	})

	t.Run("revoked session stops resolving", func(t *testing.T) {
		uid := mustUser(t, db, "ws-revoked")
		raw, _, _ := createAuthSession(db, uid, 0)
		r := httptest.NewRequest("GET", "/ws", nil)
		r.AddCookie(&http.Cookie{Name: sessionCookieName, Value: encodeSessionCookie(testRef, raw)})
		if _, _, _, ok := wsSessionToken(r); !ok {
			t.Fatal("precondition: session should resolve before revocation")
		}
		deleteUserAuthSessions(db, uid)
		if _, _, _, ok := wsSessionToken(r); ok {
			t.Error("revoked session still resolves — presence would outlive it")
		}
	})
}

// /dev/sso mints a real sign-in for ANY user, so it is a total authentication
// bypass and both of its gates have to hold. Pinned deliberately: this is the
// kind of convenience route that quietly becomes an open door.
func TestDevSSOIsDoublyGated(t *testing.T) {
	t.Run("address classification", func(t *testing.T) {
		cases := []struct {
			remote string
			want   bool
			why    string
		}{
			{"127.0.0.1:5555", true, "loopback"},
			{"[::1]:5555", true, "IPv6 loopback"},
			{"172.17.0.1:5555", true, "Docker bridge gateway — what the host looks like from inside a container"},
			{"192.168.1.50:5555", true, "LAN; allowed because SNAT makes it indistinguishable from the host"},
			{"203.0.113.7:5555", false, "public address — the one case this can still refuse"},
			{"8.8.8.8:5555", false, "public address"},
			{"", false, "unparseable"},
			{"not-an-address", false, "unparseable"},
		}
		for _, c := range cases {
			r := httptest.NewRequest("GET", "/dev/sso", nil)
			r.RemoteAddr = c.remote
			if got := isLocalOrPrivateRequest(r); got != c.want {
				t.Errorf("isLocalOrPrivateRequest(%q) = %v, want %v (%s)", c.remote, got, c.want, c.why)
			}
		}
	})
}
