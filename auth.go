// auth.go — built-in username/password authentication and user management.
//
// Ported from the csat project's admin auth system:
//   - argon2id password hashing (constant-time verification, uniform timing
//     for unknown usernames)
//   - server-side sessions stored in SQLite with sha256-hashed tokens at rest
//   - CSRF protection: double-submit cookie for pre-session forms (login,
//     invite redeem, forgot, reset) and a per-session synchronizer token for
//     authenticated forms
//   - invite-based user creation (admin mints a single-use link; the new user
//     picks their own password)
//   - admin-issued one-time password-reset links + a self-service "forgot
//     password" request flag
//   - per-username login throttling
//
// Multi-tenant: every tenant (ref) keeps its own users / auth_sessions /
// invites / password_resets tables inside its per-ref SQLite database (see
// dbPool in main.go). The session cookie encodes the ref alongside the opaque
// token, so each request is routed to the right tenant DB without a global
// scan. All pre-session pages carry the ref in the URL / a form field.
package main

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/argon2"
)

//go:embed templates/*.tmpl
var templatesFS embed.FS

// ───────────────────────── password hashing (argon2id) ─────────────────────

const (
	argonTime    = 3
	argonMemory  = 64 * 1024 // KiB
	argonThreads = 4
	argonKeyLen  = 32
	argonSaltLen = 16
)

// minPasswordLen is the enforced minimum for user-chosen passwords.
const minPasswordLen = 12

var errBadHash = errors.New("invalid password hash")

// hashPassword returns an encoded argon2id hash string.
func hashPassword(pw string) (string, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(pw), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key),
	), nil
}

// verifyPassword reports whether pw matches the encoded hash, in constant time.
func verifyPassword(encoded, pw string) bool {
	salt, want, mem, t, p, err := decodePasswordHash(encoded)
	if err != nil {
		return false
	}
	got := argon2.IDKey([]byte(pw), salt, t, mem, p, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}

func decodePasswordHash(encoded string) (salt, key []byte, mem, t uint32, threads uint8, err error) {
	parts := strings.Split(encoded, "$")
	// ["", "argon2id", "v=19", "m=..,t=..,p=..", salt, key]
	if len(parts) != 6 || parts[1] != "argon2id" {
		return nil, nil, 0, 0, 0, errBadHash
	}
	var version int
	if _, err = fmt.Sscanf(parts[2], "v=%d", &version); err != nil {
		return nil, nil, 0, 0, 0, errBadHash
	}
	if _, err = fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &mem, &t, &threads); err != nil {
		return nil, nil, 0, 0, 0, errBadHash
	}
	if salt, err = base64.RawStdEncoding.DecodeString(parts[4]); err != nil {
		return nil, nil, 0, 0, 0, errBadHash
	}
	if key, err = base64.RawStdEncoding.DecodeString(parts[5]); err != nil {
		return nil, nil, 0, 0, 0, errBadHash
	}
	return salt, key, mem, t, threads, nil
}

// dummyHash is verified against unknown usernames to keep login timing uniform
// (mitigates user enumeration). Generated once at package init.
var dummyHash, _ = hashPassword("dummy-password-for-timing-uniformity")

// ───────────────────────── tokens ──────────────────────────────────────────

// randToken returns a 256-bit URL-safe random string.
func randToken() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// hashToken returns the hex sha256 of a raw token (what we store at rest).
func hashToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// ───────────────────────── store: users ─────────────────────────────────────

// roles
const (
	RoleAdmin = "admin"
	RoleAgent = "agent"
)

var errNotFound = errors.New("not found")

// User is an admin/agent account (one row per tenant DB).
type User struct {
	ID           int64
	Username     string
	PasswordHash string
	Role         string
	MustChangePW bool
	Active       bool
	// ResetRequested reports a pending self-service password-reset request. It
	// is populated only by listUsers (the admin view); the auth-path scans
	// leave it false.
	ResetRequested bool
}

// AuthSession is a server-side login session (distinct from the presence
// "sessions" table used by the WebRTC dashboard).
type AuthSession struct {
	ID        string // sha256(raw token), hex
	UserID    int64
	CSRF      string
	ExpiresAt int64
}

func userByUsername(db *sql.DB, username string) (*User, error) {
	return scanUser(db.QueryRow(
		`SELECT id, username, password_hash, role, must_change_pw, active FROM users WHERE username = ?`, username))
}

func userByID(db *sql.DB, id int64) (*User, error) {
	return scanUser(db.QueryRow(
		`SELECT id, username, password_hash, role, must_change_pw, active FROM users WHERE id = ?`, id))
}

func scanUser(row *sql.Row) (*User, error) {
	var u User
	if err := row.Scan(&u.ID, &u.Username, &u.PasswordHash, &u.Role, &u.MustChangePW, &u.Active); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errNotFound
		}
		return nil, err
	}
	return &u, nil
}

func countUsers(db *sql.DB) (int, error) {
	var n int
	err := db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n)
	return n, err
}

func listUsers(db *sql.DB) ([]User, error) {
	rows, err := db.Query(
		`SELECT id, username, password_hash, role, must_change_pw, active, reset_requested_at
		 FROM users ORDER BY username`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		var (
			u   User
			req sql.NullInt64
		)
		if err := rows.Scan(&u.ID, &u.Username, &u.PasswordHash, &u.Role, &u.MustChangePW, &u.Active, &req); err != nil {
			return nil, err
		}
		u.ResetRequested = req.Valid
		out = append(out, u)
	}
	return out, rows.Err()
}

func setPassword(db *sql.DB, userID int64, hash string) error {
	_, err := db.Exec(`UPDATE users SET password_hash = ?, must_change_pw = 0 WHERE id = ?`, hash, userID)
	return err
}

func setLastLogin(db *sql.DB, userID int64) {
	_, _ = db.Exec(`UPDATE users SET last_login_at = ? WHERE id = ?`, time.Now().Unix(), userID)
}

func deactivateUser(db *sql.DB, userID int64) error {
	_, err := db.Exec(`UPDATE users SET active = 0 WHERE id = ?`, userID)
	return err
}

// requestPasswordReset flags an outstanding reset request for the named user.
// A non-existent username is a no-op (and silently reports no error) so the
// public handler can keep its response uniform and avoid user enumeration.
func requestPasswordReset(db *sql.DB, username string) error {
	_, err := db.Exec(
		`UPDATE users SET reset_requested_at = ? WHERE username = ?`, time.Now().Unix(), username)
	return err
}

