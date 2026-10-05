package server

import (
	"context"
	"fmt"

	"github.com/SocialGouv/iterion/pkg/llmroute"
	"github.com/SocialGouv/iterion/pkg/platformcfg"
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
// the shipped bot's `routing:` manifest block, the TEAM and ORG levels
// (their own policy records — delivery 2), the platform level (the
// settings record's routing block, then its env dials as the deployment
// default), then the built-in defaults, with the platform's triggers as
// the ceiling: the levels below the platform may only narrow what the
// platform allows them to fire. The fold's lock semantics carry the
// ADR's governance: a lock vetoes every MORE SPECIFIC level's setter
// (run, binding, bot above team; team above org); levels below a lock
// still answer — only the platform's lock binds every tenant-
// configurable level, because nothing answers below it.

// resolveRunLLMRoutePolicy resolves the effective policy for a launch and
// returns the snapshot to persist on the run doc. An ERROR means a layer
// carries a block the fold cannot read — the launch must be REFUSED, never
// folded around (a nil first return with a nil error happens only for an
// unwired resolver in tests).
//
// `higher` carries the binding layers the CALLER knows about, highest
// priority first — typically the provisioning record's routing block. The
// bot manifest, the tenant records, the platform record, the env dials
// and the built-in defaults are appended here so no launch site has to
// remember them (the reason resolveRunRetryPolicy has the same shape).
func (s *Server) resolveRunLLMRoutePolicy(ctx context.Context, teamID, botID string, higher ...llmroute.Layer) (*store.RunLLMRoutePolicy, error) {
	layers := make([]llmroute.Layer, 0, len(higher)+5)
	layers = append(layers, higher...)
	if m := s.botManifestFor(ctx, teamID, botID); m != nil {
		layers = append(layers, llmroute.Layer{Source: llmroute.SourceBot, Policy: m.RoutingPolicy()})
	}
	// The tenant levels (ADR-121 delivery 2): a team's and its org's own
	// routing-policy records, between the bot and the platform level.
	// BOTH steps sit under the teamID != "" guard — the platform admin
	// view calls ("", "") and must keep answering platform→env→default
	// without touching identity. A read failure REFUSES the launch (never
	// folded around — a read failure must never lift cost governance); an
	// absent record or an org-less team is an absent level, not a failure.
	if teamID != "" {
		layer, err := s.tenantRouteLayer(ctx, platformcfg.TeamRoutingPolicyID(teamID), llmroute.SourceTeam)
		if err != nil {
			return nil, fmt.Errorf("llmroute: refusing launch: team routing policy: %w", err)
		}
		if layer != nil {
			layers = append(layers, *layer)
		}
		if s.authStore() == nil {
			return nil, fmt.Errorf("llmroute: refusing launch: no identity store to resolve team %s's org level", teamID)
		}
		t, err := s.authStore().GetTeam(ctx, teamID)
		if err != nil {
			return nil, fmt.Errorf("llmroute: refusing launch: org resolve for team %s: %w", teamID, err)
		}
		if t.OrgID != "" {
			layer, err := s.tenantRouteLayer(ctx, platformcfg.OrgRoutingPolicyID(t.OrgID), llmroute.SourceOrg)
			if err != nil {
				return nil, fmt.Errorf("llmroute: refusing launch: org routing policy: %w", err)
			}
			if layer != nil {
				layers = append(layers, *layer)
			}
		}
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
	// platform allows them to fire. Live semantic since the tenant levels
	// landed (they sit below the platform and can set triggers); wired
	// from delivery 1's slice 1 so no level retrofitted the ordering.
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

// tenantRouteLayer reads one tenant routing-policy record with the
// platform layer's fetch-timeout bound — an unbounded point read would
// turn the fail-closed posture into fail-stuck (a wedged store must
// surface the error the fold refuses on, not hang launches). A record
// with no policy (or no record) is a nil layer: absence is not failure.
// No resolver, no TTL: serve-stale on outage would lift cost governance
// on a blip — the platform record earns its 30s resolver, the tenant
// levels do not.
func (s *Server) tenantRouteLayer(ctx context.Context, docID, source string) (*llmroute.Layer, error) {
	if s.routingPolicyStoreFor == nil {
		return nil, nil
	}
	st := s.routingPolicyStoreFor(docID)
	if st == nil {
		return nil, nil
	}
	rctx, cancel := context.WithTimeout(ctx, platformcfg.FetchTimeout)
	defer cancel()
	rec, err := st.Get(rctx)
	if err != nil {
		return nil, err
	}
	if rec == nil || rec.Policy == nil {
		return nil, nil
	}
	return &llmroute.Layer{Source: source, Policy: *rec.Policy}, nil
}
