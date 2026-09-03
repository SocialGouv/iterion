package ir

import (
	"fmt"
	"strings"
)

// RunFallbackName is the label the operator's launch-time route is
// reported by, wherever a fall-through is named. Distinct and
// recognisable on purpose: a report saying "fell back to run-fallback"
// tells the reader the route came from the launch, not from the .bot.
const RunFallbackName = "run-fallback"

// ApplyRunFallback materialises the operator's launch-time fallback
// chain (studio Launch row / CLI `--fallback`) onto the compiled
// workflow, and returns one message per node and stage where it was
// REFUSED.
//
// It writes into the IR rather than being resolved privately inside the
// executor, and that is the whole point. Three analyses read a node's
// routes BEFORE the run — the sandbox's iterion bind-mount
// (containsClawNode), parallel-branch admission
// (unrestrictedCLIBackendCanWrite) and the fan_out_each mutation guard —
// and a launch-time route resolved anywhere downstream is invisible to
// all three. An operator would then be able to reach, through a flag,
// exactly the crossings the compiler refuses in the .bot: an ungated
// node, a tools-less claw node silently gaining a full CLI toolset
// while already admitted as a read-only parallel branch, or a claw
// route with no in-container binary mounted.
//
// Eligibility mirrors the documented contract:
//   - agent nodes only. A judge's verdict is load-bearing — a weaker
//     model still emits a well-formed verdict and only the finding
//     count changes, which a deterministic merge gate reads — so a
//     judge takes a route from its own block or not at all;
//   - only a node that declares NO routes of its own. An author who
//     wrote a chain vetted where it may go;
//   - every stage independently passes the same safety predicates as C176 —
//     plus C135's, since a claw route that cannot resolve the node's
//     declared tools would fail exactly when it is needed. A refused
//     stage is skipped and the caller warns; later stages remain eligible.
//
// sandboxActive is whether THIS RUN resolves to an active sandbox, as
// the runtime itself decides it (runtime.WorkflowSandboxActive over the
// CLI-strength override + the global default). A bool, not a mode, and
// not a precedence chain re-derived here: sandbox activation is
// run-scoped — the engine starts ONE sandbox and stamps its handle on
// every node — so any second resolution living in this package could
// only diverge from the engine's. A codex stage is only takeable on a
// run that will execute UNSANDBOXED.
func ApplyRunFallback(w *Workflow, routes []Fallback, sandboxActive bool) []string {
	if w == nil || len(routes) == 0 {
		return nil
	}

	var refusals []string
	for _, n := range w.Nodes {
		nn, ok := n.(LLMNode)
		if !ok || nn.NodeKind() != NodeAgent {
			continue
		}
		if len(nn.GetFallbacks()) > 0 {
			continue
		}
		agent, ok := n.(*AgentNode)
		if !ok {
			continue
		}
		nodeBackend := effectiveNodeBackend(nn.GetLLMFields().Backend, w.DefaultBackend)
		perm := EffectivePermission(nn.GetPermission(), w.Permission)
		for stage, route := range routes {
			if route.Backend == "" && route.Model == "" && route.Provider == "" {
				continue
			}
			route.Name = RunFallbackName
			route.RunStage = stage
			route.RunStageSet = true
			refuse := func(reason string) {
				refusals = append(refusals, fmt.Sprintf(
					"agent %q: run-level fallback stage %d %s", nn.NodeID(), stage+1, reason))
			}
			if reason := ungatedCrossingReason(route.Backend, perm, len(w.PermissionAsk) > 0); reason != "" {
				refuse(reason)
				continue
			}
			if reason := toolsInversionReason(nodeBackend, route.Backend, nn.GetTools()); reason != "" {
				refuse(reason)
				continue
			}
			if reason := sessionContinuityCrossingReason(nn.GetSession(), nodeBackend, route.Backend); reason != "" {
				refuse(reason)
				continue
			}
			// Refused only on what C135 would BLOCK — a name the compiler can
			// positively identify as wrong, with no MCP wiring in sight. A bare
			// name it merely does not recognise may still resolve onto an MCP
			// tool whose catalog is merged after compilation, and dropping an
			// operator's explicit route on that guess is worse than taking it.
			// Same tiering as the diagnostic — see toolDiagReporter.
			if reason := unresolvableToolsReason(route.Backend, nn.GetTools(), mcpWiringVisible(w, n)); reason != "" {
				refuse(reason)
				continue
			}
			// A route that changes backend with no model of its own cannot
			// work — model specs are not portable — and a route naming the
			// node's own backend with no model would re-issue the identical
			// call. Both are dropped rather than left to fail at dispatch.
			//
			// Screened BEFORE the sandbox check below on purpose: a
			// malformed route can never work anywhere, so `--fallback codex`
			// (bare) is more actionably reported as "no model" than as a
			// sandbox refusal the operator would chase first.
			if route.Backend != "" && route.Model == "" {
				refuse(fmt.Sprintf(
					"names backend %q with no model — model specs are not portable across backends",
					route.Backend))
				continue
			}
			// The codex CLI cannot run inside the sandbox (the dispatch
			// guard in the delegate hard-errors on any non-noop driver) —
			// so a codex stage on a run that will be sandboxed would fail
			// EXACTLY when the chain is needed, which is worse than not
			// having it. Refused here, at launch, where the operator is
			// told.
			//
			// Matched on the EFFECTIVE backend, never the literal field:
			// dispatch expands ${...} (executor_resolve.go) and reads an
			// empty Backend as "inherit the node's", so a literal match
			// would wave through both `${FALLBACK_BACKEND:-codex}` and a
			// bare `{model: …}` stage on a codex node — the exact chain
			// this screen exists to refuse.
			if backend, origin := effectiveRouteBackend(route, nn, w); backend == "codex" && sandboxActive {
				refuse(fmt.Sprintf(
					"resolves to the codex CLI%s, which cannot run inside the sandbox this run executes in — "+
						"declare sandbox: none on the workflow, pass --sandbox none, or route to claude_code/claw",
					origin))
				continue
			}
			agent.Fallbacks = append(agent.Fallbacks, route)
		}
	}
	return refusals
}

