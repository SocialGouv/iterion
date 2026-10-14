package runtime

import (
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// codexRouteAppend is one name the runtime opens for its own plumbing, and the
// IR-visible field that puts it on a CODEX node's effective list — nil where no
// codex route ever receives it.
//
// The effective surface is a UNION over every route a node may take, while the
// question it feeds is PER ROUTE: a codex node with a `fallbacks:` route onto
// claw is asked about claw's appends too. So a name no codex route receives
// must be one codex sandboxes read-only, or that node is refused a shared
// worktree for a tool it will never hold.
type codexRouteAppend struct {
	name string
	// field puts the name on a codex node's effective list; nil means no
	// codex route receives it.
	field func(*ir.AgentNode)
	// flow carries the workflow half of the opt-in, where there is one.
	flow func(*ir.Workflow)
	// why names the route that DOES receive it, for the reader.
	why string
}

// theRuntimesOwnAppends enumerates nonWorkspaceTools — the names pkg/runtime
// lets through the held-name arm because they act somewhere other than the
// shared worktree. Exhaustive by assertion below: a name added to that map
// without a row here fails, because the map is what decides which names reach
// the route question at all.
var theRuntimesOwnAppends = []codexRouteAppend{
	{
		name:  "ask_user",
		field: func(n *ir.AgentNode) { n.Interaction = ir.InteractionHuman },
		why:   "`interaction:` appends it on any backend with a non-empty list, codex included",
	},
	{
		name:  "ask_user_async",
		field: func(n *ir.AgentNode) { n.Interaction = ir.InteractionAsync },
		why:   "`interaction: async` appends the non-blocking pair on any backend, codex included",
	},
	{
		name:  "await_answers",
		field: func(n *ir.AgentNode) { n.Interaction = ir.InteractionAsync },
		why:   "`interaction: async` appends the non-blocking pair on any backend, codex included",
	},
	{
		name:  "read_image",
		field: func(n *ir.AgentNode) { n.UserPrompt = "p" },
		flow: func(wf *ir.Workflow) {
			wf.Attachments = map[string]*ir.Attachment{"shot": {Name: "shot", Type: ir.AttachmentImage}}
			wf.Prompts = map[string]*ir.Prompt{"p": {Name: "p", Body: "look at {{attachments.shot}}"}}
		},
		why: "a prompt referencing an image attachment appends it on every NON-claw backend, codex first",
	},
	{
		name: "todo_write",
		why:  "appended on the claw route alone, so codex must read it as a read",
	},
}

// TestANameNoCodexRouteReceivesMustBeReadOnlyToCodex is the class the union
// opens: the route question is asked over the names the node HOLDS on any
// route, and a name only another route adds must not buy codex's
// workspace-write sandbox, or every codex node with such a route is refused a
// shared worktree for a tool its codex route never sees.
//
// Both halves are producer-answered: whether a codex route receives the name
// comes from the executor's own seam, and the sandbox mode from the delegate's
// own classifier.
//
// Its domain is nonWorkspaceTools: the names the runtime opens for its own
// plumbing, which is what this table is exhaustive over. The condition itself
// is wider — `toolIsWorkspaceSafe` also lets readOnlyTools and the iterion MCP
// tools through to the route question — and it is read over the appends
// themselves, from the producer, by
// TestNoRouteAppendsAWorkspaceSafeNameTheCodexRouteDoesNotReceive.
func TestANameNoCodexRouteReceivesMustBeReadOnlyToCodex(t *testing.T) {
	covered := map[string]bool{}
	for _, a := range theRuntimesOwnAppends {
		covered[a.name] = true
	}
	var missing []string
	for name := range nonWorkspaceTools {
		if !covered[name] {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("%d name(s) of nonWorkspaceTools carry no row here: the map decides which names reach the codex route question, so each one says whether a codex route receives it — and, if not, that codex reads it as a read:\n  %s",
			len(missing), strings.Join(missing, "\n  "))
	}
	for _, a := range theRuntimesOwnAppends {
		t.Run(a.name, func(t *testing.T) {
			if !nonWorkspaceTools[a.name] {
				t.Fatalf("%q is not in nonWorkspaceTools: this row is about a name the held-name arm lets through to the route question, so it tests nothing", a.name)
			}
			readOnly := delegate.CodexDeclaredListIsReadOnly([]string{a.name}, true)
			if a.field == nil {
				if !readOnly {
					t.Errorf("no codex route receives %q (%s) and codex sandboxes it workspace-write: every codex node with a route that DOES append it is then refused a shared worktree for a tool it will never hold",
						a.name, a.why)
				}
				return
			}
			node := agentNode("a", []string{"read_file"}, delegate.BackendCodex, a.field)
			wf := &ir.Workflow{Nodes: map[string]ir.Node{"a": node}}
			if a.flow != nil {
				a.flow(wf)
			}
			held := model.NewProgramExecutor(wf).EffectiveToolNames(node, false)
			if !slices.Contains(held, a.name) {
				t.Errorf("a codex node under this row's own field holds %v, without %q (%s): if no codex route receives it, the name must be one codex sandboxes read-only — it is %v",
					held, a.name, a.why, readOnly)
			}
		})
	}
}

// TestACodexFanOutKeepsAClawFallbackRoute is the true complement of the codex
// arm, and the row the union's reading rests on.
//
// A codex branch declaring a read-only list with a `fallbacks:` route onto claw
// HOLDS `todo_write`, appended for the claw route alone. That name reaches the
// codex question through the union, and the pair stays eligible for a shared
// worktree for one reason: codex sandboxes `todo_write` read-only. A fan-out
// that used to run must keep running.
func TestACodexFanOutKeepsAClawFallbackRoute(t *testing.T) {
	readOnlyOnCodex := []string{"read_file", "glob"}
	cell := toolSurfaceGridCell{primary: delegate.BackendCodex, routeSet: true, route: delegate.BackendClaw, tools: readOnlyOnCodex}
	e, wf := gridFanOut(cell, nil)
	node := wf.Nodes["a"]
	held := e.toolSurfaceResolver().EffectiveToolNames(node, false)
	if !slices.Contains(held, "todo_write") {
		t.Fatalf("the node holds %v, without `todo_write`: the claw-only append this row is about is gone, so the row no longer tests what it names", held)
	}
	if !delegate.CodexDeclaredListIsReadOnly(held, true) {
		t.Fatalf("codex sandboxes %v workspace-write: this row rests on the held list buying the read-only sandbox, so it tests nothing", held)
	}
	if message := fanOutRefusal(t, e, wf); message != "" {
		t.Errorf("a codex fan-out declaring %v with a `fallbacks:` route onto claw was refused one shared worktree: %s — on the codex route the list is %v, which codex sandboxes read-only, and `todo_write` is appended for the claw route alone, where it touches no worktree",
			readOnlyOnCodex, message, readOnlyOnCodex)
	}
}
