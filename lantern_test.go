package main

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gorilla/mux"
)

// ---------------------------------------------------------------------------
// Heartbeat window: padding, ordering, uptime ratio
// ---------------------------------------------------------------------------

func TestLeftPadEmptyBeats(t *testing.T) {
	t.Run("nil pads to full width", func(t *testing.T) {
		got := leftPadEmptyBeats(nil, 30)
		if len(got) != 30 {
			t.Fatalf("len = %d, want 30", len(got))
		}
		for i, b := range got {
			if b.Status != "empty" {
				t.Errorf("beat %d = %q, want empty", i, b.Status)
			}
		}
	})

	t.Run("pads on the left and keeps real beats newest-last", func(t *testing.T) {
		real := []HeartbeatBeat{
			{Status: "up", Msg: "a"},
			{Status: "down", Msg: "b"},
			{Status: "up", Msg: "c"},
		}
		got := leftPadEmptyBeats(real, 30)
		if len(got) != 30 {
			t.Fatalf("len = %d, want 30", len(got))
		}
		for i := 0; i < 27; i++ {
			if got[i].Status != "empty" {
				t.Errorf("beat %d = %q, want empty padding", i, got[i].Status)
			}
		}
		// Real beats must survive in order, occupying the newest slots.
		if got[27].Msg != "a" || got[28].Msg != "b" || got[29].Msg != "c" {
			t.Errorf("tail = %q/%q/%q, want a/b/c", got[27].Msg, got[28].Msg, got[29].Msg)
		}
	})

	t.Run("full window is returned untouched", func(t *testing.T) {
		full := make([]HeartbeatBeat, 30)
		for i := range full {
			full[i] = HeartbeatBeat{Status: "up"}
		}
		if got := leftPadEmptyBeats(full, 30); len(got) != 30 {
			t.Fatalf("len = %d, want 30", len(got))
		}
	})
}

