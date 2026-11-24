package runtime

import "testing"

func TestDefaultCompactionConfigForModel_KnownModel(t *testing.T) {
	// Opus 4.7 has a 1M context window per the embed registry.
	cfg := DefaultCompactionConfigForModel("claude-opus-4-7", 0, 0)
	want := int(float64(1_000_000) * DefaultCompactionThresholdRatio)
	if cfg.MaxEstimatedTokens != want {
		t.Errorf("MaxEstimatedTokens: got %d, want %d", cfg.MaxEstimatedTokens, want)
	}
	if cfg.PreserveRecentMessages != DefaultCompactionPreserveRecent {
		t.Errorf("PreserveRecentMessages: got %d, want %d", cfg.PreserveRecentMessages, DefaultCompactionPreserveRecent)
	}
}

func TestDefaultCompactionConfigForModel_GPT55(t *testing.T) {
	// GPT-5.5 ships 1.05M context per upstream sources.
	cfg := DefaultCompactionConfigForModel("gpt-5.5", 0, 0)
	want := int(float64(1_050_000) * DefaultCompactionThresholdRatio)
	if cfg.MaxEstimatedTokens != want {
		t.Errorf("MaxEstimatedTokens: got %d, want %d", cfg.MaxEstimatedTokens, want)
	}
}

func TestDefaultCompactionConfigForModel_UnknownModel(t *testing.T) {
	// Falls back to the legacy 10k threshold. We don't pin the exact value —
	// just assert it's far below any registered model's window so callers
	// can detect "unknown model" via behavior.
	cfg := DefaultCompactionConfigForModel("totally-unknown-model-xyz", 0, 0)
	if cfg.MaxEstimatedTokens >= 100_000 {
		t.Errorf("expected legacy fallback threshold, got %d", cfg.MaxEstimatedTokens)
	}
}

func TestDefaultCompactionConfigForModel_RatioOverride(t *testing.T) {
	cfg := DefaultCompactionConfigForModel("claude-opus-4-7", 0.95, 0)
	want := int(float64(1_000_000) * 0.95)
	if cfg.MaxEstimatedTokens != want {
		t.Errorf("MaxEstimatedTokens with ratio override: got %d, want %d", cfg.MaxEstimatedTokens, want)
	}
}

func TestDefaultCompactionConfigForModel_PreserveRecentOverride(t *testing.T) {
	cfg := DefaultCompactionConfigForModel("claude-opus-4-7", 0, 12)
	if cfg.PreserveRecentMessages != 12 {
		t.Errorf("PreserveRecentMessages: got %d, want 12", cfg.PreserveRecentMessages)
	}
}

func TestDefaultCompactionConfigForModel_ZeroRatioUsesDefault(t *testing.T) {
	cfg := DefaultCompactionConfigForModel("claude-opus-4-7", 0, 0)
	want := int(float64(1_000_000) * DefaultCompactionThresholdRatio)
	if cfg.MaxEstimatedTokens != want {
		t.Errorf("zero ratio should use default 0.85: got %d, want %d", cfg.MaxEstimatedTokens, want)
	}
}

func TestDefaultCompactionConfigForModel_EmptyModelUsesLegacy(t *testing.T) {
	cfg := DefaultCompactionConfigForModel("", 0, 0)
	if cfg.MaxEstimatedTokens >= 100_000 {
		t.Errorf("empty model should use legacy threshold, got %d", cfg.MaxEstimatedTokens)
	}
}

func TestDefaultCompactionConfigForModel_AliasResolves(t *testing.T) {
	// "opus" alias should resolve to claude-opus-4-7 (1M window).
	cfg := DefaultCompactionConfigForModel("opus", 0, 0)
	want := int(float64(1_000_000) * DefaultCompactionThresholdRatio)
	if cfg.MaxEstimatedTokens != want {
		t.Errorf("alias resolution failed: got %d, want %d", cfg.MaxEstimatedTokens, want)
	}
}
