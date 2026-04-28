package apikit

import (
	"fmt"
	"testing"
)

func TestModelRegistryLookup(t *testing.T) {
	reg := &ModelRegistry{}

	tests := []struct {
		name      string
		wantNil   bool
		canonical string
		maxOutput uint32
	}{
		{"opus", false, "claude-opus-4-6", 32_000},
		{"sonnet", false, "claude-sonnet-4-6", 64_000},
		{"haiku", false, "claude-haiku-4-5-20251213", 64_000},
		{"grok", false, "grok-3", 64_000},
		{"grok-mini", false, "grok-3-mini", 64_000},
		{"grok-2", false, "grok-2", 0},
		{"qwen-max", false, "qwen-max", 0},
		{"qwen-plus", false, "qwen-plus", 0},
		{"qwen-turbo", false, "qwen-turbo", 0},
		{"qwen-qwq-32b", false, "qwen-qwq-32b", 0},
		{"qwen", false, "qwen-max", 0},
		{"claude-opus-4-6", false, "claude-opus-4-6", 32_000},
		{"OPUS", false, "claude-opus-4-6", 32_000},
		{"unknown-model", true, "", 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			entry := reg.LookupModel(tt.name)
			if tt.wantNil {
				if entry != nil {
					t.Errorf("LookupModel(%q) = %+v, want nil", tt.name, entry)
				}
				return
			}
			if entry == nil {
				t.Fatalf("LookupModel(%q) = nil, want non-nil", tt.name)
			}
			if entry.Canonical != tt.canonical {
				t.Errorf("Canonical = %q, want %q", entry.Canonical, tt.canonical)
			}
			if entry.MaxOutput != tt.maxOutput {
				t.Errorf("MaxOutput = %d, want %d", entry.MaxOutput, tt.maxOutput)
			}
		})
	}
}

