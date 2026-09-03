package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// ---------------------------------------------------------------------------
// Admin credentials & sessions
// ---------------------------------------------------------------------------
//
// Lantern's original auth surface was env-only: LANTERN_AUTH_USER/PASS for
// Basic Auth and LANTERN_AUTH_TOKEN for a Bearer token. That cannot support
// changing your own password (env is read-only at runtime, and a container
// restart reverts anything written), and it cannot authenticate a WebSocket:
// a browser's WebSocket constructor accepts no headers, so /ws could never
// carry an Authorization header. Cookie-backed sessions solve both — the
// handshake sends cookies automatically.
//
// The store is opt-in. With no row in admin_credentials, authRequired() is
// false and the gate is inert, so an existing deployment keeps working
// exactly as before rather than locking its owner out.

const (
	sessionCookieName = "lantern_session"
	sessionTTL        = 30 * 24 * time.Hour
	sessionTokenBytes = 32

	// bcrypt's default cost (10) is ~50ms on this class of hardware, which is
	// the right trade for a login that a human performs by hand.
	bcryptCost = bcrypt.DefaultCost

	maxLoginFailures = 5
	loginLockout     = 15 * time.Minute
)

const authSchema = `
CREATE TABLE IF NOT EXISTS admin_credentials (
    id            INTEGER PRIMARY KEY CHECK (id = 1),
    username      TEXT    NOT NULL,
    password_hash TEXT    NOT NULL,
    updated_at    INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS sessions (
    token_hash TEXT    PRIMARY KEY,
    username   TEXT    NOT NULL,
    created_at INTEGER NOT NULL,
    expires_at INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_sessions_expires ON sessions(expires_at);
`

// sessionUserKey carries the authenticated username down to handlers that
// need to know who is acting — the credential change endpoint in particular.
const sessionUserKey contextKey = "session_user"

// credentialsPresent mirrors "admin_credentials has a row" so the middleware
// doesn't hit SQLite on every request just to learn whether to gate. It is
// refreshed at boot and after any credential write.
var credentialsPresent atomic.Bool

// authRequired reports whether the login gate is active.
func authRequired() bool { return credentialsPresent.Load() }

// refreshCredentialState re-reads whether an admin row exists.
func refreshCredentialState(db *sql.DB) {
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM admin_credentials WHERE id = 1`).Scan(&n); err != nil {
		log.Printf("auth: could not read credential state: %v", err)
		return
	}
	credentialsPresent.Store(n > 0)
}

// initAuth applies the auth schema, seeds credentials from the environment on
// first boot, and primes the cached credential state.
func initAuth(db *sql.DB, cfg *Config) {
	if _, err := db.Exec(authSchema); err != nil {
		log.Fatalf("failed to apply auth schema: %v", err)
	}
	purgeExpiredSessions(db)

	// Bootstrap: LANTERN_AUTH_USER/PASS seed the store the first time only.
	// Once the row exists it is authoritative, so a credential change made in
	// the UI is not silently reverted by a stale env var on the next restart.
	if cfg.AuthUser != "" && cfg.AuthPass != "" {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM admin_credentials WHERE id = 1`).Scan(&n); err == nil && n == 0 {
			if err := writeCredentials(db, cfg.AuthUser, cfg.AuthPass); err != nil {
				log.Printf("auth: failed to seed credentials from environment: %v", err)
			} else {
				log.Printf("auth: seeded admin credentials for %q from LANTERN_AUTH_USER", cfg.AuthUser)
			}
		}
	}
	refreshCredentialState(db)
	if authRequired() {
		log.Printf("auth: login gate active — /status and /api/public/* remain open")
	}
}

