package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gorilla/mux"
)

// seedUser inserts an account directly, so a test can set up a cast without
// going through the owner-only API first.
func seedUser(t *testing.T, db *sql.DB, username, pass, role string) {
	t.Helper()
	if err := writeCredentials(db, username, pass); err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, `UPDATE users SET role = ? WHERE username = ?`, role, username)
	refreshCredentialState(db)
}

// asRole runs one request through the middleware carrying a real session for
// a user of the given role, and returns the status code the terminal handler
// (or the role gate) produced.
func asRole(t *testing.T, db *sql.DB, cfg *Config, role, method, path string) int {
	t.Helper()
	user := "u-" + role
	seedUser(t, db, user, "correct-horse", role)
	cookie := loginFor(t, db, user, "correct-horse")

	req := httptest.NewRequest(method, path, bytes.NewReader([]byte(`{}`)))
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	authMiddleware(db, cfg, okHandler()).ServeHTTP(rec, req)
	return rec.Code
}

func TestViewerIsReadOnly(t *testing.T) {
	db, cfg := newAuthDB(t, "owner", "correct-horse")

	// Reads a viewer is meant to have.
	for _, path := range []string{"/api/services", "/api/groups", "/api/incidents"} {
		if code := asRole(t, db, cfg, roleViewer, http.MethodGet, path); code != http.StatusOK {
			t.Errorf("viewer GET %s = %d, want 200", path, code)
		}
	}

	// Every mutation is refused, whatever the route.
	for _, c := range []struct{ method, path string }{
		{http.MethodPost, "/api/status"},
		{http.MethodDelete, "/api/services/webapp"},
		{http.MethodPut, "/api/services/webapp/maintenance"},
		{http.MethodPut, "/api/branding"},
		{http.MethodPut, "/api/notifications/schedule"},
		{http.MethodPost, "/api/services/webapp/check"},
	} {
		if code := asRole(t, db, cfg, roleViewer, c.method, c.path); code != http.StatusForbidden {
			t.Errorf("viewer %s %s = %d, want 403", c.method, c.path, code)
		}
	}

	// ...and so are the reads that hand back installation-wide secrets. A
	// viewer who could GET /api/backup would have every password hash.
	for _, path := range []string{"/api/backup", "/api/webhooks", "/api/config/export", "/api/admin/audit-log"} {
		if code := asRole(t, db, cfg, roleViewer, http.MethodGet, path); code != http.StatusForbidden {
			t.Errorf("viewer GET %s = %d, want 403", path, code)
		}
	}
}

func TestAdminCanConfigureButNotManageUsers(t *testing.T) {
	db, cfg := newAuthDB(t, "owner", "correct-horse")

	for _, c := range []struct{ method, path string }{
		{http.MethodGet, "/api/backup"},
		{http.MethodGet, "/api/webhooks"},
		{http.MethodPut, "/api/branding"},
		{http.MethodGet, "/api/admin/audit-log"},
		{http.MethodDelete, "/api/services/webapp"},
	} {
		if code := asRole(t, db, cfg, roleAdmin, c.method, c.path); code != http.StatusOK {
			t.Errorf("admin %s %s = %d, want 200", c.method, c.path, code)
		}
	}

	for _, c := range []struct{ method, path string }{
		{http.MethodGet, "/api/admin/users"},
		{http.MethodPost, "/api/admin/users"},
		{http.MethodPut, "/api/admin/users/someone"},
		{http.MethodDelete, "/api/admin/users/someone"},
	} {
		if code := asRole(t, db, cfg, roleAdmin, c.method, c.path); code != http.StatusForbidden {
			t.Errorf("admin %s %s = %d, want 403", c.method, c.path, code)
		}
	}
}

func TestOwnerCanManageUsers(t *testing.T) {
	db, cfg := newAuthDB(t, "boss", "correct-horse")
	if code := asRole(t, db, cfg, roleOwner, http.MethodGet, "/api/admin/users"); code != http.StatusOK {
		t.Errorf("owner GET /api/admin/users = %d, want 200", code)
	}
}

