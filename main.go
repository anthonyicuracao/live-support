// main.go — single-binary replacement for the Node/Supabase stack.
//
// Provides:
//   - Embedded static frontend (index.html, auth.html, js/, public/)
//   - SQLite storage (sessions, calls, messages) — pure Go driver, no CGo.
//     Multi-tenant: one SQLite database file per ref, stored in DATA_DIR
//     (default ./data) as <sanitized-ref>.db
//   - WebSocket hub replacing Supabase Realtime (presence, inbox, call
//     signaling, dashboard refresh broadcasts)
//   - REST API replacing Supabase PostgREST calls
//   - Cloudflare TURN credential proxy (/ice-config)
//   - Built-in username/password authentication and per-tenant user
//     management (see auth.go): /login, /users, invites, password resets
//
// Build:  go mod tidy && go build -o live-support .
// Run:    ./live-support          (reads .env from working dir, or env vars)
package main

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/subtle"
	"database/sql"
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"image"
	"image/jpeg"
	_ "image/png" // register PNG decoder
	"io"
	"io/fs"
	"log"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	_ "modernc.org/sqlite"
)

//go:embed static
var staticFS embed.FS

// version is set at build time via -ldflags "-X main.version=...".
// The updater (live-support-update) compares `live-support -version` against
// the latest release to decide whether to swap the binary.
var version = "dev"

var (
	dbs           *dbPool
	hub           *Hub
	cfTurnTokenID string
	cfAPIToken    string
)

// ───────────────────────── .env loading ─────────────────────────

func loadDotEnv() {
	data, err := os.ReadFile(".env")
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		eq := strings.Index(line, "=")
		if eq == -1 {
			continue
		}
		key := strings.TrimSpace(line[:eq])
		val := strings.TrimSpace(line[eq+1:])
		if os.Getenv(key) == "" {
			os.Setenv(key, val)
		}
	}
}

// ───────────────────────── SQLite ─────────────────────────

const schema = `
CREATE TABLE IF NOT EXISTS sessions (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  session_id TEXT UNIQUE NOT NULL,
  ref TEXT NOT NULL,
  email TEXT NOT NULL DEFAULT 'Unknown',
  name TEXT NOT NULL DEFAULT 'Unknown',
  role TEXT NOT NULL CHECK (role IN ('auth','guest')),
  status TEXT NOT NULL DEFAULT 'available',
  has_camera INTEGER NOT NULL DEFAULT 0,
  has_mic INTEGER NOT NULL DEFAULT 0,
  logged_in_at TEXT NOT NULL,
  last_seen_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS calls (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  call_id TEXT UNIQUE NOT NULL,
  ref TEXT NOT NULL,
  caller TEXT NOT NULL,
  caller_name TEXT NOT NULL DEFAULT 'Unknown',
  callee TEXT NOT NULL,
  callee_name TEXT NOT NULL DEFAULT 'Unknown',
  type TEXT NOT NULL,
  started_at TEXT NOT NULL,
  duration INTEGER,
  status TEXT NOT NULL DEFAULT 'ringing'
);
CREATE TABLE IF NOT EXISTS messages (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  message_id TEXT UNIQUE NOT NULL,
  ref TEXT NOT NULL,
  name TEXT NOT NULL DEFAULT 'Unknown',
  contact TEXT NOT NULL DEFAULT 'Unknown',
  message TEXT NOT NULL,
  created_at TEXT NOT NULL
);
-- Profile pictures for authenticated (non-guest) users. Kept in their own
-- table (not a column on sessions) so the blob never bloats the row reads
-- that touch a session on every list/heartbeat. One avatar per session_id;
-- the image is resized + re-encoded server-side before storage, so blobs
-- stay small (a few KB). updated_at doubles as a cheap ETag.
CREATE TABLE IF NOT EXISTS avatars (
  session_id TEXT PRIMARY KEY,
  ref TEXT NOT NULL,
  image_data BLOB NOT NULL,
  content_type TEXT NOT NULL DEFAULT 'image/jpeg',
  updated_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_avatars_ref ON avatars(ref);
CREATE INDEX IF NOT EXISTS idx_sessions_ref ON sessions(ref);
CREATE INDEX IF NOT EXISTS idx_calls_ref ON calls(ref);
CREATE INDEX IF NOT EXISTS idx_messages_ref ON messages(ref);
-- Built-in authentication (see auth.go). All timestamps are UTC unix seconds.
-- users/auth_sessions/invites/password_resets are per-tenant: each ref's DB
-- holds its own accounts, so logins are scoped to the tenant.
CREATE TABLE IF NOT EXISTS users (
  id             INTEGER PRIMARY KEY,
  username       TEXT    NOT NULL UNIQUE,
  password_hash  TEXT    NOT NULL DEFAULT '',
  role           TEXT    NOT NULL CHECK (role IN ('admin','agent')),
  must_change_pw INTEGER NOT NULL DEFAULT 0,
  active         INTEGER NOT NULL DEFAULT 1,
  created_at     INTEGER NOT NULL,
  last_login_at  INTEGER,
  reset_requested_at INTEGER
);
-- Server-side login sessions (distinct from the presence "sessions" table
-- above). id is sha256(raw cookie token), hex.
CREATE TABLE IF NOT EXISTS auth_sessions (
  id         TEXT    PRIMARY KEY,
  user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  csrf_token TEXT    NOT NULL,
  created_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL
);
-- Single-use invitation links minted by admins; redeeming one creates a user.
CREATE TABLE IF NOT EXISTS invites (
  id               INTEGER PRIMARY KEY,
  token_hash       TEXT    NOT NULL UNIQUE,
  role             TEXT    NOT NULL CHECK (role IN ('admin','agent')),
  username         TEXT,
  created_by       INTEGER REFERENCES users(id),  -- NULL for platform-minted invites
  created_at       INTEGER NOT NULL,
  expires_at       INTEGER NOT NULL,
  redeemed_at      INTEGER,
  redeemed_user_id INTEGER REFERENCES users(id)
);
-- One-time, admin-issued password reset tokens, scoped to an existing user.
CREATE TABLE IF NOT EXISTS password_resets (
  id          INTEGER PRIMARY KEY,
  token_hash  TEXT    NOT NULL UNIQUE,
  user_id     INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  created_by  INTEGER REFERENCES users(id) ON DELETE SET NULL,
  created_at  INTEGER NOT NULL,
  expires_at  INTEGER NOT NULL,
  used_at     INTEGER
);
CREATE INDEX IF NOT EXISTS idx_auth_sessions_expires ON auth_sessions(expires_at);
CREATE INDEX IF NOT EXISTS idx_password_resets_expires ON password_resets(expires_at);
-- Web Push subscriptions (Phase 2). One row per browser push endpoint, tied to
-- the authenticated user. session_id is the agent's *current* presence session
-- so the ring path can map a target session to a user's subscriptions; it is
-- refreshed on every (re)subscribe. Endpoint is the unique push-service URL.
CREATE TABLE IF NOT EXISTS push_subscriptions (
  id           INTEGER PRIMARY KEY AUTOINCREMENT,
  ref          TEXT    NOT NULL,
  user_id      INTEGER NOT NULL,
  session_id   TEXT    NOT NULL DEFAULT '',
  endpoint     TEXT    NOT NULL UNIQUE,
  p256dh       TEXT    NOT NULL,
  auth         TEXT    NOT NULL,
  created_at   TEXT    NOT NULL,
  last_seen_at TEXT    NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_push_session ON push_subscriptions(ref, session_id);
CREATE INDEX IF NOT EXISTS idx_push_user ON push_subscriptions(ref, user_id);
CREATE INDEX IF NOT EXISTS idx_password_resets_user ON password_resets(user_id);
CREATE TABLE IF NOT EXISTS agent_availability (
  user_id      INTEGER PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
  ref          TEXT    NOT NULL,
  available    INTEGER NOT NULL DEFAULT 0,
  session_id   TEXT    NOT NULL DEFAULT '',
  display_name TEXT    NOT NULL DEFAULT '',
  has_camera   INTEGER NOT NULL DEFAULT 0,
  picture      TEXT    NOT NULL DEFAULT '',
  online_since TEXT    NOT NULL DEFAULT '',
  updated_at   TEXT    NOT NULL DEFAULT ''
);
`

