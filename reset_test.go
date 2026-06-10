package main

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// TestForgotPasswordFlow: an agent files a request from /forgot, the admin sees
// it on /users, issues a reset link, the agent redeems it and can log in with
// the new password.
func TestForgotPasswordFlow(t *testing.T) {
	srv, db := newServer(t)
	admin := loginAdmin(t, srv)
	inviteAgent(t, srv, admin, "agent1")

	// 1. Agent submits the forgot-password form (no auth).
	pub := newClient(t)
	_, fp := getBody(t, pub, wr(srv, "/forgot"))
	_, sent := postForm(t, pub, wr(srv, "/forgot"), url.Values{
		"csrf": {csrfFrom(t, fp)}, "username": {"agent1"},
	})
	if !strings.Contains(sent, "administrator has been notified") {
		t.Fatalf("expected confirmation, got: %s", first(sent, 300))
	}

	// 2. Admin sees the pending request on the Users page.
	_, usersPage := getBody(t, admin, wr(srv, "/users"))
	if !strings.Contains(usersPage, "reset requested") {
		t.Fatalf("users page missing reset-requested badge: %s", first(usersPage, 800))
	}

	// 3. Admin issues a reset link.
	_, body := postForm(t, admin, wr(srv, "/users/reset"), url.Values{
		"csrf": {csrfFrom(t, usersPage)}, "user_id": {userIDByName(t, db, "agent1")},
	})
	link := extractLink(t, resetLinkRE, body, "reset")

	// Issuing the link clears the pending request.
	_, usersPage2 := getBody(t, admin, wr(srv, "/users"))
	if strings.Contains(usersPage2, "reset requested") {
		t.Fatalf("reset request should be cleared after issuing a link")
	}

	// 4. Agent redeems the reset link with a new password.
	redeemer := newClient(t)
	_, rp := getBody(t, redeemer, link)
	if !strings.Contains(rp, "Set a new password") {
		t.Fatalf("reset page missing: %s", first(rp, 300))
	}
	postForm(t, redeemer, link, url.Values{
		"csrf": {csrfFrom(t, rp)}, "t": {tokenOf(t, link)},
		"new": {"agent-new-password-456"}, "confirm": {"agent-new-password-456"},
	})

	// 5. New password works; old one doesn't.
	if !canLogin(t, srv, "agent1", "agent-new-password-456") {
		t.Fatalf("agent should log in with the new password")
	}
	if canLogin(t, srv, "agent1", agentPW) {
		t.Fatalf("old password should no longer work after reset")
	}

	// 6. The reset link is single-use.
	used := newClient(t)
	_, again := getBody(t, used, link)
	if !strings.Contains(again, "not valid") {
		t.Fatalf("reset link should be single-use, got: %s", first(again, 300))
	}
}

// TestResetReactivatesInactiveUser: a deactivated user can be recovered via a
// reset link, which flips them back to active.
func TestResetReactivatesInactiveUser(t *testing.T) {
	srv, db := newServer(t)
	admin := loginAdmin(t, srv)
	inviteAgent(t, srv, admin, "agent1")
	id := userIDByName(t, db, "agent1")

	// Deactivate the agent.
	_, usersPage := getBody(t, admin, wr(srv, "/users"))
	postForm(t, admin, wr(srv, "/users/deactivate"), url.Values{
		"csrf": {csrfFrom(t, usersPage)}, "user_id": {id},
	})
	if canLogin(t, srv, "agent1", agentPW) {
		t.Fatalf("deactivated user should not be able to log in")
	}

	// Admin issues a reset for the inactive user.
	_, usersPage = getBody(t, admin, wr(srv, "/users"))
	_, body := postForm(t, admin, wr(srv, "/users/reset"), url.Values{
		"csrf": {csrfFrom(t, usersPage)}, "user_id": {id},
	})
	link := extractLink(t, resetLinkRE, body, "reset")

	// Redeem it.
	redeemer := newClient(t)
	_, rp := getBody(t, redeemer, link)
	postForm(t, redeemer, link, url.Values{
		"csrf": {csrfFrom(t, rp)}, "t": {tokenOf(t, link)},
		"new": {"back-in-action-789"}, "confirm": {"back-in-action-789"},
	})

	// The account is active again and the new password works.
	if !canLogin(t, srv, "agent1", "back-in-action-789") {
		t.Fatalf("reset should reactivate the account and set the new password")
	}
	var active int
	if err := db.QueryRow(`SELECT active FROM users WHERE id = ?`, id).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if active != 1 {
		t.Fatalf("user should be active after reset, active=%d", active)
	}
}

// TestDeleteUserFreesUsername: deleting a user lets the same username be invited
// again — the exact collision that deactivate+re-invite caused.
func TestDeleteUserFreesUsername(t *testing.T) {
	srv, db := newServer(t)
	admin := loginAdmin(t, srv)
	inviteAgent(t, srv, admin, "agent1")
	id := userIDByName(t, db, "agent1")

	// Delete the user.
	_, usersPage := getBody(t, admin, wr(srv, "/users"))
	postForm(t, admin, wr(srv, "/users/delete"), url.Values{
		"csrf": {csrfFrom(t, usersPage)}, "user_id": {id},
	})
	if n := countByName(t, db, "agent1"); n != 0 {
		t.Fatalf("user should be gone after delete, found %d", n)
	}

	// Re-inviting the same username now succeeds (no "already taken").
	inviteAgent(t, srv, admin, "agent1")
	if n := countByName(t, db, "agent1"); n != 1 {
		t.Fatalf("username should be reusable after delete, found %d rows", n)
	}
	if !canLogin(t, srv, "agent1", agentPW) {
		t.Fatalf("re-invited user should be able to log in")
	}
}

// TestDeleteLastAdminBlocked guards the sole-admin lockout.
func TestDeleteLastAdminBlocked(t *testing.T) {
	srv, db := newServer(t)
	admin := loginAdmin(t, srv)

	id := userIDByName(t, db, "admin")
	_, usersPage := getBody(t, admin, wr(srv, "/users"))
	_, body := postForm(t, admin, wr(srv, "/users/delete"), url.Values{
		"csrf": {csrfFrom(t, usersPage)}, "user_id": {id},
	})
	if !strings.Contains(body, "last admin") && !strings.Contains(body, "your own account") {
		t.Fatalf("deleting the sole admin should be blocked, got: %s", first(body, 300))
	}
	if n := countByName(t, db, "admin"); n != 1 {
		t.Fatalf("admin should still exist, found %d", n)
	}
}

// TestAgentForbiddenFromUsers confirms role enforcement: a signed-in agent
// cannot reach the admin-only user-management page.
func TestAgentForbiddenFromUsers(t *testing.T) {
	srv, _ := newServer(t)
	admin := loginAdmin(t, srv)
	inviteAgent(t, srv, admin, "agent1")

	agent := newClient(t)
	_, page := getBody(t, agent, wr(srv, "/login"))
	postForm(t, agent, wr(srv, "/login"), url.Values{
		"csrf": {csrfFrom(t, page)}, "username": {"agent1"}, "password": {agentPW},
	})
	if code, _ := getBody(t, agent, wr(srv, "/users")); code != http.StatusForbidden {
		t.Fatalf("agent should be forbidden from /users, got %d", code)
	}
}