// writeCredentials hashes pass and upserts the single admin row.
func writeCredentials(db *sql.DB, user, pass string) error {
	hash, err := bcrypt.GenerateFromPassword([]byte(pass), bcryptCost)
	if err != nil {
		return err
	}
	_, err = db.Exec(`
INSERT INTO admin_credentials (id, username, password_hash, updated_at)
VALUES (1, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET username = excluded.username,
                              password_hash = excluded.password_hash,
                              updated_at = excluded.updated_at`,
		user, string(hash), time.Now().Unix())
	return err
}

// loadCredentials returns the stored username and hash.
func loadCredentials(db *sql.DB) (user, hash string, ok bool) {
	err := db.QueryRow(`SELECT username, password_hash FROM admin_credentials WHERE id = 1`).Scan(&user, &hash)
	if err != nil {
		return "", "", false
	}
	return user, hash, true
}

// verifyCredentials checks a username/password pair against the store. The
// username is compared in constant time and the password always runs through
// bcrypt, so a wrong username costs the same as a wrong password and the
// response time leaks neither.
func verifyCredentials(db *sql.DB, user, pass string) bool {
	storedUser, storedHash, ok := loadCredentials(db)
	if !ok {
		return false
	}
	userOK := subtle.ConstantTimeCompare([]byte(user), []byte(storedUser)) == 1
	passOK := bcrypt.CompareHashAndPassword([]byte(storedHash), []byte(pass)) == nil
	return userOK && passOK
}

// ---------------------------------------------------------------------------
// Sessions
// ---------------------------------------------------------------------------

// hashSessionToken is what actually lands in SQLite. The plaintext token only
// ever exists in the cookie, so a stolen database yields no usable session.
func hashSessionToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// createSession mints a token, stores its hash, and returns the plaintext for
// the cookie along with the expiry.
func createSession(db *sql.DB, username string) (string, time.Time, error) {
	buf := make([]byte, sessionTokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", time.Time{}, err
	}
	raw := hex.EncodeToString(buf)
	now := time.Now()
	exp := now.Add(sessionTTL)
	_, err := db.Exec(`INSERT INTO sessions (token_hash, username, created_at, expires_at) VALUES (?, ?, ?, ?)`,
		hashSessionToken(raw), username, now.Unix(), exp.Unix())
	if err != nil {
		return "", time.Time{}, err
	}
	return raw, exp, nil
}

// sessionUser resolves a request's session cookie to a username.
func sessionUser(db *sql.DB, r *http.Request) (string, bool) {
	c, err := r.Cookie(sessionCookieName)
	if err != nil || c.Value == "" {
		return "", false
	}
	var username string
	var expires int64
	err = db.QueryRow(`SELECT username, expires_at FROM sessions WHERE token_hash = ?`,
		hashSessionToken(c.Value)).Scan(&username, &expires)
	if err != nil {
		return "", false
	}
	if time.Now().Unix() > expires {
		_, _ = db.Exec(`DELETE FROM sessions WHERE token_hash = ?`, hashSessionToken(c.Value))
		return "", false
	}
	return username, true
}

func revokeSession(db *sql.DB, raw string) {
	_, _ = db.Exec(`DELETE FROM sessions WHERE token_hash = ?`, hashSessionToken(raw))
}

// revokeAllSessions logs out every device. Called whenever credentials change.
func revokeAllSessions(db *sql.DB) error {
	_, err := db.Exec(`DELETE FROM sessions`)
	return err
}

func purgeExpiredSessions(db *sql.DB) {
	if _, err := db.Exec(`DELETE FROM sessions WHERE expires_at < ?`, time.Now().Unix()); err != nil {
		log.Printf("auth: session purge failed: %v", err)
	}
}

// requestIsTLS reports whether the original client request used HTTPS, so the
// Secure attribute is set when it means something and omitted when it would
// stop the cookie working over a plain-HTTP homelab address.
func requestIsTLS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

