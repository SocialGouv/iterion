package model

import (
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// humanLLMModelOf returns the model a human node's LLM half spends — the
// DIRECT generation executeHumanLLMWithSpend resolves from node.Model
// (resolveRoutingField, then the registry: no backend, no `default_backend`,
// no override cell) — and false for a human node that only ever pauses.
// The publisher's walk reads the kind through it (#2691): a node-kind switch
// copied into each walk read the human node as fieldless and refused it under
// any providers list, whatever the list named. The admission and the
// pre-flight read the same site through CallSites.
func humanLLMModelOf(n ir.Node) (string, bool) {
	h, ok := n.(*ir.HumanNode)
	if !ok || !ir.NodeUsesLLM(h) {
		return "", false
	}
	return h.Model, true
}
