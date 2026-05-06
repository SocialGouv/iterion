package server

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

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

// defaultReadinessTimeout caps each individual readiness check when the
// caller did not set Config.ReadinessTimeout.
const defaultReadinessTimeout = 1 * time.Second

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

// handleReadyz is the readiness probe. Returns 200 when every external
// dependency wired via Config.ReadinessChecks responds within
// ReadinessTimeout, 503 with a per-dep status map otherwise. Local
// mode has no dependencies and always returns 200.
//
// Cloud-ready plan §F (T-37).
func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	checks := s.cfg.ReadinessChecks
	resp := healthResponse{
		Status:  "ok",
		Mode:    s.deployMode(),
		Version: appinfo.Version,
		Commit:  appinfo.Commit,
	}
	if len(checks) == 0 {
		writeHealthJSON(w, http.StatusOK, resp)
		return
	}

	timeout := s.cfg.ReadinessTimeout
	if timeout <= 0 {
		timeout = defaultReadinessTimeout
	}

	results := make(map[string]string, len(checks))
	allOK := true
	for name, check := range checks {
		ctx, cancel := context.WithTimeout(r.Context(), timeout)
		err := check(ctx)
		cancel()
		if err != nil {
			results[name] = "error: " + err.Error()
			allOK = false
			continue
		}
		results[name] = "ok"
	}
	resp.Checks = results
	if !allOK {
		resp.Status = "degraded"
		writeHealthJSON(w, http.StatusServiceUnavailable, resp)
		return
	}
	writeHealthJSON(w, http.StatusOK, resp)
}

// deployMode reports the persistence backend in use. Reads Config.Mode
// when set (cloud bootstrap path), otherwise falls back to the
// store-directory heuristic so existing local-only callers keep
// working without a Config update.
func (s *Server) deployMode() string {
	if s.cfg.Mode != "" {
		return s.cfg.Mode
	}
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
