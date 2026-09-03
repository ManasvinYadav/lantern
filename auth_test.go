package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// newAuthDB builds a database with credentials already seeded from the
// environment-equivalent config, so the login gate is active.
func newAuthDB(t *testing.T, user, pass string) (*sql.DB, *Config) {
	t.Helper()
	cfg := &Config{DBPath: filepath.Join(t.TempDir(), "auth-test.db"), AuthUser: user, AuthPass: pass}
	cfg.AuthEnabled = cfg.AuthUser != ""
	db := initDB(cfg)
	t.Cleanup(func() { _ = db.Close() })
	return db, cfg
}

// okHandler is the terminal handler the middleware wraps in these tests.
func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
}

// loginFor performs a real login and returns the session cookie.
func loginFor(t *testing.T, db *sql.DB, user, pass string) *http.Cookie {
	t.Helper()
	body, _ := json.Marshal(loginRequest{Username: user, Password: pass})
	req := httptest.NewRequest(http.MethodPost, "/api/auth/login", bytes.NewReader(body))
	req.RemoteAddr = "10.0.0.1:1234"
	rec := httptest.NewRecorder()
	handlePostLogin(db, newLoginThrottle())(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("login = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookieName {
			return c
		}
	}
	t.Fatal("login succeeded but set no session cookie")
	return nil
}

// ---------------------------------------------------------------------------
// Bootstrap & storage
// ---------------------------------------------------------------------------

// With nothing configured, Lantern must behave exactly as it did before the
// login gate existed: fully open. This is the no-lockout guarantee.
func TestAuthIsOffWhenNoCredentialsExist(t *testing.T) {
	db := newTestDB(t)
	if authRequired() {
		t.Fatal("authRequired() = true on a database with no credentials")
	}
	cfg := &Config{}
	for _, path := range []string{"/api/services", "/ws", "/api/webhooks", "/"} {
		rec := httptest.NewRecorder()
		authMiddleware(db, cfg, okHandler()).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200 while auth is unconfigured", path, rec.Code)
		}
	}
}

func TestCredentialsSeedFromEnvOnFirstBootOnly(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{DBPath: filepath.Join(dir, "seed.db"), AuthUser: "admin", AuthPass: "hunter2hunter2"}
	cfg.AuthEnabled = true

	db := initDB(cfg)
	if !authRequired() {
		t.Fatal("authRequired() = false after seeding from env")
	}
	user, hash, ok := loadCredentials(db)
	if !ok || user != "admin" {
		t.Fatalf("loadCredentials = (%q, ok=%v), want admin", user, ok)
	}
	if strings.Contains(hash, "hunter2") {
		t.Error("stored hash contains the plaintext password")
	}
	if !strings.HasPrefix(hash, "$2") {
		t.Errorf("hash %q is not a bcrypt digest", hash)
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte("hunter2hunter2")) != nil {
		t.Error("stored hash does not verify against the seed password")
	}

	// Change the password, then reboot with the same (now stale) env pair.
	// The store must win, or a restart would silently undo the change.
	if err := writeCredentials(db, "admin", "a-different-password"); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	db2 := initDB(cfg)
	t.Cleanup(func() { _ = db2.Close() })
	if !verifyCredentials(db2, "admin", "a-different-password") {
		t.Error("reboot reverted the stored password to the env value")
	}
	if verifyCredentials(db2, "admin", "hunter2hunter2") {
		t.Error("stale env password still authenticates after a restart")
	}
}

func TestVerifyCredentialsRejectsWrongUserAndPassword(t *testing.T) {
	db, _ := newAuthDB(t, "admin", "correct-horse")
	cases := []struct {
		name, user, pass string
		want             bool
	}{
		{"both correct", "admin", "correct-horse", true},
		{"wrong password", "admin", "correct-hors", false},
		{"wrong username", "root", "correct-horse", false},
		{"both wrong", "root", "nope", false},
		{"empty", "", "", false},
	}
	for _, c := range cases {
		if got := verifyCredentials(db, c.user, c.pass); got != c.want {
			t.Errorf("%s: verifyCredentials(%q, %q) = %v, want %v", c.name, c.user, c.pass, got, c.want)
		}
	}
}

// ---------------------------------------------------------------------------
// Exemptions — the public surface must survive the gate (B3)
// ---------------------------------------------------------------------------

