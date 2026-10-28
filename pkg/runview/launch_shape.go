package runview

import (
	"fmt"
	"sort"
	"strings"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// The card lanes' launch shape (ADR-129 §7, #2665): a board card's vars
// are the delivery payload's own answer (ensureBoardCard copies the
// route's args var, the PR carry list and the bot's declared hand-offs —
// the comment body, the PR url), and no operator gesture follows the
// launch. A routing field a {{vars.*}} span leaves undecided there can
// never be certified later, and a gate may only certify the route the run
// will spend — so the field refuses the launch instead of running
// undecided. The direct lanes keep their shape: an operator's `${…}` dial
// belongs to the deployment, not to the text, and its own {{vars.*}}
// spans resolve or stay undecided under the per-reader pre-flights as
// before.

// refuseUndecidedRoutingFields refuses the first routing field — sorted
// by node, then field — whose launch vars reading cannot decide it, so
// the same launch reads the same refusal whichever map iteration ran
// first. Only a {{vars.*}} span the view cannot resolve stays written;
// an env dial is not this check's subject.
func refuseUndecidedRoutingFields(wf *ir.Workflow, spec *LaunchSpec) error {
	if wf == nil {
		return nil
	}
	// The walk judges the nodes' own routing fields; the run-level
	// fallback chain is a separate route set this shape does not judge.
	view := model.RouteViewForLaunch(wf, toModelOverrides(spec.ModelOverrides), nil, spec.Vars, spec.TrustedVarNames)
	ids := make([]string, 0, len(wf.Nodes))
	for id, node := range wf.Nodes {
		if routingFieldsOf(node) != nil {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	for _, id := range ids {
		node := wf.Nodes[id]
		fields := routingFieldsOf(node)
		for _, f := range []struct{ name, raw string }{
			{"backend", fields.Backend},
			{"model", fields.Model},
			{"provider", fields.Provider},
			{"effort", fields.ReasoningEffort},
		} {
			if f.raw == "" {
				continue
			}
			if resolved := view.ResolvedRoutingField(f.raw); strings.Contains(resolved, "{{vars.") {
				return fmt.Errorf("runview: %s %q routes on %s %q, which this launch's vars cannot decide — a card's vars are the delivery payload's answer and no operator gesture follows; pin the field or drop the template", node.NodeKind(), id, f.name, f.raw)
			}
		}
	}
	return nil
}

// routingFieldsOf returns the four-field routing block a launch resolves
// for the node: every LLM node's own, an llm-mode router's (the resolve
// takes ir.Node before the router case) — nil otherwise.
func routingFieldsOf(node ir.Node) *ir.LLMFields {
	if llm, ok := node.(ir.LLMNode); ok {
		return llm.GetLLMFields()
	}
	if r, ok := node.(*ir.RouterNode); ok && r.RouterMode == ir.RouterLLM {
		return &r.LLMFields
	}
	return nil
}
