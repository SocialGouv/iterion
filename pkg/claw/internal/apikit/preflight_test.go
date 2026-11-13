package apikit

import (
	"errors"
	"math"
	"strings"
	"testing"
)

func TestModelTokenLimitsKnownModels(t *testing.T) {
	tests := []struct {
		model         string
		maxOutput     uint32
		contextWindow uint32
	}{
		{"claude-opus-4-8", 128_000, 1_000_000},
		{"claude-opus-4-7", 128_000, 1_000_000},
		{"claude-opus-4-6", 128_000, 1_000_000},
		{"claude-sonnet-4-7", 128_000, 1_000_000},
		{"claude-sonnet-4-6", 128_000, 1_000_000},
		{"claude-haiku-4-5", 64_000, 200_000},
		{"claude-haiku-4-5-20251213", 64_000, 200_000},
		{"gpt-5.5", 128_000, 1_050_000},
		{"openai/gpt-5.5", 128_000, 1_050_000},
		{"grok-3", 64_000, 131_072},
		{"grok-3-mini", 64_000, 131_072},
	}
	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			limit := ModelTokenLimitForModel(tt.model)
			if limit == nil {
				t.Fatal("expected non-nil limit")
			}
			if limit.MaxOutputTokens != tt.maxOutput {
				t.Errorf("MaxOutputTokens: got %d, want %d", limit.MaxOutputTokens, tt.maxOutput)
			}
			if limit.ContextWindowTokens != tt.contextWindow {
				t.Errorf("ContextWindowTokens: got %d, want %d", limit.ContextWindowTokens, tt.contextWindow)
			}
		})
	}
}

func TestModelTokenLimitUnknownModelReturnsNil(t *testing.T) {
	limit := ModelTokenLimitForModel("unknown-model-v99")
	if limit != nil {
		t.Error("unknown model should return nil")
	}
}

func TestPreflightCheckPassesForUnknownModel(t *testing.T) {
	err := PreflightCheck("unknown-model", 999_999, 999_999)
	if err != nil {
		t.Errorf("unknown model should pass through, got: %v", err)
	}
}

func TestPreflightCheckPassesWithinLimit(t *testing.T) {
	// claude-opus-4-6: 1M context window
	err := PreflightCheck("claude-opus-4-6", 800_000, 128_000)
	if err != nil {
		t.Errorf("within-limit request should pass, got: %v", err)
	}
}

func TestPreflightCheckFailsExceedingLimit(t *testing.T) {
	// claude-opus-4-6: 1M context window, 900k input + 128k output = 1_028k > 1M
	err := PreflightCheck("claude-opus-4-6", 900_000, 128_000)
	if err == nil {
		t.Fatal("expected ContextWindowExceeded error")
	}

	var apiErr *ApiError
	if !errors.As(err, &apiErr) {
		t.Fatal("expected ApiError")
	}
	if apiErr.Kind != ErrContextWindowExceeded {
		t.Errorf("expected ErrContextWindowExceeded, got %d", apiErr.Kind)
	}
	if apiErr.Model != "claude-opus-4-6" {
		t.Errorf("expected model claude-opus-4-6, got %s", apiErr.Model)
	}
	if apiErr.EstimatedInputTokens != 900_000 {
		t.Errorf("expected 900000 input tokens, got %d", apiErr.EstimatedInputTokens)
	}
	if apiErr.RequestedOutputTokens != 128_000 {
		t.Errorf("expected 128000 output tokens, got %d", apiErr.RequestedOutputTokens)
	}
	if apiErr.ContextWindowTokens != 1_000_000 {
		t.Errorf("expected 1000000 context window, got %d", apiErr.ContextWindowTokens)
	}
}

func TestPreflightCheckExactBoundary(t *testing.T) {
	// Exactly at the limit should pass: 1M total = 872k input + 128k output
	err := PreflightCheck("claude-opus-4-6", 872_000, 128_000)
	if err != nil {
		t.Errorf("exact boundary should pass, got: %v", err)
	}

	// One over should fail
	err = PreflightCheck("claude-opus-4-6", 872_001, 128_000)
	if err == nil {
		t.Error("one over boundary should fail")
	}
}

func TestResolveModelAlias(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"opus", "claude-opus-5-5"},
		{"Sonnet", "claude-sonnet-5"},
		{"HAIKU", "claude-haiku-4-5"},
		{"grok", "grok-3"},
		{"grok-3", "grok-3"},
		{"grok-mini", "grok-3-mini"},
		{"grok-3-mini", "grok-3-mini"},
		{"grok-2", "grok-2"},
		{"unknown-model", "unknown-model"},
		{"  opus  ", "claude-opus-5-5"},
		{"claude-opus-4-6", "claude-opus-4-6"},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := ResolveModelAlias(tt.input)
			if got != tt.expected {
				t.Errorf("ResolveModelAlias(%q) = %q, want %q", tt.input, got, tt.expected)
			}
		})
	}
}