func TestAuthExemptPathKeepsPublicSurfaceOpen(t *testing.T) {
	cases := []struct {
		path   string
		exempt bool
		why    string
	}{
		{"/", true, "static shell must render the login form"},
		{"/status", true, "public status page"},
		{"/index.html", true, "static shell"},
		{"/api/public/services", true, "public API"},
		{"/api/public/ws", true, "public WebSocket"},
		{"/api/badge/foo.svg", true, "embeddable badge"},
		{"/metrics", true, "Prometheus scrape"},
		{"/api/auth/session", true, "shell asks whether to show the wall"},
		{"/api/auth/login", true, "you cannot log in through the gate"},
		// The container HEALTHCHECK wgets this with no credentials. Gating it
		// marks the container unhealthy as soon as auth is switched on.
		{"/api/health", true, "container healthcheck polls it"},
		{"/api/docs", true, "documented as always open"},

		{"/ws", false, "admin live feed is gated"},
		{"/api/services", false, "service data is gated"},
		{"/api/auth/credentials", false, "changing credentials needs a session"},
		{"/api/auth/logout", false, "needs a session to end"},
		{"/api/webhooks", false, "admin config is gated"},
		{"/api/services/x/docker/logs", false, "container logs are gated"},
	}
	for _, c := range cases {
		if got := authExemptPath(c.path); got != c.exempt {
			t.Errorf("authExemptPath(%q) = %v, want %v — %s", c.path, got, c.exempt, c.why)
		}
	}
}

// The regression this guards: the old middleware wrapped the SPA catch-all, so
// turning auth on took the public status page offline with it.
func TestGatedServerStillServesStatusAndPublicAPI(t *testing.T) {
	db, cfg := newAuthDB(t, "admin", "correct-horse")
	if !authRequired() {
		t.Fatal("gate is not active")
	}
	h := authMiddleware(db, cfg, okHandler())

	open := []string{"/", "/status", "/api/public/services", "/api/badge/x.svg", "/metrics",
		"/api/auth/session", "/api/health", "/api/docs"}
	for _, p := range open {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200 (must stay public under the gate)", p, rec.Code)
		}
	}

	gated := []string{"/api/services", "/ws", "/api/webhooks", "/api/groups"}
	for _, p := range gated {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("GET %s = %d, want 401 with no session", p, rec.Code)
		}
	}
}

// ---------------------------------------------------------------------------
// Login & sessions
// ---------------------------------------------------------------------------

func TestLoginSucceedsAndCookieAuthenticatesGatedRoutes(t *testing.T) {
	db, cfg := newAuthDB(t, "admin", "correct-horse")
	cookie := loginFor(t, db, "admin", "correct-horse")

	if !cookie.HttpOnly {
		t.Error("session cookie is not HttpOnly")
	}
	if cookie.SameSite != http.SameSiteStrictMode {
		t.Errorf("session cookie SameSite = %v, want Strict (CSRF defense)", cookie.SameSite)
	}
	if cookie.Path != "/" {
		t.Errorf("session cookie Path = %q, want /", cookie.Path)
	}

	// The plaintext token must not be what is stored.
	var stored string
	if err := db.QueryRow(`SELECT token_hash FROM sessions LIMIT 1`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored == cookie.Value {
		t.Error("session table stores the raw cookie value instead of its hash")
	}
	if stored != hashSessionToken(cookie.Value) {
		t.Error("stored session hash does not match sha256 of the cookie")
	}

	// /ws in particular: the cookie is the only credential a browser can put
	// on a WebSocket handshake.
	for _, p := range []string{"/api/services", "/ws", "/api/webhooks"} {
		req := httptest.NewRequest(http.MethodGet, p, nil)
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		authMiddleware(db, cfg, okHandler()).ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s with session = %d, want 200", p, rec.Code)
		}
	}
}