func setSessionCookie(w http.ResponseWriter, r *http.Request, raw string, exp time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    raw,
		Path:     "/",
		Expires:  exp,
		MaxAge:   int(time.Until(exp).Seconds()),
		HttpOnly: true,
		// Strict is the CSRF defense: CORS here is AllowedOrigins:["*"], so a
		// cross-site form post would otherwise ride this cookie.
		SameSite: http.SameSiteStrictMode,
		Secure:   requestIsTLS(r),
	})
}

func clearSessionCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   requestIsTLS(r),
	})
}

// lookupScopedToken resolves a per-service bearer token to its service.
//
// Session tokens have always been stored as a SHA-256 hash, but api_tokens
// held the bearer verbatim, so anyone who obtained the database file — a
// backup, a stray volume copy — got working credentials rather than hashes.
// Rows are matched by hash first and upgraded in place the first time a
// plaintext row is used, which keeps the existing "insert a row with sqlite3"
// workflow working while draining the plaintext out of the table.
//
// The plaintext fallback is what makes the migration lazy rather than a schema
// rewrite; it is also the reason this is a reduction of exposure over time and
// not an immediate elimination of it. A row never used again stays plaintext.
func lookupScopedToken(db *sql.DB, token string) (string, bool) {
	hashed := hashSessionToken(token)

	var serviceName string
	if err := db.QueryRow(
		`SELECT service_name FROM api_tokens WHERE token = ?`, hashed).Scan(&serviceName); err == nil {
		return serviceName, true
	}

	if err := db.QueryRow(
		`SELECT service_name FROM api_tokens WHERE token = ?`, token).Scan(&serviceName); err != nil {
		return "", false
	}

	// Upgrade this row now that we know the plaintext. A conflicting hashed
	// row would mean the same token exists twice; leaving the plaintext row
	// alone in that case is harmless and keeps the token working.
	if _, err := db.Exec(
		`UPDATE api_tokens SET token = ? WHERE token = ?`, hashed, token); err != nil {
		log.Printf("auth: could not hash stored API token for %q: %v", serviceName, err)
	}
	return serviceName, true
}

// ---------------------------------------------------------------------------
// Login throttle
// ---------------------------------------------------------------------------

// loginThrottle slows password guessing per client address. It keys on
// RemoteAddr rather than X-Forwarded-For, which a caller can set freely and
// would therefore make the limit trivial to sidestep. Behind a reverse proxy
// this throttles the proxy as a whole; that is the safe direction to err.
type loginThrottle struct {
	mu      sync.Mutex
	entries map[string]*throttleEntry
}

type throttleEntry struct {
	failures int
	until    time.Time
	// lastSeen drives eviction. Without it the map only ever shrank on a
	// successful login or an expired lockout, so a source that failed once or
	// twice and never returned stayed forever — and an attacker rotating source
	// addresses grew it without bound.
	lastSeen time.Time
}

// throttleEntryTTL is how long an entry outlives its last attempt. Comfortably
// longer than a lockout, so eviction never cuts one short.
const throttleEntryTTL = 2 * loginLockout

// sweepLocked drops entries nothing has touched recently. Called from the write
// paths, so it costs nothing when no one is logging in. Caller holds t.mu.
func (t *loginThrottle) sweepLocked(now time.Time) {
	for k, e := range t.entries {
		if !e.until.IsZero() && now.Before(e.until) {
			continue // an active lockout is never evicted
		}
		if now.Sub(e.lastSeen) > throttleEntryTTL {
			delete(t.entries, k)
		}
	}
}

func newLoginThrottle() *loginThrottle {
	return &loginThrottle{entries: make(map[string]*throttleEntry)}
}

