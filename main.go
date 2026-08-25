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

	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"sync"

	"github.com/gorilla/mux"
	"github.com/gorilla/websocket"
	"github.com/rs/cors"
	_ "modernc.org/sqlite"
)

const version = "0.4.0"

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
	Port            string // LANTERN_PORT, default "7654"
	DBPath          string // LANTERN_DB_PATH, default "/data/lantern.db"
	RetentionDays   int    // LANTERN_RETENTION_DAYS, default 30
	AuthEnabled     bool   // true when LANTERN_AUTH_USER is non-empty
	AuthUser        string // LANTERN_AUTH_USER
	AuthPass        string // LANTERN_AUTH_PASS
	StaleHours      int
	WebhookURL      string
	DemoMode        bool
	WebhookDiscord  string
	WebhookTelegram string
	WebhookGotify   string
	WebhookGeneric  string
}

// loadConfig reads configuration from environment variables and applies defaults.
func loadConfig() *Config {
	cfg := &Config{
		Port:            getEnv("LANTERN_PORT", "7654"),
		DBPath:          getEnv("LANTERN_DB_PATH", "/data/lantern.db"),
		RetentionDays:   getEnvInt("LANTERN_RETENTION_DAYS", 30),
		AuthUser:        os.Getenv("LANTERN_AUTH_USER"),
		AuthPass:        os.Getenv("LANTERN_AUTH_PASS"),
		StaleHours:      getEnvInt("LANTERN_STALE_HOURS", 2),
		WebhookURL:      os.Getenv("LANTERN_WEBHOOK_URL"),
		DemoMode:        os.Getenv("LANTERN_DEMO") == "true",
		WebhookDiscord:  os.Getenv("LANTERN_WEBHOOK_DISCORD"),
		WebhookTelegram: os.Getenv("LANTERN_WEBHOOK_TELEGRAM"),
		WebhookGotify:   os.Getenv("LANTERN_WEBHOOK_GOTIFY"),
		WebhookGeneric:  os.Getenv("LANTERN_WEBHOOK_GENERIC"),
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
	// _pragma=busy_timeout(5000) is applied by the driver to every pooled
	// connection as it's opened, so concurrent writers wait up to 5s for the
	// SQLite write lock instead of failing immediately with SQLITE_BUSY.
	dsn := cfg.DBPath
	if strings.Contains(dsn, "?") {
		dsn += "&_pragma=busy_timeout(5000)"
	} else {
		dsn += "?_pragma=busy_timeout(5000)"
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		log.Fatalf("failed to open database at %s: %v", cfg.DBPath, err)
	}

	// Ensure WAL mode for better concurrent read performance. journal_mode is
	// persisted in the database file header, so this only needs to run once
	// regardless of which pooled connection executes it.
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

CREATE INDEX IF NOT EXISTS idx_status_events_service_id
    ON status_events(service_name, id DESC);

CREATE INDEX IF NOT EXISTS idx_status_events_service_ts
    ON status_events(service_name, timestamp ASC);

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

CREATE TABLE IF NOT EXISTS service_maintenance (
    service_name TEXT PRIMARY KEY,
    enabled      INTEGER NOT NULL DEFAULT 0,
    note         TEXT,
    updated_at   DATETIME
);

CREATE TABLE IF NOT EXISTS maintenance_windows (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    service_name TEXT NOT NULL,
    started_at   DATETIME NOT NULL,
    ended_at     DATETIME,
    note         TEXT
);

CREATE INDEX IF NOT EXISTS idx_maint_windows_service
    ON maintenance_windows(service_name, started_at);

CREATE TABLE IF NOT EXISTS api_tokens (
    token TEXT PRIMARY KEY,
    service_name TEXT NOT NULL,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS webhook_configs (
    channel    TEXT PRIMARY KEY,
    url        TEXT NOT NULL,
    updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS service_groups (
    service_name TEXT PRIMARY KEY,
    group_name   TEXT NOT NULL DEFAULT '',
    updated_at   DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS webhook_deliveries (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    channel      TEXT    NOT NULL,
    service_name TEXT    NOT NULL,
    old_status   TEXT,
    new_status   TEXT,
    success      INTEGER NOT NULL DEFAULT 0,
    http_status  INTEGER,
    error        TEXT,
    created_at   DATETIME NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_webhook_deliveries_created
    ON webhook_deliveries(created_at DESC);
`
	if _, err := db.Exec(schema); err != nil {
		log.Fatalf("failed to apply schema: %v", err)
	}

	// Schema migration for webhook_configs updated_at column
	_, _ = db.Exec("ALTER TABLE webhook_configs ADD COLUMN updated_at DATETIME DEFAULT CURRENT_TIMESTAMP;")

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

// authMiddleware enforces HTTP Basic Auth or Bearer tokens.
func authMiddleware(db *sql.DB, cfg *Config, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/public/") {
			next.ServeHTTP(w, r)
			return
		}

		authHeader := r.Header.Get("Authorization")
		if strings.HasPrefix(authHeader, "Bearer ") {
			token := strings.TrimPrefix(authHeader, "Bearer ")
			var serviceName string
			err := db.QueryRow("SELECT service_name FROM api_tokens WHERE token = ?", token).Scan(&serviceName)
			if err == nil {
				ctx := context.WithValue(r.Context(), "scoped_service", serviceName)
				next.ServeHTTP(w, r.WithContext(ctx))
				return
			}
		}

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
	GroupName   string `json:"group_name,omitempty"`
}

// ServiceSummary is a single item returned by GET /api/services.

type ServiceSummary struct {
	ServiceName   string         `json:"service_name"`
	Status        string         `json:"status"`
	Message       string         `json:"message"`
	Timestamp     string         `json:"timestamp"`
	LastSeen      string         `json:"last_seen"`
	Stale         bool           `json:"stale"`
	Maintenance   bool           `json:"maintenance"`
	GroupName     string         `json:"group_name"`
	Uptime7d      float64        `json:"uptime_7d"`
	Uptime30d     float64        `json:"uptime_30d"`
	UptimePercent float64        `json:"uptime_percent"`
	History       []StatusBucket `json:"history"`
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
// Real-time WebSocket hub
// ---------------------------------------------------------------------------

// wsClient wraps one connected WebSocket with a buffered outbound queue so a
// slow reader can never block the broadcaster.
type wsClient struct {
	conn *websocket.Conn
	send chan []byte
}

// wsHub tracks all connected clients and fans out broadcast messages to them.
type wsHub struct {
	mu      sync.RWMutex
	clients map[*wsClient]bool
}

func newWSHub() *wsHub {
	return &wsHub{clients: make(map[*wsClient]bool)}
}

func (h *wsHub) register(c *wsClient) {
	h.mu.Lock()
	h.clients[c] = true
	h.mu.Unlock()
}

func (h *wsHub) unregister(c *wsClient) {
	h.mu.Lock()
	if _, ok := h.clients[c]; ok {
		delete(h.clients, c)
		close(c.send)
	}
	h.mu.Unlock()
}

// broadcast fans msg out to every connected client. Clients whose send
// buffer is full are skipped rather than blocking the broadcaster — a stuck
// client should never slow down or stall status ingestion.
func (h *wsHub) broadcast(msg []byte) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for c := range h.clients {
		select {
		case c.send <- msg:
		default:
			log.Printf("ws: client send buffer full, dropping message")
		}
	}
}

var wsUpgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	// Permissive by design: Lantern is a homelab tool with no origin
	// allowlist concept elsewhere (CORS is already AllowedOrigins: []string{"*"}).
	CheckOrigin: func(r *http.Request) bool { return true },
}

const (
	wsWriteWait  = 10 * time.Second
	wsPongWait   = 60 * time.Second
	wsPingPeriod = (wsPongWait * 9) / 10
)

func (c *wsClient) writePump() {
	ticker := time.NewTicker(wsPingPeriod)
	defer func() {
		ticker.Stop()
		c.conn.Close()
	}()
	for {
		select {
		case msg, ok := <-c.send:
			c.conn.SetWriteDeadline(time.Now().Add(wsWriteWait))
			if !ok {
				c.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}
			if err := c.conn.WriteMessage(websocket.TextMessage, msg); err != nil {
				return
			}
		case <-ticker.C:
			c.conn.SetWriteDeadline(time.Now().Add(wsWriteWait))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

func (c *wsClient) readPump(hub *wsHub) {
	defer func() {
		hub.unregister(c)
		c.conn.Close()
	}()
	c.conn.SetReadDeadline(time.Now().Add(wsPongWait))
	c.conn.SetPongHandler(func(string) error {
		c.conn.SetReadDeadline(time.Now().Add(wsPongWait))
		return nil
	})
	for {
		// Clients don't send anything meaningful; we only read to detect
		// disconnects and process control frames (pong/close).
		if _, _, err := c.conn.ReadMessage(); err != nil {
			return
		}
	}
}

// handleWS handles GET /ws (and /api/public/ws) by upgrading the connection
// and registering it with the hub.
func handleWS(hub *wsHub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		conn, err := wsUpgrader.Upgrade(w, r, nil)
		if err != nil {
			log.Printf("ws upgrade error: %v", err)
			return
		}
		client := &wsClient{conn: conn, send: make(chan []byte, 16)}
		hub.register(client)
		go client.writePump()
		go client.readPump(hub)
	}
}

// wsMessage is the envelope broadcast to connected dashboards.
type wsMessage struct {
	Type    string         `json:"type"`
	Service ServiceSummary `json:"service"`
}

// buildServiceSummary assembles the same shape returned by GET /api/services
// for a single service, for use in WebSocket broadcasts.
func buildServiceSummary(db *sql.DB, cfg *Config, name string) (ServiceSummary, bool) {
	var s ServiceSummary
	var msg sql.NullString
	err := db.QueryRow(`
SELECT s.service_name, s.status, s.message, s.timestamp, COALESCE(g.group_name, '')
FROM status_events s
LEFT JOIN service_groups g ON s.service_name = g.service_name
WHERE s.service_name = ?
ORDER BY s.id DESC LIMIT 1`, name).Scan(&s.ServiceName, &s.Status, &msg, &s.Timestamp, &s.GroupName)
	if err != nil {
		return s, false
	}
	if msg.Valid {
		s.Message = msg.String
	}
	s.LastSeen = s.Timestamp

	if t, err := time.Parse(time.RFC3339, s.Timestamp); err == nil && time.Since(t).Hours() > float64(cfg.StaleHours) {
		s.Stale = true
	}

	var maint int
	db.QueryRow("SELECT enabled FROM service_maintenance WHERE service_name = ?", name).Scan(&maint)
	s.Maintenance = maint == 1

	up7, up30, buckets := getCachedOrComputeServiceMetrics(db, name)
	s.Uptime7d = up7
	s.Uptime30d = up30
	s.UptimePercent = up30
	s.History = buckets

	return s, true
}

// broadcastServiceUpdate builds the current summary for a service and pushes
// it to all connected WebSocket clients. Intended to be called via `go`
// from request handlers so it never adds latency to the caller.
func broadcastServiceUpdate(hub *wsHub, db *sql.DB, cfg *Config, name string) {
	summary, ok := buildServiceSummary(db, cfg, name)
	if !ok {
		return
	}
	body, err := json.Marshal(wsMessage{Type: "status_update", Service: summary})
	if err != nil {
		log.Printf("ws: failed to marshal broadcast message: %v", err)
		return
	}
	hub.broadcast(body)
}

// ---------------------------------------------------------------------------
// Background workers & Webhooks
// ---------------------------------------------------------------------------

// getEffectiveWebhookURL retrieves the configured webhook URL for a channel from the DB,
// falling back to environment variables if not set in DB.
func getEffectiveWebhookURL(db *sql.DB, cfg *Config, channel string) (string, string) {
	if db != nil {
		var dbURL string
		err := db.QueryRow("SELECT url FROM webhook_configs WHERE channel = ?", channel).Scan(&dbURL)
		if err == nil && strings.TrimSpace(dbURL) != "" {
			return strings.TrimSpace(dbURL), "db"
		}
	}
	switch channel {
	case "discord":
		if cfg.WebhookDiscord != "" {
			return cfg.WebhookDiscord, "env"
		}
	case "telegram":
		if cfg.WebhookTelegram != "" {
			return cfg.WebhookTelegram, "env"
		}
	case "gotify":
		if cfg.WebhookGotify != "" {
			return cfg.WebhookGotify, "env"
		}
	case "generic":
		if cfg.WebhookGeneric != "" {
			return cfg.WebhookGeneric, "env"
		}
	}
	return "", "none"
}

// webhookJob is one outbound delivery attempt queued for a worker.
type webhookJob struct {
	channel   string
	url       string
	payload   []byte
	service   string
	oldStatus string
	newStatus string
}

// webhookDispatcher runs a bounded pool of goroutines that perform outbound
// webhook HTTP calls, so a slow or unreachable endpoint on one channel can
// never block status ingestion or the other channels. Every attempt is
// recorded to webhook_deliveries so failures are visible instead of silently
// swallowed.
type webhookDispatcher struct {
	jobs   chan webhookJob
	client *http.Client
	db     *sql.DB
}

const webhookQueueSize = 256

func newWebhookDispatcher(db *sql.DB, workers int) *webhookDispatcher {
	d := &webhookDispatcher{
		jobs:   make(chan webhookJob, webhookQueueSize),
		client: &http.Client{Timeout: 10 * time.Second},
		db:     db,
	}
	for i := 0; i < workers; i++ {
		go d.worker()
	}
	return d
}

func (d *webhookDispatcher) worker() {
	for job := range d.jobs {
		resp, err := d.client.Post(job.url, "application/json", bytes.NewReader(job.payload))
		success := false
		httpStatus := 0
		errMsg := ""
		if err != nil {
			errMsg = err.Error()
			log.Printf("webhook dispatch failed: channel=%s service=%s err=%v", job.channel, job.service, err)
		} else {
			httpStatus = resp.StatusCode
			success = resp.StatusCode < 400
			resp.Body.Close()
			if !success {
				errMsg = fmt.Sprintf("http %d", resp.StatusCode)
				log.Printf("webhook dispatch non-2xx: channel=%s service=%s status=%d", job.channel, job.service, resp.StatusCode)
			}
		}
		d.recordDelivery(job, success, httpStatus, errMsg)
	}
}

// enqueue submits a job without blocking the caller. If the queue is full
// (a sustained outage across many channels), the job is dropped and logged
// rather than backing up ingestion.
func (d *webhookDispatcher) enqueue(job webhookJob) {
	select {
	case d.jobs <- job:
	default:
		log.Printf("webhook queue full, dropping job: channel=%s service=%s", job.channel, job.service)
		d.recordDelivery(job, false, 0, "delivery queue full, job dropped")
	}
}

func (d *webhookDispatcher) recordDelivery(job webhookJob, success bool, httpStatus int, errMsg string) {
	successInt := 0
	if success {
		successInt = 1
	}
	_, err := d.db.Exec(
		`INSERT INTO webhook_deliveries (channel, service_name, old_status, new_status, success, http_status, error, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		job.channel, job.service, job.oldStatus, job.newStatus, successInt, httpStatus, errMsg, time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		log.Printf("failed to record webhook delivery: %v", err)
	}
}

// dispatchWebhooks enqueues one delivery job per configured channel. It
// returns immediately — actual HTTP calls happen on worker goroutines.
func dispatchWebhooks(dispatcher *webhookDispatcher, db *sql.DB, cfg *Config, service, oldStatus, newStatus, message string) {
	text := fmt.Sprintf("Service %s changed from %s to %s. %s", service, oldStatus, newStatus, message)

	if discordURL, _ := getEffectiveWebhookURL(db, cfg, "discord"); discordURL != "" {
		body, _ := json.Marshal(map[string]string{"content": text})
		dispatcher.enqueue(webhookJob{channel: "discord", url: discordURL, payload: body, service: service, oldStatus: oldStatus, newStatus: newStatus})
	}
	if telegramURL, _ := getEffectiveWebhookURL(db, cfg, "telegram"); telegramURL != "" {
		body, _ := json.Marshal(map[string]string{"text": text})
		dispatcher.enqueue(webhookJob{channel: "telegram", url: telegramURL, payload: body, service: service, oldStatus: oldStatus, newStatus: newStatus})
	}
	if gotifyURL, _ := getEffectiveWebhookURL(db, cfg, "gotify"); gotifyURL != "" {
		body, _ := json.Marshal(map[string]string{"title": "Lantern Alert", "message": text})
		dispatcher.enqueue(webhookJob{channel: "gotify", url: gotifyURL, payload: body, service: service, oldStatus: oldStatus, newStatus: newStatus})
	}
	if genericURL, _ := getEffectiveWebhookURL(db, cfg, "generic"); genericURL != "" {
		body, _ := json.Marshal(map[string]string{"service": service, "old": oldStatus, "new": newStatus, "message": message})
		dispatcher.enqueue(webhookJob{channel: "generic", url: genericURL, payload: body, service: service, oldStatus: oldStatus, newStatus: newStatus})
	}
}

func runStaleChecker(db *sql.DB, cfg *Config, dispatcher *webhookDispatcher, hub *wsHub) {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		rows, err := db.Query(`
SELECT service_name, status, timestamp
FROM status_events
WHERE id IN (SELECT MAX(id) FROM status_events GROUP BY service_name)
`)
		if err != nil {
			continue
		}
		for rows.Next() {
			var name, status, ts string
			rows.Scan(&name, &status, &ts)
			var maint int
			db.QueryRow("SELECT enabled FROM service_maintenance WHERE service_name = ?", name).Scan(&maint)
			if maint == 1 || status == "down" || status == "stale" {
				continue
			}
			t, err := time.Parse(time.RFC3339, ts)
			if err == nil && time.Since(t).Hours() > float64(cfg.StaleHours) {
				db.Exec(`INSERT INTO status_events (service_name, status, message, timestamp) VALUES (?, 'down', 'Service missed heartbeat timeout', ?)`, name, time.Now().UTC().Format(time.RFC3339))
				invalidateServiceMetricsCache(name)
				dispatchWebhooks(dispatcher, db, cfg, name, status, "down", "Service missed heartbeat timeout")
				go broadcastServiceUpdate(hub, db, cfg, name)
			}
		}
		rows.Close()
	}
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
func handlePostStatus(db *sql.DB, cfg *Config, dispatcher *webhookDispatcher, hub *wsHub) http.HandlerFunc {
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

		scopedSvc := r.Context().Value("scoped_service")
		if scopedSvc != nil && scopedSvc.(string) != req.ServiceName {
			writeError(w, http.StatusForbidden, "token not scoped for this service")
			return
		}

		var lastStatus string
		db.QueryRow("SELECT status FROM status_events WHERE service_name = ? ORDER BY id DESC LIMIT 1", req.ServiceName).Scan(&lastStatus)

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

		if req.GroupName != "" {
			_, _ = db.Exec(`INSERT INTO service_groups (service_name, group_name, updated_at) VALUES (?, ?, CURRENT_TIMESTAMP)
				ON CONFLICT(service_name) DO UPDATE SET group_name = excluded.group_name, updated_at = CURRENT_TIMESTAMP`,
				req.ServiceName, strings.TrimSpace(req.GroupName))
		}

		var maint int
		db.QueryRow("SELECT enabled FROM service_maintenance WHERE service_name = ?", req.ServiceName).Scan(&maint)
		if maint == 0 && lastStatus != "" && lastStatus != req.Status {
			dispatchWebhooks(dispatcher, db, cfg, req.ServiceName, lastStatus, req.Status, req.Message)
		}

		invalidateServiceMetricsCache(req.ServiceName)
		go broadcastServiceUpdate(hub, db, cfg, req.ServiceName)

		writeJSON(w, http.StatusCreated, map[string]int64{"id": id})
	}
}

// handleGetServices handles GET /api/services.
// Returns the most recent status event for every known service.
func handleGetServices(db *sql.DB, cfg *Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rows, err := db.Query(`
SELECT s.service_name, s.status, s.message, s.timestamp, COALESCE(g.group_name, '')
FROM status_events s
LEFT JOIN service_groups g ON s.service_name = g.service_name
WHERE s.id IN (
    SELECT MAX(id) FROM status_events GROUP BY service_name
)
ORDER BY s.service_name ASC;`)
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
			if err := rows.Scan(&s.ServiceName, &s.Status, &msg, &s.Timestamp, &s.GroupName); err != nil {
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

			services = append(services, s)
		}

		if err := rows.Err(); err != nil {
			log.Printf("handleGetServices rows error: %v", err)
		}

		// Compute metrics concurrently across services using cached/unified pass
		var wg sync.WaitGroup
		for i := range services {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				up7, up30, buckets := getCachedOrComputeServiceMetrics(db, services[idx].ServiceName)
				services[idx].Uptime7d = up7
				services[idx].Uptime30d = up30
				services[idx].UptimePercent = up30
				services[idx].History = buckets
			}(i)
		}
		wg.Wait()

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

		scopedSvc := r.Context().Value("scoped_service")
		if scopedSvc != nil && scopedSvc.(string) != req.ServiceName {
			writeError(w, http.StatusForbidden, "token not scoped for this service")
			return
		}

		var lastStatus string
		db.QueryRow("SELECT status FROM status_events WHERE service_name = ? ORDER BY id DESC LIMIT 1", req.ServiceName).Scan(&lastStatus)

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
	if strings.HasPrefix(r.URL.Path, "/api/") {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "API endpoint not found"})
		return
	}

	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "0")

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

type WebhookChannelInfo struct {
	Configured bool   `json:"configured"`
	URL        string `json:"url"`
	Source     string `json:"source"` // "db", "env", "none"
}

// handleGetWebhooks handles GET /api/webhooks.
func handleGetWebhooks(db *sql.DB, cfg *Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		channels := []string{"discord", "telegram", "gotify", "generic"}
		resp := make(map[string]WebhookChannelInfo)
		for _, ch := range channels {
			u, src := getEffectiveWebhookURL(db, cfg, ch)
			resp[ch] = WebhookChannelInfo{
				Configured: u != "",
				URL:        u,
				Source:     src,
			}
		}
		writeJSON(w, http.StatusOK, resp)
	}
}