// removeUser permanently deletes a user and the rows that reference them.
// SQLite foreign keys are not enforced on this connection (no foreign_keys
// pragma), so every referencing table is cleaned up explicitly, inside one
// transaction.
func removeUser(db *sql.DB, userID int64) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(
		`DELETE FROM invites WHERE created_by = ? OR redeemed_user_id = ?`, userID, userID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM auth_sessions WHERE user_id = ?`, userID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM password_resets WHERE user_id = ?`, userID); err != nil {
		return err
	}
	// Durable availability + push subscriptions also reference the user. Without
	// these, a deleted agent lingers as an un-killable ghost in discovery (their
	// availability row keeps ringing and there's no account left to Pause it).
	if _, err := tx.Exec(`DELETE FROM agent_availability WHERE user_id = ?`, userID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM push_subscriptions WHERE user_id = ?`, userID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM users WHERE id = ?`, userID); err != nil {
		return err
	}
	return tx.Commit()
}

// adminCount returns the total number of admin accounts (active or not).
func adminCount(db *sql.DB) int {
	var n int
	_ = db.QueryRow(`SELECT COUNT(*) FROM users WHERE role = ?`, RoleAdmin).Scan(&n)
	return n
}

func activeAdminCount(db *sql.DB) int {
	var n int
	_ = db.QueryRow(`SELECT COUNT(*) FROM users WHERE role = ? AND active = 1`, RoleAdmin).Scan(&n)
	return n
}

// ───────────────────────── store: auth sessions ────────────────────────────

func createAuthSession(db *sql.DB, userID int64, ttl time.Duration) (rawToken string, csrf string, err error) {
	rawToken = randToken()
	csrf = randToken()
	now := time.Now()
	_, err = db.Exec(
		`INSERT INTO auth_sessions(id, user_id, csrf_token, created_at, expires_at) VALUES(?, ?, ?, ?, ?)`,
		hashToken(rawToken), userID, csrf, now.Unix(), now.Add(ttl).Unix(),
	)
	if err != nil {
		return "", "", err
	}
	return rawToken, csrf, nil
}

// lookupAuthSession resolves a raw cookie token to its session and user,
// rejecting expired sessions and inactive users.
func lookupAuthSession(db *sql.DB, rawToken string) (*AuthSession, *User, error) {
	var s AuthSession
	err := db.QueryRow(
		`SELECT id, user_id, csrf_token, expires_at FROM auth_sessions WHERE id = ?`, hashToken(rawToken),
	).Scan(&s.ID, &s.UserID, &s.CSRF, &s.ExpiresAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil, errNotFound
		}
		return nil, nil, err
	}
	if time.Now().Unix() >= s.ExpiresAt {
		_, _ = db.Exec(`DELETE FROM auth_sessions WHERE id = ?`, s.ID)
		return nil, nil, errNotFound
	}
	u, err := userByID(db, s.UserID)
	if err != nil || !u.Active {
		return nil, nil, errNotFound
	}
	return &s, u, nil
}

func deleteAuthSession(db *sql.DB, rawToken string) {
	_, _ = db.Exec(`DELETE FROM auth_sessions WHERE id = ?`, hashToken(rawToken))
}

func sweepAuthSessions(db *sql.DB) {
	_, _ = db.Exec(`DELETE FROM auth_sessions WHERE expires_at < ?`, time.Now().Unix())
}

// ───────────────────────── store: invites ──────────────────────────────────

// Invite is a pending invitation.
type Invite struct {
	ID       int64
	Role     string
	Username sql.NullString
	// Platform is true for a platform-minted invite (created_by IS NULL), which
	// may reclaim an existing account on redemption (the provisioning break-glass
	// for an admin who lost their password).
	Platform bool
}

func createInviteRow(db *sql.DB, role, username string, createdBy int64, ttl time.Duration) (rawToken string, err error) {
	rawToken = randToken()
	now := time.Now()
	var uname any
	if username != "" {
		uname = username
	}
	// createdBy == 0 means "no tenant creator" (a platform-minted admin invite
	// for a brand-new tenant) — store NULL.
	var creator any
	if createdBy != 0 {
		creator = createdBy
	}
	_, err = db.Exec(
		`INSERT INTO invites(token_hash, role, username, created_by, created_at, expires_at)
		 VALUES(?, ?, ?, ?, ?, ?)`,
		hashToken(rawToken), role, uname, creator, now.Unix(), now.Add(ttl).Unix(),
	)
	if err != nil {
		return "", err
	}
	return rawToken, nil
}

func inviteByToken(db *sql.DB, rawToken string) (*Invite, error) {
	var (
		inv     Invite
		creator sql.NullInt64
	)
	err := db.QueryRow(
		`SELECT id, role, username, created_by FROM invites
		 WHERE token_hash = ? AND redeemed_at IS NULL AND expires_at > ?`,
		hashToken(rawToken), time.Now().Unix(),
	).Scan(&inv.ID, &inv.Role, &inv.Username, &creator)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errNotFound
		}
		return nil, err
	}
	inv.Platform = !creator.Valid // created_by IS NULL => platform-minted
	return &inv, nil
}

var errUsernameTaken = errors.New("username taken")

// redeemInvite creates the user and marks the invite redeemed atomically.
func redeemInvite(db *sql.DB, inv *Invite, username, passwordHash string) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var newID int64
	res, err := tx.Exec(
		`INSERT INTO users(username, password_hash, role, must_change_pw, active, created_at)
		 VALUES(?, ?, ?, 0, 1, ?)`,
		username, passwordHash, inv.Role, time.Now().Unix(),
	)
	switch {
	case err == nil:
		newID, _ = res.LastInsertId()
	case !isUniqueViolation(err):
		return err
	case !inv.Platform:
		// A normal (admin-issued) invite never overrides an existing account.
		return errUsernameTaken
	default:
		// Platform break-glass: the entered username already exists — reclaim
		// that account with the invite's role and the new password, reactivate
		// it, and revoke its sessions (the invite acts as a password reset).
		if _, uerr := tx.Exec(
			`UPDATE users SET password_hash = ?, role = ?, must_change_pw = 0, active = 1
			 WHERE username = ?`, passwordHash, inv.Role, username,
		); uerr != nil {
			return uerr
		}
		if uerr := tx.QueryRow(`SELECT id FROM users WHERE username = ?`, username).Scan(&newID); uerr != nil {
			return uerr
		}
		if _, uerr := tx.Exec(`DELETE FROM auth_sessions WHERE user_id = ?`, newID); uerr != nil {
			return uerr
		}
	}

	r, err := tx.Exec(
		`UPDATE invites SET redeemed_at = ?, redeemed_user_id = ? WHERE id = ? AND redeemed_at IS NULL`,
		time.Now().Unix(), newID, inv.ID,
	)
	if err != nil {
		return err
	}
	if n, _ := r.RowsAffected(); n == 0 {
		return errNotFound // raced with another redemption
	}
	return tx.Commit()
}

