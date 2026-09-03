package main

import (
	"database/sql"
	"fmt"
	"log"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/mux"
)

// ---------------------------------------------------------------------------
// Extension types
// ---------------------------------------------------------------------------

type ServiceMaintenance struct {
	ServiceName string `json:"service_name"`
	Enabled     bool   `json:"enabled"`
	Note        string `json:"note"`
	UpdatedAt   string `json:"updated_at"`
}

type MaintenanceWindow struct {
	ID          int64   `json:"id"`
	ServiceName string  `json:"service_name"`
	StartedAt   string  `json:"started_at"`
	EndedAt     *string `json:"ended_at"`
	Note        string  `json:"note"`
}

type UptimeDatapoint struct {
	Timestamp string  `json:"timestamp"`
	UptimePct float64 `json:"uptime_pct"`
}

type UptimeResponse struct {
	ServiceName          string            `json:"service_name"`
	Range                string            `json:"range"`
	UptimePct            float64           `json:"uptime_pct"`
	TotalDowntimeMinutes float64           `json:"total_downtime_minutes"`
	TotalIncidents       int               `json:"total_incidents"`
	Datapoints           []UptimeDatapoint `json:"datapoints"`
}

type StatusBucket struct {
	Start  string `json:"start"`
	Status string `json:"status"`
}

type StripResponse struct {
	ServiceName string         `json:"service_name"`
	Hours       int            `json:"hours"`
	Buckets     []StatusBucket `json:"buckets"`
}

type Incident struct {
	StartedAt       string  `json:"started_at"`
	EndedAt         string  `json:"ended_at"`
	DurationMinutes float64 `json:"duration_minutes"`
	TriggerStatus   string  `json:"trigger_status"`
	TriggerMessage  string  `json:"trigger_message"`
	InMaintenance   bool    `json:"in_maintenance"`
}

type IncidentsResponse struct {
	ServiceName          string     `json:"service_name"`
	Range                string     `json:"range"`
	TotalDowntimeMinutes float64    `json:"total_downtime_minutes"`
	Incidents            []Incident `json:"incidents"`
}

// ---------------------------------------------------------------------------
// Internal helpers
// ---------------------------------------------------------------------------

type rawEvent struct {
	Status    string
	Message   string
	Timestamp time.Time
}

// fetchEvents returns all status_events for a service from 'since' onward,
// ordered by timestamp ascending.
func fetchEvents(db *sql.DB, name string, since time.Time) ([]rawEvent, error) {
	rows, err := db.Query(
		`SELECT status, COALESCE(message,''), timestamp FROM status_events
		 WHERE service_name = ? AND timestamp >= ?
		 ORDER BY timestamp ASC`,
		name, since.Format(time.RFC3339))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var events []rawEvent
	for rows.Next() {
		var e rawEvent
		var ts string
		if err := rows.Scan(&e.Status, &e.Message, &ts); err != nil {
			continue
		}
		e.Timestamp, _ = time.Parse(time.RFC3339, ts)
		events = append(events, e)
	}
	return events, rows.Err()
}

// fetchRecentBeats returns the last `limit` status checks for a service,
// oldest first, for use in the live heartbeat bar. Uses the existing
// (service_name, id DESC) index — a cheap point lookup, not a table scan.
// If the service has fewer than `limit` recorded checks, the result is
// left-padded with HeartbeatBeat{Status: "empty"} placeholders so callers
// always get exactly `limit` entries.
func fetchRecentBeats(db *sql.DB, name string, limit int) []HeartbeatBeat {
	rows, err := db.Query(
		`SELECT status, COALESCE(message,''), timestamp, COALESCE(latency_ms, 0) FROM status_events
		 WHERE service_name = ?
		 ORDER BY timestamp DESC, id DESC LIMIT ?`,
		name, limit)
	if err != nil {
		return leftPadEmptyBeats(nil, limit)
	}
	defer rows.Close()

	var beats []HeartbeatBeat
	for rows.Next() {
		var b HeartbeatBeat
		if err := rows.Scan(&b.Status, &b.Msg, &b.Timestamp, &b.LatencyMs); err != nil {
			continue
		}
		beats = append(beats, b)
	}

	// Reverse into oldest-first order (query returned newest-first).
	for i, j := 0, len(beats)-1; i < j; i, j = i+1, j-1 {
		beats[i], beats[j] = beats[j], beats[i]
	}

	return leftPadEmptyBeats(beats, limit)
}

// leftPadEmptyBeats prepends "empty" placeholder beats so the returned slice
// is always exactly `limit` long.
func leftPadEmptyBeats(beats []HeartbeatBeat, limit int) []HeartbeatBeat {
	if len(beats) >= limit {
		return beats
	}
	padded := make([]HeartbeatBeat, 0, limit)
	for i := 0; i < limit-len(beats); i++ {
		padded = append(padded, HeartbeatBeat{Status: "empty"})
	}
	return append(padded, beats...)
}

// windowUptimePct returns the uptime percentage across a heartbeat window,
// counting only real checks: "empty" left-padding placeholders are excluded
// from both numerator and denominator, so a service with 3 recorded checks
// is scored out of 3 rather than out of 30. Returns 0 when the window holds
// no real checks at all.
func windowUptimePct(beats []HeartbeatBeat) float64 {
	var total, up int
	for _, b := range beats {
		if b.Status == "empty" {
			continue
		}
		total++
		if b.Status == "up" {
			up++
		}
	}
	if total == 0 {
		return 0
	}
	return (float64(up) / float64(total)) * 100
}

