package metrics

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNew_registersAllMetrics(t *testing.T) {
	r := New()
	if r == nil || r.reg == nil {
		t.Fatal("New() returned a nil registry")
	}
	// Smoke-fire every metric so a missing Counter/Gauge/Histogram surfaces.
	r.RunsCreatedTotal.WithLabelValues("finished").Inc()
	r.RunsActive.WithLabelValues("running").Set(1)
	r.RunDurationSeconds.WithLabelValues("finished").Observe(1.0)
	r.WSConnections.Set(1)
	r.MongoChangeStreamLagS.Set(0.5)
	r.NATSPendingMessages.Set(0)
	r.WorkspaceCloneDuration.Observe(0.1)
	r.LLMTokensTotal.WithLabelValues("claw", "x", "input").Add(10)
	r.LLMCostUSDTotal.WithLabelValues("claw", "x").Add(0.001)
	r.RunnerHeartbeatErrors.Inc()
	r.BotOverrideShadowsNewer.WithLabelValues("review-pr").Set(1)
	r.BotShadowCheckUnavailable.Set(0)
	r.BotOverrideUnorderableVersion.WithLabelValues("review-pr").Set(1)
}

func TestHandler_servesPrometheusText(t *testing.T) {
	r := New()
	r.RunsCreatedTotal.WithLabelValues("finished").Inc()
	// The override-shadow series an alert rule names: a gauge left out of
	// the MustRegister list is set happily and scraped by nobody, so the
	// alert stays silent forever on a deployment that looks instrumented.
	r.BotOverrideShadowsNewer.WithLabelValues("review-pr").Set(1)
	r.BotShadowCheckUnavailable.Set(1)
	r.BotOverrideUnorderableVersion.WithLabelValues("review-pr").Set(1)
	r.GatewayAliveRuns.Replace(map[string]float64{"gw.example": 1})
	r.GatewayAliveRunsUnavailable.Set(0)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	r.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body, _ := io.ReadAll(rec.Body)
	if !strings.Contains(string(body), "iterion_runs_created_total") {
		t.Errorf("/metrics body missing counter:\n%s", string(body))
	}
	for _, name := range []string{
		"iterion_bot_override_shadows_newer",
		"iterion_bot_shadow_check_unavailable",
		"iterion_bot_override_unorderable_version",
		"iterion_gateway_alive_runs{",
		"iterion_gateway_alive_runs_unavailable",
	} {
		if !strings.Contains(string(body), name) {
			t.Errorf("/metrics body missing %s (registered?):\n%s", name, string(body))
		}
	}
}

func TestStartServer_bindsAndShutsDown(t *testing.T) {
	r := New()
	srv, err := r.StartServer("127.0.0.1:0", nil)
	if err != nil {
		t.Fatalf("StartServer: %v", err)
	}
	if srv == nil {
		t.Fatal("StartServer returned a nil server for a non-empty addr")
	}
	if err := ShutdownServer(srv); err != nil {
		t.Errorf("ShutdownServer: %v", err)
	}
}

func TestStartServer_emptyAddr_isNoOp(t *testing.T) {
	r := New()
	srv, err := r.StartServer("", nil)
	if err != nil {
		t.Fatalf("StartServer(empty): err = %v", err)
	}
	if srv != nil {
		t.Fatal("StartServer(\"\") must return nil server")
	}
	if err := ShutdownServer(nil); err != nil {
		t.Errorf("ShutdownServer(nil): %v", err)
	}
}

func TestStartServer_bindFailure_returnsErrSync(t *testing.T) {
	r := New()
	// 0.0.0.0:1 is unprivileged-bind-only on most hosts; this picks a
	// reliably-unbindable address on linux test runners.
	_, err := r.StartServer("256.256.256.256:9", nil)
	if err == nil {
		t.Fatal("StartServer must surface bind errors synchronously")
	}
}

// A process that never samples the gateway gauge — a runner pod serving the
// same registry — publishes no availability for it: a registered zero would
// read as a reading nobody took. The first Set publishes the series.
func TestGatewayAliveRunsUnavailable_NoSeriesUntilSampled(t *testing.T) {
	r := New()
	scrape := func() string {
		t.Helper()
		rec := httptest.NewRecorder()
		r.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		return rec.Body.String()
	}
	if body := scrape(); strings.Contains(body, "iterion_gateway_alive_runs_unavailable") {
		t.Fatalf("a registry nobody sampled publishes iterion_gateway_alive_runs_unavailable:\n%s", body)
	}
	r.GatewayAliveRunsUnavailable.Set(1)
	if body := scrape(); !strings.Contains(body, "iterion_gateway_alive_runs_unavailable 1") {
		t.Fatalf("the sampler's first Set did not publish the series:\n%s", body)
	}
}
