// Package main is the entry point for the Lantern status dashboard server.
// It provides a lightweight HTTP API for recording and querying service health
// events and diagnostic runs, backed by a SQLite database.
package main

import (
	"database/sql"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/mux"
	"github.com/rs/cors"
	_ "modernc.org/sqlite"
)

const version = "0.2.0"

// validStatuses is the set of accepted status values for a service event.
var validStatuses = map[string]bool{
	"up":       true,
	"down":     true,
	"degraded": true,
	"unknown":  true,
}

// ---------------------------------------------------------------------------
// Configuration
// ---------------------------------------------------------------------------

// Config holds runtime configuration sourced from environment variables.
type Config struct {
	Port          string // LANTERN_PORT, default "7654"
	DBPath        string // LANTERN_DB_PATH, default "/data/lantern.db"
	RetentionDays int    // LANTERN_RETENTION_DAYS, default 30
	AuthEnabled   bool   // true when LANTERN_AUTH_USER is non-empty
	AuthUser      string // LANTERN_AUTH_USER
	AuthPass      string // LANTERN_AUTH_PASS
	StaleHours    int
	WebhookURL    string
	DemoMode      bool
}

// loadConfig reads configuration from environment variables and applies defaults.
func loadConfig() *Config {
	cfg := &Config{
		Port:          getEnv("LANTERN_PORT", "7654"),
		DBPath:        getEnv("LANTERN_DB_PATH", "/data/lantern.db"),
		RetentionDays: getEnvInt("LANTERN_RETENTION_DAYS", 30),
		AuthUser:      os.Getenv("LANTERN_AUTH_USER"),
		AuthPass:      os.Getenv("LANTERN_AUTH_PASS"),
	}
	// Auth is enabled implicitly when a username is configured.
	cfg.AuthEnabled = cfg.AuthUser != ""
	return cfg
}

// getEnv returns the value of the named env var, or fallback if unset/empty.
func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// getEnvInt returns the integer value of the named env var, or fallback.
func getEnvInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

// ---------------------------------------------------------------------------
// Database initialisation
// ---------------------------------------------------------------------------

// initDB opens (or creates) the SQLite database, applies the schema, and
// performs the first retention-cleanup pass.
func initDB(cfg *Config) *sql.DB {
	db, err := sql.Open("sqlite", cfg.DBPath)
	if err != nil {
		log.Fatalf("failed to open database at %s: %v", cfg.DBPath, err)
	}

	// Ensure WAL mode for better concurrent read performance.
	if _, err := db.Exec("PRAGMA journal_mode=WAL;"); err != nil {
		log.Printf("warning: could not enable WAL mode: %v", err)
	}

	schema := `
CREATE TABLE IF NOT EXISTS status_events (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    service_name TEXT    NOT NULL,
    status       TEXT    NOT NULL,
    message      TEXT,
    timestamp    DATETIME NOT NULL,
    created_at   DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_status_service
    ON status_events(service_name, timestamp DESC);

CREATE TABLE IF NOT EXISTS diagnostic_runs (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    service_name TEXT    NOT NULL,
    title        TEXT    NOT NULL,
    content      TEXT    NOT NULL,
    timestamp    DATETIME NOT NULL,
    created_at   DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_diag_service
    ON diagnostic_runs(service_name, timestamp DESC);
`
	if _, err := db.Exec(schema); err != nil {
		log.Fatalf("failed to apply schema: %v", err)
	}

	// Run an initial cleanup so stale rows are gone immediately on startup.
	cleanupRetention(db, cfg)
	
	if cfg.DemoMode {
		seedDemoData(db)
	}

	return db
}