// fetchLastEventBefore returns the most recent event before 'before'.
// Returns nil if no event exists.
func fetchLastEventBefore(db *sql.DB, name string, before time.Time) *rawEvent {
	row := db.QueryRow(
		`SELECT status, COALESCE(message,''), timestamp FROM status_events
		 WHERE service_name = ? AND timestamp < ?
		 ORDER BY timestamp DESC LIMIT 1`,
		name, before.Format(time.RFC3339))
	var e rawEvent
	var ts string
	if err := row.Scan(&e.Status, &e.Message, &ts); err != nil {
		return nil
	}
	e.Timestamp, _ = time.Parse(time.RFC3339, ts)
	return &e
}

// isInMaintenance checks whether any maintenance window covers time t.
func isInMaintenance(db *sql.DB, name string, t time.Time) bool {
	var count int
	tStr := t.Format(time.RFC3339)
	err := db.QueryRow(
		`SELECT COUNT(*) FROM maintenance_windows
		 WHERE service_name = ? AND started_at <= ? AND (ended_at IS NULL OR ended_at >= ?)`,
		name, tStr, tStr).Scan(&count)
	if err != nil {
		return false
	}
	return count > 0
}

// maintenanceWindow is one span during which a service was intentionally
// offline. A zero `end` means the window is still open.
type maintenanceWindow struct {
	start time.Time
	end   time.Time
}

func (w maintenanceWindow) covers(t time.Time) bool {
	if t.Before(w.start) {
		return false
	}
	return w.end.IsZero() || !t.After(w.end)
}

// inAnyWindow reports whether t falls inside any of the given windows.
func inAnyWindow(ws []maintenanceWindow, t time.Time) bool {
	for _, w := range ws {
		if w.covers(t) {
			return true
		}
	}
	return false
}

// loadMaintenanceWindows reads every maintenance window for a service in one
// query, for callers that need to test many timestamps against them.
//
// isInMaintenance below issues a COUNT per call, which is fine for a handful of
// checks and badly wrong inside a loop over a service's timeline: handleGetUptime
// walks one segment per recorded event, so at range=30d with a 60s monitor that
// was on the order of tens of thousands of round trips — inside a single
// request, on a route that is reachable without credentials.
func loadMaintenanceWindows(db *sql.DB, name string) []maintenanceWindow {
	rows, err := db.Query(
		`SELECT started_at, COALESCE(ended_at, '') FROM maintenance_windows WHERE service_name = ?`, name)
	if err != nil {
		return nil
	}
	defer rows.Close()

	var out []maintenanceWindow
	for rows.Next() {
		var startStr, endStr string
		if err := rows.Scan(&startStr, &endStr); err != nil {
			continue
		}
		start, err := time.Parse(time.RFC3339, startStr)
		if err != nil {
			continue
		}
		w := maintenanceWindow{start: start}
		if endStr != "" {
			if end, err := time.Parse(time.RFC3339, endStr); err == nil {
				w.end = end
			}
		}
		out = append(out, w)
	}
	return out
}

// parseRange converts a range string to hours.
func parseRange(rng string) int {
	switch rng {
	case "1h":
		return 1
	case "24h":
		return 24
	case "7d":
		return 24 * 7
	case "30d":
		return 24 * 30
	default:
		return 24 * 7
	}
}

// segment is a (time, status) pair used for timeline computation.
type segment struct {
	start   time.Time
	status  string
	message string
}

// buildTimeline constructs a timeline covering [since, now] from prior state + events.
// When there's no event before `since` — the service is newer than the
// requested window, or has no history at all — the leading segment is
// "empty" rather than "unknown". "empty" means "this service didn't exist
// yet"; callers exclude it from uptime/incident totals entirely, so a
// brand-new service doesn't read as having been up for time before it
// existed. "unknown" is reserved for genuinely indeterminate state within a
// service's real history.
func buildTimeline(prior *rawEvent, events []rawEvent, since time.Time) []segment {
	startStatus := "empty"
	startMsg := ""
	if prior != nil {
		startStatus = prior.Status
		startMsg = prior.Message
	}
	tl := []segment{{start: since, status: startStatus, message: startMsg}}
	for _, e := range events {
		tl = append(tl, segment{start: e.Timestamp, status: e.Status, message: e.Message})
	}
	return tl
}

// statusAtTime returns the status from the timeline at a given time.
func statusAtTime(timeline []segment, t time.Time) string {
	status := "unknown"
	for _, s := range timeline {
		if !s.start.After(t) {
			status = s.status
		} else {
			break
		}
	}
	return status
}

func isDown(status string) bool {
	return status == "down" || status == "degraded"
}

// ---------------------------------------------------------------------------
// GET /api/services/{name}/uptime — REAL uptime from DB
// ---------------------------------------------------------------------------

