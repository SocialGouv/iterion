package delegate

import (
	"context"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/secrets"
)

// Under the ambient stamp the pod's ZAI_API_KEY / MOONSHOT_API_KEY are not
// the run's money: a facade-pinned node with no run-held key is refused, not
// funded from the pod environment — the same choice the run-held and pinned
// arms above it make (facadeEnvForRunHeld).
func TestFacadeCredEnvForHint_ThePodEnvDoesNotFundAStampedRun(t *testing.T) {
	t.Setenv("ZAI_API_KEY", "sk-pod-zai")
	stamped := secrets.Credentials{AmbientRefused: true}
	env := facadeCredEnvForHint(string(secrets.ProviderZAI), stamped, true)
	for k, v := range env {
		if v == "sk-pod-zai" {
			t.Fatalf("%s=%q — the pod's env key funds a stamped run's facade node", k, v)
		}
	}
}

// The refusal NAMES the true reason under the stamp: the pod's variable IS
// set and refused — "no X in the environment" would send the operator
// checking a pod that answered exactly as told.
func TestFacadeHintRefusal_AStampedRunLearnsTheTrueReason(t *testing.T) {
	t.Setenv("ZAI_API_KEY", "sk-pod-zai")
	ctx := secrets.WithCredentials(context.Background(), secrets.Credentials{AmbientRefused: true})
	err := facadeHintRefusal(ctx, "zai", suppressAnthropicWireEnv())
	refused, ok := err.(*ErrNoFacadeCredential)
	if !ok {
		t.Fatalf("err = %v, want *ErrNoFacadeCredential", err)
	}
	if refused.Because == "" || strings.Contains(refused.Error(), "in the environment") {
		t.Fatalf("message = %q, want the stamp's true reason, not the environment clause", refused.Error())
	}
}
