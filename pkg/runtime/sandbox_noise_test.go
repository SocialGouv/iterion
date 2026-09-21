package runtime

import (
	"os"
	"testing"

	"github.com/SocialGouv/iterion/pkg/sandbox"
	"github.com/SocialGouv/iterion/pkg/treenoise"
)

// The sandbox half of the env (plan review F1): in-sandbox tool scripts read
// the pathspecs from the container env, set once at spec build — the same
// double-write ITERION_ARTIFACT_FILES_DIR gets. A workspace without a
// devbox.json carries it just the same (provisioning never runs there).
func TestSeedTreeNoiseEnvSetsThePathspecs(t *testing.T) {
	t.Setenv(treenoise.TreeNoiseEnvVar, "")
	if err := os.Unsetenv(treenoise.TreeNoiseEnvVar); err != nil {
		t.Fatal(err)
	}
	spec := &sandbox.Spec{}
	seedTreeNoiseEnv(spec)
	if got := spec.Env[treenoise.TreeNoiseEnvVar]; got != treenoise.EnvValue() {
		t.Fatalf("spec.Env[%q] = %q, want %q", treenoise.TreeNoiseEnvVar, got, treenoise.EnvValue())
	}
}

// An operator or a workflow that set the variable themselves win: the seed
// steps aside, like seedDefaultLocale does for LANG. An explicitly EMPTY
// preset is not a claim (verdict 9) — the canonical entry applies, the same
// rule the OS-env branch follows.
func TestSeedTreeNoiseEnvStepsAsideForAnOperatorValue(t *testing.T) {
	spec := &sandbox.Spec{Env: map[string]string{treenoise.TreeNoiseEnvVar: "':(exclude,top)vendor'"}}
	seedTreeNoiseEnv(spec)
	if got := spec.Env[treenoise.TreeNoiseEnvVar]; got != "':(exclude,top)vendor'" {
		t.Fatalf("spec.Env[%q] = %q, want the operator's value kept", treenoise.TreeNoiseEnvVar, got)
	}
}

// An explicitly EMPTY preset is not a claim either (verdict 9): the
// canonical entry applies, the same rule every other branch follows.
func TestSeedTreeNoiseEnvTreatsAnEmptyPresetAsNoClaim(t *testing.T) {
	spec := &sandbox.Spec{Env: map[string]string{treenoise.TreeNoiseEnvVar: ""}}
	seedTreeNoiseEnv(spec)
	if got := spec.Env[treenoise.TreeNoiseEnvVar]; got != treenoise.EnvValue() {
		t.Fatalf("spec.Env[%q] = %q, want the canonical entry (empty is not a claim)", treenoise.TreeNoiseEnvVar, got)
	}
}

// The operator's own EXPORTED environment (before `iterion run`) is the
// same claim: their list is honored IN the container — one rule on every
// surface (verdict 3). t.Setenv holds to the end of the test.
func TestSeedTreeNoiseEnvHonorsAnOperatorExportedValue(t *testing.T) {
	t.Setenv(treenoise.TreeNoiseEnvVar, "':(exclude,top)operator'")
	spec := &sandbox.Spec{}
	seedTreeNoiseEnv(spec)
	if got := spec.Env[treenoise.TreeNoiseEnvVar]; got != "':(exclude,top)operator'" {
		t.Fatalf("spec.Env[%q] = %q, want the operator's exported value honored", treenoise.TreeNoiseEnvVar, got)
	}
}

// A nil Env map must not panic — the spec may arrive with no env at all.
func TestSeedTreeNoiseEnvCreatesTheMap(t *testing.T) {
	spec := &sandbox.Spec{}
	if spec.Env != nil {
		t.Fatalf("precondition: Env starts nil")
	}
	seedTreeNoiseEnv(spec)
	if spec.Env == nil {
		t.Fatal("seedTreeNoiseEnv left Env nil")
	}
}
