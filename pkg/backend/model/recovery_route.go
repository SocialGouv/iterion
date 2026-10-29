package model

import (
	"strings"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// recoveryRungsOf answers for a tool node's Verified Action recovery ladder
// (ADR-044) the way dispatch spends it (executor_verified_action.go): the
// rungs exist only under `policy: recover` with a `recovery:` spec, and an
// action recipe never carries one (C262), so a surviving spec belongs to a
// command/script — repairable — recipe. spec is the authored
// `recovery.model`, RAW: the caller's view resolves it like every other
// routing field (the run's vars by provenance, a late name kept as written).
//
// The two rungs spend the SAME spec differently, and the callers read each
// the way dispatch runs it:
//
//   - rung 3 (selfRepair, MaxRepairAttempts > 0) is a DIRECT generation on
//     the spec (registry.ResolveWithContext): no backend, no
//     default_backend, no override cell — the class humanLLMModelOf answers
//     for a human node;
//   - rung 4 (agentRecovery, MaxAgentAttempts > 0) builds a SYNTHETIC agent
//     node — id `<tool>__recover`, the resolved spec its whole model — and
//     runs the full agent path on it, so a launch override whose selector
//     reaches that id (a kind word, a glob) applies exactly as at dispatch.
//
// The ONE reader both launch-time walks consult for the kind (#2783): read
// by neither, the provider a rung spends was invisible to the providers
// allowlist and to both unfunded refusals. ir.nodeUsesLLM's ToolNode arm
// shares this gate — the walks' fieldless class and the graph property must
// agree on one policy line, or an inert `recovery:` block would widen the
// walks for a spend dispatch never makes.
func recoveryRungsOf(n ir.Node) (spec string, selfRepair, agentRung bool) {
	t, ok := n.(*ir.ToolNode)
	if !ok || t.Policy != ir.PolicyRecover || t.Recovery == nil {
		return "", false, false
	}
	return t.Recovery.Model,
		t.Recovery.MaxRepairAttempts > 0,
		t.Recovery.MaxAgentAttempts > 0
}

// recoveryModelFallback is the recovery ladder's default arm, shared by
// dispatch (recoveryModel) and the launch walks so the two cannot drift over
// one unpinned spec: ITERION_VERIFIED_ACTION_MODEL, else the package
// default.
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
