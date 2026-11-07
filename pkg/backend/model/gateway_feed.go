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
// is not knowable at publish).
func RouteGatewayModelIDs(wf *ir.Workflow) []string {
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
			add(f.Model)
			if fb, hasFallbacks := n.(interface{ GetFallbacks() []ir.Fallback }); hasFallbacks {
				for _, route := range fb.GetFallbacks() {
					// A fallback with no model of its own runs the node's.
					if strings.TrimSpace(route.Model) == "" {
						add(f.Model)
						continue
					}
					add(route.Model)
				}
			}
		}
		if spec, human := humanLLMModelOf(n); human {
			add(spec)
		}
		if spec, selfRepair, agentRung := recoveryRungsOf(n); selfRepair || agentRung {
			add(spec)
		}
	}
	return ids
}
