package main

import (
	"net/http"
	"net/url"
	"time"
)

// provisionSentinel marks a hand-off token as a tenant-provisioning request (vs.
// a real sign-in). The platform mints it with the shared CONNECT_SECRET, exactly
// like a hand-off link but with the username set to this sentinel:
//
//	encryptHandoff(CONNECT_SECRET, ref, ProvisionSentinel, "admin", now)
const provisionSentinel = "__provision__"

// provision creates (or ensures) a tenant from a platform-signed token and
// returns an admin invite link as JSON. The customer opens the link, sets their
// email + password, and becomes a password-capable admin who can sign in at
// /login and manage users — the standalone counterpart to the SSO /handoff. A
// repeat call whose invite is redeemed with an already-used email reclaims that
// account (the forgot-password break-glass). Authorized solely by the shared
// secret, so only the platform can call it.
func (a *authApp) provision(w http.ResponseWriter, r *http.Request) {
	if a.handoffSecret == "" {
		http.Error(w, "provisioning is disabled (no CONNECT_SECRET)", http.StatusBadRequest)
		return
	}
	tok := r.URL.Query().Get("t")
	if tok == "" {
		_ = r.ParseForm()
		tok = r.PostFormValue("t")
	}
	ref, username, _, ts, err := decryptHandoff(a.handoffSecret, tok)
	now := time.Now().Unix()
	if err != nil || username != provisionSentinel || ref == "" ||
		now-ts > int64(handoffTTL.Seconds()) || ts-now > 60 {
		http.Error(w, "invalid provisioning token", http.StatusForbidden)
		return
	}

	db := tenantDB(ref)
	if db == nil {
		http.Error(w, "invalid tenant ref", http.StatusBadRequest)
		return
	}
	// Deliberately do NOT bootstrap a shared-password admin — the returned invite
	// creates (or reclaims) the admin.
	raw, err := createInviteRow(db, RoleAdmin, "", 0, a.inviteTTL)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	link := a.requestBaseURL(r) + "/invite?ref=" + url.QueryEscape(ref) + "&t=" + raw
	writeJSON(w, http.StatusOK, map[string]any{"ref": ref, "invite_url": link})
}