func openDB(path string) (*sql.DB, error) {
	d, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	d.SetMaxOpenConns(1) // SQLite: single writer per tenant DB
	if _, err := d.Exec(schema); err != nil {
		return nil, err
	}
	return d, nil
}

// ───────────────────────── per-ref DB pool ─────────────────────────
//
// Multi-tenancy: each ref gets its own SQLite database file under dataDir
// (<sanitized-ref>.db). Handles are opened lazily and cached for the life
// of the process. The ref column is kept in the schema so the existing
// frontend payloads and responses are unchanged.

var refSanitizer = regexp.MustCompile(`[^a-zA-Z0-9._-]`)

// safeRefFile converts a ref into a safe filename, or "" if invalid.
func safeRefFile(ref string) string {
	s := refSanitizer.ReplaceAllString(ref, "_")
	s = strings.Trim(s, ".") // no hidden files / ".." traversal
	if s == "" || len(s) > 200 {
		return ""
	}
	return s
}

type dbPool struct {
	mu      sync.Mutex
	dataDir string
	dbs     map[string]*sql.DB // sanitized ref -> handle
}

func newDBPool(dataDir string) (*dbPool, error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, err
	}
	return &dbPool{dataDir: dataDir, dbs: make(map[string]*sql.DB)}, nil
}

// get returns the DB for ref, opening (and creating) it if needed.
func (p *dbPool) get(ref string) (*sql.DB, error) {
	key := safeRefFile(ref)
	if key == "" {
		return nil, fmt.Errorf("invalid ref")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if d, ok := p.dbs[key]; ok {
		return d, nil
	}
	d, err := openDB(filepath.Join(p.dataDir, key+".db"))
	if err != nil {
		return nil, err
	}
	p.dbs[key] = d
	return d, nil
}

// exists reports whether a tenant DB is already present (cached or on disk)
// WITHOUT creating it. Lets public, ref-controlled endpoints (e.g. /api/online,
// which is CORS-* and called from arbitrary tenant sites) avoid spawning empty
// tenant DBs for unknown/guessed refs.
func (p *dbPool) exists(ref string) bool {
	key := safeRefFile(ref)
	if key == "" {
		return false
	}
	p.mu.Lock()
	_, cached := p.dbs[key]
	p.mu.Unlock()
	if cached {
		return true
	}
	_, err := os.Stat(filepath.Join(p.dataDir, key+".db"))
	return err == nil
}

// all returns handles for every tenant DB on disk (opening any not yet
// cached). Used to resolve rows by globally-unique ID when the request
// doesn't carry a ref (PATCH ?session_id=.., DELETE /api/<table>/<id>).
func (p *dbPool) all() []*sql.DB {
	entries, _ := os.ReadDir(p.dataDir)
	out := []*sql.DB{}
	seen := map[string]bool{}
	p.mu.Lock()
	cached := make(map[string]*sql.DB, len(p.dbs))
	for k, d := range p.dbs {
		cached[k] = d
	}
	p.mu.Unlock()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".db") {
			continue
		}
		key := strings.TrimSuffix(name, ".db")
		if d, ok := cached[key]; ok {
			out = append(out, d)
			seen[key] = true
			continue
		}
		p.mu.Lock()
		d, ok := p.dbs[key]
		if !ok {
			var err error
			d, err = openDB(filepath.Join(p.dataDir, name))
			if err != nil {
				p.mu.Unlock()
				continue
			}
			p.dbs[key] = d
		}
		p.mu.Unlock()
		out = append(out, d)
		seen[key] = true
	}
	for k, d := range cached {
		if !seen[k] {
			out = append(out, d)
		}
	}
	return out
}

// findByColumn locates the tenant DB containing a row where col = val in
// table, returning the DB and the row's ref. Returns nil if not found.
func (p *dbPool) findByColumn(table, col, val string) (*sql.DB, string) {
	for _, d := range p.all() {
		var ref string
		err := d.QueryRow("SELECT ref FROM "+table+" WHERE "+col+" = ?", val).Scan(&ref)
		if err == nil {
			return d, ref
		}
	}
	return nil, ""
}

func nowISO() string { return time.Now().UTC().Format(time.RFC3339) }

// cssColorRe matches the CSS color forms we accept from PRIMARY_COLOR:
//   - hex: #rgb, #rgba, #rrggbb, #rrggbbaa
//   - functional: rgb()/rgba()/hsl()/hsla() with digits, %, ., spaces, commas
//   - a plain keyword (e.g. "rebeccapurple") — letters only
//
// This is a safety allowlist: the value is set via style.setProperty on the
// client (already injection-safe), but validating here keeps anything weird
// out of the config payload entirely.
var cssColorRe = regexp.MustCompile(`^(#[0-9a-fA-F]{3,8}|(rgb|rgba|hsl|hsla)\([0-9.,%\s/]+\)|[a-zA-Z]+)$`)

// validCSSColor trims and validates a color string, returning "" (meaning
// "use the stylesheet default") if it is empty or not an accepted form.
func validCSSColor(s string) string {
	s = strings.TrimSpace(s)
	if s == "" || len(s) > 64 {
		return ""
	}
	if !cssColorRe.MatchString(s) {
		return ""
	}
	return s
}

