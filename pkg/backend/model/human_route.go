package model

import (
	"strings"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/secrets"
)

// humanLLMModelOf returns the model a human node's LLM half spends — the
// DIRECT generation executeHumanLLMWithSpend resolves from node.Model
// (resolveRoutingField, then the registry: no backend, no `default_backend`,
// no override cell) — and false for a human node that only ever pauses.
// The ONE reader both launch-time walks consult for the kind (#2691): a
// node-kind switch copied into each walk read the human node as fieldless
// and refused it under any providers list, whatever the list named.
func humanLLMModelOf(n ir.Node) (string, bool) {
	h, ok := n.(*ir.HumanNode)
	if !ok || !ir.NodeUsesLLM(h) {
		return "", false
	}
	return h.Model, true
}

// directWireRoute reads a direct generation's route for the wire guard:
// the model spec alone routes (the registry resolves its `provider/`
// prefix), so the prefix is the hint and there is no backend to pick a
// provider from. A prefix the walk cannot read — empty, or a `{{vars.*}}`
// span the view left written — keeps the route unreadable: the
// conservative reading, the guard stays armed.
func directWireRoute(id, mdl string) WireRoute {
	r := WireRoute{NodeID: id, Model: strings.TrimSpace(ir.ExpandEnvWithDefault(mdl))}
	r.Hint = providerFromModelPrefix(r.Model)
	if GLMOnAnthropicWire(r.Model) {
		// A GLM id on the anthropic wire spends z.ai's key (prefixOrWiden
		// reads it the same way).
		r.Hint = string(secrets.ProviderZAI)
	}
	r.Readable = r.Hint != ""
	return r
}
