package runtime

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// programSurfaceEngine is effectiveSurfaceEngine with the executor's
// PROGRAM-only reading: the same *model.ClawExecutor and the same two seams,
// answering from the workflow alone and never from the host (no
// ITERION_DEFAULT_BACKEND, no credential probe). It is what `iterion validate
// --exec` runs, and the only reading a gate over the whole cell space can
// freeze: with the host's, a route left to dispatch resolves to whatever
// backend the machine has credentials for, so the same table reads differently
// on two machines.
func programSurfaceEngine(wf *ir.Workflow) *Engine {
	return &Engine{workflow: wf, executor: model.NewProgramExecutor(wf)}
}

// gridFanOut builds the shape the guard decides on from one grid cell: two
// identical branches of one fan_out_all router, so a refusal is the branch
// pair's and its cause is the node's.
func gridFanOut(c toolSurfaceGridCell, mutate func(*ir.AgentNode)) (*Engine, *ir.Workflow) {
	return gridFanOutIn(c, mutate, nil)
}

// gridFanOutIn is gridFanOut with the WORKFLOW half of an opt-in as well as
// the node's: `auto_memory:` and an image attachment are declared on the
// workflow, and the list a route receives — which admission's second half
// reads — differs from the declared one only under such an opt-in.
func gridFanOutIn(c toolSurfaceGridCell, nodeMut func(*ir.AgentNode), flowMut func(*ir.Workflow)) (*Engine, *ir.Workflow) {
	mk := func(id string) *ir.AgentNode {
		n := agentNode(id, c.tools, c.primary, nodeMut)
		if c.routeSet {
			n.Fallbacks = []ir.Fallback{{Name: "rescue", Backend: c.route}}
		}
		return n
	}
	router := &ir.RouterNode{BaseNode: ir.BaseNode{ID: "fan"}, RouterMode: ir.RouterFanOutAll}
	wf := &ir.Workflow{
		DefaultBackend: c.dflt,
		Nodes:          map[string]ir.Node{"fan": router, "a": mk("a"), "b": mk("b")},
		Edges:          []*ir.Edge{{From: "fan", To: "a"}, {From: "fan", To: "b"}},
	}
	if flowMut != nil {
		flowMut(wf)
	}
	return programSurfaceEngine(wf), wf
}

// fanOutRefusal drives the guard the engine runs at the router and returns the
// refusal's message, or "" where the pair is admitted.
func fanOutRefusal(t *testing.T, e *Engine, wf *ir.Workflow) string {
	t.Helper()
	err := e.validateWorkspaceSafety("fan", wf.Edges)
	if err == nil {
		return ""
	}
	rerr, ok := err.(*RuntimeError)
	if !ok || rerr.Code != ErrCodeWorkspaceSafety {
		t.Fatalf("err = %v, want the typed workspace-safety refusal", err)
	}
	return rerr.Message
}

// causelessRefusal is the shape a refusal takes when the cause walk found no
// arm to name: the node id and nothing else. It is what an author reads as "no
// reason given", and the one thing a blocking guard must never hand them.
func causelessRefusal(nodeID string) string {
	return "node " + `"` + nodeID + `"`
}

// hasCauselessNode reports whether the message names a node with no cause after
// it — matched on the list separators the message itself uses, so a node whose
// cause merely STARTS with another node's id is not read as causeless.
func hasCauselessNode(message, nodeID string) bool {
	for _, item := range strings.Split(message, "; ") {
		if i := strings.LastIndex(item, "— "); i >= 0 {
			item = item[i+len("— "):]
		}
		if strings.TrimSpace(item) == causelessRefusal(nodeID) {
			return true
		}
	}
	return false
}

