package model

import (
	"testing"

	"claw-code-go/pkg/api"
)

// ---------------------------------------------------------------------------
// Registry unit tests (dedicated file for claw migration coverage)
// ---------------------------------------------------------------------------

// TestClawRegistry_ResolveSuccess verifies basic resolve and caching.
func TestClawRegistry_ResolveSuccess(t *testing.T) {
	r := NewRegistry()
	mock := &execMockClient{streams: []<-chan api.StreamEvent{mockStreamEvents("hello", "end_turn")}}
	r.Register("custom", func(modelID string) (api.APIClient, error) {
		return mock, nil
	})

	m, err := r.Resolve("custom/my-model")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if m == nil {
		t.Fatal("expected non-nil client")
	}
}

// TestClawRegistry_ResolveCache verifies that repeated Resolve calls return
// the same cached instance.
func TestClawRegistry_ResolveCache(t *testing.T) {
	r := NewRegistry()
	callCount := 0
	mock := &execMockClient{streams: []<-chan api.StreamEvent{mockStreamEvents("hello", "end_turn")}}
	r.Register("test", func(modelID string) (api.APIClient, error) {
		callCount++
		return mock, nil
	})

	m1, _ := r.Resolve("test/cached")
	m2, _ := r.Resolve("test/cached")

	if m1 != m2 {
		t.Error("expected same instance from cache")
	}
	if callCount != 1 {
		t.Errorf("factory called %d times, want 1 (cached)", callCount)
	}
}

// TestClawRegistry_ResolveUnknownProvider verifies error on unknown provider.
func TestClawRegistry_ResolveUnknownProvider(t *testing.T) {
	r := NewRegistry()
	_, err := r.Resolve("nonexistent/model")
	if err == nil {
		t.Fatal("expected error for unknown provider")
	}
}

// TestClawRegistry_ParseModelSpecValid verifies valid spec parsing.
func TestClawRegistry_ParseModelSpecValid(t *testing.T) {
	tests := []struct {
		spec     string
		provider string
		model    string
	}{
		{"anthropic/claude-sonnet-4-6", "anthropic", "claude-sonnet-4-6"},
		{"openai/gpt-4o", "openai", "gpt-4o"},
		{"bedrock/us.amazon.nova-pro-v1:0", "bedrock", "us.amazon.nova-pro-v1:0"},
	}

	for _, tt := range tests {
		p, m, err := ParseModelSpec(tt.spec)
		if err != nil {
			t.Errorf("ParseModelSpec(%q): unexpected error: %v", tt.spec, err)
			continue
		}
		if p != tt.provider || m != tt.model {
			t.Errorf("ParseModelSpec(%q) = (%q, %q), want (%q, %q)", tt.spec, p, m, tt.provider, tt.model)
		}
	}
}

// TestClawRegistry_ParseModelSpecInvalid verifies error on invalid specs.
func TestClawRegistry_ParseModelSpecInvalid(t *testing.T) {
	invalid := []string{
		"no-slash",
		"/missing-provider",
		"trailing/",
		"",
	}
	for _, spec := range invalid {
		_, _, err := ParseModelSpec(spec)
		if err == nil {
			t.Errorf("ParseModelSpec(%q): expected error", spec)
		}
	}
}
