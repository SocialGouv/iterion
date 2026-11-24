package bundlelint_test

// The `when:`-gated half of the route reading behind the durable-memory
// check. The chain the lint reads comes from the program-only executor,
// which holds no vars, so dispatch's posture (fallbackWhenActive) drops a
// route whose gate it cannot answer — while dispatch ON A RUN that carries
// the vars walks that same route. A capability question inherits the WALK's
// posture instead (model.FallbackWhenActive keeps what it cannot answer),
// so nodeRoutes carries a gated route ungated, and what a DECLARATION
// determines stays out (iterion#2832, H-10 round r3b finding 1: the drop
// fired C274 on bundles whose only write path sat behind a `when:` gate a
// run's vars activate).

import (
	"slices"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/backend/toolcatalog"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// whengateNode is one claude_code node whose durable write path sits behind
// its `fallbacks:` route to claw: a declared list that names no writer, the
// auto-memory grant (claw's alone) waiting on the fallback.
func whengateNode(fallbacks []ir.Fallback) *ir.AgentNode {
	return &ir.AgentNode{
		BaseNode:          ir.BaseNode{ID: "nexie"},
		LLMFields:         ir.LLMFields{Backend: "claude_code"},
		AutoMemory:        "on",
		Tools:             []string{"read_file"},
		InteractionFields: ir.InteractionFields{Interaction: ir.InteractionNone},
		Fallbacks:         fallbacks,
	}
}

// assertWalkedRouteHoldsAWriter pins the engine facts the posture rests on:
// the program executor drops the gated route, dispatch with the vars keeps
// it, and the route it then walks holds a durable writer. A change in any of
// the three retires the case below — the warning would be right after all.
func assertWalkedRouteHoldsAWriter(t *testing.T, w *ir.Workflow, node *ir.AgentNode, route string) {
	t.Helper()
	exec := model.NewProgramExecutor(w)
	if got := exec.EffectiveFallbackBackends(node); slices.Contains(got, route) {
		t.Fatalf("precondition changed: the program executor KEEPS the when-gated route with no vars (%v) — the drop this posture answers is gone", got)
	}
	vexec := model.NewProgramExecutor(w)
	vexec.SetVars(map[string]any{"use_cli": "on"})
	if got := vexec.EffectiveFallbackBackends(node); !slices.Contains(got, route) {
		t.Fatalf("precondition changed: dispatch with the vars also drops the route (%v) — no run walks it and the warning would be right", got)
	}
	writers := toolcatalog.DurableWriteCanonicals()
	store := durableStoreTools(t)
	held := false
	for _, name := range vexec.EffectiveToolNamesOnRoutes(node, true, []string{route}) {
		canonical := toolcatalog.CanonicalToolName(name)
		if slices.Contains(writers, canonical) && !slices.Contains(store, canonical) {
			held = true
		}
	}
	if !held {
		t.Fatalf("precondition changed: the route dispatch walks holds no durable writer outside the store — the warning would be right")
	}
}

// TestAWhenGatedRouteItsVarsActivateCarriesItsWriter is the regression
// witness: a VALID bundle whose only write path sits behind a `when:` gate
// the run's vars activate must stay quiet — a route the program cannot
// DECIDE is not a route it can ABANDON.
func TestAWhenGatedRouteItsVarsActivateCarriesItsWriter(t *testing.T) {
	node := whengateNode([]ir.Fallback{{
		Name:    "alt",
		Backend: "claw",
		Model:   "provider/model-a",
		When:    "vars.use_cli == 'on'",
	}})
	w := wf("probe", []string{"use_cli"}, nil, nil, node)
	assertWalkedRouteHoldsAWriter(t, w, node, "claw")

	page := skillPage(pageTeaching(durableStoreTools(t)[0]))
	if got := durableMemoryDiags(w, page); len(got) != 0 {
		t.Fatalf("the when-gated claw route was dropped from the reading and C274 fired on a bundle whose chain a real dispatch walks: %+v", got)
	}

	// The control: the same node with the gate off — the engine chain keeps
	// the route on its own, and the verdict was quiet before this posture
	// existed. If this arm fires, the delta is not the gate.
	nodeUngated := whengateNode([]ir.Fallback{{Name: "alt", Backend: "claw", Model: "provider/model-a"}})
	wu := wf("probe", []string{"use_cli"}, nil, nil, nodeUngated)
	if got := durableMemoryDiags(wu, page); len(got) != 0 {
		t.Fatalf("the ungated control fires too (%+v) — the when: gate is not the delta", got)
	}
}

// TestAGatedRouteTheDeclarationDeterminesIsNotCarried pins the rule's other
// half: what the DECLARATION takes out of the chain stays out. An `action:
// skip` route completes the node where it is reached — no backend is ever
// resolved for it — so the claw writer behind it is carried by no run, and
// the gate's vars make no difference to that.
func TestAGatedRouteTheDeclarationDeterminesIsNotCarried(t *testing.T) {
	page := skillPage(pageTeaching(durableStoreTools(t)[0]))

	node := whengateNode([]ir.Fallback{{
		Name:    "alt",
		Backend: "claw",
		Model:   "provider/model-a",
		Action:  ir.FallbackActionSkip,
		When:    "vars.use_cli == 'on'",
	}})
	w := wf("probe", []string{"use_cli"}, nil, nil, node)
	if got := durableMemoryDiags(w, page); len(got) != 1 {
		t.Fatalf("a gated `action: skip` route was carried as a write path, but skip resolves no backend ever: got %d C274, want 1: %+v", len(got), got)
	}

	// The control: the same route WITHOUT the skip is a live route, and the
	// gated reading carries it — quiet, exactly as the witness above.
	nodeLive := whengateNode([]ir.Fallback{{
		Name:    "alt",
		Backend: "claw",
		Model:   "provider/model-a",
		When:    "vars.use_cli == 'on'",
	}})
	wl := wf("probe", []string{"use_cli"}, nil, nil, nodeLive)
	if got := durableMemoryDiags(wl, page); len(got) != 0 {
		t.Fatalf("the live gated route was not carried (%+v) — the skip arm is not the delta", got)
	}
}

// TestAGatedEntryInheritingTheNodeRouteAddsNoRoute pins the inheritance:
// a `fallbacks:` entry naming no backend of its own means THE NODE'S route —
// which the chain head already carries — never the workflow default. Reading
// the empty field through the workflow default would credit this node a
// claude_code route its declaration never wrote.
func TestAGatedEntryInheritingTheNodeRouteAddsNoRoute(t *testing.T) {
	node := &ir.AgentNode{
		BaseNode:          ir.BaseNode{ID: "nexie"},
		LLMFields:         ir.LLMFields{Backend: "claw"},
		AutoMemory:        "on",
		InteractionFields: ir.InteractionFields{Interaction: ir.InteractionNone},
		Fallbacks: []ir.Fallback{{
			Name:  "alt",
			Model: "provider/model-a",
			When:  "vars.use_cli == 'on'",
		}},
	}
	w := wf("probe", []string{"use_cli"}, nil, nil, node)
	w.DefaultBackend = "claude_code"

	page := skillPage(pageTeaching(durableStoreTools(t)[0]))
	if got := durableMemoryDiags(w, page); len(got) != 1 {
		t.Fatalf("the gated entry that inherits the node's route added the workflow default as a route: got %d C274, want 1 (claw with no tools list holds nothing): %+v", len(got), got)
	}
}