// TestNoRefusedFanOutIsReportedWithoutACause is the class the reported cell
// belongs to, read over the whole grid rather than over the shapes someone
// thought to attack.
//
// The cause walk names a ROUTE wherever one decided, which is what an author
// can act on. Every other arm of the verdict — a declared writer,
// `full_access:`, a `tools: []` the route does not credit, a route still
// spelled as a template — is worded by one of the two walks the verdict was
// taken over, so the two cannot drift and no refusal arrives with no reason at
// all.
//
// The cells are crossed with the SAME append axes the verdict's own gate uses
// (toolSurfaceArmAppendAxes): the verdict has a second half, read over the list
// the route RECEIVES, and it refuses cells the declared reading admits — a gate
// that walked the bare grid would leave every refusal that half adds
// unexamined, which is how the image-attachment append arrived with 18 cells
// naming a node and nothing else.
//
// The read is the program-only one (see programSurfaceEngine), which is what
// `iterion validate --exec` reports; the host's reading of the same shapes is
// driven end-to-end in fan_out_codex_declaration_test.go.
func TestNoRefusedFanOutIsReportedWithoutACause(t *testing.T) {
	for _, name := range toolSurfaceGridEnvNames {
		t.Setenv(name, "")
	}
	var causeless []string
	refused := 0
	byAxis := map[string]int{}
	for _, axis := range toolSurfaceArmAppendAxes {
		for _, c := range toolSurfaceGridCells() {
			e, wf := gridFanOutIn(c, axis.node, axis.flow)
			message := fanOutRefusal(t, e, wf)
			if message == "" {
				continue
			}
			refused++
			byAxis[axis.name]++
			if hasCauselessNode(message, "a") || hasCauselessNode(message, "b") {
				causeless = append(causeless, c.String()+" append="+axis.name+"\n      "+message)
			}
		}
	}
	if refused == 0 {
		t.Fatal("no cell of the grid was refused: this gate reads the refusals' causes, so it tests nothing")
	}
	for _, axis := range toolSurfaceArmAppendAxes {
		if byAxis[axis.name] == 0 {
			t.Errorf("the %s axis refused no cell: a cause is read only where a refusal is, so this axis tests nothing", axis.name)
		}
	}
	if len(causeless) > 0 {
		t.Errorf("%d of %d refused cell(s) name a node and no cause — the author is told which branches to change and nothing about why, on a guard that blocks the run:\n  %s",
			len(causeless), refused, strings.Join(causeless[:min(len(causeless), 10)], "\n  "))
	}
}

// TestAFanOutRefusalNamesTheArmTheVerdictUsed pins one row per arm that words
// a cause WITHOUT naming a route, and the complement: where a route decided,
// the route is still what the message names.
//
// The sentences are the verdict's own (ToolSurfaceWriteReason's walk), never a
// second wording kept here — a message that paraphrased would drift from the
// rule the verdict used, which is the whole reason one walk answers both.
func TestAFanOutRefusalNamesTheArmTheVerdictUsed(t *testing.T) {
	for _, name := range toolSurfaceGridEnvNames {
		t.Setenv(name, "")
	}
	for _, tc := range []struct {
		name   string
		cell   toolSurfaceGridCell
		mutate func(*ir.AgentNode)
		want   string
	}{
		{
			name: "a declared writer is named by the author's own line",
			cell: toolSurfaceGridCell{primary: "claw", shape: "writer", tools: []string{"bash"}},
			want: `node "a": tools: bash`,
		},
		{
			name:   "`full_access:` is named as itself",
			cell:   toolSurfaceGridCell{primary: "claw", shape: "readonly", tools: []string{"read_file"}},
			mutate: func(n *ir.AgentNode) { n.FullAccess = true },
			want:   `node "a": full_access`,
		},
		{
			name: "a `tools: []` codex branch reads the sentence the verdict took",
			cell: toolSurfaceGridCell{primary: "codex", shape: "declared_empty", tools: []string{}},
			want: "node \"a\": `tools: []` on codex names no tool, which is no proof the node holds none",
		},
		{
			name: "a route still spelled as a template is named as unresolved, not as a backend",
			cell: toolSurfaceGridCell{primary: "{{vars.b}}", shape: "readonly", tools: []string{"read_file", "glob"}},
			want: `node "a": backend {{vars.b}} is unresolved before the run`,
		},
		{
			name: "where a route decided, the route is what the message names",
			cell: toolSurfaceGridCell{primary: "codex", shape: "advisory", tools: []string{"git_diff"}},
			want: `node "a" runs on codex, where a ` + "`tools:`" + ` list does not bound what the node holds`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, wf := gridFanOut(tc.cell, tc.mutate)
			message := fanOutRefusal(t, e, wf)
			if message == "" {
				t.Fatalf("the pair was admitted: this row is about the CAUSE of a refusal, so it tests nothing")
			}
			if !strings.Contains(message, tc.want) {
				t.Errorf("the refusal reads %q, which does not carry %q — the cause must be the arm the verdict used", message, tc.want)
			}
			if hasCauselessNode(message, "a") {
				t.Errorf("the refusal names node \"a\" with no cause: %q", message)
			}
		})
	}
}
