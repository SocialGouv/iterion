package delegate

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/runops"
)

// IsIterionMCPTool's one reader — the engine's shared-worktree classifier —
// reads a match as "this tool cannot touch the worktree the parallel branches
// share". For the board server that holds by construction: the board lives
// outside any worktree. For the RUNS server it holds only while that server
// serves reads, and a tool that LAUNCHES a run would break it — a run started
// without `worktree: auto` executes in the caller's cwd, which is that very
// worktree.
//
// So the assumption is pinned instead of believed: this test fails the day the
// runs server grows a tool outside its read-only vocabulary, before the
// classifier can call it safe.
func TestTheIterionRunsMCPServerServesOnlyReads(t *testing.T) {
	readOnly := map[string]bool{"run_events": true, "run_get": true, "runs_list": true}
	// Asked for every capability name the package knows AND some it does not,
	// so a new capability that unlocks a new tool cannot slip past by not
	// being named here.
	for _, caps := range [][]string{
		{runops.CapRunsRead},
		{runops.CapRunsRead, "runs.write", "runs.launch", "runs.resume", "runs.cancel"},
	} {
		for _, fqn := range RunToolsFor(caps) {
			name := strings.TrimPrefix(fqn, "mcp__"+runsMCPServerName+"__")
			if !readOnly[name] {
				t.Errorf("the runs MCP server serves %q, which is outside its read-only vocabulary — IsIterionMCPTool's reader would call a node holding it workspace-safe (caps %v)", name, caps)
			}
		}
	}
	// …and the operator server's launching tools are NOT matched: they are
	// served under a different name and never reach a node this way.
	for _, launcher := range []string{"mcp__iterion__local_run", "mcp__iterion__local_resume", "local_run"} {
		if IsIterionMCPTool(launcher) {
			t.Errorf("%q was read as one of this package's MCP tools — it launches runs, and a run without `worktree: auto` executes in the shared worktree", launcher)
		}
	}
}
