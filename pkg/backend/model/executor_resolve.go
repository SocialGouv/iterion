package model

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/detect"
	"github.com/SocialGouv/iterion/pkg/backend/modelroute"
	"github.com/SocialGouv/iterion/pkg/dsl/expr"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// resolveBackendName returns the effective backend name for a node.
//
// Resolution chain (first non-empty wins):
//  1. node.Backend (set on AgentNode/JudgeNode/RouterNode); supports
//     ${VAR}/${VAR:-default} env-var expansion so workflows can pick
//     a backend per environment (e.g. `backend: "${RESCUE_BACKEND:-claude_code}"`).
//     A field that resolves to nothing, and the explicit `auto`, fall through
//     to step 2 — one rule (ir.BackendFallsThrough), shared with the pre-run
//     readings that must answer for the route this one takes.
//  2. workflow-level default (e.defaultBackend, from `default_backend:` or
//     IR Preferences.BackendOrder[0]), with the same fall-through rule
//  3. ITERION_DEFAULT_BACKEND env var (legacy explicit override)
//  4. detect.Resolve over ITERION_BACKEND_PREFERENCE (auto-selection based
//     on credentials present on the host)
//  5. delegate.BackendClaw (hardcoded last-resort fallback)
//
// Step 4 is what makes the studio's empty default template "just work" when
// the user has any credential configured.
func (e *ClawExecutor) resolveBackendName(node ir.Node) string {
	// Launch-time override wins over everything — the operator explicitly
	// re-targeted this node at launch (studio dropdown / CLI --backend).
	if ov := e.modelOverrides.ForNode(node.NodeID(), node.NodeKind()); ov.Backend != "" {
		return ov.Backend
	}
	var backend string
	switch n := node.(type) {
	case *ir.AgentNode:
		backend = n.Backend
	case *ir.JudgeNode:
		backend = n.Backend
	case *ir.RouterNode:
		backend = n.Backend
	}
	backend = e.resolveRoutingField(backend)
	if !ir.BackendFallsThrough(backend) {
		return backend
	}
	// The workflow's `default_backend:` is a routing field like a node's:
	// the same template-then-env reading, or `{{vars.b}}` there became the
	// backend NAME at dispatch.
	if d := e.resolveRoutingField(e.defaultBackend); !ir.BackendFallsThrough(d) {
		return d
	}
	// A PROGRAM-ONLY executor stops here. Past this point the answer comes
	// from the host — an env var, then a probe of the machine's credentials —
	// and a static analysis that read either would give one file two verdicts
	// on two machines. Its caller reads the empty answer as "the IR is all
	// there is", which is what every other pre-run analysis does with an
	// unnamed backend.
	if e.staticProgramOnly {
		return ""
	}
	if env := os.Getenv("ITERION_DEFAULT_BACKEND"); env != "" {
		return env
	}
	if resolved := e.detectorResolve(); resolved != "" {
		return e.gatewayCapableDetection(node, resolved)
	}
	return delegate.BackendClaw
}

// gatewayCapableDetection keeps host auto-detection from naming a backend
// the node's chain cannot run on. A node whose chain carries an
// OpenAI-compatible gateway route — a gateway `model:`, a gateway provider
// hint, in any position — can only be served by claw, which is exactly what
// refuseGatewayCrossing refuses every other backend for at task build.
// Detection that ignores it concretises claude_code on any machine with the
// `claude` CLI (the common operator machine), and a chain whose gateway
// element sits in SECOND position is worse than the head case: the head
// fails on its own merits, the gateway fallback is refused at its own build,
// and the rescue route the author declared never fires — the one case a
// fallback exists for (#2785). Detection answers for the HOST — the
// credentials present, in the operator's ITERION_BACKEND_PREFERENCE order —
// and never reads the node's route, which is the one input this answer
// needs. Correcting it toward the one backend that can serve the chain
// therefore overrides no operator choice: the preference order says nothing
// about routes, and the node's own `backend:`, the workflow's
// `default_backend:`, ITERION_DEFAULT_BACKEND and a launch override all keep
// their answer and their build-time refusal.
//
// The reading is the WHOLE chain's, taken over the walk dispatch itself
// would take under the detected backend (resolveChainUnder), so the question
// and the walk cannot drift.
func (e *ClawExecutor) gatewayCapableDetection(node ir.Node, detected string) string {
	if backendServesGatewayRoute(detected) || !e.chainNeedsGateway(node, detected) {
		return detected
	}
	if e.logger != nil && e.noteGatewayReroute(node.NodeID(), detected) {
		e.logger.Warn("[%s] auto-detection picked %q, which cannot serve this node's OpenAI-compatible gateway route — selecting claw, the only backend that serves one (set `backend: claw` to make the choice explicit)",
			node.NodeID(), detected)
	}
	return delegate.BackendClaw
}

