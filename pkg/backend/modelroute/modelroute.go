// Package modelroute is the one reading of a model spec: which provider
// routes it, which id goes on the wire, and which id names its capabilities.
//
// It is a leaf — it imports nothing from iterion — so the model package, the
// cost estimator, delegate backends and the runner's meters read a spec the
// same way instead of each stripping prefixes on their own.
package modelroute

import "strings"

// OpenAICompatible is the routing prefix of a model served by an
// OpenAI-compatible gateway ("openai_compatible/<gateway model id>").
const OpenAICompatible = "openai_compatible"

// routingProviders are the provider prefixes the claw model registry routes
// on. A spec whose prefix is one of them names a route; any other slash is
// part of a model id (`meta-llama/Llama-3.3-70B`, `scaleway/gpt-oss-120b`).
// A test in the model package pins this set to the registry's factories.
var routingProviders = map[string]bool{
	"anthropic": true,
	"openai":    true,
	"bedrock":   true,
	"vertex":    true,
	"foundry":   true,
	"xai":       true,
	"moonshot":  true,
}

// Route is a model spec, read once.
type Route struct {
	// Spec is the spec as written: "<provider>/<model>" or a bare id.
	Spec string
	// Provider is the routing prefix — everything before the first "/" —
	// or "" for a bare id.
	Provider string
	// Wire is the id a request carries: everything after the first "/",
	// verbatim (a model id may hold slashes of its own), or the bare id.
	Wire string
}

// Parse reads a spec. It never fails: a spec with no provider before a "/"
// (or nothing after it) is a bare id, sent as written.
func Parse(spec string) Route {
	if i := strings.Index(spec, "/"); i > 0 && i < len(spec)-1 {
		return Route{Spec: spec, Provider: spec[:i], Wire: spec[i+1:]}
	}
	return Route{Spec: spec, Wire: spec}
}

// Gateway reports whether an OpenAI-compatible gateway serves the route.
func (r Route) Gateway() bool { return r.Provider == OpenAICompatible }

// CapabilityID is the id capability tables are keyed on: the wire id of a
// vendor route — claw's model registry, the effort matrices, the
// adaptive-thinking profiles all know vendor ids — and "" for a gateway
// route. A gateway id is whatever the gateway serves: matching it against a
// vendor's tables, by name or by prefix, would hand an alias another model's
// context window or request shape.
func (r Route) CapabilityID() string {
	if r.Gateway() {
		return ""
	}
	return r.Wire
}

// IsRoutingProvider reports whether a provider prefix is one the model
// registry routes on, the gateway included.
func IsRoutingProvider(provider string) bool {
	return routingProviders[provider] || provider == OpenAICompatible
}

// RoutingProviders lists the vendor prefixes the model registry routes on,
// the gateway excluded.
func RoutingProviders() []string {
	out := make([]string, 0, len(routingProviders))
	for p := range routingProviders {
		out = append(out, p)
	}
	return out
}
