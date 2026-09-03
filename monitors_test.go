package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/mux"
)

// ---------------------------------------------------------------------------
// body_pattern / json_path / json_expect: pure matching behaviour, exercised
// through checkHTTP's public signature since the matching logic lives inline
// there rather than in extracted helpers.
// ---------------------------------------------------------------------------

// fixtureServer returns an httptest.Server that always answers with the given
// status code and body, and registers its Close with t.Cleanup.
func fixtureServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestCheckHTTPBodyPatternMatching(t *testing.T) {
	cases := []struct {
		name        string
		body        string
		bodyPattern string
		wantStatus  string
		wantMsg     string // exact message; empty means "don't check, just look at status"
	}{
		{
			name:        "pattern matches",
			body:        `{"status":"healthy"}`,
			bodyPattern: `"status":"healthy"`,
			wantStatus:  "up",
		},
		{
			name:        "pattern does not match",
			body:        `{"status":"broken"}`,
			bodyPattern: `"status":"healthy"`,
			wantStatus:  "down",
			wantMsg:     "body pattern mismatch",
		},
		{
			// Defense in depth: PUT-time validation (regexp.Compile) should
			// normally reject a bad pattern before it ever reaches a check,
			// but checkHTTP must still fail cleanly, not panic, if one gets
			// through.
			name:        "malformed regex at check time surfaces a clear error",
			body:        `anything`,
			bodyPattern: `(unclosed`,
			wantStatus:  "down",
			wantMsg:     "invalid body_pattern regex: error parsing regexp: missing closing ): `(unclosed`",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := fixtureServer(t, http.StatusOK, tc.body)

			status, msg, certExpiry := checkHTTP(srv.Client(), srv.URL, tc.bodyPattern, "", "")
			if status != tc.wantStatus {
				t.Errorf("status = %q, want %q (msg=%q)", status, tc.wantStatus, msg)
			}
			if tc.wantMsg != "" && msg != tc.wantMsg {
				t.Errorf("message = %q, want %q", msg, tc.wantMsg)
			}
			if certExpiry != nil {
				t.Errorf("certExpiry = %v, want nil for a plain http fixture server", certExpiry)
			}
		})
	}
}

func TestCheckHTTPJSONPathMatching(t *testing.T) {
	cases := []struct {
		name       string
		body       string
		jsonPath   string
		jsonExpect string
		wantStatus string
		wantMsg    string
	}{
		{
			name:       "json path present and equal",
			body:       `{"status":"healthy"}`,
			jsonPath:   "status",
			jsonExpect: "healthy",
			wantStatus: "up",
		},
		{
			name:       "json path present and different",
			body:       `{"status":"degraded"}`,
			jsonPath:   "status",
			jsonExpect: "healthy",
			wantStatus: "down",
			wantMsg:    "json path status = degraded, want healthy",
		},
		{
			name:       "json path missing entirely",
			body:       `{"status":"healthy"}`,
			jsonPath:   "nested.nope",
			jsonExpect: "healthy",
			wantStatus: "down",
			// gjson degrades a missing key to a non-match: empty string, no error.
			wantMsg: "json path nested.nope = , want healthy",
		},
		{
			name:       "malformed JSON body",
			body:       `not json at all`,
			jsonPath:   "status",
			jsonExpect: "healthy",
			wantStatus: "down",
			wantMsg:    "json path status = , want healthy",
		},
		{
			// Nested dotted path, per gjson's syntax — exercised alongside the
			// flat-key cases above rather than as its own test.
			name:       "nested dotted json path present and equal",
			body:       `{"data":{"status":"healthy"}}`,
			jsonPath:   "data.status",
			jsonExpect: "healthy",
			wantStatus: "up",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := fixtureServer(t, http.StatusOK, tc.body)

			status, msg, _ := checkHTTP(srv.Client(), srv.URL, "", tc.jsonPath, tc.jsonExpect)
			if status != tc.wantStatus {
				t.Errorf("status = %q, want %q (msg=%q)", status, tc.wantStatus, msg)
			}
			if tc.wantMsg != "" && msg != tc.wantMsg {
				t.Errorf("message = %q, want %q", msg, tc.wantMsg)
			}
		})
	}
}