// noteGatewayReroute dedupes the redirect warn: the seams ask
// resolveBackendName several times for one node (admission, the route walk,
// dispatch), and one redirect is one fact. Returns true once per
// (node, detected) pair.
func (e *ClawExecutor) noteGatewayReroute(nodeID, detected string) bool {
	e.rerouteWarnMu.Lock()
	defer e.rerouteWarnMu.Unlock()
	if e.rerouteWarned == nil {
		e.rerouteWarned = map[string]bool{}
	}
	key := nodeID + "\x00" + detected
	if e.rerouteWarned[key] {
		return false
	}
	e.rerouteWarned[key] = true
	return true
}

// chainNeedsGateway reports whether any element of the chain the node would
// walk under `detected` is a gateway route: a gateway model of its own, or
// the node's baseline it inherits, or the gateway provider hint. Only
// elements that INHERIT the node's resolved backend count — an element
// pinning its own `backend:` is served by it and judged at its own build
// whatever detection answers, and an `action: skip` element runs no model.
func (e *ClawExecutor) chainNeedsGateway(node ir.Node, detected string) bool {
	base := e.baselineModel(node)
	for _, el := range e.resolveChainUnder(node, detected) {
		if el.Skip || el.Backend != "" {
			continue
		}
		m := el.Model
		if m == "" {
			m = base
		}
		if modelroute.Parse(m).Gateway() {
			return true
		}
		if strings.EqualFold(strings.TrimSpace(el.Provider), modelroute.OpenAICompatible) {
			return true
		}
	}
	return false
}

// EffectiveBackendName exposes the exact runtime resolution to safety checks.
// In particular, callers must not reimplement this chain and accidentally miss
// launch overrides, environment defaults, or credential-based auto-detection.
func (e *ClawExecutor) EffectiveBackendName(node ir.Node) string {
	return e.resolveBackendName(node)
}

// chainElement is one element of a node's resolved fallback chain: a
// full routing step (backend + credential hint + model), any of which
// may be empty to inherit the node's resolved value.
//
// Two producers fill it, and the difference is load-bearing:
//   - the legacy `provider: "a,b"` field (ADR-004) varies Provider and
//     optionally Model, and NEVER Backend — every element runs the same
//     backend, so the task can be reused between attempts;
//   - the `fallbacks:` block (ADR-087) may vary Backend too, which
//     re-shapes the delegate.Task and forces a rebuild per element.
//
// `Backend == ""` on every element is therefore the machine-readable
// signal that a chain is hint-only, which is what the collapse guard
// for hint-ignoring backends keys on (see chainIsHintOnly).
type chainElement struct {
	// Label is the stable id this element is named by in logs, the
	// model_fallback event and the run report — a `fallbacks:` entry
	// name. Empty for a legacy `provider:` element, which falls back to
	// its provider/model rendering.
	Label    string
	Backend  string // "" = inherit the node's resolved backend
	Provider string // routing hint ("" = auto / defer to process-env precedence)
	Model    string // per-element model override ("" = inherit the node's model)
	// On is the set of failure categories that may route TO this element
	// (i.e. it filters the failure of its PREDECESSOR). nil means the
	// package default; it is only ever non-nil for a `fallbacks:`
	// element, since a legacy chain falls through on any error.
	On []delegate.FallbackCategory
	// Skip marks an `action: skip` terminal route (ir.FallbackActionSkip):
	// reaching it completes the node with a zero-value output stamped
	// `_skipped` instead of executing anything. Always last in the chain
	// (compile-time C173).
	Skip bool
	// FallbackIndex is set only for a launch-time run fallback stage. It
	// preserves the launch array index even when an earlier stage was refused.
	FallbackIndex *int
	// CrossHarness is the run's resolved cross-harness posture riding
	// THIS policy stage (ADR-121 § Delivery 2): active ("reuse" |
	// "restart") makes the dispatch mark the model_fallback when this
	// element's crossing changes backend. Runtime-only, like
	// FallbackIndex — copied in resolveChain, never authored.
	CrossHarness string
}

