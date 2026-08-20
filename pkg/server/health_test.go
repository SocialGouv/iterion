package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
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

// The health envelope echoes the usage-cap policy so an operator can
// verify the cap actually reached the deployment — the enforcement is
// env-only and was otherwise observable nowhere.
func TestHealthzEchoesUsageCap(t *testing.T) {
	probe := func() healthResponse {
		srv := New(Config{}, iterlog.New(iterlog.LevelError, nil))
		req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
		rec := httptest.NewRecorder()
		srv.handler = srv.mux
		srv.handler.ServeHTTP(rec, req)
		var payload healthResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return payload
	}

	t.Setenv("ITERION_USAGE_CAP_WEEK_PCT", "85")
	if got := probe().UsageCap; !strings.Contains(got, "week=85%/hard") {
		t.Errorf("usage_cap = %q, want it to name week=85%%/hard", got)
	}

	t.Setenv("ITERION_USAGE_CAP_WEEK_PCT", "")
	if got := probe().UsageCap; got != "usage caps off" {
		t.Errorf("usage_cap = %q, want %q when unset", got, "usage caps off")
	}

	// A malformed value is reported, never hidden.
	t.Setenv("ITERION_USAGE_CAP_WEEK_PCT", "eighty-five")
	if got := probe().UsageCap; !strings.HasPrefix(got, "invalid: ") {
		t.Errorf("usage_cap = %q, want an invalid: prefix on a malformed value", got)
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
