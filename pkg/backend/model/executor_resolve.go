package model

import (
	"context"
	"os"
	"strings"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/detect"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// resolveBackendName returns the effective backend name for a node.
//
// Resolution chain (first non-empty wins):
//  1. node.Backend (set on AgentNode/JudgeNode/RouterNode); supports
//     ${VAR}/${VAR:-default} env-var expansion so workflows can pick
//     a backend per environment (e.g. `backend: "${RESCUE_BACKEND:-claude_code}"`).
//  2. workflow-level default (e.defaultBackend, from `default_backend:` or
//     IR Preferences.BackendOrder[0])
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
	backend = ir.ExpandEnvWithDefault(backend)
	if backend != "" && backend != "auto" {
		return backend
	}
	if e.defaultBackend != "" {
		return e.defaultBackend
	}
	if env := os.Getenv("ITERION_DEFAULT_BACKEND"); env != "" {
		return env
	}
	if resolved := e.detectorResolve(); resolved != "" {
		return resolved
	}
	return delegate.BackendClaw
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
	chain := e.resolveProviderChain(node)
	forbidMetered := forbidMeteredFallback()
	for _, fb := range nodeFallbacks(node) {
		if fb.Metered && forbidMetered {
			// The instance-wide half of the ADR-087 §12 consent pair:
			// `metered: true` is the AUTHOR's consent to spend real
			// money on a fall-through, and this is the OPERATOR's veto
			// of it. It matters beyond one run — metered spend is
			// charged to the parent org, so a chain that escapes onto a
			// metered key can trip the monthly cost cap and deny other
			// teams' launches. Pruning the route (rather than failing
			// the node) keeps the rest of the chain usable: the run
			// degrades to the routes the operator does allow.
			if e.logger != nil {
				e.logger.Warn("[%s] fallback route %q is metered and ITERION_FORBID_METERED_FALLBACK=1 — route dropped",
					node.NodeID(), fb.Name)
			}
			continue
		}
		el := chainElement{
			Label:    fb.Name,
			Backend:  strings.TrimSpace(ir.ExpandEnvWithDefault(fb.Backend)),
			Provider: strings.TrimSpace(ir.ExpandEnvWithDefault(fb.Provider)),
			Model:    strings.TrimSpace(ir.ExpandEnvWithDefault(fb.Model)),
		}
		if el.Provider == "auto" {
			el.Provider = "" // explicit auto → process-env precedence
		}
		el.On = resolveTriggers(fb.On)
		chain = append(chain, el)
	}
	return dedupeChain(chain, e.resolveBackendName(node), e.baselineModel(node))
}

// forbidMeteredFallback reports the operator's instance-wide refusal of
// any `metered: true` route. It mirrors ITERION_FORBID_SUBSCRIPTION_OAUTH
// in the opposite direction (that one refuses metered→subscription; this
// one refuses forfait→metered) and is the greppable escape hatch a
// load-bearing spend limit is required to carry.
func forbidMeteredFallback() bool {
	return strings.TrimSpace(os.Getenv("ITERION_FORBID_METERED_FALLBACK")) == "1"
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
	return strings.TrimSpace(ir.ExpandEnvWithDefault(m))
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
		if sameRoute(effective(out[len(out)-1]), effective(el)) {
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
//   - "anthropic" — force ANTHROPIC_API_KEY / CLAUDE_CONFIG_DIR, skip z.ai
//     even when ZAI_API_KEY is set on the process.
//   - "zai" — force z.ai routing (ANTHROPIC_BASE_URL=z.ai facade +
//     ANTHROPIC_AUTH_TOKEN=$ZAI_API_KEY).
//   - "openai" — for claw/OpenAI-compat: force OPENAI_API_KEY direct.
//   - "auto" / "" — current process-env-driven precedence.
func (e *ClawExecutor) resolveProvider(node ir.Node) string {
	return e.resolveProviderChain(node)[0].Provider
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
// dispatchChain). This generalises the single-node
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
// Env expansion runs on the whole field FIRST, then the result is split
// on commas — so an env var may supply the entire chain
// (`${PROVIDERS:-anthropic,zai}`) and a `:-default` may itself contain a
// comma. (Because expansion precedes the colon split, a `${VAR:-x}`
// default's `:-` is never mistaken for a provider:model separator.)
// Tokens are trimmed; an explicit "auto" normalises to "" (defer to
// process-env precedence) but is kept as a chain element; genuinely
// empty tokens (stray/trailing commas) are dropped; consecutive
// duplicate steps are collapsed. The chain is never empty: an
// unset/blank field yields a single auto attempt.
func (e *ClawExecutor) resolveProviderChain(node ir.Node) []chainElement {
	// Launch-time provider override collapses the chain to the chosen hint.
	if ov := e.modelOverrides.ForNode(node.NodeID(), node.NodeKind()); ov.Provider != "" {
		return []chainElement{{Provider: ov.Provider}}
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
	expanded := ir.ExpandEnvWithDefault(raw)
	chain := make([]chainElement, 0, 4)
	for _, part := range strings.Split(expanded, ",") {
		token := strings.TrimSpace(part)
		if token == "" {
			continue // stray, leading or trailing comma
		}
		// ir.SplitProviderStep is the single source of truth for the
		// `provider:model` element form, shared with the compiler's
		// validateProviders so parse and validation never drift.
		hint, model, _ := ir.SplitProviderStep(token)
		step := chainElement{Provider: hint, Model: model}
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
