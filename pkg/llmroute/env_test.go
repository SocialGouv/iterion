package llmroute

import (
	"reflect"
	"strings"
	"testing"
)

func TestFromEnv_AllDials(t *testing.T) {
	t.Setenv(EnvPairOrder, HarnessClaw+"+anthropic_key, "+HarnessCodex+"+chatgpt_forfait")
	t.Setenv(EnvTriggers, TriggerUsageWindow+" , "+TriggerAuth)
	t.Setenv(EnvRefusedPinnedKey, RefusedPinnedPark)
	t.Setenv(EnvStrict, "true")

	got := FromEnv()
	want := []string{HarnessClaw + "+anthropic_key", HarnessCodex + "+chatgpt_forfait"}
	if !reflect.DeepEqual(got.PairOrder, want) {
		t.Fatalf("pair_order = %v, want %v (entries trimmed)", got.PairOrder, want)
	}
	if !reflect.DeepEqual(got.Triggers, []string{TriggerUsageWindow, TriggerAuth}) {
		t.Fatalf("triggers = %v, want the two named", got.Triggers)
	}
	if got.RefusedPinnedKey != RefusedPinnedPark {
		t.Fatalf("refused_pinned_key = %q, want park", got.RefusedPinnedKey)
	}
	if got.Strict == nil || !*got.Strict {
		t.Fatalf("strict = %v, want true", got.Strict)
	}
}

func TestFromEnv_UnparseableFallsToDefault(t *testing.T) {
	t.Setenv(EnvPairOrder, "claude_code+not_a_slot")
	t.Setenv(EnvTriggers, "budget")
	t.Setenv(EnvRefusedPinnedKey, "sometimes")
	t.Setenv(EnvStrict, "maybe")

	got := FromEnv()
	if got.PairOrder != nil {
		t.Fatalf("pair_order = %v, want nil — a half-read order would route where nobody wrote", got.PairOrder)
	}
	if got.Triggers != nil {
		t.Fatalf("triggers = %v, want nil", got.Triggers)
	}
	if got.RefusedPinnedKey != "" {
		t.Fatalf("refused_pinned_key = %q, want empty", got.RefusedPinnedKey)
	}
	if got.Strict != nil {
		t.Fatalf("strict = %v, want nil", *got.Strict)
	}
}

func TestValidateEnv_BootsOnGarbage(t *testing.T) {
	tests := []struct {
		env  string
		name string
		want string
	}{
		{"claude_code+claude_forfait", EnvPairOrder, ""},
		{"nope", EnvPairOrder, EnvPairOrder},
		{"usage_window", EnvTriggers, ""},
		{"budget", EnvTriggers, EnvTriggers},
		{"forfait", EnvRefusedPinnedKey, ""},
		{"keep", EnvRefusedPinnedKey, EnvRefusedPinnedKey},
		{"true", EnvStrict, ""},
		{"kinda", EnvStrict, "not a boolean"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, e := range []string{EnvPairOrder, EnvTriggers, EnvRefusedPinnedKey, EnvStrict} {
				t.Setenv(e, "")
			}
			t.Setenv(tt.name, tt.env)
			err := ValidateEnv()
			if tt.want == "" {
				if err != nil {
					t.Fatalf("ValidateEnv: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("ValidateEnv = %v, want a refusal naming %q", err, tt.want)
			}
		})
	}
}

// The env dial enforces the SAME list contract the API does: duplicates
// are refused at boot, not silently deduped into the platform layer.
func TestValidateEnv_RefusesDuplicateListItems(t *testing.T) {
	t.Setenv(EnvPairOrder, "claw+anthropic_key,claw+anthropic_key")
	t.Setenv(EnvTriggers, "")
	t.Setenv(EnvRefusedPinnedKey, "")
	t.Setenv(EnvStrict, "")
	if err := ValidateEnv(); err == nil || !strings.Contains(err.Error(), "twice") {
		t.Fatalf("ValidateEnv = %v, want the duplicate refusal", err)
	}
	t.Setenv(EnvPairOrder, "")
	t.Setenv(EnvTriggers, "auth,auth")
	if err := ValidateEnv(); err == nil || !strings.Contains(err.Error(), "twice") {
		t.Fatalf("ValidateEnv(triggers) = %v, want the duplicate refusal", err)
	}
}
