package ir

import (
	"sort"
	"strings"
)

// Provider-routing diagnostics.
const (
	DiagUnknownProvider       DiagCode = "C087" // provider chain token outside the known set (warning)
	DiagProviderChainIgnored  DiagCode = "C088" // multi-provider chain on a backend that ignores the hint (warning)
	DiagMalformedProviderStep DiagCode = "C172" // provider chain element of the `provider:model` form with an empty provider or model part (warning)
)

// KnownProviders is the set of credential-routing hints the runtime
// understands for the per-node `provider:` field (and its comma-separated
// fallback-chain form). Like KnownCapabilities this is a soft set: an
// unknown token is a warning (C087), not an error — a token may be
// meaningful to an out-of-tree backend, and env-ref forms (${VAR}) resolve
// only at run time. Mirrors the hint values matched by
// pkg/backend/delegate.anthropicCredEnvForCLI and the claw registry.
var KnownProviders = map[string]bool{
	"anthropic": true,
	"zai":       true,
	"moonshot":  true,
	"openai":    true,
	"auto":      true,
}

// KnownProviderList renders KnownProviders for the C087 message, sorted.
// Derived rather than retyped: a diagnostic naming a smaller set than the
// compiler enforces tells an author a valid hint is a typo.
func KnownProviderList() string {
	names := make([]string, 0, len(KnownProviders))
	for name := range KnownProviders {
		names = append(names, name)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// hintIgnoringBackends are the backends that do NOT consume the per-node
// provider hint today: claw derives its provider from the model-spec
// prefix, codex ignores the hint entirely, and kimi, grok and opencode let
// their CLI resolve its own credentials — the shared CLIAgentBackend never
// reads Task.ProviderHint, so the hint has nowhere to land in their argv.
//
// pi is here for the other reason C088 exists: it DOES fold the head hint
// into its argv, but the diagnostic is about the CHAIN, and the runtime
// walks a multi-element chain for claude_code alone
// (model.providerFallbackEligible) — so on pi too, everything after the
// first element is a no-op the author should be told about. A multi-element provider chain on these
// is a no-op fall-through (the executor collapses it to the head), so C088
// tells the author the chain won't do anything there.
var hintIgnoringBackends = map[string]bool{
	"claw":     true,
	"codex":    true,
	"kimi":     true,
	"grok":     true,
	"pi":       true,
	"opencode": true,
}

// validateProviders walks every LLM-capable node (agent, judge, llm
// router) and validates the `provider:` field's fallback-chain form:
//
//   - C087 (warning) for any literal chain token whose provider part is
//     outside KnownProviders — catches typos like "anthropc". A token may
//     carry a per-element model (`zai:glm-5.2`); only the provider part is
//     checked. Fields containing a ${VAR} env ref are skipped wholesale:
//     their literal text isn't the resolved value, and a ${VAR:-a,b}
//     default may itself carry commas.
//   - C088 (warning) when a >1-element chain is declared on a backend in
//     hintIgnoringBackends above, so the author knows the fall-through is
//     inert there today.
//   - C172 (warning) for a malformed `provider:model` element — a colon
//     with an empty provider part (":glm-5.2") or empty model part
//     ("zai:"). The runtime trims to whatever is present, so this is a
//     likely typo, not a hard error.
//
// All are warnings, never errors: the run still proceeds, and the
// runtime degrades gracefully (unknown hint → default precedence; chain
// on a hint-ignoring backend → first provider only).
func (c *compiler) validateProviders(w *Workflow) {
	check := func(kind, id, backend, provider string) {
		if provider == "" {
			return
		}
		// Env-ref forms resolve at run time; we can't validate the
		// literal text, and splitting a ${VAR:-a,b} default on commas
		// would misfire. A `{{vars.…}}` reference resolves at dispatch
		// too (validateRoutingFieldRefs holds it to vars.*): its text is
		// not a hint, and saying it "will be ignored" would be false.
		if strings.Contains(provider, "${") || strings.Contains(provider, "{{") {
			return
		}
		tokens := splitProviderChain(provider)
		for _, tok := range tokens {
			hint, model, hasModel := SplitProviderStep(tok)
			if hasModel && (hint == "" || model == "") {
				c.warnfAt(DiagMalformedProviderStep, id, "",
					"%s %q: provider chain element %q is malformed — the `provider:model` form needs both a provider and a model",
					kind, id, tok)
			}
			if hint == "auto" || hint == "" {
				continue
			}
			if !KnownProviders[hint] {
				c.warnfAt(DiagUnknownProvider, id, "",
					"%s %q: provider %q is not a known routing hint (known: %s) — it will be ignored and the node falls back to default credential precedence",
					kind, id, hint, KnownProviderList())
			}
		}
		if len(tokens) > 1 && hintIgnoringBackends[backend] {
			c.warnfAt(DiagProviderChainIgnored, id, "",
				"%s %q: provider fallback chain %q has no effect on backend=%q (the runtime walks a multi-provider chain for claude_code only); the runtime uses only the first provider",
				kind, id, provider, backend)
		}
	}
	for _, n := range w.Nodes {
		switch nn := n.(type) {
		case LLMNode:
			f := nn.GetLLMFields()
			check(nn.NodeKind().String(), nn.NodeID(), effectiveNodeBackend(f.Backend, w.DefaultBackend), f.Provider)
		case *RouterNode:
			if nn.RouterMode == RouterLLM {
				check("router", nn.ID, effectiveNodeBackend(nn.Backend, w.DefaultBackend), nn.Provider)
			}
		}
	}
}

// splitProviderChain splits a literal provider field into its trimmed,
// non-empty tokens (each possibly of the `provider:model` form). Mirrors
// the runtime's resolveProviderChain so compile and runtime agree on what
// counts as a chain element.
func splitProviderChain(provider string) []string {
	parts := strings.Split(provider, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		out = append(out, p)
	}
	return out
}

// SplitProviderStep splits one provider-chain token into its provider hint
// and optional per-element model on the FIRST colon (so a model id that
// itself contains a colon survives). hasModel reports whether a colon was
// present at all, letting callers distinguish "zai" (no model) from "zai:"
// (malformed empty model). It is the single source of truth for the
// `provider:model` element form, shared by the compiler (validateProviders)
// and the runtime (model.resolveProviderChain) so the two never drift.
func SplitProviderStep(token string) (hint, model string, hasModel bool) {
	before, after, found := strings.Cut(token, ":")
	if !found {
		return token, "", false
	}
	return strings.TrimSpace(before), strings.TrimSpace(after), true
}