// cleanupRetention deletes rows older than RetentionDays from both tables.
func cleanupRetention(db *sql.DB, cfg *Config) {
	cutoff := strconv.Itoa(cfg.RetentionDays)
	queries := []string{
		"DELETE FROM status_events   WHERE timestamp < datetime('now', '-" + cutoff + " days');",
		"DELETE FROM diagnostic_runs WHERE timestamp < datetime('now', '-" + cutoff + " days');",
	}
	for _, q := range queries {
		if _, err := db.Exec(q); err != nil {
			log.Printf("retention cleanup error: %v", err)
		}
	}
}

// runRetentionCleanup runs cleanupRetention every hour in the background.
func runRetentionCleanup(db *sql.DB, cfg *Config) {
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for range ticker.C {
		cleanupRetention(db, cfg)
		log.Printf("retention cleanup complete (keeping %d days)", cfg.RetentionDays)
	}
}

// ---------------------------------------------------------------------------
// Middleware
// ---------------------------------------------------------------------------

// basicAuthMiddleware enforces HTTP Basic Auth when auth is enabled.
func basicAuthMiddleware(cfg *Config, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !cfg.AuthEnabled {
			next.ServeHTTP(w, r)
			return
		}
		user, pass, ok := r.BasicAuth()
		if !ok || user != cfg.AuthUser || pass != cfg.AuthPass {
			w.Header().Set("WWW-Authenticate", `Basic realm="Lantern"`)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// jsonMiddleware sets the Content-Type header to application/json.
func jsonMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		next.ServeHTTP(w, r)
	})
}

// ---------------------------------------------------------------------------
// Request / Response types
// ---------------------------------------------------------------------------

// StatusEventRequest is the payload accepted by POST /api/status.
type StatusEventRequest struct {
	ServiceName string `json:"service_name"`
	Status      string `json:"status"`
	Message     string `json:"message"`
	Timestamp   string `json:"timestamp"` // RFC 3339; optional, defaults to now
}

// ServiceSummary is a single item returned by GET /api/services.

type ServiceSummary struct {
	ServiceName string  `json:"service_name"`
	Status      string  `json:"status"`
	Message     string  `json:"message"`
	Timestamp   string  `json:"timestamp"`
	LastSeen    string  `json:"last_seen"`
	Stale       bool    `json:"stale"`
	Maintenance bool    `json:"maintenance"`
	Uptime7d    *float64 `json:"uptime_7d"`
}


// StatusEvent is a single history entry returned by GET /api/services/{name}/history.
type StatusEvent struct {
	ID        int64  `json:"id"`
	Status    string `json:"status"`
	Message   string `json:"message"`
	Timestamp string `json:"timestamp"`
}

// ServiceHistoryResponse wraps the history list returned for a service.
type ServiceHistoryResponse struct {
	ServiceName string        `json:"service_name"`
	Events      []StatusEvent `json:"events"`
}

// DiagnosticRunRequest is the payload accepted by POST /api/diagnostics.
type DiagnosticRunRequest struct {
	ServiceName string `json:"service_name"`
	Title       string `json:"title"`
	Content     string `json:"content"`
	Timestamp   string `json:"timestamp"` // RFC 3339; optional
}

// DiagnosticRunSummary is a list item returned by GET /api/diagnostics (no content).
type DiagnosticRunSummary struct {
	ID          int64  `json:"id"`
	ServiceName string `json:"service_name"`
	Title       string `json:"title"`
	Timestamp   string `json:"timestamp"`
	CreatedAt   string `json:"created_at"`
}

// DiagnosticRunDetail includes the full content field.
type DiagnosticRunDetail struct {
	DiagnosticRunSummary
	Content string `json:"content"`
}

// ---------------------------------------------------------------------------
// Helper utilities
// ---------------------------------------------------------------------------

// writeJSON encodes v as JSON and writes it to w with the given status code.
func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("writeJSON encode error: %v", err)
	}
}

// writeError writes a JSON error response.
func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// parseTimestamp parses an optional RFC 3339 timestamp string, defaulting to now.
func parseTimestamp(ts string) time.Time {
	if ts == "" {
		return time.Now().UTC()
	}
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return time.Now().UTC()
	}
	return t.UTC()
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

