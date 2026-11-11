package runview

import (
	"fmt"
	"sort"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/llmroute"
)

// The composed-pair check (#2637). The per-entry write gate
// (llmroute.validateOverride) judges each override entry alone, but the
// launch resolves per FIELD: a backend pinned by one entry and a provider
// pinned by another compose into a pair the per-entry check never saw —
// a pin certified at write time the harness ignores at dispatch. The
// check runs on the composed per-node override at the ONE launch fold,
// with the same three refusals naming the two entries (and their levels)
// that produced the pair.

// composedPairFinding is one LLM node — or one of its `fallbacks:` routes —
// whose composed (backend, provider) pair the gate refuses.
type composedPairFinding struct {
	kind ir.NodeKind
	id   string
	// route is the route's site key (`node#name`) for a route's pair, ""
	// for the node's own.
	route string
	err   error
	// element marks a gateway crossing finding: an element dispatch
	// refuses, rather than a pair the gate refuses.
	element bool
	// undecided marks a finding a slot dial or an undecided var decides
	// at the runner: judged on the dial's default text, it warns at launch
	// and never refuses.
	undecided bool
}

// subject names the finding's site for a sentence.
func (f composedPairFinding) subject() string {
	if f.route != "" {
		return fmt.Sprintf("%s %q, fallbacks: route %q", f.kind, f.id, f.route)
	}
	return fmt.Sprintf("%s %q", f.kind, f.id)
}

// composedPairFindings walks the workflow's LLM nodes over the sourced
// fold and returns every node — and every authored route — whose composed
// pair is refused, then every element the gateway crossing check refuses
// (gatewayCrossingFindings). Node order is sorted: the same launch must
// read the same refusal, whichever map iteration found it first.
func composedPairFindings(wf *ir.Workflow, folded []sourcedOverrideEntry) []composedPairFinding {
	if wf == nil || len(folded) == 0 {
		return nil
	}
	overrides := toModelOverrides(plainModelOverrideEntries(folded))
	var findings []composedPairFinding
	for _, id := range overrideSiteNodeIDs(wf) {
		res, src := overrides.ForNodeFieldSources(id, wf.Nodes[id].NodeKind())
		if res.Backend != "" && res.Provider != "" {
			err := llmroute.ValidateComposedPair(
				res.Backend,
				foldedEntryOrigin(folded, src.Backend, res.Backend, func(e ModelOverrideEntry) string { return e.Backend }),
				res.Provider,
				foldedEntryOrigin(folded, src.Provider, res.Provider, func(e ModelOverrideEntry) string { return e.Provider }),
			)
			if err != nil {
				findings = append(findings, composedPairFinding{kind: wf.Nodes[id].NodeKind(), id: id, err: err})
			}
		}
		findings = append(findings, routePairFindings(wf.Nodes[id], overrides, res.Backend, src.Backend, folded)...)
	}
	return append(findings, gatewayCrossingFindings(wf, overrides, folded)...)
}

