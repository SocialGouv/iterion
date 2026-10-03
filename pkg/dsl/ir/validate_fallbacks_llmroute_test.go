package ir

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/llmroute"
)

// The routing policy's harness vocabulary (pkg/llmroute) must cover every
// backend name this file's tables know: a backend added to
// gateEnforcingModes or reasoningEffortBackends without joining the policy
// vocabulary would be un-nameable in a pair_order — a dial that silently
// cannot address it. llmroute keeps its own list (it must not import the
// IR); this test is the guard that keeps the two from drifting.
func TestBackendNamesAreNameableInTheRoutingPolicy(t *testing.T) {
	for name := range gateEnforcingModes {
		if !llmrouteHarnessOK(name) {
			t.Errorf("gateEnforcingModes names backend %q, which pkg/llmroute's harness vocabulary does not admit", name)
		}
	}
	for name := range reasoningEffortBackends {
		if !llmrouteHarnessOK(name) {
			t.Errorf("reasoningEffortBackends names backend %q, which pkg/llmroute's harness vocabulary does not admit", name)
		}
	}
}

func llmrouteHarnessOK(name string) bool {
	switch name {
	case llmroute.HarnessClaudeCode, llmroute.HarnessClaw, llmroute.HarnessCodex,
		llmroute.HarnessPi, llmroute.HarnessGrok, llmroute.HarnessKimi, llmroute.HarnessOpencode:
		return true
	}
	return false
}
