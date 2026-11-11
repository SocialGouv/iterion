package runview

import (
	"fmt"
	"os"
	"strings"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/llmroute"
	"github.com/SocialGouv/iterion/pkg/store"
)

// ValidateModelOverridePermissions rejects a launch-time backend override
// that would silently remove a node's effective tool-permission gate, and
// a route-scoped override whose backend re-pins an authored `fallbacks:`
// route onto a crossing the compiler refuses for the same route written in
// the .bot (routeCellCrossings).
//
// BuildExecutor is the common choke point for CLI launch/resume, local Studio
// and the cloud runner. The cloud publisher also calls this before queueing so
// the operator gets a synchronous admission error rather than a remotely
// failed run. The backend predicates are shared with C176, C135 and
// --fallback.
func ValidateModelOverridePermissions(wf *ir.Workflow, overrides model.ModelOverrides, runPermission string) error {
	if wf == nil || overrides.Empty() {
		return nil
	}
	for _, node := range wf.Nodes {
		llm, ok := node.(ir.LLMNode)
		if !ok {
			continue
		}
		backend := overrides.ForNode(llm.NodeID(), llm.NodeKind()).Backend
		if backend == "" {
			continue
		}
		permission := firstPermission(runPermission, llm.GetPermission(), wf.Permission, os.Getenv("ITERION_PERMISSION"))
		if reason := ir.UngatedCrossingReasonForAskRules(backend, permission, ir.EffectiveAskRules(llm, wf)); reason != "" {
			return fmt.Errorf("runview: model override for %s %q %s", llm.NodeKind(), llm.NodeID(), reason)
		}
	}
	if found := routeCellCrossings(wf, overrides, runPermission); len(found) > 0 {
		return fmt.Errorf("runview: %s", found[0])
	}
	return nil
}

// routeCellCrossings is C176's verdict (ir.RouteCrossingReasons) and C135's
// (ir.RouteToolsUnresolvableReason) on every authored `fallbacks:` route
// whose backend a route-scoped override pins — a routing cell's
// `<bot>/<node>#<route>`, a launch row's `<node>#<route>` — judged as the
// compiler judges the same route written in the .bot: the route's
// backend against the gate in force, and against the node's backend as
// dispatch reads it, the node override first (a cell moving node and route
// together crosses nothing), else the run's reading of the node's field.
// One sentence per refused crossing, nodes in id order.
func routeCellCrossings(wf *ir.Workflow, overrides model.ModelOverrides, runPermission string) []string {
	var out []string
	for _, id := range overrideSiteNodeIDs(wf) {
		llm, ok := wf.Nodes[id].(ir.LLMNode)
		if !ok {
			continue
		}
		nodeBackend := overrides.ForNode(id, llm.NodeKind()).Backend
		if nodeBackend == "" {
			nodeBackend = ir.RunNodeBackendName(llm.GetLLMFields().Backend, wf.DefaultBackend)
		}
		permission := firstPermission(runPermission, llm.GetPermission(), wf.Permission, os.Getenv("ITERION_PERMISSION"))
		for _, r := range authoredRoutes(llm, overrides) {
			if r.pin.Backend == "" {
				continue
			}
			routeBackend := r.effective.Backend
			reasons := ir.RouteCrossingReasons(llm, wf, nodeBackend, routeBackend, permission)
			if reason := ir.RouteToolsUnresolvableReason(wf, llm, routeBackend); reason != "" {
				reasons = append(reasons, reason)
			}
			for _, reason := range reasons {
				out = append(out, fmt.Sprintf("model override for %s %q, fallbacks: route %q %s", llm.NodeKind(), id, r.site, reason))
			}
		}
	}
	return out
}

// ScreenPolicyOverrides compiles a bot the way a launch compiles it and
// reports, per LLM node, whether the routing block's overrides would
// re-target it onto a backend that cannot gate the node's effective
// ask-rules, or re-pin one of its routes onto a crossing the compiler
// refuses, and every composed pair or element the launch refuses — the
// SAME predicates the launch enforces as refusals
// (ValidateModelOverridePermissions, composedPairFindings), collected
// instead of failing on the first. It backs the write-surface WARNING (C1xx posture):
// a `*` cell reaches every bot the integration fans out to, and a block
// that would refuse a co-enabled bot's webhook delivery must say so at
// save time, not at the first delivery after it. NOT an enforcement — a
// bot enabled after the write is never screened, and the launch refusal
// above stays the gate.
func ScreenPolicyOverrides(path, source, bundleDir, bundleEntry string, bot llmroute.BotIdentity, pol *store.RunLLMRoutePolicy) ([]string, error) {
	if pol == nil || len(pol.Overrides) == 0 {
		return nil, nil
	}
	// A save-time screen that executes nothing: no run, no workspace — the
	// plugin `{{workspace}}` placeholders stay unresolved (#1800).
	wf, _, _, err := compileForLaunch(path, source, bundleDir, bundleEntry, "", AnalysisTrust)
	if err != nil {
		return nil, err
	}
	if wf == nil {
		return nil, nil
	}
	sourced := foldSourcedOverrides(nil, pol, bot)
	overrides := toModelOverrides(plainModelOverrideEntries(sourced))
	var warnings []string
	for _, node := range wf.Nodes {
		llm, ok := node.(ir.LLMNode)
		if !ok {
			continue
		}
		backend := overrides.ForNode(llm.NodeID(), llm.NodeKind()).Backend
		if backend == "" {
			continue
		}
		permission := firstPermission("", llm.GetPermission(), wf.Permission, os.Getenv("ITERION_PERMISSION"))
		if reason := ir.UngatedCrossingReasonForAskRules(backend, permission, ir.EffectiveAskRules(llm, wf)); reason != "" {
			warnings = append(warnings, fmt.Sprintf("the routing block moves %s %q onto a backend that cannot gate it: %s", llm.NodeKind(), llm.NodeID(), reason))
		}
	}
	for _, crossing := range routeCellCrossings(wf, overrides, "") {
		warnings = append(warnings, "the routing block re-pins a route onto a crossing the compiler refuses: "+crossing)
	}
	warnings = append(warnings, composedPairWarnings(wf, sourced)...)
	// The zero-match half of ADR-132 §2's guards, at the surface that can
	// still fix the typo: a cell written for this bot by name (its id or
	// its manifest name) that reaches nothing in it — every time it is
	// screened, not once per process.
	warnings = append(warnings, zeroMatchWarnings(wf, pol, bot)...)
	return warnings, nil
}

func firstPermission(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}