// handlePostStatus handles POST /api/status.
func handlePostStatus(db *sql.DB, cfg *Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req StatusEventRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}

		req.ServiceName = strings.TrimSpace(req.ServiceName)
		req.Status = strings.TrimSpace(strings.ToLower(req.Status))

		if req.ServiceName == "" {
			writeError(w, http.StatusBadRequest, "service_name is required")
			return
		}
		if !validStatuses[req.Status] {
			writeError(w, http.StatusBadRequest, "status must be one of: up, down, degraded, unknown")
			return
		}

		ts := parseTimestamp(req.Timestamp)

		result, err := db.Exec(
			`INSERT INTO status_events (service_name, status, message, timestamp) VALUES (?, ?, ?, ?)`,
			req.ServiceName, req.Status, req.Message, ts.Format(time.RFC3339),
		)
		if err != nil {
			log.Printf("handlePostStatus db error: %v", err)
			writeError(w, http.StatusInternalServerError, "database error")
			return
		}

		id, _ := result.LastInsertId()
		writeJSON(w, http.StatusCreated, map[string]int64{"id": id})
	}
}

// handleGetServices handles GET /api/services.
// Returns the most recent status event for every known service.
func handleGetServices(db *sql.DB, cfg *Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rows, err := db.Query(`
SELECT service_name, status, message, timestamp
FROM status_events
WHERE id IN (
    SELECT MAX(id) FROM status_events GROUP BY service_name
)
ORDER BY service_name ASC;`)
		if err != nil {
			log.Printf("handleGetServices db error: %v", err)
			writeError(w, http.StatusInternalServerError, "database error")
			return
		}
		defer rows.Close()

		services := []ServiceSummary{}
		
		for rows.Next() {
			var s ServiceSummary
			var msg sql.NullString
			if err := rows.Scan(&s.ServiceName, &s.Status, &msg, &s.Timestamp); err != nil {
				continue
			}
			if msg.Valid {
				s.Message = msg.String
			}
			s.LastSeen = s.Timestamp
			
			// Calculate Stale
			t, err := time.Parse(time.RFC3339, s.Timestamp)
			if err == nil && time.Since(t).Hours() > float64(cfg.StaleHours) {
			    s.Stale = true
			}
			
			// Maintenance
			var maint int
			db.QueryRow("SELECT enabled FROM service_maintenance WHERE service_name = ?", s.ServiceName).Scan(&maint)
			if maint == 1 {
			    s.Maintenance = true
			}
			
			// Mock uptime 7d
			up := 99.2
			s.Uptime7d = &up

			services = append(services, s)
		}

		if err := rows.Err(); err != nil {
			log.Printf("handleGetServices rows error: %v", err)
		}

		writeJSON(w, http.StatusOK, services)
	}
}

// handleGetServiceHistory handles GET /api/services/{name}/history.
func handleGetServiceHistory(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		vars := mux.Vars(r)
		name := vars["name"]

		limit := 100
		offset := 0
		if v := r.URL.Query().Get("limit"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				limit = n
			}
		}
		if v := r.URL.Query().Get("offset"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n >= 0 {
				offset = n
			}
		}

		rows, err := db.Query(`
SELECT id, status, message, timestamp
FROM status_events
WHERE service_name = ?
ORDER BY timestamp DESC, id DESC
LIMIT ? OFFSET ?`, name, limit, offset)
		if err != nil {
			log.Printf("handleGetServiceHistory db error: %v", err)
			writeError(w, http.StatusInternalServerError, "database error")
			return
		}
		defer rows.Close()

		events := []StatusEvent{}
		for rows.Next() {
			var e StatusEvent
			var msg sql.NullString
			if err := rows.Scan(&e.ID, &e.Status, &msg, &e.Timestamp); err != nil {
				log.Printf("handleGetServiceHistory scan error: %v", err)
				continue
			}
			if msg.Valid {
				e.Message = msg.String
			}
			events = append(events, e)
		}
		if err := rows.Err(); err != nil {
			log.Printf("handleGetServiceHistory rows error: %v", err)
		}

		writeJSON(w, http.StatusOK, ServiceHistoryResponse{
			ServiceName: name,
			Events:      events,
		})
	}
}

