package model

import (
	"slices"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/toolcatalog"
)

// Ultracode on claw grants the subagent tool to a node that restricts its
// tools, once — also when the author already lists it.
func TestUltracodeGrantsTheSubagentToolOnce(t *testing.T) {
	t.Parallel()
	e := &ClawExecutor{}
	for _, tools := range [][]string{{"bash"}, {"bash", "agent"}} {
		got := e.assembleEffectiveTools(backendFields{id: "n", tools: tools}, delegate.BackendClaw, nil, true)
		has := map[string]int{}
		for _, name := range got {
			has[name]++
		}
		if has[clawOrchestrationTool] != 1 || has["bash"] != 1 {
			t.Fatalf("tools %v: union = %v, want bash and %s exactly once each", tools, got, clawOrchestrationTool)
		}
	}
}

// Every name ultracode adds to a claw node's restricted list must resolve
// against the registry: the resolver errors on an unknown name and fails the
// node at dispatch, so granting a name iterion does not register is a hard,
// deterministic failure rather than a degrade. Read from the seam — what the
// union gains with ultracode on — not from the constant alone.
func TestClawGrantedToolsAreRegistered(t *testing.T) {
	t.Parallel()
	e := &ClawExecutor{}
	f := backendFields{id: "n", tools: []string{"bash"}}
	without := e.assembleEffectiveTools(f, delegate.BackendClaw, nil, false)
	with := e.assembleEffectiveTools(f, delegate.BackendClaw, nil, true)
	granted := 0
	for _, name := range with {
		if slices.Contains(without, name) {
			continue
		}
		granted++
		if !toolcatalog.IsBuiltin(name) {
			t.Fatalf("ultracode grants %q to a claw node, but no such tool is registered — "+
				"resolveToolsForNode errors on it and the node dies at dispatch", name)
		}
	}
	if granted == 0 {
		t.Fatal("ultracode granted nothing — this test reads no grant and proves nothing")
	}
}
