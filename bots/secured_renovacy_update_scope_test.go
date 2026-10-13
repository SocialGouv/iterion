package bots

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/unit"
)

// `update_scope` is the one restriction secured-renovacy DELEGATES to its
// agents. `select_candidate` is deterministic: it matches the canonical
// categories, and for a free-form sentence ("only packages whose name starts
// with @internal/", a documented example) it deliberately skips its `.kind`
// filter rather than drop entries, leaving the value as a downstream hint —
// "agents see the verbatim string". `detect_stack` is told to capture that
// scope verbatim in `notes`, and `discover_outdated` reads the hint. Neither
// can do either unless a channel of ITS OWN node shows the var: a sibling's
// prompt does not count, and the session that would carry one is dropped
// whenever the provider route changes (shouldDropSessionFork).
//
// Read, never grepped: each node's prompts go through the real resolver with
// a marker as the var's value, so a rename of the reference, a move into a
// prompt the node does not use, or a drop all fail here.
func TestSecuredRenovacyShowsTheOperatorsUpdateScope(t *testing.T) {
	const varName = "update_scope"
	path := "secured-renovacy/main.bot"

	u := unit.LoadDir(path)
	if u.Merged == nil {
		t.Fatalf("%s: no unit loaded", path)
	}
	cr := ir.Compile(u.Merged)
	if cr.Workflow == nil {
		t.Fatalf("%s: no workflow compiled", path)
	}
	w := cr.Workflow
	if _, declared := w.Vars[varName]; !declared {
		t.Fatalf("%s declares no var %q — this guard's subject is gone", path, varName)
	}

	for _, id := range []string{"detect_stack", "discover_outdated"} {
		node, ok := w.Nodes[id]
		if !ok {
			t.Fatalf("%s: no node %q", path, id)
		}
		surfaces := promptSurfacesOf(w, node)
		if len(surfaces) == 0 {
			t.Fatalf("%s: node %q has no prompt surface — nothing would be read", path, id)
		}
		resolver := &model.TemplateResolver{Vars: map[string]any{varName: marker(varName)}}
		var shown strings.Builder
		for _, s := range surfaces {
			shown.WriteString(resolver.Resolve(s.body, nil, nil))
			shown.WriteString("\n")
		}
		if !strings.Contains(shown.String(), marker(varName)) {
			t.Errorf("%s: no channel of node %q shows {{vars.%s}} — the scope filter is delegated to this agent, which would then run on the whole dependency graph the operator told it to stay out of. Render {{vars.%s}} in this node's own system or user prompt",
				path, id, varName, varName)
		}
	}
}
