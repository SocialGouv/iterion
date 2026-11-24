package testutil

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestSpawnMockService(t *testing.T) {
	svc := SpawnMockService()
	defer svc.Close()

	if svc.BaseURL() == "" {
		t.Fatal("expected non-empty base URL")
	}
	if !strings.HasPrefix(svc.BaseURL(), "http://") {
		t.Fatalf("expected http URL, got %s", svc.BaseURL())
	}
}

func TestStreamingTextScenario(t *testing.T) {
	svc := SpawnMockService()
	defer svc.Close()

	body := fmt.Sprintf(`{"model":"claude-sonnet-4-6","stream":true,"messages":[{"role":"user","content":[{"type":"text","text":"hello %sstreaming_text"}]}]}`, ScenarioPrefix)

	resp, err := http.Post(svc.BaseURL()+"/v1/messages", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("expected text/event-stream, got %s", ct)
	}

	respBody, _ := io.ReadAll(resp.Body)
	respStr := string(respBody)
	if !strings.Contains(respStr, "content_block_delta") {
		t.Fatal("expected content_block_delta in SSE response")
	}
	if !strings.Contains(respStr, "Mock streaming") {
		t.Fatal("expected 'Mock streaming' text in response")
	}
	if !strings.Contains(respStr, "parity harness") {
		t.Fatal("expected 'parity harness' text in response")
	}
}

func TestReadFileRoundtripScenario(t *testing.T) {
	svc := SpawnMockService()
	defer svc.Close()

	body := fmt.Sprintf(`{"model":"claude-sonnet-4-6","stream":true,"messages":[{"role":"user","content":[{"type":"text","text":"test %sread_file_roundtrip"}]}]}`, ScenarioPrefix)

	resp, err := http.Post(svc.BaseURL()+"/v1/messages", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post failed: %v", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	respStr := string(respBody)
	if !strings.Contains(respStr, "tool_use") {
		t.Fatal("expected tool_use in response")
	}
	if !strings.Contains(respStr, "read_file") {
		t.Fatal("expected read_file tool name in response")
	}
}

func TestCapturedRequests(t *testing.T) {
	svc := SpawnMockService()
	defer svc.Close()

	body := fmt.Sprintf(`{"model":"claude-sonnet-4-6","stream":false,"messages":[{"role":"user","content":[{"type":"text","text":"test %sstreaming_text"}]}]}`, ScenarioPrefix)

	resp, err := http.Post(svc.BaseURL()+"/v1/messages", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post failed: %v", err)
	}
	resp.Body.Close()

	captured := svc.CapturedRequests()
	if len(captured) != 1 {
		t.Fatalf("expected 1 captured request, got %d", len(captured))
	}
	if captured[0].Method != "POST" {
		t.Fatalf("expected POST, got %s", captured[0].Method)
	}
	if captured[0].Scenario != "streaming_text" {
		t.Fatalf("expected streaming_text scenario, got %s", captured[0].Scenario)
	}
	if captured[0].Stream {
		t.Fatal("expected stream=false")
	}
}

func TestNonStreamingResponse(t *testing.T) {
	svc := SpawnMockService()
	defer svc.Close()

	body := fmt.Sprintf(`{"model":"claude-sonnet-4-6","stream":false,"messages":[{"role":"user","content":[{"type":"text","text":"test %sstreaming_text"}]}]}`, ScenarioPrefix)

	resp, err := http.Post(svc.BaseURL()+"/v1/messages", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post failed: %v", err)
	}
	defer resp.Body.Close()

	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("expected application/json, got %s", ct)
	}

	var result map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode failed: %v", err)
	}
	if result["type"] != "message" {
		t.Fatalf("expected type=message, got %v", result["type"])
	}
}