// validURLOrPath trims and validates a value meant to be used as an asset URL
// (e.g. a favicon). It accepts an absolute http(s):// URL or a same-origin
// absolute path ("/...", but not protocol-relative "//host"). Returns "" for
// anything else — including javascript:/data: and relative paths — so callers
// fall back to a safe default rather than emitting an attacker-influenced URL.
func validURLOrPath(s string) string {
	s = strings.TrimSpace(s)
	if s == "" || len(s) > 2048 {
		return ""
	}
	if strings.HasPrefix(s, "/") && !strings.HasPrefix(s, "//") {
		return s // same-origin absolute path
	}
	u, err := url.Parse(s)
	if err != nil {
		return ""
	}
	if (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" {
		return s
	}
	return ""
}

// ───────────────────────── WebSocket hub ─────────────────────────
//
// Protocol (JSON messages):
//   client → server:
//     {"action":"subscribe","channel":"inbox:<id>"}
//     {"action":"unsubscribe","channel":"..."}
//     {"action":"broadcast","channel":"...","event":"message|signal|refresh","payload":{...}}
//     {"action":"track","channel":"presence:<ref>","key":"<session_id>","state":{...}}
//     {"action":"untrack","channel":"presence:<ref>","key":"<session_id>"}
//   server → client:
//     {"type":"broadcast","channel":"...","event":"...","payload":{...}}
//     {"type":"presence","channel":"presence:<ref>","users":[{...},...]}
//     {"type":"ack","action":"...","channel":"..."}

type ClientMsg struct {
	Action  string                 `json:"action"`
	Channel string                 `json:"channel"`
	Event   string                 `json:"event,omitempty"`
	Payload json.RawMessage        `json:"payload,omitempty"`
	Key     string                 `json:"key,omitempty"`
	State   map[string]interface{} `json:"state,omitempty"`
}

type ServerMsg struct {
	Type    string                   `json:"type"`
	Channel string                   `json:"channel,omitempty"`
	Event   string                   `json:"event,omitempty"`
	Payload json.RawMessage          `json:"payload,omitempty"`
	Users   []map[string]interface{} `json:"users,omitempty"`
	Action  string                   `json:"action,omitempty"`
}

type Conn struct {
	ws   *websocket.Conn
	send chan ServerMsg
	subs map[string]bool // channels this conn subscribed to
	// presence entries owned by this conn: channel -> key
	tracked map[string]string
	mu      sync.Mutex
}

type Hub struct {
	mu sync.Mutex
	// channel name -> set of conns
	channels map[string]map[*Conn]bool
	// presence channel -> key -> state
	presence map[string]map[string]map[string]interface{}
}

func newHub() *Hub {
	return &Hub{
		channels: make(map[string]map[*Conn]bool),
		presence: make(map[string]map[string]map[string]interface{}),
	}
}

func (h *Hub) subscribe(c *Conn, channel string) {
	h.mu.Lock()
	if h.channels[channel] == nil {
		h.channels[channel] = make(map[*Conn]bool)
	}
	h.channels[channel][c] = true
	h.mu.Unlock()

	c.mu.Lock()
	c.subs[channel] = true
	c.mu.Unlock()

	// If it's a presence channel, immediately send current state to this conn.
	if strings.HasPrefix(channel, "presence:") {
		h.sendPresenceTo(c, channel)
	}
}

func (h *Hub) unsubscribe(c *Conn, channel string) {
	h.mu.Lock()
	if conns := h.channels[channel]; conns != nil {
		delete(conns, c)
		if len(conns) == 0 {
			delete(h.channels, channel)
		}
	}
	h.mu.Unlock()

	c.mu.Lock()
	delete(c.subs, channel)
	key, hadTrack := c.tracked[channel]
	delete(c.tracked, channel)
	c.mu.Unlock()

	if hadTrack {
		h.untrack(channel, key)
	}
}

func (h *Hub) broadcast(channel, event string, payload json.RawMessage) {
	msg := ServerMsg{Type: "broadcast", Channel: channel, Event: event, Payload: payload}
	h.mu.Lock()
	conns := make([]*Conn, 0, len(h.channels[channel]))
	for c := range h.channels[channel] {
		conns = append(conns, c)
	}
	h.mu.Unlock()
	for _, c := range conns {
		select {
		case c.send <- msg:
		default: // slow client; drop message rather than block the hub
		}
	}
}

func (h *Hub) track(c *Conn, channel, key string, state map[string]interface{}) {
	h.mu.Lock()
	if h.presence[channel] == nil {
		h.presence[channel] = make(map[string]map[string]interface{})
	}
	h.presence[channel][key] = state
	h.mu.Unlock()

	c.mu.Lock()
	c.tracked[channel] = key
	c.mu.Unlock()

	h.syncPresence(channel)
}

func (h *Hub) untrack(channel, key string) {
	h.mu.Lock()
	if m := h.presence[channel]; m != nil {
		delete(m, key)
		if len(m) == 0 {
			delete(h.presence, channel)
		}
	}
	h.mu.Unlock()
	h.syncPresence(channel)
}

func (h *Hub) presenceUsers(channel string) []map[string]interface{} {
	h.mu.Lock()
	defer h.mu.Unlock()
	users := make([]map[string]interface{}, 0, len(h.presence[channel]))
	for _, state := range h.presence[channel] {
		users = append(users, state)
	}
	return users
}

// syncPresence pushes the full presence state to all subscribers of channel.
func (h *Hub) syncPresence(channel string) {
	users := h.presenceUsers(channel)
	msg := ServerMsg{Type: "presence", Channel: channel, Users: users}
	h.mu.Lock()
	conns := make([]*Conn, 0, len(h.channels[channel]))
	for c := range h.channels[channel] {
		conns = append(conns, c)
	}
	h.mu.Unlock()
	for _, c := range conns {
		select {
		case c.send <- msg:
		default:
		}
	}
}

func (h *Hub) sendPresenceTo(c *Conn, channel string) {
	users := h.presenceUsers(channel)
	select {
	case c.send <- ServerMsg{Type: "presence", Channel: channel, Users: users}:
	default:
	}
}

func (h *Hub) dropConn(c *Conn) {
	c.mu.Lock()
	subs := make([]string, 0, len(c.subs))
	for ch := range c.subs {
		subs = append(subs, ch)
	}
	c.mu.Unlock()
	for _, ch := range subs {
		h.unsubscribe(c, ch)
	}
}

