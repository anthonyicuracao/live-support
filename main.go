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
	"net"
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
  updated_at   TEXT    NOT NULL DEFAULT '',
  -- Per-modality intent. The available column stays the master switch ("am I
  -- working"); these say what kind of work. See openDB for the backfill, and
  -- for why has_camera (capability) is kept distinct from video_ok (intent).
  chat_ok      INTEGER NOT NULL DEFAULT 1,
  audio_ok     INTEGER NOT NULL DEFAULT 1,
  video_ok     INTEGER NOT NULL DEFAULT 0
);
-- A private two-party conversation. cid is random and appears in no public
-- response; it names the only channel the traffic flows over, and subscribing
-- to that channel additionally requires a capability token (conversation.go).
CREATE TABLE IF NOT EXISTS conversations (
  cid           TEXT    PRIMARY KEY,
  ref           TEXT    NOT NULL,
  guest_session TEXT    NOT NULL,
  guest_name    TEXT    NOT NULL DEFAULT '',
  agent_user_id INTEGER NOT NULL,
  call_type     TEXT    NOT NULL,
  created_at    INTEGER NOT NULL,
  ended_at      INTEGER,
  -- Activity state, kept separate from lifecycle (ended_at). A conversation
  -- with no recent visitor message is INACTIVE: it still exists and still shows
  -- in the agent's list, but it stops consuming capacity. Zendesk reaches the
  -- same split and defaults to not counting inactive conversations; without it,
  -- an abandoned chat occupies a slot forever and the agent silently stops
  -- being routed work.
  last_activity_at INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_conversations_agent ON conversations(agent_user_id, ended_at);
CREATE INDEX IF NOT EXISTS idx_conversations_ref ON conversations(ref);
-- Chat transcript. The live carrier is the conversation's WS channel; this is
-- the record, so an agent woken by push into a fresh console sees what the
-- guest already said instead of an empty thread.
CREATE TABLE IF NOT EXISTS chat_messages (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  cid        TEXT    NOT NULL,
  sender     TEXT    NOT NULL CHECK (sender IN ('guest','agent')),
  body       TEXT    NOT NULL,
  created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_chat_messages_cid ON chat_messages(cid, id);
-- Per-tenant configuration. Key/value rather than a column per flag: tenant
-- databases are created independently and are never all on the same version,
-- so a column each would mean a migration each.
CREATE TABLE IF NOT EXISTS tenant_settings (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
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
	// Self-heal orphans: FK cascade is off on this connection, and deletes
	// predating removeUser's explicit child cleanup left availability / push
	// rows pointing at users that no longer exist — they surface as
	// un-killable ghosts in discovery. Sweep them once on open so a deploy
	// fixes existing data, not just future deletes.
	if _, err := d.Exec(`DELETE FROM agent_availability WHERE user_id NOT IN (SELECT id FROM users)`); err != nil {
		return nil, err
	}
	if _, err := d.Exec(`DELETE FROM push_subscriptions WHERE user_id NOT IN (SELECT id FROM users)`); err != nil {
		return nil, err
	}
	// messages.is_read added after the initial schema — idempotent column add
	// (CREATE TABLE IF NOT EXISTS can't alter an existing table). Gate on an
	// explicit column-existence check rather than the ALTER's error, because the
	// pure-Go SQLite driver returns a non-nil error from "ADD COLUMN ... NOT
	// NULL" even when it succeeds. On the one boot that adds the column, mark all
	// pre-existing notes read so the new inbox starts clean — the agent has
	// already seen them in the old History table; only new notes arrive unread.
	var hasIsRead int
	_ = d.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('messages') WHERE name = 'is_read'`).Scan(&hasIsRead)
	if hasIsRead == 0 {
		_, _ = d.Exec(`ALTER TABLE messages ADD COLUMN is_read INTEGER NOT NULL DEFAULT 0`)
		_, _ = d.Exec(`UPDATE messages SET is_read = 1`)
	}

	// Per-modality availability (CHAT-CHANNEL-PLAN.md Part A1). Same
	// column-existence idiom as is_read above, for the same driver reason.
	//
	// Backfill on the one boot that adds the columns:
	//   audio_ok = 1        every existing available agent takes audio today
	//   video_ok = has_camera   discovery INFERRED video from the camera, so
	//                           this preserves exactly what guests saw
	//   chat_ok  = 1        Ron's call: chat needs no hardware, so every
	//                       available agent becomes chat-available on deploy
	//
	// has_camera is kept alongside video_ok on purpose: it is a CAPABILITY
	// fact (is there a camera) and still drives whether the console may offer
	// the video toggle, while video_ok is INTENT (am I taking video calls).
	// Conflating them is what made "I have a camera but I'm only taking chat"
	// impossible to express.
	// A visitor has at most one open conversation per modality. This is a
	// MIGRATION, not schema, and the distinction is the whole lesson: it first
	// lived in the schema block, which runs on every open. Tenants created
	// before the invariant already held duplicates, so the CREATE UNIQUE INDEX
	// failed, so openDB failed, so the tenant could not be opened AT ALL —
	// every sign-in rejected with a generic "invalid or has expired", and the
	// cause invisible.
	//
	// So: reconcile the data first, then constrain it, and treat failure as
	// non-fatal. A missing index degrades an invariant to a convention, which
	// the find-or-create paths already enforce in code. A tenant that will not
	// open is total data loss from the user's point of view. Those are not
	// remotely the same severity, and the schema block treated them as equal.
	if _, err := d.Exec(
		`UPDATE conversations SET ended_at = strftime('%s','now')
		  WHERE ended_at IS NULL AND cid NOT IN (
		    SELECT cid FROM (
		      SELECT cid, ROW_NUMBER() OVER (
		               PARTITION BY ref, guest_session, call_type
		               ORDER BY last_activity_at DESC, created_at DESC
		             ) AS rn
		        FROM conversations WHERE ended_at IS NULL
		    ) WHERE rn = 1
		  )`); err != nil {
		log.Printf("[DB] could not reconcile duplicate open conversations: %v", err)
	}
	if _, err := d.Exec(
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_conversations_open_unique
		   ON conversations(ref, guest_session, call_type) WHERE ended_at IS NULL`); err != nil {
		log.Printf("[DB] open-conversation uniqueness not enforced at the schema level: %v", err)
	}

	var hasLastActivity int
	_ = d.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('conversations') WHERE name = 'last_activity_at'`).Scan(&hasLastActivity)
	if hasLastActivity == 0 {
		_, _ = d.Exec(`ALTER TABLE conversations ADD COLUMN last_activity_at INTEGER NOT NULL DEFAULT 0`)
		_, _ = d.Exec(`UPDATE conversations SET last_activity_at = created_at`)
	}

	// Delivery receipts. Two timestamps rather than a status enum: they answer
	// "when", they are monotonic, and a later state cannot silently overwrite an
	// earlier one the way a single mutable status can.
	var hasDelivered int
	_ = d.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('chat_messages') WHERE name = 'delivered_at'`).Scan(&hasDelivered)
	if hasDelivered == 0 {
		_, _ = d.Exec(`ALTER TABLE chat_messages ADD COLUMN delivered_at INTEGER`)
		_, _ = d.Exec(`ALTER TABLE chat_messages ADD COLUMN read_at INTEGER`)
	}

	var hasChatOK int
	_ = d.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('agent_availability') WHERE name = 'chat_ok'`).Scan(&hasChatOK)
	if hasChatOK == 0 {
		_, _ = d.Exec(`ALTER TABLE agent_availability ADD COLUMN chat_ok INTEGER NOT NULL DEFAULT 1`)
		_, _ = d.Exec(`ALTER TABLE agent_availability ADD COLUMN audio_ok INTEGER NOT NULL DEFAULT 1`)
		_, _ = d.Exec(`ALTER TABLE agent_availability ADD COLUMN video_ok INTEGER NOT NULL DEFAULT 0`)
		_, _ = d.Exec(`UPDATE agent_availability SET chat_ok = 1, audio_ok = 1, video_ok = has_camera`)
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
	// Lower-cased first: a ref is a domain, and domains are case-insensitive.
	// Treating them otherwise made "instantAIguru.com" a DIFFERENT tenant from
	// "instantaiguru.com" — a separate database, with none of the real users in
	// it — so a mis-typed capital turned a correct password into a failed
	// login. Ron hit exactly that. Three case-variant databases had quietly
	// accumulated on the appliance before anyone noticed.
	s := strings.ToLower(ref)
	s = refSanitizer.ReplaceAllString(s, "_")
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
	reconcileRefCase(dataDir)
	return &dbPool{dataDir: dataDir, dbs: make(map[string]*sql.DB)}, nil
}

// reconcileRefCase renames mixed-case tenant files to their lower-case name,
// so refs written before normalisation stay reachable afterwards.
//
// Only when the lower-case name is free. If both exist they are two real
// databases with two real histories, and picking one would silently discard
// the other — so it says so and leaves them alone. That case needs a human who
// knows which is the live tenant.
//
// Non-fatal throughout: a tenant that cannot be renamed must not take the whole
// appliance down with it. Losing one tenant's routing is bad; refusing to start
// takes every tenant offline, which is how a unique index once did exactly that.
func reconcileRefCase(dataDir string) {
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".db") {
			continue // -wal and -shm ride along with their .db below
		}
		lower := strings.ToLower(name)
		if lower == name {
			continue
		}
		if _, err := os.Stat(filepath.Join(dataDir, lower)); err == nil {
			log.Printf("[DB] %q and %q both exist — leaving both; "+
				"refs now resolve to %q, so move the other aside if it is the live one", name, lower, lower)
			continue
		}
		// The journal files must travel with the database or SQLite sees a
		// truncated history on the next open.
		for _, suffix := range []string{"", "-wal", "-shm"} {
			from := filepath.Join(dataDir, name+suffix)
			if _, err := os.Stat(from); err != nil {
				continue
			}
			if err := os.Rename(from, filepath.Join(dataDir, lower+suffix)); err != nil {
				log.Printf("[DB] could not rename %q: %v", name+suffix, err)
			}
		}
		log.Printf("[DB] renamed %q to %q (refs are case-insensitive)", name, lower)
	}
}

// getExisting opens a tenant DB only if it is already on disk, so a ref nobody
// provisioned resolves to nothing instead of quietly becoming a new tenant.
func (p *dbPool) getExisting(ref string) (*sql.DB, error) {
	if !p.exists(ref) {
		return nil, fmt.Errorf("unknown ref")
	}
	return p.get(ref)
}

// get returns the DB for ref, opening (and CREATING) it if needed. Callers must
// have established that the ref is one this appliance should serve — see
// tenantDB vs tenantDBProvision.
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
	// Token authorises a conversation channel. Presented with {"action":"auth"}
	// before subscribing; see canUseChannel.
	Token string `json:"token,omitempty"`
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

	// ---- authorization state, set at handshake or by an "auth" message ----

	// agentRef is the tenant this connection is a signed-in agent of. Empty for
	// a guest. Set once, from the session cookie on the handshake, and never
	// from anything the client sends.
	agentRef string
	// agentUserID is the signed-in agent's user id, from the same handshake
	// session as agentRef. Used to bind the user-keyed ring inbox to its owner.
	agentUserID int64
	// grants is the set of conversation channels this connection has presented
	// a valid capability token for.
	grants map[string]bool
}

// grant records that this connection may use a conversation channel.
func (c *Conn) grant(channel string) {
	c.mu.Lock()
	if c.grants == nil {
		c.grants = make(map[string]bool)
	}
	c.grants[channel] = true
	c.mu.Unlock()
}

// canUseChannel is the single authorization decision for the WebSocket.
//
// A POSITIVE whitelist by namespace. Anything unrecognised is refused, so a new
// channel namespace cannot be introduced without deciding who may use it —
// which is precisely the failure this replaces, where every channel was open to
// everyone and a later feature published the ids.
//
// It is ACTION-aware because presence is genuinely asymmetric: it is a
// directory that everyone writes and only agents read. A guest must be able to
// announce itself (or the console would show no waiting visitors at all), but
// must not be able to read the roster back — reading it was the leak.
func (c *Conn) canUseChannel(action, channel string) bool {
	granted := func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.grants[channel]
	}
	ownTenant := func() bool {
		i := strings.Index(channel, ":")
		return c.agentRef != "" && i >= 0 && channel[i+1:] == c.agentRef
	}

	switch {
	case strings.HasPrefix(channel, "conv:"), strings.HasPrefix(channel, "guest:"):
		// Private traffic. Requires a capability token for THIS channel;
		// knowing the id is not enough, and no id is published in the first
		// place. guest:<sid> is how an AGENT opens contact with a visitor, who
		// has no authenticated channel of their own.
		return granted()

	case strings.HasPrefix(channel, "presence:"):
		if action == "track" || action == "untrack" {
			// Write-only for everyone: a guest announcing itself. One entry per
			// connection (Conn.tracked is keyed by channel), so this cannot be
			// used to flood the roster.
			return true
		}
		return ownTenant()

	case strings.HasPrefix(channel, "dashboard:"):
		return ownTenant()

	case strings.HasPrefix(channel, "inbox:user:"):
		// The ring fan-out target: the server broadcasts an incoming call to
		// every live console of one agent. Readable ONLY by that agent, on
		// their own tenant — identity comes from the handshake session, so a
		// client cannot name someone else's inbox and listen to their calls.
		//
		// The per-SESSION inbox:<id> channels this replaced are gone entirely:
		// their names were published, which is what let a passer-by listen.
		return c.agentRef != "" && c.agentUserID != 0 &&
			channel == fmt.Sprintf("inbox:user:%s:%d", c.agentRef, c.agentUserID)

	default:
		return false
	}
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

// subscriberCount reports how many live connections are subscribed to a
// channel — diagnostic for "did anyone actually hear this broadcast?".
func (h *Hub) subscriberCount(channel string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.channels[channel])
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

// liveAgentUserIDs reports which agents currently have a console open for a
// tenant, from live presence state.
//
// Guests used to learn this by subscribing to presence:<ref> themselves, which
// also let them read the whole roster and inject into the channel. They now get
// it folded into the REST discovery response instead: same information, none of
// the reach.
func (h *Hub) liveAgentUserIDs(ref string) map[int64]bool {
	out := map[int64]bool{}
	for _, state := range h.presenceUsers("presence:" + ref) {
		if role, _ := state["role"].(string); role != "auth" {
			continue
		}
		// JSON numbers arrive as float64 through the generic map.
		switch v := state["user_id"].(type) {
		case float64:
			if v > 0 {
				out[int64(v)] = true
			}
		case int64:
			if v > 0 {
				out[v] = true
			}
		}
	}
	return out
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

// wsSessionCheckEvery is how often an agent's WebSocket re-checks that its login
// session still exists.
const wsSessionCheckEvery = 60 * time.Second

// wsSessionToken pulls the login session out of the handshake request, if there
// is one. Cookies ride the WebSocket handshake like any other request, so this
// needs no protocol of its own.
//
// A guest has no cookie and gets ok=false — guests are unauthenticated by
// design and this must not change that.
func wsSessionToken(r *http.Request) (ref, raw string, userID int64, ok bool) {
	c, err := r.Cookie(sessionCookieName)
	if err != nil || c.Value == "" {
		return "", "", 0, false
	}
	ref, raw, ok = decodeSessionCookie(c.Value)
	if !ok {
		return "", "", 0, false
	}
	db, err := dbs.get(ref)
	if err != nil {
		return "", "", 0, false
	}
	_, u, err := lookupAuthSession(db, raw)
	if err != nil {
		return "", "", 0, false
	}
	return ref, raw, u.ID, true
}

// watchWSSession closes an agent's socket once their session stops resolving.
//
// Without this, WS presence outlives the session indefinitely: the socket was
// never authenticated, so an agent whose session had been revoked kept showing
// as live to guests, and their console kept looking signed in, while every
// authed HTTP call 401ed. Presence claiming an agent is reachable when the
// server will refuse everything they do is the lie this closes.
//
// Only connections that arrived WITH a valid session are watched. A guest
// socket has no session to lose and is left alone.
func watchWSSession(ctx context.Context, cancel context.CancelFunc, ref, raw string) {
	t := time.NewTicker(wsSessionCheckEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			db, err := dbs.get(ref)
			if err != nil {
				continue // transient tenant-DB problem is not session loss
			}
			if _, _, err := lookupAuthSession(db, raw); err != nil {
				cancel()
				return
			}
		}
	}
}

func wsHandler(w http.ResponseWriter, r *http.Request) {
	// Resolved BEFORE the upgrade, while it is still an ordinary request.
	authRef, authRaw, authUserID, authed := wsSessionToken(r)

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
		grants:  make(map[string]bool),
	}
	if authed {
		// Derived from the session cookie on the handshake — never from a
		// client-supplied field, which is the whole point.
		c.agentRef = authRef
		c.agentUserID = authUserID
	}

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	if authed {
		go watchWSSession(ctx, cancel, authRef, authRaw)
	}

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
		case "auth":
			// Present a capability token to unlock its conversation channel.
			// The channel is taken from the TOKEN, never from msg.Channel — a
			// client naming a channel it has no token for must not be able to
			// talk its way in.
			t, err := parseConvToken(convSecret, msg.Token)
			if err != nil {
				select {
				case c.send <- ServerMsg{Type: "error", Action: "auth"}:
				default:
				}
				continue
			}
			ch := channelForToken(t)
			c.grant(ch)
			select {
			case c.send <- ServerMsg{Type: "ack", Action: "auth", Channel: ch}:
			default:
			}
		case "subscribe":
			if !c.canUseChannel(msg.Action, msg.Channel) {
				select {
				case c.send <- ServerMsg{Type: "error", Action: "subscribe", Channel: msg.Channel}:
				default:
				}
				continue
			}
			hub.subscribe(c, msg.Channel)
			select {
			case c.send <- ServerMsg{Type: "ack", Action: "subscribe", Channel: msg.Channel}:
			default:
			}
		case "unsubscribe":
			// Always allowed: leaving a channel can harm nobody.
			hub.unsubscribe(c, msg.Channel)
		case "broadcast":
			// Same gate as subscribe. Injecting into a conversation (a forged
			// incoming call, a forged message, forged WebRTC signalling) is at
			// least as damaging as reading it.
			if !c.canUseChannel(msg.Action, msg.Channel) {
				continue
			}
			hub.broadcast(msg.Channel, msg.Event, msg.Payload)
		case "track":
			if !c.canUseChannel(msg.Action, msg.Channel) {
				continue
			}
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
		// ?unread=1 → just the count of unread messages for this ref (drives the
		// console's "collapse Messages when nothing is unread" + header badge).
		if r.URL.Query().Get("unread") == "1" {
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
			var n int
			_ = db.QueryRow("SELECT COUNT(*) FROM messages WHERE ref = ? AND is_read = 0", ref).Scan(&n)
			writeJSON(w, 200, map[string]int{"unread": n})
			return
		}
		listHandler("messages", "created_at")(w, r)
	case http.MethodPatch:
		// PATCH /api/messages/<id>?ref=<ref> — mark a message read (opened).
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		if len(parts) != 3 {
			errJSON(w, 400, "message id required in path")
			return
		}
		id := parts[2]
		var tdb *sql.DB
		if q := r.URL.Query().Get("ref"); q != "" {
			d, err := dbs.get(q)
			if err != nil {
				errJSON(w, 400, err.Error())
				return
			}
			tdb = d
		} else {
			tdb, _ = dbs.findByColumn("messages", "id", id)
		}
		if tdb == nil {
			writeJSON(w, 200, map[string]bool{"ok": true}) // nothing to mark
			return
		}
		if _, err := tdb.Exec("UPDATE messages SET is_read = 1 WHERE id = ?", id); err != nil {
			errJSON(w, 500, err.Error())
			return
		}
		// No refresh broadcast: the clicking console already updated optimistically,
		// and a reload here would re-collapse the note the agent just expanded to
		// read. Other consoles pick up the read state on their next list load.
		writeJSON(w, 200, map[string]bool{"ok": true})
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
				"reachable": false, "reachableCount": 0,
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
		// Count agents reachable for a call now: exclude both 'offline'
		// (disconnected) and 'paused' (connected but not taking calls). A
		// connected agent is 'available' or 'in-call'; both mean an agent is
		// present, so the legacy "is anyone online" signal stays truthful after
		// 'paused' became its own status (was previously stored as 'offline').
		rows, err := d.Query(
			"SELECT has_camera, has_mic FROM sessions WHERE role = 'auth' AND status NOT IN ('offline', 'paused') AND last_seen_at > ?",
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

	// Also surface agents reachable via Web Push even when no console is live:
	// durable-available ("until Pause/logout"), active account, with a push
	// subscription, and seen within the discovery freshness window (same gate
	// as /api/agents/available). Lets a widget light up "talk to a live agent"
	// knowing the ring will wake a closed laptop, not just an open tab.
	reachable := 0
	freshCutoff := ""
	if window := discoveryFreshness(); window > 0 {
		freshCutoff = time.Now().UTC().Add(-window).Format(time.RFC3339)
	}
	for _, d := range targets {
		var n int
		var err error
		if freshCutoff != "" {
			err = d.QueryRow(
				`SELECT COUNT(*) FROM agent_availability a
				 JOIN users u ON u.id = a.user_id AND u.active = 1
				 WHERE a.available = 1 AND a.updated_at > ?
				   AND EXISTS (SELECT 1 FROM push_subscriptions p WHERE p.user_id = a.user_id)`,
				freshCutoff).Scan(&n)
		} else {
			err = d.QueryRow(
				`SELECT COUNT(*) FROM agent_availability a
				 JOIN users u ON u.id = a.user_id AND u.active = 1
				 WHERE a.available = 1
				   AND EXISTS (SELECT 1 FROM push_subscriptions p WHERE p.user_id = a.user_id)`).Scan(&n)
		}
		if err == nil {
			reachable += n
		}
	}

	writeJSON(w, 200, map[string]interface{}{
		"online": count > 0, "count": count, "withCamera": withCamera, "withMic": withMic,
		"reachable": reachable > 0, "reachableCount": reachable,
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
		log.Println("[Server] DEV_MODE is ON — /dev/token will sign a sign-in for ANY user")
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

	// GET /dev/token?ref=&user=&role= — the LOCAL equivalent of the platform's
	// /v1/signAppliance, for development.
	//
	// A token, not a sign-in: the appliance already has exactly one sign-in
	// entry point, /sso?t=. How the token was signed is none of its business,
	// and adding a second entry point to express "signed differently" was the
	// wrong shape. Only the SIGNER varies by environment — production uses the
	// platform lambda, a dev box uses this — and the URL the caller navigates to
	// is identical either way.
	//
	// Why it has to exist at all: a managed token is signed with the PLATFORM's
	// secret, and a dev appliance holds its own, so a platform-signed token can
	// never verify here. The alternative is a production secret on a developer's
	// disk.
	//
	// This mints a session for any user, so it is a total authentication bypass,
	// gated on DEV_MODE — off by default, never set on a deployed host. The
	// listener not being reachable off-box is a NETWORK control
	// (docker-compose.local.yml publishes on 127.0.0.1); an in-process address
	// test cannot express it inside a container, where Docker SNATs host traffic
	// to the bridge gateway and host and LAN look identical. The address check
	// below is defence in depth for the one case it can still catch.
	mux.HandleFunc("GET /dev/token", func(w http.ResponseWriter, r *http.Request) {
		if !devMode || !isLocalOrPrivateRequest(r) {
			http.NotFound(w, r) // indistinguishable from the route not existing
			return
		}
		w.Header().Set("Access-Control-Allow-Origin", "*")
		ref := strings.TrimSpace(r.URL.Query().Get("ref"))
		user := strings.TrimSpace(r.URL.Query().Get("user"))
		if ref == "" || user == "" {
			errJSON(w, 400, "ref and user required")
			return
		}
		role := r.URL.Query().Get("role")
		if role != RoleAdmin {
			role = RoleAgent
		}
		tok, err := mintApplianceToken(auth.ssoSecret, ref, user, role, "", 5*time.Minute)
		if err != nil {
			errJSON(w, 500, "internal error")
			return
		}
		log.Printf("[DevToken] signed a sign-in for %s@%s (role %s)", user, ref, role)
		writeJSON(w, 200, map[string]any{"token": tok})
	})

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

// isLocalOrPrivateRequest reports whether a request arrived from a loopback or
// private address.
//
// Deliberately NOT presented as "only from this machine". Behind Docker every
// request from the host is SNAT'd to the bridge gateway, so host and LAN
// traffic are indistinguishable at this layer and a strict loopback test simply
// breaks the feature while protecting nothing. Keeping the port off public
// interfaces is the control that works; this only catches the case where a
// request provably came from a public address.
func isLocalOrPrivateRequest(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && (ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast())
}
