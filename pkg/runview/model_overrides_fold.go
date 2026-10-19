package runview

import "github.com/SocialGouv/iterion/pkg/store"

// The ONE launch fold of the routing policy's override cells under the
// launch's own model_overrides (ADR-129 §6). The ADR's precedence line:
//
//	node DSL field < policy-resolved overrides (platform < org < team < bot
//	< binding < run routing layer) < the launch's own model_overrides
//
// The policy levels resolve through the routing snapshot
// (resolveRunLLMRoutePolicy → store.RunLLMRoutePolicy), whose Overrides
// carry the winning cell per (selector, field). The launch folds them ONCE,
// through this helper, and the folded list is what the launch validates
// against, executes with, stamped on the run document and published on the
// queue message — so a resume replays what the launch resolved instead of
// re-deriving the pins from a policy record that may have changed since.
//
// Specificity is NOT precedence. Within one layer, ForNode resolves per
// field by selector specificity; across layers, the fold implements the
// launch's win at the two shapes the flat list can express: a launch row
// wins a policy cell on the SAME selector (the launch's rows sit after the
// policy's, and the later rule wins a tie), and a launch row pinning a
// field on "*" shadows every policy cell on that field outright. The
// residual: a policy cell on a MORE SPECIFIC selector than any launch row
// pinning the same field still wins that field on specificity inside the
// folded list — carrying the layer across the wire would take ranks on the
// wire twins (the queue schema), so until then a launcher states a
// field-wide gesture with "*" or repeats the exact selector.
//
// A local (CLI) launch resolves no policy at all — its other levels are
// server records (ADR-129 §8, the stated limit) — so its overrides stay
// flags-only and the helper is a no-op there (nil policy).

// EffectiveModelOverrides folds the resolved routing policy's override
// cells under the launch's own rows and returns the ONE list a launch
// carries onward: validated against, executed with, stamped on the run
// document, published on the queue message. A resume replays it.
func EffectiveModelOverrides(
	launchRows []ModelOverrideEntry,
	pol *store.RunLLMRoutePolicy,
) []ModelOverrideEntry {
	policy := policyOverrideEntries(pol)
	// A launch row pinning a field on "*" shadows every policy cell on
	// that field: the launcher's canonical gesture (a dropdown / bare flag
	// records "*") must beat a more specific policy selector on score
	// alone, and "*" matches every node any policy selector can, so the
	// shadowed cell can never decide — dropping it here is what makes the
	// flat folded list resolve, on the runner and on resume, exactly as
	// the launch resolved it.
	var shadow struct{ backend, model, provider, effort bool }
	for _, r := range launchRows {
		if r.Selector != "*" {
			continue
		}
		shadow.backend = shadow.backend || r.Backend != ""
		shadow.model = shadow.model || r.Model != ""
		shadow.provider = shadow.provider || r.Provider != ""
		shadow.effort = shadow.effort || r.Effort != ""
	}
	if shadow.backend || shadow.model || shadow.provider || shadow.effort {
		kept := make([]ModelOverrideEntry, 0, len(policy)+len(launchRows))
		for _, o := range policy {
			if shadow.backend {
				o.Backend = ""
			}
			if shadow.model {
				o.Model = ""
			}
			if shadow.provider {
				o.Provider = ""
			}
			if shadow.effort {
				o.Effort = ""
			}
			if o.Backend == "" && o.Model == "" && o.Provider == "" && o.Effort == "" {
				continue // the cell is fully shadowed
			}
			kept = append(kept, o)
		}
		return append(kept, launchRows...)
	}
	return append(policy, launchRows...)
}

// policyOverrideEntries adapts the resolved snapshot's cells onto the
// launch-entry shape — the "a tiny adapter per site" doctrine
// (pkg/backend/model/override_fold.go's OverrideEntry).
func policyOverrideEntries(pol *store.RunLLMRoutePolicy) []ModelOverrideEntry {
	if pol == nil {
		return nil
	}
	out := make([]ModelOverrideEntry, 0, len(pol.Overrides))
	for _, o := range pol.Overrides {
		out = append(out, ModelOverrideEntry{
			Selector: o.Selector,
			Backend:  o.Backend,
			Model:    o.Model,
			Provider: o.Provider,
			Effort:   o.Effort,
		})
	}
	return out
}
