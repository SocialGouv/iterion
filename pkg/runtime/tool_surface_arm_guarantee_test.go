package runtime

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// The extent of the EFFECTIVE-surface half of shared-worktree admission, over
// the grid's cell space crossed with the appends below. Two numbers, because
// the half has two readings and they answer for different things: the engine's
// own vocabulary over the names the node holds, and the route question asked a
// second time over the list the route RECEIVES.
//
// The frozen grid under testdata/ cannot carry either: it reads admission with
// a NIL tool-surface resolver, which returns before this half (see
// toolSurfaceGridRows). This is where the half is measured.
const (
	toolSurfaceHeldNameCells  = 614
	toolSurfaceRouteListCells = 706
)

// toolSurfaceArmAppendAxes are the IR-visible opt-ins that make the list a
// route RECEIVES differ from the one its author wrote — the whole reason this
// half exists. Each is a workflow and a node mutation, so a workflow-level
// opt-in is exercised as well as the per-node ones.
//
// `none` is in the table on purpose, and it must add NO refusal: with no
// opt-in the union over routes still differs from the declared list — the
// runtime appends `todo_write` on a claw route — and that name is workspace-
// safe to the engine and read-only to codex, which is why charging it to a
// codex route refuses nothing (pinned at the class in
// TestANameNoCodexRouteReceivesMustBeReadOnlyToCodex).
var toolSurfaceArmAppendAxes = []struct {
	name   string
	node   func(*ir.AgentNode)
	flow   func(*ir.Workflow)
	expect string
	// probe is the backend TestEveryAppendAxisReachesTheSurfaceItNames reads
	// the axis on: several appends are per route, so the axis names the one
	// route that receives it.
	probe string
}{
	{name: "none"},
	{
		name:   "interaction: human",
		node:   func(n *ir.AgentNode) { n.Interaction = ir.InteractionHuman },
		expect: "ask_user",
		probe:  delegate.BackendCodex,
	},
	{
		name:   "interaction: async",
		node:   func(n *ir.AgentNode) { n.Interaction = ir.InteractionAsync },
		expect: delegate.AwaitAnswersToolName,
		probe:  delegate.BackendCodex,
	},
	{
		name:   "capabilities: board.read",
		node:   func(n *ir.AgentNode) { n.Capabilities = []string{"board.read"} },
		expect: "mcp__iterion_board__get_issue",
		probe:  delegate.BackendCodex,
	},
	{
		name:   "capabilities: runs.read",
		node:   func(n *ir.AgentNode) { n.Capabilities = []string{"runs.read"} },
		expect: "mcp__iterion_runs__runs_list",
		probe:  delegate.BackendCodex,
	},
	{
		name:   "auto_memory: on at the workflow",
		flow:   func(wf *ir.Workflow) { wf.AutoMemory = "on" },
		expect: "write_file",
		probe:  delegate.BackendClaw,
	},
	{
		name: "an image attachment the prompt references",
		node: func(n *ir.AgentNode) { n.UserPrompt = "p" },
		flow: func(wf *ir.Workflow) {
			wf.Attachments = map[string]*ir.Attachment{"shot": {Name: "shot", Type: ir.AttachmentImage}}
			wf.Prompts = map[string]*ir.Prompt{"p": {Name: "p", Body: "look at {{attachments.shot}}"}}
		},
		expect: "read_image",
		probe:  delegate.BackendCodex,
	},
}

// toolSurfaceArmPair is one cell under one append axis: what admission reads
// with no tool-surface resolver (the reading the frozen grid holds) and what it
// reads with the executor answering.
type toolSurfaceArmPair struct {
	key       string
	cell      toolSurfaceGridCell
	axis      string
	node      *ir.AgentNode
	exec      *model.ClawExecutor
	held      []string
	withoutEx bool
	withEx    bool
}