func throttleKey(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// blocked reports whether the caller is locked out, and for how long.
func (t *loginThrottle) blocked(r *http.Request) (bool, time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	e := t.entries[throttleKey(r)]
	if e == nil || e.until.IsZero() {
		return false, 0
	}
	if d := time.Until(e.until); d > 0 {
		return true, d
	}
	// Lockout elapsed: reset so the next attempt starts from a clean slate.
	delete(t.entries, throttleKey(r))
	return false, 0
}

func (t *loginThrottle) fail(r *http.Request) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	t.sweepLocked(now)

	k := throttleKey(r)
	e := t.entries[k]
	if e == nil {
		e = &throttleEntry{}
		t.entries[k] = e
	}
	e.lastSeen = now
	e.failures++
	if e.failures >= maxLoginFailures {
		e.until = now.Add(loginLockout)
		e.failures = 0
	}
}

func (t *loginThrottle) succeed(r *http.Request) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.entries, throttleKey(r))
}

// ---------------------------------------------------------------------------
// Middleware
// ---------------------------------------------------------------------------

// authExemptPath reports whether a path is reachable with no credential at all.
//
// Two families qualify. The public surface — /api/public/*, /api/badge/*,
// /metrics — is deliberately open and predates the login gate. The login
// surface — /api/auth/session, /api/auth/login and the static shell — has to
// be reachable or there is nothing to render the login form into.
//
// Serving the shell is what keeps /status public once credentials are set: the
// old middleware wrapped the SPA catch-all too, so enabling auth silently took
// the public status page offline. The shell carries no service data of its
// own; every byte of that arrives over the gated /api routes.
func authExemptPath(p string) bool {
	switch {
	case strings.HasPrefix(p, "/api/public/"), strings.HasPrefix(p, "/api/badge/"):
		return true
	case p == "/metrics":
		return true
	// Always open and documented as such. /api/health in particular is what
	// the container HEALTHCHECK polls with wget, which sends no credentials —
	// gating it marks the container unhealthy the moment auth is switched on.
	// Neither route exposes service data: health returns a status and version,
	// docs is a static route reference.
	case p == "/api/health", p == "/api/docs":
		return true
	case p == "/api/auth/session", p == "/api/auth/login":
		return true
	case p == "/ws":
		return false
	case strings.HasPrefix(p, "/api/"):
		return false
	}
	return true // static shell
}

// authMiddleware resolves a request to admin, service-scoped, or anonymous.
//
// Order is cheapest-and-commonest first: the browser's session cookie, then
// the Bearer tokens that scripts and CI use, then Basic Auth. Every
// credential comparison is constant-time or bcrypt; none uses ==.
func authMiddleware(db *sql.DB, cfg *Config, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if authExemptPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}

		// 1. Session cookie. Also the only credential a browser can attach to
		//    a WebSocket handshake, which is what makes /ws work under auth.
		if user, ok := sessionUser(db, r); ok {
			ctx := context.WithValue(r.Context(), isAdminKey, true)
			ctx = context.WithValue(ctx, sessionUserKey, user)
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		}

		// 2. Bearer: the admin-wide token, then per-service scoped tokens.
		authHeader := r.Header.Get("Authorization")
		if strings.HasPrefix(authHeader, "Bearer ") {
			token := strings.TrimPrefix(authHeader, "Bearer ")

			if cfg.AuthToken != "" &&
				subtle.ConstantTimeCompare([]byte(token), []byte(cfg.AuthToken)) == 1 {
				ctx := context.WithValue(r.Context(), isAdminKey, true)
				next.ServeHTTP(w, r.WithContext(ctx))
				return
			}

			if serviceName, ok := lookupScopedToken(db, token); ok {
				// A token scoped to one service authenticates successfully
				// here, but must not be allowed to pass as a credential on
				// routes that reach every service or the admin account
				// itself (backup, webhook URLs, config export/import, the
				// audit log). Without this, a token minted for "webapp"
				// could download the full database snapshot — credential
				// hash, session hashes, and every other service's token.
				if isAdminOnlyEndpoint(r) {
					writeError(w, http.StatusForbidden, "token not permitted for this endpoint")
					return
				}
				ctx := context.WithValue(r.Context(), scopedServiceKey, serviceName)
				next.ServeHTTP(w, r.WithContext(ctx))
				return
			}
		}

		// 3. Basic Auth, validated against the store once it is seeded and
		//    against the env pair otherwise, so curl -u keeps working.
		if user, pass, ok := r.BasicAuth(); ok {
			valid := false
			if authRequired() {
				valid = verifyCredentials(db, user, pass)
			} else if cfg.AuthEnabled {
				valid = subtle.ConstantTimeCompare([]byte(user), []byte(cfg.AuthUser)) == 1 &&
					subtle.ConstantTimeCompare([]byte(pass), []byte(cfg.AuthPass)) == 1
			}
			if valid {
				ctx := context.WithValue(r.Context(), isAdminKey, true)
				ctx = context.WithValue(ctx, sessionUserKey, user)
				next.ServeHTTP(w, r.WithContext(ctx))
				return
			}
		}

		// 4. Nothing authenticated. What that costs depends on the mode.
		switch {
		case authRequired():
			// Full gate: every non-exempt route needs a session.
			w.Header().Set("WWW-Authenticate", `Basic realm="Lantern"`)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
		case cfg.AuthEnabled:
			w.Header().Set("WWW-Authenticate", `Basic realm="Lantern"`)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
		case cfg.AuthToken != "" && isProtectedEndpoint(r):
			w.Header().Set("WWW-Authenticate", `Bearer`)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
		default:
			// No credentials configured anywhere: the dashboard is open, which
			// is Lantern's out-of-the-box behavior.
			next.ServeHTTP(w, r)
		}
	})
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

