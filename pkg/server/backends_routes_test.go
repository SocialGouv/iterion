package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/detect"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// TestBackendsDetectRouteShape verifies the route returns a well-formed
// Report. Detection sondes are not mocked — we run them against a scrubbed
// env so the result is deterministic ("nothing available").
func TestBackendsDetectRouteShape(t *testing.T) {
	for _, k := range []string{
		"ITERION_BACKEND_PREFERENCE",
		"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN",
		"OPENAI_API_KEY", "XAI_API_KEY",
		"AZURE_OPENAI_API_KEY", "AZURE_OPENAI_ENDPOINT",
		"AWS_REGION", "AWS_DEFAULT_REGION",
		"GOOGLE_CLOUD_PROJECT",
		"CLAUDE_CONFIG_DIR", "CODEX_HOME",
		"ANTHROPIC_BASE_URL", "ZAI_API_KEY",
	} {
		t.Setenv(k, "")
	}
	t.Setenv("HOME", t.TempDir())

	srv := New(Config{DisableAuth: true}, iterlog.New(iterlog.LevelError, nil))
	srv.handler = srv.mux

	req := httptest.NewRequest(http.MethodGet, "/api/backends/detect", nil)
	rec := httptest.NewRecorder()
	srv.handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	var got detect.Report
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v; body=%s", err, rec.Body.String())
	}

	if len(got.PreferenceOrder) == 0 {
		t.Fatal("PreferenceOrder is empty")
	}
	if len(got.Backends) != 3 {
		t.Fatalf("got %d backends, want 3 (claude_code, claw, codex)", len(got.Backends))
	}
	if got.ResolvedDefault != "" {
		t.Fatalf("ResolvedDefault = %q, want empty (no creds)", got.ResolvedDefault)
	}
	// xai must appear in the providers list even when unavailable, so the
	// studio can render it as a configure-me option.
	foundXAI := false
	for _, p := range got.Providers {
		if p.Name == "xai" {
			foundXAI = true
			if p.Available {
				t.Fatal("xai Available=true with no XAI_API_KEY")
			}
			if p.SuggestedModel != "xai/grok-3" {
				t.Fatalf("xai SuggestedModel = %q", p.SuggestedModel)
			}
		}
	}
	if !foundXAI {
		t.Fatal("xai provider missing from detect report")
	}
}

// TestBackendsDetectReflectsAnthropic verifies that setting an env var is
// reflected in the JSON shape and that ResolvedDefault flips to claw.
func TestBackendsDetectReflectsAnthropic(t *testing.T) {
	for _, k := range []string{
		"ITERION_BACKEND_PREFERENCE",
		"ANTHROPIC_AUTH_TOKEN", "OPENAI_API_KEY", "XAI_API_KEY",
		"AZURE_OPENAI_API_KEY", "AZURE_OPENAI_ENDPOINT",
		"AWS_REGION", "AWS_DEFAULT_REGION",
		"GOOGLE_CLOUD_PROJECT",
		"CLAUDE_CONFIG_DIR", "CODEX_HOME",
		// ANTHROPIC_BASE_URL / ZAI_API_KEY steer detection onto the
		// z.ai facade (hasZAIBaseURL), which suppresses the "anthropic"
		// provider and would leave claw unresolved. Clear them so the
		// test is isolated from a host running against z.ai/bigmodel.
		"ANTHROPIC_BASE_URL", "ZAI_API_KEY",
	} {
		t.Setenv(k, "")
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-test")

	srv := New(Config{DisableAuth: true}, iterlog.New(iterlog.LevelError, nil))
	srv.handler = srv.mux

	req := httptest.NewRequest(http.MethodGet, "/api/backends/detect", nil)
	rec := httptest.NewRecorder()
	srv.handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var got detect.Report
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got.ResolvedDefault != detect.BackendClaw {
		t.Fatalf("ResolvedDefault = %q, want claw", got.ResolvedDefault)
	}
	// Ensure no API key value leaks in the JSON — only env-var names.
	if strings.Contains(rec.Body.String(), "sk-ant-test") {
		t.Fatalf("response body leaks API key value")
	}
}

// Sanity that os.Stat is what we think — guard against accidental
// reliance on test ordering when CLAUDE_CONFIG_DIR points at a real dir.
func TestBackendsDetect_NoSensitiveLeak(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	srv := New(Config{DisableAuth: true}, iterlog.New(iterlog.LevelError, nil))
	srv.handler = srv.mux
	req := httptest.NewRequest(http.MethodGet, "/api/backends/detect", nil)
	rec := httptest.NewRecorder()
	srv.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	// The body must be valid JSON (smoke).
	var report detect.Report
	if err := json.Unmarshal(rec.Body.Bytes(), &report); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	// Response must not include a 'sealed_payload' or 'access_token'
	// substring — those would indicate a leaked OAuth field.
	for _, banned := range []string{"sealed_payload", "access_token", "refresh_token"} {
		if strings.Contains(rec.Body.String(), banned) {
			t.Fatalf("response contains banned field %q", banned)
		}
	}
}
