package runtime

import (
	"slices"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// codexFanOut builds the shape the guard decides on: two branches of one
// fan_out_all router, both on codex, both declaring the same `tools:` list.
func codexFanOut(t *testing.T, tools []string, mutate func(*ir.AgentNode)) (*Engine, *ir.Workflow) {
	t.Helper()
	return codexFanOutIn(t, tools, mutate, nil)
}

// codexFanOutIn is codexFanOut with the WORKFLOW half of an opt-in: the
// image-attachment append is declared on the workflow (`attachments:` and the
// prompt body that references one), so a row for it needs both halves.
func codexFanOutIn(t *testing.T, tools []string, mutate func(*ir.AgentNode), flow func(*ir.Workflow)) (*Engine, *ir.Workflow) {
	t.Helper()
	a := agentNode("a", tools, "codex", mutate)
	b := agentNode("b", tools, "codex", mutate)
	router := &ir.RouterNode{BaseNode: ir.BaseNode{ID: "fan"}, RouterMode: ir.RouterFanOutAll}
	wf := &ir.Workflow{
		Nodes: map[string]ir.Node{"fan": router, "a": a, "b": b},
		Edges: []*ir.Edge{{From: "fan", To: "a"}, {From: "fan", To: "b"}},
	}
	if flow != nil {
		flow(wf)
	}
	// The REAL *model.ClawExecutor answers both seams admission reads, and
	// resolves `backend: codex` the way dispatch will.
	return effectiveSurfaceEngine(t, wf), wf
}

// TestTwoCodexBranchesShareAWorktreeOnlyWhereTheirListBuysTheReadOnlySandbox
// drives the guard the engine actually runs — validateWorkspaceSafety at the
// router — over the pair codex sandboxes differently.
//
// A `tools:` list on codex buys exactly one bound, the sandbox mode it maps to,
// and the mode is decided by the NAMES: `read_file` buys `read-only`, while
// `git_diff` buys `workspace-write`, where codex's built-in shell acts on the
// worktree outside the SDK's per-name callbacks. Two such branches on one
// shared worktree is the git-index race this guard exists to refuse, and the
// list they declare reads as read-only to every other backend.
//
// The second pair is the control a blocking guard owes: a legitimate read-only
// fan-out on codex still runs. The third is the escape hatch for the first,
// which the engine already documents — `readonly:` is a scheduling assertion,
// honoured before any surface is consulted.
func TestTwoCodexBranchesShareAWorktreeOnlyWhereTheirListBuysTheReadOnlySandbox(t *testing.T) {
	t.Run("workspace-write names refuse the shared worktree", func(t *testing.T) {
		e, wf := codexFanOut(t, []string{"git_diff"}, nil)
		err := e.validateWorkspaceSafety("fan", wf.Edges)
		if err == nil {
			t.Fatal("two codex branches declaring `tools: [git_diff]` were admitted onto one shared worktree: that list buys the workspace-write sandbox, where codex's built-in shell can write, and the guard exists to stop two such branches racing one git index")
		}
		rerr, ok := err.(*RuntimeError)
		if !ok || rerr.Code != ErrCodeWorkspaceSafety {
			t.Fatalf("err = %v, want the typed workspace-safety refusal", err)
		}
		// The refusal must name the route that decided, or the author is left
		// to guess which of their two branches to change and why.
		if !strings.Contains(rerr.Message, "codex") {
			t.Errorf("the refusal names no route: %q", rerr.Message)
		}
		if !strings.Contains(rerr.Hint, "readonly:") {
			t.Errorf("the refusal does not point at the escape hatch: %q", rerr.Hint)
		}
	})

	t.Run("read-only names keep the fan-out", func(t *testing.T) {
		e, wf := codexFanOut(t, []string{"read_file"}, nil)
		if err := e.validateWorkspaceSafety("fan", wf.Edges); err != nil {
			t.Fatalf("a read-only codex fan-out was refused: %v — `read_file` buys codex's read-only sandbox, so nothing these branches hold can touch the worktree", err)
		}
	})

	t.Run("readonly: is the escape hatch for the pair above", func(t *testing.T) {
		e, wf := codexFanOut(t, []string{"git_diff"}, func(n *ir.AgentNode) { n.Readonly = true })
		if err := e.validateWorkspaceSafety("fan", wf.Edges); err != nil {
			t.Fatalf("`readonly:` did not keep the refused pair eligible: %v — the author's own assertion is honoured before any tool surface is read", err)
		}
	})
}

// TestACodexFanOutIsReadOnTheListTheRouteReceivesNotTheOneItDeclared drives the
// same guard over the appends the RUNTIME makes to a node's `tools:` list.
//
// The pair above declares names codex sandboxes read-only, and that is the
// control a blocking guard owes — until one IR-visible field is added.
// `model.assembleEffectiveTools` appends `ask_user` to a non-empty list under
// `interaction:`, the board/runs MCP tools under `capabilities:`, and none of
// those names is in codex's read-only set: the list the route RECEIVES buys
// the workspace-write sandbox, where the built-in shell acts on the worktree.
// A bound read from the NAMES in a list is read from the list the route gets,
// so the pair is refused — the declared list is what the author wrote, not
// what codex is handed.
//
// The engine's own seam answers which names those are: the REAL
// *model.ClawExecutor, never a table of this test's own.
func TestACodexFanOutIsReadOnTheListTheRouteReceivesNotTheOneItDeclared(t *testing.T) {
	readOnlyOnCodex := []string{"read_file"}
	for _, tc := range []struct {
		name   string
		mutate func(*ir.AgentNode)
		flow   func(*ir.Workflow)
		adds   string
	}{
		{name: "interaction: human appends ask_user", mutate: func(n *ir.AgentNode) { n.Interaction = ir.InteractionHuman }, adds: "ask_user"},
		{name: "interaction: async appends the non-blocking pair too", mutate: func(n *ir.AgentNode) { n.Interaction = ir.InteractionAsync }, adds: "await_answers"},
		{name: "capabilities: board.read appends the board MCP tools", mutate: func(n *ir.AgentNode) { n.Capabilities = []string{"board.read"} }, adds: "mcp__iterion_board__get_issue"},
		{name: "capabilities: runs.read appends the runs MCP tools", mutate: func(n *ir.AgentNode) { n.Capabilities = []string{"runs.read"} }, adds: "mcp__iterion_runs__runs_list"},
		{
			// The one append with no `len(effectiveTools) > 0` guard, so it
			// turns an EMPTY declared list into a non-empty one — and the
			// only one whose opt-in is half workflow, half node.
			name:   "a prompt referencing an image attachment appends read_image",
			mutate: func(n *ir.AgentNode) { n.UserPrompt = "p" },
			flow: func(wf *ir.Workflow) {
				wf.Attachments = map[string]*ir.Attachment{"shot": {Name: "shot", Type: ir.AttachmentImage}}
				wf.Prompts = map[string]*ir.Prompt{"p": {Name: "p", Body: "look at {{attachments.shot}}"}}
			},
			adds: "read_image",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, wf := codexFanOutIn(t, readOnlyOnCodex, tc.mutate, tc.flow)
			node := wf.Nodes["a"]
			held := e.toolSurfaceResolver().EffectiveToolNames(node, false)
			if !slices.Contains(held, tc.adds) {
				t.Fatalf("the node holds %v, without %q: the append this row is about is gone, so the row no longer tests what it names", held, tc.adds)
			}
			if delegate.CodexDeclaredListIsReadOnly(held, true) {
				t.Fatalf("codex sandboxes %v read-only: this row rests on that list buying workspace-write, so it tests nothing", held)
			}
			err := e.validateWorkspaceSafety("fan", wf.Edges)
			if err == nil {
				t.Fatalf("two codex branches holding %v were admitted onto one shared worktree: they declared %v, which codex sandboxes read-only, and the runtime's own append put them in the workspace-write sandbox where the built-in shell can write", held, readOnlyOnCodex)
			}
			rerr, ok := err.(*RuntimeError)
			if !ok || rerr.Code != ErrCodeWorkspaceSafety {
				t.Fatalf("err = %v, want the typed workspace-safety refusal", err)
			}
			// A refusal that names no route leaves the author guessing which
			// of their two branches to change, and why.
			if !strings.Contains(rerr.Message, "codex") {
				t.Errorf("the refusal names no route: %q", rerr.Message)
			}
		})
	}
}