func wsHandler(w http.ResponseWriter, r *http.Request) {
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// Same-origin app; allow any origin so it also works behind proxies.
		OriginPatterns: []string{"*"},
	})
	if err != nil {
		return
	}
	c := &Conn{
		ws:      ws,
		send:    make(chan ServerMsg, 64),
		subs:    make(map[string]bool),
		tracked: make(map[string]string),
	}

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	// Writer goroutine
	go func() {
		for {
			select {
			case msg, ok := <-c.send:
				if !ok {
					return
				}
				wctx, wcancel := context.WithTimeout(ctx, 5*time.Second)
				err := wsjson.Write(wctx, ws, msg)
				wcancel()
				if err != nil {
					cancel()
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}()

	defer func() {
		hub.dropConn(c)
		ws.Close(websocket.StatusNormalClosure, "")
	}()

	for {
		var msg ClientMsg
		if err := wsjson.Read(ctx, ws, &msg); err != nil {
			return
		}
		switch msg.Action {
		case "subscribe":
			hub.subscribe(c, msg.Channel)
			select {
			case c.send <- ServerMsg{Type: "ack", Action: "subscribe", Channel: msg.Channel}:
			default:
			}
		case "unsubscribe":
			hub.unsubscribe(c, msg.Channel)
		case "broadcast":
			hub.broadcast(msg.Channel, msg.Event, msg.Payload)
		case "track":
			hub.track(c, msg.Channel, msg.Key, msg.State)
		case "untrack":
			c.mu.Lock()
			key := c.tracked[msg.Channel]
			delete(c.tracked, msg.Channel)
			c.mu.Unlock()
			if msg.Key != "" {
				key = msg.Key
			}
			if key != "" {
				hub.untrack(msg.Channel, key)
			}
		case "ping":
			select {
			case c.send <- ServerMsg{Type: "pong"}:
			default:
			}
		}
	}
}

// ───────────────────────── REST helpers ─────────────────────────

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func errJSON(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func notifyRefresh(ref, table string) {
	if ref == "" {
		return
	}
	payload, _ := json.Marshal(map[string]string{"table": table})
	hub.broadcast("dashboard:"+ref, "refresh", payload)
}

// rowsToMaps converts sql rows into []map for JSON output.
func rowsToMaps(rows *sql.Rows) ([]map[string]interface{}, error) {
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	out := []map[string]interface{}{}
	for rows.Next() {
		vals := make([]interface{}, len(cols))
		ptrs := make([]interface{}, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		m := map[string]interface{}{}
		for i, col := range cols {
			v := vals[i]
			if b, ok := v.([]byte); ok {
				v = string(b)
			}
			// has_camera / has_mic stored as 0/1 — surface as booleans
			if col == "has_camera" || col == "has_mic" {
				switch n := v.(type) {
				case int64:
					v = n != 0
				}
			}
			m[col] = v
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// listHandler builds paginated list endpoints: ?ref=..&page=0&pageSize=5
// plus optional role / notRole filters for sessions.
func listHandler(table, orderCol string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ref := r.URL.Query().Get("ref")
		if ref == "" {
			errJSON(w, 400, "ref required")
			return
		}
		db, err := dbs.get(ref)
		if err != nil {
			errJSON(w, 400, err.Error())
			return
		}
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		pageSize, _ := strconv.Atoi(r.URL.Query().Get("pageSize"))
		if pageSize <= 0 || pageSize > 100 {
			pageSize = 5
		}
		where := "ref = ?"
		args := []interface{}{ref}
		if role := r.URL.Query().Get("role"); role != "" && table == "sessions" {
			where += " AND role = ?"
			args = append(args, role)
		}
		if notRole := r.URL.Query().Get("notRole"); notRole != "" && table == "sessions" {
			where += " AND role != ?"
			args = append(args, notRole)
		}

		var count int
		if err := db.QueryRow("SELECT COUNT(*) FROM "+table+" WHERE "+where, args...).Scan(&count); err != nil {
			errJSON(w, 500, err.Error())
			return
		}
		query := fmt.Sprintf("SELECT * FROM %s WHERE %s ORDER BY %s DESC LIMIT ? OFFSET ?", table, where, orderCol)
		args = append(args, pageSize, page*pageSize)
		rows, err := db.Query(query, args...)
		if err != nil {
			errJSON(w, 500, err.Error())
			return
		}
		defer rows.Close()
		data, err := rowsToMaps(rows)
		if err != nil {
			errJSON(w, 500, err.Error())
			return
		}
		writeJSON(w, 200, map[string]interface{}{"data": data, "count": count})
	}
}

// deleteByID handles DELETE /api/<table>/<id> and bulk DELETE /api/<table>?ref=..
func deleteHandler(table string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			errJSON(w, 405, "method not allowed")
			return
		}
		// /api/<table>/<id>  → delete one row
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		if len(parts) == 3 { // api, table, id
			id := parts[2]
			// Resolve the tenant DB: use ?ref= if provided, else search.
			var tdb *sql.DB
			var ref string
			if q := r.URL.Query().Get("ref"); q != "" {
				d, err := dbs.get(q)
				if err != nil {
					errJSON(w, 400, err.Error())
					return
				}
				tdb, ref = d, q
			} else {
				tdb, ref = dbs.findByColumn(table, "id", id)
			}
			if tdb == nil {
				writeJSON(w, 200, map[string]bool{"ok": true}) // nothing to delete
				return
			}
			if _, err := tdb.Exec("DELETE FROM "+table+" WHERE id = ?", id); err != nil {
				errJSON(w, 500, err.Error())
				return
			}
			notifyRefresh(ref, table)
			writeJSON(w, 200, map[string]bool{"ok": true})
			return
		}
		// bulk: /api/<table>?ref=...[&notRole=auth]
		ref := r.URL.Query().Get("ref")
		if ref == "" {
			errJSON(w, 400, "ref required")
			return
		}
		db, err := dbs.get(ref)
		if err != nil {
			errJSON(w, 400, err.Error())
			return
		}
		where := "ref = ?"
		args := []interface{}{ref}
		if role := r.URL.Query().Get("role"); role != "" && table == "sessions" {
			where += " AND role = ?"
			args = append(args, role)
		}
		if notRole := r.URL.Query().Get("notRole"); notRole != "" && table == "sessions" {
			where += " AND role != ?"
			args = append(args, notRole)
		}
		if _, err := db.Exec("DELETE FROM "+table+" WHERE "+where, args...); err != nil {
			errJSON(w, 500, err.Error())
			return
		}
		notifyRefresh(ref, table)
		writeJSON(w, 200, map[string]bool{"ok": true})
	}
}

// ───────────────────────── REST: sessions ─────────────────────────

type sessionUpsert struct {
	SessionID string `json:"session_id"`
	Ref       string `json:"ref"`
	Email     string `json:"email"`
	Name      string `json:"name"`
	Role      string `json:"role"`
	Status    string `json:"status"`
	HasCamera bool   `json:"has_camera"`
	HasMic    bool   `json:"has_mic"`
}

func sessionsHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		listHandler("sessions", "logged_in_at")(w, r)
	case http.MethodPost: // upsert
		var s sessionUpsert
		if err := json.NewDecoder(r.Body).Decode(&s); err != nil {
			errJSON(w, 400, "bad json")
			return
		}
		if s.SessionID == "" || s.Ref == "" {
			errJSON(w, 400, "session_id and ref required")
			return
		}
		if s.Status == "" {
			s.Status = "available"
		}
		db, err := dbs.get(s.Ref)
		if err != nil {
			errJSON(w, 400, err.Error())
			return
		}
		now := nowISO()
		_, err = db.Exec(`
			INSERT INTO sessions (session_id, ref, email, name, role, status, has_camera, has_mic, logged_in_at, last_seen_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(session_id) DO UPDATE SET
			  ref=excluded.ref, email=excluded.email, name=excluded.name, role=excluded.role,
			  status=excluded.status, has_camera=excluded.has_camera, has_mic=excluded.has_mic,
			  logged_in_at=excluded.logged_in_at, last_seen_at=excluded.last_seen_at`,
			s.SessionID, s.Ref, s.Email, s.Name, s.Role, s.Status,
			boolInt(s.HasCamera), boolInt(s.HasMic), now, now)
		if err != nil {
			errJSON(w, 500, err.Error())
			return
		}
		notifyRefresh(s.Ref, "sessions")
		writeJSON(w, 200, map[string]bool{"ok": true})
	case http.MethodPatch: // update by session_id: ?session_id=...
		sessionID := r.URL.Query().Get("session_id")
		if sessionID == "" {
			errJSON(w, 400, "session_id required")
			return
		}
		var body map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			errJSON(w, 400, "bad json")
			return
		}
		// Resolve tenant DB: prefer ?ref=, else search by session_id.
		var db *sql.DB
		var sessRef string
		if q := r.URL.Query().Get("ref"); q != "" {
			d, err := dbs.get(q)
			if err != nil {
				errJSON(w, 400, err.Error())
				return
			}
			db, sessRef = d, q
		} else {
			db, sessRef = dbs.findByColumn("sessions", "session_id", sessionID)
		}
		if db == nil {
			writeJSON(w, 200, map[string]bool{"ok": true}) // unknown session; nothing to update
			return
		}
		allowed := map[string]bool{"status": true, "has_camera": true, "has_mic": true, "last_seen_at": true}
		sets := []string{}
		args := []interface{}{}
		for k, v := range body {
			if !allowed[k] {
				continue
			}
			if k == "has_camera" || k == "has_mic" {
				if b, ok := v.(bool); ok {
					v = boolInt(b)
				}
			}
			sets = append(sets, k+" = ?")
			args = append(args, v)
		}
		// Always bump last_seen_at unless caller provided it
		if _, ok := body["last_seen_at"]; !ok {
			sets = append(sets, "last_seen_at = ?")
			args = append(args, nowISO())
		}
		if len(sets) == 0 {
			errJSON(w, 400, "nothing to update")
			return
		}
		args = append(args, sessionID)
		if _, err := db.Exec("UPDATE sessions SET "+strings.Join(sets, ", ")+" WHERE session_id = ?", args...); err != nil {
			errJSON(w, 500, err.Error())
			return
		}
		// Only notify the dashboard for meaningful changes (not heartbeats).
		if _, statusChanged := body["status"]; statusChanged {
			notifyRefresh(sessRef, "sessions")
		}
		writeJSON(w, 200, map[string]bool{"ok": true})
	case http.MethodDelete:
		// Best-effort: remove any avatars belonging to the session(s) being
		// deleted so blobs don't outlive their session row.
		cleanupAvatarsForDelete(r)
		deleteHandler("sessions")(w, r)
	default:
		errJSON(w, 405, "method not allowed")
	}
}

// cleanupAvatarsForDelete mirrors the targeting logic of deleteHandler for the
// sessions table (single id, or bulk by ref[+role/notRole]) and removes the
// matching avatar rows first. Failures are non-fatal — a stray avatar blob is
// harmless and will be overwritten if the session id is ever reused.
func cleanupAvatarsForDelete(r *http.Request) {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	// /api/sessions/<id> → delete the avatar for that one session.
	if len(parts) == 3 {
		id := parts[2]
		var tdb *sql.DB
		if q := r.URL.Query().Get("ref"); q != "" {
			tdb, _ = dbs.get(q)
		} else {
			tdb, _ = dbs.findByColumn("sessions", "id", id)
		}
		if tdb == nil {
			return
		}
		var sessionID string
		if err := tdb.QueryRow("SELECT session_id FROM sessions WHERE id = ?", id).Scan(&sessionID); err == nil {
			tdb.Exec("DELETE FROM avatars WHERE session_id = ?", sessionID)
		}
		return
	}
	// Bulk: /api/sessions?ref=...[&role=|&notRole=] → delete avatars for every
	// session row that the bulk delete will remove (joined on session_id).
	ref := r.URL.Query().Get("ref")
	if ref == "" {
		return
	}
	db, err := dbs.get(ref)
	if err != nil {
		return
	}
	where := "ref = ?"
	args := []interface{}{ref}
	if role := r.URL.Query().Get("role"); role != "" {
		where += " AND role = ?"
		args = append(args, role)
	}
	if notRole := r.URL.Query().Get("notRole"); notRole != "" {
		where += " AND role != ?"
		args = append(args, notRole)
	}
	db.Exec("DELETE FROM avatars WHERE session_id IN (SELECT session_id FROM sessions WHERE "+where+")", args...)
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// ───────────────────────── REST: calls ─────────────────────────

func callsHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		listHandler("calls", "started_at")(w, r)
	case http.MethodPost:
		var c struct {
			CallID     string `json:"call_id"`
			Ref        string `json:"ref"`
			Caller     string `json:"caller"`
			CallerName string `json:"caller_name"`
			Callee     string `json:"callee"`
			CalleeName string `json:"callee_name"`
			Type       string `json:"type"`
		}
		if err := json.NewDecoder(r.Body).Decode(&c); err != nil {
			errJSON(w, 400, "bad json")
			return
		}
		if c.CallID == "" || c.Ref == "" {
			errJSON(w, 400, "call_id and ref required")
			return
		}
		db, err := dbs.get(c.Ref)
		if err != nil {
			errJSON(w, 400, err.Error())
			return
		}
		_, err = db.Exec(`
			INSERT INTO calls (call_id, ref, caller, caller_name, callee, callee_name, type, started_at, status)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'ringing')`,
			c.CallID, c.Ref, c.Caller, c.CallerName, c.Callee, c.CalleeName, c.Type, nowISO())
		if err != nil {
			errJSON(w, 500, err.Error())
			return
		}
		notifyRefresh(c.Ref, "calls")
		writeJSON(w, 200, map[string]bool{"ok": true})
	case http.MethodPatch: // ?call_id=...
		callID := r.URL.Query().Get("call_id")
		if callID == "" {
			errJSON(w, 400, "call_id required")
			return
		}
		var body map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			errJSON(w, 400, "bad json")
			return
		}
		// Resolve tenant DB: prefer ?ref=, else search by call_id.
		var db *sql.DB
		var callRef string
		if q := r.URL.Query().Get("ref"); q != "" {
			d, err := dbs.get(q)
			if err != nil {
				errJSON(w, 400, err.Error())
				return
			}
			db, callRef = d, q
		} else {
			db, callRef = dbs.findByColumn("calls", "call_id", callID)
		}
		if db == nil {
			writeJSON(w, 200, map[string]bool{"ok": true}) // unknown call; nothing to update
			return
		}
		allowed := map[string]bool{"status": true, "duration": true, "started_at": true}
		sets := []string{}
		args := []interface{}{}
		for k, v := range body {
			if !allowed[k] {
				continue
			}
			sets = append(sets, k+" = ?")
			args = append(args, v)
		}
		if len(sets) == 0 {
			errJSON(w, 400, "nothing to update")
			return
		}
		args = append(args, callID)
		if _, err := db.Exec("UPDATE calls SET "+strings.Join(sets, ", ")+" WHERE call_id = ?", args...); err != nil {
			errJSON(w, 500, err.Error())
			return
		}
		notifyRefresh(callRef, "calls")
		writeJSON(w, 200, map[string]bool{"ok": true})
	case http.MethodDelete:
		deleteHandler("calls")(w, r)
	default:
		errJSON(w, 405, "method not allowed")
	}
}

