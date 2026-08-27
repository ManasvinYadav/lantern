package main

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func mustExec(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatalf("exec %q: %v", query, err)
	}
}

func countOpenWindows(t *testing.T, db *sql.DB, name string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM maintenance_windows WHERE service_name = ? AND ended_at IS NULL`,
		name).Scan(&n); err != nil {
		t.Fatalf("count open windows: %v", err)
	}
	return n
}

// ---------------------------------------------------------------------------
// Maintenance windows: loaded once, tested in memory
// ---------------------------------------------------------------------------

func TestMaintenanceWindowsAgreeWithPerCallQuery(t *testing.T) {
	db := newTestDB(t)

	base := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	mustExec(t, db, `INSERT INTO maintenance_windows (service_name, started_at, ended_at, note) VALUES (?,?,?,?)`,
		"svc", base.Format(time.RFC3339), base.Add(time.Hour).Format(time.RFC3339), "closed")
	mustExec(t, db, `INSERT INTO maintenance_windows (service_name, started_at, note) VALUES (?,?,?)`,
		"svc", base.Add(24*time.Hour).Format(time.RFC3339), "still open")

	windows := loadMaintenanceWindows(db, "svc")
	if len(windows) != 2 {
		t.Fatalf("loaded %d windows, want 2", len(windows))
	}

	cases := []struct {
		name string
		at   time.Time
		want bool
	}{
		{"before everything", base.Add(-time.Hour), false},
		{"inside the closed window", base.Add(30 * time.Minute), true},
		{"exactly at its start", base, true},
		{"exactly at its end", base.Add(time.Hour), true},
		{"after it closed", base.Add(2 * time.Hour), false},
		{"inside the open window", base.Add(48 * time.Hour), true},
		{"far future, window never closed", base.Add(9000 * time.Hour), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := inAnyWindow(windows, c.at); got != c.want {
				t.Errorf("inAnyWindow = %v, want %v", got, c.want)
			}
			// The in-memory path must agree with the per-call query it replaced
			// on the hot loops, or uptime accounting silently changes meaning.
			if got := isInMaintenance(db, "svc", c.at); got != c.want {
				t.Errorf("isInMaintenance = %v, want %v (disagrees with inAnyWindow)", got, c.want)
			}
		})
	}
}

func TestLoadMaintenanceWindowsIsScopedToOneService(t *testing.T) {
	db := newTestDB(t)
	now := time.Now().UTC().Format(time.RFC3339)
	mustExec(t, db, `INSERT INTO maintenance_windows (service_name, started_at) VALUES (?,?)`, "mine", now)
	mustExec(t, db, `INSERT INTO maintenance_windows (service_name, started_at) VALUES (?,?)`, "theirs", now)

	if got := len(loadMaintenanceWindows(db, "mine")); got != 1 {
		t.Errorf("windows for \"mine\" = %d, want 1", got)
	}
	if got := len(loadMaintenanceWindows(db, "nobody")); got != 0 {
		t.Errorf("windows for an unknown service = %d, want 0", got)
	}
}

// ---------------------------------------------------------------------------
// Maintenance toggle is atomic and does not stack open windows
// ---------------------------------------------------------------------------

func TestSetMaintenanceStateOpensAndClosesExactlyOneWindow(t *testing.T) {
	db := newTestDB(t)

	setMaintenanceState(db, "svc", true, "first")
	// A double enable used to leave two open windows, and only one would ever
	// close — permanently excluding real downtime from uptime afterwards.
	setMaintenanceState(db, "svc", true, "double click")

	if got := countOpenWindows(t, db, "svc"); got != 1 {
		t.Fatalf("open windows after two enables = %d, want 1", got)
	}

	setMaintenanceState(db, "svc", false, "")
	if got := countOpenWindows(t, db, "svc"); got != 0 {
		t.Fatalf("open windows after disable = %d, want 0", got)
	}

	var enabled int
	if err := db.QueryRow(`SELECT enabled FROM service_maintenance WHERE service_name = ?`, "svc").Scan(&enabled); err != nil {
		t.Fatalf("read maintenance flag: %v", err)
	}
	if enabled != 0 {
		t.Errorf("flag = %d after disable, want 0 — the flag and the window disagree", enabled)
	}
}

// ---------------------------------------------------------------------------
// WebSocket origin checking
// ---------------------------------------------------------------------------

func TestWebSocketOriginPolicy(t *testing.T) {
	cases := []struct {
		name   string
		host   string
		origin string
		want   bool
	}{
		{"no Origin header at all (curl, scripts, CI jobs)", "lantern.local:7654", "", true},
		{"same host", "lantern.local:7654", "http://lantern.local:7654", true},
		{"same host over https", "lantern.local", "https://lantern.local", true},
		{"same host, different case", "Lantern.Local:7654", "http://lantern.local:7654", true},
		{"another site entirely", "lantern.local:7654", "https://evil.example", false},
		{"same name, different port", "lantern.local:7654", "http://lantern.local:9999", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/ws", nil)
			r.Host = c.host
			if c.origin != "" {
				r.Header.Set("Origin", c.origin)
			}
			if got := wsOriginAllowed(r); got != c.want {
				t.Errorf("wsOriginAllowed(host=%q, origin=%q) = %v, want %v", c.host, c.origin, got, c.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Login throttle forgets stale entries
// ---------------------------------------------------------------------------

func TestLoginThrottleEvictsStaleEntries(t *testing.T) {
	tr := newLoginThrottle()

	stale := httptest.NewRequest(http.MethodPost, "/api/auth/login", nil)
	stale.RemoteAddr = "10.1.1.1:1111"
	tr.fail(stale)

	tr.mu.Lock()
	for _, e := range tr.entries {
		e.lastSeen = time.Now().Add(-2 * throttleEntryTTL)
	}
	tr.mu.Unlock()

	// Any later write sweeps, so the map cannot grow without bound under a
	// source address that rotates.
	fresh := httptest.NewRequest(http.MethodPost, "/api/auth/login", nil)
	fresh.RemoteAddr = "10.2.2.2:2222"
	tr.fail(fresh)

	tr.mu.Lock()
	n := len(tr.entries)
	_, staleStillThere := tr.entries["10.1.1.1"]
	tr.mu.Unlock()

	if staleStillThere {
		t.Error("an entry untouched for longer than the TTL survived the sweep")
	}
	if n != 1 {
		t.Errorf("entries = %d, want 1 (only the fresh source)", n)
	}
}

func TestLoginThrottleNeverEvictsAnActiveLockout(t *testing.T) {
	tr := newLoginThrottle()
	r := httptest.NewRequest(http.MethodPost, "/api/auth/login", nil)
	r.RemoteAddr = "10.3.3.3:3333"

	for i := 0; i < maxLoginFailures; i++ {
		tr.fail(r)
	}
	if blocked, _ := tr.blocked(r); !blocked {
		t.Fatal("expected a lockout after maxLoginFailures")
	}

	// Backdate past the TTL. The lockout must still hold, or an attacker could
	// clear their own by simply waiting quietly.
	tr.mu.Lock()
	for _, e := range tr.entries {
		e.lastSeen = time.Now().Add(-10 * throttleEntryTTL)
	}
	tr.mu.Unlock()

	other := httptest.NewRequest(http.MethodPost, "/api/auth/login", nil)
	other.RemoteAddr = "10.4.4.4:4444"
	tr.fail(other)

	if blocked, _ := tr.blocked(r); !blocked {
		t.Error("an active lockout was evicted by the sweep")
	}
}

// ---------------------------------------------------------------------------
// Schema migrations distinguish a no-op from a real failure
// ---------------------------------------------------------------------------

func TestApplyMigrationIsIdempotent(t *testing.T) {
	db := newTestDB(t)
	// initDB already added latency_ms; re-running is the duplicate-column no-op
	// that must stay silent and must not damage the column.
	applyMigration(db, "ALTER TABLE status_events ADD COLUMN latency_ms INTEGER DEFAULT 0;")

	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('status_events') WHERE name = 'latency_ms'`).Scan(&n); err != nil {
		t.Fatalf("inspect columns: %v", err)
	}
	if n != 1 {
		t.Errorf("latency_ms column count = %d, want 1", n)
	}
}

