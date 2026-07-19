// Package httpx provides the shared JSON request/response helpers used by
// iterion's HTTP handlers.
package httpx

import (
	"encoding/json"
	"io"
	"net/http"
)

// maxBody bounds request bodies read by DecodeJSON (10 MB).
const maxBody = 10 << 20

// WriteJSON writes v as an application/json response with the given status
// code. Content-Type is set before WriteHeader; encoding uses json.NewEncoder
// defaults (HTML escaping on, trailing newline), and encode errors are
// discarded — the status line is already on the wire.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// EncodeJSON writes v as an application/json body WITHOUT touching the status
// code: callers either rely on the implicit 200 from the first body write or
// have already called WriteHeader themselves (in which case the Content-Type
// set here is a no-op, matching the historical helper's behavior).
func EncodeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// DecodeJSON reads at most 10 MB of the request body and unmarshals it into
// dst.
func DecodeJSON(r *http.Request, dst any) error {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
	if err != nil {
		return err
	}
	return json.Unmarshal(body, dst)
}