func sweepInvites(db *sql.DB) {
	_, _ = db.Exec(`DELETE FROM invites WHERE redeemed_at IS NULL AND expires_at < ?`, time.Now().Unix())
}

func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(strings.ToUpper(err.Error()), "CONSTRAINT FAILED")
}

// ───────────────────────── store: password resets ──────────────────────────

// createResetRow mints a one-time reset token for an existing user and clears
// any pending self-service request (the admin is now fulfilling it).
func createResetRow(db *sql.DB, userID, createdBy int64, ttl time.Duration) (rawToken string, err error) {
	rawToken = randToken()
	now := time.Now()
	tx, err := db.Begin()
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(
		`INSERT INTO password_resets(token_hash, user_id, created_by, created_at, expires_at)
		 VALUES(?, ?, ?, ?, ?)`,
		hashToken(rawToken), userID, createdBy, now.Unix(), now.Add(ttl).Unix(),
	); err != nil {
		return "", err
	}
	if _, err = tx.Exec(`UPDATE users SET reset_requested_at = NULL WHERE id = ?`, userID); err != nil {
		return "", err
	}
	if err = tx.Commit(); err != nil {
		return "", err
	}
	return rawToken, nil
}

// resetByToken resolves a raw reset token to its user id, rejecting tokens that
// are expired or already used.
func resetByToken(db *sql.DB, rawToken string) (userID int64, err error) {
	err = db.QueryRow(
		`SELECT user_id FROM password_resets
		 WHERE token_hash = ? AND used_at IS NULL AND expires_at > ?`,
		hashToken(rawToken), time.Now().Unix(),
	).Scan(&userID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, errNotFound
		}
		return 0, err
	}
	return userID, nil
}

// redeemReset sets the user's new password and consumes the token atomically.
// It also reactivates the account (so a deactivated user can recover), clears
// any pending request, and revokes all of the user's existing sessions.
func redeemReset(db *sql.DB, rawToken, passwordHash string) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var (
		resetID int64
		userID  int64
	)
	err = tx.QueryRow(
		`SELECT id, user_id FROM password_resets
		 WHERE token_hash = ? AND used_at IS NULL AND expires_at > ?`,
		hashToken(rawToken), time.Now().Unix(),
	).Scan(&resetID, &userID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return errNotFound
		}
		return err
	}

	if _, err = tx.Exec(
		`UPDATE users SET password_hash = ?, must_change_pw = 0, active = 1, reset_requested_at = NULL
		 WHERE id = ?`, passwordHash, userID,
	); err != nil {
		return err
	}

	r, err := tx.Exec(
		`UPDATE password_resets SET used_at = ? WHERE id = ? AND used_at IS NULL`,
		time.Now().Unix(), resetID,
	)
	if err != nil {
		return err
	}
	if n, _ := r.RowsAffected(); n == 0 {
		return errNotFound // raced with another redemption
	}

	// Revoke existing sessions: a password reset should invalidate any live
	// login, and any token still outstanding for this user is now moot.
	if _, err = tx.Exec(`DELETE FROM auth_sessions WHERE user_id = ?`, userID); err != nil {
		return err
	}
	return tx.Commit()
}

func sweepResets(db *sql.DB) {
	_, _ = db.Exec(`DELETE FROM password_resets WHERE used_at IS NULL AND expires_at < ?`, time.Now().Unix())
}

// ───────────────────────── platform appliance tokens ──────────────────────
//
// Opaque tokens use AES-256-GCM(key, payload), key = SHA-256(secret), with a
// fresh 12-byte nonce prepended and the result base64url-encoded. The platform
// signs "SEC|payload" (see appliance.go for the SEC layout + parsing); ssoGCM
// and ssoNonceSize are the shared crypto primitives.

func ssoGCM(secret string) (cipher.AEAD, error) {
	key := sha256.Sum256([]byte(secret))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

const ssoNonceSize = 12

// ensureConnectSecret returns CONNECT_SECRET, generating one on first run.
//
// Config is treated as immutable: rather than write back to .env, a generated
// secret is persisted under DATA_DIR (the app's writable state directory) so it
// survives restarts. This keeps the config file read-only, which lets the
// systemd unit run hardened (ProtectSystem=strict, root-owned EnvironmentFile).
// The secret is also printed to the log so it can be pasted into the platform's
// integration settings.
func ensureConnectSecret() string {
	if v := strings.TrimSpace(os.Getenv("CONNECT_SECRET")); v != "" {
		return v
	}
	dataDir := os.Getenv("DATA_DIR")
	if dataDir == "" {
		dataDir = "data"
	}
	path := filepath.Join(dataDir, "connect_secret")

	// Reuse a previously generated secret so SSO links survive restarts.
	if b, err := os.ReadFile(path); err == nil {
		if s := strings.TrimSpace(string(b)); s != "" {
			os.Setenv("CONNECT_SECRET", s)
			return s
		}
	}

	secret := randToken()
	if err := os.MkdirAll(dataDir, 0o750); err != nil {
		log.Printf("[Auth] CONNECT_SECRET not set and %s not writable — using an ephemeral secret for this run", dataDir)
	} else if err := os.WriteFile(path, []byte(secret+"\n"), 0o600); err != nil {
		log.Printf("[Auth] CONNECT_SECRET not set and %s not writable — using an ephemeral secret for this run", path)
	} else {
		log.Printf("[Auth] generated CONNECT_SECRET and saved it to %s", path)
	}
	log.Printf("[Auth] CONNECT_SECRET: %s", secret)
	log.Printf("[Auth]     paste it into the platform's integration settings to enable sign-in SSO links")
	os.Setenv("CONNECT_SECRET", secret)
	return secret
}

// ───────────────────────── login throttle ──────────────────────────────────

type loginThrottle struct {
	mu    sync.Mutex
	fails map[string]*failRec
}

type failRec struct {
	count int
	until time.Time
}

const (
	throttleMax    = 5
	throttleWindow = 5 * time.Minute
)

func newLoginThrottle() *loginThrottle {
	return &loginThrottle{fails: make(map[string]*failRec)}
}

func (t *loginThrottle) blocked(key string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	r := t.fails[key]
	return r != nil && r.count >= throttleMax && time.Now().Before(r.until)
}

func (t *loginThrottle) fail(key string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	r := t.fails[key]
	if r == nil {
		r = &failRec{}
		t.fails[key] = r
	}
	r.count++
	r.until = time.Now().Add(throttleWindow)
}

func (t *loginThrottle) reset(key string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.fails, key)
}

// ───────────────────────── CSRF (pre-session forms) ────────────────────────
//
// Double-submit-cookie defense for forms that exist before a session does
// (login, invite redemption, forgot/reset). Authenticated forms use the
// per-session synchronizer token instead (see requireCSRF).

