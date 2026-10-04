package server

import (
	"context"
	"fmt"

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
// The levels: the launch's binding layer (the provisioning record that
// owns this launch — schedule row, trigger subscription, webhook config),
// the shipped bot's `routing:` manifest block, the platform level (the
// settings record's routing block, then its env dials as the deployment
// default), then the built-in defaults, with the platform's triggers as
// the ceiling (a proven no-op while the platform level is the only level
// BELOW the ceiling's owner — wired so the run level of slice 3 slots in
// without retrofitting the ordering). The fold's lock semantics carry the
// ADR's governance: the author's lock stops the binding, the platform's
// lock binds every tenant-configurable level.

// resolveRunLLMRoutePolicy resolves the effective policy for a launch and
// returns the snapshot to persist on the run doc. An ERROR means a layer
// carries a block the fold cannot read — the launch must be REFUSED, never
// folded around (a nil first return with a nil error happens only for an
// unwired resolver in tests).
//
// `higher` carries the binding layers the CALLER knows about, highest
// priority first — typically the provisioning record's routing block. The
// bot manifest, the platform record, the env dials and the built-in
// defaults are appended here so no launch site has to remember them (the
// reason resolveRunRetryPolicy has the same shape).
func (s *Server) resolveRunLLMRoutePolicy(ctx context.Context, teamID, botID string, higher ...llmroute.Layer) (*store.RunLLMRoutePolicy, error) {
	layers := make([]llmroute.Layer, 0, len(higher)+3)
	layers = append(layers, higher...)
	if m := s.botManifestFor(ctx, teamID, botID); m != nil {
		layers = append(layers, llmroute.Layer{Source: llmroute.SourceBot, Policy: m.RoutingPolicy()})
	}
	var platform llmroute.Policy
	if s.platformCreds != nil {
		if rec := s.platformCreds.Get(ctx); rec != nil && rec.Routing != nil {
			platform = *rec.Routing
		}
	}
	layers = append(layers, llmroute.Layer{Source: llmroute.SourcePlatform, Policy: platform})
	layers = append(layers, llmroute.Layer{Source: llmroute.SourceEnv, Policy: llmroute.FromEnv()})

	// A layer that carries a block the fold cannot read refuses the LAUNCH,
	// naming the level and the field. Records can carry hand-edited or
	// rolling-deploy values no write path ever validated (the subscription
	// and webhook records have no validated write surface today); silently
	// folding garbage would route the run somewhere nobody wrote.
	for _, l := range layers {
		if err := llmroute.Validate(l.Policy); err != nil {
			return nil, fmt.Errorf("llmroute: refusing launch: %s routing policy: %w", l.Source, err)
		}
	}

	pol, sources := llmroute.Resolve(layers...)
	// The platform voice — the record's value, else its env default — is
	// the ceiling on triggers: the levels below may only narrow what the
	// platform allows them to fire. While no level below the platform
	// sets triggers yet, this is a proven no-op; it is wired so the run
	// level of slice 3 inherits the ordering.
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
	}, nil
}
