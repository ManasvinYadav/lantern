package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func setSchedule(t *testing.T, db *sql.DB, enabled bool, startMin, endMin int, mode string) {
	t.Helper()
	enabledInt := 0
	if enabled {
		enabledInt = 1
	}
	mustExec(t, db, `
INSERT INTO notification_schedule (id, enabled, start_minute, end_minute, mode) VALUES (1,?,?,?,?)
ON CONFLICT(id) DO UPDATE SET enabled = excluded.enabled, start_minute = excluded.start_minute,
    end_minute = excluded.end_minute, mode = excluded.mode`,
		enabledInt, startMin, endMin, mode)
}

func atUTC(hour, min int) time.Time {
	return time.Date(2026, 1, 1, hour, min, 0, 0, time.UTC)
}

func TestQuietHoursActiveHandlesWrapAndNonWrapWindows(t *testing.T) {
	db := newTestDB(t)
	setSchedule(t, db, true, 22*60, 8*60, "mute") // 22:00-08:00, wraps midnight

	cases := []struct {
		label string
		at    time.Time
		want  bool
	}{
		{"well before window", atUTC(12, 0), false},
		{"exactly at start", atUTC(22, 0), true},
		{"after midnight, inside", atUTC(2, 0), true},
		{"exactly at end", atUTC(8, 0), false},
		{"just after end", atUTC(8, 1), false},
		{"just before start", atUTC(21, 59), false},
	}
	for _, c := range cases {
		t.Run(c.label, func(t *testing.T) {
			active, mode := quietHoursActive(db, c.at)
			if active != c.want {
				t.Errorf("active = %v, want %v", active, c.want)
			}
			if c.want && mode != "mute" {
				t.Errorf("mode = %q, want mute", mode)
			}
		})
	}
}

func TestQuietHoursActiveNonWrappingWindow(t *testing.T) {
	db := newTestDB(t)
	setSchedule(t, db, true, 9*60, 17*60, "digest") // 09:00-17:00, same-day

	if active, _ := quietHoursActive(db, atUTC(8, 59)); active {
		t.Error("08:59 should be outside a 09:00-17:00 window")
	}
	if active, mode := quietHoursActive(db, atUTC(12, 0)); !active || mode != "digest" {
		t.Errorf("12:00 = (%v, %q), want (true, digest)", active, mode)
	}
	if active, _ := quietHoursActive(db, atUTC(17, 0)); active {
		t.Error("17:00 (the end minute) should be outside the window")
	}
}

func TestQuietHoursActiveDisabledOrZeroWindow(t *testing.T) {
	db := newTestDB(t)

	// No row at all: getNotificationSchedule falls back to a disabled default.
	if active, _ := quietHoursActive(db, atUTC(23, 0)); active {
		t.Error("no schedule configured should never be active")
	}

	setSchedule(t, db, false, 22*60, 8*60, "mute")
	if active, _ := quietHoursActive(db, atUTC(23, 0)); active {
		t.Error("disabled schedule should never be active even inside its window")
	}

	setSchedule(t, db, true, 60, 60, "mute") // start == end
	if active, _ := quietHoursActive(db, atUTC(1, 0)); active {
		t.Error("a zero-length window (start == end) should never be active")
	}
}

// windowAroundNow returns a schedule window guaranteed to contain the
// current wall-clock minute, for exercising ingestStatusEvent's real
// time.Now()-based quiet-hours check without depending on when the test
// happens to run.
func windowAroundNow(marginMin int) (start, end int) {
	now := time.Now().UTC()
	minute := now.Hour()*60 + now.Minute()
	start = ((minute-marginMin)%1440 + 1440) % 1440
	end = (minute + marginMin) % 1440
	return
}

