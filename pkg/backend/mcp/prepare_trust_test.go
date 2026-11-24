package mcp

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The compile seam's record (ADR-162): the MCP catalog is resolved at compile
// time for the trust the caller names, and the resolution says which — so a
// catalog an executor boots carries the workspace it was resolved for.
func TestPrepareWorkflow_RecordsTheTrustItResolvedFor(t *testing.T) {
	for _, trust := range []store.RunTrust{store.RunTrustDefault, store.RunTrustFork} {
		wf := &ir.Workflow{Name: "w", Nodes: map[string]ir.Node{}}
		if err := PrepareWorkflow(wf, t.TempDir(), t.TempDir(), trust); err != nil {
			t.Fatalf("PrepareWorkflow(%q): %v", trust, err)
		}
		if wf.MCPPrepared == nil {
			t.Fatalf("PrepareWorkflow(%q) recorded no resolution", trust)
		}
		if got := store.RunTrust(wf.MCPPrepared.Trust); got != trust {
			t.Errorf("PrepareWorkflow(%q) recorded trust %q", trust, got)
		}
	}
}
