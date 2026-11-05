package model

import (
	"context"
	"github.com/SocialGouv/iterion/pkg/backend/compatgw"
	"github.com/SocialGouv/iterion/pkg/backend/modelroute"
	"slices"
	"strconv"
	"strings"
)

// routeReasoningEffort honors an explicit route matrix. An absent matrix
// retains the backend's existing behavior. Equal distances prefer more effort.
func (e *ClawExecutor) routeReasoningEffort(ctx context.Context, nodeID, spec, requested string) string {
	applied := gatewayReasoningEffort(ctx, spec, requested, e.logger.Warn)
	if applied == requested {
		return requested
	}
	info := ReasoningEffortAdjustment{Model: spec, Requested: requested, Applied: applied, Reason: "requested effort is absent from OPENAI_COMPATIBLE_REASONING_EFFORTS"}
	e.logger.Warn("reasoning_effort_adjusted: node %q model %q requested=%s applied=%s reason=%s", nodeID, spec, requested, applied, info.Reason)
	if e.hooks.OnReasoningEffortAdjusted != nil {
		e.hooks.OnReasoningEffortAdjusted(nodeID, info)
	}
	return applied
}

// Gateway wire IDs are opaque. A named effort uses the nearest named level
// (ties prefer more effort); integers compare only with integers. If an
// operator lists only the other representation, its first value wins. No
// undocumented conversion between a named level and a numeric budget is made.
func gatewayReasoningEffort(ctx context.Context, spec, requested string, warn func(string, ...any)) string {
	if requested == "" {
		return requested
	}
	route := modelroute.Parse(spec)
	if !route.Gateway() {
		return requested
	}
	supported := compatgw.ReasoningEfforts(route.Wire, func(name string) string { return envGetenv(ctx, name) }, warn)
	if len(supported) == 0 {
		return requested
	}
	normalized := strings.ToLower(strings.TrimSpace(wireEffort(requested)))
	if slices.Contains(supported, normalized) {
		return normalized
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
		return best
	}
	return supported[0]
}
func effortPosition(value string) (int, bool) {
	if n, err := strconv.Atoi(value); err == nil {
		return n, true
	}
	return effortRank(value), false
}
