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

// composedPairFinding is one LLM node whose composed (backend, provider)
// pair the gate refuses.
type composedPairFinding struct {
	kind ir.NodeKind
	id   string
	err  error
}

// composedPairFindings walks the workflow's LLM nodes over the sourced
// fold and returns every node whose composed pair is refused. Node order
// is sorted: the same launch must read the same refusal, whichever map
// iteration found it first.
func composedPairFindings(wf *ir.Workflow, folded []sourcedOverrideEntry) []composedPairFinding {
	if wf == nil || len(folded) == 0 {
		return nil
	}
	overrides := toModelOverrides(plainModelOverrideEntries(folded))
	ids := make([]string, 0, len(wf.Nodes))
	for id, node := range wf.Nodes {
		if _, ok := node.(ir.LLMNode); ok {
			ids = append(ids, id)
			continue
		}
		// An llm-mode router composes the same pair the executor spends —
		// the resolve reads ForNode before the router case — but
		// *ir.RouterNode satisfies no LLMNode, so it walks here or the
		// split pin stays live for it (#2637). A deterministic router
		// spends nothing, so its overrides stay unjudged.
		if r, ok := node.(*ir.RouterNode); ok && r.RouterMode == ir.RouterLLM {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	var findings []composedPairFinding
	for _, id := range ids {
		res, backendSrc, providerSrc := overrides.ForNodeFieldSources(id, wf.Nodes[id].NodeKind())
		if res.Backend == "" || res.Provider == "" {
			continue
		}
		err := llmroute.ValidateComposedPair(
			res.Backend,
			foldedEntryOrigin(folded, backendSrc, res.Backend, func(e ModelOverrideEntry) string { return e.Backend }),
			res.Provider,
			foldedEntryOrigin(folded, providerSrc, res.Provider, func(e ModelOverrideEntry) string { return e.Provider }),
		)
		if err != nil {
			findings = append(findings, composedPairFinding{kind: wf.Nodes[id].NodeKind(), id: id, err: err})
		}
	}
	return findings
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
		out = append(out, fmt.Sprintf("the routing block composes %s %q onto a pair the launch gate refuses: %v", f.kind, f.id, f.err))
	}
	return out
}
