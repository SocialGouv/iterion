package apikit

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestResolveModelAliasRouting(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"opus", "claude-opus-4-6"},
		{"sonnet", "claude-sonnet-4-6"},
		{"haiku", "claude-haiku-4-5-20251213"},
		{"grok", "grok-3"},
		{"grok-3", "grok-3"},
		{"grok-mini", "grok-3-mini"},
		{"grok-3-mini", "grok-3-mini"},
		{"grok-2", "grok-2"},
		// DashScope aliases
		{"qwen", "qwen-max"},
		{"qwen-max", "qwen-max"},
		{"qwen-plus", "qwen-plus"},
		{"qwen-turbo", "qwen-turbo"},
		{"qwen-qwq-32b", "qwen-qwq-32b"},
		// Case insensitivity
		{"Opus", "claude-opus-4-6"},
		{"SONNET", "claude-sonnet-4-6"},
		{"Haiku", "claude-haiku-4-5-20251213"},
		{"GROK", "grok-3"},
		// Unknown model passes through
		{"my-custom-model", "my-custom-model"},
		{"claude-sonnet-4-6", "claude-sonnet-4-6"},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := ResolveModelAlias(tt.input)
			if got != tt.want {
				t.Errorf("ResolveModelAlias(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestMetadataForModel(t *testing.T) {
	tests := []struct {
		model    string
		wantNil  bool
		provider ProviderKind
		authEnv  string
		baseURL  string
	}{
		{"claude-sonnet-4-6", false, ProviderAnthropic, "ANTHROPIC_API_KEY", "https://api.anthropic.com"},
		{"claude-opus-4-6", false, ProviderAnthropic, "ANTHROPIC_API_KEY", "https://api.anthropic.com"},
		{"grok-3", false, ProviderXai, "XAI_API_KEY", "https://api.x.ai/v1"},
		{"grok-3-mini", false, ProviderXai, "XAI_API_KEY", "https://api.x.ai/v1"},
		{"openai/gpt-4", false, ProviderOpenAI, "OPENAI_API_KEY", "https://api.openai.com/v1"},
		{"gpt-4o", false, ProviderOpenAI, "OPENAI_API_KEY", "https://api.openai.com/v1"},
		{"qwen/qwen-max", false, ProviderDashScope, "DASHSCOPE_API_KEY", "https://dashscope.aliyuncs.com/compatible-mode/v1"},
		{"qwen-max", false, ProviderDashScope, "DASHSCOPE_API_KEY", "https://dashscope.aliyuncs.com/compatible-mode/v1"},
		{"qwen-plus", false, ProviderDashScope, "DASHSCOPE_API_KEY", "https://dashscope.aliyuncs.com/compatible-mode/v1"},
		{"qwen-turbo", false, ProviderDashScope, "DASHSCOPE_API_KEY", "https://dashscope.aliyuncs.com/compatible-mode/v1"},
		{"qwen-qwq-32b", false, ProviderDashScope, "DASHSCOPE_API_KEY", "https://dashscope.aliyuncs.com/compatible-mode/v1"},
		{"qwen", false, ProviderDashScope, "DASHSCOPE_API_KEY", "https://dashscope.aliyuncs.com/compatible-mode/v1"},
		// Aliases should be resolved before prefix matching (FIX-R1).
		{"opus", false, ProviderAnthropic, "ANTHROPIC_API_KEY", "https://api.anthropic.com"},
		{"sonnet", false, ProviderAnthropic, "ANTHROPIC_API_KEY", "https://api.anthropic.com"},
		{"haiku", false, ProviderAnthropic, "ANTHROPIC_API_KEY", "https://api.anthropic.com"},
		{"grok-mini", false, ProviderXai, "XAI_API_KEY", "https://api.x.ai/v1"},
		{"grok", false, ProviderXai, "XAI_API_KEY", "https://api.x.ai/v1"},
		{"unknown-model", true, "", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			meta := MetadataForModel(tt.model)
			if tt.wantNil {
				if meta != nil {
					t.Errorf("MetadataForModel(%q) = %+v, want nil", tt.model, meta)
				}
				return
			}
			if meta == nil {
				t.Fatalf("MetadataForModel(%q) = nil, want non-nil", tt.model)
			}
			if meta.Provider != tt.provider {
				t.Errorf("Provider = %q, want %q", meta.Provider, tt.provider)
			}
			if meta.AuthEnvVar != tt.authEnv {
				t.Errorf("AuthEnvVar = %q, want %q", meta.AuthEnvVar, tt.authEnv)
			}
			if meta.DefaultBaseURL != tt.baseURL {
				t.Errorf("DefaultBaseURL = %q, want %q", meta.DefaultBaseURL, tt.baseURL)
			}
		})
	}
}

