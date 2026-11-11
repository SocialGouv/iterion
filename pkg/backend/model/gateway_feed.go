package model

import (
	"strings"

	"github.com/SocialGouv/iterion/pkg/backend/modelroute"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// RouteGatewayModelIDs collects the gateway wire ids a workflow's nodes
// can name — the coarse walk the price-feed fold's narrowing fallback
// reads (ADR-163). Coarse ON PURPOSE: it runs only when the folded table
// exceeds the channel's byte budget, and an id it misses there prices at
// the operator floor — the state every run had before the feed. Primary
// chains, fallbacks, the human/interaction and recovery models ride
// along; a templated name stays as written and matches nothing (its id
// is not knowable at publish). The models are the ones the run sends:
// overrides is the run's folded list, so a node pin, a route-scoped pin
// and a supervisor pin name their gateway ids too — the declarations as
// written are read where nothing pins them.
func RouteGatewayModelIDs(wf *ir.Workflow, overrides ModelOverrides) []string {
	if wf == nil {
		return nil
	}
	seen := map[string]bool{}
	var ids []string
	add := func(specs ...string) {
		for _, spec := range specs {
			spec = strings.TrimSpace(spec)
			if spec == "" || seen[spec] {
				continue
			}
			seen[spec] = true
			if route := modelroute.Parse(spec); route.Gateway() {
				ids = append(ids, route.Wire)
			}
		}
	}
	for _, n := range wf.Nodes {
		if f, ok := llmFieldsOf(n); ok && f != nil {
			nodeModel := f.Model
			if ov := overrides.ForNode(n.NodeID(), n.NodeKind()); ov.Model != "" {
				nodeModel = ov.Model
			}
			add(nodeModel)
			if fb, hasFallbacks := n.(interface{ GetFallbacks() []ir.Fallback }); hasFallbacks {
				for i, route := range fb.GetFallbacks() {
					route = overrides.EffectiveFallback(n.NodeID(), i, route)
					// A fallback with no model of its own runs the node's.
					if strings.TrimSpace(route.Model) == "" {
						add(nodeModel)
						continue
					}
					add(route.Model)
				}
			}
		}
		if spec, human := humanLLMModelOf(n); human {
			add(spec)
		}
		// Rung 3 generates directly on the authored spec; rung 4 runs the
		// synthetic agent `<tool>__recover`, whose model a node override
		// reaching that id replaces, as at dispatch.
		if spec, selfRepair, agentRung := recoveryRungsOf(n); selfRepair || agentRung {
			if selfRepair {
				add(spec)
			}
			if agentRung {
				agentSpec := spec
				if ov := overrides.ForNode(agentRecoveryNodeID(n.NodeID()), ir.NodeAgent); ov.Model != "" {
					agentSpec = ov.Model
				}
				add(agentSpec)
			}
		}
	}
	for _, sup := range wf.Supervisors {
		add(overrides.EffectiveSupervisorModel(sup))
	}
	return ids
}