func TestLoginWithWrongPasswordIs401AndCreatesNoSession(t *testing.T) {
	db, _ := newAuthDB(t, "admin", "correct-horse")
	body, _ := json.Marshal(loginRequest{Username: "admin", Password: "wrong"})
	req := httptest.NewRequest(http.MethodPost, "/api/auth/login", bytes.NewReader(body))
	req.RemoteAddr = "10.0.0.2:1234"
	rec := httptest.NewRecorder()
	handlePostLogin(db, newLoginThrottle())(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("login with wrong password = %d, want 401", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Invalid credentials") {
		t.Errorf("body = %q, want an Invalid credentials message", rec.Body.String())
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookieName && c.Value != "" {
			t.Error("a failed login handed out a session cookie")
		}
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("sessions after a failed login = %d, want 0", n)
	}
}

func TestExpiredSessionIsRejectedAndPurged(t *testing.T) {
	db, _ := newAuthDB(t, "admin", "correct-horse")
	cookie := loginFor(t, db, "admin", "correct-horse")

	// Backdate the row past its TTL.
	if _, err := db.Exec(`UPDATE sessions SET expires_at = ?`, time.Now().Add(-time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/services", nil)
	req.AddCookie(cookie)
	if _, ok := sessionUser(db, req); ok {
		t.Error("an expired session still authenticates")
	}
	var n int
	_ = db.QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&n)
	if n != 0 {
		t.Errorf("expired session rows = %d, want 0 (should be purged on use)", n)
	}
}

func TestLogoutRevokesTheSession(t *testing.T) {
	db, cfg := newAuthDB(t, "admin", "correct-horse")
	cookie := loginFor(t, db, "admin", "correct-horse")

	req := httptest.NewRequest(http.MethodPost, "/api/auth/logout", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	handlePostLogout(db)(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("logout = %d, want 200", rec.Code)
	}

	after := httptest.NewRequest(http.MethodGet, "/api/services", nil)
	after.AddCookie(cookie)
	arec := httptest.NewRecorder()
	authMiddleware(db, cfg, okHandler()).ServeHTTP(arec, after)
	if arec.Code != http.StatusUnauthorized {
		t.Errorf("GET /api/services after logout = %d, want 401", arec.Code)
	}
}

func TestLoginThrottleLocksOutAfterRepeatedFailures(t *testing.T) {
	db, _ := newAuthDB(t, "admin", "correct-horse")
	th := newLoginThrottle()
	post := func(pass string) int {
		body, _ := json.Marshal(loginRequest{Username: "admin", Password: pass})
		req := httptest.NewRequest(http.MethodPost, "/api/auth/login", bytes.NewReader(body))
		req.RemoteAddr = "10.9.9.9:5555"
		rec := httptest.NewRecorder()
		handlePostLogin(db, th)(rec, req)
		return rec.Code
	}
	for i := 0; i < maxLoginFailures; i++ {
		if got := post("wrong"); got != http.StatusUnauthorized {
			t.Fatalf("attempt %d = %d, want 401", i+1, got)
		}
	}
	// Even the correct password is refused while the lockout holds.
	if got := post("correct-horse"); got != http.StatusTooManyRequests {
		t.Errorf("attempt after %d failures = %d, want 429", maxLoginFailures, got)
	}
}

// ---------------------------------------------------------------------------
// Credential management
// ---------------------------------------------------------------------------

func putCredentials(t *testing.T, db *sql.DB, body credentialsRequest) *httptest.ResponseRecorder {
	t.Helper()
	return putCredentialsWithConfig(t, db, &Config{}, body)
}

// putCredentialsWithConfig is the same call with an explicit Config, so a test
// can exercise the token-mode setup guard.
func putCredentialsWithConfig(t *testing.T, db *sql.DB, cfg *Config, body credentialsRequest) *httptest.ResponseRecorder {
	t.Helper()
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPut, "/api/auth/credentials", bytes.NewReader(b))
	rec := httptest.NewRecorder()
	handlePutCredentials(db, cfg)(rec, req)
	return rec
}

func TestCredentialsUpdateRejectsWrongCurrentPasswordWith401(t *testing.T) {
	db, _ := newAuthDB(t, "admin", "correct-horse")
	rec := putCredentials(t, db, credentialsRequest{
		CurrentPassword: "not-the-password",
		NewPassword:     "brand-new-password",
	})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("update with wrong current password = %d, want 401", rec.Code)
	}
	if !verifyCredentials(db, "admin", "correct-horse") {
		t.Error("a rejected update still changed the stored password")
	}
}

func TestCredentialsUpdateRequiresCurrentPassword(t *testing.T) {
	db, _ := newAuthDB(t, "admin", "correct-horse")
	rec := putCredentials(t, db, credentialsRequest{NewPassword: "brand-new-password"})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("update with no current password = %d, want 400", rec.Code)
	}
}

