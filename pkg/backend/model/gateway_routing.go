package model

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/SocialGouv/iterion/pkg/backend/compatgw"
	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/modelroute"
)

// refuseGatewayCrossing is the parity rule for OpenAI-compatible gateway
// routes: a gateway model or a gateway provider hint is claw-only, from
// BOTH directions. It is judged on the element as RESOLVED — the chain's
// overrides and expansions have already spoken, so this sees the backend
// that would actually serve and the hint that would actually travel.
//
//   - a gateway model beside a vendor hint names two wires; the hint loses
//     (a gateway id borrows no vendor's wire) and the refusal says which;
//   - a gateway hint on a non-gateway model is a route to nowhere;
//   - a gateway model on any backend but claw (and the unset/auto one that
//     resolves to claw) is a backend that cannot speak to a gateway.
//
// Callers turn the refusal into a BUILD failure, so a refused element walks
// the chain like any other unresolvable one instead of killing the node
// after dispatch.
func refuseGatewayCrossing(backend, hint, model string) error {
	h := strings.ToLower(strings.TrimSpace(hint))
	hintGateway := h == modelroute.OpenAICompatible
	gateway := modelroute.Parse(model).Gateway()
	switch {
	case gateway && h != "" && h != "auto" && !hintGateway:
		return fmt.Errorf("model %q is served by an OpenAI-compatible gateway, but the route's provider hint %q names another wire — drop the hint, or route the node to the claw backend, which serves gateway routes", model, hint)
	case !gateway && hintGateway:
		return fmt.Errorf("provider hint %q serves gateway models, but model %q is not a gateway route — use %s/<gateway model id>", hint, model, modelroute.OpenAICompatible)
	}
	if !gateway && !hintGateway {
		return nil
	}
	if backendServesGatewayRoute(backend) {
		return nil
	}
	return fmt.Errorf("backend %q cannot serve an OpenAI-compatible gateway route — gateway models run on the claw backend", backend)
}

// backendServesGatewayRoute is the backend half of the parity rule above,
// on its own so the AUTO-SELECTION can consult it too (executor_resolve.go):
// the one answer to "can this backend speak to a gateway", shared so the
// selection and the refusal can never disagree about one backend.
func backendServesGatewayRoute(backend string) bool {
	switch strings.ToLower(strings.TrimSpace(backend)) {
	case "", delegate.BackendClaw, "auto":
		return true
	}
	return false
}

// checkGatewayEnv validates the environment a gateway-served element needs,
// before the element is dispatched anywhere: the same read the claw factory
// and the sandbox forward will make, refused here — with the variable
// named, the value never printed — while the chain can still walk to a
// fallback.
func checkGatewayEnv(strict bool) error {
	// The operator table is judged HERE (not only inside the catalog
	// resolution): a malformed table must REFUSE the route — naming the
	// variable — instead of silently degrading the gateway to unknown
	// while the run continues unpriced.
	if err := compatgw.CheckOperatorTable(os.Getenv); err != nil {
		return err
	}
	cfg, err := compatgw.FromEnv(os.Getenv)
	if err != nil {
		return err
	}
	return cfg.Validate(strict)
}

// gatewaySpecRecord renders the invocation's catalog provenance: which
// table answered and what it said (compact JSON — the output travels
// through maps that must stay single-line-safe). An unknown record is
// recorded AS unknown: the absence of a window or a price is a fact about
// the catalog, not an omission. Nil only when the record cannot marshal.
func gatewaySpecRecord(resolved compatgw.Resolved) json.RawMessage {
	record := struct {
		Source           string  `json:"source"`
		ContextWindow    int     `json:"context_window,omitempty"`
		MaxOutputTokens  int     `json:"max_output_tokens,omitempty"`
		InputUSDPerMTok  float64 `json:"input_usd_per_mtok,omitempty"`
		OutputUSDPerMTok float64 `json:"output_usd_per_mtok,omitempty"`
	}{
		Source:           resolved.Source,
		ContextWindow:    resolved.Spec.ContextWindow,
		MaxOutputTokens:  resolved.Spec.MaxOutputTokens,
		InputUSDPerMTok:  resolved.Spec.InputCostPerM,
		OutputUSDPerMTok: resolved.Spec.OutputCostPerM,
	}
	raw, err := json.Marshal(record)
	if err != nil {
		return nil
	}
	return raw
}

// stampGatewaySpec records the invocation's catalog provenance on the
// node output. The stamp on the map alone does not reach the operator:
// the output's reserved namespace is re-projected from the Result
// downstream (stampDelegateOutputMeta), so the caller also carries the
// record on Result.GatewaySpec — this stamp is what the backend's own
// return value shows. Nil-safe.
func stampGatewaySpec(output map[string]any, resolved compatgw.Resolved) map[string]any {
	if output == nil {
		return output
	}
	if raw := gatewaySpecRecord(resolved); len(raw) > 0 {
		output["_gateway_spec"] = raw
	}
	return output
}

// gatewayOutputBudget is a gateway route's max_tokens when the node sets
// none: the model's own max output from the catalog. claw's 8192 default is
// smaller than what a reasoning model behind a gateway can spend on hidden
// reasoning before it answers (#2651: final turn and recovery both out=8192
// of 8192, text empty, on a model the catalog serves 16384 for). It is
// raised only when the catalog also knows the window: the request is then
// fitted to it (GenerationOptions.MaxTokensWindow, buildRequest), so a large
// prompt never overflows the window by the budget's own size. 0 — claw's
// default — otherwise.
func gatewayOutputBudget(resolved compatgw.Resolved) int {
	if !resolved.Known() || resolved.Spec.MaxOutputTokens <= 0 || resolved.Spec.ContextWindow <= 0 {
		return 0
	}
	return resolved.Spec.MaxOutputTokens
}
