package secrets

import (
	"testing"

	"github.com/SocialGouv/iterion/internal/envtrust"
)

// The private-endpoint switch is the OPERATOR's. The runner inside a
// sandboxed crossing auto-loads the workspace `.env`, so a repository under
// review could plant the name and make the runner lax where the run proxy
// stays strict; a value a project `.env` planted therefore does not lift the
// public-address requirement, in the host and in the runner alike.
func TestLLMEndpointAllowPrivateIsTheOperatorsSwitchNotTheRepositorys(t *testing.T) {
	const name = "ITERION_LLM_ENDPOINT_ALLOW_PRIVATE"
	t.Setenv(envtrust.EnvPlantedNames, "")
	envtrust.ResetForTest()
	t.Cleanup(envtrust.ResetForTest)

	t.Setenv(name, "")
	if LLMEndpointAllowPrivate() {
		t.Fatal("with nothing exported the requirement is lifted")
	}
	t.Setenv(name, "1")
	if !LLMEndpointAllowPrivate() {
		t.Fatal("the operator's own export does not lift the requirement")
	}
	envtrust.MarkPlanted(name)
	if LLMEndpointAllowPrivate() {
		t.Fatal("a value a project .env planted lifted the public-address requirement: the repository under review spoke for the operator")
	}
}