// toolSurfaceArmPairs reads both sides over every cell of the grid crossed with
// every append axis, with the PROGRAM-only reading of the real
// *model.ClawExecutor (see programSurfaceEngine): a gate over a space this wide
// must answer the same on every machine, and the host's reading resolves a
// route left to dispatch from the machine's own credentials.
//
// The BACKEND seam is answered on both sides, and only the surface seam moves.
// The engine reads the two from one executor — both answered, or both the
// refusing stand-ins (see toolSurfaceResolver and backendResolver) — so a
// baseline that left the route to the IR while the surface seam answered would
// measure a pair no caller can produce, and report cells as moved by this half
// that the route reading had already refused.
func toolSurfaceArmPairs(t *testing.T) []toolSurfaceArmPair {
	t.Helper()
	for _, name := range toolSurfaceGridEnvNames {
		t.Setenv(name, "")
	}
	var pairs []toolSurfaceArmPair
	for _, axis := range toolSurfaceArmAppendAxes {
		for _, c := range toolSurfaceGridCells() {
			node := c.node()
			if axis.node != nil {
				axis.node(node)
			}
			wf := &ir.Workflow{DefaultBackend: c.dflt, Nodes: map[string]ir.Node{"a": node}}
			if axis.flow != nil {
				axis.flow(wf)
			}
			exec := model.NewProgramExecutor(wf)
			pairs = append(pairs, toolSurfaceArmPair{
				key:       fmt.Sprintf("%s append=%s", c.String(), axis.name),
				cell:      c,
				axis:      axis.name,
				node:      node,
				exec:      exec,
				held:      exec.EffectiveToolNames(node, false),
				withoutEx: isMutatingNodeCtx(node, c.dflt, exec, nil, false),
				withEx:    isMutatingNodeCtx(node, c.dflt, exec, exec, false),
			})
		}
	}
	return pairs
}

