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
//
// vars carries the launch's `--var` overrides, so a node whose
// `backend:` is a `{{vars.<name>}}` reference is screened by what THIS
// run resolves — the same reading the dispatch makes (resolveRoutingField:
// template first, `${…}` after) — rather than passing unscreened as a
// field nothing decided. The declared defaults under the overrides come
// from the workflow itself; a reference neither answers stays undecided,
// as the compiler reads it.
func ApplyRunFallback(w *Workflow, routes []Fallback, sandboxed bool, vars map[string]string) []string {
	if w == nil || len(routes) == 0 {
		return nil
	}
	run := runBackend.withVars(launchVarsView(w, vars))

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
		// The RUN's reading on both sides: this screen runs in the process
		// that will dispatch the node, so a dial set in that process's
		// environment IS the route, where the compiler may only read what
		// the source declares — and the launch's vars decide a `{{vars.x}}`
		// the source deliberately left open.
		nodeBackend := run.effective(nn.GetLLMFields().Backend, w.DefaultBackend)
		perm := EffectivePermission(nn.GetPermission(), w.Permission)
		for stage, route := range routes {
			if route.Backend == "" && route.Model == "" && route.Provider == "" {
				continue
			}
			// The route's backend as the run reads it, so an operator's
			// `--fallback '${DIAL:-claw} …'` is screened by what it
			// resolves to rather than by its spelling.
			routeBackend := run.routeName(route.Backend)
			route.Name = RunFallbackName
			route.RunStage = stage
			route.RunStageSet = true
			refuse := func(reason string) {
				refusals = append(refusals, fmt.Sprintf(
					"agent %q: run-level fallback stage %d %s", nn.NodeID(), stage+1, reason))
			}
			if reason := UngatedCrossingReasonForAskRules(routeBackend, perm, EffectiveAskRules(nn, w)); reason != "" {
				refuse(reason)
				continue
			}
			if reason := toolsInversionReason(nodeBackend, routeBackend, nn.GetTools()); reason != "" {
				refuse(reason)
				continue
			}
			if reason := sessionContinuityCrossingReason(nn.GetSession(), nodeBackend, routeBackend); reason != "" {
				refuse(reason)
				continue
			}
			// Refused only on what C135 would BLOCK — a name the compiler can
			// positively identify as wrong, with no MCP wiring in sight. A bare
			// name it merely does not recognise may still resolve onto an MCP
			// tool whose catalog is merged after compilation, and dropping an
			// operator's explicit route on that guess is worse than taking it.
			// Same tiering as the diagnostic — see toolDiagReporter.
			if reason := unresolvableToolsReason(routeBackend, nn.GetTools(), mcpWiringVisible(w, n)); reason != "" {
				refuse(reason)
				continue
			}
			// The codex CLI cannot run inside the sandbox (the dispatch
			// guard in the delegate hard-errors on any non-noop driver) —
			// so a codex stage on a sandboxed run would fail EXACTLY when
			// the chain is needed, which is worse than not having it.
			// Refused here, at launch, where the operator is told.
			if routeBackend == "codex" && sandboxed {
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

// launchVarsView is the vars reading the launch-time screen resolves
// `{{vars.<name>}}` routing fields against: the declared defaults under
// the launch's overrides — the two layers resolveVars stacks, in its
// order, and with its rule that a launch value for a var the workflow
// does not declare is dropped. Values stay RAW text: the reader's own
// `${…}` expansion runs after the substitution (the runtime's
// template-then-env order), so a var holding `${X:-claw}` is screened
// by what the run resolves, not by its spelling. Nil when nothing
// declares or overrides a var, which reads exactly as before.
func launchVarsView(w *Workflow, overrides map[string]string) map[string]string {
	if len(w.Vars) == 0 {
		return nil
	}
	var view map[string]string
	put := func(name, value string) {
		if view == nil {
			view = make(map[string]string, len(w.Vars))
		}
		view[name] = value
	}
	for name, v := range w.Vars {
		if v == nil || !v.HasDefault {
			continue
		}
		if s, ok := varDefaultText(v.Default); ok {
			put(name, s)
		}
	}
	for name, s := range overrides {
		if _, declared := w.Vars[name]; declared {
			put(name, s)
		}
	}
	return view
}

// varDefaultText renders a declared default as the text a routing field
// substitutes — a string as written, a scalar the way the template
// resolver's JSON-ish formatting prints it.
func varDefaultText(v any) (string, bool) {
	switch t := v.(type) {
	case string:
		return t, true
	case int64, float64, bool:
		return fmt.Sprintf("%v", t), true
	}
	return "", false
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
