package main

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

const agentIMMaxLen = 4000

func isMessageablePeer(db *sql.DB, userID int64) bool {
	var id int64
	err := db.QueryRow(`SELECT id FROM users WHERE id = ? AND active = 1`, userID).Scan(&id)
	return err == nil
}

func agentPicture(db *sql.DB, userID int64) string {
	var pic string
	_ = db.QueryRow(`SELECT picture FROM agent_availability WHERE user_id = ?`, userID).Scan(&pic)
	return pic
}

func (a *authApp) agentIMSendHandler(w http.ResponseWriter, r *http.Request) {
	info := authFrom(r.Context())
	if info == nil {
		errJSON(w, 401, "not signed in")
		return
	}
	var body struct {
		ToUserID int64  `json:"toUserId"`
		Body     string `json:"body"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		errJSON(w, 400, "bad json")
		return
	}
	if body.ToUserID == 0 {
		errJSON(w, 400, "missing recipient")
		return
	}
	if body.ToUserID == info.user.ID {
		errJSON(w, 400, "cannot message yourself")
		return
	}
	text := strings.TrimSpace(body.Body)
	if text == "" {
		errJSON(w, 400, "empty message")
		return
	}
	if len(text) > agentIMMaxLen {
		text = text[:agentIMMaxLen]
	}
	if !isMessageablePeer(info.db, body.ToUserID) {
		errJSON(w, 404, "not found")
		return
	}

	now := time.Now()
	payload, _ := json.Marshal(map[string]any{
		"type":        "im",
		"fromUserId":  info.user.ID,
		"fromName":    agentDisplayName(info.db, info.user.ID),
		"fromPicture": agentPicture(info.db, info.user.ID),
		"body":        text,
		"ts":          now.UnixMilli(),
	})
	ch := userInboxChannel(info.ref, body.ToUserID)
	delivered := hub.broadcastCount(ch, "message", payload)

	writeJSON(w, 200, map[string]any{
		"ok":        true,
		"delivered": delivered > 0,
		"consoles":  delivered,
		"ts":        now.UnixMilli(),
	})
}

func (a *authApp) mountAgentIM(mux *http.ServeMux) {
	mux.Handle("POST /api/im/send", a.authedJSON(a.agentIMSendHandler))
}
