package dispatcher

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// TestEngineRunnerCompilesTheRunAgainstItsDispatchWorkspace pins the
// per-dispatch MCP resolution (#1800): one EngineRunner shares its compiled
// IR across concurrent dispatches, so each run's `{{workspace}}` placeholders
// are resolved on the run's OWN workflow against ITS workspace. The mutation
// that derives the workspace from the bot's path again reddens on the ws1/ws2
// assertions.
func TestEngineRunnerCompilesTheRunAgainstItsDispatchWorkspace(t *testing.T) {
	t.Setenv("ITERION_PLUGINS_ENABLE", "codeindex")
	dir := t.TempDir()
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
	r, err := NewEngineRunner(bot, iterlog.Nop())
	if err != nil {
		t.Fatalf("NewEngineRunner: %v", err)
	}
	defer func() { _ = r.Close() }()

	ws1, ws2 := t.TempDir(), t.TempDir()
	if ws1 == ws2 {
		t.Fatal("precondition failed: the two workspaces must differ for the assertion to mean anything")
	}
	wf1, _, err := r.compileForDispatch(ws1)
	if err != nil {
		t.Fatalf("compileForDispatch ws1: %v", err)
	}
	wf2, _, err := r.compileForDispatch(ws2)
	if err != nil {
		t.Fatalf("compileForDispatch ws2: %v", err)
	}

	if wf1 == wf2 {
		t.Fatal("compileForDispatch returned the SHARED workflow — a per-run resolution would race across concurrent dispatches")
	}
	if got := lastCodeindexRepoArg(t, wf1); got != ws1 {
		t.Fatalf("dispatch workspace 1: codeindex --repo = %q, want %q — {{workspace}} resolved against the bot's directory", got, ws1)
	}
	if got := lastCodeindexRepoArg(t, wf2); got != ws2 {
		t.Fatalf("dispatch workspace 2: codeindex --repo = %q, want %q — the per-run resolution is not per run", got, ws2)
	}
}

// lastCodeindexRepoArg is codeindex's expanded --repo value: the LAST arg of
// the plugin's pinned argv, which the manifest spells {{workspace}}.
func lastCodeindexRepoArg(t *testing.T, wf *ir.Workflow) string {
	t.Helper()
	codeindex, ok := wf.ResolvedMCPServers["codeindex"]
	if !ok {
		t.Fatalf("codeindex absent from the compiled catalog (%d servers)", len(wf.ResolvedMCPServers))
	}
	return codeindex.Args[len(codeindex.Args)-1]
}