func handleGetUptime(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		vars := mux.Vars(r)
		name := vars["name"]
		rng := r.URL.Query().Get("range")
		if rng == "" {
			rng = "7d"
		}

		hours := parseRange(rng)
		now := time.Now().UTC()
		since := now.Add(time.Duration(-hours) * time.Hour)

		events, err := fetchEvents(db, name, since)
		if err != nil {
			log.Printf("handleGetUptime db error: %v", err)
			writeError(w, http.StatusInternalServerError, "database error")
			return
		}
		prior := fetchLastEventBefore(db, name, since)
		timeline := buildTimeline(prior, events, since)
		windows := loadMaintenanceWindows(db, name)

		totalSec := now.Sub(since).Seconds()
		if totalSec <= 0 {
			totalSec = 1
		}

		var downSec, emptySec float64
		var incidentCount int
		inIncident := false

		for i, seg := range timeline {
			var end time.Time
			if i+1 < len(timeline) {
				end = timeline[i+1].start
			} else {
				end = now
			}
			dur := end.Sub(seg.start).Seconds()
			if dur < 0 {
				dur = 0
			}
			if seg.status == "empty" {
				// Service didn't exist yet for this stretch of the window;
				// exclude it from the uptime denominator entirely rather
				// than silently counting it as up.
				emptySec += dur
				inIncident = false
				continue
			}
			if isDown(seg.status) {
				if !inAnyWindow(windows, seg.start) {
					downSec += dur
				}
				if !inIncident {
					incidentCount++
					inIncident = true
				}
			} else {
				inIncident = false
			}
		}

		effectiveTotalSec := totalSec - emptySec
		if effectiveTotalSec <= 0 {
			effectiveTotalSec = 1
		}

		uptimePct := math.Round(((effectiveTotalSec-downSec)/effectiveTotalSec*100)*100) / 100
		if uptimePct > 100 {
			uptimePct = 100
		}
		if uptimePct < 0 {
			uptimePct = 0
		}

		// Generate datapoints
		var dpInterval time.Duration
		switch rng {
		case "1h":
			dpInterval = 2 * time.Minute
		case "24h":
			dpInterval = 30 * time.Minute
		case "7d":
			dpInterval = 3 * time.Hour
		case "30d":
			dpInterval = 12 * time.Hour
		default:
			dpInterval = 3 * time.Hour
		}

		datapoints := []UptimeDatapoint{}
		for t := since; t.Before(now); t = t.Add(dpInterval) {
			bucketEnd := t.Add(dpInterval)
			if bucketEnd.After(now) {
				bucketEnd = now
			}
			bucketTotal := bucketEnd.Sub(t).Seconds()
			if bucketTotal <= 0 {
				continue
			}

			bucketDown := 0.0
			bs := statusAtTime(timeline, t)
			cursor := t

			for _, seg := range timeline {
				if !seg.start.After(t) || seg.start.After(bucketEnd) {
					continue
				}
				if seg.start.After(cursor) {
					if isDown(bs) {
						bucketDown += seg.start.Sub(cursor).Seconds()
					}
					cursor = seg.start
				}
				bs = seg.status
			}
			if bucketEnd.After(cursor) {
				if isDown(bs) {
					bucketDown += bucketEnd.Sub(cursor).Seconds()
				}
			}

			dpUptime := math.Round(((bucketTotal-bucketDown)/bucketTotal*100)*100) / 100
			if dpUptime > 100 {
				dpUptime = 100
			}
			datapoints = append(datapoints, UptimeDatapoint{
				Timestamp: t.Format(time.RFC3339),
				UptimePct: dpUptime,
			})
		}

		writeJSON(w, http.StatusOK, UptimeResponse{
			ServiceName:          name,
			Range:                rng,
			UptimePct:            uptimePct,
			TotalDowntimeMinutes: math.Round(downSec/60*100) / 100,
			TotalIncidents:       incidentCount,
			Datapoints:           datapoints,
		})
	}
}

// ---------------------------------------------------------------------------
// GET /api/services/{name}/strip — REAL bucketed status history
// ---------------------------------------------------------------------------

func handleGetStrip(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		vars := mux.Vars(r)
		name := vars["name"]
		hoursStr := r.URL.Query().Get("hours")
		hours := 24
		if h, err := strconv.Atoi(hoursStr); err == nil && h > 0 && h <= 720 {
			hours = h
		}

		now := time.Now().UTC()
		since := now.Add(time.Duration(-hours) * time.Hour)

		events, err := fetchEvents(db, name, since)
		if err != nil {
			log.Printf("handleGetStrip db error: %v", err)
			writeError(w, http.StatusInternalServerError, "database error")
			return
		}
		prior := fetchLastEventBefore(db, name, since)
		timeline := buildTimeline(prior, events, since)

		// 48 buckets for 24h, scale for other ranges, cap at 96
		numBuckets := hours * 2
		if numBuckets > 96 {
			numBuckets = 96
		}
		if numBuckets < 1 {
			numBuckets = 1
		}
		bucketDur := time.Duration(hours) * time.Hour / time.Duration(numBuckets)

		buckets := make([]StatusBucket, 0, numBuckets)
		for i := 0; i < numBuckets; i++ {
			bStart := since.Add(time.Duration(i) * bucketDur)
			bEnd := bStart.Add(bucketDur)
			if bEnd.After(now) {
				bEnd = now
			}
			bDur := bEnd.Sub(bStart).Seconds()
			if bDur <= 0 {
				buckets = append(buckets, StatusBucket{
					Start:  bStart.Format(time.RFC3339),
					Status: "unknown",
				})
				continue
			}

			// Count time in each status
			statusTime := map[string]float64{}
			bs := statusAtTime(timeline, bStart)
			cursor := bStart

			for _, s := range timeline {
				if !s.start.After(bStart) || s.start.After(bEnd) {
					continue
				}
				if s.start.After(cursor) {
					statusTime[bs] += s.start.Sub(cursor).Seconds()
					cursor = s.start
				}
				bs = s.status
			}
			if bEnd.After(cursor) {
				statusTime[bs] += bEnd.Sub(cursor).Seconds()
			}

			// Pick dominant status
			dominant := "unknown"
			maxDur := 0.0
			for st, d := range statusTime {
				if d > maxDur || (d == maxDur && statusPriority(st) > statusPriority(dominant)) {
					dominant = st
					maxDur = d
				}
			}

			buckets = append(buckets, StatusBucket{
				Start:  bStart.Format(time.RFC3339),
				Status: dominant,
			})
		}

		writeJSON(w, http.StatusOK, StripResponse{
			ServiceName: name,
			Hours:       hours,
			Buckets:     buckets,
		})
	}
}