func TestCredentialsUpdateChangesBothAndRotatesSessions(t *testing.T) {
	db, cfg := newAuthDB(t, "admin", "correct-horse")

	// Two devices signed in; both must be revoked by the change.
	stale := loginFor(t, db, "admin", "correct-horse")
	_ = loginFor(t, db, "admin", "correct-horse")

	rec := putCredentials(t, db, credentialsRequest{
		CurrentPassword: "correct-horse",
		NewUsername:     "operator",
		NewPassword:     "brand-new-password",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("update = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}

	if !verifyCredentials(db, "operator", "brand-new-password") {
		t.Error("new credentials do not verify")
	}
	if verifyCredentials(db, "admin", "correct-horse") {
		t.Error("old credentials still verify after the change")
	}

	// Exactly one session survives: the freshly minted one for this caller.
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("sessions after credential change = %d, want 1 (caller re-issued)", n)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/services", nil)
	req.AddCookie(stale)
	srec := httptest.NewRecorder()
	authMiddleware(db, cfg, okHandler()).ServeHTTP(srec, req)
	if srec.Code != http.StatusUnauthorized {
		t.Errorf("stale session after credential change = %d, want 401", srec.Code)
	}

	// The caller keeps working on the cookie the response handed back.
	var fresh *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookieName {
			fresh = c
		}
	}
	if fresh == nil || fresh.Value == "" {
		t.Fatal("credential change did not re-issue a session cookie")
	}
	freq := httptest.NewRequest(http.MethodGet, "/api/services", nil)
	freq.AddCookie(fresh)
	frec := httptest.NewRecorder()
	authMiddleware(db, cfg, okHandler()).ServeHTTP(frec, freq)
	if frec.Code != http.StatusOK {
		t.Errorf("re-issued session = %d, want 200", frec.Code)
	}
}

func TestCredentialsUsernameOnlyChangeKeepsPassword(t *testing.T) {
	db, _ := newAuthDB(t, "admin", "correct-horse")
	rec := putCredentials(t, db, credentialsRequest{
		CurrentPassword: "correct-horse",
		NewUsername:     "operator",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("username-only update = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	if !verifyCredentials(db, "operator", "correct-horse") {
		t.Error("password did not survive a username-only change")
	}
}

func TestCredentialsPasswordOnlyChangeKeepsUsername(t *testing.T) {
	db, _ := newAuthDB(t, "admin", "correct-horse")
	rec := putCredentials(t, db, credentialsRequest{
		CurrentPassword: "correct-horse",
		NewPassword:     "brand-new-password",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("password-only update = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	if !verifyCredentials(db, "admin", "brand-new-password") {
		t.Error("username did not survive a password-only change")
	}
}

func TestCredentialsUpdateRejectsShortPassword(t *testing.T) {
	db, _ := newAuthDB(t, "admin", "correct-horse")
	rec := putCredentials(t, db, credentialsRequest{
		CurrentPassword: "correct-horse",
		NewPassword:     "short",
	})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("short password = %d, want 400", rec.Code)
	}
	if !verifyCredentials(db, "admin", "correct-horse") {
		t.Error("a rejected short password still changed the store")
	}
}

// First-run setup: with nothing configured the dashboard is already wide open,
// so requiring a current password would make it impossible to ever turn auth
// on from the UI.
func TestFirstTimeSetupNeedsNoCurrentPassword(t *testing.T) {
	db := newTestDB(t)
	if authRequired() {
		t.Fatal("gate is already active on a fresh database")
	}
	rec := putCredentials(t, db, credentialsRequest{
		NewUsername: "admin",
		NewPassword: "brand-new-password",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("first-time setup = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	if !authRequired() {
		t.Error("gate is still inactive after first-time setup")
	}
	if !verifyCredentials(db, "admin", "brand-new-password") {
		t.Error("credentials were not stored by first-time setup")
	}
}

// ---------------------------------------------------------------------------
// Back-compat: scripts and CI must keep working under the gate
// ---------------------------------------------------------------------------

func TestBearerAdminTokenStillWorksUnderTheGate(t *testing.T) {
	db, cfg := newAuthDB(t, "admin", "correct-horse")
	cfg.AuthToken = "a-long-admin-token"

	req := httptest.NewRequest(http.MethodGet, "/api/services", nil)
	req.Header.Set("Authorization", "Bearer a-long-admin-token")
	rec := httptest.NewRecorder()
	authMiddleware(db, cfg, okHandler()).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("admin bearer token = %d, want 200", rec.Code)
	}

	bad := httptest.NewRequest(http.MethodGet, "/api/services", nil)
	bad.Header.Set("Authorization", "Bearer a-long-admin-toke")
	brec := httptest.NewRecorder()
	authMiddleware(db, cfg, okHandler()).ServeHTTP(brec, bad)
	if brec.Code != http.StatusUnauthorized {
		t.Errorf("near-miss bearer token = %d, want 401", brec.Code)
	}
}

func TestScopedApiTokenStillResolvesUnderTheGate(t *testing.T) {
	db, cfg := newAuthDB(t, "admin", "correct-horse")
	if _, err := db.Exec(`INSERT INTO api_tokens (token, service_name) VALUES (?, ?)`,
		"scoped-token", "webapp"); err != nil {
		t.Fatal(err)
	}

	var got string
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if v, ok := r.Context().Value(scopedServiceKey).(string); ok {
			got = v
		}
		w.WriteHeader(http.StatusOK)
	})
	req := httptest.NewRequest(http.MethodPost, "/api/status", nil)
	req.Header.Set("Authorization", "Bearer scoped-token")
	rec := httptest.NewRecorder()
	authMiddleware(db, cfg, h).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("scoped token = %d, want 200", rec.Code)
	}
	if got != "webapp" {
		t.Errorf("scoped service = %q, want webapp", got)
	}
}

func TestBasicAuthValidatesAgainstTheStore(t *testing.T) {
	db, cfg := newAuthDB(t, "admin", "correct-horse")

	ok := httptest.NewRequest(http.MethodGet, "/api/services", nil)
	ok.SetBasicAuth("admin", "correct-horse")
	orec := httptest.NewRecorder()
	authMiddleware(db, cfg, okHandler()).ServeHTTP(orec, ok)
	if orec.Code != http.StatusOK {
		t.Errorf("basic auth with stored credentials = %d, want 200", orec.Code)
	}

	// After a change, the old password must stop working immediately.
	if err := writeCredentials(db, "admin", "rotated-password"); err != nil {
		t.Fatal(err)
	}
	stale := httptest.NewRequest(http.MethodGet, "/api/services", nil)
	stale.SetBasicAuth("admin", "correct-horse")
	srec := httptest.NewRecorder()
	authMiddleware(db, cfg, okHandler()).ServeHTTP(srec, stale)
	if srec.Code != http.StatusUnauthorized {
		t.Errorf("basic auth with the pre-change password = %d, want 401", srec.Code)
	}
}

// ---------------------------------------------------------------------------
// Session endpoint
// ---------------------------------------------------------------------------

func TestAuthSessionEndpointReportsGateState(t *testing.T) {
	db, cfg := newAuthDB(t, "admin", "correct-horse")

	decode := func(rec *httptest.ResponseRecorder) authSessionResponse {
		var out authSessionResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode %q: %v", rec.Body.String(), err)
		}
		return out
	}

	rec := httptest.NewRecorder()
	handleGetAuthSession(db, cfg)(rec, httptest.NewRequest(http.MethodGet, "/api/auth/session", nil))
	out := decode(rec)
	if !out.AuthRequired || out.Authenticated || out.CanSetup {
		t.Errorf("anonymous session = %+v, want auth_required=true authenticated=false can_setup=false", out)
	}

	cookie := loginFor(t, db, "admin", "correct-horse")
	areq := httptest.NewRequest(http.MethodGet, "/api/auth/session", nil)
	areq.AddCookie(cookie)
	arec := httptest.NewRecorder()
	handleGetAuthSession(db, cfg)(arec, areq)
	aout := decode(arec)
	if !aout.Authenticated || aout.Username != "admin" {
		t.Errorf("authenticated session = %+v, want authenticated=true username=admin", aout)
	}
}

func TestAuthSessionReportsTokenModeWithoutAWall(t *testing.T) {
	db := newTestDB(t)
	cfg := &Config{AuthToken: "legacy-token"}
	rec := httptest.NewRecorder()
	handleGetAuthSession(db, cfg)(rec, httptest.NewRequest(http.MethodGet, "/api/auth/session", nil))

	var out authSessionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.AuthRequired {
		t.Error("token-only mode must not raise the login wall — there is no password to type")
	}
	if !out.TokenMode {
		t.Error("token_mode = false, want true when LANTERN_AUTH_TOKEN is set alone")
	}
}

// ---------------------------------------------------------------------------
// Regression: routes that the gate used to miss
//
// Every case below was a live bypass before this change, verified against a
// running server rather than only in a test. They are pinned here so the next
// route added to setupRoutes cannot quietly reopen one.
// ---------------------------------------------------------------------------

// tokenModeMiddleware builds the LANTERN_AUTH_TOKEN-only configuration: no
// admin row, no username/password, just the bearer token. In that mode the
// middleware falls through to isProtectedEndpoint, so anything that function
// forgets is reachable by anyone.
func tokenModeMiddleware(t *testing.T) http.Handler {
	t.Helper()
	db := newTestDB(t)
	if authRequired() {
		t.Fatal("token-mode fixture unexpectedly has stored credentials")
	}
	return authMiddleware(db, &Config{AuthToken: "super-secret-admin-token"}, okHandler())
}

func TestTokenModeGatesCredentialChange(t *testing.T) {
	h := tokenModeMiddleware(t)
	rec := httptest.NewRecorder()
	body := strings.NewReader(`{"new_username":"attacker","new_password":"attackerpass"}`)
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/api/auth/credentials", body))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous PUT /api/auth/credentials = %d, want 401 — this is the "+
			"unauthenticated admin takeover: setup mode hands back an admin session", rec.Code)
	}
}

