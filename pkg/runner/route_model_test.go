package runner

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/cloud/metrics"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// routeModel keys a step on the node's route. A report that is itself a
// routing spec names its own route; any other report is an id as the backend
// called it — which may hold slashes of its own — and names the declared
// route when it is the declared model: exactly for an opaque gateway id, up
// to its snapshot alias for a vendor id.
func TestRouteModel(t *testing.T) {
	cases := []struct {
		name, declared, reported, want string
	}{
		{"a model id's own slash is not a route", "openrouter/meta-llama/x", "meta-llama/x", "openrouter/meta-llama/x"},
		{"a routing spec names its own route", "anthropic/claude-opus-5", "openai/claude-opus-5", "openai/claude-opus-5"},
		{"a credential provider is a route too", "anthropic/glm-5.3", "zai/glm-5.3", "zai/glm-5.3"},
		{"vendor snapshot alias", "anthropic/claude-opus-5", "claude-opus-5-20260101", "anthropic/claude-opus-5"},
		{"gateway route reported by its wire id", "openai_compatible/team-a/m", "team-a/m", "openai_compatible/team-a/m"},
		{"another gateway model sharing the last segment", "openai_compatible/team-a/m", "team-b/m", "team-b/m"},
		{"a bare fallback model", "anthropic/claude-opus-5", "gpt-5.5", "gpt-5.5"},
		{"no report", "anthropic/claude-opus-5", "", "anthropic/claude-opus-5"},
		{"no declaration", "", "meta-llama/x", "meta-llama/x"},
		{"a wire id whose first segment names a provider", "openrouter/anthropic/claude-sonnet-4.5", "anthropic/claude-sonnet-4.5", "openrouter/anthropic/claude-sonnet-4.5"},
		{"an older runner's wire id, nested", "openai/zai/glm-4.6", "zai/glm-4.6", "openai/zai/glm-4.6"},
		{"a dated report of a model id with its own slash", "openrouter/meta-llama/llama-3.3-70b-instruct", "meta-llama/llama-3.3-70b-instruct-20250101", "openrouter/meta-llama/llama-3.3-70b-instruct"},
		{"a registry provider names its own route, credential or not", "anthropic/claude-opus-5", "foundry/claude-opus-5", "foundry/claude-opus-5"},
		{"the gateway names its own route", "anthropic/claude-opus-5", "openai_compatible/claude-opus-5", "openai_compatible/claude-opus-5"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := routeModel(tc.declared, tc.reported); got != tc.want {
				t.Errorf("routeModel(%q, %q) = %q, want %q", tc.declared, tc.reported, got, tc.want)
			}
		})
	}
}

// A claw llm_request that names its wire id beside its model says the model
// is the route it was resolved from: a fallback element on
// "anthropic/claude-x" under a declared "openai/anthropic/claude-x" is keyed
// on the Anthropic route, not folded back into the declared one because its
// spec equals the declared wire id. Red when the emitter ignores wire_model.
func TestMetricsEmitter_ARequestNamingItsWireIDIsItsOwnRoute(t *testing.T) {
	usage := newMetricsEmitter(&recordingEmitter{}, metrics.New())
	usage.observe(store.Event{Type: store.EventDelegateStarted, NodeID: "n",
		Data: map[string]any{"backend": "claw", "declared_model": "openai/anthropic/claude-x"}})
	usage.observe(store.Event{Type: store.EventLLMRequest, NodeID: "n",
		Data: map[string]any{"model": "anthropic/claude-x", "wire_model": "claude-x"}})
	usage.observe(store.Event{Type: store.EventLLMStepFinished, NodeID: "n",
		Data: map[string]any{"input_tokens": float64(1000), "output_tokens": float64(100)}})
	if _, ok := usage.RouteTotals()[routeKey{backend: "claw", model: "anthropic/claude-x"}]; !ok {
		t.Fatalf("routes = %v, want (claw, anthropic/claude-x): the request named its own route", usage.RouteTotals())
	}
}

// A delegation is keyed on the route its executor names — the chain element
// that served — refined by the model the backend reports when that is
// another model.
func TestServedRoute(t *testing.T) {
	cases := []struct {
		name, route, effective, want string
	}{
		{"no report", "openrouter/anthropic/claude-sonnet-4.5", "", "openrouter/anthropic/claude-sonnet-4.5"},
		{"OpenRouter's own id for the route's model", "openrouter/anthropic/claude-sonnet-4.5", "anthropic/claude-sonnet-4.5", "openrouter/anthropic/claude-sonnet-4.5"},
		{"a dated report of the route's model", "openrouter/anthropic/claude-sonnet-4.5", "anthropic/claude-sonnet-4.5-20250929", "openrouter/anthropic/claude-sonnet-4.5"},
		{"another model on the same provider", "anthropic/claude-opus-5", "claude-sonnet-4-6", "anthropic/claude-sonnet-4-6"},
		{"another model on a bare route", "claude-opus-5", "claude-sonnet-4-6", "claude-sonnet-4-6"},
		{"a gateway reporting its own spec", "openai_compatible/team-a/m", "openai_compatible/team-a/m", "openai_compatible/team-a/m"},
		{"a gateway reporting its wire id", "openai_compatible/team-a/m", "team-a/m", "openai_compatible/team-a/m"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := servedRoute(tc.route, tc.effective); got != tc.want {
				t.Errorf("servedRoute(%q, %q) = %q, want %q", tc.route, tc.effective, got, tc.want)
			}
		})
	}
}

// A pi node that fell back to OpenRouter reports OpenRouter's model id,
// "anthropic/claude-sonnet-4.5": the delegation is charged to the route the
// executor names, never to the Anthropic credential the id's first segment
// spells. Red when the meter ignores route_model.
func TestMetricsEmitter_ADelegationIsChargedToTheRouteThatServed(t *testing.T) {
	usage := newMetricsEmitter(&recordingEmitter{}, metrics.New())
	usage.observe(store.Event{Type: store.EventDelegateStarted, NodeID: "n",
		Data: map[string]any{"backend": "pi", "declared_model": "anthropic/claude-sonnet-4-5"}})
	usage.observe(store.Event{Type: store.EventDelegateFinished, NodeID: "n",
		Data: map[string]any{"backend": "pi", "tokens": float64(5000),
			"route_model": "openrouter/anthropic/claude-sonnet-4.5", "effective_model": "anthropic/claude-sonnet-4.5"}})
	key := routeKey{backend: "pi", model: "openrouter/anthropic/claude-sonnet-4.5"}
	if _, ok := usage.RouteTotals()[key]; !ok {
		t.Fatalf("routes = %v, want %v", usage.RouteTotals(), key)
	}
	creds := secrets.Credentials{APIKeys: map[secrets.Provider]string{
		secrets.ProviderAnthropic: "sk-ant", secrets.ProviderOpenRouter: "sk-or",
	}}
	if got := credentialSlotForRoute(creds, delegate.BackendPi, key.model); got != string(secrets.ProviderOpenRouter) {
		t.Errorf("charged to slot %q, want openrouter", got)
	}
}