func TestIngestStatusEventMuteDropsNotificationWithoutQueuing(t *testing.T) {
	db := newTestDB(t)
	dispatcher := newWebhookDispatcher(db, 1)
	cfg := &Config{}
	hub := newWSHub()

	start, end := windowAroundNow(5)
	setSchedule(t, db, true, start, end, "mute")
	mustExec(t, db, `INSERT INTO webhook_configs (channel, url) VALUES (?, ?)`, "generic", "http://example.invalid/hook")

	// shouldNotify flap-dampens "down": it takes two consecutive down beats
	// before an alert fires, so the sequence has to include both.
	if _, err := ingestStatusEvent(db, cfg, dispatcher, hub, "svc", "up", "seed", time.Now(), 0); err != nil {
		t.Fatalf("seed ingest: %v", err)
	}
	if _, err := ingestStatusEvent(db, cfg, dispatcher, hub, "svc", "down", "boom", time.Now(), 0); err != nil {
		t.Fatalf("first down ingest: %v", err)
	}
	if _, err := ingestStatusEvent(db, cfg, dispatcher, hub, "svc", "down", "still down", time.Now(), 0); err != nil {
		t.Fatalf("second down ingest: %v", err)
	}

	var queued int
	if err := db.QueryRow(`SELECT COUNT(*) FROM notification_digest_queue`).Scan(&queued); err != nil {
		t.Fatal(err)
	}
	if queued != 0 {
		t.Errorf("digest queue = %d, want 0 in mute mode", queued)
	}
	var delivered int
	if err := db.QueryRow(`SELECT COUNT(*) FROM webhook_deliveries`).Scan(&delivered); err != nil {
		t.Fatal(err)
	}
	if delivered != 0 {
		t.Errorf("webhook_deliveries = %d, want 0 (muted)", delivered)
	}
}

func TestIngestStatusEventDigestModeQueuesInsteadOfSending(t *testing.T) {
	db := newTestDB(t)
	dispatcher := newWebhookDispatcher(db, 1)
	cfg := &Config{}
	hub := newWSHub()

	start, end := windowAroundNow(5)
	setSchedule(t, db, true, start, end, "digest")
	mustExec(t, db, `INSERT INTO webhook_configs (channel, url) VALUES (?, ?)`, "generic", "http://example.invalid/hook")

	// shouldNotify flap-dampens "down": it takes two consecutive down beats
	// before an alert fires, so the sequence has to include both.
	if _, err := ingestStatusEvent(db, cfg, dispatcher, hub, "svc", "up", "seed", time.Now(), 0); err != nil {
		t.Fatalf("seed ingest: %v", err)
	}
	if _, err := ingestStatusEvent(db, cfg, dispatcher, hub, "svc", "down", "boom", time.Now(), 0); err != nil {
		t.Fatalf("first down ingest: %v", err)
	}
	if _, err := ingestStatusEvent(db, cfg, dispatcher, hub, "svc", "down", "still down", time.Now(), 0); err != nil {
		t.Fatalf("second down ingest: %v", err)
	}

	var svc, oldStatus, newStatus string
	if err := db.QueryRow(`SELECT service_name, old_status, new_status FROM notification_digest_queue`).
		Scan(&svc, &oldStatus, &newStatus); err != nil {
		t.Fatalf("expected one queued digest event: %v", err)
	}
	if svc != "svc" || oldStatus != "down" || newStatus != "down" {
		t.Errorf("queued event = %s %s->%s, want svc down->down (the confirming second beat)", svc, oldStatus, newStatus)
	}
	var queued int
	if err := db.QueryRow(`SELECT COUNT(*) FROM notification_digest_queue`).Scan(&queued); err != nil {
		t.Fatal(err)
	}
	if queued != 1 {
		t.Errorf("digest queue = %d, want 1 (only the confirmed down alert fires)", queued)
	}
	var delivered int
	if err := db.QueryRow(`SELECT COUNT(*) FROM webhook_deliveries`).Scan(&delivered); err != nil {
		t.Fatal(err)
	}
	if delivered != 0 {
		t.Errorf("webhook_deliveries = %d, want 0 (queued, not yet sent)", delivered)
	}
}

// TestFlushDigestIfDueSendsOnceWindowClosesAndRespectsRouting is the
// end-to-end path: a digest queued while the window was open is combined
// into one delivery per channel once the window has closed, and a service
// routed away from a channel is excluded from that channel's digest.
func TestFlushDigestIfDueSendsOnceWindowClosesAndRespectsRouting(t *testing.T) {
	db := newTestDB(t)
	dispatcher := newWebhookDispatcher(db, 2)
	cfg := &Config{}

	var genericHits, discordHits int
	var mu sync.Mutex
	genericSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		genericHits++
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer genericSrv.Close()
	discordSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		discordHits++
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer discordSrv.Close()

	mustExec(t, db, `INSERT INTO webhook_configs (channel, url) VALUES (?, ?)`, "generic", genericSrv.URL)
	mustExec(t, db, `INSERT INTO webhook_configs (channel, url) VALUES (?, ?)`, "discord", discordSrv.URL)

	// "routed-out" only accepts discord alerts, so its queued event must be
	// excluded from the generic digest.
	mustExec(t, db, `INSERT INTO service_alert_routes (service_name, channels) VALUES (?, ?)`, "routed-out", "discord")

	// The window is already closed (disabled), so flushDigestIfDue should
	// flush immediately once events are queued directly.
	queueDigestEvent(db, "svc-a", "up", "down", "boom", time.Now())
	queueDigestEvent(db, "routed-out", "up", "down", "boom", time.Now())

	flushDigestIfDue(dispatcher, db, cfg)

	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		g, d := genericHits, discordHits
		mu.Unlock()
		if g >= 1 && d >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for digest delivery: generic=%d discord=%d", g, d)
		}
		time.Sleep(10 * time.Millisecond)
	}

	var remaining int
	if err := db.QueryRow(`SELECT COUNT(*) FROM notification_digest_queue`).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 0 {
		t.Errorf("digest queue = %d after flush, want 0", remaining)
	}

	var genericDeliveries int
	if err := db.QueryRow(`SELECT COUNT(*) FROM webhook_deliveries WHERE channel = 'generic'`).Scan(&genericDeliveries); err != nil {
		t.Fatal(err)
	}
	if genericDeliveries != 1 {
		t.Errorf("generic deliveries = %d, want exactly 1 combined digest", genericDeliveries)
	}
}