func statusPriority(s string) int {
	switch s {
	case "down":
		return 3
	case "degraded":
		return 2
	case "up":
		return 1
	default:
		return 0
	}
}

// ---------------------------------------------------------------------------
// GET /api/services/{name}/incidents — REAL incident detection
// ---------------------------------------------------------------------------

// countRecentIncidents counts distinct down/degraded incidents since a given
// time, without the duration/maintenance detail handleGetIncidents computes
// — used by the Prometheus exporter, which needs one count per service per
// scrape rather than a full incident list.
func countRecentIncidents(db *sql.DB, name string, since time.Time) int {
	events, err := fetchEvents(db, name, since)
	if err != nil {
		return 0
	}
	prior := fetchLastEventBefore(db, name, since)
	timeline := buildTimeline(prior, events, since)

	count := 0
	inIncident := false
	for _, seg := range timeline {
		if isDown(seg.status) {
			if !inIncident {
				count++
				inIncident = true
			}
		} else {
			inIncident = false
		}
	}
	return count
}

func handleGetIncidents(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		vars := mux.Vars(r)
		name := vars["name"]
		rng := r.URL.Query().Get("range")
		if rng == "" {
			rng = "30d"
		}

		hours := parseRange(rng)
		now := time.Now().UTC()
		since := now.Add(time.Duration(-hours) * time.Hour)

		events, err := fetchEvents(db, name, since)
		if err != nil {
			log.Printf("handleGetIncidents db error: %v", err)
			writeError(w, http.StatusInternalServerError, "database error")
			return
		}
		prior := fetchLastEventBefore(db, name, since)
		timeline := buildTimeline(prior, events, since)
		windows := loadMaintenanceWindows(db, name)

		var incidents []Incident
		var incStart *time.Time
		var triggerStatus, triggerMsg string
		var totalDownSec float64

		for i, seg := range timeline {
			var end time.Time
			if i+1 < len(timeline) {
				end = timeline[i+1].start
			} else {
				end = now
			}

			if isDown(seg.status) {
				dur := end.Sub(seg.start).Seconds()
				if dur > 0 {
					totalDownSec += dur
				}
				if incStart == nil {
					t := seg.start
					incStart = &t
					triggerStatus = seg.status
					triggerMsg = seg.message
				}
			} else {
				if incStart != nil {
					inMaint := inAnyWindow(windows, *incStart)
					durMin := seg.start.Sub(*incStart).Minutes()
					incidents = append(incidents, Incident{
						StartedAt:       incStart.Format(time.RFC3339),
						EndedAt:         seg.start.Format(time.RFC3339),
						DurationMinutes: math.Round(durMin*100) / 100,
						TriggerStatus:   triggerStatus,
						TriggerMessage:  triggerMsg,
						InMaintenance:   inMaint,
					})
					incStart = nil
				}
			}
		}

		// Still in an incident at end of window
		if incStart != nil {
			inMaint := inAnyWindow(windows, *incStart)
			durMin := now.Sub(*incStart).Minutes()
			incidents = append(incidents, Incident{
				StartedAt:       incStart.Format(time.RFC3339),
				EndedAt:         "",
				DurationMinutes: math.Round(durMin*100) / 100,
				TriggerStatus:   triggerStatus,
				TriggerMessage:  triggerMsg,
				InMaintenance:   inMaint,
			})
		}

		if incidents == nil {
			incidents = []Incident{}
		}

		writeJSON(w, http.StatusOK, IncidentsResponse{
			ServiceName:          name,
			Range:                rng,
			TotalDowntimeMinutes: math.Round(totalDownSec/60*100) / 100,
			Incidents:            incidents,
		})
	}
}

// ---------------------------------------------------------------------------
// PUT /api/services/{name}/maintenance
// ---------------------------------------------------------------------------

// setMaintenanceState is the single write path for toggling a service's
// maintenance flag, used by both PUT /api/services/{name}/maintenance and
// the optional `maintenance` field on POST /api/status, so both routes stay
// in sync on service_maintenance and the maintenance_windows audit trail
// instead of the two diverging.
func setMaintenanceState(db *sql.DB, name string, enabled bool, note string) string {
	now := time.Now().UTC().Format(time.RFC3339)

	val := 0
	if enabled {
		val = 1
	}

	// One transaction, because these two writes have to agree: the flag on
	// service_maintenance is what suppresses alerts, and the row in
	// maintenance_windows is what uptime accounting reads. Previously they were
	// two bare Execs with both errors discarded, so a failure between them left
	// a service flagged as under maintenance with no window to prove it — or a
	// window that never closed, quietly excluding real downtime from uptime
	// forever after.
	tx, err := db.Begin()
	if err != nil {
		log.Printf("setMaintenanceState: begin failed for %s: %v", name, err)
		return now
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec(`INSERT INTO service_maintenance (service_name, enabled, note, updated_at)
		VALUES (?, ?, ?, ?) ON CONFLICT(service_name) DO UPDATE SET enabled=?, note=?, updated_at=?`,
		name, val, note, now, val, note, now); err != nil {
		log.Printf("setMaintenanceState: flag write failed for %s: %v", name, err)
		return now
	}

	if enabled {
		// Guard against a second "enable" opening a duplicate window; without
		// it a double click leaves two open windows and only one ever closes.
		var open int
		if err := tx.QueryRow(
			`SELECT COUNT(*) FROM maintenance_windows WHERE service_name = ? AND ended_at IS NULL`,
			name).Scan(&open); err != nil {
			log.Printf("setMaintenanceState: open-window check failed for %s: %v", name, err)
			return now
		}
		if open == 0 {
			if _, err := tx.Exec(
				`INSERT INTO maintenance_windows (service_name, started_at, note) VALUES (?, ?, ?)`,
				name, now, note); err != nil {
				log.Printf("setMaintenanceState: window open failed for %s: %v", name, err)
				return now
			}
		}
	} else {
		if _, err := tx.Exec(
			`UPDATE maintenance_windows SET ended_at = ? WHERE service_name = ? AND ended_at IS NULL`,
			now, name); err != nil {
			log.Printf("setMaintenanceState: window close failed for %s: %v", name, err)
			return now
		}
	}

	if err := tx.Commit(); err != nil {
		log.Printf("setMaintenanceState: commit failed for %s: %v", name, err)
	}
	return now
}

func handlePutMaintenance(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		vars := mux.Vars(r)
		name := vars["name"]
		var req struct {
			Enabled bool   `json:"enabled"`
			Note    string `json:"note"`
		}
		if !decodeJSONBody(w, r, maxConfigBody, &req) {
			return
		}

		now := setMaintenanceState(db, name, req.Enabled, req.Note)

		writeJSON(w, http.StatusOK, ServiceMaintenance{
			ServiceName: name,
			Enabled:     req.Enabled,
			Note:        req.Note,
			UpdatedAt:   now,
		})
	}
}