func TestRateLimited429Scenario(t *testing.T) {
	svc := SpawnMockService()
	defer svc.Close()

	body := fmt.Sprintf(`{"model":"claude-sonnet-4-6","stream":false,"messages":[{"role":"user","content":[{"type":"text","text":"test %srate_limited_429"}]}]}`, ScenarioPrefix)

	resp, err := http.Post(svc.BaseURL()+"/v1/messages", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d", resp.StatusCode)
	}
	if ra := resp.Header.Get("Retry-After"); ra != "30" {
		t.Fatalf("expected Retry-After: 30, got %s", ra)
	}

	respBody, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(respBody), "rate_limit_error") {
		t.Fatal("expected rate_limit_error in response body")
	}
}

func TestAuthFailure401Scenario(t *testing.T) {
	svc := SpawnMockService()
	defer svc.Close()

	body := fmt.Sprintf(`{"model":"claude-sonnet-4-6","stream":false,"messages":[{"role":"user","content":[{"type":"text","text":"test %sauth_failure_401"}]}]}`, ScenarioPrefix)

	resp, err := http.Post(svc.BaseURL()+"/v1/messages", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}

	respBody, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(respBody), "authentication_error") {
		t.Fatal("expected authentication_error in response body")
	}
}

func TestAuthForbidden403Scenario(t *testing.T) {
	svc := SpawnMockService()
	defer svc.Close()

	body := fmt.Sprintf(`{"model":"claude-sonnet-4-6","stream":false,"messages":[{"role":"user","content":[{"type":"text","text":"test %sauth_forbidden_403"}]}]}`, ScenarioPrefix)

	resp, err := http.Post(svc.BaseURL()+"/v1/messages", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", resp.StatusCode)
	}

	respBody, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(respBody), "permission_error") {
		t.Fatal("expected permission_error in response body")
	}
}

func TestContextWindowExceededScenario(t *testing.T) {
	svc := SpawnMockService()
	defer svc.Close()

	body := fmt.Sprintf(`{"model":"claude-sonnet-4-6","stream":false,"messages":[{"role":"user","content":[{"type":"text","text":"test %scontext_window_exceeded"}]}]}`, ScenarioPrefix)

	resp, err := http.Post(svc.BaseURL()+"/v1/messages", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}

	respBody, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(respBody), "prompt is too long") {
		t.Fatal("expected context window error message in response body")
	}
}

func TestChunkedSSEScenario(t *testing.T) {
	svc := SpawnMockService()
	defer svc.Close()

	body := fmt.Sprintf(`{"model":"claude-sonnet-4-6","stream":true,"messages":[{"role":"user","content":[{"type":"text","text":"test %schunked_sse"}]}]}`, ScenarioPrefix)

	resp, err := http.Post(svc.BaseURL()+"/v1/messages", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("expected text/event-stream, got %s", ct)
	}

	respBody, _ := io.ReadAll(resp.Body)
	respStr := string(respBody)

	// Should contain multiple content_block_delta events
	count := strings.Count(respStr, "content_block_delta")
	if count < 6 {
		t.Fatalf("expected at least 6 content_block_delta events, got %d", count)
	}

	// Should contain all chunks
	for _, chunk := range []string{"Chunk", " one.", " two.", " three."} {
		if !strings.Contains(respStr, chunk) {
			t.Fatalf("missing chunk %q in response", chunk)
		}
	}
}

func TestScenarioParsing(t *testing.T) {
	tests := []struct {
		input string
		want  Scenario
		ok    bool
	}{
		{"streaming_text", ScenarioStreamingText, true},
		{"read_file_roundtrip", ScenarioReadFileRoundtrip, true},
		{"bash_stdout_roundtrip", ScenarioBashStdoutRoundtrip, true},
		{"unknown_scenario", 0, false},
		{" streaming_text ", ScenarioStreamingText, true},
	}

	for _, tt := range tests {
		s, ok := ParseScenario(tt.input)
		if ok != tt.ok {
			t.Errorf("ParseScenario(%q): ok=%v, want %v", tt.input, ok, tt.ok)
		}
		if ok && s != tt.want {
			t.Errorf("ParseScenario(%q): got %v, want %v", tt.input, s, tt.want)
		}
	}
}