// ───────────────────────── REST: messages ─────────────────────────

func messagesHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		listHandler("messages", "created_at")(w, r)
	case http.MethodPost:
		var m struct {
			MessageID string `json:"message_id"`
			Ref       string `json:"ref"`
			Name      string `json:"name"`
			Contact   string `json:"contact"`
			Message   string `json:"message"`
		}
		if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
			errJSON(w, 400, "bad json")
			return
		}
		if m.MessageID == "" || m.Ref == "" || m.Message == "" {
			errJSON(w, 400, "message_id, ref and message required")
			return
		}
		db, err := dbs.get(m.Ref)
		if err != nil {
			errJSON(w, 400, err.Error())
			return
		}
		_, err = db.Exec(`
			INSERT INTO messages (message_id, ref, name, contact, message, created_at)
			VALUES (?, ?, ?, ?, ?, ?)`,
			m.MessageID, m.Ref, m.Name, m.Contact, m.Message, nowISO())
		if err != nil {
			errJSON(w, 500, err.Error())
			return
		}
		notifyRefresh(m.Ref, "messages")
		writeJSON(w, 200, map[string]bool{"ok": true})
	case http.MethodDelete:
		deleteHandler("messages")(w, r)
	default:
		errJSON(w, 405, "method not allowed")
	}
}