// ---------------------------------------------------------------------------
// GET /api/services/{name}/maintenance
// ---------------------------------------------------------------------------

func handleGetMaintenance(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		vars := mux.Vars(r)
		name := vars["name"]
		var sm ServiceMaintenance
		sm.ServiceName = name
		var val int
		err := db.QueryRow(`SELECT enabled, note, updated_at FROM service_maintenance WHERE service_name = ?`,
			name).Scan(&val, &sm.Note, &sm.UpdatedAt)
		if err == nil {
			sm.Enabled = val == 1
		}
		writeJSON(w, http.StatusOK, sm)
	}
}

// ---------------------------------------------------------------------------
// GET /api/docs
// ---------------------------------------------------------------------------

// apiDocRoute describes one route for the /api/docs reference page.
type apiDocRoute struct {
	Method, Path, Desc, Example string
}

// apiDocSection groups routes under a heading, in the order they should
// appear on the page.
type apiDocSection struct {
	Title  string
	Routes []apiDocRoute
}

// apiDocSections is hand-maintained from the actual route table in
// setupRoutes — kept accurate by being edited alongside real route changes,
// not generated from a separate spec that can drift.
var apiDocSections = []apiDocSection{
	{"Authentication", []apiDocRoute{
		{"—", "—", "Session cookie (browser): sign in at the dashboard; the cookie also authenticates the /ws live feed, which cannot carry a header. Bearer token: Authorization: Bearer <LANTERN_AUTH_TOKEN> (admin) or a per-service token from api_tokens (scoped). Basic Auth: the stored admin credentials, seeded once from LANTERN_AUTH_USER/LANTERN_AUTH_PASS. With admin credentials set, everything outside /status, /api/public/*, /api/badge/* and /metrics requires one of these; with none set, Lantern stays open — see docs/CONFIG.md.", ""},
		{"GET", "/api/auth/session", "Whether auth is on and who you are. Always open.", "curl http://localhost:7654/api/auth/session"},
		{"POST", "/api/auth/login", "Exchange username/password for a session cookie. Rate limited per client address.", `curl -X POST /api/auth/login -d '{"username":"admin","password":"..."}'`},
		{"POST", "/api/auth/logout", "Revoke the current session.", "curl -X POST /api/auth/logout"},
		{"PUT", "/api/auth/credentials", "Change the admin username and/or password. Verifies current_password, then revokes every session and re-issues one to the caller. 401 if current_password is wrong.", `curl -X PUT /api/auth/credentials -d '{"current_password":"...","new_username":"...","new_password":"..."}'`},
	}},
	{"Health & Meta", []apiDocRoute{
		{"GET", "/api/health", "Service health and version, always open.", ""},
		{"GET", "/metrics", "Prometheus text-format metrics, always open.", "curl http://localhost:7654/metrics"},
		{"GET", "/api/docs", "This page.", ""},
	}},
	{"Status Ingestion", []apiDocRoute{
		{"POST", "/api/status", "Report a service's status. Requires auth once configured.", `curl -X POST /api/status -d '{"service_name":"db","status":"up","message":"ok","maintenance":false}'`},
	}},
	{"Services", []apiDocRoute{
		{"GET", "/api/services", "Latest status, uptime %, 30-day history, group, and monitor type for every service.", ""},
		{"GET", "/api/services/{name}/history?limit=&offset=", "Raw status_events for one service. limit capped at 500.", ""},
		{"GET", "/api/services/{name}/export?format=csv|json", "Download a service's full status history.", ""},
		{"PUT", "/api/services/{name}/group", "Assign a service to a group. Requires auth once configured.", `curl -X PUT /api/services/db/group -d '{"group":"data"}'`},
		{"GET", "/api/services/{name}/metadata", "Container/host metadata (ports, image, IP) plus Lantern telemetry.", ""},
		{"GET", "/api/services/{name}/uptime?range=1h|24h|7d|30d", "Uptime %, downtime, incident count, and graph datapoints.", ""},
		{"GET", "/api/services/{name}/strip?hours=", "Bucketed status history for the trend bar (max 96 buckets).", ""},
		{"GET", "/api/services/{name}/incidents?range=", "Detected down/degraded incidents with duration.", ""},
	}},
	{"Groups", []apiDocRoute{
		{"GET", "/api/groups", "Every group name and its service count.", ""},
	}},
	{"Maintenance", []apiDocRoute{
		{"GET", "/api/services/{name}/maintenance", "Current maintenance state for a service.", ""},
		{"PUT", "/api/services/{name}/maintenance", "Enable/disable maintenance mode. Requires auth once configured.", `curl -X PUT /api/services/db/maintenance -d '{"enabled":true,"note":"upgrade"}'`},
	}},
	{"Active Monitoring", []apiDocRoute{
		{"GET", "/api/monitors", "Every configured active monitor across all services.", ""},
		{"GET", "/api/services/{name}/monitor", "Active monitor config for one service (404 if none).", ""},
		{"PUT", "/api/services/{name}/monitor", "Create/update an active monitor. Requires auth once configured.", `curl -X PUT /api/services/db/monitor -d '{"monitor_type":"tcp","target":"db:5432","interval_seconds":60}'`},
		{"DELETE", "/api/services/{name}/monitor", "Remove an active monitor (service reverts to push-only). Requires auth once configured.", ""},
	}},
	{"Diagnostics", []apiDocRoute{
		{"POST", "/api/diagnostics", "Attach a diagnostic run (log dump, debug output) to a service. Requires auth once configured.", ""},
		{"GET", "/api/diagnostics?service_name=&limit=&offset=", "List diagnostic runs, optionally filtered. limit capped at 500.", ""},
		{"GET", "/api/diagnostics/{id}", "Full content of one diagnostic run.", ""},
	}},
	{"Activity", []apiDocRoute{
		{"GET", "/api/activity?limit=", "Merged, timestamp-sorted feed of status changes and webhook deliveries across all services.", ""},
	}},
	{"Webhooks", []apiDocRoute{
		{"GET", "/api/webhooks", "Configured webhook URLs per channel and their source (db/env/none). Requires auth once configured — a Discord or Telegram webhook URL is itself a credential.", ""},
		{"PUT", "/api/webhooks", "Save webhook URL(s). Requires auth once configured.", `curl -X PUT /api/webhooks -d '{"discord":"https://discord.com/api/webhooks/..."}'`},
		{"POST", "/api/webhooks/test", "Send a test message to one or all configured channels. Requires auth once configured.", `curl -X POST /api/webhooks/test -d '{"channel":"discord"}'`},
		{"GET", "/api/webhooks/deliveries?limit=", "Recent delivery attempts (success/failure) across all channels.", ""},
	}},
	{"Docker Management", []apiDocRoute{
		{"GET", "/api/services/{name}/docker/status", "Whether a matching container was found and its current state. Requires auth once configured.", ""},
		{"POST", "/api/services/{name}/docker/restart", "Restart the matching container. Requires auth once configured.", ""},
		{"GET", "/api/services/{name}/docker/logs?tail=", "Recent container logs (tail capped at 1000 lines). Requires auth once configured.", ""},
	}},
	{"Backup", []apiDocRoute{
		{"GET", "/api/backup", "Download a consistent database snapshot (VACUUM INTO). Requires auth once configured — the snapshot contains the credential hash, session hashes and API tokens. See docs/BACKUP.md for restore steps.", ""},
	}},
	{"Real-time", []apiDocRoute{
		{"WS", "/ws", "WebSocket: broadcasts {type:\"status_update\", service:{...}} and {type:\"heartbeat\", ...} on every status change. Same auth as the rest of the app; the anonymous mirror at /api/public/ws carries strictly less.", ""},
	}},
	{"Public (always unauthenticated)", []apiDocRoute{
		{"GET", "/api/public/services", "Same shape as /api/services — powers the public /status page.", ""},
		{"GET", "/api/public/groups", "Same shape as /api/groups.", ""},
		{"GET", "/api/public/services/{name}/uptime", "Same shape as the private uptime endpoint.", ""},
		{"WS", "/api/public/ws", "WebSocket for the public status page — no auth, ever. Carries a reduced status_update envelope only: no heartbeat deltas and no check history. A separate hub from /ws, so the gated feed is never mirrored here.", ""},
	}},
}