// handlePutWebhooks handles PUT and POST /api/webhooks to save webhook URLs in DB.
func handlePutWebhooks(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req map[string]string
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid json payload")
			return
		}

		validChannels := map[string]bool{"discord": true, "telegram": true, "gotify": true, "generic": true}

		saveChannel := func(ch, rawURL string) error {
			ch = strings.ToLower(strings.TrimSpace(ch))
			if !validChannels[ch] {
				return fmt.Errorf("invalid channel: %s", ch)
			}
			url := strings.TrimSpace(rawURL)
			if url == "" {
				_, err := db.Exec("DELETE FROM webhook_configs WHERE channel = ?", ch)
				return err
			}
			_, err := db.Exec(`INSERT INTO webhook_configs (channel, url) VALUES (?, ?)
				ON CONFLICT(channel) DO UPDATE SET url = excluded.url`, ch, url)
			return err
		}

		// Single channel payload: { "channel": "discord", "url": "..." }
		if ch, ok := req["channel"]; ok {
			if err := saveChannel(ch, req["url"]); err != nil {
				writeError(w, http.StatusBadRequest, err.Error())
				return
			}
		} else {
			// Multi-channel map: { "discord": "...", "telegram": "..." }
			for ch, rawURL := range req {
				if err := saveChannel(ch, rawURL); err != nil {
					writeError(w, http.StatusBadRequest, err.Error())
					return
				}
			}
		}

		writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "message": "webhook configurations updated"})
	}
}