// TestEveryAppendAxisReachesTheSurfaceItNames keeps the axes honest: an axis
// whose append stopped landing would make every assertion below pass over an
// empty question. The producer answers — the executor's own seam, never a list
// kept here.
func TestEveryAppendAxisReachesTheSurfaceItNames(t *testing.T) {
	for _, name := range toolSurfaceGridEnvNames {
		t.Setenv(name, "")
	}
	for _, axis := range toolSurfaceArmAppendAxes {
		if axis.expect == "" {
			continue
		}
		t.Run(axis.name, func(t *testing.T) {
			node := agentNode("a", []string{"read_file"}, axis.probe, axis.node)
			wf := &ir.Workflow{Nodes: map[string]ir.Node{"a": node}}
			if axis.flow != nil {
				axis.flow(wf)
			}
			held := model.NewProgramExecutor(wf).EffectiveToolNames(node, false)
			found := false
			for _, n := range held {
				if n == axis.expect {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("a node under %s holds %v, without %q: this axis no longer makes the list a route receives differ from the declared one, so every cell it carries tests nothing",
					axis.name, held, axis.expect)
			}
		})
	}
}

// heldListArm is one reason a cell admitted on its DECLARATION is refused once
// the executor says what the node will hold: the clause the engine words it
// with, the reading that emitted it, and the cells it answers for.
//
// Keyed on the engine's own message, like theArmsThatWiden: this table
// re-derives nothing the code under test decides. A new arm matches nothing and
// fails; an arm whose reach changed fails with its own number.
type heldListArm struct {
	arm         string
	clause      string
	viaFallback bool
	cells       int
}

func (a heldListArm) matches(reason string) bool {
	return strings.Contains(reason, a.clause) && strings.HasPrefix(reason, fallbackReasonPrefix) == a.viaFallback
}

// theArmsTheHeldListAdds are the route readings that refuse a cell the declared
// reading admitted, with the cells each answers for.
var theArmsTheHeldListAdds = []heldListArm{
	{
		arm:    "codex, where the names the route RECEIVES decide the sandbox mode",
		clause: "maps the `tools:` list onto a sandbox mode, and what this node declares buys the workspace-write one, where codex's built-in shell acts on the worktree (#1727)",
		cells:  490,
	},
	{
		arm:         "a `fallbacks:` route onto codex, where the names the route RECEIVES decide the sandbox mode",
		clause:      "maps the `tools:` list onto a sandbox mode, and what this node declares buys the workspace-write one, where codex's built-in shell acts on the worktree (#1727)",
		viaFallback: true,
		cells:       126,
	},
	{
		arm:         "a `fallbacks:` route that never receives the list, on a node whose own list is empty until an append lands on it",
		clause:      "never receives the `tools:` list, so a declared bound is dropped (#1652)",
		viaFallback: true,
		cells:       54,
	},
	{
		arm:         "a `fallbacks:` route onto claude_code, on a node whose own list is empty until an append lands on it",
		clause:      "takes the `tools:` list as --disallowedTools over a closed native roster iterion does not own, so a declared list bounds nothing (#1652)",
		viaFallback: true,
		cells:       18,
	},
	{
		arm:         "a `fallbacks:` route still spelled as a template, on a node whose own list is empty until an append lands on it",
		clause:      "is unresolved before the run, so no backend it may take bounds what the node holds",
		viaFallback: true,
		cells:       18,
	},
}

// TestTheEffectiveSurfaceHalfOnlyRefusesAndOnlyThroughANamedArm is the
// guarantee for the half the frozen grid cannot reach.
//
// Admission's first reading is the node's DECLARATION and the route it may
// take, which the frozen table measures. Its second is what the node will
// actually HOLD, and that one has two readings: the engine's vocabulary over
// the held names (a question channel and the board MCP tools touch no
// worktree), then the route question asked a second time over the list the
// route RECEIVES.
//
// A cell that moves here stops being eligible for a parallel read-only
// fan-out, so this half owes what the declared one owes: it only ever REFUSES,
// every refusal it adds joins through a NAMED arm with the engine's own
// wording, and the extent is counted rather than described.
//
// The route reading adds two kinds, and the table names both. One is codex,
// where the held names pick a sandbox MODE. The other is a node whose own
// `tools:` list is empty — the route walk is skipped for it, since an omitted
// list bounds nothing to begin with — and which an append gives a non-empty
// list, so the walk runs and a `fallbacks:` route no declaration can bound
// answers. That refusal stands on its own merits rather than on the append:
// the node holds whatever that route's native toolset is, and `readonly:` is
// the author's hatch, honoured before any surface is read.
func TestTheEffectiveSurfaceHalfOnlyRefusesAndOnlyThroughANamedArm(t *testing.T) {
	pairs := toolSurfaceArmPairs(t)
	var freed, unexplained []string
	byArm := map[string]int{}
	byAxis := map[string]int{}
	counted := map[string]int{}
	for _, p := range pairs {
		if p.withoutEx == p.withEx {
			continue
		}
		if !p.withEx {
			freed = append(freed, p.key)
			continue
		}
		byAxis[p.axis]++
		unsafe := ""
		for _, n := range p.held {
			if !toolIsWorkspaceSafe(n) {
				unsafe = n
				break
			}
		}
		if unsafe != "" {
			byArm["held name"]++
			continue
		}
		byArm["route list"]++
		fields := p.node.GetLLMFields()
		reason := unrestrictedRouteWriteReason(p.node, *fields, p.held, p.cell.dflt, p.exec, nil, unnamedRouteUnread)
		var hit []string
		for _, arm := range theArmsTheHeldListAdds {
			if arm.matches(reason) {
				hit = append(hit, arm.arm)
			}
		}
		if len(hit) != 1 {
			unexplained = append(unexplained, fmt.Sprintf("%s\n      holds %v\n      reason %q\n      matches %d arms %v", p.key, p.held, reason, len(hit), hit))
			continue
		}
		counted[hit[0]]++
	}
	sort.Strings(freed)
	sort.Strings(unexplained)
	if len(freed) > 0 {
		t.Errorf("%d cell(s) became eligible for a shared worktree once the executor answered — reading what a node HOLDS may only refuse, and admitting a node is the race the guard exists to prevent:\n  %s",
			len(freed), strings.Join(freed[:min(len(freed), 20)], "\n  "))
	}
	if len(unexplained) > 0 {
		t.Errorf("%d cell(s) lost their shared-worktree eligibility on the route reading under a cause no arm of this table claims — a fan-out that used to run now fails at the router, so name the arm with its clause and its cells, or fix the reading:\n  %s",
			len(unexplained), strings.Join(unexplained[:min(len(unexplained), 10)], "\n  "))
	}
	if byArm["held name"] != toolSurfaceHeldNameCells {
		t.Errorf("the held-name reading refused %d cells, the measured move is %d (by append axis: %v): the refusals this half adds are reviewed one by one, not inferred",
			byArm["held name"], toolSurfaceHeldNameCells, byAxis)
	}
	if byArm["route list"] != toolSurfaceRouteListCells {
		t.Errorf("the route reading refused %d cells, the measured move is %d (by append axis: %v): the refusals this half adds are reviewed one by one, not inferred",
			byArm["route list"], toolSurfaceRouteListCells, byAxis)
	}
	for _, arm := range theArmsTheHeldListAdds {
		if counted[arm.arm] != arm.cells {
			t.Errorf("arm %q refused %d cells, want %d", arm.arm, counted[arm.arm], arm.cells)
		}
	}
}
