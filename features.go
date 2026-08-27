// Announcement banners, per-service alert routing, and portable configuration
// export/import. These three share a file because they are all operator-facing
// configuration surfaces rather than parts of the monitoring pipeline.
package main

import (
	"database/sql"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/gorilla/mux"
)

// featureSchema is applied alongside the main schema in initDB.
const featureSchema = `
CREATE TABLE IF NOT EXISTS incident_banners (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    level        TEXT     NOT NULL,
    title        TEXT     NOT NULL,
    body         TEXT     NOT NULL DEFAULT '',
    created_at   DATETIME NOT NULL,
    dismissed_at DATETIME
);

CREATE INDEX IF NOT EXISTS idx_banners_active ON incident_banners(dismissed_at, id DESC);

CREATE TABLE IF NOT EXISTS service_alert_routes (
    service_name TEXT PRIMARY KEY,
    channels     TEXT     NOT NULL DEFAULT '',
    updated_at   DATETIME DEFAULT CURRENT_TIMESTAMP
);
`

// ---------------------------------------------------------------------------
// Announcement banner
// ---------------------------------------------------------------------------

// bannerLevels are the accepted severities, mapped in the UI to the same
// palette the status pills already use.
var bannerLevels = map[string]bool{"info": true, "warning": true, "critical": true}

// Banner is one announcement. At most one is active at a time: posting a new
// one dismisses whatever was showing, which matches how an operator actually
// uses this — the current situation replaces the previous one rather than
// stacking on top of it.
type Banner struct {
	ID        int64  `json:"id"`
	Level     string `json:"level"`
	Title     string `json:"title"`
	Body      string `json:"body,omitempty"`
	CreatedAt string `json:"created_at"`
}

// activeBanner returns the current announcement, if any. The zero value with
// ok=false means "nothing to show", which the frontend renders as no banner
// rather than as an error.
func activeBanner(db *sql.DB) (Banner, bool) {
	var b Banner
	err := db.QueryRow(`
SELECT id, level, title, body, created_at
FROM incident_banners
WHERE dismissed_at IS NULL
ORDER BY id DESC
LIMIT 1`).Scan(&b.ID, &b.Level, &b.Title, &b.Body, &b.CreatedAt)
	if err != nil {
		return Banner{}, false
	}
	return b, true
}

// handleGetBanner serves the active announcement. Registered on both the gated
// and the public router: the public status page is exactly where an outage
// notice most needs to be visible, and a banner is published deliberately, so
// it carries nothing that is not already meant to be read by anyone.
func handleGetBanner(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		b, ok := activeBanner(db)
		if !ok {
			writeJSON(w, http.StatusOK, map[string]any{"active": false})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"active": true, "banner": b})
	}
}

func handlePostBanner(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Level string `json:"level"`
			Title string `json:"title"`
			Body  string `json:"body"`
		}
		if !decodeJSONBody(w, r, maxConfigBody, &req) {
			return
		}

		req.Level = strings.ToLower(strings.TrimSpace(req.Level))
		if req.Level == "" {
			req.Level = "info"
		}
		if !bannerLevels[req.Level] {
			writeError(w, http.StatusBadRequest, "level must be one of: info, warning, critical")
			return
		}
		req.Title = strings.TrimSpace(req.Title)
		if req.Title == "" {
			writeError(w, http.StatusBadRequest, "title is required")
			return
		}

		now := time.Now().UTC().Format(time.RFC3339)

		// Dismiss-then-insert in one transaction, so there is never a moment
		// with two active banners or, worse, none at all.
		tx, err := db.Begin()
		if err != nil {
			log.Printf("handlePostBanner begin: %v", err)
			writeError(w, http.StatusInternalServerError, "database error")
			return
		}
		defer func() { _ = tx.Rollback() }()

		if _, err := tx.Exec(
			`UPDATE incident_banners SET dismissed_at = ? WHERE dismissed_at IS NULL`, now); err != nil {
			log.Printf("handlePostBanner dismiss: %v", err)
			writeError(w, http.StatusInternalServerError, "database error")
			return
		}
		res, err := tx.Exec(
			`INSERT INTO incident_banners (level, title, body, created_at) VALUES (?, ?, ?, ?)`,
			req.Level, req.Title, strings.TrimSpace(req.Body), now)
		if err != nil {
			log.Printf("handlePostBanner insert: %v", err)
			writeError(w, http.StatusInternalServerError, "database error")
			return
		}
		if err := tx.Commit(); err != nil {
			log.Printf("handlePostBanner commit: %v", err)
			writeError(w, http.StatusInternalServerError, "database error")
			return
		}

		id, _ := res.LastInsertId()
		writeJSON(w, http.StatusCreated, Banner{
			ID: id, Level: req.Level, Title: req.Title,
			Body: strings.TrimSpace(req.Body), CreatedAt: now,
		})
	}
}