func handleDocs() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var b strings.Builder
		b.WriteString(`<!doctype html><html lang="en"><head><meta charset="utf-8">
<title>Lantern API Reference</title>
<style>
  :root { color-scheme: dark; }
  body { background:#0a0d14; color:#f8fafc; font-family:-apple-system,BlinkMacSystemFont,'Inter',sans-serif; margin:0; padding:32px 24px 80px; line-height:1.5; }
  .wrap { max-width:900px; margin:0 auto; }
  h1 { font-size:24px; margin-bottom:4px; }
  .sub { color:#94a3b8; font-size:13px; margin-bottom:36px; }
  h2 { font-size:15px; text-transform:uppercase; letter-spacing:0.5px; color:#94a3b8; border-bottom:1px solid rgba(255,255,255,0.08); padding-bottom:8px; margin:36px 0 12px; }
  table { width:100%; border-collapse:collapse; font-size:13px; }
  td { padding:8px 10px; vertical-align:top; border-bottom:1px solid rgba(255,255,255,0.06); }
  td.method { font-family:'JetBrains Mono',Consolas,monospace; font-weight:600; white-space:nowrap; width:70px; }
  td.path { font-family:'JetBrains Mono',Consolas,monospace; color:#f8fafc; white-space:nowrap; }
  td.desc { color:#94a3b8; }
  .m-GET { color:#10b981; } .m-POST { color:#f59e0b; } .m-PUT { color:#60a5fa; } .m-DELETE { color:#f43f5e; } .m-WS { color:#8b5cf6; }
  pre { background:rgba(255,255,255,0.04); border:1px solid rgba(255,255,255,0.08); border-radius:6px; padding:8px 10px; margin-top:4px; font-size:11px; color:#94a3b8; overflow-x:auto; }
</style></head><body><div class="wrap">
<h1>Lantern API Reference</h1>
<div class="sub">Generated from the live route table. See docs/API.md, docs/CONFIG.md, and docs/WEBHOOKS.md for more detail.</div>`)

		for _, section := range apiDocSections {
			fmt.Fprintf(&b, "<h2>%s</h2><table><tbody>", htmlEscape(section.Title))
			for _, route := range section.Routes {
				example := ""
				if route.Example != "" {
					example = "<pre>" + htmlEscape(route.Example) + "</pre>"
				}
				fmt.Fprintf(&b, `<tr><td class="method m-%s">%s</td><td class="path">%s</td><td class="desc">%s%s</td></tr>`,
					htmlEscape(route.Method), htmlEscape(route.Method), htmlEscape(route.Path), htmlEscape(route.Desc), example)
			}
			b.WriteString("</tbody></table>")
		}

		b.WriteString("</div></body></html>")

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, b.String())
	}
}

