package server

import (
	"encoding/json"
	"net/http"

	"github.com/SocialGouv/iterion/pkg/internal/appinfo"
)

// healthResponse is the JSON envelope returned by /healthz and /readyz.
type healthResponse struct {
	Status  string            `json:"status"`            // "ok" or "degraded"
	Mode    string            `json:"mode"`              // "local" or "cloud"
	Version string            `json:"version,omitempty"` // build version
	Commit  string            `json:"commit,omitempty"`  // build commit
	Checks  map[string]string `json:"checks,omitempty"`  // per-dependency status (cloud only)
}

// handleHealthz is the liveness probe. Always returns 200 — its only
// promise is that the HTTP server's mux loop is responsive. Cloud
// deployments use this for the kubelet `livenessProbe`; a 503 here
// would mean restart the pod, which is a much stronger signal than
// "Mongo briefly degraded".
//
// Cloud-ready plan §F (T-37).
func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeHealthJSON(w, http.StatusOK, healthResponse{
		Status:  "ok",
		Mode:    s.deployMode(),
		Version: appinfo.Version,
		Commit:  appinfo.Commit,
	})
}

// handleReadyz is the readiness probe. Returns 200 when the server is
// ready to take traffic, 503 otherwise. For local mode there is
// nothing to ping (filesystem store), so the answer is always 200.
// Cloud mode will ping Mongo, NATS, and S3 with short (1s) timeouts
// once T-25/T-26 wire the cloud-side store dependencies into the
// server. Until then we surface a "ready" answer with mode tagged so
// smoke tests can still gate on the endpoint.
//
// Cloud-ready plan §F (T-37).
func (s *Server) handleReadyz(w http.ResponseWriter, _ *http.Request) {
	writeHealthJSON(w, http.StatusOK, healthResponse{
		Status:  "ok",
		Mode:    s.deployMode(),
		Version: appinfo.Version,
		Commit:  appinfo.Commit,
	})
}

// deployMode reports the persistence backend in use. The editor
// server is the local-mode entry point today; once T-30 (cmd/
// iterion/server.go) lands a cloud-aware server, that path will pass
// a Capabilities()-aware store and we'll learn the mode from there.
func (s *Server) deployMode() string {
	if s.runs == nil {
		return "local"
	}
	if s.runs.StoreDir() == "" {
		return "cloud"
	}
	return "local"
}

// writeHealthJSON is a one-liner JSON response helper for the health
// endpoints. Not exported — the rest of the server composes responses
// inline today.
func writeHealthJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}