// ───────────────────────── REST: avatars ─────────────────────────
//
// Profile pictures for authenticated (non-guest) users.
//
//   POST   /api/avatar?ref=<ref>&session_id=<id>   (multipart, field "avatar")
//          Accepts a JPEG or PNG, decodes it, downscales to fit avatarMaxPx,
//          re-encodes as JPEG, and stores the small blob. Restricted to a
//          session whose role is 'auth' — guests cannot upload.
//   GET    /api/avatar?ref=<ref>&session_id=<id>
//          Streams the stored image with caching headers. 404 if none.
//   DELETE /api/avatar?ref=<ref>&session_id=<id>
//          Removes the avatar (auth sessions only).
//
// Avatars live in their own per-ref table and are cascade-deleted when the
// owning session row is removed (see cleanupAvatarsForDelete).

const (
	avatarMaxPx       = 256     // longest edge after downscale
	avatarMaxUpload   = 8 << 20 // 8 MiB cap on the raw upload
	avatarJPEGQuality = 82      // re-encode quality
)

// resizeNearest downscales src so its longest edge is at most maxEdge,
// preserving aspect ratio, using nearest-neighbor sampling. Returns src
// unchanged if it already fits. Dependency-free (stdlib image only) — quality
// is fine for small avatar thumbnails. Never upscales.
func resizeNearest(src image.Image, maxEdge int) image.Image {
	b := src.Bounds()
	sw, sh := b.Dx(), b.Dy()
	if sw <= maxEdge && sh <= maxEdge {
		return src
	}
	dw, dh := sw, sh
	if sw >= sh {
		dw = maxEdge
		dh = int(float64(sh) * float64(maxEdge) / float64(sw))
	} else {
		dh = maxEdge
		dw = int(float64(sw) * float64(maxEdge) / float64(sh))
	}
	if dw < 1 {
		dw = 1
	}
	if dh < 1 {
		dh = 1
	}
	dst := image.NewRGBA(image.Rect(0, 0, dw, dh))
	for y := 0; y < dh; y++ {
		sy := b.Min.Y + y*sh/dh
		for x := 0; x < dw; x++ {
			sx := b.Min.X + x*sw/dw
			dst.Set(x, y, src.At(sx, sy))
		}
	}
	return dst
}

// sessionRole returns the role for a session_id in the given tenant DB, or ""
// if the session does not exist.
func sessionRole(db *sql.DB, sessionID string) string {
	var role string
	if err := db.QueryRow("SELECT role FROM sessions WHERE session_id = ?", sessionID).Scan(&role); err != nil {
		return ""
	}
	return role
}

func avatarHandler(w http.ResponseWriter, r *http.Request) {
	ref := r.URL.Query().Get("ref")
	sessionID := r.URL.Query().Get("session_id")
	if ref == "" || sessionID == "" {
		errJSON(w, 400, "ref and session_id required")
		return
	}
	db, err := dbs.get(ref)
	if err != nil {
		errJSON(w, 400, err.Error())
		return
	}

	switch r.Method {
	case http.MethodGet:
		var data []byte
		var ctype, updatedAt string
		err := db.QueryRow(
			"SELECT image_data, content_type, updated_at FROM avatars WHERE session_id = ?",
			sessionID).Scan(&data, &ctype, &updatedAt)
		if err != nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		// Weak ETag derived from the stored bytes; lets the browser skip the
		// blob on repeat loads (the roster re-renders frequently).
		sum := sha1.Sum(data)
		etag := fmt.Sprintf(`"%x"`, sum[:8])
		if match := r.Header.Get("If-None-Match"); match == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("Content-Type", ctype)
		w.Header().Set("ETag", etag)
		w.Header().Set("Cache-Control", "private, max-age=60")
		w.Write(data)

	case http.MethodPost:
		// Only authenticated (non-guest) users may have a profile picture.
		if sessionRole(db, sessionID) != "auth" {
			errJSON(w, 403, "profile pictures are only available to authenticated users")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, avatarMaxUpload)
		if err := r.ParseMultipartForm(avatarMaxUpload); err != nil {
			errJSON(w, 400, "upload too large or malformed")
			return
		}
		file, _, err := r.FormFile("avatar")
		if err != nil {
			errJSON(w, 400, "missing 'avatar' file field")
			return
		}
		defer file.Close()
		raw, err := io.ReadAll(file)
		if err != nil {
			errJSON(w, 400, "could not read upload")
			return
		}
		// image.Decode sniffs the format from the registered decoders (JPEG +
		// PNG). Anything else — including non-image uploads — fails here, so we
		// never store attacker-controlled bytes verbatim.
		img, _, err := image.Decode(bytes.NewReader(raw))
		if err != nil {
			errJSON(w, 400, "file is not a valid JPEG or PNG image")
			return
		}
		img = resizeNearest(img, avatarMaxPx)
		var buf bytes.Buffer
		if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: avatarJPEGQuality}); err != nil {
			errJSON(w, 500, "could not encode image")
			return
		}
		now := nowISO()
		_, err = db.Exec(`
			INSERT INTO avatars (session_id, ref, image_data, content_type, updated_at)
			VALUES (?, ?, ?, 'image/jpeg', ?)
			ON CONFLICT(session_id) DO UPDATE SET
			  ref=excluded.ref, image_data=excluded.image_data,
			  content_type=excluded.content_type, updated_at=excluded.updated_at`,
			sessionID, ref, buf.Bytes(), now)
		if err != nil {
			errJSON(w, 500, err.Error())
			return
		}
		// URL the client can hand around in presence. The updated_at query
		// param busts the browser cache when the picture changes.
		avatarURL := fmt.Sprintf("/api/avatar?ref=%s&session_id=%s&v=%s",
			url.QueryEscape(ref), url.QueryEscape(sessionID), url.QueryEscape(now))
		writeJSON(w, 200, map[string]interface{}{"ok": true, "url": avatarURL})

	case http.MethodDelete:
		if sessionRole(db, sessionID) != "auth" {
			errJSON(w, 403, "not allowed")
			return
		}
		if _, err := db.Exec("DELETE FROM avatars WHERE session_id = ?", sessionID); err != nil {
			errJSON(w, 500, err.Error())
			return
		}
		writeJSON(w, 200, map[string]bool{"ok": true})

	default:
		errJSON(w, 405, "method not allowed")
	}
}

