package delegate

import (
	"errors"
	"fmt"
	"testing"
)

// TestClassifyFallback pins the vocabulary a `fallbacks: … on: […]`
// filter is written against. The table is the contract: a category that
// silently changes meaning re-routes runs the author never intended to
// re-route.
func TestClassifyFallback(t *testing.T) {
	tests := []struct {
		name      string
		err       error
		retryable bool
		want      FallbackCategory
	}{
		{
			name: "forfait window — the archetypal reason to route elsewhere",
			err:  &ErrRateLimited{Provider: BackendClaudeCode, Kind: RateLimitKindUsageWindow},
			want: FallbackUsageWindow,
		},
		{
			name: "explicit transient rate limit is NOT a usage window",
			err:  &ErrRateLimited{Provider: BackendClaudeCode, Kind: RateLimitKindTransient},
			want: FallbackTransientExhausted,
		},
		{
			// delegate.go documents empty Kind as "unclassified (legacy),
			// treated as transient" — codex constructs exactly this shape.
			// Classifying it as a usage window would park a run on a
			// condition waiting cannot cure.
			name: "empty Kind is transient, never a usage window",
			err:  &ErrRateLimited{Provider: BackendCodex},
			want: FallbackTransientExhausted,
		},
		{
			name: "dead credential",
			err:  &ErrAuthFailed{Provider: BackendClaudeCode, Detail: "invalid bearer token"},
			want: FallbackAuth,
		},
		{
			name: "model the credential cannot reach",
			err:  &ErrModelUnavailable{Provider: BackendClaudeCode, Model: "openai/gpt-5.5"},
			want: FallbackUnavailable,
		},
		{
			name:      "transient that survived the retry budget",
			err:       &ErrTransient{Provider: BackendClaudeCode, Reason: "5xx upstream"},
			retryable: true,
			want:      FallbackTransientExhausted,
		},
		{
			// The sandboxed-claw IPC envelope flattens every error to a
			// string, and kimi/grok have no error channel at all. Those
			// land here, and the chain advances on them.
			name: "opaque error keeps its type-less honesty",
			err:  errors.New("claw backend: runner: something went wrong"),
			want: FallbackUnclassified,
		},
		{
			name: "wrapped typed error is still found",
			err:  fmt.Errorf("node %q: %w", "implement", &ErrRateLimited{Kind: RateLimitKindUsageWindow}),
			want: FallbackUsageWindow,
		},
		{
			name: "nil",
			err:  nil,
			want: FallbackUnclassified,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyFallback(tc.err, tc.retryable); got != tc.want {
				t.Errorf("ClassifyFallback = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestClassifyFallback_NoStringNeedleTier guards the deliberate absence
// of prose matching in the shared vocabulary: an agent quoting an error
// in its own answer must never be reclassified into a route change.
func TestClassifyFallback_NoStringNeedleTier(t *testing.T) {
	quoted := errors.New("the run failed earlier with: You've hit your usage limit, resets 3pm")
	if got := ClassifyFallback(quoted, false); got != FallbackUnclassified {
		t.Errorf("prose describing a usage window classified as %q; the shared classifier must be typed-only", got)
	}
}

// TestIsUsageWindow keeps the predicate honest for the two callers that
// must agree on it: the in-node retry carve-out and the run-level
// usage-window retry.
func TestIsUsageWindow(t *testing.T) {
	if !IsUsageWindow(&ErrRateLimited{Kind: RateLimitKindUsageWindow}) {
		t.Error("usage-window rate limit not recognised")
	}
	if IsUsageWindow(&ErrRateLimited{Kind: RateLimitKindTransient}) {
		t.Error("transient throttle misread as a usage window")
	}
	if IsUsageWindow(errors.New("boom")) {
		t.Error("untyped error misread as a usage window")
	}
}