func TestDetectProviderKind(t *testing.T) {
	t.Run("explicit prefix wins", func(t *testing.T) {
		got := DetectProviderKind("claude-sonnet-4-6")
		if got != ProviderAnthropic {
			t.Errorf("got %q, want %q", got, ProviderAnthropic)
		}
	})

	t.Run("explicit prefix grok", func(t *testing.T) {
		got := DetectProviderKind("grok-3")
		if got != ProviderXai {
			t.Errorf("got %q, want %q", got, ProviderXai)
		}
	})

	t.Run("fallback to ANTHROPIC_API_KEY", func(t *testing.T) {
		t.Setenv("ANTHROPIC_API_KEY", "sk-test")
		t.Setenv("OPENAI_API_KEY", "")
		t.Setenv("XAI_API_KEY", "")
		t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
		got := DetectProviderKind("unknown-model")
		if got != ProviderAnthropic {
			t.Errorf("got %q, want %q", got, ProviderAnthropic)
		}
	})

	t.Run("fallback to ANTHROPIC_AUTH_TOKEN", func(t *testing.T) {
		t.Setenv("ANTHROPIC_API_KEY", "")
		t.Setenv("ANTHROPIC_AUTH_TOKEN", "token-test")
		t.Setenv("OPENAI_API_KEY", "")
		t.Setenv("XAI_API_KEY", "")
		got := DetectProviderKind("unknown-model")
		if got != ProviderAnthropic {
			t.Errorf("got %q, want %q", got, ProviderAnthropic)
		}
	})

	t.Run("fallback to OPENAI_API_KEY", func(t *testing.T) {
		t.Setenv("ANTHROPIC_API_KEY", "")
		t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
		t.Setenv("OPENAI_API_KEY", "sk-openai")
		t.Setenv("XAI_API_KEY", "")
		got := DetectProviderKind("unknown-model")
		if got != ProviderOpenAI {
			t.Errorf("got %q, want %q", got, ProviderOpenAI)
		}
	})

	t.Run("fallback to XAI_API_KEY", func(t *testing.T) {
		t.Setenv("ANTHROPIC_API_KEY", "")
		t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
		t.Setenv("OPENAI_API_KEY", "")
		t.Setenv("XAI_API_KEY", "xai-test")
		got := DetectProviderKind("unknown-model")
		if got != ProviderXai {
			t.Errorf("got %q, want %q", got, ProviderXai)
		}
	})

	t.Run("fallback to DASHSCOPE_API_KEY", func(t *testing.T) {
		t.Setenv("ANTHROPIC_API_KEY", "")
		t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
		t.Setenv("OPENAI_API_KEY", "")
		t.Setenv("XAI_API_KEY", "")
		t.Setenv("DASHSCOPE_API_KEY", "ds-test")
		got := DetectProviderKind("unknown-model")
		if got != ProviderDashScope {
			t.Errorf("got %q, want %q", got, ProviderDashScope)
		}
	})

	t.Run("explicit prefix qwen", func(t *testing.T) {
		got := DetectProviderKind("qwen-max")
		if got != ProviderDashScope {
			t.Errorf("got %q, want %q", got, ProviderDashScope)
		}
	})

	t.Run("explicit prefix qwen/", func(t *testing.T) {
		got := DetectProviderKind("qwen/qwen-plus")
		if got != ProviderDashScope {
			t.Errorf("got %q, want %q", got, ProviderDashScope)
		}
	})

	t.Run("default to Anthropic when no env", func(t *testing.T) {
		t.Setenv("ANTHROPIC_API_KEY", "")
		t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
		t.Setenv("OPENAI_API_KEY", "")
		t.Setenv("XAI_API_KEY", "")
		t.Setenv("DASHSCOPE_API_KEY", "")
		got := DetectProviderKind("unknown-model")
		if got != ProviderAnthropic {
			t.Errorf("got %q, want %q", got, ProviderAnthropic)
		}
	})
}

func TestMaxTokensForModel(t *testing.T) {
	tests := []struct {
		model string
		want  uint32
	}{
		{"claude-opus-4-6", 32_000},
		{"claude-sonnet-4-6", 64_000},
		{"claude-haiku-4-5-20251213", 64_000},
		{"grok-3", 64_000},
		{"grok-3-mini", 64_000},
		// Aliases
		{"opus", 32_000},
		{"sonnet", 64_000},
		{"haiku", 64_000},
		{"grok", 64_000},
		// Unknown models use heuristic: opus → 32k, others → 64k
		{"unknown-model", 64_000},
		{"my-opus-variant", 32_000}, // contains "opus"
	}
	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			got := MaxTokensForModel(tt.model)
			if got != tt.want {
				t.Errorf("MaxTokensForModel(%q) = %d, want %d", tt.model, got, tt.want)
			}
		})
	}
}

