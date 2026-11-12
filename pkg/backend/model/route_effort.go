package model

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/SocialGouv/iterion/pkg/backend/compatgw"
	"github.com/SocialGouv/iterion/pkg/backend/modelroute"
)

// routeReasoningEffort honors the route's accepted efforts
// (compatgw.AcceptedEfforts: the run's feed table, then the runner's env
// table). A route with no accepted list keeps the requested effort; a
// substitution is warned and reported with the source that listed the
// route's efforts. Equal distances prefer more effort.
func (e *ClawExecutor) routeReasoningEffort(ctx context.Context, nodeID, spec, requested string) string {
	applied, source := gatewayReasoningEffort(ctx, spec, requested, e.logger.Warn)
	if applied == requested {
		return requested
	}
	info := ReasoningEffortAdjustment{
		Model:     spec,
		Requested: requested,
		Applied:   applied,
		Source:    source,
		Reason:    fmt.Sprintf("requested effort is not among %s's accepted efforts (%s)", modelroute.Parse(spec).Wire, source),
	}
	e.logger.Warn("reasoning_effort_adjusted: node %q model %q requested=%s applied=%s reason=%s", nodeID, spec, requested, applied, info.Reason)
	if e.hooks.OnReasoningEffortAdjusted != nil {
		e.hooks.OnReasoningEffortAdjusted(nodeID, info)
	}
	return applied
}

// gatewayReasoningEffort maps a requested effort onto the gateway route's
// accepted list, read through the invocation's environment
// (compatgw.AcceptedEfforts), and answers the list's source ("" when no
// list answered and the requested effort passes). Gateway wire IDs are
// opaque. A named effort uses the nearest named level (ties prefer more
// effort); integers compare only with integers. If an operator lists only
// the other representation, its first value wins. No undocumented
// conversion between a named level and a numeric budget is made.
func gatewayReasoningEffort(ctx context.Context, spec, requested string, warn func(string, ...any)) (applied, source string) {
	if requested == "" {
		return requested, ""
	}
	route := modelroute.Parse(spec)
	if !route.Gateway() {
		return requested, ""
	}
	supported, source := compatgw.AcceptedEfforts(route.Wire, func(name string) string { return envGetenv(ctx, name) }, warn)
	if len(supported) == 0 {
		return requested, ""
	}
	normalized := strings.ToLower(strings.TrimSpace(wireEffort(requested)))
	if slices.Contains(supported, normalized) {
		return normalized, source
	}
	target, numeric := effortPosition(normalized)
	best := ""
	bestDistance := int(^uint(0) >> 1)
	bestRank := 0
	for _, candidate := range supported {
		rank, isNumeric := effortPosition(candidate)
		if isNumeric != numeric {
			continue
		}
		d := rank - target
		if d < 0 {
			d = -d
		}
		if d < bestDistance || d == bestDistance && rank > bestRank {
			best = candidate
			bestDistance = d
			bestRank = rank
		}
	}
	if best != "" {
		return best, source
	}
	return supported[0], source
}
func effortPosition(value string) (int, bool) {
	if n, err := strconv.Atoi(value); err == nil {
		return n, true
	}
	return effortRank(value), false
}