func TestTokenModeGatesBackupAndWebhookReads(t *testing.T) {
	h := tokenModeMiddleware(t)
	// Both are reads, which is exactly why they were missed: isProtectedEndpoint
	// was written around mutating routes. /api/backup returns the credential
	// hash, session hashes and API tokens; /api/webhooks returns Discord and
	// Telegram URLs, which are themselves credentials.
	for _, path := range []string{"/api/backup", "/api/webhooks"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("anonymous GET %s = %d, want 401", path, rec.Code)
		}
	}
}

func TestTokenModeStillAllowsTheAdminToken(t *testing.T) {
	h := tokenModeMiddleware(t)
	for _, path := range []string{"/api/backup", "/api/webhooks", "/api/auth/credentials"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer super-secret-admin-token")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s with the admin token = %d, want 200 — gating must not "+
				"break the operator's own access", path, rec.Code)
		}
	}
}

// TestScopedApiTokenCannotReachAdminOnlyEndpoints closes a privilege
// escalation: a token minted for one service authenticated successfully via
// Bearer on every route, including ones that reach far past that one
// service — a full database snapshot with every credential hash and every
// other service's token, all webhook URLs, and the whole installation's
// config export/import. isAdminOnlyEndpoint plus the check in authMiddleware
// is what's supposed to stop that; this is the regression test for it.
func TestScopedApiTokenCannotReachAdminOnlyEndpoints(t *testing.T) {
	db, cfg := newAuthDB(t, "admin", "correct-horse")
	cfg.AuthToken = "a-long-admin-token"
	if _, err := db.Exec(`INSERT INTO api_tokens (token, service_name) VALUES (?, ?)`,
		"scoped-token", "webapp"); err != nil {
		t.Fatal(err)
	}

	paths := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/api/backup"},
		{http.MethodGet, "/api/webhooks"},
		{http.MethodPut, "/api/webhooks"},
		{http.MethodPost, "/api/webhooks/test"},
		{http.MethodGet, "/api/config/export"},
		{http.MethodPost, "/api/config/import"},
		{http.MethodPut, "/api/auth/credentials"},
		{http.MethodGet, "/api/admin/audit-log"},
	}
	for _, p := range paths {
		req := httptest.NewRequest(p.method, p.path, nil)
		req.Header.Set("Authorization", "Bearer scoped-token")
		rec := httptest.NewRecorder()
		authMiddleware(db, cfg, okHandler()).ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s %s with a scoped token = %d, want 403", p.method, p.path, rec.Code)
		}
	}

	// The admin-wide token must still work on all of them — this is a
	// scoping fix, not a new gate on legitimate admin access.
	for _, p := range paths {
		req := httptest.NewRequest(p.method, p.path, nil)
		req.Header.Set("Authorization", "Bearer "+cfg.AuthToken)
		rec := httptest.NewRecorder()
		authMiddleware(db, cfg, okHandler()).ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("%s %s with the admin token = %d, want 200", p.method, p.path, rec.Code)
		}
	}
}