// defaultFallbackTriggers is the `on:` set a `fallbacks:` route gets
// when the author declares none.
//
// It is a closed POSITIVE list, and neither omission is accidental:
//   - `any` is excluded because a budget cap or a schema-shape failure
//     re-fails identically on every route, so routing on them buys
//     nothing and costs a second credential;
//   - `auth` is excluded because AuthFailedRecipe deliberately routes a
//     rejected credential to a human rather than automating around it —
//     enabling it by default would reverse a shipped, argued decision.
//
// Both remain available explicitly.
var defaultFallbackTriggers = []delegate.FallbackCategory{
	delegate.FallbackUsageWindow,
	delegate.FallbackUnavailable,
}

// resolveChain builds a node's complete ordered route list: its legacy
// `provider:` hint chain first (which always yields at least the node's
// own route), then each `fallbacks:` entry in declaration order.

// The operator's launch-time route is NOT resolved here: it is
// materialised onto the node's `fallbacks:` at launch
// (ir.ApplyRunFallback), so it passes the same safety screen as an
// authored route and is visible to the three pre-run analyses. By the
// time the executor sees it, it is an authored route like any other.
//
// The two surfaces stay independent by design. `provider:` swaps a
// credential on one backend and falls through on ANY error, which is
// what every shipped chain relies on; `fallbacks:` is a full
// re-resolution filtered by `on:`. Desugaring one into the other would
// silently change the first's semantics and invalidate C088, which two
// catalog bots document in their own comments.
func (e *ClawExecutor) resolveChain(node ir.Node) []chainElement {
	return e.resolveChainUnder(node, e.resolveBackendName(node))
}

// resolveChainUnder is resolveChain with the node's resolved backend handed
// in — the one reading auto-selection can take without recursing into its
// own resolution (resolveBackendName → gatewayCapableDetection → the chain
// → resolveBackendName would be a cycle; the detected backend IS the answer
// the chain read needs).
func (e *ClawExecutor) resolveChainUnder(node ir.Node, nodeBackend string) []chainElement {
	chain := e.resolveProviderChain(node)
	for _, fb := range nodeFallbacks(node) {
		if !e.fallbackWhenActive(node.NodeID(), fb) {
			continue
		}
		el := chainElement{
			Label:    fb.Name,
			Backend:  e.resolveRoutingField(fb.Backend),
			Provider: e.resolveRoutingField(fb.Provider),
			Model:    e.resolveRoutingField(fb.Model),
			Skip:     fb.Action == ir.FallbackActionSkip,
		}
		if fb.RunStageSet {
			stage := fb.RunStage
			el.FallbackIndex = &stage
		}
		el.CrossHarness = fb.CrossHarness
		if el.Provider == "auto" {
			el.Provider = "" // explicit auto → process-env precedence
		}
		el.On = resolveTriggers(fb.On)
		chain = append(chain, el)
	}
	return dedupeChain(e.dropForbidden(node, chain), nodeBackend, e.baselineModel(node))
}

