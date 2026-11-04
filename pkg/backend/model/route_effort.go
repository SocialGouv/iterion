package model

import (
	"github.com/SocialGouv/iterion/pkg/backend/compatgw"
	"github.com/SocialGouv/iterion/pkg/backend/modelroute"
	"github.com/SocialGouv/iterion/pkg/backend/modelspecs"
	"os"
	"slices"
)

// routeReasoningEffort honors an explicit route matrix. An absent matrix
// retains the backend's existing behavior. Equal distances prefer more effort.
func (e *ClawExecutor) routeReasoningEffort(nodeID, spec, requested string) string {
	if requested == "" {
		return requested
	}
	route := modelroute.Parse(spec)
	var supported []string
	if route.Gateway() {
		supported = compatgw.ResolveCatalogQuiet(route.Wire, modelspecs.Default(), os.Getenv).Spec.ReasoningEfforts
	} else if registry := modelspecs.Default(); registry != nil {
		if record, ok := registry.LookupExact(route.Provider, route.Wire); ok {
			supported = record.ReasoningEfforts
		}
	}
	if len(supported) == 0 || slices.Contains(supported, requested) {
		return requested
	}
	applied := supported[0]
	distance := func(value string) int {
		d := effortRank(value) - effortRank(wireEffort(requested))
		if d < 0 {
			return -d
		}
		return d
	}
	for _, candidate := range supported[1:] {
		if distance(candidate) < distance(applied) || distance(candidate) == distance(applied) && effortRank(candidate) > effortRank(applied) {
			applied = candidate
		}
	}
	info := ReasoningEffortAdjustment{Model: spec, Requested: requested, Applied: applied, Reason: "requested effort is absent from the route reasoning_efforts"}
	e.logger.Warn("reasoning_effort_adjusted: node %q model %q requested=%s applied=%s reason=%s", nodeID, spec, requested, applied, info.Reason)
	if e.hooks.OnReasoningEffortAdjusted != nil {
		e.hooks.OnReasoningEffortAdjusted(nodeID, info)
	}
	return applied
}
