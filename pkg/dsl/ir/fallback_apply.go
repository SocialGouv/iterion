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
// sandboxed reports whether this run resolves to an ACTIVE sandbox.
// The caller computes it with runtime.WorkflowSandboxActive — the same
// pickMode precedence (CLI-strength override → workflow block → global
// default) the engine itself applies — because the IR cannot know the
// deployment's tiers, and a hand-rolled resolution here already lied
// once: it honoured a node-level `sandbox:` tier the engine does not
// have (one sandbox per run), advertising an escape hatch that
// re-created the exact dispatch failure it claimed to prevent.
func ApplyRunFallback(w *Workflow, routes []Fallback, sandboxed bool) []string {
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
			// The codex CLI cannot run inside the sandbox (the dispatch
			// guard in the delegate hard-errors on any non-noop driver) —
			// so a codex stage on a sandboxed run would fail EXACTLY when
			// the chain is needed, which is worse than not having it.
			// Refused here, at launch, where the operator is told.
			if route.Backend == "codex" && sandboxed {
				refuse("targets the codex CLI, which cannot run inside the sandbox this run resolves to — set sandbox: none (workflow block or ITERION_SANDBOX_OVERRIDE), or route to claude_code/claw")
				continue
			}
			// A route that changes backend with no model of its own cannot
			// work — model specs are not portable — and a route naming the
			// node's own backend with no model would re-issue the identical
			// call. Both are dropped rather than left to fail at dispatch.
			if route.Backend != "" && route.Model == "" {
				refuse(fmt.Sprintf(
					"names backend %q with no model — model specs are not portable across backends",
					route.Backend))
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