func TestPreflightCheckWithAlias(t *testing.T) {
	// "opus" resolves to claude-opus-5-5 (1M context window)
	// 900k input + 128k output = 1_028k > 1M → should fail
	err := PreflightCheck("opus", 900_000, 128_000)
	if err == nil {
		t.Fatal("expected ContextWindowExceeded for alias 'opus', got nil")
	}
	var apiErr *ApiError
	if !errors.As(err, &apiErr) {
		t.Fatal("expected ApiError")
	}
	if apiErr.Kind != ErrContextWindowExceeded {
		t.Errorf("expected ErrContextWindowExceeded, got %d", apiErr.Kind)
	}
	if apiErr.Model != "claude-opus-5-5" {
		t.Errorf("expected resolved model 'claude-opus-5-5', got %q", apiErr.Model)
	}
}

func TestPreflightSaturatingOverflow(t *testing.T) {
	// math.MaxUint32-50 + 100 would wrap to 49 without saturating add.
	// With saturating add it should cap at MaxUint32, which exceeds any context window.
	err := PreflightCheck("claude-opus-4-6", math.MaxUint32-50, 100)
	if err == nil {
		t.Fatal("expected ContextWindowExceeded on uint32 overflow, got nil")
	}
	var apiErr *ApiError
	if !errors.As(err, &apiErr) {
		t.Fatal("expected ApiError")
	}
	if apiErr.Kind != ErrContextWindowExceeded {
		t.Errorf("expected ErrContextWindowExceeded, got %d", apiErr.Kind)
	}
	if apiErr.EstimatedTotalTokens != math.MaxUint32 {
		t.Errorf("expected saturated total %d, got %d", uint32(math.MaxUint32), apiErr.EstimatedTotalTokens)
	}
}

func TestMaxTokensForModelWithOverride(t *testing.T) {
	// Without override: uses model default (opus → claude-opus-4-8, 128k output).
	got := MaxTokensForModelWithOverride("opus", nil)
	if got != 128_000 {
		t.Errorf("without override: got %d, want 128000", got)
	}

	// With override: prefers plugin value.
	override := uint32(16_000)
	got = MaxTokensForModelWithOverride("opus", &override)
	if got != 16_000 {
		t.Errorf("with override: got %d, want 16000", got)
	}

	// Override on unknown model.
	override = uint32(4_096)
	got = MaxTokensForModelWithOverride("unknown-model", &override)
	if got != 4_096 {
		t.Errorf("override on unknown: got %d, want 4096", got)
	}

	// Nil override on unknown model falls back to 64k.
	got = MaxTokensForModelWithOverride("unknown-model", nil)
	if got != 64_000 {
		t.Errorf("nil override on unknown: got %d, want 64000", got)
	}
}

func TestPreflightMessageRequest(t *testing.T) {
	t.Run("oversized request rejected", func(t *testing.T) {
		// Build a large messages slice that will serialize to enough bytes
		// to exceed claude-opus-4-6's 1M context window.
		// 1M tokens ≈ 4M bytes of JSON. Build ~4.5M to be safe.
		bigContent := strings.Repeat("x", 4_500_000)
		messages := []map[string]string{
			{"role": "user", "content": bigContent},
		}
		err := PreflightMessageRequest("claude-opus-4-6", messages, 128_000)
		if err == nil {
			t.Fatal("expected error for oversized request")
		}
		var apiErr *ApiError
		if !errors.As(err, &apiErr) {
			t.Fatal("expected ApiError")
		}
		if apiErr.Kind != ErrContextWindowExceeded {
			t.Errorf("expected ErrContextWindowExceeded, got %d", apiErr.Kind)
		}
	})

	t.Run("within limit passes", func(t *testing.T) {
		messages := []map[string]string{
			{"role": "user", "content": "Hello"},
		}
		err := PreflightMessageRequest("claude-opus-4-6", messages, 8096)
		if err != nil {
			t.Errorf("expected nil, got: %v", err)
		}
	})

	t.Run("unknown model passes through", func(t *testing.T) {
		bigContent := strings.Repeat("x", 900_000)
		messages := []map[string]string{
			{"role": "user", "content": bigContent},
		}
		err := PreflightMessageRequest("unknown-model-xyz", messages, 999_999)
		if err != nil {
			t.Errorf("unknown model should pass through, got: %v", err)
		}
	})
}

func TestEstimateSerializedTokens(t *testing.T) {
	// Simple string
	tokens := EstimateSerializedTokens("hello world")
	if tokens == 0 {
		t.Error("should estimate non-zero tokens")
	}

	// Larger object should produce more tokens
	small := EstimateSerializedTokens("hi")
	large := EstimateSerializedTokens(map[string]any{
		"messages": []map[string]string{
			{"role": "user", "content": "This is a much longer message that should produce more tokens"},
		},
	})
	if large <= small {
		t.Errorf("larger object should estimate more tokens: small=%d, large=%d", small, large)
	}
}