// TestHandlePutServiceMonitorRejectsInvalidBodyPatternRegex is the API-layer
// half of "malformed regex should not panic and should surface a clear
// error": PUT-time validation must reject it with 400 before it is ever
// persisted or scheduled, rather than only failing later at check time.
func TestHandlePutServiceMonitorRejectsInvalidBodyPatternRegex(t *testing.T) {
	db := newTestDB(t)
	sched := newTestScheduler(db)

	const svc = "bad-regex-probe"
	body, _ := json.Marshal(map[string]any{
		"monitor_type":     "http",
		"target":           "https://example.com",
		"interval_seconds": 30,
		"body_pattern":     "(unclosed",
	})
	req := httptest.NewRequest(http.MethodPut, "/api/services/"+svc+"/monitor", bytes.NewReader(body))
	req = mux.SetURLVars(req, map[string]string{"name": svc})
	rec := httptest.NewRecorder()
	handlePutServiceMonitor(db, sched)(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %s)", rec.Code, rec.Body.String())
	}
	var resp map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response is not JSON: %q", rec.Body.String())
	}
	if !strings.Contains(resp["error"], "invalid body_pattern regex:") {
		t.Errorf("error = %q, want it to mention invalid body_pattern regex", resp["error"])
	}

	// A rejected config must not be persisted — otherwise the scheduler would
	// pick up a monitor whose regex is known-bad on the next process restart.
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM active_monitors WHERE service_name = ?`, svc).Scan(&n); err != nil {
		t.Fatalf("count active_monitors: %v", err)
	}
	if n != 0 {
		t.Errorf("active_monitors has %d row(s) for a config that failed validation, want 0", n)
	}
}

// ---------------------------------------------------------------------------
// End-to-end through checkHTTP: status code, body pattern and JSON path
// combined, in the order they are actually evaluated.
// ---------------------------------------------------------------------------

func TestCheckHTTPEndToEndCombinedChecks(t *testing.T) {
	const goodBody = `{"status":"healthy"}`

	t.Run("status ok + body match + json match is up", func(t *testing.T) {
		srv := fixtureServer(t, http.StatusOK, goodBody)
		status, msg, _ := checkHTTP(srv.Client(), srv.URL, "healthy", "status", "healthy")
		if status != "up" {
			t.Fatalf("status = %q, want up (msg=%q)", status, msg)
		}
		if !strings.Contains(msg, "HTTP 200") {
			t.Errorf("message = %q, want it to report the status code on success", msg)
		}
	})

	t.Run("bad status code fails and short-circuits before body/json checks", func(t *testing.T) {
		// The body would satisfy neither bodyPattern nor jsonExpect, but the
		// status-code check must be the one that reports the failure since it
		// runs first and stops the rest.
		srv := fixtureServer(t, http.StatusServiceUnavailable, `not json, does not match anything`)
		status, msg, _ := checkHTTP(srv.Client(), srv.URL, "healthy", "status", "healthy")
		if status != "down" {
			t.Fatalf("status = %q, want down", status)
		}
		if msg != "HTTP 503" {
			t.Errorf("message = %q, want %q", msg, "HTTP 503")
		}
	})

	t.Run("body pattern mismatch fails even with good status and json", func(t *testing.T) {
		srv := fixtureServer(t, http.StatusOK, goodBody)
		status, msg, _ := checkHTTP(srv.Client(), srv.URL, "this-will-not-match", "status", "healthy")
		if status != "down" {
			t.Fatalf("status = %q, want down", status)
		}
		if msg != "body pattern mismatch" {
			t.Errorf("message = %q, want %q", msg, "body pattern mismatch")
		}
	})

	t.Run("json path mismatch fails even with good status and body", func(t *testing.T) {
		srv := fixtureServer(t, http.StatusOK, goodBody)
		status, msg, _ := checkHTTP(srv.Client(), srv.URL, "healthy", "status", "degraded")
		if status != "down" {
			t.Fatalf("status = %q, want down", status)
		}
		want := "json path status = healthy, want degraded"
		if msg != want {
			t.Errorf("message = %q, want %q", msg, want)
		}
	})
}

// ---------------------------------------------------------------------------
// DB round trip: PUT a monitor config with body_pattern/json_path/json_expect
// set, then read it back through the GET handlers.
// ---------------------------------------------------------------------------

func TestServiceMonitorBodyJSONFieldsRoundTrip(t *testing.T) {
	db := newTestDB(t)
	sched := newTestScheduler(db)

	const svc = "roundtrip-probe"
	// Seed a minimal existing service, matching the DB-seeding style used
	// elsewhere for monitor-related handler tests (lantern_test.go).
	if _, err := db.Exec(`INSERT INTO status_events (service_name, status, message, timestamp) VALUES (?,?,?,?)`,
		svc, "up", "seed", "2026-08-26T00:00:00Z"); err != nil {
		t.Fatalf("seed status_events: %v", err)
	}

	const wantBodyPattern = `"status":"healthy"`
	const wantJSONPath = "status"
	const wantJSONExpect = "healthy"

	putBody, _ := json.Marshal(map[string]any{
		"monitor_type":     "http",
		"target":           "https://example.com/health",
		"interval_seconds": 30,
		"body_pattern":     wantBodyPattern,
		"json_path":        wantJSONPath,
		"json_expect":      wantJSONExpect,
	})
	putReq := httptest.NewRequest(http.MethodPut, "/api/services/"+svc+"/monitor", bytes.NewReader(putBody))
	putReq = mux.SetURLVars(putReq, map[string]string{"name": svc})
	putRec := httptest.NewRecorder()
	handlePutServiceMonitor(db, sched)(putRec, putReq)
	if putRec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200 (body %s)", putRec.Code, putRec.Body.String())
	}

	assertRoundTripped := func(t *testing.T, m ActiveMonitor) {
		t.Helper()
		if m.BodyPattern == nil || *m.BodyPattern != wantBodyPattern {
			t.Errorf("BodyPattern = %s, want %q", strOrNil(m.BodyPattern), wantBodyPattern)
		}
		if m.JSONPath == nil || *m.JSONPath != wantJSONPath {
			t.Errorf("JSONPath = %s, want %q", strOrNil(m.JSONPath), wantJSONPath)
		}
		if m.JSONExpect == nil || *m.JSONExpect != wantJSONExpect {
			t.Errorf("JSONExpect = %s, want %q", strOrNil(m.JSONExpect), wantJSONExpect)
		}
	}

	// The PUT response itself already reflects what was written.
	var putResp ActiveMonitor
	if err := json.Unmarshal(putRec.Body.Bytes(), &putResp); err != nil {
		t.Fatalf("decode PUT response: %v", err)
	}
	assertRoundTripped(t, putResp)

	t.Run("GET /api/services/{name}/monitor", func(t *testing.T) {
		getReq := httptest.NewRequest(http.MethodGet, "/api/services/"+svc+"/monitor", nil)
		getReq = mux.SetURLVars(getReq, map[string]string{"name": svc})
		getRec := httptest.NewRecorder()
		handleGetServiceMonitor(db)(getRec, getReq)
		if getRec.Code != http.StatusOK {
			t.Fatalf("GET status = %d, want 200 (body %s)", getRec.Code, getRec.Body.String())
		}

		var got ActiveMonitor
		if err := json.Unmarshal(getRec.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode GET response: %v", err)
		}
		assertRoundTripped(t, got)
	})

	t.Run("GET /api/monitors", func(t *testing.T) {
		listReq := httptest.NewRequest(http.MethodGet, "/api/monitors", nil)
		listRec := httptest.NewRecorder()
		handleGetMonitors(db)(listRec, listReq)
		if listRec.Code != http.StatusOK {
			t.Fatalf("GET /api/monitors status = %d, want 200 (body %s)", listRec.Code, listRec.Body.String())
		}

		var list []ActiveMonitor
		if err := json.Unmarshal(listRec.Body.Bytes(), &list); err != nil {
			t.Fatalf("decode monitors list: %v", err)
		}
		var found *ActiveMonitor
		for i := range list {
			if list[i].ServiceName == svc {
				found = &list[i]
			}
		}
		if found == nil {
			t.Fatalf("service %q not present in GET /api/monitors", svc)
		}
		assertRoundTripped(t, *found)
	})
}

// ---------------------------------------------------------------------------
// Scoped-token authorization: a token scoped to one service must not be able
// to reach into another service's monitor config. handlePutServiceAlerts and
// handlePutServiceGroup already enforce this (r.Context().Value(scopedServiceKey)
// must match the path's {name}); handlePutServiceMonitor/handleDeleteServiceMonitor
// previously had no such check, so a token scoped to "webapp" could PUT or
// DELETE any other service's monitor entirely.
// ---------------------------------------------------------------------------

func TestHandlePutServiceMonitorRejectsCrossServiceScopedToken(t *testing.T) {
	db := newTestDB(t)
	sched := newTestScheduler(db)

	const victim = "victim-service"
	body, _ := json.Marshal(map[string]any{
		"monitor_type":     "http",
		"target":           "http://internal.attacker.example/",
		"interval_seconds": 30,
	})
	req := httptest.NewRequest(http.MethodPut, "/api/services/"+victim+"/monitor", bytes.NewReader(body))
	req = mux.SetURLVars(req, map[string]string{"name": victim})
	req = req.WithContext(context.WithValue(req.Context(), scopedServiceKey, "attacker-service"))
	rec := httptest.NewRecorder()
	handlePutServiceMonitor(db, sched)(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (body %s)", rec.Code, rec.Body.String())
	}

	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM active_monitors WHERE service_name = ?`, victim).Scan(&n); err != nil {
		t.Fatalf("count active_monitors: %v", err)
	}
	if n != 0 {
		t.Errorf("active_monitors has %d row(s) for %q after a rejected cross-service PUT, want 0", n, victim)
	}
}

