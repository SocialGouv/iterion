package auth

import (
	"os"
	"testing"
)

// The env branch order is the contract: ANTHROPIC_* first (an operator who
// pointed ANTHROPIC_BASE_URL at z.ai keeps that explicit choice), OPENAI
// next, then z.ai's native key — a bare ZAI_API_KEY now names its own
// provider instead of falling through to the store's default ("anthropic").
func TestResolveCredentials_ZAIKeyNamesZaiProvider(t *testing.T) {
	t.Setenv("ZAI_API_KEY", "zai-key")
	os.Unsetenv("ANTHROPIC_API_KEY")
	os.Unsetenv("OPENAI_API_KEY")

	provider, _, method, err := ResolveCredentials()
	if err != nil {
		t.Fatalf("ResolveCredentials: %v", err)
	}
	if provider != "zai" {
		t.Errorf("provider = %q, want %q", provider, "zai")
	}
	if method != "api_key" {
		t.Errorf("method = %q, want %q", method, "api_key")
	}
}

// An explicit Anthropic key keeps precedence over the ambient z.ai key: the
// env-var order is a precedence chain, and ANTHROPIC_* comes first.
func TestResolveCredentials_AnthropicKeyBeatsZai(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-test")
	t.Setenv("ZAI_API_KEY", "zai-key")
	os.Unsetenv("OPENAI_API_KEY")

	provider, _, _, err := ResolveCredentials()
	if err != nil {
		t.Fatalf("ResolveCredentials: %v", err)
	}
	if provider != "anthropic" {
		t.Errorf("provider = %q, want %q (ANTHROPIC_* precedes ZAI_API_KEY)", provider, "anthropic")
	}
}

// Same contract for Moonshot: a bare MOONSHOT_API_KEY names its own provider
// rather than falling through to the store's default ("anthropic") — falling
// through would spend an Anthropic credential for a route the operator
// pointed at Kimi.
func TestResolveCredentials_MoonshotKeyNamesMoonshotProvider(t *testing.T) {
	t.Setenv("MOONSHOT_API_KEY", "moonshot-key")
	os.Unsetenv("ANTHROPIC_API_KEY")
	os.Unsetenv("OPENAI_API_KEY")
	os.Unsetenv("ZAI_API_KEY")

	provider, _, method, err := ResolveCredentials()
	if err != nil {
		t.Fatalf("ResolveCredentials: %v", err)
	}
	if provider != "moonshot" {
		t.Errorf("provider = %q, want %q", provider, "moonshot")
	}
	if method != "api_key" {
		t.Errorf("method = %q, want %q", method, "api_key")
	}
}

// An explicit Anthropic key keeps precedence over the ambient Moonshot key,
// exactly as it does over z.ai's.
func TestResolveCredentials_AnthropicKeyBeatsMoonshot(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-test")
	t.Setenv("MOONSHOT_API_KEY", "moonshot-key")
	os.Unsetenv("OPENAI_API_KEY")
	os.Unsetenv("ZAI_API_KEY")

	provider, _, _, err := ResolveCredentials()
	if err != nil {
		t.Fatalf("ResolveCredentials: %v", err)
	}
	if provider != "anthropic" {
		t.Errorf("provider = %q, want %q (ANTHROPIC_* precedes MOONSHOT_API_KEY)", provider, "anthropic")
	}
}