// The setup path exists so an open dashboard can turn auth on from the UI.
// With a token configured the dashboard is not open, so setup there would be a
// privilege escalation even if the middleware ever let the request through.
func TestSetupModeRefusedWhenATokenIsConfigured(t *testing.T) {
	db := newTestDB(t)
	rec := putCredentialsWithConfig(t, db, &Config{AuthToken: "super-secret-admin-token"},
		credentialsRequest{NewUsername: "attacker", NewPassword: "attackerpass"})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("setup with a token configured = %d, want 401", rec.Code)
	}
	if authRequired() {
		t.Fatal("a refused setup still wrote credentials")
	}
}

// /ws is deliberately gated. /api/public/ws was registered onto the same hub,
// so the gate bought nothing: the anonymous socket carried identical
// broadcasts. They are separate hubs now, and this pins the exemption split.
func TestPublicWebsocketIsExemptAndPrivateOneIsNot(t *testing.T) {
	if authExemptPath("/ws") {
		t.Error("/ws is exempt from the gate; it must require a session")
	}
	if !authExemptPath("/api/public/ws") {
		t.Error("/api/public/ws must stay open — it powers the public status page")
	}
	hub := newWSHub()
	if hub.public == nil {
		t.Fatal("newWSHub did not create the public counterpart hub")
	}
	if hub.public == hub {
		t.Fatal("the public hub is the private hub; a gated broadcast would be mirrored to anonymous clients")
	}
	if hub.public.public != nil {
		t.Error("the public hub has its own public hub, which would recurse")
	}
}

