// Package handlers wires HTTP handlers against a *store.Store.
package handlers

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"example.com/userservice/internal/store"
)

// NewUsersList handles GET /users — returns the full user list.
func NewUsersList(db *store.Store) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		writeJSON(w, http.StatusOK, db.ListUsers())
	})
}

// NewUserByID handles GET /users/{id} — returns one user or 404.
func NewUserByID(db *store.Store) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Strip the "/users/" prefix; nested paths (e.g. "/users/u1/posts")
		// are NOT handled here yet. The bot's feature work should route
		// those without breaking GET /users/{id}.
		rest := strings.TrimPrefix(r.URL.Path, "/users/")
		if rest == "" || strings.Contains(rest, "/") {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		u, err := db.GetUser(rest)
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "user not found")
			return
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, u)
	})
}

// writeJSON serialises body as JSON with the given status code.
func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// writeError emits {"error":"<msg>"} as JSON. Used by every endpoint
// in this package so error shapes stay consistent.
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