// handleDeleteBanner dismisses the active announcement. Dismissal is a
// timestamp rather than a DELETE, so the record of what was announced and when
// survives — useful when reconstructing an incident afterwards.
func handleDeleteBanner(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		res, err := db.Exec(
			`UPDATE incident_banners SET dismissed_at = ? WHERE dismissed_at IS NULL`,
			time.Now().UTC().Format(time.RFC3339))
		if err != nil {
			log.Printf("handleDeleteBanner: %v", err)
			writeError(w, http.StatusInternalServerError, "database error")
			return
		}
		n, _ := res.RowsAffected()
		writeJSON(w, http.StatusOK, map[string]any{"dismissed": n})
	}
}

// ---------------------------------------------------------------------------
// Per-service alert routing
// ---------------------------------------------------------------------------

var alertChannels = map[string]bool{"discord": true, "telegram": true, "gotify": true, "generic": true}

// alertRouteFor returns the channels a service's alerts should go to.
//
// An absent row, or a row with an empty channel list, means "every configured
// channel" — which is what Lantern did before routing existed, so an upgrade
// changes nothing until an operator opts a service in.
func alertRouteFor(db *sql.DB, serviceName string) []string {
	var raw string
	if err := db.QueryRow(
		`SELECT channels FROM service_alert_routes WHERE service_name = ?`, serviceName).Scan(&raw); err != nil {
		return nil
	}
	return parseChannels(raw)
}

func parseChannels(raw string) []string {
	var out []string
	for _, c := range strings.Split(raw, ",") {
		if c = strings.ToLower(strings.TrimSpace(c)); c != "" && alertChannels[c] {
			out = append(out, c)
		}
	}
	sort.Strings(out)
	return out
}

// channelAllowed reports whether a channel should receive this service's
// alerts. A nil route means unrouted, which allows everything.
func channelAllowed(route []string, channel string) bool {
	if len(route) == 0 {
		return true
	}
	for _, c := range route {
		if c == channel {
			return true
		}
	}
	return false
}

func handleGetServiceAlerts(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimSpace(mux.Vars(r)["name"])
		route := alertRouteFor(db, name)
		writeJSON(w, http.StatusOK, map[string]any{
			"service_name": name,
			"channels":     route,
			// Explicit, because an empty list is meaningfully different from
			// "no channels": it means every configured channel.
			"routes_all": len(route) == 0,
		})
	}
}