type authSessionResponse struct {
	AuthRequired  bool   `json:"auth_required"`
	Authenticated bool   `json:"authenticated"`
	Username      string `json:"username,omitempty"`
	// TokenMode reports the legacy LANTERN_AUTH_TOKEN setup, where there is no
	// username/password to type and the dashboard must not show a login wall.
	TokenMode bool `json:"token_mode"`
	// CanSetup is true when no credentials exist yet, so the Settings form can
	// offer first-time setup instead of asking for a current password.
	CanSetup bool `json:"can_setup"`
}

// handleGetAuthSession tells the frontend whether to render the login gate.
// Deliberately unauthenticated: it reports only whether auth is on and who (if
// anyone) the caller currently is.
func handleGetAuthSession(db *sql.DB, cfg *Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		resp := authSessionResponse{
			AuthRequired: authRequired(),
			TokenMode:    !authRequired() && cfg.AuthToken != "",
			CanSetup:     !authRequired(),
		}
		if user, ok := sessionUser(db, r); ok {
			resp.Authenticated = true
			resp.Username = user
		}
		writeJSON(w, http.StatusOK, resp)
	}
}

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func handlePostLogin(db *sql.DB, throttle *loginThrottle) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if blocked, wait := throttle.blocked(r); blocked {
			w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
			writeError(w, http.StatusTooManyRequests,
				"Too many failed attempts. Try again in "+wait.Round(time.Second).String()+".")
			return
		}
		if !authRequired() {
			writeError(w, http.StatusBadRequest, "No credentials are configured on this server.")
			return
		}

		var req loginRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "Malformed request body.")
			return
		}
		if !verifyCredentials(db, req.Username, req.Password) {
			throttle.fail(r)
			writeError(w, http.StatusUnauthorized, "Invalid credentials")
			return
		}

		raw, exp, err := createSession(db, req.Username)
		if err != nil {
			log.Printf("auth: could not create session: %v", err)
			writeError(w, http.StatusInternalServerError, "Could not start a session.")
			return
		}
		throttle.succeed(r)
		setSessionCookie(w, r, raw, exp)
		writeJSON(w, http.StatusOK, map[string]any{"authenticated": true, "username": req.Username})
	}
}

func handlePostLogout(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie(sessionCookieName); err == nil && c.Value != "" {
			revokeSession(db, c.Value)
		}
		clearSessionCookie(w, r)
		writeJSON(w, http.StatusOK, map[string]any{"authenticated": false})
	}
}

