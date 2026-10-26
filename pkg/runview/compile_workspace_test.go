package runview

import (
	"os"
	"path/filepath"
	"testing"
)

// TestCompileWorkflowPathResolvesPluginWorkspaceAgainstTheRunWorkspace pins
// the run-workspace threading (#1800): the .bot's directory feeds the project
// `.mcp.json` tier, while the plugin `{{workspace}}` placeholders expand
// against the workspace the caller names — never derived from the path. The
// mutation that derives it from path again (PrepareWorkflow(cr.Workflow,
// mcpDir, mcpDir)) reddens on the last assertion.
func TestCompileWorkflowPathResolvesPluginWorkspaceAgainstTheRunWorkspace(t *testing.T) {
	t.Setenv("ITERION_PLUGINS_ENABLE", "codeindex")
	dir := t.TempDir()
	workspace := t.TempDir()
	const src = `prompt sys:
  Use the tools.
prompt usr:
  go.
schema out:
  x: string
agent a:
  backend: "claw"
  model: "openai/gpt-5.4-mini"
  system: sys
  user: usr
  output: out
  tools: [mcp.codeindex.*]
  mcp:
    servers: [codeindex]
  session: fresh
workflow main:
  entry: a
  a -> done
`
	bot := filepath.Join(dir, "main.bot")
	if err := os.WriteFile(bot, []byte(src), 0o644); err != nil {
		t.Fatalf("write bot: %v", err)
	}

	wf, _, _, err := CompileWorkflowPath(bot, workspace)
	if err != nil {
		t.Fatalf("CompileWorkflowPath: %v", err)
	}

	codeindex, ok := wf.ResolvedMCPServers["codeindex"]
	if !ok {
		t.Fatalf("codeindex absent from the compiled catalog (%d servers)", len(wf.ResolvedMCPServers))
	}
	if got := codeindex.Args[len(codeindex.Args)-1]; got != workspace {
		t.Fatalf("codeindex --repo = %q, want the run workspace %q — the compile derived {{workspace}} from the .bot's directory", got, workspace)
	}
}
