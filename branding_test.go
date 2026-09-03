package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// putBrandingReq performs an authenticated PUT /api/branding against the
// handler directly (no middleware) and returns the recorder.
func putBrandingReq(t *testing.T, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	db := newTestDB(t)
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPut, "/api/branding", bytes.NewReader(raw))
	rec := httptest.NewRecorder()
	handlePutBranding(db)(rec, req)
	return rec
}

func TestHandlePutBrandingRejectsUnsafeLogoURLs(t *testing.T) {
	// A stored logo_url is written straight into an <img src>, so anything
	// that is not http(s) has to be refused at the boundary.
	bad := []string{
		"javascript:alert(1)",
		"data:image/svg+xml;base64,PHN2Zz48L3N2Zz4=",
		"vbscript:msgbox(1)",
		"file:///etc/passwd",
		"//evil.example.com/logo.png",
		"https://",
	}
	for _, u := range bad {
		t.Run(u, func(t *testing.T) {
			rec := putBrandingReq(t, map[string]any{"logo_url": u})
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("logo_url %q = %d, want 400", u, rec.Code)
			}
		})
	}
}

func TestHandlePutBrandingValidation(t *testing.T) {
	cases := []struct {
		label string
		body  map[string]any
		want  int
	}{
		{"title too long", map[string]any{"title": strings.Repeat("a", brandingTitleMaxLen+1)}, http.StatusBadRequest},
		{"title at the limit", map[string]any{"title": strings.Repeat("a", brandingTitleMaxLen)}, http.StatusOK},
		{"bad accent", map[string]any{"accent_color": "red"}, http.StatusBadRequest},
		{"accent missing hash", map[string]any{"accent_color": "10b981"}, http.StatusBadRequest},
		{"accent wrong length", map[string]any{"accent_color": "#10b98"}, http.StatusBadRequest},
		{"empty is fine", map[string]any{}, http.StatusOK},
		{"short hex is fine", map[string]any{"accent_color": "#abc"}, http.StatusOK},
		{"full set is fine", map[string]any{"title": "Acme Status", "logo_url": "https://acme.example/logo.svg", "accent_color": "#7c5cff"}, http.StatusOK},
	}
	for _, c := range cases {
		t.Run(c.label, func(t *testing.T) {
			rec := putBrandingReq(t, c.body)
			if rec.Code != c.want {
				t.Fatalf("%s = %d, want %d (body %s)", c.label, rec.Code, c.want, rec.Body.String())
			}
		})
	}
}

func TestBrandingRoundTripAndAudit(t *testing.T) {
	db := newTestDB(t)

	raw, _ := json.Marshal(map[string]any{
		"title": "Acme Status", "logo_url": "https://acme.example/logo.svg", "accent_color": "#7C5CFF",
	})
	rec := httptest.NewRecorder()
	handlePutBranding(db)(rec, httptest.NewRequest(http.MethodPut, "/api/branding", bytes.NewReader(raw)))
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	handleGetBranding(db)(rec, httptest.NewRequest(http.MethodGet, "/api/public/branding", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET = %d, want 200", rec.Code)
	}
	var got statusPageBranding
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Title != "Acme Status" || got.LogoURL != "https://acme.example/logo.svg" || got.AccentColor != "#7C5CFF" {
		t.Fatalf("round trip = %+v", got)
	}

	// Clearing a field must actually clear it, not leave the old value behind.
	raw, _ = json.Marshal(map[string]any{"title": "Acme Status"})
	rec = httptest.NewRecorder()
	handlePutBranding(db)(rec, httptest.NewRequest(http.MethodPut, "/api/branding", bytes.NewReader(raw)))
	if rec.Code != http.StatusOK {
		t.Fatalf("second PUT = %d, want 200", rec.Code)
	}
	if b := getStatusPageBranding(db); b.LogoURL != "" || b.AccentColor != "" {
		t.Fatalf("cleared fields survived: %+v", b)
	}

	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM admin_audit_log WHERE action = 'branding_change'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("branding_change audit entries = %d, want 2", n)
	}
}

func TestGetBrandingOnEmptyTableReturnsDefaults(t *testing.T) {
	db := newTestDB(t)
	rec := httptest.NewRecorder()
	handleGetBranding(db)(rec, httptest.NewRequest(http.MethodGet, "/api/public/branding", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET = %d, want 200", rec.Code)
	}
	// omitempty everywhere, so an unconfigured install serves an empty object
	// and the frontend keeps Lantern's own defaults.
	if got := rec.Body.String(); got != "{}\n" && got != "{}" {
		t.Fatalf("empty branding body = %q", got)
	}
}

func TestScopedTokenCannotChangeBranding(t *testing.T) {
	db, cfg := newAuthDB(t, "admin", "correct-horse")
	if _, err := db.Exec(`INSERT INTO api_tokens (token, service_name) VALUES (?, ?)`, "scoped-token", "webapp"); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{"title": "Pwned"})
	req := httptest.NewRequest(http.MethodPut, "/api/branding", bytes.NewReader(raw))
	req.Header.Set("Authorization", "Bearer scoped-token")
	rec := httptest.NewRecorder()
	authMiddleware(db, cfg, handlePutBranding(db)).ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("scoped token PUT branding = %d, want 403", rec.Code)
	}
	if b := getStatusPageBranding(db); b.Title != "" {
		t.Fatalf("scoped token changed branding to %q", b.Title)
	}
}
