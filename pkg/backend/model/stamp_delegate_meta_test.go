package model

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
)

// TestStampDelegateOutputMeta verifies that the per-node observability
// keys land on the output map only when the backend supplied them. The
// run-view's per-node model label and context-usage bar read from
// "_model" / "_context_window" / "_context_used" / "_max_output_tokens",
// so leaking zero-valued keys would mislead the UI (e.g. a 0/0 ratio
// rendering as 100% or NaN).
func TestStampDelegateOutputMeta(t *testing.T) {
	tests := []struct {
		name     string
		result   delegate.Result
		backend  string
		wantKeys map[string]any
		notKeys  []string
	}{
		{
			name: "claude_code with full model meta — all keys written",
			result: delegate.Result{
				Output:          map[string]any{},
				Tokens:          1500,
				SessionID:       "sess-abc",
				EffectiveModel:  "glm-4.6",
				ContextWindow:   200_000,
				PeakInputTokens: 120_000,
				MaxOutputTokens: 8_192,
			},
			backend: "claude_code",
			wantKeys: map[string]any{
				"_tokens":            1500,
				"_backend":           "claude_code",
				"_session_id":        "sess-abc",
				"_model":             "glm-4.6",
				"_context_window":    200_000,
				"_context_used":      120_000,
				"_max_output_tokens": 8_192,
			},
		},
		{
			name: "backend without model meta — only baseline keys written",
			result: delegate.Result{
				Output: map[string]any{},
				Tokens: 42,
			},
			backend:  "codex",
			wantKeys: map[string]any{"_tokens": 42, "_backend": "codex"},
			notKeys:  []string{"_session_id", "_model", "_context_window", "_context_used", "_max_output_tokens"},
		},
		{
			name: "pre-existing _tokens — not overwritten",
			result: delegate.Result{
				Output: map[string]any{"_tokens": 999},
				Tokens: 42,
			},
			backend:  "claude_code",
			wantKeys: map[string]any{"_tokens": 999, "_backend": "claude_code"},
		},
		{
			name: "effective model without window — model written, window skipped",
			result: delegate.Result{
				Output:         map[string]any{},
				EffectiveModel: "kimi-k2",
			},
			backend:  "claude_code",
			wantKeys: map[string]any{"_model": "kimi-k2", "_backend": "claude_code"},
			notKeys:  []string{"_context_window", "_context_used", "_max_output_tokens"},
		},
		{
			// ADR-087: `_backend` must name what SERVED, not what was
			// asked for. The two differ only after a fallback chain
			// crosses backends — and that is exactly the run where the
			// studio's backends_used chip and `iterion report`'s
			// per-step tag would otherwise assert a false fact.
			name: "backend reported by the delegate wins over the requested one",
			result: delegate.Result{
				Output:         map[string]any{},
				BackendName:    "claw",
				EffectiveModel: "openai/gpt-5.5",
			},
			backend:  "claude_code",
			wantKeys: map[string]any{"_backend": "claw", "_model": "openai/gpt-5.5"},
		},
		{
			name: "nil output map — no panic, no-op",
			result: delegate.Result{
				Output:         nil,
				EffectiveModel: "x",
			},
			backend: "claude_code",
			// No assertions: the helper should just return.
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			stampDelegateOutputMeta(tc.result.Output, tc.result, tc.backend)
			for k, want := range tc.wantKeys {
				got, ok := tc.result.Output[k]
				if !ok {
					t.Errorf("missing key %q", k)
					continue
				}
				if got != want {
					t.Errorf("key %q: got %v (%T), want %v (%T)", k, got, got, want, want)
				}
			}
			for _, k := range tc.notKeys {
				if _, ok := tc.result.Output[k]; ok {
					t.Errorf("unexpected key %q present", k)
				}
			}
		})
	}
}