func handlePutServiceAlerts(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimSpace(mux.Vars(r)["name"])
		if name == "" {
			writeError(w, http.StatusBadRequest, "service name is required")
			return
		}

		scopedSvc := r.Context().Value(scopedServiceKey)
		if scopedSvc != nil && scopedSvc.(string) != name {
			writeError(w, http.StatusForbidden, "token not scoped for this service")
			return
		}

		var req struct {
			Channels []string `json:"channels"`
		}
		if !decodeJSONBody(w, r, maxConfigBody, &req) {
			return
		}

		var clean []string
		for _, c := range req.Channels {
			c = strings.ToLower(strings.TrimSpace(c))
			if c == "" {
				continue
			}
			if !alertChannels[c] {
				writeError(w, http.StatusBadRequest,
					fmt.Sprintf("unknown channel %q; valid channels are discord, telegram, gotify, generic", c))
				return
			}
			if !channelAllowed(clean, c) || len(clean) == 0 {
				clean = append(clean, c)
			}
		}
		sort.Strings(clean)
		clean = dedupeSorted(clean)

		if len(clean) == 0 {
			// Clearing the route restores the default of alerting everywhere,
			// rather than silencing the service — silencing is what maintenance
			// mode is for, and conflating the two would be a trap.
			if _, err := db.Exec(`DELETE FROM service_alert_routes WHERE service_name = ?`, name); err != nil {
				log.Printf("handlePutServiceAlerts delete: %v", err)
				writeError(w, http.StatusInternalServerError, "database error")
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{
				"service_name": name, "channels": []string{}, "routes_all": true,
			})
			return
		}

		if _, err := db.Exec(`
INSERT INTO service_alert_routes (service_name, channels, updated_at) VALUES (?, ?, CURRENT_TIMESTAMP)
ON CONFLICT(service_name) DO UPDATE SET channels = excluded.channels, updated_at = CURRENT_TIMESTAMP`,
			name, strings.Join(clean, ",")); err != nil {
			log.Printf("handlePutServiceAlerts upsert: %v", err)
			writeError(w, http.StatusInternalServerError, "database error")
			return
		}

		writeJSON(w, http.StatusOK, map[string]any{
			"service_name": name, "channels": clean, "routes_all": false,
		})
	}
}

