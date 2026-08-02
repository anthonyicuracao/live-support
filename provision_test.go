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
	tok, err := mintApplianceToken("test-connect-secret", ref, "", "", "", time.Hour)
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
	// A token signed with the wrong secret must not validate.
	tok, _ := mintApplianceToken("a-totally-different-secret-value", testRef, "", "", "", time.Hour)
	if code, _ := postForm(t, newClient(t), srv.URL+"/provision?t="+tok, url.Values{}); code != 403 {
		t.Fatalf("wrong-secret token: want 403, got %d", code)
	}
}

// TestProvisionCreatesUnknownTenant: /provision must bring a tenant that does
// not yet exist into being. It is the platform's provisioning endpoint, so
// that is its entire purpose.
//
// Every other test uses newServer's ref, which the harness creates up front —
// so when /provision was left resolving tenants through the existing-only
// lookup, it silently lost this and the suite stayed green.
func TestProvisionCreatesUnknownTenant(t *testing.T) {
	srv, _ := newServer(t)
	const fresh = "brand-new-tenant.example"

	link := provisionInvite(t, srv, fresh)
	redeemAdmin(t, link, fresh, "owner", "owner-password-123")

	db, err := dbs.getExisting(fresh)
	if err != nil {
		t.Fatalf("/provision did not create %q: %v", fresh, err)
	}
	var admins int
	if err := db.QueryRow(`SELECT COUNT(*) FROM users WHERE role = ?`, RoleAdmin).Scan(&admins); err != nil {
		t.Fatal(err)
	}
	if admins != 1 {
		t.Fatalf("want exactly one admin in the new tenant, got %d", admins)
	}
}

// TestRefCaseIsInsensitive: a ref differing only in case is the same tenant.
// Ron hit the opposite — a mis-typed capital resolved to a different database
// with none of his users in it, so a correct password reported a failed login.
func TestRefCaseIsInsensitive(t *testing.T) {
	newServer(t)
	lower, err := dbs.get("Case.Example")
	if err != nil {
		t.Fatal(err)
	}
	upper, err := dbs.get("CASE.EXAMPLE")
	if err != nil {
		t.Fatal(err)
	}
	if lower != upper {
		t.Fatal("refs differing only in case resolved to different databases")
	}
	if got := safeRefFile("Mixed.Case.Example"); got != "mixed.case.example" {
		t.Fatalf("safeRefFile did not normalise: %q", got)
	}
}
