package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gorilla/mux"
)

// TestHandlePutMaintenanceRejectsCrossServiceScopedToken closes the same
// class of gap Fix 1 (v0.62.4) closed for the monitor handlers: a token
// scoped to one service must not be able to toggle maintenance mode — and
// therefore alert suppression — for a service it was not issued for.
func TestHandlePutMaintenanceRejectsCrossServiceScopedToken(t *testing.T) {
	db := newTestDB(t)
	const victim = "victim-service"
	if _, err := db.Exec(`INSERT INTO status_events (service_name, status, message, timestamp) VALUES (?,?,?,?)`,
		victim, "up", "seed", "2026-08-26T00:00:00Z"); err != nil {
		t.Fatal(err)
	}

	body, _ := json.Marshal(map[string]any{"enabled": true, "note": "hidden by attacker"})
	req := httptest.NewRequest(http.MethodPut, "/api/services/"+victim+"/maintenance", bytes.NewReader(body))
	req = mux.SetURLVars(req, map[string]string{"name": victim})
	req = req.WithContext(context.WithValue(req.Context(), scopedServiceKey, "attacker-service"))
	rec := httptest.NewRecorder()
	handlePutMaintenance(db)(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (body %s)", rec.Code, rec.Body.String())
	}

	var enabled int
	err := db.QueryRow(`SELECT enabled FROM service_maintenance WHERE service_name = ?`, victim).Scan(&enabled)
	if err == nil && enabled == 1 {
		t.Error("victim service was placed into maintenance despite the 403")
	}
}

// TestHandlePutMaintenanceAllowsMatchingScopedToken is the companion
// positive case: a token scoped to the same service the request targets
// must still work.
func TestHandlePutMaintenanceAllowsMatchingScopedToken(t *testing.T) {
	db := newTestDB(t)
	const svc = "self-service"
	if _, err := db.Exec(`INSERT INTO status_events (service_name, status, message, timestamp) VALUES (?,?,?,?)`,
		svc, "up", "seed", "2026-08-26T00:00:00Z"); err != nil {
		t.Fatal(err)
	}

	body, _ := json.Marshal(map[string]any{"enabled": true, "note": "self-service maintenance"})
	req := httptest.NewRequest(http.MethodPut, "/api/services/"+svc+"/maintenance", bytes.NewReader(body))
	req = mux.SetURLVars(req, map[string]string{"name": svc})
	req = req.WithContext(context.WithValue(req.Context(), scopedServiceKey, svc))
	rec := httptest.NewRecorder()
	handlePutMaintenance(db)(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
}

// TestHandlePostLoginRecordsAuditEntries covers the login-attempt half of
// the audit log: both a failed and a successful login must be persisted,
// closing the "throttling is in-memory only, no persisted record of login
// attempts" gap.
func TestHandlePostLoginRecordsAuditEntries(t *testing.T) {
	db, _ := newAuthDB(t, "admin", "correct-horse")

	badBody, _ := json.Marshal(loginRequest{Username: "admin", Password: "wrong-password"})
	badReq := httptest.NewRequest(http.MethodPost, "/api/auth/login", bytes.NewReader(badBody))
	badReq.RemoteAddr = "10.0.0.9:1234"
	rec := httptest.NewRecorder()
	handlePostLogin(db, newLoginThrottle())(rec, badReq)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad login = %d, want 401", rec.Code)
	}

	_ = loginFor(t, db, "admin", "correct-horse")

	rows, err := db.Query(`SELECT actor, action, success, ip FROM admin_audit_log ORDER BY id ASC`)
	if err != nil {
		t.Fatalf("query audit log: %v", err)
	}
	defer rows.Close()

	type row struct {
		actor, action, ip string
		success           int
	}
	var got []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.actor, &r.action, &r.success, &r.ip); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, r)
	}
	if len(got) != 2 {
		t.Fatalf("audit log has %d row(s), want 2 (%+v)", len(got), got)
	}
	if got[0].action != "login" || got[0].success != 0 || got[0].actor != "admin" {
		t.Errorf("row 0 = %+v, want failed login for admin", got[0])
	}
	if got[0].ip != "10.0.0.9" {
		t.Errorf("row 0 ip = %q, want 10.0.0.9", got[0].ip)
	}
	if got[1].action != "login" || got[1].success != 1 || got[1].actor != "admin" {
		t.Errorf("row 1 = %+v, want successful login for admin", got[1])
	}
}

// TestRecordAuditDistinguishesScopedTokenActorFromAdmin confirms the actor
// column names the scoped token's service rather than a generic "admin",
// so the audit trail can actually tell who did what.
func TestRecordAuditDistinguishesScopedTokenActorFromAdmin(t *testing.T) {
	db := newTestDB(t)
	sched := newTestScheduler(db)

	const svc = "audited-service"
	body, _ := json.Marshal(map[string]any{
		"monitor_type": "http", "target": "https://example.com/healthz", "interval_seconds": 60,
	})
	req := httptest.NewRequest(http.MethodPut, "/api/services/"+svc+"/monitor", bytes.NewReader(body))
	req = mux.SetURLVars(req, map[string]string{"name": svc})
	req = req.WithContext(context.WithValue(req.Context(), scopedServiceKey, svc))
	rec := httptest.NewRecorder()
	handlePutServiceMonitor(db, sched)(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}

	var actor, action, target string
	if err := db.QueryRow(`SELECT actor, action, target FROM admin_audit_log ORDER BY id DESC LIMIT 1`).
		Scan(&actor, &action, &target); err != nil {
		t.Fatalf("query audit log: %v", err)
	}
	if actor != "token:"+svc {
		t.Errorf("actor = %q, want %q", actor, "token:"+svc)
	}
	if action != "monitor_change" || target != svc {
		t.Errorf("action/target = %q/%q, want monitor_change/%s", action, target, svc)
	}
}

func TestHandleGetAuditLogOrdersNewestFirstAndRespectsLimit(t *testing.T) {
	db := newTestDB(t)
	for i := 0; i < 5; i++ {
		recordAuditAs(db, "admin", "127.0.0.1", "test_action", "svc", true, "")
	}

	r := mux.NewRouter()
	r.Handle("/api/admin/audit-log", handleGetAuditLog(db)).Methods(http.MethodGet)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/admin/audit-log?limit=3", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var entries []AuditEntry
	if err := json.Unmarshal(rec.Body.Bytes(), &entries); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("entries = %d, want 3 (limit respected)", len(entries))
	}
	for i := 0; i < len(entries)-1; i++ {
		if entries[i].ID < entries[i+1].ID {
			t.Fatalf("entries not newest-first: id[%d]=%d < id[%d]=%d", i, entries[i].ID, i+1, entries[i+1].ID)
		}
	}
}