// handleTestWebhook handles POST /api/webhooks/test.
func handleTestWebhook(db *sql.DB, cfg *Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Channel string `json:"channel"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			req.Channel = "all"
		}
		if req.Channel == "" {
			req.Channel = "all"
		}
		req.Channel = strings.ToLower(strings.TrimSpace(req.Channel))

		results := make(map[string]map[string]any)
		testMsg := "🔆 Lantern Test Webhook: Notifications are working correctly!"

		doTest := func(name string, payload any) {
			if req.Channel != "all" && req.Channel != name {
				return
			}
			url, src := getEffectiveWebhookURL(db, cfg, name)
			if url == "" {
				results[name] = map[string]any{"attempted": false, "source": "none", "message": "Webhook URL not configured"}
				return
			}

			body, _ := json.Marshal(payload)
			resp, err := http.Post(url, "application/json", bytes.NewBuffer(body))
			if err != nil {
				results[name] = map[string]any{"attempted": true, "success": false, "source": src, "message": err.Error()}
				return
			}
			defer resp.Body.Close()
			results[name] = map[string]any{"attempted": true, "success": resp.StatusCode < 400, "source": src, "status_code": resp.StatusCode}
		}

		doTest("discord", map[string]string{"content": testMsg})
		doTest("telegram", map[string]string{"text": testMsg})
		doTest("gotify", map[string]string{"title": "Lantern Alert", "message": testMsg})
		doTest("generic", map[string]string{"content": testMsg})

		writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "results": results})
	}
}

// WebhookDelivery is one row returned by GET /api/webhooks/deliveries.
type WebhookDelivery struct {
	ID          int64  `json:"id"`
	Channel     string `json:"channel"`
	ServiceName string `json:"service_name"`
	OldStatus   string `json:"old_status"`
	NewStatus   string `json:"new_status"`
	Success     bool   `json:"success"`
	HTTPStatus  int    `json:"http_status"`
	Error       string `json:"error"`
	CreatedAt   string `json:"created_at"`
}

// handleGetWebhookDeliveries handles GET /api/webhooks/deliveries.
// Surfaces recent delivery attempts (success and failure) so a slow or
// unreachable endpoint is visible instead of silently swallowed.
func handleGetWebhookDeliveries(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		limit := 20
		if v := r.URL.Query().Get("limit"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 200 {
				limit = n
			}
		}

		rows, err := db.Query(`
SELECT id, channel, service_name, COALESCE(old_status,''), COALESCE(new_status,''), success, COALESCE(http_status,0), COALESCE(error,''), created_at
FROM webhook_deliveries
ORDER BY id DESC
LIMIT ?`, limit)
		if err != nil {
			log.Printf("handleGetWebhookDeliveries db error: %v", err)
			writeError(w, http.StatusInternalServerError, "database error")
			return
		}
		defer rows.Close()

		deliveries := []WebhookDelivery{}
		for rows.Next() {
			var d WebhookDelivery
			var success int
			if err := rows.Scan(&d.ID, &d.Channel, &d.ServiceName, &d.OldStatus, &d.NewStatus, &success, &d.HTTPStatus, &d.Error, &d.CreatedAt); err != nil {
				continue
			}
			d.Success = success == 1
			deliveries = append(deliveries, d)
		}

		writeJSON(w, http.StatusOK, deliveries)
	}
}

// GroupSummary represents a service group and the number of services in it.
type GroupSummary struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

// handleGetGroups handles GET /api/groups.
func handleGetGroups(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rows, err := db.Query(`
			SELECT COALESCE(g.group_name, ''), COUNT(DISTINCT s.service_name)
			FROM status_events s
			LEFT JOIN service_groups g ON s.service_name = g.service_name
			WHERE s.id IN (SELECT MAX(id) FROM status_events GROUP BY service_name)
			GROUP BY COALESCE(g.group_name, '')
			ORDER BY COALESCE(g.group_name, '') ASC;
		`)
		if err != nil {
			log.Printf("handleGetGroups db error: %v", err)
			writeError(w, http.StatusInternalServerError, "database error")
			return
		}
		defer rows.Close()

		groups := []GroupSummary{}
		for rows.Next() {
			var g GroupSummary
			if err := rows.Scan(&g.Name, &g.Count); err != nil {
				continue
			}
			if g.Name != "" {
				groups = append(groups, g)
			}
		}
		writeJSON(w, http.StatusOK, groups)
	}
}

// handlePutServiceGroup handles PUT /api/services/{name}/group.
func handlePutServiceGroup(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		vars := mux.Vars(r)
		name := strings.TrimSpace(vars["name"])
		if name == "" {
			writeError(w, http.StatusBadRequest, "service name is required")
			return
		}

		scopedSvc := r.Context().Value("scoped_service")
		if scopedSvc != nil && scopedSvc.(string) != name {
			writeError(w, http.StatusForbidden, "token not scoped for this service")
			return
		}

		var req struct {
			Group     string `json:"group"`
			GroupName string `json:"group_name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid json body")
			return
		}
		group := strings.TrimSpace(req.Group)
		if group == "" {
			group = strings.TrimSpace(req.GroupName)
		}

		_, err := db.Exec(`INSERT INTO service_groups (service_name, group_name, updated_at) VALUES (?, ?, CURRENT_TIMESTAMP)
			ON CONFLICT(service_name) DO UPDATE SET group_name = excluded.group_name, updated_at = CURRENT_TIMESTAMP`,
			name, group)
		if err != nil {
			log.Printf("handlePutServiceGroup db error: %v", err)
			writeError(w, http.StatusInternalServerError, "database error")
			return
		}

		writeJSON(w, http.StatusOK, map[string]string{
			"status":       "ok",
			"service_name": name,
			"group_name":   group,
		})
	}
}