const csrfCookieName = "csrf"

// issueCSRF mints a token, sets it as a cookie, and returns the value to embed
// in a form's hidden field.
func issueCSRF(w http.ResponseWriter, secure bool) string {
	tok := randToken()
	http.SetCookie(w, &http.Cookie{
		Name:     csrfCookieName,
		Value:    tok,
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   3600,
	})
	return tok
}

// checkCSRF reports whether the submitted form token matches the cookie.
func checkCSRF(r *http.Request) bool {
	c, err := r.Cookie(csrfCookieName)
	if err != nil || c.Value == "" {
		return false
	}
	form := r.PostFormValue("csrf")
	if form == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(c.Value), []byte(form)) == 1
}

// ───────────────────────── session cookie ───────────────────────────────────
//
// The cookie value is base64url(ref) + "." + rawToken, so the server knows
// which tenant DB to look the session up in.

const sessionCookieName = "sid"

func encodeSessionCookie(ref, rawToken string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(ref)) + "." + rawToken
}

func decodeSessionCookie(v string) (ref, rawToken string, ok bool) {
	dot := strings.IndexByte(v, '.')
	if dot <= 0 || dot == len(v)-1 {
		return "", "", false
	}
	refBytes, err := base64.RawURLEncoding.DecodeString(v[:dot])
	if err != nil || len(refBytes) == 0 {
		return "", "", false
	}
	return string(refBytes), v[dot+1:], true
}

// ───────────────────────── templates ───────────────────────────────────────

// pageTemplates are the standalone page templates (each composed with
// layout.tmpl, whose "content" block they define).
var pageTemplates = []string{
	"login.tmpl",
	"invite_redeem.tmpl",
	"forgot.tmpl",
	"reset.tmpl",
	"force_change.tmpl",
	"users.tmpl",
}

type pageRenderer struct {
	pages map[string]*template.Template
}

func loadTemplates() (*pageRenderer, error) {
	t := &pageRenderer{pages: make(map[string]*template.Template)}
	for _, name := range pageTemplates {
		tmpl, err := template.ParseFS(templatesFS, "templates/layout.tmpl", "templates/"+name)
		if err != nil {
			return nil, err
		}
		t.pages[name] = tmpl
	}
	return t, nil
}

func (t *pageRenderer) render(w io.Writer, name string, data any) error {
	tmpl, ok := t.pages[name]
	if !ok {
		return fmt.Errorf("unknown template %q", name)
	}
	return tmpl.ExecuteTemplate(w, "layout.tmpl", data)
}

// ───────────────────────── authApp ──────────────────────────────────────────

// authApp holds dependencies and configuration for the auth area.
type authApp struct {
	tmpl       *pageRenderer
	secure     bool
	siteName   string
	primary    string // CSS color for the auth pages' accent
	favicon    string // favicon URL for the auth pages
	sessionTTL time.Duration
	inviteTTL  time.Duration
	resetTTL   time.Duration

	adminUsername  string
	adminInitialPW string
	ssoSecret      string   // shared secret for platform SSO links
	corsOrigins    []string // browser origins allowed to read /provision cross-origin
	throttle       *loginThrottle
	bootstrapped   sync.Map // ref -> true, once the tenant has been checked
}

func envHours(key string, def int) time.Duration {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return time.Duration(n) * time.Hour
		}
	}
	return time.Duration(def) * time.Hour
}

// newAuthApp builds the auth area from .env configuration and starts a
// background sweeper that prunes expired sessions/invites/resets hourly.
func newAuthApp() (*authApp, error) {
	tmpl, err := loadTemplates()
	if err != nil {
		return nil, err
	}
	primary := validCSSColor(os.Getenv("PRIMARY_COLOR"))
	if primary == "" {
		primary = "#2563eb"
	}
	siteName := strings.TrimSpace(os.Getenv("SITE_NAME"))
	if siteName == "" {
		siteName = "Live Support"
	}
	adminUsername := strings.TrimSpace(os.Getenv("ADMIN_USERNAME"))
	if adminUsername == "" {
		adminUsername = "admin"
	}
	// Same favicon override the dashboard pages use (FAVICON_URL, validated),
	// falling back to the bundled icon.
	favicon := validURLOrPath(os.Getenv("FAVICON_URL"))
	if favicon == "" {
		favicon = "/public/favicon.svg"
	}
	a := &authApp{
		tmpl:           tmpl,
		secure:         os.Getenv("SECURE_COOKIES") == "true",
		siteName:       siteName,
		primary:        primary,
		favicon:        favicon,
		sessionTTL:     envHours("SESSION_TTL_HOURS", 168),
		inviteTTL:      envHours("INVITE_TTL_HOURS", 72),
		resetTTL:       envHours("RESET_TTL_HOURS", 24),
		adminUsername:  adminUsername,
		adminInitialPW: strings.TrimSpace(os.Getenv("ADMIN_INITIAL_PASSWORD")),
		ssoSecret:      ensureConnectSecret(),
		corsOrigins:    splitCommaList(os.Getenv("CORS_ORIGINS")),
		throttle:       newLoginThrottle(),
	}
	go a.sweepLoop()
	return a, nil
}

// splitCommaList parses a comma-separated env value into trimmed, non-empty entries.
func splitCommaList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func (a *authApp) sweepLoop() {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for range t.C {
		for _, d := range dbs.all() {
			sweepAuthSessions(d)
			sweepInvites(d)
			sweepResets(d)
		}
	}
}

// bootstrapTenant seeds a single admin user the first time a tenant's user
// table is touched. With ADMIN_INITIAL_PASSWORD unset, a temporary password is
// generated and printed to the server log (and must be changed on first login).
func (a *authApp) bootstrapTenant(db *sql.DB, ref string) {
	if _, done := a.bootstrapped.Load(ref); done {
		return
	}
	n, err := countUsers(db)
	if err != nil {
		return // retried on the next visit
	}
	if n > 0 {
		a.bootstrapped.Store(ref, true)
		return
	}
	pw := a.adminInitialPW
	if pw == "" {
		pw = randToken()[:16]
		log.Printf("[Auth] %s: no ADMIN_INITIAL_PASSWORD set — generated a temporary one for %q:", ref, a.adminUsername)
		log.Printf("[Auth]     %s   (you must change it on first login)", pw)
	}
	hash, err := hashPassword(pw)
	if err != nil {
		return
	}
	if _, err := db.Exec(
		`INSERT INTO users(username, password_hash, role, must_change_pw, active, created_at)
		 VALUES(?, ?, ?, 1, 1, ?)`,
		a.adminUsername, hash, RoleAdmin, time.Now().Unix(),
	); err != nil {
		return
	}
	a.bootstrapped.Store(ref, true)
	log.Printf("[Auth] %s: created initial admin user %q", ref, a.adminUsername)
}