func TestResolveModelAliasGoldenFixtures(t *testing.T) {
	type fixture struct {
		Alias     string `json:"alias"`
		Canonical string `json:"canonical"`
	}

	data, err := os.ReadFile(filepath.Join("..", "..", "testdata", "golden", "model_aliases.json"))
	if err != nil {
		t.Fatalf("read golden file: %v", err)
	}

	var fixtures []fixture
	if err := json.Unmarshal(data, &fixtures); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	for _, f := range fixtures {
		t.Run(f.Alias, func(t *testing.T) {
			got := ResolveModelAlias(f.Alias)
			if got != f.Canonical {
				t.Errorf("ResolveModelAlias(%q) = %q, want %q", f.Alias, got, f.Canonical)
			}
		})
	}
}

// TestMetadataForModelPrefixFallback verifies that Go-only providers (Bedrock/Vertex/Foundry)
// that route models via prefix detection still work correctly even though they are
// not in the registry.
func TestMetadataForModelPrefixFallback(t *testing.T) {
	tests := []struct {
		model    string
		provider ProviderKind
		authEnv  string
		baseURL  string
	}{
		// OpenAI prefix models (not individually registered)
		{"openai/gpt-4", ProviderOpenAI, "OPENAI_API_KEY", "https://api.openai.com/v1"},
		{"openai/gpt-4o-mini", ProviderOpenAI, "OPENAI_API_KEY", "https://api.openai.com/v1"},
		{"gpt-4o", ProviderOpenAI, "OPENAI_API_KEY", "https://api.openai.com/v1"},
		// Qwen/DashScope prefix models
		{"qwen/qwen-max", ProviderDashScope, "DASHSCOPE_API_KEY", "https://dashscope.aliyuncs.com/compatible-mode/v1"},
		{"qwen-turbo", ProviderDashScope, "DASHSCOPE_API_KEY", "https://dashscope.aliyuncs.com/compatible-mode/v1"},
		{"qwen/qwen-plus", ProviderDashScope, "DASHSCOPE_API_KEY", "https://dashscope.aliyuncs.com/compatible-mode/v1"},
		// Claude prefix models not in registry (hypothetical future variants)
		{"claude-next-gen-v1", ProviderAnthropic, "ANTHROPIC_API_KEY", "https://api.anthropic.com"},
		// Grok prefix models not in registry
		{"grok-4-preview", ProviderXai, "XAI_API_KEY", "https://api.x.ai/v1"},
	}
	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			meta := MetadataForModel(tt.model)
			if meta == nil {
				t.Fatalf("MetadataForModel(%q) = nil, want non-nil (prefix fallback)", tt.model)
			}
			if meta.Provider != tt.provider {
				t.Errorf("Provider = %q, want %q", meta.Provider, tt.provider)
			}
			if meta.AuthEnvVar != tt.authEnv {
				t.Errorf("AuthEnvVar = %q, want %q", meta.AuthEnvVar, tt.authEnv)
			}
			if meta.DefaultBaseURL != tt.baseURL {
				t.Errorf("DefaultBaseURL = %q, want %q", meta.DefaultBaseURL, tt.baseURL)
			}
		})
	}
}

// TestMetadataForModelRegistryLookup verifies that registry-registered models
// are found via MetadataForModel and return registry metadata.
func TestMetadataForModelRegistryLookup(t *testing.T) {
	tests := []struct {
		model    string
		provider ProviderKind
	}{
		{"claude-opus-4-6", ProviderAnthropic},
		{"claude-sonnet-4-6", ProviderAnthropic},
		{"claude-haiku-4-5-20251213", ProviderAnthropic},
		{"grok-3", ProviderXai},
		{"grok-3-mini", ProviderXai},
		{"grok-2", ProviderXai},
		// Via aliases
		{"opus", ProviderAnthropic},
		{"sonnet", ProviderAnthropic},
		{"grok", ProviderXai},
	}
	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			meta := MetadataForModel(tt.model)
			if meta == nil {
				t.Fatalf("MetadataForModel(%q) = nil, want non-nil", tt.model)
			}
			if meta.Provider != tt.provider {
				t.Errorf("Provider = %q, want %q", meta.Provider, tt.provider)
			}
		})
	}
}

