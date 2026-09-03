package main

import (
	"database/sql"
	"log"
	"net/http"
	"strconv"
	"time"
)

// AuditEntry is one row of the admin action audit log, returned by
// GET /api/admin/audit-log.
type AuditEntry struct {
	ID        int64  `json:"id"`
	Actor     string `json:"actor"`
	Action    string `json:"action"`
	Target    string `json:"target,omitempty"`
	Detail    string `json:"detail,omitempty"`
	Success   bool   `json:"success"`
	IP        string `json:"ip"`
	Timestamp string `json:"timestamp"`
}

// auditActor identifies who performed a mutating admin request. Lantern has
// no multi-user model yet (see the RBAC feature-gap note), so every session,
// Basic Auth, or admin-bearer-token caller shares one identity; a scoped
// token is distinguished by the service it was issued for.
func auditActor(r *http.Request) string {
	if svc, ok := r.Context().Value(scopedServiceKey).(string); ok && svc != "" {
		return "token:" + svc
	}
	return "admin"
}

// recordAudit appends one entry to the admin action audit log. A logging
// failure is itself only logged, never surfaced to the caller — the audit
// trail must not be able to block the action it records.
func recordAudit(db *sql.DB, r *http.Request, action, target string, success bool, detail string) {
	recordAuditAs(db, auditActor(r), throttleKey(r), action, target, success, detail)
}

// recordAuditAs is recordAudit with an explicit actor and IP, for call sites
// like login attempts where the request has not (or has not yet, on
// failure) authenticated as anyone recordAudit could name from context.
func recordAuditAs(db *sql.DB, actor, ip, action, target string, success bool, detail string) {
	successInt := 0
	if success {
		successInt = 1
	}
	if _, err := db.Exec(
		`INSERT INTO admin_audit_log (actor, action, target, detail, success, ip, created_at) VALUES (?,?,?,?,?,?,?)`,
		actor, action, target, detail, successInt, ip, time.Now().UTC().Format(time.RFC3339),
	); err != nil {
		log.Printf("audit: failed to record %s on %q: %v", action, target, err)
	}
}

// handleGetAuditLog handles GET /api/admin/audit-log. Admin-only: see
// isAdminOnlyEndpoint.
func handleGetAuditLog(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		limit := 100
		if v := r.URL.Query().Get("limit"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 500 {
				limit = n
			}
		}

		rows, err := db.Query(
			`SELECT id, actor, action, target, COALESCE(detail, ''), success, ip, created_at
			 FROM admin_audit_log ORDER BY id DESC LIMIT ?`, limit)
		if err != nil {
			log.Printf("handleGetAuditLog db error: %v", err)
			writeError(w, http.StatusInternalServerError, "database error")
			return
		}
		defer rows.Close()

		entries := []AuditEntry{}
		for rows.Next() {
			var e AuditEntry
			var success int
			if err := rows.Scan(&e.ID, &e.Actor, &e.Action, &e.Target, &e.Detail, &success, &e.IP, &e.Timestamp); err != nil {
				continue
			}
			e.Success = success == 1
			entries = append(entries, e)
		}
		if err := rows.Err(); err != nil {
			log.Printf("handleGetAuditLog rows error: %v", err)
			writeError(w, http.StatusInternalServerError, "database error")
			return
		}

		writeJSON(w, http.StatusOK, entries)
	}
}