func TestWindowUptimePct(t *testing.T) {
	beats := func(statuses ...string) []HeartbeatBeat {
		out := make([]HeartbeatBeat, 0, len(statuses))
		for _, s := range statuses {
			out = append(out, HeartbeatBeat{Status: s})
		}
		return out
	}

	cases := []struct {
		name string
		in   []HeartbeatBeat
		want float64
	}{
		{"nil window is zero, not NaN", nil, 0},
		{"all padding is zero, not NaN", beats("empty", "empty", "empty"), 0},
		{"all up", beats("up", "up", "up", "up"), 100},
		{"all down", beats("down", "down"), 0},
		{"half up", beats("up", "down"), 50},
		{"padding excluded from both sides", beats("empty", "empty", "up", "up"), 100},
		{"partial window scored on real checks only", beats("empty", "up", "up", "down"), 200.0 / 3.0},
		{"degraded counts against uptime but stays in denominator", beats("up", "degraded"), 50},
		{"maintenance is not up", beats("up", "maintenance"), 50},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := windowUptimePct(tc.in)
			if diff := got - tc.want; diff > 0.0001 || diff < -0.0001 {
				t.Errorf("windowUptimePct = %v, want %v", got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Webhook flap dampening
// ---------------------------------------------------------------------------

func TestShouldNotify(t *testing.T) {
	cases := []struct {
		name     string
		prev2    string
		prev1    string
		current  string
		wantFire bool
		wantKind string
	}{
		{"first ever event is a baseline", "", "", "up", false, ""},
		{"first ever event, down, is still a baseline", "", "", "down", false, ""},
		{"steady up is silent", "up", "up", "up", false, ""},
		{"first down is held back", "up", "up", "down", false, ""},
		{"second consecutive down fires once", "up", "down", "down", true, "down"},
		{"third down does not re-alert", "down", "down", "down", false, ""},
		{"recovery after announced outage fires", "down", "down", "up", true, "recovery"},
		{"single-beat flap is fully silent", "up", "down", "up", false, ""},
		{"new service confirmed down fires", "", "down", "down", true, "down"},
		{"leaving confirmed down to degraded is a recovery", "down", "down", "degraded", true, "recovery"},
		{"up to degraded reports immediately", "up", "up", "degraded", true, "change"},
		{"degraded to up reports immediately", "up", "degraded", "up", true, "change"},
		{"steady degraded is silent", "degraded", "degraded", "degraded", false, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fire, kind := shouldNotify(tc.prev2, tc.prev1, tc.current)
			if fire != tc.wantFire || kind != tc.wantKind {
				t.Errorf("shouldNotify(%q,%q,%q) = (%v,%q), want (%v,%q)",
					tc.prev2, tc.prev1, tc.current, fire, kind, tc.wantFire, tc.wantKind)
			}
		})
	}
}

// TestShouldNotifyOutageProducesExactlyOnePair walks a realistic outage and
// asserts the whole episode yields one DOWN and one recovery, not a stream.
func TestShouldNotifyOutageProducesExactlyOnePair(t *testing.T) {
	seq := []string{"up", "up", "down", "down", "down", "down", "up", "up"}

	var downs, recoveries int
	for i := 2; i < len(seq); i++ {
		fire, kind := shouldNotify(seq[i-2], seq[i-1], seq[i])
		if !fire {
			continue
		}
		switch kind {
		case "down":
			downs++
		case "recovery":
			recoveries++
		default:
			t.Errorf("unexpected kind %q at index %d", kind, i)
		}
	}
	if downs != 1 || recoveries != 1 {
		t.Errorf("got %d down / %d recovery alerts, want exactly 1 each", downs, recoveries)
	}
}

// ---------------------------------------------------------------------------
// SVG badge colours
// ---------------------------------------------------------------------------

func TestBadgeColorForStatus(t *testing.T) {
	cases := map[string]string{
		"up":          "#10B981",
		"down":        "#F43F5E",
		"degraded":    "#F59E0B",
		"maintenance": "#6B7280",
		"unknown":     "#6B7280",
		"":            "#6B7280",
		"nonsense":    "#6B7280",
	}
	for status, want := range cases {
		if got := badgeColorForStatus(status); got != want {
			t.Errorf("badgeColorForStatus(%q) = %s, want %s", status, got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// Native Docker discovery mapping
// ---------------------------------------------------------------------------

func TestDockerHealthFromStatus(t *testing.T) {
	cases := map[string]string{
		"Up 4 days (healthy)":             "healthy",
		"Up 2 minutes (unhealthy)":        "unhealthy",
		"Up 5 seconds (health: starting)": "starting",
		"Up 4 days":                       "",
		"Exited (0) 4 hours ago":          "",
		"":                                "",
	}
	for status, want := range cases {
		if got := dockerHealthFromStatus(status); got != want {
			t.Errorf("dockerHealthFromStatus(%q) = %q, want %q", status, got, want)
		}
	}
}

func TestDockerStatusFor(t *testing.T) {
	cases := []struct {
		name   string
		state  string
		status string
		want   string
	}{
		{"running healthy", "running", "Up 4 days (healthy)", "up"},
		{"running with no healthcheck is taken at its word", "running", "Up 4 days", "up"},
		{"running but still warming up is not yet up", "running", "Up 5 seconds (health: starting)", "degraded"},
		{"running but unhealthy", "running", "Up 2 minutes (unhealthy)", "degraded"},
		{"restarting", "restarting", "Restarting (1) 5 seconds ago", "degraded"},
		{"paused", "paused", "Up 2 days (Paused)", "degraded"},
		{"exited", "exited", "Exited (0) 4 hours ago", "down"},
		{"dead", "dead", "Dead", "down"},
		{"created but never started", "created", "Created", "down"},
		{"removing", "removing", "Removal In Progress", "down"},
		{"unrecognised state", "teleporting", "Who knows", "unknown"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, msg := dockerStatusFor(tc.state, tc.status)
			if got != tc.want {
				t.Errorf("dockerStatusFor(%q,%q) = %q, want %q", tc.state, tc.status, got, tc.want)
			}
			// The Docker status line is already human-readable and is carried
			// through verbatim as the beat message.
			if msg != tc.status {
				t.Errorf("message = %q, want passthrough %q", msg, tc.status)
			}
		})
	}

	t.Run("empty status falls back to the raw state", func(t *testing.T) {
		if _, msg := dockerStatusFor("running", ""); msg != "state: running" {
			t.Errorf("message = %q, want %q", msg, "state: running")
		}
	})
}

func TestDockerServiceName(t *testing.T) {
	t.Run("strips the leading slash", func(t *testing.T) {
		got := dockerServiceName(DockerContainerSummary{Names: []string{"/lantern"}})
		if got != "lantern" {
			t.Errorf("got %q, want lantern", got)
		}
	})

	t.Run("skips empty names", func(t *testing.T) {
		got := dockerServiceName(DockerContainerSummary{Names: []string{"", "/redis"}})
		if got != "redis" {
			t.Errorf("got %q, want redis", got)
		}
	})

	t.Run("falls back to a short container id", func(t *testing.T) {
		got := dockerServiceName(DockerContainerSummary{ID: "abcdef1234567890fedcba"})
		if got != "abcdef123456" {
			t.Errorf("got %q, want abcdef123456", got)
		}
	})
}

func TestDockerDiscoveryIgnored(t *testing.T) {
	cases := []struct {
		name   string
		labels map[string]string
		want   bool
	}{
		{"nil labels", nil, false},
		{"label absent", map[string]string{"com.docker.compose.service": "web"}, false},
		{"opted out", map[string]string{"lantern.ignore": "true"}, true},
		{"opted out, odd casing", map[string]string{"lantern.ignore": "TRUE"}, true},
		{"opted out, padded", map[string]string{"lantern.ignore": "  true  "}, true},
		{"explicitly opted in", map[string]string{"lantern.ignore": "false"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := dockerDiscoveryIgnored(tc.labels); got != tc.want {
				t.Errorf("dockerDiscoveryIgnored(%v) = %v, want %v", tc.labels, got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Service lifecycle: deletion cascade and manual check trigger
// ---------------------------------------------------------------------------

func newTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db := initDB(&Config{DBPath: filepath.Join(t.TempDir(), "lantern-test.db")})
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// newTestScheduler builds the struct directly rather than calling
// newMonitorPool, which spawns worker goroutines that would run real network
// probes. Here the job channel is inspected instead.
func newTestScheduler(db *sql.DB) *monitorScheduler {
	pool := &monitorPool{jobs: make(chan monitorCheckJob, 8), db: db}
	return &monitorScheduler{stopFns: make(map[string]chan struct{}), pool: pool, db: db}
}

// If a future migration adds a service-scoped table, this fails until the
// author decides whether deleting a service should clear it.
func TestServiceScopedTablesCoversSchema(t *testing.T) {
	db := newTestDB(t)

	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'`)
	if err != nil {
		t.Fatalf("list tables: %v", err)
	}
	defer rows.Close()

	var tables []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatalf("scan table name: %v", err)
		}
		tables = append(tables, n)
	}

	inCascade := make(map[string]bool, len(serviceScopedTables))
	for _, tbl := range serviceScopedTables {
		inCascade[tbl] = true
	}
	// Deliberately retained: an audit log of deliveries that were actually
	// sent, which outlives the service it names.
	exempt := map[string]bool{"webhook_deliveries": true}

	for _, tbl := range tables {
		cols, err := db.Query(`SELECT name FROM pragma_table_info(?)`, tbl)
		if err != nil {
			t.Fatalf("pragma_table_info(%s): %v", tbl, err)
		}
		hasServiceName := false
		for cols.Next() {
			var c string
			if err := cols.Scan(&c); err != nil {
				t.Fatalf("scan column: %v", err)
			}
			if c == "service_name" {
				hasServiceName = true
			}
		}
		cols.Close()

		if hasServiceName && !inCascade[tbl] && !exempt[tbl] {
			t.Errorf("table %q is keyed by service_name but is neither in serviceScopedTables nor exempt", tbl)
		}
	}

	for _, tbl := range serviceScopedTables {
		if !inCascade[tbl] {
			continue
		}
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name = ?`, tbl).Scan(&n); err != nil || n != 1 {
			t.Errorf("serviceScopedTables names %q, which does not exist in the schema", tbl)
		}
	}
}

func TestHandleDeleteServiceCascade(t *testing.T) {
	db := newTestDB(t)
	sched := newTestScheduler(db)

	const svc = "cascade-probe"
	seed := []struct {
		q    string
		args []any
	}{
		{`INSERT INTO status_events (service_name, status, message, timestamp) VALUES (?,?,?,?)`,
			[]any{svc, "up", "seed", "2026-08-26T00:00:00Z"}},
		{`INSERT INTO diagnostic_runs (service_name, title, content, timestamp) VALUES (?,?,?,?)`,
			[]any{svc, "t", "c", "2026-08-26T00:00:00Z"}},
		{`INSERT INTO service_maintenance (service_name, enabled) VALUES (?,?)`, []any{svc, 1}},
		{`INSERT INTO maintenance_windows (service_name, started_at) VALUES (?,?)`,
			[]any{svc, "2026-08-26T00:00:00Z"}},
		{`INSERT INTO service_groups (service_name, group_name) VALUES (?,?)`, []any{svc, "g"}},
		{`INSERT INTO active_monitors (service_name, monitor_type, target, interval_seconds, enabled) VALUES (?,?,?,?,?)`,
			[]any{svc, "http", "https://example.com", 60, 1}},
		{`INSERT INTO api_tokens (token, service_name) VALUES (?,?)`, []any{"tok-cascade", svc}},
		{`INSERT INTO notification_digest_queue (service_name, old_status, new_status, occurred_at) VALUES (?,?,?,?)`,
			[]any{svc, "up", "down", "2026-08-26T00:00:00Z"}},
		// Must survive the delete.
		{`INSERT INTO webhook_deliveries (channel, service_name, old_status, new_status, success, http_status, created_at) VALUES (?,?,?,?,?,?,?)`,
			[]any{"discord", svc, "up", "down", 1, 204, "2026-08-26T00:00:00Z"}},
	}
	for _, s := range seed {
		if _, err := db.Exec(s.q, s.args...); err != nil {
			t.Fatalf("seed %q: %v", s.q, err)
		}
	}

	r := mux.NewRouter()
	r.Handle("/api/services/{name}", handleDeleteService(db, sched)).Methods(http.MethodDelete)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/api/services/"+svc, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}

	for _, tbl := range serviceScopedTables {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM `+tbl+` WHERE service_name = ?`, svc).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", tbl, err)
		}
		if n != 0 {
			t.Errorf("%s still holds %d row(s) for the deleted service", tbl, n)
		}
	}

	var kept int
	if err := db.QueryRow(`SELECT COUNT(*) FROM webhook_deliveries WHERE service_name = ?`, svc).Scan(&kept); err != nil {
		t.Fatalf("count webhook_deliveries: %v", err)
	}
	if kept != 1 {
		t.Errorf("webhook_deliveries = %d rows, want 1 retained as an audit record", kept)
	}
}

func TestHandleDeleteServiceUnknownIs404(t *testing.T) {
	db := newTestDB(t)
	r := mux.NewRouter()
	r.Handle("/api/services/{name}", handleDeleteService(db, newTestScheduler(db))).Methods(http.MethodDelete)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/api/services/never-existed", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestHandlePostServiceCheck(t *testing.T) {
	db := newTestDB(t)
	sched := newTestScheduler(db)
	r := mux.NewRouter()
	r.Handle("/api/services/{name}/check", handlePostServiceCheck(db, sched)).Methods(http.MethodPost)

	call := func(name string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/services/"+name+"/check", nil))
		return rec
	}

	t.Run("no monitor is a conflict, not a queued check", func(t *testing.T) {
		if rec := call("push-only"); rec.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409", rec.Code)
		}
		if len(sched.pool.jobs) != 0 {
			t.Errorf("queued %d job(s) for a service with no monitor", len(sched.pool.jobs))
		}
	})

	t.Run("disabled monitor is a conflict", func(t *testing.T) {
		if _, err := db.Exec(`INSERT INTO active_monitors (service_name, monitor_type, target, interval_seconds, enabled) VALUES (?,?,?,?,?)`,
			"paused", "http", "https://example.com", 60, 0); err != nil {
			t.Fatalf("seed: %v", err)
		}
		if rec := call("paused"); rec.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409", rec.Code)
		}
		if len(sched.pool.jobs) != 0 {
			t.Errorf("queued %d job(s) for a disabled monitor", len(sched.pool.jobs))
		}
	})

	t.Run("enabled monitor queues exactly one job", func(t *testing.T) {
		if _, err := db.Exec(`INSERT INTO active_monitors (service_name, monitor_type, target, interval_seconds, enabled) VALUES (?,?,?,?,?)`,
			"live", "tcp", "db.local:5432", 60, 1); err != nil {
			t.Fatalf("seed: %v", err)
		}
		rec := call("live")
		if rec.Code != http.StatusAccepted {
			t.Fatalf("status = %d, want 202 (body %s)", rec.Code, rec.Body.String())
		}
		select {
		case job := <-sched.pool.jobs:
			if job.serviceName != "live" || job.monitorType != "tcp" || job.target != "db.local:5432" {
				t.Errorf("job = %+v, want the stored monitor's type and target", job)
			}
		default:
			t.Fatal("nothing was queued")
		}
	})
}

func TestIsProtectedEndpointCoversServiceLifecycle(t *testing.T) {
	cases := []struct {
		method, path string
		want         bool
	}{
		{http.MethodDelete, "/api/services/foo", true},
		{http.MethodPost, "/api/services/foo/check", true},
		// Reads stay open, matching the documented purpose of the token.
		{http.MethodGet, "/api/services", false},
		{http.MethodGet, "/api/services/foo/uptime", false},
	}
	for _, c := range cases {
		req := httptest.NewRequest(c.method, c.path, nil)
		if got := isProtectedEndpoint(req); got != c.want {
			t.Errorf("isProtectedEndpoint(%s %s) = %v, want %v", c.method, c.path, got, c.want)
		}
	}
}

// ---------------------------------------------------------------------------
// Service source classification and history latency
// ---------------------------------------------------------------------------

func TestServiceSourcePrecedence(t *testing.T) {
	// Package-level registry: restore it so ordering between tests cannot matter.
	t.Cleanup(func() { setDockerDiscovered(map[string]struct{}{}) })
	setDockerDiscovered(map[string]struct{}{"in-docker": {}, "both": {}})

	cases := []struct {
		name, service, monitorType, want string
	}{
		{"an explicit probe wins over discovery", "both", "http", "monitor"},
		{"probe on a service discovery never saw", "solo", "tcp", "monitor"},
		{"discovered container with no probe", "in-docker", "", "docker"},
		{"anything else is a pusher", "systemd-unit", "", "host"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := serviceSource(c.service, c.monitorType); got != c.want {
				t.Errorf("serviceSource(%q, %q) = %q, want %q", c.service, c.monitorType, got, c.want)
			}
		})
	}
}

// A failed discovery pass must not blank the registry, or every container would
// briefly report as host-sourced.
func TestDockerDiscoveredSnapshotIsReplacedWholesale(t *testing.T) {
	t.Cleanup(func() { setDockerDiscovered(map[string]struct{}{}) })

	setDockerDiscovered(map[string]struct{}{"a": {}, "b": {}})
	if !isDockerDiscovered("a") || !isDockerDiscovered("b") {
		t.Fatal("seeded names should be present")
	}
	// A later pass that no longer sees "b" must drop it.
	setDockerDiscovered(map[string]struct{}{"a": {}})
	if !isDockerDiscovered("a") {
		t.Error("a should still be discovered")
	}
	if isDockerDiscovered("b") {
		t.Error("b vanished from the pass and should no longer read as docker")
	}
}

func TestServiceHistoryReturnsLatency(t *testing.T) {
	db := newTestDB(t)

	const svc = "history-latency-probe"
	rows := []struct {
		status  string
		msg     string
		ts      string
		latency int64
	}{
		{"up", "first", "2026-08-26T00:00:00Z", 12},
		{"down", "second", "2026-08-26T00:01:00Z", 940},
		{"up", "third", "2026-08-26T00:02:00Z", 0},
	}
	for _, r := range rows {
		if _, err := db.Exec(
			`INSERT INTO status_events (service_name, status, message, timestamp, latency_ms) VALUES (?,?,?,?,?)`,
			svc, r.status, r.msg, r.ts, r.latency); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	r := mux.NewRouter()
	r.Handle("/api/services/{name}/history", handleGetServiceHistory(db)).Methods(http.MethodGet)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/services/"+svc+"/history?limit=90", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var resp ServiceHistoryResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Events) != 3 {
		t.Fatalf("events = %d, want 3", len(resp.Events))
	}

	// Newest first, matching the handler's ORDER BY.
	want := []int64{0, 940, 12}
	for i, w := range want {
		if resp.Events[i].LatencyMs != w {
			t.Errorf("event %d latency_ms = %d, want %d", i, resp.Events[i].LatencyMs, w)
		}
	}

	// The field must be present on the wire, not merely zero-valued in the struct.
	if !strings.Contains(rec.Body.String(), `"latency_ms"`) {
		t.Error("latency_ms is missing from the serialised response")
	}
}

// TestHandleGetGroupsRollup covers the worst-status-wins aggregation: a group
// with one down member and two up members must report "down" even though
// down is the minority, and its per-service maintenance flag must surface
// independently of that status rollup.
func TestHandleGetGroupsRollup(t *testing.T) {
	db := newTestDB(t)

	seedService := func(name, status, group string, maintenance bool) {
		if _, err := db.Exec(
			`INSERT INTO status_events (service_name, status, message, timestamp) VALUES (?,?,?,?)`,
			name, status, "seed", "2026-08-26T00:00:00Z"); err != nil {
			t.Fatalf("seed status_events for %s: %v", name, err)
		}
		if group != "" {
			if _, err := db.Exec(
				`INSERT INTO service_groups (service_name, group_name) VALUES (?,?)`, name, group); err != nil {
				t.Fatalf("seed service_groups for %s: %v", name, err)
			}
		}
		if maintenance {
			if _, err := db.Exec(
				`INSERT INTO service_maintenance (service_name, enabled) VALUES (?,?)`, name, 1); err != nil {
				t.Fatalf("seed service_maintenance for %s: %v", name, err)
			}
		}
	}

	// "web" group: majority up, one down -> rollup must be "down".
	seedService("web-1", "up", "web", false)
	seedService("web-2", "up", "web", false)
	seedService("web-3", "down", "web", false)

	// "db" group: all up, one under maintenance -> status "up", maintenance true.
	seedService("db-1", "up", "db", false)
	seedService("db-2", "up", "db", true)

	// Ungrouped service must not appear in the response at all.
	seedService("standalone", "down", "", false)

	r := mux.NewRouter()
	r.Handle("/api/groups", handleGetGroups(db)).Methods(http.MethodGet)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/groups", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}

	var groups []GroupSummary
	if err := json.Unmarshal(rec.Body.Bytes(), &groups); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(groups) != 2 {
		t.Fatalf("groups = %d, want 2 (got %+v)", len(groups), groups)
	}

	byName := map[string]GroupSummary{}
	for _, g := range groups {
		byName[g.Name] = g
	}

	web, ok := byName["web"]
	if !ok {
		t.Fatalf("missing group %q in %+v", "web", groups)
	}
	if web.Count != 3 {
		t.Errorf("web.Count = %d, want 3", web.Count)
	}
	if web.Status != "down" {
		t.Errorf("web.Status = %q, want %q (worst member must win)", web.Status, "down")
	}
	if web.Maintenance {
		t.Error("web.Maintenance = true, want false")
	}

	dbGroup, ok := byName["db"]
	if !ok {
		t.Fatalf("missing group %q in %+v", "db", groups)
	}
	if dbGroup.Count != 2 {
		t.Errorf("db.Count = %d, want 2", dbGroup.Count)
	}
	if dbGroup.Status != "up" {
		t.Errorf("db.Status = %q, want %q", dbGroup.Status, "up")
	}
	if !dbGroup.Maintenance {
		t.Error("db.Maintenance = false, want true (one member is under maintenance)")
	}
}