// The public envelope is spelled out field by field precisely so that a field
// added to ServiceSummary does not reach anonymous listeners by default.
// History is the one deliberately dropped: the public page has no heartbeat bar.
func TestPublicBroadcastDropsHeartbeatHistory(t *testing.T) {
	full := ServiceSummary{
		ServiceName: "db",
		Status:      "up",
		Message:     "ok",
		History:     []HeartbeatBeat{{Status: "up", Msg: "internal detail"}},
	}
	pub := publicViewOf(full)
	body, err := json.Marshal(wsPublicMessage{Type: "status_update", Service: pub})
	if err != nil {
		t.Fatalf("marshal public message: %v", err)
	}
	if strings.Contains(string(body), "internal detail") {
		t.Errorf("public broadcast carries heartbeat message bodies: %s", body)
	}
	if strings.Contains(string(body), `"history"`) {
		t.Errorf("public broadcast carries check history: %s", body)
	}
	if pub.ServiceName != "db" || pub.Status != "up" {
		t.Errorf("public view lost the fields the status page needs: %+v", pub)
	}
}

// Session tokens were always hashed at rest; per-service API tokens were not,
// so a copy of the database yielded working bearers rather than digests.
func TestScopedApiTokenIsHashedInPlaceOnFirstUse(t *testing.T) {
	db := newTestDB(t)
	const raw = "plaintext-scoped-token"
	if _, err := db.Exec(`INSERT INTO api_tokens (token, service_name) VALUES (?, ?)`, raw, "db"); err != nil {
		t.Fatalf("seed api_tokens: %v", err)
	}

	name, ok := lookupScopedToken(db, raw)
	if !ok || name != "db" {
		t.Fatalf("lookupScopedToken(plaintext) = (%q, %v), want (db, true)", name, ok)
	}

	var stored string
	if err := db.QueryRow(`SELECT token FROM api_tokens WHERE service_name = ?`, "db").Scan(&stored); err != nil {
		t.Fatalf("read back token: %v", err)
	}
	if stored == raw {
		t.Error("token is still stored in plaintext after being used")
	}
	if stored != hashSessionToken(raw) {
		t.Errorf("stored token = %q, want the SHA-256 of the bearer", stored)
	}

	// Still resolvable afterwards — the upgrade must be transparent.
	if name, ok := lookupScopedToken(db, raw); !ok || name != "db" {
		t.Errorf("lookupScopedToken after upgrade = (%q, %v), want (db, true)", name, ok)
	}
	if _, ok := lookupScopedToken(db, "not-a-token"); ok {
		t.Error("an unknown bearer resolved to a service")
	}
}