// The LANTERN_AUTH_TOKEN bearer credential runs the installation but must not
// be able to mint accounts on it — it is a static string in an env var.
func TestAdminApiTokenCannotManageUsers(t *testing.T) {
	db, cfg := newAuthDB(t, "boss", "correct-horse")
	cfg.AuthToken = "env-admin-token"

	req := httptest.NewRequest(http.MethodPost, "/api/admin/users", bytes.NewReader([]byte(`{}`)))
	req.Header.Set("Authorization", "Bearer env-admin-token")
	rec := httptest.NewRecorder()
	authMiddleware(db, cfg, okHandler()).ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("admin token POST /api/admin/users = %d, want 403", rec.Code)
	}

	// It still runs everything else.
	req = httptest.NewRequest(http.MethodGet, "/api/backup", nil)
	req.Header.Set("Authorization", "Bearer env-admin-token")
	rec = httptest.NewRecorder()
	authMiddleware(db, cfg, okHandler()).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin token GET /api/backup = %d, want 200", rec.Code)
	}
}

func TestLegacyCredentialsMigrateToOwner(t *testing.T) {
	db := newTestDB(t)
	// A pre-v0.70 install: the singleton row exists, `users` is empty.
	mustExec(t, db, `DELETE FROM users`)
	mustExec(t, db, `INSERT INTO admin_credentials (id, username, password_hash, updated_at)
	                 VALUES (1, 'legacy-admin', '$2a$10$abcdefghijklmnopqrstuv', 0)`)

	migrateLegacyCredentials(db)

	var role string
	var hash string
	if err := db.QueryRow(`SELECT role, password_hash FROM users WHERE username = 'legacy-admin'`).
		Scan(&role, &hash); err != nil {
		t.Fatalf("legacy operator was not migrated: %v", err)
	}
	if role != roleOwner {
		t.Errorf("migrated role = %q, want owner", role)
	}
	if hash != "$2a$10$abcdefghijklmnopqrstuv" {
		t.Error("migration re-hashed the password instead of carrying the digest across")
	}

	// Idempotent: a second boot must not duplicate or overwrite.
	mustExec(t, db, `UPDATE users SET role = 'viewer' WHERE username = 'legacy-admin'`)
	migrateLegacyCredentials(db)
	var n int
	_ = db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n)
	if n != 1 {
		t.Errorf("users after a second migration = %d, want 1", n)
	}
	_ = db.QueryRow(`SELECT COUNT(*) FROM users WHERE role = 'viewer'`).Scan(&n)
	if n != 1 {
		t.Error("a second migration overwrote a role the operator had since changed")
	}
}

// ---------------------------------------------------------------------------
// Guardrails
// ---------------------------------------------------------------------------

