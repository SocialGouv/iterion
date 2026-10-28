package runview

import (
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/llmroute"
)

// refuseOutsideAllowlist is the tenant's providers lock on the in-process
// lane (#2665 item 7): the same launch view, the same walk and the same two
// refusals the cloud publisher runs before it seals (model.AllowlistRefusal),
// so the lock holds whether the deployment publishes to a queue or runs in
// process. A CLI launch resolves no policy (ADR-129 §8) and passes untouched;
// so does a policy that named no list.
func refuseOutsideAllowlist(wf *ir.Workflow, spec *LaunchSpec) error {
	allowed := spec.LLMRoutePolicy.ProviderAllowlist()
	if wf == nil || len(allowed) == 0 {
		return nil
	}
	// The run-level chain on the model's twin shape — field-identical to the
	// launch entry, a tiny adapter per site (override_fold.go's doctrine).
	chain := make([]model.FallbackEntry, 0, len(spec.Fallback))
	for _, e := range spec.Fallback {
		chain = append(chain, model.FallbackEntry{Backend: e.Backend, Model: e.Model, Provider: e.Provider})
	}
	view := model.RouteViewForLaunch(wf, toModelOverrides(spec.ModelOverrides), chain, spec.Vars, spec.TrustedVarNames)
	view.AllowedProviders = allowed
	return model.AllowlistRefusal(model.EffectiveProviders(view, model.KnownProviderSet()), spec.LLMRoutePolicy.Sources[llmroute.FieldProviders])
}