// TestMetadataForModelRuntimeRegistered verifies that models registered at
// runtime via RegisterModel are picked up by MetadataForModel.
func TestMetadataForModelRuntimeRegistered(t *testing.T) {
	reg := DefaultModelRegistry()
	reg.RegisterModel(ModelEntry{
		Canonical:     "my-custom-llm-v1",
		Provider:      ProviderOpenAI,
		MaxOutput:     16_000,
		ContextWindow: 128_000,
		Aliases:       []string{"custom-llm"},
		Metadata: &ProviderMetadata{
			Provider:       ProviderOpenAI,
			AuthEnvVar:     "CUSTOM_LLM_KEY",
			BaseURLEnvVar:  "CUSTOM_LLM_BASE_URL",
			DefaultBaseURL: "https://custom-llm.example.com/v1",
		},
	})

	// Lookup by canonical name
	meta := MetadataForModel("my-custom-llm-v1")
	if meta == nil {
		t.Fatal("MetadataForModel(\"my-custom-llm-v1\") = nil after RegisterModel")
	}
	if meta.AuthEnvVar != "CUSTOM_LLM_KEY" {
		t.Errorf("AuthEnvVar = %q, want CUSTOM_LLM_KEY", meta.AuthEnvVar)
	}

	// Lookup by alias
	meta = MetadataForModel("custom-llm")
	if meta == nil {
		t.Fatal("MetadataForModel(\"custom-llm\") = nil after RegisterModel")
	}
	if meta.Provider != ProviderOpenAI {
		t.Errorf("Provider = %q, want %q", meta.Provider, ProviderOpenAI)
	}
}

// TestModelTokenLimitRuntimeRegistered verifies that ModelTokenLimitForModel
// picks up runtime-registered models from the registry.
func TestModelTokenLimitRuntimeRegistered(t *testing.T) {
	reg := DefaultModelRegistry()
	reg.RegisterModel(ModelEntry{
		Canonical:     "runtime-model-v2",
		Provider:      ProviderOpenAI,
		MaxOutput:     8_000,
		ContextWindow: 64_000,
		Aliases:       []string{"rtm2"},
	})

	limit := ModelTokenLimitForModel("runtime-model-v2")
	if limit == nil {
		t.Fatal("expected non-nil limit for runtime-registered model")
	}
	if limit.MaxOutputTokens != 8_000 {
		t.Errorf("MaxOutputTokens = %d, want 8000", limit.MaxOutputTokens)
	}
	if limit.ContextWindowTokens != 64_000 {
		t.Errorf("ContextWindowTokens = %d, want 64000", limit.ContextWindowTokens)
	}

	// Also via alias
	limit = ModelTokenLimitForModel("rtm2")
	if limit == nil {
		t.Fatal("expected non-nil limit for runtime-registered model via alias")
	}
	if limit.MaxOutputTokens != 8_000 {
		t.Errorf("MaxOutputTokens = %d, want 8000", limit.MaxOutputTokens)
	}
}

// TestResolveModelAliasDelegatesToRegistry verifies that ResolveModelAlias
// now delegates to the registry and handles runtime-registered aliases.
func TestResolveModelAliasDelegatesToRegistry(t *testing.T) {
	reg := DefaultModelRegistry()
	reg.RegisterModel(ModelEntry{
		Canonical:     "delegated-model-v1",
		Provider:      ProviderAnthropic,
		MaxOutput:     16_000,
		ContextWindow: 200_000,
		Aliases:       []string{"dm1"},
	})

	got := ResolveModelAlias("dm1")
	if got != "delegated-model-v1" {
		t.Errorf("ResolveModelAlias(\"dm1\") = %q, want \"delegated-model-v1\"", got)
	}

	// Unknown still passes through
	got = ResolveModelAlias("totally-unknown")
	if got != "totally-unknown" {
		t.Errorf("ResolveModelAlias(\"totally-unknown\") = %q, want \"totally-unknown\"", got)
	}
}

func TestLookupModelTokenLimit(t *testing.T) {
	tests := []struct {
		model       string
		wantNil     bool
		wantOutput  uint32
		wantContext uint32
	}{
		{"claude-opus-4-6", false, 32000, 200000},
		{"claude-sonnet-4-6", false, 64000, 200000},
		{"claude-haiku-4-5-20251213", false, 64000, 200000},
		{"grok-3", false, 64000, 131072},
		{"grok-3-mini", false, 64000, 131072},
		{"unknown-model", true, 0, 0},
	}
	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			got := LookupModelTokenLimit(tt.model)
			if tt.wantNil {
				if got != nil {
					t.Errorf("LookupModelTokenLimit(%q) = %+v, want nil", tt.model, got)
				}
				return
			}
			if got == nil {
				t.Fatalf("LookupModelTokenLimit(%q) = nil, want non-nil", tt.model)
			}
			if got.MaxOutputTokens != tt.wantOutput {
				t.Errorf("MaxOutputTokens = %d, want %d", got.MaxOutputTokens, tt.wantOutput)
			}
			if got.ContextWindowTokens != tt.wantContext {
				t.Errorf("ContextWindowTokens = %d, want %d", got.ContextWindowTokens, tt.wantContext)
			}
		})
	}
}