func setupRoutes(db *sql.DB, cfg *Config, dispatcher *webhookDispatcher, hub *wsHub) http.Handler {
	r := mux.NewRouter()

	// --- WebSocket routes (registered outside jsonMiddleware; the upgrade
	// handshake writes its own headers via Hijack) ---
	r.Handle("/ws", handleWS(hub)).Methods(http.MethodGet)
	r.Handle("/api/public/ws", handleWS(hub)).Methods(http.MethodGet)

	// --- API routes ---
	api := r.PathPrefix("/api").Subrouter()
	api.Use(jsonMiddleware)

	api.Handle("/health", handleHealth()).Methods(http.MethodGet)
	api.Handle("/webhooks", handleGetWebhooks(db, cfg)).Methods(http.MethodGet)
	api.Handle("/webhooks", handlePutWebhooks(db)).Methods(http.MethodPut, http.MethodPost)
	api.Handle("/webhooks/test", handleTestWebhook(db, cfg)).Methods(http.MethodPost)
	api.Handle("/webhooks/deliveries", handleGetWebhookDeliveries(db)).Methods(http.MethodGet)
	api.Handle("/groups", handleGetGroups(db)).Methods(http.MethodGet)

	api.Handle("/status", handlePostStatus(db, cfg, dispatcher, hub)).Methods(http.MethodPost)
	api.Handle("/services", handleGetServices(db, cfg)).Methods(http.MethodGet)
	api.Handle("/services/{name}/history", handleGetServiceHistory(db)).Methods(http.MethodGet)
	api.Handle("/services/{name}/group", handlePutServiceGroup(db)).Methods(http.MethodPut, http.MethodPost)
	api.Handle("/services/{name}/metadata", handleGetServiceMetadata(db)).Methods(http.MethodGet)
	api.Handle("/services/{name}/docker/status", handleGetDockerStatus()).Methods(http.MethodGet)
	api.Handle("/services/{name}/docker/restart", handlePostDockerRestart(db)).Methods(http.MethodPost)
	api.Handle("/services/{name}/docker/logs", handleGetDockerLogs()).Methods(http.MethodGet)
	api.Handle("/diagnostics", handlePostDiagnostics(db)).Methods(http.MethodPost)
	api.Handle("/diagnostics", handleGetDiagnostics(db)).Methods(http.MethodGet)
	api.Handle("/diagnostics/{id}", handleGetDiagnosticByID(db)).Methods(http.MethodGet)

	api.Handle("/services/{name}/uptime", handleGetUptime(db)).Methods(http.MethodGet)
	api.Handle("/services/{name}/strip", handleGetStrip(db)).Methods(http.MethodGet)
	api.Handle("/services/{name}/incidents", handleGetIncidents(db)).Methods(http.MethodGet)
	api.Handle("/services/{name}/maintenance", handlePutMaintenance(db)).Methods(http.MethodPut)
	api.Handle("/services/{name}/maintenance", handleGetMaintenance(db)).Methods(http.MethodGet)
	api.Handle("/docs", handleDocs()).Methods(http.MethodGet)

	publicApi := r.PathPrefix("/api/public").Subrouter()
	publicApi.Use(jsonMiddleware)
	publicApi.Handle("/services", handleGetServices(db, cfg)).Methods(http.MethodGet)
	publicApi.Handle("/groups", handleGetGroups(db)).Methods(http.MethodGet)
	publicApi.Handle("/services/{name}/metadata", handleGetServiceMetadata(db)).Methods(http.MethodGet)
	publicApi.Handle("/services/{name}/uptime", handleGetUptime(db)).Methods(http.MethodGet)

	// --- Static / SPA ---
	r.PathPrefix("/").Handler(spaHandler{staticDir: http.Dir("./static/")})

	// --- CORS (permissive for homelab use) ---
	c := cors.New(cors.Options{
		AllowedOrigins:   []string{"*"},
		AllowedMethods:   []string{"GET", "POST", "PUT", "DELETE", "OPTIONS"},
		AllowedHeaders:   []string{"*"},
		AllowCredentials: false,
	})

	// --- Auth and Gzip compression wrap everything ---
	return gzipMiddleware(authMiddleware(db, cfg, c.Handler(r)))
}