// ---------------------------------------------------------------------------
// Security headers
// ---------------------------------------------------------------------------

func TestSecurityHeadersAreSetOnEveryResponse(t *testing.T) {
	rec := httptest.NewRecorder()
	securityHeadersMiddleware(okHandler()).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	for k, want := range map[string]string{
		"X-Content-Type-Options": "nosniff",
		"Referrer-Policy":        "strict-origin-when-cross-origin",
		"X-Frame-Options":        "SAMEORIGIN",
	} {
		if got := rec.Header().Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}

	csp := rec.Header().Get("Content-Security-Policy")
	directives := map[string]bool{}
	for _, d := range strings.Split(csp, ";") {
		directives[strings.TrimSpace(d)] = true
	}
	for _, want := range []string{
		"default-src 'self'",
		"connect-src 'self'",
		"frame-ancestors 'self'",
		"base-uri 'self'",
		"form-action 'self'",
	} {
		if !directives[want] {
			t.Errorf("CSP missing %q; got %q", want, csp)
		}
	}
}

// ---------------------------------------------------------------------------
// Request bodies are bounded
// ---------------------------------------------------------------------------

func TestOversizedStatusBodyIsRejected(t *testing.T) {
	db := newTestDB(t)
	cfg := &Config{}
	h := handlePostStatus(db, cfg, newWebhookDispatcher(db, 1), newWSHub())

	huge := `{"service_name":"x","status":"up","message":"` + strings.Repeat("A", maxStatusBody+1024) + `"}`
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodPost, "/api/status", strings.NewReader(huge)))

	if rec.Code != http.StatusBadRequest {
		t.Errorf("oversized POST /api/status = %d, want 400", rec.Code)
	}

	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM status_events WHERE service_name = ?`, "x").Scan(&n); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if n != 0 {
		t.Errorf("a rejected oversized body still wrote %d event(s)", n)
	}
}
