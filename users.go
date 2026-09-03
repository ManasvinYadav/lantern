package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/mux"
	"golang.org/x/crypto/bcrypt"
)

// ---------------------------------------------------------------------------
// Users & roles
// ---------------------------------------------------------------------------
//
// Lantern used to hold exactly one operator, in the admin_credentials
// singleton row. `users` replaces it: many accounts, each with one of three
// roles. The old table is migrated once at startup and then left alone — see
// migrateLegacyCredentials.
//
// Roles are coarse on purpose. Per-service access for a *person* is not a
// thing Lantern needs; per-service access for a *machine* already exists as
// scoped API tokens, which are an orthogonal axis and are unchanged by any of
// this. Three levels cover the real cases: someone who runs the install,
// someone who operates it, and someone who is only allowed to look.
//
//	owner   everything, including managing other users
//	admin   everything except managing other users
//	viewer  read-only, and blind to the routes that carry installation-wide
//	        secrets (backup, webhook URLs, config export, the audit log)

const (
	roleOwner  = "owner"
	roleAdmin  = "admin"
	roleViewer = "viewer"
)

const usersSchema = `
CREATE TABLE IF NOT EXISTS users (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    username      TEXT    NOT NULL UNIQUE COLLATE NOCASE,
    password_hash TEXT    NOT NULL,
    role          TEXT    NOT NULL DEFAULT 'admin',
    disabled      INTEGER NOT NULL DEFAULT 0,
    created_at    INTEGER NOT NULL,
    updated_at    INTEGER NOT NULL
);
`

// bcryptDummyHash is compared against when a username does not exist, so a
// wrong username costs the same as a wrong password. Without it the lookup
// returns immediately and the response time leaks which accounts are real.
// (Hash of a value nothing can present: bcrypt of 32 random bytes, cost 10.)
const bcryptDummyHash = "$2a$10$N9qo8uLOickgx2ZMRZoMyeIjZAgcfl7p92ldGxad68LJZdL17lhWy"

func roleRank(role string) int {
	switch role {
	case roleOwner:
		return 3
	case roleAdmin:
		return 2
	case roleViewer:
		return 1
	}
	return 0
}

func roleValid(role string) bool { return roleRank(role) > 0 }

// isOwnerOnlyEndpoint reports whether a route may only be reached by an owner.
// Only user management qualifies: an admin configures the installation, an
// owner decides who else gets to. Note that changing your *own* password
// (/api/auth/credentials) is not here — every role may do that.
func isOwnerOnlyEndpoint(r *http.Request) bool {
	return r.URL.Path == "/api/admin/users" || strings.HasPrefix(r.URL.Path, "/api/admin/users/")
}

// roleAllows is the role gate, applied in authMiddleware after a credential
// has been recognised. It answers "may this role make this request at all",
// separately from "is this caller authenticated".
func roleAllows(role string, r *http.Request) bool {
	switch role {
	case roleOwner:
		return true
	case roleAdmin:
		return !isOwnerOnlyEndpoint(r)
	case roleViewer:
		// Read-only, and not even read on the routes that hand back
		// installation-wide secrets — isAdminOnlyEndpoint is already exactly
		// that set (backup, webhook URLs, config export/import, audit log,
		// credentials), which is why it is reused here rather than restated.
		return r.Method == http.MethodGet && !isAdminOnlyEndpoint(r)
	}
	return false
}

// ---------------------------------------------------------------------------
// Storage
// ---------------------------------------------------------------------------

// User is one row of `users`. The password hash is deliberately not a field:
// nothing outside this file needs it, and a struct that carries it is a struct
// that eventually gets serialised into a response.
type User struct {
	Username  string `json:"username"`
	Role      string `json:"role"`
	Disabled  bool   `json:"disabled"`
	CreatedAt int64  `json:"created_at"`
	UpdatedAt int64  `json:"updated_at"`
}

var errLastOwner = errors.New("the last enabled owner cannot be removed, disabled or demoted")