// Mount registers all auth/user-management routes onto mux.
func (a *authApp) Mount(mux *http.ServeMux) {
	// public (pre-session)
	mux.HandleFunc("GET /login", a.loginForm)
	mux.HandleFunc("POST /login", a.login)
	mux.HandleFunc("GET /invite", a.inviteRedeemForm)
	mux.HandleFunc("POST /invite", a.inviteRedeem)
	mux.HandleFunc("GET /forgot", a.forgotForm)
	mux.HandleFunc("POST /forgot", a.forgot)
	mux.HandleFunc("GET /reset", a.resetForm)
	mux.HandleFunc("POST /reset", a.reset)
	mux.HandleFunc("GET /sso", a.sso)
	// platform tenant provisioning (token-signed; returns an admin invite link)
	mux.HandleFunc("POST /provision", a.provision)

	// session required
	mux.Handle("POST /logout", a.authedCSRF(a.logout))
	mux.Handle("GET /account/password", a.authed(a.changePasswordForm))
	mux.Handle("POST /account/password", a.authedCSRF(a.changePassword))
	mux.Handle("GET /api/me", a.authedJSON(a.apiMe))

	// admin role required
	mux.Handle("GET /users", a.adminOnly(a.usersPage))
	mux.Handle("POST /users/invite", a.adminCSRF(a.createInvite))
	mux.Handle("POST /users/deactivate", a.adminCSRF(a.deactivate))
	mux.Handle("POST /users/reset", a.adminCSRF(a.resetUser))
	mux.Handle("POST /users/delete", a.adminCSRF(a.deleteUser))
}

// ---- request context ----

// authInfo carries the resolved tenant + session + user for a request.
type authInfo struct {
	ref  string
	db   *sql.DB
	user *User
	sess *AuthSession
}

type authCtxKey int

const authKey authCtxKey = 0

func authFrom(ctx context.Context) *authInfo {
	v, _ := ctx.Value(authKey).(*authInfo)
	return v
}

// sessionFromRequest resolves the session cookie to a tenant + user, or
// (nil, false) if absent/invalid. Used by middleware and by the /auth.html
// gate in main.go.
func (a *authApp) sessionFromRequest(r *http.Request) (*authInfo, bool) {
	c, err := r.Cookie(sessionCookieName)
	if err != nil || c.Value == "" {
		return nil, false
	}
	ref, raw, ok := decodeSessionCookie(c.Value)
	if !ok {
		return nil, false
	}
	db, err := dbs.get(ref)
	if err != nil {
		return nil, false
	}
	sess, user, err := lookupAuthSession(db, raw)
	if err != nil {
		return nil, false
	}
	return &authInfo{ref: ref, db: db, user: user, sess: sess}, true
}

// ---- middleware composition helpers ----

func (a *authApp) authed(h http.HandlerFunc) http.Handler {
	return a.requireAuth(h)
}

func (a *authApp) authedCSRF(h http.HandlerFunc) http.Handler {
	return a.requireAuth(a.requireCSRF(h))
}

func (a *authApp) adminOnly(h http.HandlerFunc) http.Handler {
	return a.requireAuth(a.requireRole(RoleAdmin, h))
}

func (a *authApp) adminCSRF(h http.HandlerFunc) http.Handler {
	return a.requireAuth(a.requireRole(RoleAdmin, a.requireCSRF(h)))
}

// requireAuth loads the session/user into context, redirecting to /login if
// absent and to the password-change page while a forced change is pending.
func (a *authApp) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		info, ok := a.sessionFromRequest(r)
		if !ok {
			a.clearSessionCookie(w)
			// Preserve the tenant on the login page when the (expired or
			// invalid) cookie still names one.
			target := "/login"
			if c, err := r.Cookie(sessionCookieName); err == nil {
				if ref, _, ok := decodeSessionCookie(c.Value); ok && ref != "" {
					target += "?ref=" + url.QueryEscape(ref)
				}
			}
			http.Redirect(w, r, target, http.StatusSeeOther)
			return
		}
		if info.user.MustChangePW && !passwordChangeAllowed(r.URL.Path) {
			http.Redirect(w, r, "/account/password", http.StatusSeeOther)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), authKey, info)))
	})
}

// authedJSON is requireAuth for API endpoints: it answers 401 JSON instead of
// redirecting, and lets a pending forced password change through (the caller
// gets a mustChangePassword flag and handles the redirect client-side).
func (a *authApp) authedJSON(h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		info, ok := a.sessionFromRequest(r)
		if !ok {
			errJSON(w, 401, "not signed in")
			return
		}
		h.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), authKey, info)))
	})
}

func (a *authApp) requireRole(role string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		info := authFrom(r.Context())
		if info == nil || (role == RoleAdmin && info.user.Role != RoleAdmin) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (a *authApp) requireCSRF(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		info := authFrom(r.Context())
		if info == nil || subtle.ConstantTimeCompare([]byte(r.PostFormValue("csrf")), []byte(info.sess.CSRF)) != 1 {
			http.Error(w, "invalid CSRF token", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func passwordChangeAllowed(path string) bool {
	return path == "/account/password" || path == "/logout" || path == "/api/me"
}

func (a *authApp) setSessionCookie(w http.ResponseWriter, ref, raw string) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    encodeSessionCookie(ref, raw),
		Path:     "/",
		HttpOnly: true,
		Secure:   a.secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(a.sessionTTL.Seconds()),
	})
}

func (a *authApp) clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookieName, Value: "", Path: "/",
		HttpOnly: true, Secure: a.secure, SameSite: http.SameSiteLaxMode, MaxAge: -1,
	})
}

// ---- rendering ----

// base is embedded in every auth page's view model so the layout can render
// consistently.
type base struct {
	SiteName     string
	PrimaryColor template.CSS
	FaviconURL   string
	Ref          string
	User         *User
	CSRF         string
	Wide         bool
}

func (a *authApp) publicBase(ref string) base {
	return base{SiteName: a.siteName, PrimaryColor: template.CSS(a.primary), FaviconURL: a.favicon, Ref: ref}
}

func (a *authApp) newBase(r *http.Request) base {
	b := base{SiteName: a.siteName, PrimaryColor: template.CSS(a.primary), FaviconURL: a.favicon}
	if info := authFrom(r.Context()); info != nil {
		b.Ref = info.ref
		b.User = info.user
		b.CSRF = info.sess.CSRF
	}
	return b
}

