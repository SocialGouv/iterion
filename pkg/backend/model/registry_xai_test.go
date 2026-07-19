package model

import (
	"context"
	"testing"

	"github.com/SocialGouv/iterion/pkg/secrets"
)

func TestXAIBaseURL_Default(t *testing.T) {
	t.Setenv("XAI_BASE_URL", "")
	if got := xaiBaseURL(); got != secrets.XAIDefaultBaseURL {
		t.Fatalf("xaiBaseURL() = %q, want %q", got, secrets.XAIDefaultBaseURL)
	}
}

func TestXAIBaseURL_StripTrailingV1(t *testing.T) {
	// Users copy the OpenAI-SDK style base URL from xAI docs
	// (`https://api.x.ai/v1`); the openai client appends
	// `/v1/chat/completions`, so we strip the trailing /v1.
	t.Setenv("XAI_BASE_URL", "https://api.x.ai/v1/")
	if got := xaiBaseURL(); got != "https://api.x.ai" {
		t.Fatalf("xaiBaseURL() = %q, want https://api.x.ai", got)
	}
}

func TestXAIBaseURL_ProxyOverride(t *testing.T) {
	t.Setenv("XAI_BASE_URL", "https://proxy.example.com/xai")
	if got := xaiBaseURL(); got != "https://proxy.example.com/xai" {
		t.Fatalf("xaiBaseURL() = %q, want proxy host", got)
	}
}

func TestRegistry_XAIProviderRegistered(t *testing.T) {
	// Without a key the factory still builds a client — the openai
	// provider only errors when APIKey is empty *and* no OAuth pair is
	// present. We pass a dummy key via env so NewClient succeeds.
	t.Setenv("XAI_API_KEY", "xai-test")
	t.Setenv("XAI_BASE_URL", "")

	r := NewRegistry()
	client, err := r.Resolve("xai/grok-3")
	if err != nil {
		t.Fatalf("Resolve(xai/grok-3): %v", err)
	}
	if client == nil {
		t.Fatal("expected non-nil client")
	}

	// Capabilities should resolve without error (xai is a known provider).
	caps, err := r.Capabilities("xai/grok-3")
	if err != nil {
		t.Fatalf("Capabilities: %v", err)
	}
	if !caps.ToolCall {
		t.Error("grok-3 should support tool calling")
	}
	if !caps.Temperature {
		t.Error("grok-3 should accept temperature")
	}
	if caps.ContextWindow != 131_072 {
		t.Errorf("ContextWindow = %d, want 131072", caps.ContextWindow)
	}
}

func TestRegistry_XAIProviderWithKey(t *testing.T) {
	// BYOK path: ResolveWithContext with a per-run key for "xai".
	// Env empty so we prove the keyed factory is what built the client.
	t.Setenv("XAI_API_KEY", "")
	r := NewRegistry()

	SetCredentialsLookup(func(ctx context.Context) (func(string) string, bool) {
		return func(provider string) string {
			if provider == "xai" {
				return "byok-xai-key"
			}
			return ""
		}, true
	})
	t.Cleanup(func() {
		SetCredentialsLookup(func(context.Context) (func(string) string, bool) {
			return nil, false
		})
	})

	client, err := r.ResolveWithContext(context.Background(), "xai/grok-3-mini")
	if err != nil {
		t.Fatalf("ResolveWithContext: %v", err)
	}
	if client == nil {
		t.Fatal("expected non-nil BYOK client")
	}

	// Env-only Resolve should fail (no XAI_API_KEY) — proves BYOK is
	// the path that succeeded above rather than a silent env fallback.
	if _, err := r.Resolve("xai/grok-3-mini"); err == nil {
		t.Fatal("Resolve without env key should fail")
	}
}

func TestXAICapabilities_ReasoningMini(t *testing.T) {
	caps := curatedCapabilities("xai", "grok-3-mini")
	if !caps.Reasoning {
		t.Error("grok-3-mini should be Reasoning")
	}
	if caps.Temperature {
		t.Error("grok-3-mini should not accept Temperature")
	}
	caps = curatedCapabilities("xai", "grok-3")
	if caps.Reasoning {
		t.Error("grok-3 should not be Reasoning by default")
	}
	if !caps.Temperature {
		t.Error("grok-3 should accept Temperature")
	}
}