var gzipWriterPool = sync.Pool{
	New: func() interface{} {
		return gzip.NewWriter(io.Discard)
	},
}

type gzipResponseWriter struct {
	io.Writer
	http.ResponseWriter
	wroteHeader bool
}

func (w *gzipResponseWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.Header().Del("Content-Length")
		w.Header().Set("Content-Encoding", "gzip")
		w.ResponseWriter.WriteHeader(http.StatusOK)
		w.wroteHeader = true
	}
	return w.Writer.Write(b)
}

func (w *gzipResponseWriter) WriteHeader(code int) {
	if !w.wroteHeader {
		w.Header().Del("Content-Length")
		w.Header().Set("Content-Encoding", "gzip")
		w.ResponseWriter.WriteHeader(code)
		w.wroteHeader = true
	}
}

func gzipMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// WebSocket upgrades hijack the underlying connection directly;
		// our gzipResponseWriter doesn't implement http.Hijacker, so it
		// must never wrap a /ws request.
		if r.URL.Path == "/ws" || r.URL.Path == "/api/public/ws" {
			next.ServeHTTP(w, r)
			return
		}
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			next.ServeHTTP(w, r)
			return
		}
		gz := gzipWriterPool.Get().(*gzip.Writer)
		defer gzipWriterPool.Put(gz)
		gz.Reset(w)
		defer gz.Close()

		gzw := &gzipResponseWriter{Writer: gz, ResponseWriter: w}
		next.ServeHTTP(gzw, r)
	})
}

// ---------------------------------------------------------------------------
// Entry point
// ---------------------------------------------------------------------------

const webhookWorkerCount = 4

func main() {
	cfg := loadConfig()
	db := initDB(cfg)
	defer db.Close()

	hub := newWSHub()
	dispatcher := newWebhookDispatcher(db, webhookWorkerCount)

	// Run retention cleanup in the background every hour.
	go runRetentionCleanup(db, cfg)

	// Background worker for missing heartbeats
	go runStaleChecker(db, cfg, dispatcher, hub)

	router := setupRoutes(db, cfg, dispatcher, hub)

	log.Printf("Lantern v%s listening on :%s (auth=%v, db=%s, retention=%dd)",
		version, cfg.Port, cfg.AuthEnabled, cfg.DBPath, cfg.RetentionDays)
	log.Fatal(http.ListenAndServe(":"+cfg.Port, router))
}