// ParseRunFallbackFlag parses the `--fallback` / launch-row value into a
// route.
//
// The form is `<backend>:<model>` — split on the FIRST colon, so a model
// id that itself contains one survives. A bare value with no colon is
// read as a backend, which ApplyRunFallback then refuses for the reason
// above; failing at parse would be indistinguishable from a typo.
//
// Deliberately no trigger syntax: the flag takes the default `on:` set,
// and an operator who needs a different one is authoring a chain, which
// belongs in the .bot.
func ParseRunFallbackFlag(arg string) (Fallback, error) {
	arg = strings.TrimSpace(arg)
	if arg == "" {
		return Fallback{}, nil
	}
	backend, model, _ := strings.Cut(arg, ":")
	backend = strings.TrimSpace(backend)
	model = strings.TrimSpace(model)
	if backend == "" {
		return Fallback{}, fmt.Errorf("--fallback %q: missing backend (expected <backend>:<model>)", arg)
	}
	return Fallback{Name: RunFallbackName, Backend: backend, Model: model}, nil
}

// effectiveRouteBackend resolves the backend a stage will actually
// DISPATCH on, mirroring the executor's own resolution over the data
// this package owns: expand `${VAR}` / `${VAR:-default}` exactly as
// resolveChain does, and read an empty Backend as "inherit the node's
// resolved backend" (chainElement.Backend == "" — the node's own field,
// then the workflow default). It also returns a short parenthetical
// naming WHERE the resolution came from, empty for a literal, so a
// refusal about a `${...}` or inherited stage does not read as nonsense.
//
// The boundary, stated rather than implied: `ir` cannot see
// ExecutorSpec.ModelOverrides (a --backend-for retarget) or the
// credential auto-detection behind ITERION_BACKEND_PREFERENCE, so a node
// reaching codex through either is invisible here. Both are blind spots
// where the node's PRIMARY is codex too — a larger, separately unscreened
// problem whose home is ClawExecutor.EffectiveBackendName, which already
// exposes the full resolution to safety checks. Widening this screen to
// them means moving it there, not re-deriving the chain in a leaf package.
//
// The "codex" literal is deliberate: importing delegate.BackendCodex
// would put a backend dependency on a leaf package that has none.
func effectiveRouteBackend(route Fallback, node LLMNode, w *Workflow) (backend, origin string) {
	declared := strings.TrimSpace(route.Backend)
	// Emptiness is judged AFTER expansion, because dispatch judges it
	// there too: an unset `${VAR}` carrying no `:-` default expands to
	// "" (ExpandEnvWithDefault), and resolveChain assigns that "" to
	// chainElement.Backend, which means "inherit the node's resolved
	// backend". So `${UNSET}` and an absent field are the SAME route at
	// dispatch, and screening only the absent one would leave the pair
	// self-inconsistent — a codex node refusing `{model: …}` while
	// waving through `{backend: "${UNSET}", model: …}`.
	if expanded := strings.TrimSpace(ExpandEnvWithDefault(declared)); expanded != "" {
		if expanded != declared {
			return expanded, fmt.Sprintf(" (via %s)", declared)
		}
		return expanded, ""
	}
	// Inherited: the node's own backend, then the workflow default —
	// effectiveNodeBackend's exact chain, fed the EXPANDED inputs. Reusing
	// it rather than re-walking the tiers is what keeps this screen and
	// the three above from drifting apart; the only difference is the
	// expansion, which the others deliberately do not do (they refuse to
	// judge a `${...}` backend at all, where this one must resolve it or
	// miss the shape that motivated the screen).
	nodeBackend := strings.TrimSpace(ExpandEnvWithDefault(node.GetLLMFields().Backend))
	// Name the tier the backend actually came from: a node declaring none
	// takes the workflow's `default_backend:`, and a refusal blaming "the
	// node's backend" would send the operator to a line that is not there.
	from := "the node's backend"
	if effectiveNodeBackend(nodeBackend, "") == "" {
		from = "the workflow's default_backend"
	}
	origin = fmt.Sprintf(" (inherited from %s)", from)
	if declared != "" {
		// The operator DID name a backend — it just resolved to nothing.
		// Saying only "inherited" would hide the unset variable that is
		// the actual thing to fix.
		origin = fmt.Sprintf(" (%s is unset, inherited from %s)", declared, from)
	}
	return effectiveNodeBackend(
		nodeBackend,
		strings.TrimSpace(ExpandEnvWithDefault(w.DefaultBackend)),
	), origin
}