func dedupeSorted(in []string) []string {
	if len(in) < 2 {
		return in
	}
	out := in[:1]
	for _, v := range in[1:] {
		if v != out[len(out)-1] {
			out = append(out, v)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Configuration export / import
// ---------------------------------------------------------------------------

// redactedPlaceholder marks a secret the export deliberately withheld. On
// import it means "leave whatever is already stored alone", so a redacted
// export can be re-imported without wiping the credentials it omitted.
const redactedPlaceholder = "__REDACTED__"

// ConfigExport is the portable shape of a Lantern installation: what it
// watches and how it alerts, but never its history. Status events are
// deliberately excluded — they are large, they are observations rather than
// configuration, and /api/backup already exists for a full snapshot.
type ConfigExport struct {
	Version    string          `json:"version"`
	ExportedAt string          `json:"exported_at"`
	Redacted   bool            `json:"redacted"`
	Services   []ConfigService `json:"services"`
	Webhooks   []ConfigWebhook `json:"webhooks"`
}

type ConfigService struct {
	Name  string `json:"name"`
	Group string `json:"group,omitempty"`
	// Monitor is nil for push-based and Docker-discovered services.
	Monitor       *ConfigMonitor `json:"monitor,omitempty"`
	AlertChannels []string       `json:"alert_channels,omitempty"`
	Maintenance   bool           `json:"maintenance,omitempty"`
}

type ConfigMonitor struct {
	Type            string `json:"type"`
	Target          string `json:"target"`
	IntervalSeconds int    `json:"interval_seconds"`
	Enabled         bool   `json:"enabled"`
}

type ConfigWebhook struct {
	Channel string `json:"channel"`
	URL     string `json:"url"`
}

// buildConfigExport gathers current configuration. includeSecrets is opt-in and
// off by default: a webhook URL is the credential, so an export that carries
// them is as sensitive as a database backup and far easier to paste into an
// issue by accident.
func buildConfigExport(db *sql.DB, includeSecrets bool) (ConfigExport, error) {
	out := ConfigExport{
		Version:    version,
		ExportedAt: time.Now().UTC().Format(time.RFC3339),
		Redacted:   !includeSecrets,
		Services:   []ConfigService{},
		Webhooks:   []ConfigWebhook{},
	}

	// The service list is every name Lantern currently knows about, from any
	// source, so a discovered container is exported alongside a configured one.
	rows, err := db.Query(`
SELECT DISTINCT service_name FROM (
    SELECT service_name FROM status_events
    UNION SELECT service_name FROM active_monitors
    UNION SELECT service_name FROM service_groups
    UNION SELECT service_name FROM service_alert_routes
) ORDER BY service_name ASC`)
	if err != nil {
		return out, err
	}
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err == nil && n != "" {
			names = append(names, n)
		}
	}
	rows.Close()

	for _, name := range names {
		svc := ConfigService{Name: name}

		var group sql.NullString
		_ = db.QueryRow(`SELECT group_name FROM service_groups WHERE service_name = ?`, name).Scan(&group)
		if group.Valid {
			svc.Group = group.String
		}

		var m ConfigMonitor
		var enabled int
		err := db.QueryRow(
			`SELECT monitor_type, target, interval_seconds, enabled FROM active_monitors WHERE service_name = ?`,
			name).Scan(&m.Type, &m.Target, &m.IntervalSeconds, &enabled)
		if err == nil {
			m.Enabled = enabled == 1
			svc.Monitor = &m
		}

		svc.AlertChannels = alertRouteFor(db, name)

		var maint int
		_ = db.QueryRow(`SELECT enabled FROM service_maintenance WHERE service_name = ?`, name).Scan(&maint)
		svc.Maintenance = maint == 1

		out.Services = append(out.Services, svc)
	}

	wrows, err := db.Query(`SELECT channel, url FROM webhook_configs ORDER BY channel ASC`)
	if err != nil {
		return out, err
	}
	defer wrows.Close()
	for wrows.Next() {
		var c ConfigWebhook
		if err := wrows.Scan(&c.Channel, &c.URL); err != nil {
			continue
		}
		if !includeSecrets {
			c.URL = redactedPlaceholder
		}
		out.Webhooks = append(out.Webhooks, c)
	}

	return out, nil
}

func handleConfigExport(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		includeSecrets := strings.EqualFold(r.URL.Query().Get("include_secrets"), "true")

		cfgExport, err := buildConfigExport(db, includeSecrets)
		if err != nil {
			log.Printf("handleConfigExport: %v", err)
			writeError(w, http.StatusInternalServerError, "database error")
			return
		}

		if includeSecrets {
			log.Printf("config export requested WITH secrets (%d webhook URLs in cleartext)", len(cfgExport.Webhooks))
		}

		w.Header().Set("Content-Disposition",
			fmt.Sprintf(`attachment; filename="lantern-config-%s.json"`, time.Now().UTC().Format("20060102-150405")))
		writeJSON(w, http.StatusOK, cfgExport)
	}
}

// handleConfigImport applies an exported configuration.
//
// Additive and idempotent by design: it upserts what the file describes and
// never deletes anything the file omits. An import is therefore safe to run
// against a populated instance, and running it twice changes nothing the second
// time. Removing a service remains an explicit DELETE.
func handleConfigImport(db *sql.DB, scheduler *monitorScheduler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var in ConfigExport
		if !decodeJSONBody(w, r, maxImportBody, &in) {
			return
		}

		applied := map[string]int{"groups": 0, "monitors": 0, "alert_routes": 0, "webhooks": 0, "maintenance": 0}
		var problems []string

		for _, svc := range in.Services {
			name := strings.TrimSpace(svc.Name)
			if name == "" {
				continue
			}

			if svc.Group != "" {
				if _, err := db.Exec(`
INSERT INTO service_groups (service_name, group_name, updated_at) VALUES (?, ?, CURRENT_TIMESTAMP)
ON CONFLICT(service_name) DO UPDATE SET group_name = excluded.group_name, updated_at = CURRENT_TIMESTAMP`,
					name, svc.Group); err != nil {
					problems = append(problems, fmt.Sprintf("%s: group: %v", name, err))
				} else {
					applied["groups"]++
				}
			}

			if m := svc.Monitor; m != nil {
				mtype := strings.ToLower(strings.TrimSpace(m.Type))
				if !validMonitorTypes[mtype] {
					problems = append(problems, fmt.Sprintf("%s: unknown monitor type %q", name, m.Type))
				} else if err := validateMonitorTarget(mtype, m.Target); err != nil {
					problems = append(problems, fmt.Sprintf("%s: %v", name, err))
				} else {
					interval := m.IntervalSeconds
					if interval < minMonitorIntervalSeconds {
						interval = minMonitorIntervalSeconds
					}
					if interval > maxMonitorIntervalSeconds {
						interval = maxMonitorIntervalSeconds
					}
					enabled := 0
					if m.Enabled {
						enabled = 1
					}
					if _, err := db.Exec(`
INSERT INTO active_monitors (service_name, monitor_type, target, interval_seconds, enabled, updated_at)
VALUES (?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
ON CONFLICT(service_name) DO UPDATE SET
    monitor_type = excluded.monitor_type, target = excluded.target,
    interval_seconds = excluded.interval_seconds, enabled = excluded.enabled,
    updated_at = CURRENT_TIMESTAMP`,
						name, mtype, strings.TrimSpace(m.Target), interval, enabled); err != nil {
						problems = append(problems, fmt.Sprintf("%s: monitor: %v", name, err))
					} else {
						applied["monitors"]++
						// Reflect the change in the running scheduler, or the
						// import would not take effect until the next restart.
						if m.Enabled {
							scheduler.start(name, mtype, strings.TrimSpace(m.Target), interval)
						} else {
							scheduler.stop(name)
						}
					}
				}
			}

			if len(svc.AlertChannels) > 0 {
				clean := dedupeSorted(parseChannels(strings.Join(svc.AlertChannels, ",")))
				if len(clean) > 0 {
					if _, err := db.Exec(`
INSERT INTO service_alert_routes (service_name, channels, updated_at) VALUES (?, ?, CURRENT_TIMESTAMP)
ON CONFLICT(service_name) DO UPDATE SET channels = excluded.channels, updated_at = CURRENT_TIMESTAMP`,
						name, strings.Join(clean, ",")); err != nil {
						problems = append(problems, fmt.Sprintf("%s: alert route: %v", name, err))
					} else {
						applied["alert_routes"]++
					}
				}
			}

			if svc.Maintenance {
				setMaintenanceState(db, name, true, "restored from config import")
				applied["maintenance"]++
			}
		}

		for _, wh := range in.Webhooks {
			channel := strings.ToLower(strings.TrimSpace(wh.Channel))
			if !alertChannels[channel] {
				problems = append(problems, fmt.Sprintf("unknown webhook channel %q", wh.Channel))
				continue
			}
			// A redacted export carries the placeholder rather than the URL.
			// Skipping it preserves whatever is already configured instead of
			// overwriting a working webhook with a literal "__REDACTED__".
			if wh.URL == redactedPlaceholder || strings.TrimSpace(wh.URL) == "" {
				continue
			}
			if _, err := db.Exec(`
INSERT INTO webhook_configs (channel, url) VALUES (?, ?)
ON CONFLICT(channel) DO UPDATE SET url = excluded.url`, channel, strings.TrimSpace(wh.URL)); err != nil {
				problems = append(problems, fmt.Sprintf("webhook %s: %v", channel, err))
			} else {
				applied["webhooks"]++
			}
		}

		resp := map[string]any{"status": "ok", "applied": applied}
		if len(problems) > 0 {
			// Reported rather than fatal: one bad service should not discard an
			// otherwise valid import, and the caller needs to know what was
			// skipped.
			resp["skipped"] = problems
		}
		writeJSON(w, http.StatusOK, resp)
	}
}
