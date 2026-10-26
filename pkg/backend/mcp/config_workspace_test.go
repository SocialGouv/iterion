package mcp

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// TestPrepareWorkflowTakesTheProjectAndTheWorkspaceApart pins the two-input
// contract (#1800): the project `.mcp.json` is read from projectDir while the
// plugin `{{workspace}}` placeholders expand against the RUN's workspace, a
// different directory. The mutation that re-conflates the two — passing
// projectDir to loadPluginServers — reddens on the workspace assertion.
func TestPrepareWorkflowTakesTheProjectAndTheWorkspaceApart(t *testing.T) {
	t.Setenv("ITERION_PLUGINS_ENABLE", "codeindex")
	t.Setenv(EnvAutoLoad, "true")
	projectDir := t.TempDir()
	writeProjectMCPFile(t, projectDir, `{"mcpServers":{"project-sentinel":{"type":"http","url":"https://project-sentinel.invalid/mcp"}}}`)
	workspace := t.TempDir()
	wf := &ir.Workflow{}

	if err := PrepareWorkflow(wf, projectDir, workspace); err != nil {
		t.Fatalf("PrepareWorkflow: %v", err)
	}

	sentinel, ok := wf.ResolvedMCPServers["project-sentinel"]
	if !ok {
		t.Fatalf("project .mcp.json server absent — the projectDir input was dropped (%d servers)", len(wf.ResolvedMCPServers))
	}
	if sentinel.URL != "https://project-sentinel.invalid/mcp" {
		t.Fatalf("project server URL = %q, want the one the project .mcp.json declared", sentinel.URL)
	}
	codeindex, ok := wf.ResolvedMCPServers["codeindex"]
	if !ok {
		t.Fatalf("codeindex absent from the catalog — the plugin did not load (%d servers)", len(wf.ResolvedMCPServers))
	}
	if got := codeindex.Args[len(codeindex.Args)-1]; got != workspace {
		t.Fatalf("codeindex --repo = %q, want the RUN workspace %q — {{workspace}} expanded against the project dir", got, workspace)
	}
}
