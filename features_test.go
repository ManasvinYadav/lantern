package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/mux"
)

// ---------------------------------------------------------------------------
// Request size limiting
// ---------------------------------------------------------------------------

// Every write route caps its body and answers identically, so a client can
// distinguish "you sent junk" from a real validation failure without
// special-casing each endpoint.
func TestOversizedBodiesAreRejectedUniformly(t *testing.T) {
	db := newTestDB(t)
	cfg := &Config{}
	dispatcher := newWebhookDispatcher(db, 1)
	hub := newWSHub()
	scheduler := newMonitorScheduler(db, newMonitorPool(db, cfg, dispatcher, hub, 1))

	oversized := func(n int64) string {
		return `{"note":"` + strings.Repeat("A", int(n)+2048) + `"}`
	}

	cases := []struct {
		name   string
		limit  int64
		method string
		target string
		// vars stands in for the path variables gorilla/mux would populate.
		// handlePutServiceMonitor reads {name} before it reads the body, so
		// without them it fails validation first and never reaches the cap.
		vars    map[string]string
		handler http.HandlerFunc
	}{
		{"POST /api/status", maxStatusBody, http.MethodPost, "/api/status",
			nil, handlePostStatus(db, cfg, dispatcher, hub)},
		{"POST /api/diagnostics", maxDiagnosticsBody, http.MethodPost, "/api/diagnostics",
			nil, handlePostDiagnostics(db)},
		{"PUT /api/webhooks", maxConfigBody, http.MethodPut, "/api/webhooks",
			nil, handlePutWebhooks(db)},
		{"PUT /api/services/x/monitor", maxConfigBody, http.MethodPut, "/api/services/x/monitor",
			map[string]string{"name": "x"}, handlePutServiceMonitor(db, scheduler)},
		{"POST /api/banner", maxConfigBody, http.MethodPost, "/api/banner",
			nil, handlePostBanner(db)},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(c.method, c.target, strings.NewReader(oversized(c.limit)))
			if c.vars != nil {
				req = mux.SetURLVars(req, c.vars)
			}
			c.handler(rec, req)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", rec.Code)
			}
			var body map[string]string
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("response is not JSON: %q", rec.Body.String())
			}
			if body["error"] != "payload too large or invalid json" {
				t.Errorf("error = %q, want the shared message", body["error"])
			}
		})
	}
}

