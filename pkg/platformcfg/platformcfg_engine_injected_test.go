package platformcfg

import (
	"slices"
	"testing"
)

// TestEngineInjectedVarName pins the launch-side classification (#2416): an
// env-shaped bot-var name — the sovereign-pool provision's stamped
// ITERION_VIBE_* dials and model ids — is engine-injected and rides a strict
// launch, while an infra name, a credential-shaped name, or a plain typo'd
// key is NOT: no ladder level answers those, so the #1757 refusal keeps
// owing them to the caller.
func TestEngineInjectedVarName(t *testing.T) {
	for _, name := range []string{
		// The provisioned repo-bots integration's stamps, measured in #2416.
		"ITERION_VIBE_BACKEND_EMIT",
		"ITERION_VIBE_PROVIDER_EMIT",
		"ITERION_VIBE_MODEL_EMIT",
		"ITERION_VIBE_MODEL_CLAUDE",
		"ITERION_VIBE_EFFORT_CLAUDE",
	} {
		if !EngineInjectedVarName(name) {
			t.Errorf("%s must classify engine-injected — the strict launch accepts it", name)
		}
	}
	for _, name := range []string{
		// Infra namespaces: no ladder level carries them — a launch input
		// spelling one is a no-op the refusal owes the caller.
		"ITERION_MONGO_URL",
		"ITERION_DISABLE_AUTH",
		"ITERION_CLOUD_MAX_COST_USD",
		"ITERION_DSL_READ_BUDGET",
		"ITERION_RUN_ID",
		// Credential-shaped: never ride a non-secret surface.
		"ITERION_VIBE_TOKEN_EMIT",
		// Not env-shaped bot-var names at all: the typo class #1757 exists for.
		"engin",
		"gate_context",
		"ITERION_vibe_backend_emit",
		"VETTY_MODEL_ESCALATE",
		"",
	} {
		if EngineInjectedVarName(name) {
			t.Errorf("%s must NOT classify engine-injected — the strict launch refuses it", name)
		}
	}
}

// TestSplitEngineInjectedInputs pins the partition the five refusal doors
// share: injected names ride, the rest stay refused, and both sides keep the
// input's (sorted) order so a refusal's message stays stable.
func TestSplitEngineInjectedInputs(t *testing.T) {
	injected, refused := SplitEngineInjectedInputs([]string{
		"ITERION_MONGO_URL",
		"ITERION_VIBE_BACKEND_EMIT",
		"ITERION_VIBE_PROVIDER_EMIT",
		"gate_contex",
	})
	if !slices.Equal(injected, []string{"ITERION_VIBE_BACKEND_EMIT", "ITERION_VIBE_PROVIDER_EMIT"}) {
		t.Errorf("injected = %v, want the two VIBE dials in input order", injected)
	}
	if !slices.Equal(refused, []string{"ITERION_MONGO_URL", "gate_contex"}) {
		t.Errorf("refused = %v, want the infra name and the typo in input order", refused)
	}

	injected, refused = SplitEngineInjectedInputs(nil)
	if len(injected) != 0 || len(refused) != 0 {
		t.Errorf("nil input split to injected=%v refused=%v, want both empty", injected, refused)
	}
}
