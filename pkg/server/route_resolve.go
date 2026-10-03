package server

import (
	"context"

	"github.com/SocialGouv/iterion/pkg/llmroute"
	"github.com/SocialGouv/iterion/pkg/store"
)

// Launch-time resolution of a run's adaptive-routing policy (ADR-121,
// pkg/llmroute). The `resolveRunRetryPolicy` twin, for the same reasons:
// only the launch sites see every level at once; the run doc is the
// carrier every consumer already loads; and "why did this run route to X"
// must be answerable from the snapshot alone, without replaying the
// launch.
//
// In slice 1 the only level is the platform level: the settings record's
// routing block, then its env dials as the deployment default, then the
// built-in defaults, with the platform's triggers as the ceiling (a
// proven no-op while it is the only level — wired now so the bot/binding/
// run levels of slices 2–3 slot in without retrofitting the ordering).

// resolveRunLLMRoutePolicy resolves the effective policy for a launch and
// returns the snapshot to persist on the run doc.
//
// The layers run highest-priority FIRST, under the same contract as
// resolveRunRetryPolicy's `higher` parameter: slices 2–3 prepend the bot,
// binding and run layers ahead of the platform layer, so those slices
// extend the call instead of rewriting it.
func (s *Server) resolveRunLLMRoutePolicy(ctx context.Context, higher ...llmroute.Layer) *store.RunLLMRoutePolicy {
	layers := make([]llmroute.Layer, 0, len(higher)+2)
	layers = append(layers, higher...)
	var platform llmroute.Policy
	if s.platformCreds != nil {
		if rec := s.platformCreds.Get(ctx); rec != nil && rec.Routing != nil {
			platform = *rec.Routing
		}
	}
	layers = append(layers, llmroute.Layer{Source: llmroute.SourcePlatform, Policy: platform})
	layers = append(layers, llmroute.Layer{Source: llmroute.SourceEnv, Policy: llmroute.FromEnv()})

	pol, sources := llmroute.Resolve(layers...)
	// The platform voice — the record's value, else its env default — is
	// the ceiling on triggers: the levels below may only narrow what the
	// platform allows them to fire. While the platform level is the only
	// level, this is a proven no-op (the fold already answered with the
	// platform's list); it is wired now so slices 2–3 inherit the
	// ordering.
	ceiling := llmroute.Ceiling{Triggers: platform.Triggers}
	if len(platform.Triggers) == 0 {
		ceiling.Triggers = llmroute.FromEnv().Triggers
	}
	pol = llmroute.Clamp(pol, ceiling, sources)

	return &store.RunLLMRoutePolicy{
		PairOrder:        pol.PairOrder,
		Triggers:         pol.Triggers,
		RefusedPinnedKey: pol.RefusedPinnedKey,
		Strict:           pol.Strict != nil && *pol.Strict,
		Sources:          sources,
	}
}
