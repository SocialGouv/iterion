package bots

import (
	"github.com/SocialGouv/iterion/pkg/dsl/unit/unittest"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/unit"
)

// Evoly's investigation is re-entered: the operator answers `ask_continue`
// with `refine` plus a detail, and the refine edge carries that detail as the
// turn's new steering. The node's prompt therefore renders TWO references for
// one name, because from turn 2 on they are two different values:
// `{{input.scope_notes}}` is what the selected edge mapped for this turn, and
// `{{vars.scope_notes}}` is the brief the launch carried — the dispatcher's
// issue title + body, unchanged for the life of the run. Reading only the var
// hides the operator's newest words; reading only the input drops their
// mission from turn 2 (and shows nothing at all when they refine without
// typing a detail, since `derive_continue.detail` is then empty).
//
// The input read is only whole while EVERY edge into the node fills the key —
// the input is built from the selected edges' `with` mappings (plus, on a loop
// re-entry, the forward edges they overlay: pkg/runtime/engine_resolve.go,
// `overlayForward`). What a route that stops mapping the key delivers is NOT
// an empty value:
//
//   - an exclusive FORWARD route leaves the key absent, and an unresolvable
//     reference is kept as written, so the model reads the literal
//     `{{input.scope_notes}}` placeholder
//     (pkg/backend/model/template_resolver.go);
//   - the bounded BACK-edge only overlays, so the forward edge's value
//     survives — the stale launch value, which is the defect this pin exists
//     to stop (pkg/runtime/incoming_test.go,
//     TestSelectedIncoming_LoopReentryOverlaysPartialBackEdge).
//
// `TestCatalogInputReadsAreMappedByAnIncomingEdge` proves only that SOME edge
// maps a read field — the weaker property, and deliberately so, since a loop's
// back-edge legitimately overlays only what changes per iteration. Here the
// value changes per turn on every route, which is what makes the stronger
// property owed.
//
// Mutations that must redden it: drop `scope_notes` from either
// `-> investigate` edge in bots/evolve/main.bot, or drop either steering line
// from investigate_user.
func TestEvolveInvestigateTakesItsSteeringFromEveryIncomingEdge(t *testing.T) {
	const (
		bot  = "evolve/main.bot"
		node = "investigate"
	)

	u := unittest.Whole(unit.LoadDir(bot))
	if u.Merged == nil {
		t.Fatalf("%s: the bundle did not parse: %v", bot, u.Diagnostics)
	}
	cr := ir.Compile(u.Merged)
	if cr.Workflow == nil {
		t.Fatalf("%s: the bundle did not compile: %v", bot, cr.Diagnostics)
	}
	w := cr.Workflow

	llm, ok := w.Nodes[node].(ir.LLMNode)
	if !ok {
		t.Fatalf("%s: %q is not an LLM node (%T) — the node was renamed and this guard proves nothing",
			bot, node, w.Nodes[node])
	}

	// The subject is read off the node's own prompts, not listed here: a field
	// a later turn of authoring renders is held the same day it is rendered.
	fields := map[string]bool{}
	fromVars := map[string]bool{}
	f := llm.GetLLMFields()
	for _, pn := range []string{f.SystemPrompt, f.UserPrompt} {
		p := w.Prompts[pn]
		if p == nil {
			continue
		}
		for _, ref := range p.TemplateRefs {
			if len(ref.Path) == 0 || strings.HasPrefix(ref.Path[0], "_") {
				continue
			}
			switch ref.Kind {
			case ir.RefInput:
				fields[ref.Path[0]] = true
			case ir.RefVars:
				fromVars[ref.Path[0]] = true
			}
		}
	}
	read := slices.Sorted(maps.Keys(fields))
	if len(read) == 0 {
		t.Fatalf("%s: no {{input.*}} reference in %q's own prompts — either the per-turn steering went back to {{vars.*}} (the defect: the model then sees the launch value on every refine turn), or the prompts were renamed and this guard is blind",
			bot, node)
	}

	var seen int
	for _, e := range w.Edges {
		if e == nil || e.To != node {
			continue
		}
		seen++
		var mapped []string
		for _, m := range e.With {
			if m != nil {
				mapped = append(mapped, m.Key)
			}
		}
		for _, field := range read {
			if slices.Contains(mapped, field) {
				continue
			}
			t.Errorf("%s: edge %s -> %s (%s) does not map %q, which %q's own prompt renders as {{input.%s}}: on that route the model is not handed this turn's value — a forward route leaves the key absent and the model reads the literal {{input.%s}} placeholder, a bounded back-edge falls back to the forward edge's value (the stale launch value). Map it on the edge (the entry route carries the launch var, a refine route carries the operator's new detail), or stop reading it from the input",
				bot, e.From, e.To, edgeGuard(e), field, node, field, field)
		}
	}

	// Both routes have to exist for the assertion above to mean anything: the
	// entry pass and the refine loop-back. A rename that left one would check
	// the other alone and stay green.
	if seen < 2 {
		t.Fatalf("%s: %d edge(s) reach %q, want the entry pass and the refine loop-back — the graph was rewired and this guard checks less than it says",
			bot, seen, node)
	}

	// The other half of the same line: a per-turn read must not COST the
	// launch brief. A field the workflow also declares as a var is a launch
	// value the dispatcher fills, and the input overwrites it from turn 2, so
	// the node's prompts owe `{{vars.<field>}}` as well — the two are
	// different values, not two spellings of one.
	for _, field := range read {
		if w.Vars[field] == nil || fromVars[field] {
			continue
		}
		t.Errorf("%s: %q's prompts render {{input.%s}} but not {{vars.%s}}, which the workflow declares as a launch var: from the second turn the refine edge overwrites the key, so the brief the launch carried leaves the model's screen — and a refine with no detail typed shows no steering at all. Render both, labelled (the launch brief, then this turn's steering)",
			bot, node, field, field)
	}
}