func TestFlushDigestIfDueDoesNothingWhileWindowStillOpen(t *testing.T) {
	db := newTestDB(t)
	dispatcher := newWebhookDispatcher(db, 1)
	cfg := &Config{}

	start, end := windowAroundNow(5)
	setSchedule(t, db, true, start, end, "digest")
	mustExec(t, db, `INSERT INTO webhook_configs (channel, url) VALUES (?, ?)`, "generic", "http://example.invalid/hook")

	queueDigestEvent(db, "svc-a", "up", "down", "boom", time.Now())
	flushDigestIfDue(dispatcher, db, cfg)

	var remaining int
	if err := db.QueryRow(`SELECT COUNT(*) FROM notification_digest_queue`).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 1 {
		t.Errorf("digest queue = %d, want 1 (window still open, must not flush)", remaining)
	}
}

func TestHandlePutNotificationScheduleValidatesAndAudits(t *testing.T) {
	db := newTestDB(t)

	bad, _ := json.Marshal(map[string]any{"enabled": true, "start_minute": 0, "end_minute": 60, "mode": "explode"})
	req := httptest.NewRequest(http.MethodPut, "/api/notifications/schedule", bytes.NewReader(bad))
	rec := httptest.NewRecorder()
	handlePutNotificationSchedule(db)(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid mode = %d, want 400", rec.Code)
	}

	outOfRange, _ := json.Marshal(map[string]any{"enabled": true, "start_minute": 5000, "end_minute": 60, "mode": "mute"})
	req = httptest.NewRequest(http.MethodPut, "/api/notifications/schedule", bytes.NewReader(outOfRange))
	rec = httptest.NewRecorder()
	handlePutNotificationSchedule(db)(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("out-of-range minute = %d, want 400", rec.Code)
	}

	good, _ := json.Marshal(map[string]any{"enabled": true, "start_minute": 1320, "end_minute": 480, "mode": "digest"})
	req = httptest.NewRequest(http.MethodPut, "/api/notifications/schedule", bytes.NewReader(good))
	rec = httptest.NewRecorder()
	handlePutNotificationSchedule(db)(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("valid update = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}

	got := getNotificationSchedule(db)
	if !got.Enabled || got.StartMinute != 1320 || got.EndMinute != 480 || got.Mode != "digest" {
		t.Errorf("schedule after PUT = %+v, want enabled=true start=1320 end=480 mode=digest", got)
	}

	var action string
	if err := db.QueryRow(`SELECT action FROM admin_audit_log ORDER BY id DESC LIMIT 1`).Scan(&action); err != nil {
		t.Fatalf("expected an audit entry for the successful PUT: %v", err)
	}
	if action != "notification_schedule_change" {
		t.Errorf("audit action = %q, want notification_schedule_change", action)
	}
}

func TestScopedTokenCannotChangeGlobalNotificationSchedule(t *testing.T) {
	db, cfg := newAuthDB(t, "admin", "correct-horse")
	if _, err := db.Exec(`INSERT INTO api_tokens (token, service_name) VALUES (?, ?)`, "scoped-token", "webapp"); err != nil {
		t.Fatal(err)
	}

	body, _ := json.Marshal(map[string]any{"enabled": true, "start_minute": 0, "end_minute": 60, "mode": "mute"})
	req := httptest.NewRequest(http.MethodPut, "/api/notifications/schedule", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer scoped-token")
	rec := httptest.NewRecorder()
	authMiddleware(db, cfg, handlePutNotificationSchedule(db)).ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("scoped token PUT schedule = %d, want 403", rec.Code)
	}
}
