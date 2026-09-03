package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// Notification schedule: quiet hours, either muting or digesting alerts
// ---------------------------------------------------------------------------
//
// Maintenance mode (service_maintenance) is a per-service, manually-toggled
// switch. This is the other axis: one global, time-of-day window — e.g.
// "22:00-08:00 UTC" — during which every service's alerts are either
// dropped (mode "mute", matching maintenance's behavior) or queued and sent
// as a single combined message once the window closes (mode "digest").
// Singleton row (id=1), same pattern as admin_credentials.

const notificationScheduleSchema = `
CREATE TABLE IF NOT EXISTS notification_schedule (
    id           INTEGER PRIMARY KEY CHECK (id = 1),
    enabled      INTEGER NOT NULL DEFAULT 0,
    start_minute INTEGER NOT NULL DEFAULT 0,
    end_minute   INTEGER NOT NULL DEFAULT 0,
    mode         TEXT    NOT NULL DEFAULT 'mute',
    updated_at   DATETIME
);

CREATE TABLE IF NOT EXISTS notification_digest_queue (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    service_name TEXT    NOT NULL,
    old_status   TEXT    NOT NULL,
    new_status   TEXT    NOT NULL,
    message      TEXT,
    occurred_at  DATETIME NOT NULL
);
`

// notificationSchedule mirrors the notification_schedule row.
type notificationSchedule struct {
	Enabled     bool   `json:"enabled"`
	StartMinute int    `json:"start_minute"` // minutes since midnight UTC, 0-1439
	EndMinute   int    `json:"end_minute"`   // minutes since midnight UTC, 0-1439
	Mode        string `json:"mode"`         // "mute" or "digest"
}

func getNotificationSchedule(db *sql.DB) notificationSchedule {
	var s notificationSchedule
	var enabled int
	err := db.QueryRow(`SELECT enabled, start_minute, end_minute, mode FROM notification_schedule WHERE id = 1`).
		Scan(&enabled, &s.StartMinute, &s.EndMinute, &s.Mode)
	if err != nil {
		return notificationSchedule{Mode: "mute"}
	}
	s.Enabled = enabled == 1
	return s
}

// quietHoursActive reports whether now falls inside the configured window,
// and which mode applies. A window where end < start wraps past midnight
// (e.g. 22:00-08:00); start == end means "no window" regardless of enabled.
func quietHoursActive(db *sql.DB, now time.Time) (active bool, mode string) {
	s := getNotificationSchedule(db)
	if !s.Enabled || s.StartMinute == s.EndMinute {
		return false, s.Mode
	}
	minute := now.UTC().Hour()*60 + now.UTC().Minute()
	if s.StartMinute < s.EndMinute {
		active = minute >= s.StartMinute && minute < s.EndMinute
	} else {
		active = minute >= s.StartMinute || minute < s.EndMinute
	}
	return active, s.Mode
}

func queueDigestEvent(db *sql.DB, serviceName, oldStatus, newStatus, message string, ts time.Time) {
	if _, err := db.Exec(
		`INSERT INTO notification_digest_queue (service_name, old_status, new_status, message, occurred_at) VALUES (?,?,?,?,?)`,
		serviceName, oldStatus, newStatus, message, ts.UTC().Format(time.RFC3339),
	); err != nil {
		log.Printf("queueDigestEvent: failed to queue %s: %v", serviceName, err)
	}
}

type digestEvent struct {
	ServiceName          string
	OldStatus, NewStatus string
	Message              string
	OccurredAt           string
}

// drainDigestQueue returns every queued event and deletes them, atomically —
// a flush that then failed to send would otherwise either lose events (if
// deleted first) or resend them forever (if never deleted).
func drainDigestQueue(db *sql.DB) []digestEvent {
	tx, err := db.Begin()
	if err != nil {
		log.Printf("drainDigestQueue: begin failed: %v", err)
		return nil
	}
	defer func() { _ = tx.Rollback() }()

	rows, err := tx.Query(`SELECT service_name, old_status, new_status, COALESCE(message, ''), occurred_at
		FROM notification_digest_queue ORDER BY id ASC`)
	if err != nil {
		log.Printf("drainDigestQueue: query failed: %v", err)
		return nil
	}
	var events []digestEvent
	for rows.Next() {
		var e digestEvent
		if err := rows.Scan(&e.ServiceName, &e.OldStatus, &e.NewStatus, &e.Message, &e.OccurredAt); err != nil {
			continue
		}
		events = append(events, e)
	}
	rows.Close()
	if len(events) == 0 {
		return nil
	}

	if _, err := tx.Exec(`DELETE FROM notification_digest_queue`); err != nil {
		log.Printf("drainDigestQueue: delete failed: %v", err)
		return nil
	}
	if err := tx.Commit(); err != nil {
		log.Printf("drainDigestQueue: commit failed: %v", err)
		return nil
	}
	return events
}

