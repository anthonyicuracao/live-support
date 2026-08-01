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
	"os"
	"strconv"
	"time"
)

// discoveryFreshness bounds how long an OFFLINE-reachable (closed-console)
// agent keeps appearing in guest discovery after their console was last seen.
// It does NOT clear availability — only Pause/logout may do that ("Available
// until Pause or log out"). It only stops RINGING a record whose console has
// been gone for the whole window, so a truly abandoned ghost ages out of
// discovery while a just-closed laptop stays reachable for the grace window
// (an open console touches updated_at every few minutes; a push-woken reopen
// re-touches it). A live agent is surfaced via WS presence and is never
// subject to this filter. Override with DISCOVERY_FRESHNESS_HOURS; 0 disables.
func discoveryFreshness() time.Duration {
	if v := os.Getenv("DISCOVERY_FRESHNESS_HOURS"); v != "" {
		if h, err := strconv.Atoi(v); err == nil {
			return time.Duration(h) * time.Hour
		}
	}
	return 24 * time.Hour
}

// modes is an agent's per-modality intent: what kinds of contact they are
// taking right now. Distinct from device capability (has_camera).
type modes struct {
	Chat  bool `json:"chat"`
	Audio bool `json:"audio"`
	Video bool `json:"video"`
}

func (m modes) any() bool { return m.Chat || m.Audio || m.Video }

// allows reports whether this agent takes the given call type. Unknown types
// are refused rather than allowed: a positive whitelist, so a typo or a future
// call type cannot silently ring everyone.
func (m modes) allows(callType string) bool {
	switch callType {
	case callTypeChat:
		return m.Chat
	case callTypeAudio:
		return m.Audio
	case callTypeVideo:
		return m.Video
	default:
		return false
	}
}

// The call types the system recognises, in one place so the ring gate, the
// guest buttons and the notification wording cannot drift apart.
const (
	callTypeChat  = "chat"
	callTypeAudio = "audio"
	callTypeVideo = "video"
)

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

func upsertAvailability(db *sql.DB, ref string, userID int64, available bool, sessionID, displayName string, hasCamera bool, picture, onlineSince string, m modes) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := db.Exec(
		`INSERT INTO agent_availability (user_id, ref, available, session_id, display_name, has_camera, picture, online_since, updated_at, chat_ok, audio_ok, video_ok)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(user_id) DO UPDATE SET
		   ref = excluded.ref,
		   available = excluded.available,
		   session_id = excluded.session_id,
		   display_name = excluded.display_name,
		   has_camera = excluded.has_camera,
		   picture = excluded.picture,
		   online_since = excluded.online_since,
		   updated_at = excluded.updated_at,
		   chat_ok = excluded.chat_ok,
		   audio_ok = excluded.audio_ok,
		   video_ok = excluded.video_ok`,
		userID, ref, b2i(available), sessionID, displayName, hasCamera, picture, onlineSince, now,
		b2i(m.Chat), b2i(m.Audio), b2i(m.Video))
	return err
}

// userModes reads an agent's per-modality intent. A missing row means no
// availability at all, so every modality is off.
func userModes(db *sql.DB, userID int64) modes {
	var chat, audio, video int
	err := db.QueryRow(
		`SELECT chat_ok, audio_ok, video_ok FROM agent_availability WHERE user_id = ?`, userID).
		Scan(&chat, &audio, &video)
	if err != nil {
		return modes{}
	}
	return modes{Chat: chat == 1, Audio: audio == 1, Video: video == 1}
}

// userTakesCallType is the ring gate's modality check: is this agent available
// AT ALL, and do they take this particular kind of contact.
//
// Separate from userIsAvailable because a stale guest roster is the normal
// case, not an edge case — a guest may hold a roster from before the agent
// turned video off, and the ring path is where that has to be caught.
func userTakesCallType(db *sql.DB, userID int64, callType string) bool {
	if !userIsAvailable(db, userID) {
		return false
	}
	return userModes(db, userID).allows(callType)
}

func clearAvailability(db *sql.DB, userID int64) {
	_, _ = db.Exec(`UPDATE agent_availability SET available = 0 WHERE user_id = ?`, userID)
}

