package model

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/mcp"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// The launch surface's prediction is never the last word: a sandbox-by-default
// run degrades to the host when no container runtime is there, several
// surfaces cannot predict at all, and a resumed or child run resolves its own.
// The engine settles the question once, and the executor is where that answer
// meets the MCP manager.
//
// Both directions are asserted. The tightening one is the leak; the relaxing
// one is the regression that would follow a fix written for the leak alone —
// an unsandboxed run whose project MCP servers silently stopped working.
func TestSetSandboxSettlesTheMCPStartPolicy(t *testing.T) {
	t.Run("a live sandbox tightens a permissive prediction", func(t *testing.T) {
		m := mcp.NewManager(nil, mcp.WithStartPolicy(mcp.StartAllServers))
		e := &ClawExecutor{logger: iterlog.Nop(), mcpManager: m}

		e.SetSandbox(unmetSandboxRun{})

		if got := m.StartPolicy(); got != mcp.StartOperatorServersOnly {
			t.Errorf("policy after a live sandbox = %v, want operator servers only", got)
		}
	})

	t.Run("settling without a sandbox opens an undecided manager", func(t *testing.T) {
		// The zero value: no surface answered. Without this call the manager
		// could not tell "no sandbox" from "not settled yet" and would refuse
		// every workflow-controlled server for the whole run.
		m := mcp.NewManager(nil)
		e := &ClawExecutor{logger: iterlog.Nop(), mcpManager: m}
		if got := m.StartPolicy(); got != mcp.StartPolicyUnknown {
			t.Fatalf("premise: an unarmed manager starts undecided, got %v", got)
		}

		e.SetSandbox(nil)

		if got := m.StartPolicy(); got != mcp.StartAllServers {
			t.Errorf("policy after settling without a sandbox = %v, want all servers", got)
		}
	})

	t.Run("no manager, no panic", func(t *testing.T) {
		e := &ClawExecutor{logger: iterlog.Nop()}
		e.SetSandbox(unmetSandboxRun{})
		e.SetSandbox(nil)
	})
}
