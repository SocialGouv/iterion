package delegate

import (
	"net/url"
	"strings"

	"github.com/SocialGouv/iterion/pkg/secrets"
)

// ---------------------------------------------------------------------------
// Facade substitution: the served model is not the requested label (#2772).
//
// An Anthropic-shaped facade (z.ai, Moonshot) answers ANY model id with the
// one model its account aliases it to: the call succeeds, declared and
// effective ids AGREE on the wire (emitModelDrift stays silent), and the
// run's record names a model that never saw the prompt — review-pr's claude
// primary served by GLM under the claude label, with the declared `glm`
// rescue route never firing because the primary "succeeded". The delegate
// is the one place that already knows both halves of the mismatch — the
// route it just resolved (FacadeSlotEnvKey) and the id it was asked to
// serve — so it refuses BEFORE the spawn, typed ErrModelUnavailable: a
// model the credential cannot reach IS the class ("bad id, unauthorized,
// or withdrawn" — here: substituted), it classifies `unavailable`, and a
// declared `fallbacks:` route whose `on:` names it fires by the chain's own
// machinery. Nothing is spent on the doomed call.
// ---------------------------------------------------------------------------

// facadeServedModelPrefix is the served-family table of the anthropic-wire
// facades: every z.ai id starts with "glm", every Moonshot id with "kimi".
// Anchored on the PREFIX, never a substring — "notglm" is not a GLM id
// (model.modelServedByZAI states the rule and its reason). A facade joining
// the wire adds its family HERE, once.
func facadeServedModelPrefix(slot string) string {
	switch slot {
	case string(secrets.ProviderZAI):
		return "glm"
	case string(secrets.ProviderMoonshot):
		return "kimi"
	}
	return ""
}

// FacadeServesModelID reports whether the named facade slot serves the wire
// model id — the id AFTER any routing prefix is stripped. A slot that names
// no anthropic-wire facade serves whatever it is asked for as far as this
// guard is concerned (true): the substitution verdict belongs to the slots
// the resolver can stamp, and a false judgement there would refuse routes
// nobody substituted. Exported for the claw registry, whose z.ai synthesis
// and moonshot factory are the same facade shape on the other backend.
func FacadeServesModelID(slot, wireID string) bool {
	prefix := facadeServedModelPrefix(slot)
	if prefix == "" {
		return true
	}
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(wireID)), prefix)
}

// sameEndpoint compares two endpoint URLs loosely: case, surrounding
// whitespace and a trailing slash carry no routing meaning, and an explicit
// default port (:443 on https, :80 on http) names the endpoint its no-port
// spelling names — `https://api.z.ai:443/api/anthropic` is a respelling of
// ZAIDefaultBaseURL, not another destination. A string that does not parse
// as an absolute URL keeps the plain comparison.
func sameEndpoint(a, b string) bool {
	return normEndpoint(a) == normEndpoint(b)
}

// normEndpoint folds the respellings sameEndpoint ignores into one spelling:
// whitespace, case and a trailing slash first, then — for a parsable
// absolute URL — the explicit default port dropped from the host.
func normEndpoint(raw string) string {
	norm := strings.TrimRight(strings.ToLower(strings.TrimSpace(raw)), "/")
	parsed, err := url.Parse(norm)
	if err != nil || parsed.Host == "" {
		return norm // no absolute URL: the plain respellings are all there is
	}
	if port := parsed.Port(); port != "" && port == schemeDefaultPort(parsed.Scheme) {
		parsed.Host = parsed.Hostname() // the port the scheme implies is no routing fact
	}
	return strings.TrimRight(parsed.String(), "/")
}

// schemeDefaultPort is the port a URL scheme implies — the one an explicit
// spelling of it does not have to carry to name the same endpoint.
func schemeDefaultPort(scheme string) string {
	switch scheme {
	case "https":
		return "443"
	case "http":
		return "80"
	}
	return ""
}

// OperatorEndpointClaim reports whether an explicitly set base URL is the
// operator naming a destination of their own — the one override the facade
// substitution verdict honours. Unset (empty) is no claim, and neither is
// any of the vendor's OWN published endpoints (secrets.FacadeVendorEndpoints):
// the same Anthropic wire ships on two hosts per facade — Moonshot's .ai
// default and its .cn gateway, z.ai's default and Zhipu's open.bigmodel.cn —
// and every one of them wires the call to the vendor exactly as the absence
// of a choice would, so the substitution verdict covers the ensemble alike,
// however cosmetically respelled (an explicit default port included). A
// DIFFERENT endpoint stays the claim — a proxy may genuinely serve the id.
// A slot that names no facade holds no vendor endpoint, so any endpoint at
// all is the operator's.
//
// Exported beside FacadeServesModelID for the claw registry: on
// ZAI_API_KEY + ANTHROPIC_BASE_URL=<a z.ai-family endpoint> the delegate
// refuses a claude id and the registry built the client — GLM served under
// the claude label with the run record blessing it. One predicate here,
// judged by both backends, is what keeps "the refusal and the routing can
// never disagree" true in both directions.
func OperatorEndpointClaim(endpoint, slot string) bool {
	if strings.TrimSpace(endpoint) == "" {
		return false
	}
	for _, vendor := range secrets.FacadeVendorEndpoints(secrets.Provider(slot)) {
		if sameEndpoint(endpoint, vendor) {
			return false
		}
	}
	return true
}

// facadeSubstitutionRefusal refuses a route the resolved facade cannot serve
// — z.ai asked for a claude id, Moonshot asked for anything but a kimi id —
// or returns nil when the route serves what it names.
//
// It reads the RESOLVED env (FacadeSlotEnvKey, ANTHROPIC_BASE_URL), never
// re-runs the selection: the code that knows the route stamps it, and a
// second derivation would be free to disagree with the one that routes
// (facadeHintRefusal's contract, applied to the model half).
//
// Two scopes keep the refusal honest:
//
//   - Only a route the RESOLVER built as a vendor facade (the slot stamp)
//     is judged: an ambient endpoint the run merely inherited is the
//     operator's configuration, not a facade iterion chose.
//   - Only the vendor's OWN published endpoints — the same ensemble the
//     registry judges (OperatorEndpointClaim, secrets.FacadeVendorEndpoints):
//     an explicit base-URL override (ANTHROPIC_BASE_URL / MOONSHOT_BASE_URL)
//     naming any OTHER destination is the operator's claim — a proxy may
//     genuinely serve the id, and an explicit routing choice is honoured,
//     never silently second-guessed.
//
// An empty task.Model reads as the backend's own default
// (defaultClaudeCodeModel): the CLI would substitute exactly the same way.
func facadeSubstitutionRefusal(task Task, credEnv map[string]string) error {
	slot := credEnv[FacadeSlotEnvKey]
	if facadeServedModelPrefix(slot) == "" {
		return nil // not a resolver-built vendor facade route
	}
	if OperatorEndpointClaim(credEnv["ANTHROPIC_BASE_URL"], slot) {
		return nil // the operator's own gateway is their claim
	}
	wireID := claudeCodeModelID(task.Model)
	if wireID == "" {
		wireID = defaultClaudeCodeModel
	}
	if FacadeServesModelID(slot, wireID) {
		return nil
	}
	return &ErrModelUnavailable{
		Provider: slot,
		Model:    wireID,
		Detail: "the " + slot + " facade does not serve it — it would answer with its own model under the requested label; " +
			"route the node to a model the facade serves, or declare a `fallbacks:` route that does",
	}
}
