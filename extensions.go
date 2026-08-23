package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/http"
	"strconv"
	"strings"
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

// parseRange converts a range string to hours.
func parseRange(rng string) int {
	switch rng {
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
func buildTimeline(prior *rawEvent, events []rawEvent, since time.Time) []segment {
	startStatus := "unknown"
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

		totalSec := now.Sub(since).Seconds()
		if totalSec <= 0 {
			totalSec = 1
		}

		var downSec float64
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
			if isDown(seg.status) {
				if !isInMaintenance(db, name, seg.start) {
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

		uptimePct := math.Round(((totalSec-downSec)/totalSec*100)*100) / 100
		if uptimePct > 100 {
			uptimePct = 100
		}
		if uptimePct < 0 {
			uptimePct = 0
		}

		// Generate datapoints
		var dpInterval time.Duration
		switch rng {
		case "24h":
			dpInterval = 1 * time.Hour
		case "30d":
			dpInterval = 24 * time.Hour
		default:
			dpInterval = 6 * time.Hour
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
					inMaint := isInMaintenance(db, name, *incStart)
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
			inMaint := isInMaintenance(db, name, *incStart)
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

func handlePutMaintenance(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		vars := mux.Vars(r)
		name := vars["name"]
		var req struct {
			Enabled bool   `json:"enabled"`
			Note    string `json:"note"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid json")
			return
		}
		now := time.Now().UTC().Format(time.RFC3339)

		val := 0
		if req.Enabled {
			val = 1
		}
		db.Exec(`INSERT INTO service_maintenance (service_name, enabled, note, updated_at)
			VALUES (?, ?, ?, ?) ON CONFLICT(service_name) DO UPDATE SET enabled=?, note=?, updated_at=?`,
			name, val, req.Note, now, val, req.Note, now)

		if req.Enabled {
			db.Exec(`INSERT INTO maintenance_windows (service_name, started_at, note) VALUES (?, ?, ?)`,
				name, now, req.Note)
		} else {
			db.Exec(`UPDATE maintenance_windows SET ended_at = ? WHERE service_name = ? AND ended_at IS NULL`,
				now, name)
		}

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

func handleDocs() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, "<html><head><title>API Docs</title></head><body><h1>Lantern API Docs</h1></body></html>")
	}
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

// ---------------------------------------------------------------------------
// Compute real 7-day uptime for the services list endpoint
// ---------------------------------------------------------------------------

func computeUptime7d(db *sql.DB, serviceName string) float64 {
	now := time.Now().UTC()
	since := now.Add(-7 * 24 * time.Hour)

	events, err := fetchEvents(db, serviceName, since)
	if err != nil {
		return 0
	}
	prior := fetchLastEventBefore(db, serviceName, since)
	timeline := buildTimeline(prior, events, since)

	totalSec := now.Sub(since).Seconds()
	if totalSec <= 0 {
		return 100
	}

	var downSec float64
	for i, s := range timeline {
		var end time.Time
		if i+1 < len(timeline) {
			end = timeline[i+1].start
		} else {
			end = now
		}
		dur := end.Sub(s.start).Seconds()
		if dur < 0 {
			dur = 0
		}
		if isDown(s.status) {
			downSec += dur
		}
	}

	pct := math.Round(((totalSec-downSec)/totalSec*100)*100) / 100
	if pct > 100 {
		pct = 100
	}
	if pct < 0 {
		pct = 0
	}
	return pct
}

// ---------------------------------------------------------------------------
// Webhook helper
// ---------------------------------------------------------------------------

func fireWebhook(cfg *Config, serviceName, prevStatus, newStatus, message string, timestamp time.Time) {
	if cfg.WebhookURL == "" {
		return
	}
	payload := map[string]interface{}{
		"type":            "status_change",
		"service_name":    serviceName,
		"previous_status": prevStatus,
		"new_status":      newStatus,
		"message":         message,
		"timestamp":       timestamp.Format(time.RFC3339),
	}
	var body []byte
	if strings.Contains(cfg.WebhookURL, "discord.com/api/webhooks") {
		msg := fmt.Sprintf("**%s** status changed: `%s` -> `%s`\n> %s", serviceName, prevStatus, newStatus, message)
		body, _ = json.Marshal(map[string]string{"content": msg})
	} else {
		body, _ = json.Marshal(payload)
	}

	go func() {
		resp, err := http.Post(cfg.WebhookURL, "application/json", bytes.NewBuffer(body))
		if err != nil {
			log.Printf("Webhook error: %v", err)
		} else {
			resp.Body.Close()
		}
	}()
}

func computeUptime30d(db *sql.DB, serviceName string) float64 {
	now := time.Now().UTC()
	since := now.Add(-30 * 24 * time.Hour)

	events, err := fetchEvents(db, serviceName, since)
	if err != nil {
		return 0
	}
	prior := fetchLastEventBefore(db, serviceName, since)
	timeline := buildTimeline(prior, events, since)

	totalSec := now.Sub(since).Seconds()
	if totalSec <= 0 {
		return 100
	}

	var downSec float64
	for i, s := range timeline {
		var end time.Time
		if i+1 < len(timeline) {
			end = timeline[i+1].start
		} else {
			end = now
		}
		dur := end.Sub(s.start).Seconds()
		if dur < 0 {
			dur = 0
		}
		if isDown(s.status) {
			downSec += dur
		}
	}
	pct := ((totalSec - downSec) / totalSec) * 100
	if pct < 0 {
		pct = 0
	}
	// Round to 1 decimal place
	return float64(int(pct*10)) / 10
}
