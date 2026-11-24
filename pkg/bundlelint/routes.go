package bundlelint

import (
	"slices"

	"github.com/SocialGouv/iterion/pkg/backend/backendcaps"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// NodeBackends returns the backends a capability check must consider for a
// node's declaration. The engine resolves a copy with all fallback conditions
// open: a capability check cannot discard a route whose vars it does not hold.
// Skip and inheritance still belong to the engine, as does expansion of an
// explicit ${VAR:-default}. Unresolved template names remain unresolved;
// unnamed routes expand to the registered candidates, never host defaults.
func NodeBackends(llm ir.LLMNode, workflow *ir.Workflow) []string {
	// Preserve the concrete type: the engine reads routing fields from it.
	// Clone the slice too, so auditing does not change a later dispatch.
	switch node := llm.(type) {
	case *ir.AgentNode:
		copy := *node
		copy.Fallbacks = unconditionalFallbacks(node.Fallbacks)
		llm = &copy
	case *ir.JudgeNode:
		copy := *node
		copy.Fallbacks = unconditionalFallbacks(node.Fallbacks)
		llm = &copy
	default:
		// A future LLM node needs the same concrete-copy treatment before
		// an audit can narrow its possible routes.
		return backendcaps.Names()
	}
	return nodeRoutes(model.NewProgramExecutor(workflow), llm, workflow)
}

func unconditionalFallbacks(fallbacks []ir.Fallback) []ir.Fallback {
	copy := slices.Clone(fallbacks)
	for i := range copy {
		copy[i].When = ""
	}
	return copy
}
