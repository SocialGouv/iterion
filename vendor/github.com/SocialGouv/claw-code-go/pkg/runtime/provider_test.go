package runtime_test

import (
	"context"
	"testing"

	"github.com/SocialGouv/claw-code-go/pkg/api"
	pkgrt "github.com/SocialGouv/claw-code-go/pkg/runtime"
)

func TestSelectProviderAllNames(t *testing.T) {
	names := []struct {
		input    string
		wantName string
	}{
		{"anthropic", "anthropic"},
		{"openai", "openai"},
		{"xai", "openai"},       // xai uses openai provider
		{"dashscope", "openai"}, // dashscope uses openai provider
		{"bedrock", "bedrock"},
		{"vertex", "vertex"},
		{"foundry", "foundry"},
	}
	for _, tc := range names {
		p := pkgrt.SelectProvider(tc.input)
		if p == nil {
			t.Fatalf("SelectProvider(%q) returned nil", tc.input)
		}
		if got := p.Name(); got != tc.wantName {
			t.Errorf("SelectProvider(%q).Name() = %q, want %q", tc.input, got, tc.wantName)
		}
	}
}

func TestSelectProviderDefault(t *testing.T) {
	p := pkgrt.SelectProvider("")
	if p == nil {
		t.Fatal("SelectProvider(\"\") returned nil")
	}
	if got := p.Name(); got != "anthropic" {
		t.Errorf("default provider = %q, want anthropic", got)
	}
}

func TestNewNoAuthClient(t *testing.T) {
	c := pkgrt.NewNoAuthClient()
	if c == nil {
		t.Fatal("NewNoAuthClient returned nil")
	}
	// StreamResponse should return an error.
	_, err := c.StreamResponse(context.Background(), api.CreateMessageRequest{})
	if err == nil {
		t.Error("expected error from NoAuthClient")
	}
}