// dispatchDigest sends one combined message per channel, built only from
// the events that channel's routing allows — the same per-service
// channelAllowed/alertRouteFor rule dispatchWebhooks applies per event,
// just evaluated once per channel over the whole batch.
func dispatchDigest(dispatcher *webhookDispatcher, db *sql.DB, cfg *Config, events []digestEvent) {
	for _, channel := range []string{"discord", "telegram", "gotify", "generic"} {
		url, _ := getEffectiveWebhookURL(db, cfg, channel)
		if url == "" {
			continue
		}
		var routed []digestEvent
		for _, e := range events {
			if channelAllowed(alertRouteFor(db, e.ServiceName), channel) {
				routed = append(routed, e)
			}
		}
		if len(routed) == 0 {
			continue
		}

		var body []byte
		switch channel {
		case "discord":
			body = buildDiscordDigestPayload(routed)
		default:
			body, _ = json.Marshal(map[string]any{
				channelDigestKey(channel): buildTextDigest(routed),
			})
		}

		dispatcher.enqueue(webhookJob{
			channel: channel, url: url, payload: body,
			service: "(digest)", oldStatus: "digest", newStatus: fmt.Sprintf("%d events", len(routed)),
		})
	}
}

// channelDigestKey mirrors the JSON field name dispatchWebhooks already uses
// per channel for a plain-text payload (telegram/gotify's "message" alias
// aside, gotify is the one channel that also wants a title).
func channelDigestKey(channel string) string {
	if channel == "telegram" {
		return "text"
	}
	return "message"
}

func buildTextDigest(events []digestEvent) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Lantern notification digest — %d event(s):\n", len(events))
	for _, e := range events {
		fmt.Fprintf(&b, "- %s: %s → %s", e.ServiceName, strings.ToUpper(e.OldStatus), strings.ToUpper(e.NewStatus))
		if e.Message != "" {
			fmt.Fprintf(&b, " (%s)", e.Message)
		}
		b.WriteString("\n")
	}
	return b.String()
}

// discordEmbedFieldLimit is Discord's own cap on fields per embed.
const discordEmbedFieldLimit = 25

func buildDiscordDigestPayload(events []digestEvent) []byte {
	fields := make([]discordEmbedField, 0, len(events))
	shown := events
	truncated := 0
	if len(shown) > discordEmbedFieldLimit {
		truncated = len(shown) - (discordEmbedFieldLimit - 1)
		shown = shown[:discordEmbedFieldLimit-1]
	}
	for _, e := range shown {
		value := fmt.Sprintf("%s → %s", strings.ToUpper(e.OldStatus), strings.ToUpper(e.NewStatus))
		if e.Message != "" {
			value += "\n" + e.Message
		}
		fields = append(fields, discordEmbedField{Name: e.ServiceName, Value: value})
	}
	if truncated > 0 {
		fields = append(fields, discordEmbedField{Name: "…", Value: fmt.Sprintf("%d more event(s) not shown", truncated)})
	}
	embed := discordEmbed{
		Title:     fmt.Sprintf("📋 Notification Digest — %d event(s)", len(events)),
		Color:     0x64748b,
		Fields:    fields,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	}
	body, _ := json.Marshal(map[string]any{"embeds": []discordEmbed{embed}})
	return body
}

// flushDigestIfDue is called from a 1-minute ticker (see runDigestFlusher).
// It only flushes once the window has closed — while still inside it,
// events keep queuing — and flushes whatever is queued regardless of the
// schedule's current mode, so switching mute -> digest -> off mid-window
// never strands events unsent.
func flushDigestIfDue(dispatcher *webhookDispatcher, db *sql.DB, cfg *Config) {
	active, _ := quietHoursActive(db, time.Now())
	if active {
		return
	}
	events := drainDigestQueue(db)
	if len(events) == 0 {
		return
	}
	dispatchDigest(dispatcher, db, cfg, events)
}

func runDigestFlusher(dispatcher *webhookDispatcher, db *sql.DB, cfg *Config) {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		flushDigestIfDue(dispatcher, db, cfg)
	}
}

// ---------------------------------------------------------------------------
// HTTP handlers
// ---------------------------------------------------------------------------

// handleGetNotificationSchedule handles GET /api/notifications/schedule.
func handleGetNotificationSchedule(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, getNotificationSchedule(db))
	}
}

// handlePutNotificationSchedule handles PUT /api/notifications/schedule.
func handlePutNotificationSchedule(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req notificationSchedule
		if !decodeJSONBody(w, r, maxConfigBody, &req) {
			return
		}
		if req.StartMinute < 0 || req.StartMinute > 1439 || req.EndMinute < 0 || req.EndMinute > 1439 {
			writeError(w, http.StatusBadRequest, "start_minute and end_minute must be between 0 and 1439")
			return
		}
		req.Mode = strings.ToLower(strings.TrimSpace(req.Mode))
		if req.Mode != "mute" && req.Mode != "digest" {
			writeError(w, http.StatusBadRequest, `mode must be "mute" or "digest"`)
			return
		}
		enabledInt := 0
		if req.Enabled {
			enabledInt = 1
		}
		if _, err := db.Exec(`
INSERT INTO notification_schedule (id, enabled, start_minute, end_minute, mode, updated_at)
VALUES (1, ?, ?, ?, ?, CURRENT_TIMESTAMP)
ON CONFLICT(id) DO UPDATE SET
    enabled = excluded.enabled, start_minute = excluded.start_minute,
    end_minute = excluded.end_minute, mode = excluded.mode, updated_at = CURRENT_TIMESTAMP`,
			enabledInt, req.StartMinute, req.EndMinute, req.Mode); err != nil {
			log.Printf("handlePutNotificationSchedule db error: %v", err)
			writeError(w, http.StatusInternalServerError, "database error")
			return
		}
		recordAudit(db, r, "notification_schedule_change", "", true,
			fmt.Sprintf("enabled=%t start=%d end=%d mode=%s", req.Enabled, req.StartMinute, req.EndMinute, req.Mode))

		writeJSON(w, http.StatusOK, req)
	}
}
