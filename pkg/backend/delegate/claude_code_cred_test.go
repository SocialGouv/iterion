package delegate

import (
	"context"
	"testing"

	"github.com/SocialGouv/iterion/pkg/secrets"
)

// resetClaudeCredEnv scrubs every process-env var that participates in
// anthropicCredEnvForCLI's resolution so each test starts from a clean
// slate. Keeps the matrix focused on the ctx-creds + hint inputs.
func resetClaudeCredEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"ANTHROPIC_API_KEY",
		"ANTHROPIC_AUTH_TOKEN",
		"ANTHROPIC_BASE_URL",
		"ZAI_API_KEY",
		"CLAUDE_CONFIG_DIR",
	} {
		t.Setenv(k, "")
	}
}

// ctxWithCreds wires a minimal sealed-credentials context for the
// helper. Either or both maps may be nil/empty.
func ctxWithCreds(t *testing.T, apiKeys map[secrets.Provider]string, oauthDirs map[string]string) context.Context {
	t.Helper()
	return secrets.WithCredentials(context.Background(), secrets.Credentials{
		APIKeys:              apiKeys,
		OAuthCredentialFiles: oauthDirs,
	})
}

// --- default precedence (no hint) ----------------------------------

func TestAnthropicCredEnv_AutoZAIFromCtxWinsOverAnthropic(t *testing.T) {
	resetClaudeCredEnv(t)
	ctx := ctxWithCreds(t, map[secrets.Provider]string{
		secrets.ProviderZAI:       "zai-test",
		secrets.ProviderAnthropic: "sk-anthropic-test",
	}, nil)
	got := anthropicCredEnvForCLI(ctx, "")
	if got["ANTHROPIC_BASE_URL"] != secrets.ZAIDefaultBaseURL {
		t.Fatalf("ANTHROPIC_BASE_URL: got %q, want %q", got["ANTHROPIC_BASE_URL"], secrets.ZAIDefaultBaseURL)
	}
	if got["ANTHROPIC_AUTH_TOKEN"] != "zai-test" {
		t.Errorf("ANTHROPIC_AUTH_TOKEN: got %q, want zai-test", got["ANTHROPIC_AUTH_TOKEN"])
	}
	if _, present := got["ANTHROPIC_API_KEY"]; present {
		t.Errorf("ANTHROPIC_API_KEY must NOT be set when z.ai key wins precedence")
	}
}

func TestAnthropicCredEnv_AutoAnthropicWhenNoZAI(t *testing.T) {
	resetClaudeCredEnv(t)
	ctx := ctxWithCreds(t, map[secrets.Provider]string{
		secrets.ProviderAnthropic: "sk-anthropic-test",
	}, nil)
	got := anthropicCredEnvForCLI(ctx, "")
	if got["ANTHROPIC_API_KEY"] != "sk-anthropic-test" {
		t.Errorf("ANTHROPIC_API_KEY: got %q, want sk-anthropic-test", got["ANTHROPIC_API_KEY"])
	}
}

func TestAnthropicCredEnv_AutoEnvFallbackZAI(t *testing.T) {
	resetClaudeCredEnv(t)
	t.Setenv("ZAI_API_KEY", "env-zai-test")
	got := anthropicCredEnvForCLI(context.Background(), "")
	if got["ANTHROPIC_AUTH_TOKEN"] != "env-zai-test" {
		t.Errorf("ANTHROPIC_AUTH_TOKEN: got %q, want env-zai-test", got["ANTHROPIC_AUTH_TOKEN"])
	}
	if got["ANTHROPIC_BASE_URL"] != secrets.ZAIDefaultBaseURL {
		t.Errorf("ANTHROPIC_BASE_URL: got %q, want default z.ai URL", got["ANTHROPIC_BASE_URL"])
	}
}

// --- hint: anthropic ------------------------------------------------

// TestAnthropicCredEnv_HintAnthropicSkipsZAIInCtx is THE motivating
// case for the provider feature: a node says "I need Anthropic's 1M
// context, route me there even though ZAI_API_KEY is set on the
// process and would otherwise win the precedence".
func TestAnthropicCredEnv_HintAnthropicSkipsZAIInCtx(t *testing.T) {
	resetClaudeCredEnv(t)
	ctx := ctxWithCreds(t, map[secrets.Provider]string{
		secrets.ProviderZAI:       "zai-test",
		secrets.ProviderAnthropic: "sk-anthropic-test",
	}, nil)
	got := anthropicCredEnvForCLI(ctx, "anthropic")
	if got["ANTHROPIC_API_KEY"] != "sk-anthropic-test" {
		t.Fatalf("ANTHROPIC_API_KEY: got %q, want sk-anthropic-test (hint must force this even with z.ai key present)", got["ANTHROPIC_API_KEY"])
	}
	// And critically, z.ai routing must NOT be wired.
	if got["ANTHROPIC_BASE_URL"] != "" {
		t.Errorf("ANTHROPIC_BASE_URL: got %q, want unset (hint anthropic must not route to z.ai)", got["ANTHROPIC_BASE_URL"])
	}
	if got["ANTHROPIC_AUTH_TOKEN"] != "" {
		t.Errorf("ANTHROPIC_AUTH_TOKEN: got %q, want unset", got["ANTHROPIC_AUTH_TOKEN"])
	}
}

func TestAnthropicCredEnv_HintAnthropicFallsToOAuthDir(t *testing.T) {
	resetClaudeCredEnv(t)
	ctx := ctxWithCreds(t, nil, map[string]string{
		string(secrets.OAuthKindClaudeCode): "/tmp/iterion-oauth-claude",
	})
	got := anthropicCredEnvForCLI(ctx, "anthropic")
	if got["CLAUDE_CONFIG_DIR"] != "/tmp/iterion-oauth-claude" {
		t.Errorf("CLAUDE_CONFIG_DIR: got %q, want /tmp/iterion-oauth-claude", got["CLAUDE_CONFIG_DIR"])
	}
}

