package runview

import (
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/llmroute"
	"github.com/SocialGouv/iterion/pkg/store"
)

// refuseOutsideAllowlist is the tenant's providers lock on the launch lane
// (#2665 item 7): the same launch view, the same walk and the same two
// refusals the cloud publisher runs before it seals (model.AllowlistRefusal),
// so the lock holds whether the deployment publishes to a queue or runs in
// process. A CLI launch resolves no policy (ADR-129 §8) and passes untouched;
// so does a policy that named no list.
func refuseOutsideAllowlist(wf *ir.Workflow, spec *LaunchSpec) error {
	return refuseOutsideAllowlistOn(wf, spec, spec.Vars, spec.TrustedVarNames)
}

// refuseOutsideAllowlistOn is refuseOutsideAllowlist over the vars the run
// actually stores: the in-process launch passes its merged inputs (a
// preset's values under the launch's own) and the trusted names that go
// with them, so the launch walks the same vars the resume gate later reads
// from the run document.
func refuseOutsideAllowlistOn[V any](wf *ir.Workflow, spec *LaunchSpec, vars map[string]V, trusted []string) error {
	// The run-level chain on the model's twin shape — field-identical to the
	// launch entry, a tiny adapter per site (override_fold.go's doctrine).
	chain := make([]model.FallbackEntry, 0, len(spec.Fallback))
	for _, e := range spec.Fallback {
		chain = append(chain, model.FallbackEntry{Backend: e.Backend, Model: e.Model, Provider: e.Provider})
	}
	view := model.RouteViewForLaunch(wf, toModelOverrides(spec.ModelOverrides), chain, vars, trusted)
	return allowlistRefusalOn(view, spec.LLMRoutePolicy)
}

// refuseResumeOutsideAllowlist is the tenant's providers lock on the local
// resume lanes (in-process and detached spawn; the cloud resume's walk is
// SubmitResume's): the dispatch view over the run's own frozen overrides,
// fallback chain and inputs — the reading the runner applies to those same
// values — walked under the list the run document froze at launch. A resume
// certifies nothing new (ADR-129 §7's parity): it re-reads what the launch
// admitted, so a run its launch admitted passes; a source edited between
// park and resume, or a run some older lane admitted unwalked, refuses
// before anything moves the run. A run the detached lane launched froze no
// list (that lane resolves no policy, ADR-129 §8), so its resume holds no
// lock either. Nil-safe on both arguments.
func refuseResumeOutsideAllowlist(wf *ir.Workflow, r *store.Run) error {
	if wf == nil || r == nil {
		return nil
	}
	chain := make([]model.FallbackEntry, 0, len(r.Fallback))
	for _, e := range r.Fallback {
		chain = append(chain, model.FallbackEntry{Backend: e.Backend, Model: e.Model, Provider: e.Provider})
	}
	view := model.RouteViewForDispatch(wf, ModelOverridesFromRun(r.ModelOverrides), chain, r.Inputs)
	return allowlistRefusalOn(view, r.LLMRoutePolicy)
}

// allowlistRefusalOn is the shared tail of both lanes' gate: the walk over
// the view under the policy's frozen list, read as the two refusals
// model.AllowlistRefusal raises — a route the list cannot check first, then
// a primary route no allowed provider serves. nil when no workflow walks or
// no list is in force.
func allowlistRefusalOn(view model.RouteView, pol *store.RunLLMRoutePolicy) error {
	allowed := pol.ProviderAllowlist()
	if view.Workflow == nil || len(allowed) == 0 {
		return nil
	}
	view.AllowedProviders = allowed
	return model.AllowlistRefusal(model.EffectiveProviders(view, model.KnownProviderSet()), pol.Sources[llmroute.FieldProviders])
}
