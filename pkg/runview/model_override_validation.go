package runview

import (
	"fmt"
	"os"
	"slices"
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
// It is the dispatching process's verdict: BuildExecutor calls it on every
// plane — CLI launch/resume, local Studio, the cloud runner — and a node
// backend a slot dial decides is read on this process's environment. The
// launch, which may run on another process than the one that dispatches,
// reads the same screen through screenModelOverridePermissions. The
// backend predicates are shared with C176, C135 and --fallback.
func ValidateModelOverridePermissions(wf *ir.Workflow, overrides model.ModelOverrides, runPermission string) error {
	if err := refuseUngatedNodeOverride(wf, overrides, runPermission); err != nil {
		return err
	}
	if found := routeCellCrossings(wf, overrides, runPermission, true); len(found) > 0 {
		return fmt.Errorf("runview: %s", found[0].sentence)
	}
	return nil
}

// screenModelOverridePermissions is the launch's reading of the same
// screen (foldAndScreen): a node override that ungates its node, and a
// route crossing whose verdict reads no node backend a dial decides,
// refuse; a crossing read on the default of a node backend dial — the
// runner expands it, on its own environment — is returned as a warning,
// and BuildExecutor refuses it on the dispatching process's reading.
func screenModelOverridePermissions(wf *ir.Workflow, overrides model.ModelOverrides, runPermission string) ([]string, error) {
	if err := refuseUngatedNodeOverride(wf, overrides, runPermission); err != nil {
		return nil, err
	}
	var warnings []string
	for _, c := range routeCellCrossings(wf, overrides, runPermission, false) {
		if c.undecided {
			warnings = append(warnings, "the launch's override cells re-pin a route onto a crossing the compiler refuses unless the runner's environment decides a dial otherwise: "+c.sentence)
			continue
		}
		return nil, fmt.Errorf("runview: %s", c.sentence)
	}
	return warnings, nil
}

// refuseUngatedNodeOverride refuses the first node override whose backend
// cannot gate the node's effective ask-rules.
func refuseUngatedNodeOverride(wf *ir.Workflow, overrides model.ModelOverrides, runPermission string) error {
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
	return nil
}

// routeCrossing is one refused route crossing. undecided marks a verdict
// read on the default of the node backend dial the dispatching process
// expands.
type routeCrossing struct {
	sentence  string
	undecided bool
}

// nodeBackendDial is the dial text a node's backend reads, "" when none:
// its own `backend:`, else the `default_backend:` it falls through to.
func nodeBackendDial(nodeBackend, workflowDefault string) string {
	if undecidedText(nodeBackend) {
		return nodeBackend
	}
	if ir.BackendFallsThrough(nodeBackend) && undecidedText(workflowDefault) {
		return workflowDefault
	}
	return ""
}

// routeCellCrossings is C176's verdict (ir.RouteCrossingReasons) and C135's
// (ir.RouteToolsUnresolvableReason) on every authored `fallbacks:` route
// whose backend a route-scoped override pins — a routing cell's
// `<bot>/<node>#<route>`, a launch row's `<node>#<route>` — judged as the
// compiler judges the same route written in the .bot: the route's
// backend against the gate in force, and against the node's backend as
// dispatch reads it, the node override first (a cell moving node and route
// together crosses nothing).
//
// Without a node override, atDispatch reads the node's field on this
// process's environment (ir.RunNodeBackendName) — the dispatching
// process's word. A launch (atDispatch false) reads the source
// (ir.SourceNodeBackendName, a dial's default): an arm that reads the
// node's backend through a dial is undecided there, while the gate and
// C135, which read only the route's backend, stay decided. One crossing
// per refused reason, nodes in id order.
func routeCellCrossings(wf *ir.Workflow, overrides model.ModelOverrides, runPermission string, atDispatch bool) []routeCrossing {
	var out []routeCrossing
	for _, id := range overrideSiteNodeIDs(wf) {
		llm, ok := wf.Nodes[id].(ir.LLMNode)
		if !ok {
			continue
		}
		f := llm.GetLLMFields()
		nodeBackend := overrides.ForNode(id, llm.NodeKind()).Backend
		dial := ""
		if nodeBackend == "" {
			if atDispatch {
				nodeBackend = ir.RunNodeBackendName(f.Backend, wf.DefaultBackend)
			} else {
				nodeBackend = ir.SourceNodeBackendName(f.Backend, wf.DefaultBackend)
				dial = nodeBackendDial(f.Backend, wf.DefaultBackend)
			}
		}
		permission := firstPermission(runPermission, llm.GetPermission(), wf.Permission, os.Getenv("ITERION_PERMISSION"))
		for _, r := range authoredRoutes(llm, overrides) {
			if r.pin.Backend == "" {
				continue
			}
			routeBackend := r.effective.Backend
			sentence := func(reason string) string {
				return fmt.Sprintf("model override for %s %q, fallbacks: route %q %s", llm.NodeKind(), id, r.site, reason)
			}
			// The arms that read no node backend: decided whatever a dial
			// expands to.
			decided := ir.RouteCrossingReasons(llm, wf, "", routeBackend, permission)
			if reason := ir.RouteToolsUnresolvableReason(wf, llm, routeBackend); reason != "" {
				decided = append(decided, reason)
			}
			for _, reason := range decided {
				out = append(out, routeCrossing{sentence: sentence(reason)})
			}
			// The arms that read the node's backend.
			for _, reason := range ir.RouteCrossingReasons(llm, wf, nodeBackend, routeBackend, permission) {
				if slices.Contains(decided, reason) {
					continue
				}
				c := routeCrossing{sentence: sentence(reason) + fmt.Sprintf(" — or move the node with the route: a cell on node %q pinning backend %q", llm.NodeID(), routeBackend)}
				if dial != "" {
					c.sentence += fmt.Sprintf(" (read on the default of the node's backend dial %q, which the runner expands)", dial)
					c.undecided = true
				}
				out = append(out, c)
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
	for _, crossing := range routeCellCrossings(wf, overrides, "", false) {
		warnings = append(warnings, "the routing block re-pins a route onto a crossing the compiler refuses: "+crossing.sentence)
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