// EffectiveFallbackBackends reports the backends a node's `fallbacks:` routes
// resolve to for THIS run, in chain order and deduplicated: the chain dispatch
// itself walks (resolveChain), so the run's vars have been read through
// resolveRoutingField, each route's `when:` gate has been evaluated against
// them, and a launch-time route materialised onto the node
// (ir.ApplyRunFallback) is in it like an authored one.
//
// The node's OWN route is not in the answer — a caller resolves that through
// EffectiveBackendName, and a route that names that same backend to swap a
// credential on it leaves nothing for a reader of this answer to judge — and
// neither is a route that names no backend of its own: that one inherits the
// node's, so it adds no route to read. A spelling the run resolves to nothing
// (`${VAR}` unset) inherits it too, which is what dispatch does with it
// (dedupeChain's effective(), then the dispatch's own empty-Backend reading).
// An `action: skip` route is absent for the same reason read the other way:
// it completes the node where it is reached, so no backend is ever resolved
// for it.
//
// A route whose `when:` gate the executor cannot evaluate is absent too: the
// gate deactivates it (fallbackWhenActive), and dispatch walks that same
// reading, so the route is out of both chains.
//
// It exists so a pre-run analysis that keys on a node's routes can ask instead
// of re-deriving them from the IR, which is wrong in both directions: a route
// spelled `{{vars.fb}}` is a backend NAME at dispatch and nothing in the IR,
// and a route the run's vars gate off is in the IR and never in the chain. The
// answer may still carry a `{{…}}` span, for the one reading
// resolveRoutingField does not make: a template over a namespace that does not
// exist before the node runs (`{{outputs.…}}`) stays as written, and a caller
// reads it as the unresolved route it is.
//
// Same seam, and same reason, as EffectiveBackendName and EffectiveToolNames.
func (e *ClawExecutor) EffectiveFallbackBackends(node ir.Node) []string {
	if e == nil || node == nil {
		return nil
	}
	// Seeded with the node's own route, so a `fallbacks:` entry that names it
	// to vary a credential is recognised as the node's route and not as one
	// more for a caller to read.
	seen := map[string]bool{e.resolveBackendName(node): true}
	var out []string
	for _, el := range e.resolveChain(node) {
		// A legacy `provider:` element never varies Backend (chainElement's
		// own note), so what is left here is the `fallbacks:` block's.
		if el.Skip || el.Backend == "" || seen[el.Backend] {
			continue
		}
		seen[el.Backend] = true
		out = append(out, el.Backend)
	}
	return out
}

// fallbackWhenActive evaluates a route's optional `when:` gate against the
// run's vars. The compiler guarantees the expr parses and reads only
// vars.* (C173), so a runtime failure here is exceptional; it deactivates
// the route WITH a warning rather than failing the node — a broken gate on
// a fallback must not take down the primary it exists to back up.
//
// Its posture is DISPATCH's, not the walk's: a gate the run's vars cannot
// answer deactivates the route, on the executor's complete reading. The
// launch-time walk reads the same grammar through FallbackWhenActive, which
// keeps what it cannot answer — a walk that dropped a provider the run
// might spend would under-attribute the run's own cost.
func (e *ClawExecutor) fallbackWhenActive(nodeID string, fb ir.Fallback) bool {
	if strings.TrimSpace(fb.When) == "" {
		return true
	}
	ast, err := expr.Parse(fb.When)
	if err == nil {
		var active bool
		active, err = ast.EvalBool(&expr.Context{
			Vars: func(path []string) any { return lookupPath(e.vars, path) },
		})
		if err == nil {
			return active
		}
	}
	if e.logger != nil {
		e.logger.Warn("[%s] fallback %q: when: %q failed to evaluate (%v) — route treated as inactive",
			nodeID, fb.Name, fb.When, err)
	}
	return false
}