func TestWellFormedBodyUnderTheLimitStillWorks(t *testing.T) {
	db := newTestDB(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/status",
		strings.NewReader(`{"service_name":"ok-svc","status":"up","message":"fine"}`))
	handlePostStatus(db, &Config{}, newWebhookDispatcher(db, 1), newWSHub())(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (%s)", rec.Code, rec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// Transaction rollback
// ---------------------------------------------------------------------------

// A forced mid-transaction failure must leave nothing behind.
//
// maintenance_windows is dropped so the first statement in setMaintenanceState
// succeeds and the second cannot. Before the transaction existed, the flag was
// written anyway, leaving a service marked under maintenance with no window to
// account for it — the exact split-state the transaction is there to prevent.
func TestSetMaintenanceStateRollsBackOnPartialFailure(t *testing.T) {
	db := newTestDB(t)
	mustExec(t, db, `DROP TABLE maintenance_windows`)

	setMaintenanceState(db, "svc", true, "should not survive")

	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM service_maintenance WHERE service_name = ?`, "svc").Scan(&n); err != nil {
		t.Fatalf("count maintenance rows: %v", err)
	}
	if n != 0 {
		t.Errorf("service_maintenance has %d row(s) after a failed transaction, want 0 — the flag write was not rolled back", n)
	}
}

// The banner swap is dismiss-then-insert. Both must land, or neither: a partial
// apply would leave the old announcement showing, or none at all.
func TestBannerReplacementIsAtomic(t *testing.T) {
	db := newTestDB(t)

	post := func(title string) {
		t.Helper()
		body, _ := json.Marshal(map[string]string{"level": "warning", "title": title})
		rec := httptest.NewRecorder()
		handlePostBanner(db)(rec, httptest.NewRequest(http.MethodPost, "/api/banner", bytes.NewReader(body)))
		if rec.Code != http.StatusCreated {
			t.Fatalf("post %q = %d (%s)", title, rec.Code, rec.Body.String())
		}
	}

	post("first incident")
	post("second incident")

	var active int
	if err := db.QueryRow(`SELECT COUNT(*) FROM incident_banners WHERE dismissed_at IS NULL`).Scan(&active); err != nil {
		t.Fatalf("count active: %v", err)
	}
	if active != 1 {
		t.Fatalf("active banners = %d, want exactly 1", active)
	}

	b, ok := activeBanner(db)
	if !ok || b.Title != "second incident" {
		t.Errorf("active banner = %+v, want the most recent one", b)
	}

	// Dismissal is a timestamp, not a delete, so the record survives.
	var total int
	_ = db.QueryRow(`SELECT COUNT(*) FROM incident_banners`).Scan(&total)
	if total != 2 {
		t.Errorf("total banners = %d, want 2 — dismissal should not destroy history", total)
	}

	rec := httptest.NewRecorder()
	handleDeleteBanner(db)(rec, httptest.NewRequest(http.MethodDelete, "/api/banner", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("dismiss = %d", rec.Code)
	}
	if _, ok := activeBanner(db); ok {
		t.Error("a banner is still active after dismissal")
	}
}

func TestBannerRejectsBadInput(t *testing.T) {
	db := newTestDB(t)
	for _, c := range []struct{ name, payload string }{
		{"unknown level", `{"level":"catastrophic","title":"x"}`},
		{"missing title", `{"level":"info","title":"   "}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			handlePostBanner(db)(rec, httptest.NewRequest(http.MethodPost, "/api/banner", strings.NewReader(c.payload)))
			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", rec.Code)
			}
			if _, ok := activeBanner(db); ok {
				t.Error("a rejected banner was still published")
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Alert routing
// ---------------------------------------------------------------------------

func TestUnroutedServiceAlertsEverywhere(t *testing.T) {
	// The pre-routing behaviour has to survive the upgrade untouched, or every
	// existing install silently loses alerts.
	for _, ch := range []string{"discord", "telegram", "gotify", "generic"} {
		if !channelAllowed(nil, ch) {
			t.Errorf("unrouted service blocked %s", ch)
		}
	}
}

func TestAlertRouteRestrictsToNamedChannels(t *testing.T) {
	db := newTestDB(t)
	mustExec(t, db, `INSERT INTO service_alert_routes (service_name, channels) VALUES (?, ?)`,
		"db", "discord,gotify")

	route := alertRouteFor(db, "db")
	if len(route) != 2 {
		t.Fatalf("route = %v, want two channels", route)
	}
	for ch, want := range map[string]bool{
		"discord": true, "gotify": true, "telegram": false, "generic": false,
	} {
		if got := channelAllowed(route, ch); got != want {
			t.Errorf("channelAllowed(%s) = %v, want %v", ch, got, want)
		}
	}
}

func TestParseChannelsDropsUnknownAndBlank(t *testing.T) {
	got := parseChannels(" discord , ,nonsense, GOTIFY ")
	if len(got) != 2 || got[0] != "discord" || got[1] != "gotify" {
		t.Errorf("parseChannels = %v, want [discord gotify]", got)
	}
}

// ---------------------------------------------------------------------------
// Config export / import
// ---------------------------------------------------------------------------

func TestConfigExportRedactsSecretsByDefault(t *testing.T) {
	db := newTestDB(t)
	const secret = "https://discord.com/api/webhooks/000/SECRETVALUE"
	mustExec(t, db, `INSERT INTO webhook_configs (channel, url) VALUES (?, ?)`, "discord", secret)

	redacted, err := buildConfigExport(db, false)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	blob, _ := json.Marshal(redacted)
	if strings.Contains(string(blob), "SECRETVALUE") {
		t.Fatal("the default export leaked a webhook URL")
	}
	if !redacted.Redacted {
		t.Error("export is not flagged as redacted")
	}
	if len(redacted.Webhooks) != 1 || redacted.Webhooks[0].URL != redactedPlaceholder {
		t.Errorf("webhook = %+v, want the placeholder", redacted.Webhooks)
	}

	full, err := buildConfigExport(db, true)
	if err != nil {
		t.Fatalf("export with secrets: %v", err)
	}
	if full.Webhooks[0].URL != secret {
		t.Error("opt-in export did not include the real URL")
	}
	if full.Redacted {
		t.Error("opt-in export is still flagged redacted")
	}
}

// Re-importing a redacted export must not overwrite a live webhook with the
// literal placeholder — that would break alerting on every restore.
func TestImportOfRedactedExportPreservesExistingSecrets(t *testing.T) {
	db := newTestDB(t)
	cfg := &Config{}
	const secret = "https://discord.com/api/webhooks/000/STILL-VALID"
	mustExec(t, db, `INSERT INTO webhook_configs (channel, url) VALUES (?, ?)`, "discord", secret)

	export, err := buildConfigExport(db, false)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	body, _ := json.Marshal(export)

	scheduler := newMonitorScheduler(db, newMonitorPool(db, cfg, newWebhookDispatcher(db, 1), newWSHub(), 1))
	rec := httptest.NewRecorder()
	handleConfigImport(db, scheduler)(rec, httptest.NewRequest(http.MethodPost, "/api/config/import", bytes.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("import = %d (%s)", rec.Code, rec.Body.String())
	}

	var stored string
	if err := db.QueryRow(`SELECT url FROM webhook_configs WHERE channel = ?`, "discord").Scan(&stored); err != nil {
		t.Fatalf("read webhook: %v", err)
	}
	if stored != secret {
		t.Errorf("stored URL = %q, want the original — a redacted import clobbered a live credential", stored)
	}
}

func TestConfigRoundTripRestoresServices(t *testing.T) {
	src := newTestDB(t)
	mustExec(t, src, `INSERT INTO status_events (service_name, status, message, timestamp) VALUES (?,?,?,?)`,
		"api", "up", "ok", time.Now().UTC().Format(time.RFC3339))
	mustExec(t, src, `INSERT INTO service_groups (service_name, group_name) VALUES (?,?)`, "api", "edge")
	mustExec(t, src, `INSERT INTO active_monitors (service_name, monitor_type, target, interval_seconds, enabled) VALUES (?,?,?,?,?)`,
		"api", "http", "https://example.com/healthz", 60, 1)
	mustExec(t, src, `INSERT INTO service_alert_routes (service_name, channels) VALUES (?,?)`, "api", "discord")

	export, err := buildConfigExport(src, false)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	body, _ := json.Marshal(export)

	dst := newTestDB(t)
	// newTestScheduler avoids newMonitorPool's live worker goroutines: this
	// import enables an "http" monitor whose target is a real URL, and a real
	// worker would probe it over the network for the rest of the test binary's
	// life, racing on the package-level cert threshold vars other tests mutate.
	scheduler := newTestScheduler(dst)
	rec := httptest.NewRecorder()
	handleConfigImport(dst, scheduler)(rec, httptest.NewRequest(http.MethodPost, "/api/config/import", bytes.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("import = %d (%s)", rec.Code, rec.Body.String())
	}

	var group string
	if err := dst.QueryRow(`SELECT group_name FROM service_groups WHERE service_name = ?`, "api").Scan(&group); err != nil || group != "edge" {
		t.Errorf("group = %q (err %v), want edge", group, err)
	}
	var mt, target string
	if err := dst.QueryRow(`SELECT monitor_type, target FROM active_monitors WHERE service_name = ?`, "api").Scan(&mt, &target); err != nil {
		t.Fatalf("monitor not restored: %v", err)
	}
	if mt != "http" || target != "https://example.com/healthz" {
		t.Errorf("monitor = %s/%s, want http/https://example.com/healthz", mt, target)
	}
	if route := alertRouteFor(dst, "api"); len(route) != 1 || route[0] != "discord" {
		t.Errorf("alert route = %v, want [discord]", route)
	}
}

func TestImportSkipsBadEntriesWithoutDiscardingGoodOnes(t *testing.T) {
	db := newTestDB(t)
	// newTestScheduler avoids newMonitorPool's live worker goroutines: this
	// import enables a "tcp" monitor, and a real worker would call certStatusFor
	// for the rest of the test binary's life, racing on the package-level cert
	// threshold vars that other tests mutate.
	scheduler := newTestScheduler(db)

	payload := `{"services":[
      {"name":"good","group":"core","monitor":{"type":"tcp","target":"db:5432","interval_seconds":60,"enabled":true}},
      {"name":"bad","monitor":{"type":"telepathy","target":"???","interval_seconds":60,"enabled":true}}
    ]}`

	rec := httptest.NewRecorder()
	handleConfigImport(db, scheduler)(rec, httptest.NewRequest(http.MethodPost, "/api/config/import", strings.NewReader(payload)))
	if rec.Code != http.StatusOK {
		t.Fatalf("import = %d", rec.Code)
	}

	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["skipped"] == nil {
		t.Error("the invalid monitor was not reported as skipped")
	}

	var target string
	if err := db.QueryRow(`SELECT target FROM active_monitors WHERE service_name = ?`, "good").Scan(&target); err != nil {
		t.Fatalf("the valid service was not imported: %v", err)
	}
	if target != "db:5432" {
		t.Errorf("target = %q, want db:5432", target)
	}
	var n int
	_ = db.QueryRow(`SELECT COUNT(*) FROM active_monitors WHERE service_name = ?`, "bad").Scan(&n)
	if n != 0 {
		t.Error("the invalid monitor was imported anyway")
	}
}

// ---------------------------------------------------------------------------
// TLS certificate expiry classification
// ---------------------------------------------------------------------------

func TestCertStatusThresholds(t *testing.T) {
	origWarn, origCrit := certWarnDays, certCriticalDays
	t.Cleanup(func() { certWarnDays, certCriticalDays = origWarn, origCrit })
	certWarnDays, certCriticalDays = 30, 7

	for _, c := range []struct {
		days int
		want string
	}{
		{365, certStatusOK},
		{31, certStatusOK},
		{30, certStatusWarning},
		{8, certStatusWarning},
		{7, certStatusCritical},
		{1, certStatusCritical},
		{0, certStatusCritical},
		{-1, certStatusExpired},
		{-90, certStatusExpired},
	} {
		if got := certStatusFor(c.days); got != c.want {
			t.Errorf("certStatusFor(%d) = %q, want %q", c.days, got, c.want)
		}
	}
}

func TestCertThresholdsAreConfigurable(t *testing.T) {
	origWarn, origCrit := certWarnDays, certCriticalDays
	t.Cleanup(func() { certWarnDays, certCriticalDays = origWarn, origCrit })

	certWarnDays, certCriticalDays = 90, 45
	if got := certStatusFor(60); got != certStatusWarning {
		t.Errorf("60 days with a 90-day warning threshold = %q, want warning", got)
	}
	if got := certStatusFor(30); got != certStatusCritical {
		t.Errorf("30 days with a 45-day critical threshold = %q, want critical", got)
	}
}

func TestApplyCertFieldsSetsStatus(t *testing.T) {
	origWarn, origCrit := certWarnDays, certCriticalDays
	t.Cleanup(func() { certWarnDays, certCriticalDays = origWarn, origCrit })
	certWarnDays, certCriticalDays = 30, 7

	var m ActiveMonitor
	soon := time.Now().UTC().Add(72 * time.Hour).Format(time.RFC3339)
	applyCertFields(&m, sql.NullString{String: soon, Valid: true})

	if m.CertStatus != certStatusCritical {
		t.Errorf("CertStatus = %q, want critical", m.CertStatus)
	}
	if !m.CertWarning {
		t.Error("CertWarning should stay true for anything that is not ok")
	}
	if m.CertDaysRemaining == nil || *m.CertDaysRemaining > 3 {
		t.Errorf("CertDaysRemaining = %v, want about 2-3", m.CertDaysRemaining)
	}
}
