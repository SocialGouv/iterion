package runops

import (
	"context"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/store"
)

// This server's whole vocabulary is READ-ONLY, and something outside this
// package depends on it: the engine's shared-worktree classifier treats an
// `mcp__iterion_runs__*` tool as unable to touch the worktree parallel
// branches share. A tool that LAUNCHED work would break that — a run started
// without `worktree: auto` executes in the caller's cwd, which is that very
// worktree.
//
// Pinned HERE, over the package-level slice, and not only across the delegate
// seam: a capability-gated tool is invisible to any test that has to guess
// capability names, and `ToolsFor` is gated. This one guesses nothing.
func TestThisServerAdvertisesOnlyReads(t *testing.T) {
	readOnly := map[string]bool{"run_events": true, "run_get": true, "runs_list": true}
	for _, tool := range tools {
		if !readOnly[tool.Name] {
			t.Errorf("runops advertises %q, outside the read-only vocabulary the engine's workspace classifier relies on — teach the classifier first, then add it here", tool.Name)
		}
	}
	// …and dispatch cannot serve a name the advertisement does not carry, so
	// a tool added to Call's switch alone is refused rather than served.
	for _, name := range []string{"run_launch", "run_resume", "run_cancel", ""} {
		if advertises(name) {
			t.Errorf("%q is advertised by this server", name)
		}
	}
}

// …and the guarantee behind that second half, exercised through the real entry
// point: a tool added to Call's switch but never advertised is REFUSED, so the
// set this server serves cannot drift from the set it declares. Both MCP
// transports (the CLI's and the server's) dispatch straight into Call, so this
// is where the two can be tied together.
func TestCallRefusesAToolThisServerDoesNotAdvertise(t *testing.T) {
	rs, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	caps := NewCapabilities(CapRunsRead)
	if _, err := Call(context.Background(), rs, caps, "run_launch", nil); err == nil ||
		!strings.Contains(err.Error(), "unknown tool") {
		t.Fatalf("Call served an unadvertised tool (err=%v) — a name reachable here but absent from `tools` is judged by the engine on a vocabulary it is not in", err)
	}
	// The advertised ones still dispatch (the guard must not close the door
	// on the vocabulary it protects).
	if _, err := Call(context.Background(), rs, caps, "runs_list", []byte(`{}`)); err != nil {
		t.Fatalf("an advertised tool was refused: %v", err)
	}
}