func TestHandlePutServiceMonitorAllowsMatchingScopedToken(t *testing.T) {
	db := newTestDB(t)
	sched := newTestScheduler(db)

	const svc = "webapp"
	body, _ := json.Marshal(map[string]any{
		"monitor_type":     "http",
		"target":           "https://example.com",
		"interval_seconds": 30,
	})
	req := httptest.NewRequest(http.MethodPut, "/api/services/"+svc+"/monitor", bytes.NewReader(body))
	req = mux.SetURLVars(req, map[string]string{"name": svc})
	req = req.WithContext(context.WithValue(req.Context(), scopedServiceKey, svc))
	rec := httptest.NewRecorder()
	handlePutServiceMonitor(db, sched)(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 for a token scoped to its own service (body %s)", rec.Code, rec.Body.String())
	}
}

func TestHandleDeleteServiceMonitorRejectsCrossServiceScopedToken(t *testing.T) {
	db := newTestDB(t)
	sched := newTestScheduler(db)

	const victim = "victim-service"
	if _, err := db.Exec(`INSERT INTO active_monitors (service_name, monitor_type, target, interval_seconds, enabled) VALUES (?,?,?,?,1)`,
		victim, "http", "https://example.com", 30); err != nil {
		t.Fatalf("seed active_monitors: %v", err)
	}

	req := httptest.NewRequest(http.MethodDelete, "/api/services/"+victim+"/monitor", nil)
	req = mux.SetURLVars(req, map[string]string{"name": victim})
	req = req.WithContext(context.WithValue(req.Context(), scopedServiceKey, "attacker-service"))
	rec := httptest.NewRecorder()
	handleDeleteServiceMonitor(db, sched)(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (body %s)", rec.Code, rec.Body.String())
	}

	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM active_monitors WHERE service_name = ?`, victim).Scan(&n); err != nil {
		t.Fatalf("count active_monitors: %v", err)
	}
	if n != 1 {
		t.Errorf("active_monitors has %d row(s) for %q after a rejected cross-service DELETE, want 1 (still present)", n, victim)
	}
}

// ---------------------------------------------------------------------------
// body_pattern compile caching: checkHTTP must not recompile the same pattern
// on every call (it's invoked once per monitor per interval_seconds, so an
// uncached regexp.Compile is repeated work on a hot path).
// ---------------------------------------------------------------------------

func TestCompiledBodyPatternIsCached(t *testing.T) {
	const pattern = `"status":"healthy"`

	re1, err := compiledBodyPattern(pattern)
	if err != nil {
		t.Fatalf("compiledBodyPattern: %v", err)
	}
	re2, err := compiledBodyPattern(pattern)
	if err != nil {
		t.Fatalf("compiledBodyPattern (second call): %v", err)
	}
	if re1 != re2 {
		t.Errorf("compiledBodyPattern returned a different *regexp.Regexp for the same pattern, want the cached instance")
	}

	// A different pattern must still compile correctly and independently —
	// caching by pattern string must not leak state between distinct patterns.
	other, err := compiledBodyPattern(`"status":"degraded"`)
	if err != nil {
		t.Fatalf("compiledBodyPattern (other pattern): %v", err)
	}
	if other == re1 {
		t.Errorf("compiledBodyPattern returned the same instance for two different patterns")
	}
	if !other.MatchString(`{"status":"degraded"}`) {
		t.Errorf("cached entry for a different pattern doesn't match its own pattern")
	}

	if _, err := compiledBodyPattern(`(unclosed`); err == nil {
		t.Errorf("compiledBodyPattern accepted a malformed regex, want an error")
	}
}

// strOrNil renders a *string for test failure messages without dereferencing
// a nil pointer.
func strOrNil(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}