func TestModelRegistryResolveAlias(t *testing.T) {
	reg := &ModelRegistry{}

	tests := []struct {
		input string
		want  string
	}{
		{"opus", "claude-opus-4-6"},
		{"sonnet", "claude-sonnet-4-6"},
		{"haiku", "claude-haiku-4-5-20251213"},
		{"grok", "grok-3"},
		{"grok-mini", "grok-3-mini"},
		{"grok-2", "grok-2"},
		{"qwen", "qwen-max"},
		{"qwen-max", "qwen-max"},
		{"qwen-plus", "qwen-plus"},
		{"qwen-turbo", "qwen-turbo"},
		{"qwen-qwq-32b", "qwen-qwq-32b"},
		{"claude-sonnet-4-6", "claude-sonnet-4-6"},
		{"unknown", "unknown"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := reg.ResolveAlias(tt.input)
			if got != tt.want {
				t.Errorf("ResolveAlias(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestModelRegistryProviderDetection(t *testing.T) {
	reg := &ModelRegistry{}

	tests := []struct {
		model    string
		provider ProviderKind
	}{
		{"opus", ProviderAnthropic},
		{"sonnet", ProviderAnthropic},
		{"grok", ProviderXai},
		{"grok-mini", ProviderXai},
		{"qwen", ProviderDashScope},
		{"qwen-max", ProviderDashScope},
		{"qwen-turbo", ProviderDashScope},
	}

	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			entry := reg.LookupModel(tt.model)
			if entry == nil {
				t.Fatalf("LookupModel(%q) = nil", tt.model)
			}
			if entry.Provider != tt.provider {
				t.Errorf("Provider = %q, want %q", entry.Provider, tt.provider)
			}
		})
	}
}

func TestModelRegistryRegisterModel(t *testing.T) {
	reg := &ModelRegistry{}

	// Register a new model at runtime.
	reg.RegisterModel(ModelEntry{
		Canonical:     "custom-model-v1",
		Provider:      ProviderOpenAI,
		MaxOutput:     8_000,
		ContextWindow: 128_000,
		Aliases:       []string{"custom"},
	})

	entry := reg.LookupModel("custom")
	if entry == nil {
		t.Fatal("expected to find registered model via alias")
	}
	if entry.Canonical != "custom-model-v1" {
		t.Errorf("Canonical = %q, want 'custom-model-v1'", entry.Canonical)
	}
	if entry.MaxOutput != 8_000 {
		t.Errorf("MaxOutput = %d, want 8000", entry.MaxOutput)
	}

	// Also findable by canonical name.
	entry = reg.LookupModel("custom-model-v1")
	if entry == nil {
		t.Fatal("expected to find registered model via canonical name")
	}
}

func TestModelRegistryConsistencyWithPreflight(t *testing.T) {
	// Verify the registry produces the same results as the existing
	// hardcoded functions for all known models.
	reg := &ModelRegistry{}

	models := []string{"opus", "sonnet", "haiku", "grok", "grok-mini", "grok-2", "qwen", "qwen-max", "qwen-turbo"}
	for _, model := range models {
		canonical := ResolveModelAlias(model)
		regCanonical := reg.ResolveAlias(model)
		if canonical != regCanonical {
			t.Errorf("model %q: ResolveModelAlias=%q, registry=%q", model, canonical, regCanonical)
		}

		limit := ModelTokenLimitForModel(model)
		entry := reg.LookupModel(model)
		if limit != nil && entry != nil {
			if limit.MaxOutputTokens != entry.MaxOutput {
				t.Errorf("model %q: MaxOutput preflight=%d, registry=%d", model, limit.MaxOutputTokens, entry.MaxOutput)
			}
			if limit.ContextWindowTokens != entry.ContextWindow {
				t.Errorf("model %q: ContextWindow preflight=%d, registry=%d", model, limit.ContextWindowTokens, entry.ContextWindow)
			}
		}
	}
}

func TestDefaultModelRegistrySingleton(t *testing.T) {
	r1 := DefaultModelRegistry()
	r2 := DefaultModelRegistry()
	if r1 != r2 {
		t.Error("DefaultModelRegistry should return the same singleton")
	}
}

func TestModelRegistryConcurrentAccess(t *testing.T) {
	// Verify the RWMutex-based registry is safe under concurrent read+write.
	reg := &ModelRegistry{}

	const goroutines = 50
	done := make(chan struct{})

	// Half the goroutines read, half write.
	for i := 0; i < goroutines; i++ {
		go func(idx int) {
			defer func() { done <- struct{}{} }()
			if idx%2 == 0 {
				// Reader: lookup known and unknown models.
				_ = reg.LookupModel("opus")
				_ = reg.ResolveAlias("sonnet")
				_ = reg.LookupModel("unknown-model")
			} else {
				// Writer: register a new model.
				reg.RegisterModel(ModelEntry{
					Canonical:     fmt.Sprintf("test-model-%d", idx),
					Provider:      ProviderOpenAI,
					MaxOutput:     8_000,
					ContextWindow: 128_000,
					Aliases:       []string{fmt.Sprintf("tm%d", idx)},
				})
			}
		}(i)
	}

	for i := 0; i < goroutines; i++ {
		<-done
	}

	// Verify at least one registered model is findable.
	entry := reg.LookupModel("tm1")
	if entry == nil {
		t.Error("expected to find concurrently registered model")
	}
}

func TestModelRegistryRegisterAndLookupConcurrently(t *testing.T) {
	// Stress test: concurrent RegisterModel and LookupModel must not panic.
	reg := &ModelRegistry{}

	const N = 100
	done := make(chan struct{}, N*2)

	for i := 0; i < N; i++ {
		go func(idx int) {
			defer func() { done <- struct{}{} }()
			reg.RegisterModel(ModelEntry{
				Canonical:     fmt.Sprintf("concurrent-%d", idx),
				Provider:      ProviderAnthropic,
				MaxOutput:     16_000,
				ContextWindow: 200_000,
				Aliases:       []string{fmt.Sprintf("c%d", idx)},
			})
		}(i)
		go func(idx int) {
			defer func() { done <- struct{}{} }()
			_ = reg.LookupModel(fmt.Sprintf("c%d", idx))
			_ = reg.ResolveAlias(fmt.Sprintf("concurrent-%d", idx))
		}(i)
	}

	for i := 0; i < N*2; i++ {
		<-done
	}
}
