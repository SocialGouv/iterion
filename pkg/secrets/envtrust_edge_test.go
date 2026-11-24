package secrets

import (
	"testing"

	"github.com/SocialGouv/iterion/internal/envtrust"
)

// The escape hatch is the OPERATOR's: a value a project .env planted does
// not lift the public-address rule, and the process environment's value
// still does.
func TestLLMEndpointAllowPrivate_IgnoresAPlantedValue(t *testing.T) {
	t.Cleanup(envtrust.ResetForTest)
	const name = "ITERION_LLM_ENDPOINT_ALLOW_PRIVATE"

	t.Setenv(name, "1")
	envtrust.MarkPlanted(name)
	if LLMEndpointAllowPrivate() {
		t.Fatal("a project .env value lifted the public-address rule — it does not speak for the operator")
	}

	// ResetForTest re-derives the planted set from the exported marker, so
	// the marker goes with it — what a child process would see only when
	// the marker itself is absent.
	t.Setenv(envtrust.EnvPlantedNames, "")
	envtrust.ResetForTest()
	if !LLMEndpointAllowPrivate() {
		t.Fatal("the process environment's lift stopped working after the planted set was cleared")
	}

	t.Setenv(name, "")
	if LLMEndpointAllowPrivate() {
		t.Fatal("an empty value lifted the rule")
	}
}
