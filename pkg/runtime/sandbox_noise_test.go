package runtime

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/sandbox"
	"github.com/SocialGouv/iterion/pkg/treenoise"
)

// The sandbox half of the env (plan review F1): in-sandbox tool scripts read
// the pathspecs from the container env, set once at spec build — the same
// double-write ITERION_ARTIFACT_FILES_DIR gets. A workspace without a
// devbox.json carries it just the same (provisioning never runs there).
func TestSeedTreeNoiseEnvSetsThePathspecs(t *testing.T) {
	spec := &sandbox.Spec{}
	seedTreeNoiseEnv(spec)
	if got := spec.Env[treenoise.TreeNoiseEnvVar]; got != treenoise.EnvValue() {
		t.Fatalf("spec.Env[%q] = %q, want %q", treenoise.TreeNoiseEnvVar, got, treenoise.EnvValue())
	}
}

// An operator or a workflow that set the variable themselves win: the seed
// steps aside, like seedDefaultLocale does for LANG.
func TestSeedTreeNoiseEnvStepsAsideForAnOperatorValue(t *testing.T) {
	spec := &sandbox.Spec{Env: map[string]string{treenoise.TreeNoiseEnvVar: "':(exclude,top)vendor'"}}
	seedTreeNoiseEnv(spec)
	if got := spec.Env[treenoise.TreeNoiseEnvVar]; got != "':(exclude,top)vendor'" {
		t.Fatalf("spec.Env[%q] = %q, want the operator's value kept", treenoise.TreeNoiseEnvVar, got)
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