// FallbackWhenActive evaluates a route's optional `when:` gate against an
// explicit vars reading — the launch-time walk's entry to the gate, the
// grammar `ir.DecideOnLaunchVars` answers. Its posture is the WALK's: a
// gate the vars cannot answer (a name absent from vars, a late one, a
// parse or evaluation failure) keeps the route ACTIVE, because the walk
// exists to attribute spend and must never drop a provider the run might
// spend. Dispatch's own reading is the method fallbackWhenActive, which
// deactivates a gate it cannot evaluate WITH a warning — at runtime a
// broken gate must not take down the primary it backs up. One grammar, two
// named postures; warn is nil where a derivation has no logger.
func FallbackWhenActive(vars map[string]any, nodeID string, fb ir.Fallback, warn func(format string, args ...any)) bool {
	if strings.TrimSpace(fb.When) == "" {
		return true
	}
	ast, err := expr.Parse(fb.When)
	if err != nil {
		if warn != nil {
			warn("[%s] fallback %q: when: %q failed to parse (%v) — route kept active", nodeID, fb.Name, fb.When, err)
		}
		return true
	}
	holds, decided := ir.DecideOnLaunchVars(ast, vars)
	if !decided {
		return true
	}
	return holds
}

// ResolveRoutingField is the reading dispatch takes of a routing field — a
// node's `backend:`, a route's, the workflow default — against this executor's
// vars and environment (resolveRoutingField).
//
// It exists for a caller that owes a pre-run answer per FIELD where
// resolveChain answers per chain. pkg/dryrun does: on a launch its program does
// not fully specify it keeps the author's spelling of a TEMPLATED field, the
// one part of it that launch decides, and owes the program's own reading of
// every other spelling — and a second copy of that reading is the drift the
// seams above exist to remove. The answer is taken against the vars a caller
// handed THIS executor (SetVars), so it is the answer its chain would be built
// from.
func (e *ClawExecutor) ResolveRoutingField(raw string) string {
	return e.resolveRoutingField(raw)
}

// lookupPath walks a dotted path into a nested map, returning nil when any
// segment is absent — the same "absent path is nil" contract the expr
// evaluator expects from its namespaces.
func lookupPath(m map[string]any, path []string) any {
	var cur any = m
	for _, seg := range path {
		mm, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = mm[seg]
	}
	return cur
}

// baselineModel is the model an element that pins none of its own runs:
// the launch override if the operator set one, else the node's `model:`
// with env refs expanded.
//
// It deliberately stops short of claw's detector-suggested default
// (buildTask applies that when the model is still empty at dispatch):
// this exists to compare two routes for equality, and an unresolved
// empty baseline compares equal to another unresolved empty one, which
// is the right answer.
func (e *ClawExecutor) baselineModel(node ir.Node) string {
	if ov := e.modelOverrides.ForNode(node.NodeID(), node.NodeKind()); ov.Model != "" {
		return ov.Model
	}
	var m string
	switch n := node.(type) {
	case *ir.AgentNode:
		m = n.Model
	case *ir.JudgeNode:
		m = n.Model
	case *ir.RouterNode:
		m = n.Model
	}
	return e.resolveRoutingField(m)
}

// resolveRoutingVars resolves a routing field's `{{vars.*}}` spans against
// an explicit vars reading, the way dispatch resolves the template half of
// resolveRoutingField. Every other namespace stays as written, and so does
// a name in late — its value was decided by the reading process's own
// environment, so a field resolved through it is exactly as opaque as the
// raw `${…}` the runner would expand with a different environment
// (ir.LaunchVarsView's late set, ADR-129 §4). The `${…}` pass is
// deliberately NOT run here: the launch-time walk answers its provenance
// questions (envDeferred, resolveRouteBackend's fromEnv bit) on the raw
// spelling, and expanding it here would take this process's environment as
// the runner's word.
func resolveRoutingVars(raw string, vars map[string]any, late map[string]bool) string {
	if vars == nil || !strings.Contains(raw, "{{") {
		return raw
	}
	effective := vars
	if len(late) > 0 {
		effective = make(map[string]any, len(vars))
		for name, v := range vars {
			if !late[name] {
				effective[name] = v
			}
		}
	}
	return (&TemplateResolver{Vars: effective}).Resolve(raw, nil, nil)
}

// ResolveRoutingFieldWith is the dispatch reading of a routing field —
// template, then environment, then trim — against an explicit vars reading.
// It is the one function a launch-time reader uses when it can reproduce
// the run's vars; the executor's method delegates with its own.
func ResolveRoutingFieldWith(vars map[string]any, late map[string]bool, raw string) string {
	return strings.TrimSpace(ir.ExpandEnvWithDefault(resolveRoutingVars(raw, vars, late)))
}

