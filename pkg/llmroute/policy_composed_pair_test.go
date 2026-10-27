package llmroute

import (
	"strings"
	"testing"
)

// The composed pair (#2637): each half validates alone on its own entry,
// so ValidateComposedPair is the only place the split pin can be
// refused. The three refusals mirror the per-entry ones, naming both
// entries and their levels.
func TestValidateComposedPair_RefusesTheSplitThePerEntryCheckCertifies(t *testing.T) {
	policyCell := `the routing policy's override cell "work"`
	launchRow := `the launch's own override row "*"`
	cases := []struct {
		name          string
		backend       string
		backendFrom   string
		provider      string
		providerFrom  string
		wantFragments []string
	}{
		{
			name:          "a harness that consumes no hint",
			backend:       "codex",
			backendFrom:   `the routing policy's override cell "reviewer_*"`,
			provider:      "anthropic",
			providerFrom:  launchRow,
			wantFragments: []string{"does not consume the provider hint", `the routing policy's override cell "reviewer_*"`, launchRow},
		},
		{
			name:          "a single-value harness offered a chain",
			backend:       "claw",
			backendFrom:   policyCell,
			provider:      "anthropic,zai",
			providerFrom:  launchRow,
			wantFragments: []string{"does not walk chains", "anthropic"},
		},
		{
			name:          "a value the harness ignores",
			backend:       "claw",
			backendFrom:   policyCell,
			provider:      "zai",
			providerFrom:  launchRow,
			wantFragments: []string{"acts on none of this value"},
		},
		{
			name:          "auto stays a refusal when composed",
			backend:       "claude_code",
			backendFrom:   policyCell,
			provider:      "auto",
			providerFrom:  launchRow,
			wantFragments: []string{"never defers"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateComposedPair(tc.backend, tc.backendFrom, tc.provider, tc.providerFrom)
			if err == nil {
				t.Fatalf("composed pair %s + %s was admitted", tc.backend, tc.provider)
			}
			for _, want := range tc.wantFragments {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("error = %q, want it to carry %q", err, want)
				}
			}
		})
	}
}

// The pairs the harness honours stay admitted when composed, and a
// half-set pair is not a pair.
func TestValidateComposedPair_KeepsTheHonouredPairs(t *testing.T) {
	legal := []struct{ backend, provider string }{
		{"claw", "anthropic"},
		{"claude_code", "anthropic"},
		{"claude_code", "zai"},
		{"claude_code", "anthropic,zai"},
	}
	for _, tc := range legal {
		if err := ValidateComposedPair(tc.backend, `the routing policy's override cell "work"`, tc.provider, `the launch's own override row "*"`); err != nil {
			t.Errorf("composed pair %s + %s refused: %v", tc.backend, tc.provider, err)
		}
	}
	if err := ValidateComposedPair("claw", "x", "", "y"); err != nil {
		t.Errorf("a provider-less pair was judged: %v", err)
	}
	if err := ValidateComposedPair("", "x", "zai", "y"); err != nil {
		t.Errorf("a backend-less pair was judged: %v", err)
	}
}

// The premise the composed check rests on: the per-entry write gate
// admits each half alone — which is exactly the split it certifies blind.
func TestValidateOverride_AdmitsEachHalfOfASplitPair(t *testing.T) {
	if err := validateOverride(Override{Selector: "work", Backend: "claw"}); err != nil {
		t.Fatalf("the backend half was refused on its own: %v", err)
	}
	if err := validateOverride(Override{Selector: "work", Provider: "zai"}); err != nil {
		t.Fatalf("the provider half was refused on its own: %v", err)
	}
}
