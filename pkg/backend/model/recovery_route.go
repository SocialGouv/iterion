package model

import (
	"strings"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// recoveryRungsOf answers for a tool node's Verified Action recovery ladder
// (ADR-044) which rungs dispatch can spend, and on what spec. spec is the
// authored `recovery.model`, RAW: the caller's view resolves it like every
// other routing field (the run's vars by provenance, a late name kept as
// written).
//
// The two rungs spend the SAME spec differently, and the callers read each
// the way dispatch runs it:
//
//   - rung 3 (selfRepair) is a DIRECT generation on the spec
//     (registry.ResolveWithContext): no backend, no default_backend, no
//     override cell — the class humanLLMModelOf answers for a human node;
//   - rung 4 (agentRung) builds a SYNTHETIC agent node — id
//     `<tool>__recover`, the resolved spec its whole model — and runs the
//     full agent path on it, so a launch override whose selector reaches
//     that id (a kind word, a glob) applies exactly as at dispatch.
//
// The ONE reader both launch-time walks consult for the kind (#2783): read
// by neither, the provider a rung spends was invisible to the providers
// allowlist and to both unfunded refusals. Each rung's reachability is the
// predicate DISPATCH gates it on — ir.ToolRecoverySelfRepairRung,
// ir.ToolRecoveryAgentRung, which ir.nodeUsesLLM reads too. Restated here
// instead, the walks priced a rung dispatch never runs: a registry recipe
// (a bare `command:`) has no command for rung 3 to correct, so a launch was
// refused by name for a provider nothing could spend.
func recoveryRungsOf(n ir.Node) (spec string, selfRepair, agentRung bool) {
	t, ok := n.(*ir.ToolNode)
	if !ok {
		return "", false, false
	}
	selfRepair = ir.ToolRecoverySelfRepairRung(t)
	agentRung = ir.ToolRecoveryAgentRung(t)
	if !selfRepair && !agentRung {
		return "", false, false
	}
	return t.Recovery.Model, selfRepair, agentRung
}

// recoveryModelFallback is the recovery ladder's default arm, shared by
// dispatch (recoveryModel) and the launch walks so the two cannot drift over
// one unpinned spec: ITERION_VERIFIED_ACTION_MODEL, else the package
// default.
//
// The helper is shared; the ENV it reads is each process's own. A launch
// walk runs in the API server and dispatch in the runner pod, so the
// provider this arm names is dispatch's only while the dial reads the same
// on both — which the chart guarantees by giving the two deployments one
// envFrom ConfigMap, and which docs/environment-variables.md asks of an
// operator who sets it by hand.
func recoveryModelFallback() string {
	if m := ir.LookupEnv("ITERION_VERIFIED_ACTION_MODEL"); m != "" {
		return m
	}
	return defaultVerifiedActionModel
}

// recoverySpecOf applies the walk's half of recoveryModel's ladder: the
// authored spec through the view's vars reading, the env dial and the
// package default when the field names nothing. A spec that DEFERS (a
// `${VAR}` this process cannot fill, a late `{{vars.…}}`) stays written —
// non-empty — so the arms below widen on it exactly as dispatch would
// resolve it on the runner, rather than falling through to a default the
// runner may never reach.
func recoverySpecOf(v RouteView, spec string) string {
	spec = v.field(spec)
	if strings.TrimSpace(spec) == "" {
		return recoveryModelFallback()
	}
	return spec
}

// recoveryAgentNode is the rung-4 synthetic agent as dispatch builds it
// (agentRecovery): same id, same single-field route, same tool set. The
// walks resolve overrides and the backend against it, so a launch-time
// routing cell sees the rung exactly where dispatch does.
func recoveryAgentNode(toolID, spec string, agentTools []string) *ir.AgentNode {
	return &ir.AgentNode{
		BaseNode:  ir.BaseNode{ID: toolID + "__recover"},
		LLMFields: ir.LLMFields{Model: spec},
		Tools:     agentTools,
	}
}