func listUsers(db *sql.DB) ([]User, error) {
	rows, err := db.Query(`SELECT username, role, disabled, created_at, updated_at
	                       FROM users ORDER BY username COLLATE NOCASE`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []User{}
	for rows.Next() {
		var u User
		var disabled int
		if err := rows.Scan(&u.Username, &u.Role, &disabled, &u.CreatedAt, &u.UpdatedAt); err != nil {
			return nil, err
		}
		u.Disabled = disabled != 0
		out = append(out, u)
	}
	return out, rows.Err()
}

// lookupUserAuth returns the stored hash and role for a username. found is
// false for an unknown *or disabled* account: a disabled user must not be able
// to sign in, and must not keep an existing session alive either.
func lookupUserAuth(db *sql.DB, username string) (hash, role string, found bool) {
	err := db.QueryRow(`SELECT password_hash, role FROM users
	                    WHERE username = ? COLLATE NOCASE AND disabled = 0`, username).
		Scan(&hash, &role)
	if err != nil {
		return "", "", false
	}
	return hash, role, true
}

func countEnabledOwners(db *sql.DB, excluding string) int {
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM users
	                       WHERE role = ? AND disabled = 0 AND username <> ? COLLATE NOCASE`,
		roleOwner, excluding).Scan(&n); err != nil {
		return 0
	}
	return n
}

// migrateLegacyCredentials copies the pre-v0.70 admin_credentials singleton
// into `users` as the owner, once. admin_credentials is intentionally left in
// place rather than dropped: a downgrade to v0.69 still finds its credentials
// where it expects them, and the row is small.
func migrateLegacyCredentials(db *sql.DB) {
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n); err != nil || n > 0 {
		return
	}
	var username, hash string
	err := db.QueryRow(`SELECT username, password_hash FROM admin_credentials WHERE id = 1`).
		Scan(&username, &hash)
	if err != nil {
		return // no legacy row: a fresh install, nothing to carry over
	}
	now := time.Now().Unix()
	if _, err := db.Exec(`INSERT INTO users (username, password_hash, role, disabled, created_at, updated_at)
	                      VALUES (?, ?, ?, 0, ?, ?)`, username, hash, roleOwner, now, now); err != nil {
		log.Printf("users: could not migrate legacy credentials: %v", err)
		return
	}
	log.Printf("users: migrated existing operator %q to the users table as owner", username)
}

// ---------------------------------------------------------------------------
// Handlers — all owner-only, gated in authMiddleware via isOwnerOnlyEndpoint
// ---------------------------------------------------------------------------

type userRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
	Role     string `json:"role"`
	Disabled *bool  `json:"disabled"`
}

// requireLoginGate refuses account management on an install that has no
// credentials at all. Such an install is wide open by design, so without this
// any passer-by could POST themselves an owner account and lock the real
// operator out — a takeover dressed up as first-time setup. Setting up the
// first account goes through PUT /api/auth/credentials, which is the one
// route that is allowed to mint an identity out of nothing and has its own
// guard for token-mode deployments.
func requireLoginGate(w http.ResponseWriter) bool {
	if !authRequired() {
		writeError(w, http.StatusBadRequest,
			"Set up sign-in first (Settings \u2192 Account & Security). Accounts cannot be managed on an install with no credentials.")
		return false
	}
	return true
}

func handleGetUsers(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireLoginGate(w) {
			return
		}
		users, err := listUsers(db)
		if err != nil {
			log.Printf("handleGetUsers: %v", err)
			writeError(w, http.StatusInternalServerError, "database error")
			return
		}
		writeJSON(w, http.StatusOK, users)
	}
}

func handlePostUser(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireLoginGate(w) {
			return
		}
		var req userRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "Malformed request body.")
			return
		}
		req.Username = strings.TrimSpace(req.Username)
		if req.Username == "" {
			writeError(w, http.StatusBadRequest, "A username is required.")
			return
		}
		if len(req.Username) > 64 {
			writeError(w, http.StatusBadRequest, "Username must be 64 characters or fewer.")
			return
		}
		if len(req.Password) < 8 {
			writeError(w, http.StatusBadRequest, "Password must be at least 8 characters.")
			return
		}
		if !roleValid(req.Role) {
			writeError(w, http.StatusBadRequest, "Role must be owner, admin or viewer.")
			return
		}

		hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcryptCost)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "Could not create the account.")
			return
		}
		now := time.Now().Unix()
		_, err = db.Exec(`INSERT INTO users (username, password_hash, role, disabled, created_at, updated_at)
		                  VALUES (?, ?, ?, 0, ?, ?)`, req.Username, string(hash), req.Role, now, now)
		if err != nil {
			if strings.Contains(err.Error(), "UNIQUE") {
				writeError(w, http.StatusConflict, "That username is already taken.")
				return
			}
			log.Printf("handlePostUser: %v", err)
			writeError(w, http.StatusInternalServerError, "Could not create the account.")
			return
		}
		recordAudit(db, r, "user_create", req.Username, true, "role="+req.Role)
		refreshCredentialState(db)
		writeJSON(w, http.StatusCreated, User{Username: req.Username, Role: req.Role, CreatedAt: now, UpdatedAt: now})
	}
}

// handlePutUser changes a user's role, enabled state, or password. Any field
// left out is left alone.
func handlePutUser(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireLoginGate(w) {
			return
		}
		target := mux.Vars(r)["username"]
		var req userRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "Malformed request body.")
			return
		}
		if req.Role != "" && !roleValid(req.Role) {
			writeError(w, http.StatusBadRequest, "Role must be owner, admin or viewer.")
			return
		}
		if req.Password != "" && len(req.Password) < 8 {
			writeError(w, http.StatusBadRequest, "Password must be at least 8 characters.")
			return
		}

		var curRole string
		var curDisabled int
		if err := db.QueryRow(`SELECT role, disabled FROM users WHERE username = ? COLLATE NOCASE`, target).
			Scan(&curRole, &curDisabled); err != nil {
			writeError(w, http.StatusNotFound, "No such user.")
			return
		}

		newRole := curRole
		if req.Role != "" {
			newRole = req.Role
		}
		newDisabled := curDisabled
		if req.Disabled != nil {
			newDisabled = 0
			if *req.Disabled {
				newDisabled = 1
			}
		}

		// Losing the last owner locks everyone out of user management with no
		// way back in short of editing the database by hand.
		losesOwner := curRole == roleOwner && curDisabled == 0 &&
			(newRole != roleOwner || newDisabled == 1)
		if losesOwner && countEnabledOwners(db, target) == 0 {
			writeError(w, http.StatusConflict, errLastOwner.Error())
			return
		}

		now := time.Now().Unix()
		if req.Password != "" {
			hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcryptCost)
			if err != nil {
				writeError(w, http.StatusInternalServerError, "Could not update the account.")
				return
			}
			_, err = db.Exec(`UPDATE users SET password_hash = ?, role = ?, disabled = ?, updated_at = ?
			                  WHERE username = ? COLLATE NOCASE`,
				string(hash), newRole, newDisabled, now, target)
			if err != nil {
				log.Printf("handlePutUser: %v", err)
				writeError(w, http.StatusInternalServerError, "Could not update the account.")
				return
			}
			// A password reset by someone else must not leave the old
			// sessions usable.
			revokeUserSessions(db, target)
		} else {
			if _, err := db.Exec(`UPDATE users SET role = ?, disabled = ?, updated_at = ?
			                      WHERE username = ? COLLATE NOCASE`,
				newRole, newDisabled, now, target); err != nil {
				log.Printf("handlePutUser: %v", err)
				writeError(w, http.StatusInternalServerError, "Could not update the account.")
				return
			}
		}
		if newDisabled == 1 {
			revokeUserSessions(db, target)
		}

		detail := "role=" + newRole
		if newDisabled != curDisabled {
			detail += " disabled=" + boolStr(newDisabled == 1)
		}
		if req.Password != "" {
			detail += " password reset"
		}
		recordAudit(db, r, "user_update", target, true, detail)
		refreshCredentialState(db)
		writeJSON(w, http.StatusOK, User{Username: target, Role: newRole, Disabled: newDisabled == 1, UpdatedAt: now})
	}
}

func handleDeleteUser(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireLoginGate(w) {
			return
		}
		target := mux.Vars(r)["username"]

		// Deleting yourself ends your own session mid-request and, if you were
		// the only owner, locks the install. Refuse rather than surprise.
		if actor, ok := r.Context().Value(sessionUserKey).(string); ok &&
			strings.EqualFold(actor, target) {
			writeError(w, http.StatusConflict, "You cannot delete the account you are signed in as.")
			return
		}

		var curRole string
		var curDisabled int
		if err := db.QueryRow(`SELECT role, disabled FROM users WHERE username = ? COLLATE NOCASE`, target).
			Scan(&curRole, &curDisabled); err != nil {
			writeError(w, http.StatusNotFound, "No such user.")
			return
		}
		if curRole == roleOwner && curDisabled == 0 && countEnabledOwners(db, target) == 0 {
			writeError(w, http.StatusConflict, errLastOwner.Error())
			return
		}

		if _, err := db.Exec(`DELETE FROM users WHERE username = ? COLLATE NOCASE`, target); err != nil {
			log.Printf("handleDeleteUser: %v", err)
			writeError(w, http.StatusInternalServerError, "Could not delete the account.")
			return
		}
		revokeUserSessions(db, target)
		recordAudit(db, r, "user_delete", target, true, "role="+curRole)
		refreshCredentialState(db)
		writeJSON(w, http.StatusOK, map[string]any{"deleted": true, "username": target})
	}
}

func revokeUserSessions(db *sql.DB, username string) {
	if _, err := db.Exec(`DELETE FROM sessions WHERE username = ? COLLATE NOCASE`, username); err != nil {
		log.Printf("users: could not revoke sessions for %q: %v", username, err)
	}
}