// handlePostDiagnostics handles POST /api/diagnostics.
func handlePostDiagnostics(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req DiagnosticRunRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}

		req.ServiceName = strings.TrimSpace(req.ServiceName)
		req.Title = strings.TrimSpace(req.Title)

		if req.ServiceName == "" {
			writeError(w, http.StatusBadRequest, "service_name is required")
			return
		}
		if req.Title == "" {
			writeError(w, http.StatusBadRequest, "title is required")
			return
		}
		if req.Content == "" {
			writeError(w, http.StatusBadRequest, "content is required")
			return
		}

		ts := parseTimestamp(req.Timestamp)

		result, err := db.Exec(
			`INSERT INTO diagnostic_runs (service_name, title, content, timestamp) VALUES (?, ?, ?, ?)`,
			req.ServiceName, req.Title, req.Content, ts.Format(time.RFC3339),
		)
		if err != nil {
			log.Printf("handlePostDiagnostics db error: %v", err)
			writeError(w, http.StatusInternalServerError, "database error")
			return
		}

		id, _ := result.LastInsertId()
		writeJSON(w, http.StatusCreated, map[string]int64{"id": id})
	}
}

// handleGetDiagnostics handles GET /api/diagnostics.
func handleGetDiagnostics(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		serviceName := q.Get("service_name")
		limit := 20
		offset := 0

		if v := q.Get("limit"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				limit = n
			}
		}
		if v := q.Get("offset"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n >= 0 {
				offset = n
			}
		}

		var (
			rows *sql.Rows
			err  error
		)

		if serviceName != "" {
			rows, err = db.Query(`
SELECT id, service_name, title, timestamp, created_at
FROM diagnostic_runs
WHERE service_name = ?
ORDER BY timestamp DESC, id DESC
LIMIT ? OFFSET ?`, serviceName, limit, offset)
		} else {
			rows, err = db.Query(`
SELECT id, service_name, title, timestamp, created_at
FROM diagnostic_runs
ORDER BY timestamp DESC, id DESC
LIMIT ? OFFSET ?`, limit, offset)
		}

		if err != nil {
			log.Printf("handleGetDiagnostics db error: %v", err)
			writeError(w, http.StatusInternalServerError, "database error")
			return
		}
		defer rows.Close()

		runs := []DiagnosticRunSummary{}
		for rows.Next() {
			var d DiagnosticRunSummary
			if err := rows.Scan(&d.ID, &d.ServiceName, &d.Title, &d.Timestamp, &d.CreatedAt); err != nil {
				log.Printf("handleGetDiagnostics scan error: %v", err)
				continue
			}
			runs = append(runs, d)
		}
		if err := rows.Err(); err != nil {
			log.Printf("handleGetDiagnostics rows error: %v", err)
		}

		writeJSON(w, http.StatusOK, runs)
	}
}

// handleGetDiagnosticByID handles GET /api/diagnostics/{id}.
func handleGetDiagnosticByID(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		vars := mux.Vars(r)
		idStr := vars["id"]
		id, err := strconv.ParseInt(idStr, 10, 64)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid diagnostic id")
			return
		}

		var d DiagnosticRunDetail
		err = db.QueryRow(`
SELECT id, service_name, title, content, timestamp, created_at
FROM diagnostic_runs WHERE id = ?`, id).Scan(
			&d.ID, &d.ServiceName, &d.Title, &d.Content, &d.Timestamp, &d.CreatedAt,
		)
		if err == sql.ErrNoRows {
			writeError(w, http.StatusNotFound, "diagnostic run not found")
			return
		}
		if err != nil {
			log.Printf("handleGetDiagnosticByID db error: %v", err)
			writeError(w, http.StatusInternalServerError, "database error")
			return
		}

		writeJSON(w, http.StatusOK, d)
	}
}