// htmlEscape escapes the handful of characters that matter when building
// apiDocSections' hand-written text into the docs page's HTML.
func htmlEscape(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
}

// ---------------------------------------------------------------------------
// Demo seed data
// ---------------------------------------------------------------------------

func seedDemoData(db *sql.DB) {
	log.Printf("Lantern demo mode: seeded 6 events")
	services := []string{"nginx", "postgres", "redis", "grafana", "pihole", "gitea"}
	now := time.Now().UTC().Format(time.RFC3339)
	for _, s := range services {
		db.Exec(`INSERT INTO status_events (service_name, status, message, timestamp) VALUES (?, 'up', 'Demo', ?)`,
			s, now)
	}
}

// computeServiceMetricsUnified computes 7-day and 30-day uptime in a single
// pass over 30 days of events.
//
// It used to also build 30 daily status buckets — and every one of its three
// callers discarded them with `_`. That was not free: the bucket loop ran a
// maintenance lookup per bucket, so each call spent 30 extra queries producing
// a value nothing read, on every uncached dashboard poll, every WebSocket
// broadcast, and every Prometheus scrape. The bucketing logic that is actually
// used lives in handleGetStrip.
func computeServiceMetricsUnified(db *sql.DB, serviceName string) (float64, float64) {
	now := time.Now().UTC()
	since30d := now.Add(-30 * 24 * time.Hour)
	since7d := now.Add(-7 * 24 * time.Hour)

	events, err := fetchEvents(db, serviceName, since30d)
	if err != nil {
		return 0, 0
	}
	prior := fetchLastEventBefore(db, serviceName, since30d)
	timeline := buildTimeline(prior, events, since30d)

	totalSec30d := now.Sub(since30d).Seconds()
	if totalSec30d <= 0 {
		totalSec30d = 1
	}
	totalSec7d := now.Sub(since7d).Seconds()
	if totalSec7d <= 0 {
		totalSec7d = 1
	}

	var downSec30d, downSec7d, emptySec30d, emptySec7d float64

	for i, s := range timeline {
		var end time.Time
		if i+1 < len(timeline) {
			end = timeline[i+1].start
		} else {
			end = now
		}
		if end.After(now) {
			end = now
		}

		dur30d := end.Sub(s.start).Seconds()
		if dur30d < 0 {
			dur30d = 0
		}

		segStart7d := s.start
		if segStart7d.Before(since7d) {
			segStart7d = since7d
		}
		var dur7d float64
		if end.After(since7d) {
			dur7d = end.Sub(segStart7d).Seconds()
			if dur7d < 0 {
				dur7d = 0
			}
		}

		if s.status == "empty" {
			// Service didn't exist yet for this stretch; exclude it from
			// both denominators instead of counting it as up.
			emptySec30d += dur30d
			emptySec7d += dur7d
			continue
		}

		if isDown(s.status) {
			downSec30d += dur30d
			downSec7d += dur7d
		}
	}

	effectiveTotalSec30d := totalSec30d - emptySec30d
	if effectiveTotalSec30d <= 0 {
		effectiveTotalSec30d = 1
	}
	effectiveTotalSec7d := totalSec7d - emptySec7d
	if effectiveTotalSec7d <= 0 {
		effectiveTotalSec7d = 1
	}

	pct30d := math.Round(((effectiveTotalSec30d-downSec30d)/effectiveTotalSec30d*100)*10) / 10
	if pct30d > 100 {
		pct30d = 100
	}
	if pct30d < 0 {
		pct30d = 0
	}

	pct7d := math.Round(((effectiveTotalSec7d-downSec7d)/effectiveTotalSec7d*100)*100) / 100
	if pct7d > 100 {
		pct7d = 100
	}
	if pct7d < 0 {
		pct7d = 0
	}

	return pct7d, pct30d
}

type cachedServiceMetrics struct {
	Uptime7d  float64
	Uptime30d float64
	CachedAt  time.Time
}

var (
	metricsCacheMutex sync.RWMutex
	metricsCache      = make(map[string]cachedServiceMetrics)
)

func getCachedOrComputeServiceMetrics(db *sql.DB, serviceName string) (float64, float64) {
	metricsCacheMutex.RLock()
	c, ok := metricsCache[serviceName]
	metricsCacheMutex.RUnlock()

	if ok && time.Since(c.CachedAt) < 15*time.Second {
		return c.Uptime7d, c.Uptime30d
	}

	up7, up30 := computeServiceMetricsUnified(db, serviceName)

	metricsCacheMutex.Lock()
	metricsCache[serviceName] = cachedServiceMetrics{
		Uptime7d:  up7,
		Uptime30d: up30,
		CachedAt:  time.Now(),
	}
	metricsCacheMutex.Unlock()

	return up7, up30
}