// ───────────────────────── /api/online ─────────────────────────

func onlineHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	cutoff := time.Now().UTC().Add(-60 * time.Second).Format(time.RFC3339)
	ref := r.URL.Query().Get("ref")

	// Pick tenant DBs: just the ref's DB, or all of them if no ref given.
	var targets []*sql.DB
	if ref != "" {
		// Never auto-create from this public CORS-* endpoint: an unknown ref
		// means "nobody online", not "provision a new tenant".
		if !dbs.exists(ref) {
			writeJSON(w, 200, map[string]interface{}{
				"online": false, "count": 0, "withCamera": 0, "withMic": 0,
			})
			return
		}
		d, err := dbs.get(ref)
		if err != nil {
			errJSON(w, 400, err.Error())
			return
		}
		targets = []*sql.DB{d}
	} else {
		targets = dbs.all()
	}

	count, withCamera, withMic := 0, 0, 0
	for _, d := range targets {
		rows, err := d.Query(
			"SELECT has_camera, has_mic FROM sessions WHERE role = 'auth' AND status != 'offline' AND last_seen_at > ?",
			cutoff)
		if err != nil {
			continue
		}
		for rows.Next() {
			var cam, mic int
			if err := rows.Scan(&cam, &mic); err != nil {
				continue
			}
			count++
			if cam != 0 {
				withCamera++
			}
			if mic != 0 {
				withMic++
			}
		}
		rows.Close()
	}
	writeJSON(w, 200, map[string]interface{}{
		"online": count > 0, "count": count, "withCamera": withCamera, "withMic": withMic,
	})
}

// ───────────────────────── /ice-config ─────────────────────────

func iceConfigHandler(w http.ResponseWriter, r *http.Request) {
	fallback := map[string]interface{}{
		"iceServers": []map[string]interface{}{
			{"urls": "stun:stun.cloudflare.com:3478"},
			{"urls": "stun:stun.l.google.com:19302"},
		},
	}
	if cfTurnTokenID == "" || cfAPIToken == "" {
		writeJSON(w, 200, fallback)
		return
	}
	body, _ := json.Marshal(map[string]int{"ttl": 86400})
	req, err := http.NewRequest("POST",
		"https://rtc.live.cloudflare.com/v1/turn/keys/"+cfTurnTokenID+"/credentials/generate-ice-servers",
		bytes.NewReader(body))
	if err != nil {
		writeJSON(w, 200, fallback)
		return
	}
	req.Header.Set("Authorization", "Bearer "+cfAPIToken)
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		log.Println("[ICE] Cloudflare error:", err)
		writeJSON(w, 200, fallback)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 && resp.StatusCode != 201 {
		log.Println("[ICE] Cloudflare status:", resp.StatusCode)
		writeJSON(w, 200, fallback)
		return
	}
	var out interface{}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		writeJSON(w, 200, fallback)
		return
	}
	writeJSON(w, 200, out)
}

// ───────────────────────── main ─────────────────────────

