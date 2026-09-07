package model

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/toolcatalog"
)

// Ultracode on claw grants the subagent tool to a node that restricts its
// tools, once.
func TestWithClawOrchestrationTools(t *testing.T) {
	got := withClawOrchestrationTools([]string{"bash", "agent"})
	has := map[string]int{}
	for _, name := range got {
		has[name]++
	}
	if has["agent"] != 1 || has["bash"] != 1 {
		t.Fatalf("tools = %v, want bash and agent exactly once each", got)
	}
}

// Every name the engine adds to a claw node's restricted list must resolve
// against the registry: the resolver errors on an unknown name and fails the
// node at dispatch, so granting a name iterion does not register is a hard,
// deterministic failure rather than a degrade.
func TestClawGrantedToolsAreRegistered(t *testing.T) {
	for _, name := range withClawOrchestrationTools(nil) {
		if !toolcatalog.IsBuiltin(name) {
			t.Fatalf("ultracode grants %q to a claw node, but no such tool is registered — "+
				"resolveToolsForNode errors on it and the node dies at dispatch", name)
		}
	}
}
