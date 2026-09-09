package runview

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/store"
)

func TestExecutionContextPolicyFromEnvReliabilityAlias(t *testing.T) {
	t.Setenv("ITERION_RELIABILITY_MODE", "enforce")
	t.Setenv("ITERION_EXECUTION_CONTEXT_POLICY", "report")
	if got := ExecutionContextPolicyFromEnv(); got != store.ContextPolicyEnforce {
		t.Fatalf("policy = %s, want enforce", got)
	}
	t.Setenv("ITERION_RELIABILITY_MODE", "")
	if got := ExecutionContextPolicyFromEnv(); got != store.ContextPolicyReport {
		t.Fatalf("legacy policy fallback = %s, want report", got)
	}
	t.Setenv("ITERION_EXECUTION_CONTEXT_POLICY", "report")
	if got := ExecutionContextPolicyFromEnv(); got != store.ContextPolicyReport {
		t.Fatalf("specific policy = %s, want report", got)
	}
}