// resolveRoutingField is the one reading every routing field takes on the
// dispatch path — a node's `model:`, `backend:` and `provider:`, the same
// three on each `fallbacks:` route, an llm router's model, a human or review
// node's companion model, a verified action's recovery model. A `{{vars.…}}`
// template resolves first (the run's vars are the one namespace that exists
// before the node runs; any other stays as written), then the `${VAR:-default}`
// environment form, then the value is trimmed. Without the first step a
// `model: "{{vars.model}}"` reached the backend as written: the claw registry
// refused it as an invalid spec, the claude CLI asked for a model literally
// named `{{vars.model}}` (examples/clarify; feed-watch's bilan). The compiler
// holds the same line (C033 for an undeclared var, C148 for any other
// `{{…}}` span). The resolver here carries the vars and nothing else — no
// secret guard, no node input, no run state — so a `{{secrets.…}}` that
// slipped past the compiler cannot render a secret into a model id that
// backends log and events record. The environment form is expanded AFTER
// the template on purpose: a var's default is where the DSL writes
// `${MODEL:-anthropic/x}` (examples/clarify), and the executor receives
// that default as written. The launch-time pre-flights (wire_reach.go,
// override_fold.go) read the same template half through RouteView.field —
// the run's vars by provenance, a late name kept as written — and run
// their own `${…}` posture on top, so a template there is a name when the
// run's own vars decide it, and a hint that defers when they do not.
func (e *ClawExecutor) resolveRoutingField(raw string) string {
	return ResolveRoutingFieldWith(e.vars, nil, raw)
}

// dedupeChain drops any element that would re-issue the call its
// predecessor just made.
//
// This is the protection the old providerFallbackEligible collapse
// bought and that merging three sources (provider hints, authored
// routes, the run-level route) can otherwise lose: a route resolving to
// the same backend+credential+model as the one before it cannot succeed
// where that one failed, and pays a second full retry budget to prove
// it. Comparison is on the EFFECTIVE backend AND model, so a route that
// names the node's own backend — or restates its `model:` verbatim
// rather than inheriting it — is recognised as the duplicate it is.
func dedupeChain(chain []chainElement, nodeBackend, baseModel string) []chainElement {
	if len(chain) < 2 {
		return chain
	}
	effective := func(el chainElement) chainElement {
		if el.Backend == "" {
			el.Backend = nodeBackend
		}
		if el.Model == "" {
			el.Model = baseModel
		}
		return el
	}
	out := chain[:1]
	for _, el := range chain[1:] {
		// A skip element executes nothing, so it can never "re-issue the
		// call its predecessor just made" — and its empty backend/model
		// would otherwise compare equal to the node's own route.
		if !el.Skip && !out[len(out)-1].Skip && sameRoute(effective(out[len(out)-1]), effective(el)) {
			continue
		}
		out = append(out, el)
	}
	return out
}

// resolveTriggers maps a route's declared `on:` tokens onto categories.
// An empty list takes the package default; an explicit `any` yields nil,
// which the walker reads as "accept every condition".
func resolveTriggers(on []string) []delegate.FallbackCategory {
	if len(on) == 0 {
		return defaultFallbackTriggers
	}
	out := make([]delegate.FallbackCategory, 0, len(on))
	for _, raw := range on {
		token := strings.TrimSpace(ir.ExpandEnvWithDefault(raw))
		if token == "" {
			continue
		}
		if token == "any" {
			return nil
		}
		out = append(out, delegate.FallbackCategory(token))
	}
	if len(out) == 0 {
		return defaultFallbackTriggers
	}
	return out
}

// nodeFallbacks returns a node's compiled `fallbacks:` routes, or nil
// for a node kind that cannot declare them.
func nodeFallbacks(node ir.Node) []ir.Fallback {
	if n, ok := node.(ir.LLMNode); ok {
		return n.GetFallbacks()
	}
	return nil
}

