package dispatcher

import (
	"encoding/json"
	"net/http"

	"github.com/SocialGouv/iterion/internal/httpx"
)

// WriteJSON encodes v as JSON with the given HTTP status. Exported so
// the sub-package pkg/dispatcher/native can share the same shape
// without duplicating the function.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// WriteErr is the convention for surfacing an error to the SPA:
// {"error": "<message>"} at the requested status.
func WriteErr(w http.ResponseWriter, status int, err error) {
	httpx.WriteError(w, status, "", err.Error())
}
