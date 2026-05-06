package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// /healthz must always return 200 even when the run console is
// disabled — the kubelet liveness probe relies on this contract.
func TestHealthzAlwaysOK(t *testing.T) {
	t.Parallel()

	srv := New(Config{}, iterlog.New(iterlog.LevelError, nil))

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	srv.handler = srv.mux
	srv.handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var payload healthResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if payload.Status != "ok" {
		t.Errorf("status = %q, want ok", payload.Status)
	}
	if payload.Mode != "local" {
		t.Errorf("mode = %q, want local for filesystem store", payload.Mode)
	}
}

// /readyz returns 200 in local mode (no dependencies to ping). Cloud
// pings come via T-26 once Mongo/NATS/S3 are wired into the server's
// dependency graph.
func TestReadyzLocalReturnsOK(t *testing.T) {
	t.Parallel()

	srv := New(Config{}, iterlog.New(iterlog.LevelError, nil))

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()
	srv.handler = srv.mux
	srv.handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}
