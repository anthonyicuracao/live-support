// availability.go — Phase 3: durable agent availability ("Available until
// Pause or log out").
//
// WS presence only knows about live tabs, so before this file an agent who
// closed the console vanished from guest discovery even though Web Push could
// still wake and ring them. agent_availability is the server-side truth:
//   - set when the agent goes Available, cleared on Pause and on logout;
//   - merged into guest discovery via GET /api/agents/available (only agents
//     who ALSO hold a push subscription — an unreachable agent is never shown);
//   - enforced in /api/call/ring, so a Paused or logged-out agent can never be
//     pushed, even by a guest holding a stale roster.
package main

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"time"
)

func upsertAvailability(db *sql.DB, ref string, userID int64, available bool, sessionID, displayName string, hasCamera bool, picture, onlineSince string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	avail := 0
	if available {
		avail = 1
	}
	_, err := db.Exec(
		`INSERT INTO agent_availability (user_id, ref, available, session_id, display_name, has_camera, picture, online_since, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(user_id) DO UPDATE SET
		   ref = excluded.ref,
		   available = excluded.available,
		   session_id = excluded.session_id,
		   display_name = excluded.display_name,
		   has_camera = excluded.has_camera,
		   picture = excluded.picture,
		   online_since = excluded.online_since,
		   updated_at = excluded.updated_at`,
		userID, ref, avail, sessionID, displayName, hasCamera, picture, onlineSince, now)
	return err
}

func clearAvailability(db *sql.DB, userID int64) {
	_, _ = db.Exec(`UPDATE agent_availability SET available = 0 WHERE user_id = ?`, userID)
}

// userIsAvailable reports the durable availability gate for a user. Used by the
// ring handler so Pause/logout takes effect server-side immediately.
func userIsAvailable(db *sql.DB, userID int64) bool {
	var avail int
	err := db.QueryRow(`SELECT available FROM agent_availability WHERE user_id = ?`, userID).Scan(&avail)
	return err == nil && avail == 1
}

// GET /api/availability (authed): this user's durable availability — the
// console restores its toggle from this on load, so "Available until Pause or
// log out" survives tab close, browser quit, and reopening on another machine.
func getAvailabilityHandler(w http.ResponseWriter, r *http.Request) {
	info := authFrom(r.Context())
	if info == nil {
		errJSON(w, 401, "not signed in")
		return
	}
	var avail, hasCamera int
	var sessionID, displayName, picture, onlineSince string
	err := info.db.QueryRow(
		`SELECT available, session_id, display_name, has_camera, picture, online_since
		 FROM agent_availability WHERE user_id = ?`, info.user.ID).
		Scan(&avail, &sessionID, &displayName, &hasCamera, &picture, &onlineSince)
	if err != nil {
		writeJSON(w, 200, map[string]any{"available": false})
		return
	}
	writeJSON(w, 200, map[string]any{
		"available":   avail == 1,
		"sessionId":   sessionID,
		"displayName": displayName,
		"hasCamera":   hasCamera == 1,
		"picture":     picture,
		"onlineSince": onlineSince,
	})
}

// POST /api/availability (authed): set/clear durable availability. Called on
// every toggle flip; Pause posts available=false.
func setAvailabilityHandler(w http.ResponseWriter, r *http.Request) {
	info := authFrom(r.Context())
	if info == nil {
		errJSON(w, 401, "not signed in")
		return
	}
	var body struct {
		Available   bool   `json:"available"`
		Touch       bool   `json:"touch"`
		SessionID   string `json:"sessionId"`
		DisplayName string `json:"displayName"`
		HasCamera   bool   `json:"hasCamera"`
		Picture     string `json:"picture"`
		OnlineSince string `json:"onlineSince"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		errJSON(w, 400, "bad json")
		return
	}
	if body.Touch {
		// Touch: re-point the session id (and display fields) of an ALREADY
		// available record — used when a fresh tab resumes durable
		// availability. It must never set the availability bit itself, so a
		// slow in-flight touch can't race a Pause click and revive it.
		now := time.Now().UTC().Format(time.RFC3339)
		_, _ = info.db.Exec(
			`UPDATE agent_availability SET session_id = ?, display_name = ?, has_camera = ?, picture = ?, updated_at = ?
			 WHERE user_id = ? AND available = 1`,
			body.SessionID, body.DisplayName, body.HasCamera, body.Picture, now, info.user.ID)
		writeJSON(w, 200, map[string]any{"ok": true})
		return
	}
	if err := upsertAvailability(info.db, info.ref, info.user.ID, body.Available,
		body.SessionID, body.DisplayName, body.HasCamera, body.Picture, body.OnlineSince); err != nil {
		errJSON(w, 500, "store failed")
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// GET /api/agents/available?ref=… (public, ref-controlled): durable-available
// agents who can actually be reached (≥1 push subscription). Guests merge this
// with live WS presence so a closed-tab agent still shows as callable.
func agentsAvailableHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	ref := r.URL.Query().Get("ref")
	if ref == "" || !dbs.exists(ref) {
		writeJSON(w, 200, map[string]any{"agents": []any{}})
		return
	}
	db, err := dbs.get(ref)
	if err != nil {
		errJSON(w, 400, err.Error())
		return
	}
	rows, err := db.Query(
		`SELECT DISTINCT a.session_id, a.display_name, a.has_camera, a.picture, a.online_since
		 FROM agent_availability a
		 JOIN push_subscriptions p ON p.user_id = a.user_id
		 WHERE a.available = 1`)
	if err != nil {
		writeJSON(w, 200, map[string]any{"agents": []any{}})
		return
	}
	defer rows.Close()
	agents := []map[string]any{}
	for rows.Next() {
		var sessionID, displayName, picture, onlineSince string
		var hasCamera int
		if err := rows.Scan(&sessionID, &displayName, &hasCamera, &picture, &onlineSince); err == nil {
			agents = append(agents, map[string]any{
				"session_id":   sessionID,
				"name":         displayName,
				"role":         "auth",
				"status":       "available",
				"has_mic":      true,
				"has_camera":   hasCamera == 1,
				"picture":      picture,
				"online_since": onlineSince,
			})
		}
	}
	writeJSON(w, 200, map[string]any{"agents": agents})
}

func mountAvailability(mux *http.ServeMux, a *authApp) {
	mux.Handle("GET /api/availability", a.authedJSON(getAvailabilityHandler))
	mux.Handle("POST /api/availability", a.authedJSON(setAvailabilityHandler))
	mux.HandleFunc("GET /api/agents/available", agentsAvailableHandler)
}