func userReq(t *testing.T, h http.HandlerFunc, method, target, username string, body any, actor string) *httptest.ResponseRecorder {
	t.Helper()
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(method, target, bytes.NewReader(raw))
	if username != "" {
		req = mux.SetURLVars(req, map[string]string{"username": username})
	}
	if actor != "" {
		req = req.WithContext(context.WithValue(req.Context(), sessionUserKey, actor))
	}
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

func TestLastOwnerCannotBeRemovedDisabledOrDemoted(t *testing.T) {
	db, _ := newAuthDB(t, "boss", "correct-horse")
	seedUser(t, db, "helper", "correct-horse", roleAdmin)

	// Demote.
	rec := userReq(t, handlePutUser(db), http.MethodPut, "/api/admin/users/boss", "boss",
		map[string]any{"role": roleAdmin}, "boss")
	if rec.Code != http.StatusConflict {
		t.Errorf("demoting the last owner = %d, want 409", rec.Code)
	}
	// Disable.
	rec = userReq(t, handlePutUser(db), http.MethodPut, "/api/admin/users/boss", "boss",
		map[string]any{"disabled": true}, "boss")
	if rec.Code != http.StatusConflict {
		t.Errorf("disabling the last owner = %d, want 409", rec.Code)
	}
	// Delete (by someone else, so the self-delete guard is not what fires).
	rec = userReq(t, handleDeleteUser(db), http.MethodDelete, "/api/admin/users/boss", "boss", nil, "helper")
	if rec.Code != http.StatusConflict {
		t.Errorf("deleting the last owner = %d, want 409", rec.Code)
	}

	var role string
	var disabled int
	if err := db.QueryRow(`SELECT role, disabled FROM users WHERE username = 'boss'`).Scan(&role, &disabled); err != nil {
		t.Fatal(err)
	}
	if role != roleOwner || disabled != 0 {
		t.Fatalf("owner survived as role=%q disabled=%d, want owner/0", role, disabled)
	}

	// With a second owner in place, the first may step down.
	seedUser(t, db, "boss2", "correct-horse", roleOwner)
	rec = userReq(t, handlePutUser(db), http.MethodPut, "/api/admin/users/boss", "boss",
		map[string]any{"role": roleAdmin}, "boss")
	if rec.Code != http.StatusOK {
		t.Errorf("demoting one of two owners = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
}

func TestCannotDeleteYourOwnAccount(t *testing.T) {
	db, _ := newAuthDB(t, "boss", "correct-horse")
	seedUser(t, db, "boss2", "correct-horse", roleOwner)
	rec := userReq(t, handleDeleteUser(db), http.MethodDelete, "/api/admin/users/boss", "boss", nil, "boss")
	if rec.Code != http.StatusConflict {
		t.Fatalf("self-delete = %d, want 409", rec.Code)
	}
}

func TestDisablingAUserKillsTheirSessionImmediately(t *testing.T) {
	db, cfg := newAuthDB(t, "boss", "correct-horse")
	seedUser(t, db, "helper", "correct-horse", roleAdmin)
	cookie := loginFor(t, db, "helper", "correct-horse")

	probe := func() int {
		req := httptest.NewRequest(http.MethodGet, "/api/services", nil)
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		authMiddleware(db, cfg, okHandler()).ServeHTTP(rec, req)
		return rec.Code
	}
	if probe() != http.StatusOK {
		t.Fatal("a freshly signed-in admin was rejected")
	}

	rec := userReq(t, handlePutUser(db), http.MethodPut, "/api/admin/users/helper", "helper",
		map[string]any{"disabled": true}, "boss")
	if rec.Code != http.StatusOK {
		t.Fatalf("disable = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	if got := probe(); got == http.StatusOK {
		t.Errorf("a disabled user's existing session still authenticates (%d)", got)
	}
}

// A demotion has to bind on the very next request, not at next sign-in —
// which is why the role is joined live rather than stamped onto the session.
func TestDemotionTakesEffectOnTheNextRequest(t *testing.T) {
	db, cfg := newAuthDB(t, "boss", "correct-horse")
	seedUser(t, db, "helper", "correct-horse", roleAdmin)
	cookie := loginFor(t, db, "helper", "correct-horse")

	probeDelete := func() int {
		req := httptest.NewRequest(http.MethodDelete, "/api/services/webapp", nil)
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		authMiddleware(db, cfg, okHandler()).ServeHTTP(rec, req)
		return rec.Code
	}
	if probeDelete() != http.StatusOK {
		t.Fatal("an admin could not delete a service")
	}

	rec := userReq(t, handlePutUser(db), http.MethodPut, "/api/admin/users/helper", "helper",
		map[string]any{"role": roleViewer}, "boss")
	if rec.Code != http.StatusOK {
		t.Fatalf("demote = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	if got := probeDelete(); got != http.StatusForbidden {
		t.Errorf("a demoted admin can still delete services (%d)", got)
	}
}

func TestCreateUserValidation(t *testing.T) {
	db, _ := newAuthDB(t, "boss", "correct-horse")
	cases := []struct {
		label string
		body  map[string]any
		want  int
	}{
		{"no username", map[string]any{"password": "correct-horse", "role": roleAdmin}, http.StatusBadRequest},
		{"short password", map[string]any{"username": "x", "password": "short", "role": roleAdmin}, http.StatusBadRequest},
		{"bad role", map[string]any{"username": "x", "password": "correct-horse", "role": "superuser"}, http.StatusBadRequest},
		{"no role", map[string]any{"username": "x", "password": "correct-horse"}, http.StatusBadRequest},
		{"valid", map[string]any{"username": "newbie", "password": "correct-horse", "role": roleViewer}, http.StatusCreated},
		{"duplicate", map[string]any{"username": "newbie", "password": "correct-horse", "role": roleViewer}, http.StatusConflict},
		{"duplicate, different case", map[string]any{"username": "NEWBIE", "password": "correct-horse", "role": roleViewer}, http.StatusConflict},
	}
	for _, c := range cases {
		t.Run(c.label, func(t *testing.T) {
			rec := userReq(t, handlePostUser(db), http.MethodPost, "/api/admin/users", "", c.body, "boss")
			if rec.Code != c.want {
				t.Fatalf("%s = %d, want %d (%s)", c.label, rec.Code, c.want, rec.Body.String())
			}
		})
	}

	// The created account signs in with the role it was given.
	if role, ok := verifyCredentials(db, "newbie", "correct-horse"); !ok || role != roleViewer {
		t.Fatalf("new account verifies as (%q, %v), want viewer/true", role, ok)
	}
}

func TestUserActionsAreAudited(t *testing.T) {
	db, _ := newAuthDB(t, "boss", "correct-horse")
	userReq(t, handlePostUser(db), http.MethodPost, "/api/admin/users", "",
		map[string]any{"username": "newbie", "password": "correct-horse", "role": roleViewer}, "boss")
	userReq(t, handlePutUser(db), http.MethodPut, "/api/admin/users/newbie", "newbie",
		map[string]any{"role": roleAdmin}, "boss")
	userReq(t, handleDeleteUser(db), http.MethodDelete, "/api/admin/users/newbie", "newbie", nil, "boss")

	rows, err := db.Query(`SELECT actor, action, target FROM admin_audit_log
	                       WHERE action LIKE 'user_%' ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var actor, action, target string
		if err := rows.Scan(&actor, &action, &target); err != nil {
			t.Fatal(err)
		}
		got = append(got, actor+" "+action+" "+target)
	}
	want := []string{"boss user_create newbie", "boss user_update newbie", "boss user_delete newbie"}
	if len(got) != len(want) {
		t.Fatalf("audit entries = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("audit entry %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestDeletingAUserRevokesTheirSessions(t *testing.T) {
	db, _ := newAuthDB(t, "boss", "correct-horse")
	seedUser(t, db, "helper", "correct-horse", roleAdmin)
	loginFor(t, db, "helper", "correct-horse")

	var n int
	_ = db.QueryRow(`SELECT COUNT(*) FROM sessions WHERE username = 'helper'`).Scan(&n)
	if n == 0 {
		t.Fatal("login left no session row to revoke")
	}
	rec := userReq(t, handleDeleteUser(db), http.MethodDelete, "/api/admin/users/helper", "helper", nil, "boss")
	if rec.Code != http.StatusOK {
		t.Fatalf("delete = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	_ = db.QueryRow(`SELECT COUNT(*) FROM sessions WHERE username = 'helper'`).Scan(&n)
	if n != 0 {
		t.Errorf("sessions surviving a user delete = %d, want 0", n)
	}
}

// A credential-free install is wide open by design. Account management must
// still be closed on it: otherwise anyone reaching the port could mint
// themselves an owner and lock the real operator out.
func TestUsersCannotBeManagedWithoutALoginGate(t *testing.T) {
	db := newTestDB(t)
	mustExec(t, db, `DELETE FROM users`)
	refreshCredentialState(db)
	if authRequired() {
		t.Fatal("gate is active on a database with no accounts")
	}

	for _, c := range []struct {
		label string
		h     http.HandlerFunc
		body  map[string]any
	}{
		{"list", handleGetUsers(db), nil},
		{"create", handlePostUser(db), map[string]any{"username": "intruder", "password": "correct-horse", "role": roleOwner}},
		{"update", handlePutUser(db), map[string]any{"role": roleOwner}},
		{"delete", handleDeleteUser(db), nil},
	} {
		rec := userReq(t, c.h, http.MethodPost, "/api/admin/users", "someone", c.body, "")
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s on an ungated install = %d, want 400", c.label, rec.Code)
		}
	}

	var n int
	_ = db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n)
	if n != 0 {
		t.Errorf("an ungated install gained %d account(s)", n)
	}
}
