package ir

import (
	"strings"
	"testing"
)

func installPolicy(t *testing.T, fn func(string) bool) {
	t.Helper()
	SetProcessEnvPolicy(fn)
	t.Cleanup(func() { SetProcessEnvPolicy(nil) })
}

// Workflow text reads a refused name as unset — its `:-` default fires —
// while the operator's own configuration still reads it, and with no policy
// installed (a local operator's environment) every name resolves.
func TestProcessEnvPolicyGatesWorkflowTextOnly(t *testing.T) {
	const value = "policy-probe-value"
	t.Setenv("PROBE_POLICY_SECRET", value)
	if got := LookupEnv("PROBE_POLICY_SECRET"); got != value {
		t.Fatalf("with no policy, LookupEnv = %q, want the value", got)
	}
	installPolicy(t, func(name string) bool { return !strings.Contains(name, "SECRET") })
	if got := LookupEnv("PROBE_POLICY_SECRET"); got != "" {
		t.Errorf("LookupEnv = %q, want a refused name read as unset", got)
	}
	if got := ExpandEnvWithDefault("${PROBE_POLICY_SECRET:-fallback}"); got != "fallback" {
		t.Errorf("ExpandEnvWithDefault = %q, want the default", got)
	}
	if got := LookupOperatorEnv("PROBE_POLICY_SECRET"); got != value {
		t.Errorf("LookupOperatorEnv = %q, want the value: the policy is about workflow text", got)
	}
	t.Setenv("PROBE_POLICY_KNOB", "knob")
	if got := LookupEnv("PROBE_POLICY_KNOB"); got != "knob" {
		t.Errorf("LookupEnv = %q, want an allowed name read", got)
	}
}