func main() {
	showVersion := flag.Bool("version", false, "print version and exit")
	genVAPID := flag.Bool("genvapid", false, "generate a Web Push VAPID keypair and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return
	}
	if *genVAPID {
		if err := printVAPIDKeys(); err != nil {
			log.Fatal("genvapid:", err)
		}
		return
	}

	loadDotEnv()

	cfTurnTokenID = os.Getenv("CLOUDFLARE_TURN_TOKEN_ID")
	cfAPIToken = os.Getenv("CLOUDFLARE_API_TOKEN")
	port := os.Getenv("PORT")
	if port == "" {
		port = "8000"
	}
	// Bind loopback by default so the app is only reachable through the
	// reverse proxy (Caddy/nginx). Set BIND_ADDR=0.0.0.0 for direct LAN access.
	bindAddr := os.Getenv("BIND_ADDR")
	if bindAddr == "" {
		bindAddr = "127.0.0.1"
	}
	dataDir := os.Getenv("DATA_DIR")
	if dataDir == "" {
		dataDir = "data"
	}

	var err error
	dbs, err = newDBPool(dataDir)
	if err != nil {
		log.Fatal("init db pool:", err)
	}
	hub = newHub()

	mux := http.NewServeMux()

	// Liveness probe for the installer smoke test and uptime checks.
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok\n"))
	})

	// WebSocket
	mux.HandleFunc("/ws", wsHandler)

	// REST
	mux.HandleFunc("/api/sessions", sessionsHandler)
	mux.HandleFunc("/api/sessions/", sessionsHandler)
	mux.HandleFunc("/api/calls", callsHandler)
	mux.HandleFunc("/api/calls/", callsHandler)
	mux.HandleFunc("/api/messages", messagesHandler)
	mux.HandleFunc("/api/messages/", messagesHandler)
	mux.HandleFunc("/api/avatar", avatarHandler)
	mux.HandleFunc("/api/online", onlineHandler)
	mux.HandleFunc("/ice-config", iceConfigHandler)

	// Dev-mode flag for the frontend. When DEV_MODE=true the auth page may
	// bypass JWT validation (local testing only — never set in production).
	devMode := os.Getenv("DEV_MODE") == "true"
	mux.HandleFunc("/api/dev", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]bool{"devMode": devMode})
	})
	if devMode {
		log.Println("[Server] DEV_MODE is ON — auth.html JWT validation can be bypassed with ?dev=true")
	}

	// Optional brand color. PRIMARY_COLOR overrides the CSS --primary variable
	// at runtime (the frontend applies it on load). Validated here so an
	// invalid value falls back to the CSS default rather than injecting junk
	// into the page; empty string means "use the stylesheet default".
	primaryColor := validCSSColor(os.Getenv("PRIMARY_COLOR"))
	// Optional favicon override. FAVICON_URL replaces the embedded default
	// favicon at runtime (the frontend swaps the <link rel="icon"> on load).
	// Validated to an http(s):// or same-origin "/path" URL; anything else
	// (or empty) falls back to the bundled /public/favicon.svg.
	faviconURL := validURLOrPath(os.Getenv("FAVICON_URL"))
	// Phase 2 Web Push: VAPID keypair from env. The public key is exposed to the
	// client so it can subscribe; the private key signs the push requests and
	// never leaves the server. Unset → push is disabled and the app degrades to
	// the inbox-over-WS path (no behaviour change).
	initPush(
		strings.TrimSpace(os.Getenv("VAPID_PUBLIC_KEY")),
		strings.TrimSpace(os.Getenv("VAPID_PRIVATE_KEY")),
		strings.TrimSpace(os.Getenv("VAPID_SUBJECT")),
	)
	mux.HandleFunc("/api/connect-config", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{
			"primaryColor":   primaryColor,
			"faviconUrl":     faviconURL,
			"vapidPublicKey": vapid.publicKey,
			"version":        version,
		})
	})

	// Optional one-time passcode gate for the agent dashboard (auth.html).
	//
	//   AGENT_PASSCODE  if set (non-empty), the frontend prompts agents for
	//                   this code before revealing the dashboard, once per
	//                   browser. Unset/empty disables the gate entirely — the
	//                   page behaves exactly as before.
	//
	// The code itself is never sent to the browser. /api/agent-gate reports
	// only whether a code is *required* (GET), and verifies a submitted code
	// server-side with a constant-time compare (POST), so it can't be read out
	// of the page source or timed.
	agentPasscode := strings.TrimSpace(os.Getenv("AGENT_PASSCODE"))
	mux.HandleFunc("/api/agent-gate", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, 200, map[string]bool{"required": agentPasscode != ""})
		case http.MethodPost:
			// No passcode configured → nothing to check; treat as open.
			if agentPasscode == "" {
				writeJSON(w, 200, map[string]bool{"ok": true})
				return
			}
			var body struct {
				Code string `json:"code"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				errJSON(w, 400, "bad json")
				return
			}
			ok := subtle.ConstantTimeCompare([]byte(body.Code), []byte(agentPasscode)) == 1
			writeJSON(w, 200, map[string]bool{"ok": ok})
		default:
			errJSON(w, 405, "method not allowed")
		}
	})
	if agentPasscode != "" {
		log.Println("[Server] Agent dashboard passcode gate is ON (AGENT_PASSCODE set).")
	}

	// Built-in authentication (see auth.go): username/password login with
	// server-side sessions, invite-based user creation, password resets, and a
	// per-tenant user-management page at /users. Configured via .env
	// (ADMIN_USERNAME, ADMIN_INITIAL_PASSWORD, SESSION_TTL_HOURS, ...).
	auth, err := newAuthApp()
	if err != nil {
		log.Fatal("init auth:", err)
	}
	auth.Mount(mux)

	// Phase 2 Web Push routes (subscribe/unsubscribe, call ring + pending). The
	// authed routes reuse the auth middleware to resolve tenant + user.
	mountPush(mux, auth)

	// Phase 3 durable availability ("Available until Pause or log out"): the
	// toggle's server-side truth + closed-tab agent discovery for guests.
	mountAvailability(mux, auth)

	// Serve the PWA manifest with the correct type (Go's MIME table has no
	// .webmanifest entry, so it would otherwise fall back to text/plain).
	_ = mime.AddExtensionType(".webmanifest", "application/manifest+json")

	// Static files (embedded)
	staticRoot, err := fs.Sub(staticFS, "static")
	if err != nil {
		log.Fatal(err)
	}
	fileServer := http.FileServer(http.FS(staticRoot))
	// serveHTML serves an embedded HTML page with the build version stamped in
	// (replacing the __APP_VERSION__ placeholder), so the page can detect when
	// it's a stale cached copy and offer a one-click refresh.
	serveHTML := func(w http.ResponseWriter, name string) {
		data, err := fs.ReadFile(staticRoot, name)
		if err != nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		out := strings.ReplaceAll(string(data), "__APP_VERSION__", version)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(out))
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// Always revalidate the frontend (HTML/JS/CSS) so a deploy takes effect
		// on the next load instead of being masked by a stale browser cache —
		// embedded assets carry no ETag/Last-Modified, so without this a browser
		// may serve old JS indefinitely. The service worker is exempt: browsers
		// must be able to refetch /sw.js to pick up updates regardless.
		w.Header().Set("Cache-Control", "no-cache")
		// The agent dashboard requires a signed-in session (the dev-mode
		// bypass below is for local testing only). Guests (index.html) stay
		// public — they never log in.
		if r.URL.Path == "/auth.html" {
			devBypass := devMode && r.URL.Query().Get("dev") == "true"
			if _, ok := auth.sessionFromRequest(r); !ok && !devBypass {
				target := "/login"
				if ref := r.URL.Query().Get("ref"); ref != "" {
					target += "?ref=" + url.QueryEscape(ref)
				}
				http.Redirect(w, r, target, http.StatusSeeOther)
				return
			}
			serveHTML(w, "auth.html")
			return
		}
		// http.FileServer 301-redirects /index.html → /, so serve index.html
		// content directly for both paths instead of redirecting (a redirect
		// to /index.html would loop forever).
		if r.URL.Path == "/" || r.URL.Path == "/index.html" {
			serveHTML(w, "index.html")
			return
		}
		fileServer.ServeHTTP(w, r)
	})

	log.Printf("[Server] Running at http://localhost:%s", port)
	log.Printf("[Server] Agent sign-in: http://localhost:%s/login?ref=YOUR_REF", port)
	if devMode {
		log.Printf("[Server] Agent (dev): http://localhost:%s/auth.html?dev=true&ref=example.com&name=Agent&admin=true", port)
		log.Printf("[Server] Guest (dev): http://localhost:%s/index.html?ref=example.com&name=John+Doe&email=john@example.com", port)
	} else {
		log.Printf("[Server] Guest: http://localhost:%s/index.html?ref=YOUR_REF&name=John+Doe&email=john@example.com", port)
	}
	log.Printf("[Server] Data dir (one SQLite DB per ref): %s", dataDir)
	log.Printf("[Server] Listening on %s:%s", bindAddr, port)
	log.Fatal(http.ListenAndServe(bindAddr+":"+port, mux))
}