// overrideSiteNodeIDs is, sorted, every node whose LLM call a launch
// override re-pins: the LLM nodes and the llm-mode routers. An llm-mode
// router composes the same pair the executor spends — the resolve reads
// ForNode before the router case — but *ir.RouterNode satisfies no
// LLMNode, so it is listed here or a split pin stays live for it (#2637).
// A deterministic router spends nothing, so its overrides stay unjudged.
// The launch's composed checks — the pair, the gateway crossing, the
// route crossing screen — walk this one list.
func overrideSiteNodeIDs(wf *ir.Workflow) []string {
	ids := make([]string, 0, len(wf.Nodes))
	for id, node := range wf.Nodes {
		if _, ok := node.(ir.LLMNode); ok {
			ids = append(ids, id)
			continue
		}
		if r, ok := node.(*ir.RouterNode); ok && r.RouterMode == ir.RouterLLM {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

// authoredRoute is one of a node's authored `fallbacks:` routes as a
// launch reads it: its site key (its position when the author named it
// none), the author's fields, the
// fields the route-scoped cells pin (ForRoute) and the route those pins
// make (EffectiveFallback) — the reading dispatch's chain builds from.
type authoredRoute struct {
	site      string
	authored  ir.Fallback
	pin       model.NodeModelOverride
	effective ir.Fallback
}

// authoredRoutes lists a node's authored `fallbacks:` routes — a launch
// stage or a policy rung is pinned where it is written, never by a route
// cell — in declaration order.
func authoredRoutes(node ir.Node, overrides model.ModelOverrides) []authoredRoute {
	gf, ok := node.(interface{ GetFallbacks() []ir.Fallback })
	if !ok {
		return nil
	}
	var out []authoredRoute
	for i, fb := range gf.GetFallbacks() {
		if fb.RunStageSet || fb.Policy {
			continue
		}
		out = append(out, authoredRoute{
			site:      routeSiteKey(node.NodeID(), i, fb.Name),
			authored:  fb,
			pin:       overrides.ForRoute(node.NodeID(), i, fb.Name),
			effective: overrides.EffectiveFallback(node.NodeID(), i, fb),
		})
	}
	return out
}

// routePairFindings is the composed-pair check on a node's authored
// `fallbacks:` routes: a route-scoped cell's provider beside the backend
// the route runs on when an override decided it — the route cell's own,
// else, for a route that names no backend of its own, the node's
// overridden backend (nodeBackend, written by nodeBackendSrc's rule).
func routePairFindings(node ir.Node, overrides model.ModelOverrides, nodeBackend string, nodeBackendSrc model.FieldSource, folded []sourcedOverrideEntry) []composedPairFinding {
	var out []composedPairFinding
	for _, r := range authoredRoutes(node, overrides) {
		if r.pin.Provider == "" {
			continue
		}
		site := model.FieldSource{Selector: r.site}
		backend, backendSrc := r.pin.Backend, site
		if backend == "" && r.authored.Backend == "" {
			backend, backendSrc = nodeBackend, nodeBackendSrc
		}
		if backend == "" {
			continue
		}
		err := llmroute.ValidateComposedPair(
			backend,
			foldedEntryOrigin(folded, backendSrc, backend, func(e ModelOverrideEntry) string { return e.Backend }),
			r.pin.Provider,
			foldedEntryOrigin(folded, site, r.pin.Provider, func(e ModelOverrideEntry) string { return e.Provider }),
		)
		if err != nil {
			out = append(out, composedPairFinding{kind: node.NodeKind(), id: node.NodeID(), route: r.site, err: err})
		}
	}
	return out
}

// routeSiteKey is a route's site key — the selector a route-scoped cell
// names it by: `node#name`, its index when the author named none.
func routeSiteKey(nodeID string, index int, name string) string {
	if name != "" {
		return nodeID + llmroute.SelectorRouteSeparator + name
	}
	return fmt.Sprintf("%s%s%d", nodeID, llmroute.SelectorRouteSeparator, index)
}

// foldedEntryOrigin names the folded entry a won field came from: the
// LAST entry matching (selector, value) — the later rule wins a tie, so
// the last match is the rule ForNode picked. The shape fallback is
// unreachable through the fold; it names no level rather than inventing
// one.
func foldedEntryOrigin(folded []sourcedOverrideEntry, src model.FieldSource, value string, field func(ModelOverrideEntry) string) string {
	for i := len(folded) - 1; i >= 0; i-- {
		e := folded[i]
		if src.Selector == e.Selector && field(e.ModelOverrideEntry) == value {
			return fmt.Sprintf("%s %q", e.origin, e.Selector)
		}
	}
	return "an override entry"
}

// composedPairWarnings renders the findings as the write-surface screen's
// WARNING lines (C1xx posture): the launch refusal stays the gate.
func composedPairWarnings(wf *ir.Workflow, folded []sourcedOverrideEntry) []string {
	findings := composedPairFindings(wf, folded)
	if len(findings) == 0 {
		return nil
	}
	out := make([]string, 0, len(findings))
	for _, f := range findings {
		out = append(out, "the routing block "+f.sentence())
	}
	return out
}

// sentence renders a finding after "the routing block" (the write screen)
// or "the launch's override cells" (a launch's undecided warning).
func (f composedPairFinding) sentence() string {
	switch {
	case f.undecided:
		return fmt.Sprintf("composes %s onto an element dispatch refuses unless the runner's environment decides a dial otherwise: %v", f.subject(), f.err)
	case f.element:
		return fmt.Sprintf("composes %s onto an element dispatch refuses: %v", f.subject(), f.err)
	}
	return fmt.Sprintf("composes %s onto a pair the launch gate refuses: %v", f.subject(), f.err)
}