func invalidateServiceMetricsCache(serviceName string) {
	metricsCacheMutex.Lock()
	delete(metricsCache, serviceName)
	metricsCacheMutex.Unlock()
}

// ---------------------------------------------------------------------------
// Service lifecycle: manual check trigger and service deletion
// ---------------------------------------------------------------------------

// serviceScopedTables lists every table keyed by service_name that holds a
// service's own configuration or history, and so must be cleared when the
// service is deleted.
//
// webhook_deliveries is deliberately absent: it is an audit log of what was
// actually sent, and outlives the service it refers to. webhook_configs has no
// service_name column at all — webhooks are configured per channel, globally.
var serviceScopedTables = []string{
	"status_events",
	"diagnostic_runs",
	"service_maintenance",
	"maintenance_windows",
	"service_groups",
	"active_monitors",
	"api_tokens",
	"service_alert_routes",
}

// handleDeleteService handles DELETE /api/services/{name}.
//
// This removes Lantern's record of a service, not the service itself. A
// running container that Docker discovery can still see is re-registered on
// the next poll (LANTERN_DOCKER_POLL_SECONDS, default 60), so the UI warns
// about that before calling this.
func handleDeleteService(db *sql.DB, scheduler *monitorScheduler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimSpace(mux.Vars(r)["name"])
		if name == "" {
			writeError(w, http.StatusBadRequest, "service name is required")
			return
		}

		var count int
		if err := db.QueryRow(
			`SELECT COUNT(*) FROM status_events WHERE service_name = ?`, name).Scan(&count); err != nil {
			log.Printf("handleDeleteService count error: %v", err)
			writeError(w, http.StatusInternalServerError, "database error")
			return
		}
		if count == 0 {
			writeError(w, http.StatusNotFound, "unknown service")
			return
		}

		// Stop the active-monitor ticker before the rows go. Deleting the
		// active_monitors row on its own leaves the goroutine running, and it
		// keeps writing status_events — which silently resurrects the service
		// we were asked to remove. stop() is a no-op when nothing is scheduled.
		scheduler.stop(name)

		tx, err := db.Begin()
		if err != nil {
			log.Printf("handleDeleteService begin error: %v", err)
			writeError(w, http.StatusInternalServerError, "database error")
			return
		}
		defer func() { _ = tx.Rollback() }()

		deleted := make(map[string]int64, len(serviceScopedTables))
		for _, table := range serviceScopedTables {
			// Table names come from the package-level slice above, never from
			// the request, so interpolating them here is not injectable.
			res, err := tx.Exec(`DELETE FROM `+table+` WHERE service_name = ?`, name)
			if err != nil {
				log.Printf("handleDeleteService delete from %s: %v", table, err)
				writeError(w, http.StatusInternalServerError, "database error")
				return
			}
			n, _ := res.RowsAffected()
			deleted[table] = n
		}

		if err := tx.Commit(); err != nil {
			log.Printf("handleDeleteService commit error: %v", err)
			writeError(w, http.StatusInternalServerError, "database error")
			return
		}

		// Without this the cache keeps serving the deleted service's uptime for
		// up to 15 seconds, and a service recreated under the same name inherits
		// the old figures.
		invalidateServiceMetricsCache(name)

		log.Printf("service deleted: %s (%v)", name, deleted)
		writeJSON(w, http.StatusOK, map[string]any{
			"status":       "ok",
			"service_name": name,
			"deleted":      deleted,
		})
	}
}

// handlePostServiceCheck handles POST /api/services/{name}/check: runs one
// active-monitor probe immediately rather than waiting for the next tick.
//
// Returns 409 when there is nothing to probe. Push-based services and
// containers found by Docker discovery report their own status, so there is no
// target for Lantern to check on demand.
func handlePostServiceCheck(db *sql.DB, scheduler *monitorScheduler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimSpace(mux.Vars(r)["name"])
		if name == "" {
			writeError(w, http.StatusBadRequest, "service name is required")
			return
		}

		var monitorType, target string
		var enabled int
		var bodyPattern, jsonPath, jsonExpect sql.NullString
		err := db.QueryRow(
			`SELECT monitor_type, target, enabled, body_pattern, json_path, json_expect FROM active_monitors WHERE service_name = ?`,
			name).Scan(&monitorType, &target, &enabled, &bodyPattern, &jsonPath, &jsonExpect)
		if err == sql.ErrNoRows {
			writeError(w, http.StatusConflict, "no active monitor configured for this service")
			return
		}
		if err != nil {
			log.Printf("handlePostServiceCheck db error: %v", err)
			writeError(w, http.StatusInternalServerError, "database error")
			return
		}
		if enabled != 1 {
			writeError(w, http.StatusConflict, "active monitor is disabled for this service")
			return
		}

		scheduler.pool.enqueue(monitorCheckJob{
			serviceName: name,
			monitorType: monitorType,
			target:      target,
			BodyPattern: nullStringPtr(bodyPattern),
			JSONPath:    nullStringPtr(jsonPath),
			JSONExpect:  nullStringPtr(jsonExpect),
		})
		// 202: the probe is queued, and its result arrives over the normal
		// status_update / heartbeat broadcast once the worker runs it.
		writeJSON(w, http.StatusAccepted, map[string]string{
			"status":       "queued",
			"service_name": name,
			"monitor_type": monitorType,
		})
	}
}