// sameRoute reports whether two elements resolve to the same call —
// same backend, same credential hint, same model. It deliberately
// ignores Label and On: two elements that differ only in their name or
// trigger filter would still re-issue an identical request, which is
// the waste the consecutive-duplicate collapse exists to avoid.
func sameRoute(a, b chainElement) bool {
	return a.Backend == b.Backend && a.Provider == b.Provider && a.Model == b.Model
}

// resolveProvider returns the first (preferred) credential-routing hint
// for a node, or "" to defer to the global precedence. It is the
// single-value façade over resolveProviderChain, kept for the call
// sites and tests that only need the head of the chain.
//
// Known hint values (matched by anthropicCredEnvForCLI / the claw registry):
//   - "anthropic" — force ANTHROPIC_API_KEY / CLAUDE_CONFIG_DIR, skip the
//     facades even when their keys are set on the process.
//   - "zai" — force z.ai routing (ANTHROPIC_BASE_URL=z.ai facade +
//     ANTHROPIC_AUTH_TOKEN=$ZAI_API_KEY).
//   - "moonshot" — force Moonshot routing (the Kimi family, same facade
//     shape with MOONSHOT_BASE_URL + ANTHROPIC_AUTH_TOKEN=$MOONSHOT_API_KEY).
//   - "openai" — for claw/OpenAI-compat: force OPENAI_API_KEY direct.
//   - "auto" / "" — current process-env-driven precedence.
func (e *ClawExecutor) resolveProvider(node ir.Node) (string, error) {
	chain := e.resolveProviderChain(node)
	if len(chain) == 0 {
		// The allowlist dropped every element: there is no route to name,
		// and "" IS the defer-to-process spelling — returning it would read
		// "every route refused" as "no preference, proceed". The refusal
		// travels; the dispatch sites refuse the same way.
		return "", fmt.Errorf("node %q: every route resolves outside this run's provider allowlist (%s): %w",
			node.NodeID(), ProviderAllowlistNames(e.allowedProviders), ErrProviderOutsideAllowlist)
	}
	return chain[0].Provider, nil
}

// resolveProviderChain resolves the per-node `provider:` field into an
// ordered fallback chain of credential-routing hints, each optionally
// carrying its own model. A single value (the historical form, incl.
// `${RESCUE_PROVIDER:-zai}`) yields a one-element chain, so existing
// workflows behave exactly as before.
//
// A comma-separated value (`provider: "anthropic,zai,openai"`) yields
// the ordered list: the executor tries each provider in turn, falling
// through to the next on a hard failure beyond the retry budget (see
// dispatchWithProviderFallback). This generalises the single-node
// RESCUE_PROVIDER escape hatch into a declarative chain.
//
// Each element may pin its own model with a `provider:model` token
// (`provider: "zai:glm-5.2,anthropic:claude-opus-4-8"`): on fall-through
// the executor swaps BOTH the credential hint AND the wire model, so a
// chain can route a provider-specific model (glm-5.2 on z.ai) to a
// different model on the next provider (claude-opus-4-8 on anthropic).
// The split is on the FIRST colon only, so a model id that itself
// contains a colon survives intact. A token without a colon keeps the
// node's `model:` (Model == "").
//
// Env expansion runs on the whole field FIRST (the run's vars resolved,
// then the environment), then parseProviderChain splits the result: the
// same parse a launch-time override goes through, so both spellings of
// one pin reach dispatch identically.
func (e *ClawExecutor) resolveProviderChain(node ir.Node) []chainElement {
	// A launch-time provider override (a routing-policy cell or the
	// launch's own rows) carries the same vocabulary as the node's own
	// field — a validated cell may name a multi-step chain or a
	// `provider:model` step — so it is read through the ONE parse below.
	// Returning the raw string as a single element handed dispatch a
	// provider name no credential family matches, while the write gate
	// and the credential preview certified each step of the chain.
	if ov := e.modelOverrides.ForNode(node.NodeID(), node.NodeKind()); ov.Provider != "" {
		return e.dropForbidden(node, parseProviderChain(e.resolveRoutingField(ov.Provider)))
	}
	var raw string
	switch n := node.(type) {
	case *ir.AgentNode:
		raw = n.Provider
	case *ir.JudgeNode:
		raw = n.Provider
	case *ir.RouterNode:
		raw = n.Provider
	}
	return e.dropForbidden(node, parseProviderChain(e.resolveRoutingField(raw)))
}