func (a *authApp) render(w http.ResponseWriter, status int, name string, data any) {
	var buf strings.Builder
	if err := a.tmpl.render(&buf, name, data); err != nil {
		log.Printf("[Auth] render %s: %v", name, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, buf.String())
}

// refFromRequest extracts the tenant ref from the query string or form body.
func refFromRequest(r *http.Request) string {
	ref := strings.TrimSpace(r.URL.Query().Get("ref"))
	if ref == "" {
		ref = strings.TrimSpace(r.PostFormValue("ref"))
	}
	return ref
}

// tenantDB resolves the ref to its tenant DB, returning nil if the ref is
// missing or invalid.
func tenantDB(ref string) *sql.DB {
	if ref == "" || safeRefFile(ref) == "" {
		return nil
	}
	db, err := dbs.get(ref)
	if err != nil {
		return nil
	}
	return db
}

// requestBaseURL reconstructs the externally-visible base URL for building
// absolute links (invites, resets). Honors X-Forwarded-* when present (the
// app is expected to run behind a TLS-terminating proxy in production).
func (a *authApp) requestBaseURL(r *http.Request) string {
	scheme := "http"
	if a.secure {
		scheme = "https"
	}
	host := r.Host
	if p := r.Header.Get("X-Forwarded-Proto"); p != "" {
		scheme = strings.TrimSpace(strings.Split(p, ",")[0])
	}
	if h := r.Header.Get("X-Forwarded-Host"); h != "" {
		host = strings.TrimSpace(strings.Split(h, ",")[0])
	}
	return scheme + "://" + host
}

// ───────────────────────── handlers: login / logout ────────────────────────

type loginView struct {
	base
	FormCSRF string
	Error    string
	// RefLocked hides the ref input when the tenant came in via ?ref=.
	RefLocked bool
}

func (a *authApp) loginForm(w http.ResponseWriter, r *http.Request) {
	ref := refFromRequest(r)
	if db := tenantDB(ref); db != nil {
		a.bootstrapTenant(db, ref)
	}
	a.render(w, http.StatusOK, "login.tmpl", loginView{
		base:      a.publicBase(ref),
		FormCSRF:  issueCSRF(w, a.secure),
		RefLocked: ref != "",
	})
}

func (a *authApp) login(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	ref := refFromRequest(r)
	renderErr := func(msg string) {
		a.render(w, http.StatusOK, "login.tmpl", loginView{
			base:      a.publicBase(ref),
			FormCSRF:  issueCSRF(w, a.secure),
			Error:     msg,
			RefLocked: r.URL.Query().Get("ref") != "",
		})
	}

	if !checkCSRF(r) {
		renderErr("Your session expired. Please try again.")
		return
	}
	db := tenantDB(ref)
	if db == nil {
		renderErr("Please enter a valid domain.")
		return
	}
	a.bootstrapTenant(db, ref)

	username := r.PostFormValue("username")
	password := r.PostFormValue("password")
	throttleKey := ref + "|" + username

	if a.throttle.blocked(throttleKey) {
		renderErr("Too many attempts. Please wait a few minutes and try again.")
		return
	}

	user, err := userByUsername(db, username)
	if err != nil {
		// keep timing uniform for unknown users
		verifyPassword(dummyHash, password)
		a.throttle.fail(throttleKey)
		renderErr("Invalid username or password.")
		return
	}
	if !user.Active || !verifyPassword(user.PasswordHash, password) {
		a.throttle.fail(throttleKey)
		renderErr("Invalid username or password.")
		return
	}

	a.throttle.reset(throttleKey)
	raw, _, err := createAuthSession(db, user.ID, a.sessionTTL)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	a.setSessionCookie(w, ref, raw)
	setLastLogin(db, user.ID)
	if user.MustChangePW {
		http.Redirect(w, r, "/account/password", http.StatusSeeOther)
		return
	}
	// Admins land on the Admin page (user management + console link); agents go
	// straight to the agent console.
	if user.Role == RoleAdmin {
		http.Redirect(w, r, "/users", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/auth.html", http.StatusSeeOther)
}

func (a *authApp) logout(w http.ResponseWriter, r *http.Request) {
	info := authFrom(r.Context())
	if c, err := r.Cookie(sessionCookieName); err == nil {
		if _, raw, ok := decodeSessionCookie(c.Value); ok {
			deleteAuthSession(info.db, raw)
		}
	}
	// "Available until Pause or log out" — logging out ends durable
	// availability, so guests stop seeing (and ringing) this agent.
	clearAvailability(info.db, info.user.ID)
	a.clearSessionCookie(w)
	http.Redirect(w, r, "/login?ref="+url.QueryEscape(info.ref), http.StatusSeeOther)
}

// sso signs a user in via a platform-minted token (see the SSO token
// section above). Unknown usernames are auto-provisioned in the tenant with an
// empty password hash — such accounts can only ever enter through SSO
// links (password verification always fails on an empty hash) until an admin
// issues a reset link to set a real password.
func (a *authApp) sso(w http.ResponseWriter, r *http.Request) {
	failed := func() {
		a.render(w, http.StatusOK, "login.tmpl", loginView{
			base:     a.publicBase(""),
			FormCSRF: issueCSRF(w, a.secure),
			Error:    "This sign-in link is invalid or has expired. Please sign in below, or return to your admin dashboard and try again.",
		})
	}

	tok := r.URL.Query().Get("t")
	if a.ssoSecret == "" || tok == "" {
		failed()
		return
	}
	sec, _, err := parseAppliance(a.ssoSecret, tok)
	if err != nil || sec.User == "" {
		failed()
		return
	}
	ref, username, role := sec.Ref, sec.User, sec.Role
	now := time.Now().Unix()
	db := tenantDB(ref)
	if db == nil {
		failed()
		return
	}
	a.bootstrapTenant(db, ref)

	user, err := userByUsername(db, username)
	if errors.Is(err, errNotFound) {
		if role != RoleAdmin {
			role = RoleAgent
		}
		res, ierr := db.Exec(
			`INSERT INTO users(username, password_hash, role, must_change_pw, active, created_at)
			 VALUES(?, '', ?, 0, 1, ?)`,
			username, role, now,
		)
		if ierr != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		id, _ := res.LastInsertId()
		user, err = userByID(db, id)
	}
	if err != nil || !user.Active {
		failed()
		return
	}

	raw, _, err := createAuthSession(db, user.ID, a.sessionTTL)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	a.setSessionCookie(w, ref, raw)
	setLastLogin(db, user.ID)
	http.Redirect(w, r, "/auth.html", http.StatusSeeOther)
}

// ───────────────────────── handlers: change password ───────────────────────

type changePWView struct {
	base
	Forced bool
	Error  string
}

func (a *authApp) changePasswordForm(w http.ResponseWriter, r *http.Request) {
	info := authFrom(r.Context())
	a.render(w, http.StatusOK, "force_change.tmpl", changePWView{
		base:   a.newBase(r),
		Forced: info != nil && info.user.MustChangePW,
	})
}

func (a *authApp) changePassword(w http.ResponseWriter, r *http.Request) {
	info := authFrom(r.Context())
	_ = r.ParseForm()
	renderErr := func(msg string) {
		a.render(w, http.StatusOK, "force_change.tmpl", changePWView{
			base:   a.newBase(r),
			Forced: info.user.MustChangePW,
			Error:  msg,
		})
	}

	current := r.PostFormValue("current")
	next := r.PostFormValue("new")
	confirm := r.PostFormValue("confirm")

	if !verifyPassword(info.user.PasswordHash, current) {
		renderErr("Your current password is incorrect.")
		return
	}
	if len(next) < minPasswordLen {
		renderErr(fmt.Sprintf("New password must be at least %d characters.", minPasswordLen))
		return
	}
	if next != confirm {
		renderErr("The new passwords don't match.")
		return
	}
	hash, err := hashPassword(next)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if err := setPassword(info.db, info.user.ID, hash); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/auth.html", http.StatusSeeOther)
}

// ───────────────────────── handlers: invites ───────────────────────────────

type inviteView struct {
	base
	FormCSRF       string
	Token          string
	Username       string
	PresetUsername bool
	Error          string
	Invalid        bool
}

func (a *authApp) inviteRedeemForm(w http.ResponseWriter, r *http.Request) {
	ref := refFromRequest(r)
	tok := r.URL.Query().Get("t")
	db := tenantDB(ref)
	if db == nil {
		a.render(w, http.StatusOK, "invite_redeem.tmpl", inviteView{base: a.publicBase(ref), Invalid: true})
		return
	}
	inv, err := inviteByToken(db, tok)
	if err != nil {
		a.render(w, http.StatusOK, "invite_redeem.tmpl", inviteView{base: a.publicBase(ref), Invalid: true})
		return
	}
	a.render(w, http.StatusOK, "invite_redeem.tmpl", inviteView{
		base:           a.publicBase(ref),
		FormCSRF:       issueCSRF(w, a.secure),
		Token:          tok,
		Username:       inv.Username.String,
		PresetUsername: inv.Username.Valid && inv.Username.String != "",
	})
}

func (a *authApp) inviteRedeem(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	ref := refFromRequest(r)
	tok := r.URL.Query().Get("t")
	if tok == "" {
		tok = r.PostFormValue("t")
	}

	db := tenantDB(ref)
	if db == nil {
		a.render(w, http.StatusOK, "invite_redeem.tmpl", inviteView{base: a.publicBase(ref), Invalid: true})
		return
	}
	inv, err := inviteByToken(db, tok)
	if err != nil {
		a.render(w, http.StatusOK, "invite_redeem.tmpl", inviteView{base: a.publicBase(ref), Invalid: true})
		return
	}

	username := inv.Username.String
	if !(inv.Username.Valid && inv.Username.String != "") {
		username = strings.TrimSpace(r.PostFormValue("username"))
	}
	password := r.PostFormValue("new")
	confirm := r.PostFormValue("confirm")

	rerender := func(msg string) {
		a.render(w, http.StatusOK, "invite_redeem.tmpl", inviteView{
			base:           a.publicBase(ref),
			FormCSRF:       issueCSRF(w, a.secure),
			Token:          tok,
			Username:       username,
			PresetUsername: inv.Username.Valid && inv.Username.String != "",
			Error:          msg,
		})
	}

	if !checkCSRF(r) {
		rerender("Your session expired. Please try again.")
		return
	}
	if username == "" {
		rerender("Please choose a username.")
		return
	}
	if len(password) < minPasswordLen {
		rerender(fmt.Sprintf("Password must be at least %d characters.", minPasswordLen))
		return
	}
	if password != confirm {
		rerender("The passwords don't match.")
		return
	}
	hash, err := hashPassword(password)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	switch err := redeemInvite(db, inv, username, hash); {
	case err == nil:
		http.Redirect(w, r, "/login?ref="+url.QueryEscape(ref), http.StatusSeeOther)
	case errors.Is(err, errUsernameTaken):
		rerender("That username is already taken.")
	case errors.Is(err, errNotFound):
		a.render(w, http.StatusOK, "invite_redeem.tmpl", inviteView{base: a.publicBase(ref), Invalid: true})
	default:
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

// ───────────────────────── handlers: forgot / reset ────────────────────────

type forgotView struct {
	base
	FormCSRF string
	Sent     bool
	Error    string
}

func (a *authApp) forgotForm(w http.ResponseWriter, r *http.Request) {
	ref := refFromRequest(r)
	a.render(w, http.StatusOK, "forgot.tmpl", forgotView{
		base:     a.publicBase(ref),
		FormCSRF: issueCSRF(w, a.secure),
	})
}

func (a *authApp) forgot(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	ref := refFromRequest(r)
	if !checkCSRF(r) {
		a.render(w, http.StatusOK, "forgot.tmpl", forgotView{
			base:     a.publicBase(ref),
			FormCSRF: issueCSRF(w, a.secure),
			Error:    "Your session expired. Please try again.",
		})
		return
	}
	username := strings.TrimSpace(r.PostFormValue("username"))
	if db := tenantDB(ref); db != nil && username != "" {
		// Best-effort: a missing username is a no-op inside the store call.
		_ = requestPasswordReset(db, username)
	}
	// Always the same response, whether or not the account exists, so the page
	// can't be used to enumerate usernames.
	a.render(w, http.StatusOK, "forgot.tmpl", forgotView{
		base: a.publicBase(ref),
		Sent: true,
	})
}

type resetView struct {
	base
	FormCSRF string
	Token    string
	Error    string
	Invalid  bool
}

func (a *authApp) resetForm(w http.ResponseWriter, r *http.Request) {
	ref := refFromRequest(r)
	tok := r.URL.Query().Get("t")
	db := tenantDB(ref)
	if db == nil {
		a.render(w, http.StatusOK, "reset.tmpl", resetView{base: a.publicBase(ref), Invalid: true})
		return
	}
	if _, err := resetByToken(db, tok); err != nil {
		a.render(w, http.StatusOK, "reset.tmpl", resetView{base: a.publicBase(ref), Invalid: true})
		return
	}
	a.render(w, http.StatusOK, "reset.tmpl", resetView{
		base:     a.publicBase(ref),
		FormCSRF: issueCSRF(w, a.secure),
		Token:    tok,
	})
}

func (a *authApp) reset(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	ref := refFromRequest(r)
	tok := r.URL.Query().Get("t")
	if tok == "" {
		tok = r.PostFormValue("t")
	}

	db := tenantDB(ref)
	if db == nil {
		a.render(w, http.StatusOK, "reset.tmpl", resetView{base: a.publicBase(ref), Invalid: true})
		return
	}
	if _, err := resetByToken(db, tok); err != nil {
		a.render(w, http.StatusOK, "reset.tmpl", resetView{base: a.publicBase(ref), Invalid: true})
		return
	}

	rerender := func(msg string) {
		a.render(w, http.StatusOK, "reset.tmpl", resetView{
			base:     a.publicBase(ref),
			FormCSRF: issueCSRF(w, a.secure),
			Token:    tok,
			Error:    msg,
		})
	}

	if !checkCSRF(r) {
		rerender("Your session expired. Please try again.")
		return
	}
	password := r.PostFormValue("new")
	confirm := r.PostFormValue("confirm")
	if len(password) < minPasswordLen {
		rerender(fmt.Sprintf("Password must be at least %d characters.", minPasswordLen))
		return
	}
	if password != confirm {
		rerender("The passwords don't match.")
		return
	}
	hash, err := hashPassword(password)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	switch err := redeemReset(db, tok, hash); {
	case err == nil:
		http.Redirect(w, r, "/login?ref="+url.QueryEscape(ref), http.StatusSeeOther)
	case errors.Is(err, errNotFound):
		a.render(w, http.StatusOK, "reset.tmpl", resetView{base: a.publicBase(ref), Invalid: true})
	default:
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

// ───────────────────────── handlers: user management ───────────────────────

type usersView struct {
	base
	Users      []User
	InviteLink string
	ResetLink  string
	Error      string
}

func (a *authApp) usersPage(w http.ResponseWriter, r *http.Request) {
	a.renderUsers(w, r, "", "")
}

func (a *authApp) renderUsers(w http.ResponseWriter, r *http.Request, inviteLink, errMsg string) {
	a.renderUsersView(w, r, usersView{InviteLink: inviteLink, Error: errMsg})
}

// renderUsersWithReset re-renders the page showing a freshly minted reset link.
func (a *authApp) renderUsersWithReset(w http.ResponseWriter, r *http.Request, resetLink string) {
	a.renderUsersView(w, r, usersView{ResetLink: resetLink})
}

func (a *authApp) renderUsersView(w http.ResponseWriter, r *http.Request, v usersView) {
	info := authFrom(r.Context())
	users, err := listUsers(info.db)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	v.base = a.newBase(r)
	v.base.Wide = true
	v.Users = users
	a.render(w, http.StatusOK, "users.tmpl", v)
}

func (a *authApp) createInvite(w http.ResponseWriter, r *http.Request) {
	info := authFrom(r.Context())
	role := r.PostFormValue("role")
	if role != RoleAdmin && role != RoleAgent {
		role = RoleAgent
	}
	username := strings.TrimSpace(r.PostFormValue("username"))

	raw, err := createInviteRow(info.db, role, username, info.user.ID, a.inviteTTL)
	if err != nil {
		a.renderUsers(w, r, "", "Could not create invite. Please try again.")
		return
	}
	link := a.requestBaseURL(r) + "/invite?ref=" + url.QueryEscape(info.ref) + "&t=" + raw
	a.renderUsers(w, r, link, "")
}

func (a *authApp) deactivate(w http.ResponseWriter, r *http.Request) {
	info := authFrom(r.Context())
	id, err := strconv.ParseInt(r.PostFormValue("user_id"), 10, 64)
	if err != nil {
		http.Redirect(w, r, "/users", http.StatusSeeOther)
		return
	}
	if id == info.user.ID {
		a.renderUsers(w, r, "", "You can't deactivate your own account.")
		return
	}
	target, err := userByID(info.db, id)
	if err != nil {
		http.Redirect(w, r, "/users", http.StatusSeeOther)
		return
	}
	if target.Role == RoleAdmin && activeAdminCount(info.db) <= 1 {
		a.renderUsers(w, r, "", "You can't deactivate the last active admin.")
		return
	}
	if err := deactivateUser(info.db, id); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/users", http.StatusSeeOther)
}

// resetUser mints a reset link an admin can hand to a user who has lost access.
func (a *authApp) resetUser(w http.ResponseWriter, r *http.Request) {
	info := authFrom(r.Context())
	id, err := strconv.ParseInt(r.PostFormValue("user_id"), 10, 64)
	if err != nil {
		http.Redirect(w, r, "/users", http.StatusSeeOther)
		return
	}
	if _, err := userByID(info.db, id); err != nil {
		http.Redirect(w, r, "/users", http.StatusSeeOther)
		return
	}
	raw, err := createResetRow(info.db, id, info.user.ID, a.resetTTL)
	if err != nil {
		a.renderUsers(w, r, "", "Could not create a reset link. Please try again.")
		return
	}
	link := a.requestBaseURL(r) + "/reset?ref=" + url.QueryEscape(info.ref) + "&t=" + raw
	a.renderUsersWithReset(w, r, link)
}

// deleteUser permanently removes an account (and frees its username for reuse).
func (a *authApp) deleteUser(w http.ResponseWriter, r *http.Request) {
	info := authFrom(r.Context())
	id, err := strconv.ParseInt(r.PostFormValue("user_id"), 10, 64)
	if err != nil {
		http.Redirect(w, r, "/users", http.StatusSeeOther)
		return
	}
	if id == info.user.ID {
		a.renderUsers(w, r, "", "You can't delete your own account.")
		return
	}
	target, err := userByID(info.db, id)
	if err != nil {
		http.Redirect(w, r, "/users", http.StatusSeeOther)
		return
	}
	if target.Role == RoleAdmin && adminCount(info.db) <= 1 {
		a.renderUsers(w, r, "", "You can't delete the last admin account.")
		return
	}
	if err := removeUser(info.db, id); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/users", http.StatusSeeOther)
}

// ───────────────────────── handlers: /api/me ───────────────────────────────

// apiMe reports the signed-in identity to the dashboard frontend (auth.js).
func (a *authApp) apiMe(w http.ResponseWriter, r *http.Request) {
	info := authFrom(r.Context())
	email := ""
	if strings.Contains(info.user.Username, "@") {
		email = info.user.Username
	}
	writeJSON(w, 200, map[string]any{
		"ref":                info.ref,
		"userId":             info.user.ID,
		"username":           info.user.Username,
		"name":               info.user.Username,
		"email":              email,
		"isAdmin":            info.user.Role == RoleAdmin,
		"mustChangePassword": info.user.MustChangePW,
		"csrf":               info.sess.CSRF,
	})
}