type credentialsRequest struct {
	CurrentPassword string `json:"current_password"`
	NewUsername     string `json:"new_username"`
	NewPassword     string `json:"new_password"`
}

// handlePutCredentials changes the admin username and/or password.
//
// When credentials already exist this sits behind the gate (the middleware has
// already rejected anonymous callers) and additionally re-checks the current
// password, so a borrowed session cannot silently change the password. When no
// credentials exist yet it performs first-time setup without a current
// password — there is nothing to verify, and in that state the dashboard is
// already fully open, so this grants no access that was not already there. It
// is the only way to turn auth on from the UI.
//
// That last argument only holds when nothing is configured at all. With
// LANTERN_AUTH_TOKEN set the dashboard is *not* open, so setup mode there
// additionally requires the admin token.
func handlePutCredentials(db *sql.DB, cfg *Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req credentialsRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "Malformed request body.")
			return
		}

		setup := !authRequired()

		// Setup mode grants an admin session to a caller who proved nothing,
		// which is only defensible when the dashboard was already wide open.
		// On a token-mode deployment the operator has configured auth, so
		// minting an admin out of nothing is a privilege escalation — the
		// middleware now gates this route, and this is the second lock.
		if setup && cfg.AuthToken != "" {
			ctxAdmin, _ := r.Context().Value(isAdminKey).(bool)
			if !ctxAdmin {
				writeError(w, http.StatusUnauthorized,
					"Set credentials with the admin API token, or unset LANTERN_AUTH_TOKEN first.")
				return
			}
		}
		currentUser, _, _ := loadCredentials(db)

		if !setup {
			if req.CurrentPassword == "" {
				writeError(w, http.StatusBadRequest, "Current password is required.")
				return
			}
			if !verifyCredentials(db, currentUser, req.CurrentPassword) {
				writeError(w, http.StatusUnauthorized, "Current password is incorrect")
				return
			}
		}

		newUser := strings.TrimSpace(req.NewUsername)
		if newUser == "" {
			newUser = currentUser
		}
		newPass := req.NewPassword

		if setup {
			if newUser == "" {
				writeError(w, http.StatusBadRequest, "A username is required.")
				return
			}
			if newPass == "" {
				writeError(w, http.StatusBadRequest, "A password is required.")
				return
			}
		}
		if newPass == "" {
			// Username-only change: the existing hash is left untouched.
			if _, err := db.Exec(`UPDATE admin_credentials SET username = ?, updated_at = ? WHERE id = 1`,
				newUser, time.Now().Unix()); err != nil {
				writeError(w, http.StatusInternalServerError, "Could not update credentials.")
				return
			}
		} else {
			if len(newPass) < 8 {
				writeError(w, http.StatusBadRequest, "New password must be at least 8 characters.")
				return
			}
			if err := writeCredentials(db, newUser, newPass); err != nil {
				log.Printf("auth: credential update failed: %v", err)
				writeError(w, http.StatusInternalServerError, "Could not update credentials.")
				return
			}
		}

		// Rotate: every existing session dies, including this caller's, then
		// the caller is handed a fresh one so the tab they are using stays
		// logged in. Any other device has to sign in again.
		if err := revokeAllSessions(db); err != nil {
			log.Printf("auth: could not revoke sessions: %v", err)
		}
		refreshCredentialState(db)

		raw, exp, err := createSession(db, newUser)
		if err != nil {
			log.Printf("auth: could not re-issue session: %v", err)
			clearSessionCookie(w, r)
			writeJSON(w, http.StatusOK, map[string]any{"updated": true, "username": newUser, "reauth": true})
			return
		}
		setSessionCookie(w, r, raw, exp)
		writeJSON(w, http.StatusOK, map[string]any{"updated": true, "username": newUser, "reauth": false})
	}
}