// dropForbidden drops every chain element whose resolved provider is
// outside the run's allowlist (ADR-129 §5, the dispatch half): the launch
// pruned the route, and the chain must not burn a retry budget re-arming
// it. An element naming no provider ("") is kept only when nothing else
// names one either: the model the element carries — or the node baseline
// it inherits at build — is the route the build resolves, so the SAME
// reading the walk runs decides the keep (a GLM id on the anthropic wire
// spends z.ai's credential — RouteProviderHint mints that hint at build —
// and a `provider/` prefix names its own). What survives is the true
// defer-to-process element: no provider, and no model-implied one. The
// chain may come back EMPTY — every live dispatch site refuses an empty
// chain (dispatchWithObservability, and dispatchChain beneath them)
// rather than silently re-filling it with a defer-to-process element,
// which would spend a forbidden provider in silence.
func (e *ClawExecutor) dropForbidden(node ir.Node, chain []chainElement) []chainElement {
	if e.allowedProviders == nil {
		return chain
	}
	base := e.baselineModel(node)
	out := make([]chainElement, 0, len(chain))
	dropped := false
	for _, el := range chain {
		p := strings.ToLower(strings.TrimSpace(el.Provider))
		if p == "" {
			m := el.Model
			if m == "" {
				m = base
			}
			p = providerOfModelSpec(m)
		}
		if p != "" && !e.allowedProviders[p] {
			dropped = true
			continue
		}
		out = append(out, el)
	}
	if !dropped {
		return chain
	}
	return out
}

// parseProviderChain splits a resolved provider field into the ordered
// chain dispatch walks — the ONE parse of the provider vocabulary, shared
// by the node's DSL field and every launch-time override so a pin
// certified at a write surface is walked at dispatch exactly as the
// validator and the credential preview parsed it. ir.SplitProviderStep is
// the single source of truth for the `provider:model` element form,
// shared with the compiler's validateProviders so parse and validation
// never drift; the hint is folded to its lower-case route form (the form
// every consumer compares — see delegate.normalizeProviderHint). An
// explicit "auto" normalises to "" (defer to process-env precedence) but
// is kept as a chain element; genuinely empty tokens (stray, leading or
// trailing commas) are dropped; consecutive duplicate steps are
// collapsed. The chain is never empty: an unset/blank field yields a
// single auto attempt.
func parseProviderChain(expanded string) []chainElement {
	chain := make([]chainElement, 0, 4)
	for _, part := range strings.Split(expanded, ",") {
		token := strings.TrimSpace(part)
		if token == "" {
			continue // stray, leading or trailing comma
		}
		hint, model, _ := ir.SplitProviderStep(token)
		step := chainElement{Provider: strings.ToLower(hint), Model: model}
		if step.Provider == "auto" {
			step.Provider = "" // explicit auto → process-env precedence
		}
		if len(chain) > 0 && sameRoute(chain[len(chain)-1], step) {
			continue // collapse consecutive duplicates
		}
		chain = append(chain, step)
	}
	if len(chain) == 0 {
		return []chainElement{{}}
	}
	return chain
}

// detectorResolve picks the first available backend in
// ITERION_BACKEND_PREFERENCE order, or "" when nothing is available.
func (e *ClawExecutor) detectorResolve() string {
	report := e.detector.Get(context.Background())
	return detect.Resolve(report.PreferenceOrder, report.Backends)
}

// detectorSuggestedModel returns the model spec for claw based on
// detected providers, or "" when none are available (the registry then
// emits a clear "no model" error).
func (e *ClawExecutor) detectorSuggestedModel() string {
	report := e.detector.Get(context.Background())
	return detect.SuggestedModel(detect.BackendClaw, report.Providers)
}
