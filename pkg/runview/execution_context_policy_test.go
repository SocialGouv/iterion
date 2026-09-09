package runview

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/reliability"
	"github.com/SocialGouv/iterion/pkg/store"
)

// TestExecutionContextPolicyFromEnvDelegatesToReliability pins the property
// that makes the documented rollback real: the launch surfaces and the
// rollout report answer "what policy is in effect" through ONE resolution. A
// second, re-derived precedence chain here is exactly the drift that let
// ITERION_RELIABILITY_MODE=legacy be a silent no-op on a host carrying the
// older ITERION_EXECUTION_CONTEXT_POLICY.
func TestExecutionContextPolicyFromEnvDelegatesToReliability(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mode   string
		policy string
	}{
		{name: "both unset"},
		{name: "mode alone", mode: "enforce"},
		{name: "alias alone", policy: "report"},
		{name: "rollback over an enforcing alias", mode: "legacy", policy: "enforce"},
		{name: "mode wins over alias", mode: "report", policy: "enforce"},
		{name: "invalid mode", mode: "nonsense", policy: "enforce"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(reliability.EnvMode, tc.mode)
			t.Setenv(reliability.EnvContextPolicyAlias, tc.policy)
			want := reliability.ContextPolicyFromEnv()
			if got := ExecutionContextPolicyFromEnv(); got != want {
				t.Fatalf("policy = %s, want %s (reliability resolution)", got, want)
			}
		})
	}
}

// TestExecutionContextPolicyFromEnvRollbackIsAuthoritative states the
// operator-visible half of the same contract in its own terms, so the
// delegation test above cannot go green with both sides wrong together.
func TestExecutionContextPolicyFromEnvRollbackIsAuthoritative(t *testing.T) {
	t.Setenv(reliability.EnvContextPolicyAlias, "enforce")
	t.Setenv(reliability.EnvMode, "legacy")
	if got := ExecutionContextPolicyFromEnv(); got != store.ContextPolicyLegacy {
		t.Fatalf("rollback policy = %s, want legacy", got)
	}
}

func TestExecutionContextPolicyFromEnvInvalidModeKeepsValidAlias(t *testing.T) {
	t.Setenv(reliability.EnvContextPolicyAlias, "enforce")
	t.Setenv(reliability.EnvMode, "enforced")
	if got := ExecutionContextPolicyFromEnv(); got != store.ContextPolicyEnforce {
		t.Fatalf("policy with invalid new mode = %s, want enforce from compatibility alias", got)
	}
}
