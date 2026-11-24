package runtime

// #2811 asked whether the workspace-safety arm can stop reading the
// ultracode-granted `agent` as a writer, now that the shipped runner bounds
// a child to the parent node's own effective tools (#1940). The decision is
// that it cannot — and this file pins the verdict that follows, on the
// admission itself rather than on the name list.
//
// The bound is real but it is the HOST's: `ClawDefaults.Subagent` is a
// host's to set, and an embedding wiring its own runner over a registry of
// its choosing is exactly the case the worst case exists for. Neither the IR
// nor the program-only executor behind `validate --exec` — whose answers
// must agree with dispatch's — can see that wiring, so "safe only for the
// shipped hosts" is a verdict this name reading cannot carry. Relaxing it
// anyway would admit, silently, the one shape the guard exists to refuse.
//
// The reading is revisited the day the seam carries the runner's bound in a
// form BOTH readers can answer — and this test is where that relaxation must
// come to redden first.

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// A node holding only readers plus the ultracode `agent` grant is refused a
// shared worktree, and the refusal says why — the grant's name, not a bare
// node id. Red when `agent` leaves the arm's unsafe reading without the seam
// carrying the runner's bound: the pair below is then admitted on the
// strength of a guarantee this package cannot see.
func TestTheAgentGrantKeepsAnUltracodeReaderOffTheSharedWorktree(t *testing.T) {
	pinTheHostsAutoMemoryMode(t)
	ultracode := twoReaderBranches(func(n *ir.AgentNode) { n.ReasoningEffort = "ultracode" })
	e, wf := effectiveSurfaceEngine(t, ultracode), ultracode

	// The reading this verdict rests on: the grant is on the node's effective
	// list, or the refusal below is someone else's doing and the test
	// certifies nothing.
	held := e.toolSurfaceResolver().EffectiveToolNames(wf.Nodes["a"], false)
	sawAgent := false
	for _, name := range held {
		if name == "agent" {
			sawAgent = true
		}
	}
	if !sawAgent {
		t.Fatalf("precondition: ultracode granted no `agent` on %v — the arm this test pins reads nothing", held)
	}

	err := e.validateWorkspaceSafety("fan", wf.Edges)
	if err == nil {
		t.Fatal("two ultracode reader branches were admitted onto one shared worktree: the arm stopped reading `agent` as a writer, but the runner's bound is the host's wiring, which neither the IR nor the program-only reader can see — see this file's header")
	}
	rerr, ok := err.(*RuntimeError)
	if !ok || rerr.Code != ErrCodeWorkspaceSafety {
		t.Fatalf("err = %v, want the typed workspace-safety refusal", err)
	}
	if !strings.Contains(rerr.Message, `holds "agent"`) {
		t.Errorf("the refusal does not name the grant that decided it — an unexplained refusal is the bug this arm exists not to be: %s", rerr.Message)
	}

	// The refusal is the grant's doing, not the shape's: the same branches
	// without ultracode share the worktree.
	plain := twoReaderBranches()
	pe := effectiveSurfaceEngine(t, plain)
	if err := pe.validateWorkspaceSafety("fan", plain.Edges); err != nil {
		t.Fatalf("control: two reader branches without the grant were refused — the verdict above is not the grant's doing: %v", err)
	}

	// And the escape hatch holds: readonly: is the author's assertion,
	// honoured before the surface is consulted.
	locked := twoReaderBranches(func(n *ir.AgentNode) { n.ReasoningEffort = "ultracode"; n.Readonly = true })
	le := effectiveSurfaceEngine(t, locked)
	if err := le.validateWorkspaceSafety("fan", locked.Edges); err != nil {
		t.Fatalf("readonly: must admit the same ultracode readers — it is the documented escape hatch: %v", err)
	}
}
