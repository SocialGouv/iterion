package runtime

import (
	"github.com/SocialGouv/iterion/pkg/claw/internal/api"
	"os"
	"path/filepath"
	"testing"
)

// clearProviderEnvs sets all provider-related env vars to empty for test isolation.
func clearProviderEnvs(t *testing.T) {
	t.Helper()
	t.Setenv("CLAUDE_CODE_USE_BEDROCK", "")
	t.Setenv("CLAUDE_CODE_USE_VERTEX", "")
	t.Setenv("CLAUDE_CODE_USE_FOUNDRY", "")
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	t.Setenv("XAI_API_KEY", "")
	t.Setenv("DASHSCOPE_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "")
}

func TestLoadConfigAllowImmediateStructuredOutput(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Chdir(dir)
	clearProviderEnvs(t)

	settingsDir := filepath.Join(dir, ".claude")
	if err := os.MkdirAll(settingsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	settings := []byte(`{"allowImmediateStructuredOutput": true}`)
	if err := os.WriteFile(filepath.Join(settingsDir, "settings.json"), settings, 0o644); err != nil {
		t.Fatal(err)
	}

	if cfg := LoadConfig(); !cfg.AllowImmediateStructuredOutput {
		t.Fatal("allowImmediateStructuredOutput setting was not loaded")
	}
}

func TestDetectProviderXAI(t *testing.T) {
	clearProviderEnvs(t)
	t.Setenv("XAI_API_KEY", "xai-test-key")

	if got := detectProvider(); got != "xai" {
		t.Errorf("detectProvider() = %q, want \"xai\"", got)
	}
}

func TestDetectProviderDashScope(t *testing.T) {
	clearProviderEnvs(t)
	t.Setenv("DASHSCOPE_API_KEY", "ds-test-key")

	if got := detectProvider(); got != "dashscope" {
		t.Errorf("detectProvider() = %q, want \"dashscope\"", got)
	}
}

func TestDetectProviderOpenAI(t *testing.T) {
	clearProviderEnvs(t)
	t.Setenv("OPENAI_API_KEY", "sk-openai")

	if got := detectProvider(); got != "openai" {
		t.Errorf("detectProvider() = %q, want \"openai\"", got)
	}
}

func TestDetectProviderPrecedence(t *testing.T) {
	t.Run("bedrock beats all", func(t *testing.T) {
		clearProviderEnvs(t)
		t.Setenv("CLAUDE_CODE_USE_BEDROCK", "1")
		t.Setenv("CLAUDE_CODE_USE_VERTEX", "1")
		t.Setenv("ANTHROPIC_API_KEY", "key")
		t.Setenv("XAI_API_KEY", "key")
		t.Setenv("DASHSCOPE_API_KEY", "key")
		t.Setenv("OPENAI_API_KEY", "key")
		if got := detectProvider(); got != "bedrock" {
			t.Errorf("got %q, want \"bedrock\"", got)
		}
	})

	t.Run("vertex beats foundry", func(t *testing.T) {
		clearProviderEnvs(t)
		t.Setenv("CLAUDE_CODE_USE_VERTEX", "1")
		t.Setenv("CLAUDE_CODE_USE_FOUNDRY", "1")
		t.Setenv("ANTHROPIC_API_KEY", "key")
		if got := detectProvider(); got != "vertex" {
			t.Errorf("got %q, want \"vertex\"", got)
		}
	})

	t.Run("anthropic beats xai", func(t *testing.T) {
		clearProviderEnvs(t)
		t.Setenv("ANTHROPIC_API_KEY", "key")
		t.Setenv("XAI_API_KEY", "key")
		t.Setenv("DASHSCOPE_API_KEY", "key")
		t.Setenv("OPENAI_API_KEY", "key")
		if got := detectProvider(); got != "anthropic" {
			t.Errorf("got %q, want \"anthropic\"", got)
		}
	})

	t.Run("xai beats dashscope", func(t *testing.T) {
		clearProviderEnvs(t)
		t.Setenv("XAI_API_KEY", "key")
		t.Setenv("DASHSCOPE_API_KEY", "key")
		t.Setenv("OPENAI_API_KEY", "key")
		if got := detectProvider(); got != "xai" {
			t.Errorf("got %q, want \"xai\"", got)
		}
	})

	t.Run("dashscope beats openai", func(t *testing.T) {
		clearProviderEnvs(t)
		t.Setenv("DASHSCOPE_API_KEY", "key")
		t.Setenv("OPENAI_API_KEY", "key")
		if got := detectProvider(); got != "dashscope" {
			t.Errorf("got %q, want \"dashscope\"", got)
		}
	})

	t.Run("default anthropic", func(t *testing.T) {
		clearProviderEnvs(t)
		if got := detectProvider(); got != "anthropic" {
			t.Errorf("got %q, want \"anthropic\"", got)
		}
	})
}

func TestXAIClientCreation(t *testing.T) {
	p := SelectProvider("xai")
	if _, err := p.NewClient(providerConfig("xai-key", "https://api.x.ai/v1", "grok-3", 8096)); err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
}

func TestDashScopeClientCreation(t *testing.T) {
	p := SelectProvider("dashscope")
	if _, err := p.NewClient(providerConfig("ds-key", "https://dashscope.aliyuncs.com/compatible-mode/v1", "qwen-max", 8096)); err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
}

func providerConfig(apiKey, baseURL, model string, maxTokens int) api.ProviderConfig {
	return api.ProviderConfig{
		APIKey:    apiKey,
		BaseURL:   baseURL,
		Model:     model,
		MaxTokens: maxTokens,
	}
}
