package ir

import (
	"fmt"
	"slices"
	"strings"

	"github.com/SocialGouv/iterion/pkg/llmroute"
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
// does not declare is dropped.
//
// Values carry the type resolveVars gives them — ResolveVarText is its
// one reading of a var's text, shared here — so a `json` var is its
// parsed document and a dotted `{{vars.cfg.backend}}` drills into it,
// the executor's own semantics. The expansion pass is resolveVars' too:
// LookupEnv, the bot-vars overlay then the process environment
// (varExpandFn ends in it, ADR-093). The reader's own `${…}` pass then
// runs on the substituted field, resolveRoutingField's second. Storing the
// raw text instead read every var-held `${…}` once too few, and a reading
// through another environment than dispatch's screens a backend the run
// never resolves — or misses one it does.
//
// One resolveVars reading the screen cannot reproduce stays UNDECIDED
// instead: a var whose expansion would CONSULT one of the five names
// varExpandFn answers from engine state — PROJECT_DIR, BUNDLE_DIR,
// BUNDLE_SKILLS_DIR, PROJECT_MEMORY_DIR, PROJECT_SCRATCH_DIR — is omitted
// (the screen has no workDir, worktree or container workspace; the
// environment reads them "", decided-empty where dispatch reads a path). The probe
// reads the text the way dispatch expands THAT TYPE — a json var's string
// leaves braced-only, keys never — and it counts a name consulted but
// discarded (`${SET:-${PROJECT_DIR}}` with SET in the environment: the
// inside-out pass consults the inner segment before the outer
// short-circuits), which is over-conservative by construction: dispatch
// decides the outer value where the screen says nothing. Undecided never
// refuses a route the run would take; it only screens less, the pre-#1606
// posture for every var. Nil when nothing declares or overrides a var,
// which reads exactly as before.
func launchVarsView(w *Workflow, overrides map[string]string) map[string]any {
	if len(w.Vars) == 0 {
		return nil
	}
	var view map[string]any
	put := func(name string, value any) {
		if view == nil {
			view = make(map[string]any, len(w.Vars))
		}
		view[name] = value
	}
	read := func(name string, raw any, vt VarType) {
		if referencesEngineSuppliedName(raw, vt) {
			return
		}
		v, err := ResolveVarText(raw, vt, LookupEnv)
		if err != nil {
			// resolveVars' own fallback: a coercion failure logs and runs
			// the RAW value, env-expanded (engine_resolve.go) — a flat
			// {{vars.b}} reads it, and a drill into it finds nothing, as
			// into any non-map. Omitting the var instead read the node as
			// undecided — or left the declared DEFAULT standing over a
			// failed override — both screening a backend the run never
			// resolves.
			if s, isText := raw.(string); isText {
				put(name, ExpandWithDefault(s, LookupEnv))
			}
			return
		}
		put(name, v)
	}
	for name, v := range w.Vars {
		if v == nil || !v.HasDefault {
			continue
		}
		read(name, v.Default, v.Type)
	}
	for name, s := range overrides {
		if decl, declared := w.Vars[name]; declared && decl != nil {
			read(name, s, decl.Type)
		}
	}
	return view
}

// EngineSuppliedVarNames are the names the engine's varExpandFn answers
// from run state (pkg/runtime/engine_resolve.go) rather than from any
// environment the launch-time screen runs with. Exported so the twin is
// pinned by a pkg/runtime test against varExpandFn itself.
var EngineSuppliedVarNames = []string{
	"PROJECT_DIR",
	"BUNDLE_DIR",
	"BUNDLE_SKILLS_DIR",
	"PROJECT_MEMORY_DIR",
	"PROJECT_SCRATCH_DIR",
}

// referencesEngineSuppliedName reports whether reading raw as a var of
// type vt would CONSULT one of the engine-supplied names — the probe
// reads the text the way dispatch expands that type: the full reading
// for a scalar, and the braced-only reading of a json document's string
// LEAVES (expandJSONLeaves — keys are names, never expanded, so
// `{"note":"$PROJECT_DIR"}` is data, not a reference). CoerceVarValue
// never errors for VarJSON — a non-JSON text is left a string
// (var_value.go) — so there is no unparseable-json path to narrate: it
// is the notJSON arm, probed braced-only as dispatch expands it.
//
// Probing with a recording lookup reproduces the names the real expansion
// reads instead of guessing at the spelling with a substring match
// (`${PROJECT_DIR2}` is not PROJECT_DIR) — including an INDIRECTED name:
// resolveBracedSegment parses the outer name from the resolved inner
// text, so the lookup answers non-listed names through LookupEnv (the
// reading the screen itself expands with, dispatch's) and `${${A}}`
// with A=PROJECT_DIR set still consults — and records — PROJECT_DIR.
// The price is stated on launchVarsView: a name consulted but discarded
// still counts.
func referencesEngineSuppliedName(raw any, vt VarType) bool {
	s, isText := raw.(string)
	if !isText || !strings.ContainsRune(s, '$') {
		return false
	}
	if vt == VarJSON {
		out, _ := CoerceVarValue(s, vt)
		if text, notJSON := out.(string); notJSON {
			return probesEngineSuppliedName(text, expandPolicy{bracedOnly: true})
		}
		found := false
		expandJSONLeaves(out, func(leaf string) string {
			if probesEngineSuppliedName(leaf, expandPolicy{bracedOnly: true}) {
				found = true
			}
			return leaf
		})
		return found
	}
	return probesEngineSuppliedName(s, expandPolicy{})
}

// probesEngineSuppliedName reports whether expanding s under policy would
// consult an engine-supplied name. Non-listed names answer through
// LookupEnv, as at dispatch, so a name reached through an inner expansion is
// still consulted; a listed name's own value is irrelevant to the probe.
func probesEngineSuppliedName(s string, policy expandPolicy) bool {
	found := false
	expandWithDefault(s, func(name string) string {
		if slices.Contains(EngineSuppliedVarNames, name) {
			found = true
			return ""
		}
		return LookupEnv(name)
	}, policy)
	return found
}

// LaunchBackendName resolves ONE backend field the way the launch-time
// fallback screen reads it: a `{{vars.<name>}}` reference decided by the
// launch's vars (declared defaults under the overrides, a dotted path
// drilled into a json var's document), then the field's `${…}` expansion,
// in the run's order — and "" when the screen cannot name the backend.
// It is name() only: ApplyRunFallback composes more on top of the same
// reader (the default_backend: fallthrough for a node field, routeName
// for a route's `auto`), so this is NOT the screen's verdict, just its
// field reading. Exported for the drill twin it is pinned against in
// pkg/backend/model (routing_twin_test) — its only caller; a host
// screening a launch belongs on ApplyRunFallback itself.
func LaunchBackendName(w *Workflow, overrides map[string]string, field string) string {
	return runBackend.withVars(launchVarsView(w, overrides)).name(field)
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

// providerHintFor maps a policy credential slot onto the provider hint a
// route carries: <provider>_key strips to the provider; a forfait slot
// carries no hint (the family's default precedence serves it); pool and
// unknown slots pass through for the caller to vet.
func providerHintFor(credential string) string {
	if strings.HasSuffix(credential, "_key") {
		return strings.TrimSuffix(credential, "_key")
	}
	return ""
}

// PolicyLadderStage is one rung of the adaptive-routing policy's computed
// ladder (ADR-121 §1): a held (harness, credential) pair from the
// resolved policy's pair order. The MODEL is computed per node at
// materialization — each node's own model is what the crossing maps — so
// a stage carries the pair, never a run-level model.
type PolicyLadderStage struct {
	Harness    string
	Credential string
	On         []string // the resolved policy's trigger set (nil = the chain default)
}

// ApplyPolicyLadder materializes the policy's computed ladder onto the
// compiled workflow through the SAME screen ApplyRunFallback applies —
// agent nodes only, nodes with routes of their own excluded, every stage
// through the C176/C135 predicates and the sandboxed-codex refusal — with
// ONE difference: the stage's model is computed PER NODE through
// nodeModel (the node's own model is what the crossing maps; a pair with
// no delivery-1 mapping for this node is skipped for that node, never
// emitted modelless). The triggers ride the stage's On verbatim — the
// policy's resolved vocabulary, which includes auth and
// transient_exhausted, NOT the chain's narrower default set: the
// launch-time selection exists to fall through the auth-typed
// no-credential failure the default set would stop.
//
// The routes are flagged Policy so the usagecap preflight judges their
// spend surfaces and the timeline can say "policy-selected". Refusals are
// returned exactly like ApplyRunFallback's, named for the ladder.
//
// The runner re-applies the SAME function on its recompiled workflow (the
// publish serializes source, not IR): the two screens are the documented
// twin — the publisher's answer feeds its derivations (advisory), the
// runner's is authoritative.
func ApplyPolicyLadder(w *Workflow, stages []PolicyLadderStage, sandboxed bool, vars map[string]string, nodeModel func(LLMNode) string) []string {
	if w == nil || len(stages) == 0 {
		return nil
	}
	run := runBackend.withVars(launchVarsView(w, vars))

	var refusals []string
	for _, n := range w.Nodes {
		nn, ok := n.(LLMNode)
		if !ok || nn.NodeKind() != NodeAgent {
			continue
		}
		// Eligibility is AUTHOR-declared routes only: the operator's
		// launch-time stages (RunStageSet — applied moments earlier by
		// ApplyRunFallback on the same workflow) COMPOSE with the ladder,
		// they do not veto it. An author who wrote a chain vetted where
		// it may go, and the ladder takes nothing of theirs.
		hasAuthored := false
		stageBase := 0
		for _, fb := range nn.GetFallbacks() {
			if fb.RunStageSet {
				stageBase++
			} else {
				hasAuthored = true
			}
		}
		if hasAuthored {
			continue
		}
		agent, ok := n.(*AgentNode)
		if !ok {
			continue
		}
		nodeBackend := run.effective(nn.GetLLMFields().Backend, w.DefaultBackend)
		perm := EffectivePermission(nn.GetPermission(), w.Permission)
		for i, st := range stages {
			stage := stageBase + i
			routeBackend := run.routeName(st.Harness)
			nodeMdl := ""
			if nodeModel != nil {
				nodeMdl = nodeModel(nn)
			}
			refuse := func(reason string) {
				refusals = append(refusals, fmt.Sprintf(
					"agent %q: policy ladder stage %d (%s) %s", nn.NodeID(), stage+1, st.Harness, reason))
			}
			// The crossing's model, mapped per node from the pair. A pair
			// with no delivery-1 mapping is skipped for THIS node — named,
			// never emitted modelless (ApplyRunFallback's portability
			// refusal would drop it anyway, later and less precisely).
			mdl, ok := llmroute.StageModel(st.Harness, st.Credential, nodeMdl)
			if !ok {
				refuse("has no delivery-1 model mapping — stage skipped")
				continue
			}
			route := Fallback{
				Name:    RunFallbackName,
				Backend: st.Harness,
				Model:   mdl,
				// The stage's PROVIDER HINT, not its policy slot: the hint
				// steers the credential at dispatch ("anthropic", "zai") —
				// the slot spelling ("anthropic_key") would be read as a
				// garbage hint. A forfait slot carries no hint: the family's
				// default precedence already serves it.
				Provider:    providerHintFor(st.Credential),
				On:          st.On,
				RunStage:    stage,
				RunStageSet: true,
				Policy:      true,
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
			if reason := unresolvableToolsReason(routeBackend, nn.GetTools(), mcpWiringVisible(w, n)); reason != "" {
				refuse(reason)
				continue
			}
			if routeBackend == "codex" && sandboxed {
				refuse("targets the codex CLI, which cannot run inside the sandbox this run resolves to — set sandbox: none (workflow block or ITERION_SANDBOX_OVERRIDE), or route to claude_code/claw")
				continue
			}
			agent.Fallbacks = append(agent.Fallbacks, route)
		}
	}
	return refusals
}
