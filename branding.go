package main

import (
	"database/sql"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync/atomic"
)

// ---------------------------------------------------------------------------
// Status page branding
// ---------------------------------------------------------------------------
//
// A single, global set of overrides for Lantern's own name, logo, and
// accent color — shown in the header on both the admin dashboard and the
// public status page, since they share one HTML page (see `isPublic` in
// static/index.html). Singleton row, same pattern as notification_schedule.
// Custom domains are a reverse-proxy concern, not application state — see
// docs/CUSTOM_DOMAIN.md — so there is no domain field here.

const brandingSchema = `
CREATE TABLE IF NOT EXISTS status_page_branding (
    id           INTEGER PRIMARY KEY CHECK (id = 1),
    title        TEXT,
    logo_url     TEXT,
    accent_color TEXT,
    updated_at   DATETIME
);
`

const (
	brandingTitleMaxLen = 60
	brandingURLMaxLen   = 2048
)

var hexColorPattern = regexp.MustCompile(`^#[0-9a-fA-F]{3}$|^#[0-9a-fA-F]{6}$`)

// statusPageBranding mirrors the status_page_branding row. Empty fields mean
// "use Lantern's default" — the frontend already knows what that is, so
// unset values are omitted from the JSON rather than sent as "".
type statusPageBranding struct {
	Title       string `json:"title,omitempty"`
	LogoURL     string `json:"logo_url,omitempty"`
	AccentColor string `json:"accent_color,omitempty"`
}

// logoOrigin caches the scheme://host of the configured logo, so
// securityHeadersMiddleware can widen img-src by exactly that one origin and
// nothing else. The CSP is otherwise "img-src 'self' data:", which would block
// the very logo the operator just configured. Cached rather than queried per
// request because every response carries this header, static assets included.
var logoOrigin atomic.Value // string

func brandingLogoOrigin() string {
	v, _ := logoOrigin.Load().(string)
	return v
}

// setBrandingLogoOrigin records the origin to allow. An unparseable or
// host-less URL stores "", which leaves the CSP at its default.
func setBrandingLogoOrigin(raw string) {
	u, err := url.Parse(raw)
	if raw == "" || err != nil || u.Host == "" {
		logoOrigin.Store("")
		return
	}
	logoOrigin.Store(u.Scheme + "://" + u.Host)
}

func getStatusPageBranding(db *sql.DB) statusPageBranding {
	var b statusPageBranding
	var title, logoURL, accent sql.NullString
	err := db.QueryRow(`SELECT title, logo_url, accent_color FROM status_page_branding WHERE id = 1`).
		Scan(&title, &logoURL, &accent)
	if err != nil {
		return statusPageBranding{}
	}
	b.Title = title.String
	b.LogoURL = logoURL.String
	b.AccentColor = accent.String
	return b
}

// handleGetBranding handles GET /api/branding (and its public mirror).
// Deliberately unauthenticated on both routers, like handleGetBanner: an
// operator-chosen name, logo and color are meant to be seen by every
// visitor, public status page included.
func handleGetBranding(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, getStatusPageBranding(db))
	}
}

// handlePutBranding handles PUT /api/branding.
func handlePutBranding(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req statusPageBranding
		if !decodeJSONBody(w, r, maxConfigBody, &req) {
			return
		}

		req.Title = strings.TrimSpace(req.Title)
		if len(req.Title) > brandingTitleMaxLen {
			writeError(w, http.StatusBadRequest, "title must be 60 characters or fewer")
			return
		}

		req.LogoURL = strings.TrimSpace(req.LogoURL)
		if req.LogoURL != "" {
			if len(req.LogoURL) > brandingURLMaxLen {
				writeError(w, http.StatusBadRequest, "logo_url is too long")
				return
			}
			// Restricted to http(s) so a stored value can never become a
			// javascript: or data: URI reflected back into an <img src>.
			u, err := url.Parse(req.LogoURL)
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
				writeError(w, http.StatusBadRequest, "logo_url must be an http(s) URL")
				return
			}
		}

		req.AccentColor = strings.TrimSpace(req.AccentColor)
		if req.AccentColor != "" && !hexColorPattern.MatchString(req.AccentColor) {
			writeError(w, http.StatusBadRequest, "accent_color must be a hex color like #7c5cff")
			return
		}

		if _, err := db.Exec(`
INSERT INTO status_page_branding (id, title, logo_url, accent_color, updated_at)
VALUES (1, ?, ?, ?, CURRENT_TIMESTAMP)
ON CONFLICT(id) DO UPDATE SET
    title = excluded.title, logo_url = excluded.logo_url,
    accent_color = excluded.accent_color, updated_at = CURRENT_TIMESTAMP`,
			nullIfEmpty(req.Title), nullIfEmpty(req.LogoURL), nullIfEmpty(req.AccentColor)); err != nil {
			log.Printf("handlePutBranding db error: %v", err)
			writeError(w, http.StatusInternalServerError, "database error")
			return
		}
		setBrandingLogoOrigin(req.LogoURL)
		recordAudit(db, r, "branding_change", "", true,
			"title="+req.Title+" logo_url_set="+boolStr(req.LogoURL != "")+" accent="+req.AccentColor)

		writeJSON(w, http.StatusOK, req)
	}
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