func TestAnthropicCredEnv_HintAnthropicClearsStaleZAIEnv(t *testing.T) {
	resetClaudeCredEnv(t)
	// Simulate a stale parent-shell env where z.ai vars are already set
	// — the hint must actively unset them so the CLI subprocess inherits
	// only what we want.
	t.Setenv("ANTHROPIC_BASE_URL", "https://api.z.ai/api/anthropic")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "leftover-zai")
	got := anthropicCredEnvForCLI(context.Background(), "anthropic")
	if got["ANTHROPIC_BASE_URL"] != "" {
		t.Errorf("ANTHROPIC_BASE_URL: got %q, want '' (must clear stale value)", got["ANTHROPIC_BASE_URL"])
	}
	if got["ANTHROPIC_AUTH_TOKEN"] != "" {
		t.Errorf("ANTHROPIC_AUTH_TOKEN: got %q, want '' (must clear stale value)", got["ANTHROPIC_AUTH_TOKEN"])
	}
}

// --- hint: zai ------------------------------------------------------

func TestAnthropicCredEnv_HintZAIForcesEvenWithAnthropicCtx(t *testing.T) {
	resetClaudeCredEnv(t)
	ctx := ctxWithCreds(t, map[secrets.Provider]string{
		secrets.ProviderAnthropic: "sk-anthropic-test",
		secrets.ProviderZAI:       "zai-test",
	}, nil)
	got := anthropicCredEnvForCLI(ctx, "zai")
	if got["ANTHROPIC_AUTH_TOKEN"] != "zai-test" {
		t.Errorf("ANTHROPIC_AUTH_TOKEN: got %q, want zai-test (hint zai pins z.ai routing)", got["ANTHROPIC_AUTH_TOKEN"])
	}
	if _, present := got["ANTHROPIC_API_KEY"]; present {
		t.Errorf("ANTHROPIC_API_KEY must NOT be set when hint forces z.ai")
	}
}

// TestAnthropicCredEnv_HintZAIFallsToEnvKey ensures the hint also
// works when only the process env carries ZAI_API_KEY (the common
// desktop case).
func TestAnthropicCredEnv_HintZAIFallsToEnvKey(t *testing.T) {
	resetClaudeCredEnv(t)
	t.Setenv("ZAI_API_KEY", "env-zai-test")
	got := anthropicCredEnvForCLI(context.Background(), "zai")
	if got["ANTHROPIC_AUTH_TOKEN"] != "env-zai-test" {
		t.Errorf("ANTHROPIC_AUTH_TOKEN: got %q, want env-zai-test", got["ANTHROPIC_AUTH_TOKEN"])
	}
}

// --- provider fingerprint (cross-provider session fork guard) -------

func TestProviderFingerprint_FacadeBaseURL(t *testing.T) {
	got := providerFingerprint(map[string]string{
		"ANTHROPIC_BASE_URL":   "https://api.z.ai/api/anthropic",
		"ANTHROPIC_AUTH_TOKEN": "redacted",
	})
	want := "facade:https://api.z.ai/api/anthropic"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestProviderFingerprint_AnthropicDirect(t *testing.T) {
	got := providerFingerprint(map[string]string{"ANTHROPIC_API_KEY": "sk-..."})
	if got != "anthropic-direct" {
		t.Errorf("got %q, want anthropic-direct", got)
	}
}

func TestProviderFingerprint_AnthropicOAuth(t *testing.T) {
	got := providerFingerprint(map[string]string{"CLAUDE_CONFIG_DIR": "/some/dir"})
	if got != "anthropic-oauth" {
		t.Errorf("got %q, want anthropic-oauth", got)
	}
}

func TestProviderFingerprint_ClearedEnvIsAnthropic(t *testing.T) {
	// The providerHint==anthropic path actively clears BASE_URL +
	// AUTH_TOKEN so a stale z.ai value can't leak in. The fingerprint
	// must reflect "Anthropic-direct (env)" in that shape so a session
	// produced under it doesn't trigger a false cross-provider drop on
	// a follow-up node that lands in the same shape.
	got := providerFingerprint(map[string]string{
		"ANTHROPIC_BASE_URL":   "",
		"ANTHROPIC_AUTH_TOKEN": "",
	})
	if got != "anthropic-env" {
		t.Errorf("got %q, want anthropic-env", got)
	}
}

func TestProviderFingerprint_DifferentFacadesDiffer(t *testing.T) {
	// Two facades on different gateways must not collide — the parent
	// session signature won't validate on the other gateway either.
	a := providerFingerprint(map[string]string{"ANTHROPIC_BASE_URL": "https://api.z.ai/api/anthropic"})
	b := providerFingerprint(map[string]string{"ANTHROPIC_BASE_URL": "https://other.proxy/anthropic"})
	if a == b {
		t.Errorf("facades on different hosts collided: both %q", a)
	}
}

func TestProviderFingerprint_DirectVsFacadeDiffer(t *testing.T) {
	direct := providerFingerprint(map[string]string{"ANTHROPIC_API_KEY": "sk-..."})
	facade := providerFingerprint(map[string]string{
		"ANTHROPIC_BASE_URL":   "https://api.z.ai/api/anthropic",
		"ANTHROPIC_AUTH_TOKEN": "redacted",
	})
	if direct == facade {
		t.Errorf("anthropic-direct vs z.ai facade fingerprints collided: %q", direct)
	}
}