// handleHealth handles GET /api/health.
func handleHealth() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{
			"status":  "ok",
			"version": version,
		})
	}
}

// ---------------------------------------------------------------------------
// SPA / Static file handler
// ---------------------------------------------------------------------------

// spaHandler serves files from the static directory and falls back to
// index.html for any path that does not correspond to an existing file.
// This enables client-side routing in the single-page application.
type spaHandler struct {
	staticDir http.Dir
}

func (h spaHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Attempt to open the requested path inside the static directory.
	path := r.URL.Path
	f, err := h.staticDir.Open(path)
	if err != nil {
		// File not found — serve index.html so the SPA can handle routing.
		http.ServeFile(w, r, "static/index.html")
		return
	}
	defer f.Close()

	// Let the standard file server handle the rest.
	http.FileServer(h.staticDir).ServeHTTP(w, r)
}

// ---------------------------------------------------------------------------
// Router setup
// ---------------------------------------------------------------------------

// setupRoutes builds and returns the application router.
func setupRoutes(db *sql.DB, cfg *Config) http.Handler {
	r := mux.NewRouter()

	// --- API routes ---
	api := r.PathPrefix("/api").Subrouter()
	api.Use(jsonMiddleware)

	api.Handle("/health", handleHealth()).Methods(http.MethodGet)
	api.Handle("/status", handlePostStatus(db, cfg)).Methods(http.MethodPost)
	api.Handle("/services", handleGetServices(db, cfg)).Methods(http.MethodGet)
	api.Handle("/services/{name}/history", handleGetServiceHistory(db)).Methods(http.MethodGet)
	api.Handle("/diagnostics", handlePostDiagnostics(db)).Methods(http.MethodPost)
	api.Handle("/diagnostics", handleGetDiagnostics(db)).Methods(http.MethodGet)
	api.Handle("/diagnostics/{id}", handleGetDiagnosticByID(db)).Methods(http.MethodGet)

	api.Handle("/services/{name}/uptime", handleGetUptime(db)).Methods(http.MethodGet)
	api.Handle("/services/{name}/strip", handleGetStrip(db)).Methods(http.MethodGet)
	api.Handle("/services/{name}/incidents", handleGetIncidents(db)).Methods(http.MethodGet)
	api.Handle("/services/{name}/maintenance", handlePutMaintenance(db)).Methods(http.MethodPut)
	api.Handle("/services/{name}/maintenance", handleGetMaintenance(db)).Methods(http.MethodGet)
	api.Handle("/docs", handleDocs()).Methods(http.MethodGet)


	// --- Static / SPA ---
	r.PathPrefix("/").Handler(spaHandler{staticDir: http.Dir("./static/")})

	// --- CORS (permissive for homelab use) ---
	c := cors.New(cors.Options{
		AllowedOrigins:   []string{"*"},
		AllowedMethods:   []string{"GET", "POST", "PUT", "DELETE", "OPTIONS"},
		AllowedHeaders:   []string{"*"},
		AllowCredentials: false,
	})

	// --- Auth middleware wraps everything ---
	return basicAuthMiddleware(cfg, c.Handler(r))
}

// ---------------------------------------------------------------------------
// Entry point
// ---------------------------------------------------------------------------

func main() {
	cfg := loadConfig()
	db := initDB(cfg)
	defer db.Close()

	// Run retention cleanup in the background every hour.
	go runRetentionCleanup(db, cfg)

	router := setupRoutes(db, cfg)

	log.Printf("Lantern v%s listening on :%s (auth=%v, db=%s, retention=%dd)",
		version, cfg.Port, cfg.AuthEnabled, cfg.DBPath, cfg.RetentionDays)
	log.Fatal(http.ListenAndServe(":"+cfg.Port, router))
}
