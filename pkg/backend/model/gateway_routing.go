package model

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/SocialGouv/iterion/pkg/secrets"
	httpdial "github.com/SocialGouv/iterion/pkg/secure/httpdial"
	"net"
	"net/url"
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
	switch strings.ToLower(strings.TrimSpace(backend)) {
	case "", delegate.BackendClaw, "auto":
		return nil
	}
	return fmt.Errorf("backend %q cannot serve an OpenAI-compatible gateway route — gateway models run on the claw backend", backend)
}

// checkGatewayEnv validates the environment a gateway-served element needs,
// before the element is dispatched anywhere: the same read the claw factory
// and the sandbox forward will make, refused here — with the variable
// named, the value never printed — while the chain can still walk to a
// fallback.
// checkGatewaySource validates that A source exists for a gateway-served
// element before it is dispatched anywhere: a run holding its own gateway
// pair (the sealed credential the registry's uncached arm serves) is
// validated at the same dispatch-time standard the env pair gets — syntax
// here, and the public-address rule under strict, resolved fresh per
// attempt — while a run holding no pair reads the pod environment exactly
// as before. Under the ambient stamp with neither source, the refusal
// names the stamp: the run's LLM routes are funded by its sealed
// credentials only (#2005, ADR-131 §7).
func checkGatewaySource(ctx context.Context, spec string, strict bool) error {
	if creds, ok := secrets.CredentialsFromContext(ctx); ok {
		key, baseURL, err := creds.GatewayCredentialForRoute(secrets.ProviderOpenAICompatible)
		if err != nil {
			// A key held without its URL is the typed pairing refusal —
			// the registry answers the same state with the same error;
			// never the pod's env gateway.
			return fmt.Errorf("openai_compatible: %w", err)
		}
		if key != "" && baseURL != "" {
			// Syntax at the seam; the address class for a LITERAL host —
			// a private literal was refused at the seal, but the ctx
			// credentials may be hand-built. A NAME resolves at dial time
			// (PR-C'): the resolver's answer is the dial guard's to judge.
			if err := (compatgw.Config{BaseURL: baseURL}).Validate(false); err != nil {
				return err
			}
			if strict {
				if u, uerr := url.Parse(baseURL); uerr == nil {
					if ip := net.ParseIP(u.Hostname()); ip != nil && !httpdial.IsPublicUnicast(ip) {
						return compatgw.RemedyPrivate(fmt.Errorf("the run-held gateway endpoint is a private or non-public address"))
					}
				}
			}
			return nil
		}
	}
	if ambientRefused(ctx) {
		return &ErrAmbientCredentialRefused{Provider: string(secrets.ProviderOpenAICompatible), Spec: spec}
	}
	return checkGatewayEnv(strict)
}

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

// stampGatewaySpec records the invocation's catalog provenance on the
// node output: which table answered and what it said (compact JSON — the
// output travels through maps that must stay single-line-safe). An unknown
// record is recorded AS unknown: the absence of a window or a price is a
// fact about the catalog, not an omission. Nil-safe.
func stampGatewaySpec(output map[string]any, resolved compatgw.Resolved) map[string]any {
	if output == nil {
		return output
	}
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
	if raw, err := json.Marshal(record); err == nil {
		output["_gateway_spec"] = json.RawMessage(raw)
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