// userIsAvailable reports the durable availability gate for a user. Used by the
// ring handler so Pause/logout takes effect server-side immediately.
//
// The users JOIN mirrors what /api/agents/available already enforces. Discovery
// and the ring gate must agree, and of the two the ring gate is the one that
// actually wakes a phone — it has no business being the laxer check.
func userIsAvailable(db *sql.DB, userID int64) bool {
	var avail int
	err := db.QueryRow(
		`SELECT a.available FROM agent_availability a
		 JOIN users u ON u.id = a.user_id AND u.active = 1
		 WHERE a.user_id = ?`, userID).Scan(&avail)
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
	var avail, hasCamera, chatOK, audioOK, videoOK int
	var sessionID, displayName, picture, onlineSince string
	err := info.db.QueryRow(
		`SELECT available, session_id, display_name, has_camera, picture, online_since,
		        chat_ok, audio_ok, video_ok
		 FROM agent_availability WHERE user_id = ?`, info.user.ID).
		Scan(&avail, &sessionID, &displayName, &hasCamera, &picture, &onlineSince,
			&chatOK, &audioOK, &videoOK)
	if err != nil {
		// No row yet. Report the defaults the console should start from rather
		// than a bare false, so a first-time agent sees Chat and Audio armed
		// and only has to opt into video.
		writeJSON(w, 200, map[string]any{
			"available": false,
			"modes":     modes{Chat: true, Audio: true},
		})
		return
	}
	writeJSON(w, 200, map[string]any{
		"available":   avail == 1,
		"sessionId":   sessionID,
		"displayName": displayName,
		"hasCamera":   hasCamera == 1,
		"picture":     picture,
		"onlineSince": onlineSince,
		"modes":       modes{Chat: chatOK == 1, Audio: audioOK == 1, Video: videoOK == 1},
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
		Modes       *modes `json:"modes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		errJSON(w, 400, "bad json")
		return
	}
	// A caller that omits modes entirely keeps whatever is stored, so an older
	// console build cannot silently wipe an agent's modality choices.
	m := userModes(info.db, info.user.ID)
	if body.Modes != nil {
		m = *body.Modes
	}
	// Available with nothing selected is not a state worth storing: it would be
	// an agent who appears on duty and can be reached by nothing. Treat it as
	// Pause, which is what it means.
	if body.Available && !m.any() {
		body.Available = false
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
		body.SessionID, body.DisplayName, body.HasCamera, body.Picture, body.OnlineSince, m); err != nil {
		errJSON(w, 500, "store failed")
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "available": body.Available, "modes": m})
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
	// JOIN users so an orphaned availability row (deleted account) or a
	// deactivated agent can never surface as a callable ghost — only a real,
	// active user with a push subscription is discoverable.
	query := `SELECT DISTINCT a.user_id, a.session_id, a.display_name, a.has_camera, a.picture, a.online_since,
		        a.chat_ok, a.audio_ok, a.video_ok
		 FROM agent_availability a
		 JOIN push_subscriptions p ON p.user_id = a.user_id
		 JOIN users u ON u.id = a.user_id AND u.active = 1
		 WHERE a.available = 1`
	var rows *sql.Rows
	if window := discoveryFreshness(); window > 0 {
		// updated_at is RFC3339 UTC — lexically comparable. Hide records whose
		// console hasn't been seen within the window (stale ghosts).
		cutoff := time.Now().UTC().Add(-window).Format(time.RFC3339)
		rows, err = db.Query(query+` AND a.updated_at > ?`, cutoff)
	} else {
		rows, err = db.Query(query)
	}
	if err != nil {
		writeJSON(w, 200, map[string]any{"agents": []any{}})
		return
	}
	defer rows.Close()
	agents := []map[string]any{}
	for rows.Next() {
		var userID int64
		var sessionID, displayName, picture, onlineSince string
		var hasCamera, chatOK, audioOK, videoOK int
		if err := rows.Scan(&userID, &sessionID, &displayName, &hasCamera, &picture, &onlineSince,
			&chatOK, &audioOK, &videoOK); err == nil {
			agents = append(agents, map[string]any{
				// user_id is the address a guest uses to REQUEST a conversation
				// (POST /api/conversation/start). Publishing it is safe because
				// it is not a capability: the server still gates availability,
				// modality and chat capacity, and it names no channel.
				//
				// session_id is deliberately NOT here. It used to be, and that
				// is what broke the original security model: it is the key of
				// the agent's inbox channel, so publishing it handed every
				// passer-by the ability to read and inject an agent's traffic.
				// Conversations are keyed by a random cid instead, and the cid
				// appears in no public response.
				"user_id": userID,
				"name":    displayName,
				"role":    "auth",
				"status":  "available",
				// has_mic was hardcoded true and has_camera came from device
				// capability, so a guest was told what an agent COULD do rather
				// than what they were willing to do. Both now report intent.
				"has_mic":    audioOK == 1,
				"has_camera": hasCamera == 1 && videoOK == 1,
				"modes": map[string]bool{
					"chat":  chatOK == 1,
					"audio": audioOK == 1,
					"video": videoOK == 1,
				},
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
