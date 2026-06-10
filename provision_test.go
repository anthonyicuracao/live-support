package main

import (
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

func provisionToken(t *testing.T, ref string) string {
	t.Helper()
	tok, err := encryptSSO("test-connect-secret", ref, provisionSentinel, "admin", time.Now().Unix())
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func provisionInvite(t *testing.T, srv *httptest.Server, ref string) string {
	t.Helper()
	_, body := postForm(t, newClient(t), srv.URL+"/provision?t="+provisionToken(t, ref), url.Values{})
	var r struct {
		InviteURL string `json:"invite_url"`
	}
	if err := json.Unmarshal([]byte(body), &r); err != nil || r.InviteURL == "" {
		t.Fatalf("bad provision response: %s", first(body, 200))
	}
	return r.InviteURL
}

func redeemAdmin(t *testing.T, link, ref, username, pass string) {
	t.Helper()
	c := newClient(t)
	_, rp := getBody(t, c, link)
	postForm(t, c, link, url.Values{
		"csrf": {csrfFrom(t, rp)}, "t": {tokenOf(t, link)}, "ref": {ref},
		"username": {username}, "new": {pass}, "confirm": {pass},
	})
}

// TestProvisionAdminInviteAndReclaim: the platform provisions a password-capable
// admin (the standalone-login counterpart to SSO /sso); a repeat with the
// same email reclaims that account (forgot-password break-glass).
func TestProvisionAdminInviteAndReclaim(t *testing.T) {
	srv, _ := newServer(t)

	redeemAdmin(t, provisionInvite(t, srv, testRef), testRef, "owner", "owner-password-123")
	if !canLogin(t, srv, "owner", "owner-password-123") {
		t.Fatal("owner should log in after provisioning")
	}

	// Re-provision; redeem with the SAME email reclaims the account (reset).
	redeemAdmin(t, provisionInvite(t, srv, testRef), testRef, "owner", "recovered-pw-456")
	if !canLogin(t, srv, "owner", "recovered-pw-456") {
		t.Fatal("owner should log in with the recovered password")
	}
	if canLogin(t, srv, "owner", "owner-password-123") {
		t.Fatal("old password should no longer work after reclaim")
	}

	var admins int
	db, _ := dbs.get(testRef)
	if err := db.QueryRow(`SELECT COUNT(*) FROM users WHERE role = ?`, RoleAdmin).Scan(&admins); err != nil {
		t.Fatal(err)
	}
	if admins != 1 {
		t.Fatalf("reclaim should not duplicate the admin, got %d", admins)
	}
}

func TestProvisionRejectsBadToken(t *testing.T) {
	srv, _ := newServer(t)
	if code, _ := postForm(t, newClient(t), srv.URL+"/provision?t=garbage", url.Values{}); code != 403 {
		t.Fatalf("garbage token: want 403, got %d", code)
	}
	// A normal SSO token (real username, not the sentinel) must not provision.
	tok, _ := encryptSSO("test-connect-secret", testRef, "alice", "admin", time.Now().Unix())
	if code, _ := postForm(t, newClient(t), srv.URL+"/provision?t="+tok, url.Values{}); code != 403 {
		t.Fatalf("non-sentinel sso: want 403, got %d", code)
	}
}
