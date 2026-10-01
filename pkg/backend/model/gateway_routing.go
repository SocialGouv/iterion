package model

import (
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
func checkGatewayEnv(strict bool) error {
	cfg, err := compatgw.FromEnv(os.Getenv)
	if err != nil {
		return err
	}
	return cfg.Validate(strict)
}
