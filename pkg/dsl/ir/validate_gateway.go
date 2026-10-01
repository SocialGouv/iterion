package ir

import (
	"fmt"
	"strings"

	"github.com/SocialGouv/iterion/pkg/backend/modelroute"
)

// The OpenAI-compatible gateway diagnostics. An `openai_compatible/<id>`
// model is a claw-only route: the executor refuses the crossing per chain
// element at build time (refuseGatewayCrossing,
// pkg/backend/model/gateway_routing.go). C184 is that rule's compile-time
// mirror for the routes whose text is decidable as written.
const (
	DiagGatewayCrossing DiagCode = "C186" // a literal `openai_compatible/…` model on a route whose backend or provider hint explicitly names a non-claw wire (warning)
)

// validateGatewayRoutes warns on a route the executor would refuse: a
// LITERAL `openai_compatible/…` model — the element's own text, a
// `{{vars.…}}` or `${VAR}` model expanding at dispatch and is nobody's
// route at compile time — beside an explicit non-claw backend, or beside a
// provider hint that names a vendor wire. It walks the same shapes the
// executor's chain does: an agent or judge node's primary route and each
// `fallbacks:` entry, and an `mode: llm` router. The runtime judgement is
// mirrored field by field (refuseGatewayCrossing): the hint arm first — a
// gateway model borrows no vendor's wire — then the backend arm, where "",
// `claw` and `auto` all resolve to claw and stay silent.
//
// It is a WARNING, not the runtime's refusal: a bot that compiles today
// must keep compiling, and the executor already fails the element loud at
// dispatch (the route walks the chain like any unresolvable one) — C184
// only moves the sentence from the first run to `iterion validate`.
//
// Silent by construction, because nothing there names a backend: a human
// node's `interaction: llm` companion route and a supervisor's model are
// both resolved with NO backend at run time (override_fold resolveDirect),
// and a tool node's `recovery.model` runs on a backendless synthetic node —
// a gateway model on any of them has nothing to conflict with. A route
// whose backend is a reference the source does not answer (`${X}` with no
// default, a `{{vars.…}}` the launch may override) stays silent too: the
// launch environment decides it.
func (c *compiler) validateGatewayRoutes(w *Workflow) {
	for _, n := range w.Nodes {
		switch nn := n.(type) {
		case LLMNode:
			f := nn.GetLLMFields()
			c.checkGatewayRoute(nn.NodeID(), nn.NodeKind().String(), "", f.Model,
				effectiveNodeBackend(f.Backend, w.DefaultBackend), f.Provider)
			for _, fb := range nn.GetFallbacks() {
				be, decided := routeGatewayBackend(fb.Backend, f.Backend, w.DefaultBackend)
				if !decided {
					continue
				}
				c.checkGatewayRoute(nn.NodeID(), nn.NodeKind().String(), fb.Name, fb.Model, be, fb.Provider)
			}
		case *RouterNode:
			if nn.RouterMode != RouterLLM {
				continue
			}
			c.checkGatewayRoute(nn.NodeID(), "router", "", nn.Model,
				effectiveNodeBackend(nn.Backend, w.DefaultBackend), nn.Provider)
		}
	}
}

// checkGatewayRoute emits C184 for one route element, in the order the
// runtime refuses it: a provider hint naming another wire first, then a
// backend that cannot serve a gateway route.
func (c *compiler) checkGatewayRoute(nodeID, kind, routeName, model, backend, provider string) {
	if !isLiteralGatewayModel(model) {
		return
	}
	where := fmt.Sprintf("%s %q", kind, nodeID)
	if routeName != "" {
		where += fmt.Sprintf(", fallback route %q", routeName)
	}
	if hint, conflict := gatewayHintConflict(provider); conflict {
		c.warnfAt(DiagGatewayCrossing, nodeID, "",
			"%s: model %q is served by an OpenAI-compatible gateway, but an element of provider %q names another wire — a gateway model borrows no vendor's wire; the refusing element walks the chain like any unresolvable route (drop that element, or route the node to claw, which serves gateway routes)",
			where, model, hint)
		return
	}
	if conflict := gatewayBackendConflict(backend); conflict != "" {
		c.warnfAt(DiagGatewayCrossing, nodeID, "",
			"%s: model %q is served by an OpenAI-compatible gateway, but %s — a backend that cannot serve one; gateway routes run on the claw backend (drop the explicit backend, or route the node to claw)",
			where, model, conflict)
	}
}

// isLiteralGatewayModel reports whether model, as written, names a
// gateway route. A value carrying a reference resolves at dispatch and is
// no route at compile time — the same skip every routing-field validator
// applies (`${…}` and `{{…}}`).
func isLiteralGatewayModel(model string) bool {
	if model == "" || strings.Contains(model, "${") || strings.Contains(model, "{{") {
		return false
	}
	return modelroute.Parse(model).Gateway()
}

// gatewayHintConflict mirrors the hint arm of refuseGatewayCrossing: a
// gateway model beside a provider hint that names a vendor wire is two
// wires on one element. The field is read as the chain reads it — split
// into elements (splitProviderChain), each possibly `hint:model`
// (SplitProviderStep) — and any element whose hint is not auto/empty and
// not the gateway prefix conflicts; the offending hint is returned for the
// message. A field carrying a reference is skipped wholesale, as
// validateProviders skips it: its text is not the resolved value.
func gatewayHintConflict(provider string) (string, bool) {
	if provider == "" || strings.Contains(provider, "${") || strings.Contains(provider, "{{") {
		return "", false
	}
	for _, tok := range splitProviderChain(provider) {
		hint, _, _ := SplitProviderStep(tok)
		switch strings.ToLower(strings.TrimSpace(hint)) {
		case "", "auto", modelroute.OpenAICompatible:
			continue
		}
		return hint, true
	}
	return "", false
}

// gatewayBackendConflict mirrors the backend arm of refuseGatewayCrossing:
// only "", `claw` and `auto` resolve to claw; any other NAMED backend
// cannot serve a gateway route. The return is the message's subject —
// "" when the backend stays silent (unnamed, claw-resolving, or a
// reference the source does not answer).
func gatewayBackendConflict(backend string) string {
	switch strings.ToLower(strings.TrimSpace(backend)) {
	case "", "claw", "auto":
		return ""
	}
	return fmt.Sprintf("backend is %q", backend)
}

// routeGatewayBackend is a fallback route's serving backend, read the way
// the chain reads it (executor_retry: the route's own backend when it
// names one, the node's route otherwise). A route backend that is a
// reference the source does not answer is undecided — the launch
// environment may name anything, so nothing is certain enough to warn on.
func routeGatewayBackend(routeBackend, nodeBackend, workflowDefault string) (backend string, decided bool) {
	if strings.TrimSpace(routeBackend) == "" {
		return effectiveNodeBackend(nodeBackend, workflowDefault), true
	}
	return sourceBackend.resolve(routeBackend)
}
