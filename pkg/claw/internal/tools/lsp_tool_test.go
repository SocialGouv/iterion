package tools

import (
	"encoding/json"
	"github.com/SocialGouv/iterion/pkg/claw/internal/lsp"
	"strings"
	"testing"
)

func TestLSP_Diagnostics(t *testing.T) {
	reg := lsp.NewRegistry()
	reg.Register("go", lsp.StatusConnected, nil, []string{"diagnostics"})
	reg.AddDiagnostics("go", []lsp.LspDiagnostic{
		{Path: "/tmp/main.go", Message: "unused import", Severity: "warning"},
	})

	result, err := ExecuteLSP(map[string]any{
		"action": "diagnostics",
		"path":   "/tmp/main.go",
	}, reg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(result), &parsed); err != nil {
		t.Fatalf("failed to parse result: %v", err)
	}
	if parsed["action"] != "diagnostics" {
		t.Errorf("expected action 'diagnostics', got %v", parsed["action"])
	}
	count, ok := parsed["count"].(float64)
	if !ok || count != 1 {
		t.Errorf("expected count=1, got %v", parsed["count"])
	}
}

func TestLSP_AllDiagnostics(t *testing.T) {
	reg := lsp.NewRegistry()
	reg.Register("go", lsp.StatusConnected, nil, []string{"diagnostics"})

	result, err := ExecuteLSP(map[string]any{
		"action": "diagnostics",
	}, reg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(result), &parsed); err != nil {
		t.Fatalf("failed to parse result: %v", err)
	}
	if parsed["action"] != "diagnostics" {
		t.Errorf("expected action 'diagnostics', got %v", parsed["action"])
	}
}

func TestLSP_Hover(t *testing.T) {
	reg := lsp.NewRegistry()
	root := "/tmp"
	reg.Register("go", lsp.StatusConnected, &root, []string{"hover"})

	result, err := ExecuteLSP(map[string]any{
		"action":    "hover",
		"path":      "/tmp/main.go",
		"line":      float64(10),
		"character": float64(5),
	}, reg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(result), &parsed); err != nil {
		t.Fatalf("failed to parse result: %v", err)
	}
	// Without a transport, we get a placeholder response.
	if parsed["action"] != "hover" {
		t.Errorf("expected action 'hover', got %v", parsed["action"])
	}
}

func TestLSP_InvalidAction(t *testing.T) {
	reg := lsp.NewRegistry()

	result, err := ExecuteLSP(map[string]any{
		"action": "invalid_action",
	}, reg)
	if err != nil {
		t.Fatalf("unexpected hard error: %v", err)
	}
	// Should return structured error JSON, not a Go error.
	var parsed map[string]any
	if err := json.Unmarshal([]byte(result), &parsed); err != nil {
		t.Fatalf("failed to parse result: %v", err)
	}
	if parsed["status"] != "error" {
		t.Errorf("expected status 'error', got %v", parsed["status"])
	}
	if parsed["action"] != "invalid_action" {
		t.Errorf("expected action 'invalid_action', got %v", parsed["action"])
	}
}

func TestLSP_MissingAction(t *testing.T) {
	reg := lsp.NewRegistry()

	_, err := ExecuteLSP(map[string]any{}, reg)
	if err == nil {
		t.Fatal("expected error for missing action")
	}
	if !strings.Contains(err.Error(), "action") {
		t.Errorf("expected error about 'action', got: %v", err)
	}
}

func TestLSP_NilRegistry(t *testing.T) {
	_, err := ExecuteLSP(map[string]any{"action": "diagnostics"}, nil)
	if err == nil {
		t.Fatal("expected error for nil registry")
	}
	if !strings.Contains(err.Error(), "not available") {
		t.Errorf("expected 'not available' error, got: %v", err)
	}
}

func TestLSP_NoServerForPath(t *testing.T) {
	reg := lsp.NewRegistry()
	// No servers registered.

	result, err := ExecuteLSP(map[string]any{
		"action": "hover",
		"path":   "/tmp/test.xyz",
	}, reg)
	if err != nil {
		t.Fatalf("unexpected hard error: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(result), &parsed); err != nil {
		t.Fatalf("failed to parse result: %v", err)
	}
	if parsed["status"] != "error" {
		t.Errorf("expected status 'error', got %v", parsed["status"])
	}
}
