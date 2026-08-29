package model

import (
	"github.com/SocialGouv/iterion/pkg/backend/modelspecs"
)

// Merging the dynamic aggregator over the curated table.
//
// The registry itself lives in pkg/backend/modelspecs, a leaf package, so that
// pkg/backend/cost can price a run from published rates. This half — deciding
// what the aggregator is allowed to override — stays here, with the curated
// table it overrides: curated-is-the-floor is a model/ policy, and a registry
// that knew about it would have to know about the table too.

// mergeSpec overlays an aggregator spec onto the curated fallback. Anything the
// aggregator has no answer for (zero number, nil flag — including a field
// consensusSpec zeroed because publishers disagreed) leaves the curated value
// standing.
func mergeSpec(spec modelspecs.Spec, curated ModelCapabilities) ModelCapabilities {
	out := curated
	// A fetched ContextWindow>0 overrides the static one.
	if spec.ContextWindow > 0 {
		out.ContextWindow = spec.ContextWindow
	}
	// Max output has no curated counterpart today, but the >0 guard still
	// matters: it keeps "the publishers disagreed" from erasing a value a
	// later curated branch may supply.
	if spec.MaxOutputTokens > 0 {
		out.MaxOutputTokens = spec.MaxOutputTokens
	}
	// Flags fall back to heuristics when the source omits them (nil pointer).
	if spec.Reasoning != nil {
		out.Reasoning = *spec.Reasoning
	}
	if spec.ToolCall != nil {
		out.ToolCall = *spec.ToolCall
	}
	if spec.Temperature != nil {
		out.Temperature = *spec.Temperature
	}
	// Pricing has no curated counterpart to fall back on, so it is carried
	// through as published: zero stays zero and means "the aggregator had no
	// price", never "free".
	out.InputCostPerM = spec.InputCostPerM
	out.OutputCostPerM = spec.OutputCostPerM
	return out
}
