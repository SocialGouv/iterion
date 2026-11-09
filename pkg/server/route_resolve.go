package server

import (
	"context"
	"fmt"

	"github.com/SocialGouv/iterion/pkg/credpool"
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

	overrides := make([]store.RunLLMRouteOverride, 0, len(pol.Overrides))
	for _, o := range pol.Overrides {
		overrides = append(overrides, store.RunLLMRouteOverride{
			Selector: o.Selector,
			Backend:  o.Backend,
			Model:    o.Model,
			Provider: o.Provider,
			Effort:   o.Effort,
		})
	}
	return &store.RunLLMRoutePolicy{
		PairOrder:        pol.PairOrder,
		Triggers:         pol.Triggers,
		RefusedPinnedKey: pol.RefusedPinnedKey,
		Strict:           pol.Strict != nil && *pol.Strict,
		ModelClasses:     pol.ModelClasses,
		CrossHarness:     pol.CrossHarness,
		Overrides:        overrides,
		Providers:        pol.Providers,
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

// Launch-time resolution of a run's pool attachment (ADR-164). The
// routing-policy fold's sibling, for the same reasons: only the launch
// sites see every level at once, the run doc is the carrier every consumer
// already loads, and "why did this run draw from that pool" must be
// answerable from the snapshot alone.
//
// The levels, nearest first: the launch's binding layers (the provisioning
// records' pool: blocks), then the TEAM, ORG and PLATFORM pool-attachment
// records. The nearest answer wins; an absent level inherits the next one;
// an Enabled=false answer is RESOLVED-DISABLED — the fold stops there and
// the run draws from no pool at all (attached or legacy audience pools:
// an explicit opt-out outranks the inheritance). A read failure REFUSES
// the launch (never folded around — a wedged store must not lift pool
// governance), as does a block the fold cannot read or a pool that has
// vanished or been switched off — each naming its level. The platform
// attachment's triggers are the ceiling: the levels below may only narrow
// what the platform lets them fire, exactly like the routing triggers.
//
// NIL snapshot = no attachment resolved anywhere: every run keeps today's
// pool behavior byte for byte (law 2 — zero pool without an opt-in).
func (s *Server) resolveRunPoolAttachment(ctx context.Context, teamID, botID string, higher ...credpool.BindingLayer) (*store.RunPoolAttachment, error) {
	if s.credPoolAttachments == nil || s.credPool == nil {
		return nil, nil
	}
	type poolAnswer struct {
		level    string
		scopeID  string
		poolID   string
		strategy credpool.Strategy
		limits   credpool.DrawLimits
		disabled bool
		readErr  error
	}
	var answer *poolAnswer

	// The binding levels come in nearest-first from the caller; the first
	// one answers, whatever it says (including disabled).
	for _, bl := range higher {
		answer = &poolAnswer{level: bl.Source, scopeID: "binding:" + bl.Source, poolID: bl.PoolID, strategy: bl.Strategy, disabled: !bl.Enabled}
		break
	}

	// The stored scopes, team then org then platform. A team-less launch
	// (the platform admin view's ("", "")) walks platform only, like the
	// routing fold. Absence inherits; a read failure refuses.
	readAt := func(scopeID, level string) *poolAnswer {
		rctx, cancel := context.WithTimeout(ctx, platformcfg.FetchTimeout)
		defer cancel()
		a, err := s.credPoolAttachments.Get(rctx, scopeID)
		if err != nil {
			if err == credpool.ErrNotFound {
				return nil
			}
			// A read failure refuses the launch — never folded around (a
			// wedged store must not lift pool governance). The refusal is
			// carried through the answer's error field below.
			return &poolAnswer{level: level, readErr: err}
		}
		return &poolAnswer{level: level, scopeID: scopeID, poolID: a.PoolID, strategy: a.Strategy, limits: a.Limits, disabled: !a.Enabled}
	}
	_ = botID
	// The BOT level (ADR-164): the bot-in-team attachment — nearest after
	// the binding layers, before the team level. A schedule with no pool
	// block inherits this answer; the routes store bot:<team>/<bot> docs
	// and the orgsweep purges them, so the fold MUST read them or the
	// level is writable-and-ignored (the round-2 review's R2948d2).
	if answer == nil && teamID != "" && botID != "" {
		answer = readAt(credpool.BotScopeID(teamID, botID), "bot")
	}
	if answer == nil && teamID != "" {
		answer = readAt(credpool.TeamScopeID(teamID), "team")
	}
	if answer == nil && teamID != "" {
		if s.authStore() == nil {
			return nil, fmt.Errorf("pool attachment: refusing launch: no identity store to resolve team %s's org level", teamID)
		}
		t, err := s.authStore().GetTeam(ctx, teamID)
		if err != nil {
			return nil, fmt.Errorf("pool attachment: refusing launch: org resolve for team %s: %w", teamID, err)
		}
		if t.OrgID != "" {
			answer = readAt(credpool.OrgScopeID(t.OrgID), "org")
		}
	}
	if answer == nil {
		answer = readAt(credpool.ScopePlatform, "platform")
	}
	if answer == nil {
		return nil, nil
	}
	if answer.readErr != nil {
		return nil, fmt.Errorf("pool attachment: refusing launch: %s level: %w", answer.level, answer.readErr)
	}

	// Resolved-disabled: the run draws from no pool at all. The empty
	// strategy freezes as an empty trigger list — an ANSWER, not an
	// absence.
	if answer.disabled {
		return &store.RunPoolAttachment{ScopeID: answer.scopeID, Strategy: store.RunPoolStrategy{Triggers: []string{}}}, nil
	}

	// The platform attachment carries the trigger ceiling even when a
	// nearer level won. Its own garbage refuses the launch too (fail-closed
	// at its level) — an unreadable platform ceiling must not silently
	// become "no ceiling".
	ceiling := llmroute.Triggers
	rctx, cancel := context.WithTimeout(ctx, platformcfg.FetchTimeout)
	defer cancel()
	if plat, err := s.credPoolAttachments.Get(rctx, credpool.ScopePlatform); err == nil && plat.Enabled {
		if err := validatePoolTriggers(plat.Strategy.Triggers); err != nil {
			return nil, fmt.Errorf("pool attachment: refusing launch: platform level: %w", err)
		}
		if len(plat.Strategy.Triggers) > 0 {
			ceiling = plat.Strategy.Triggers
		}
	} else if err != nil && err != credpool.ErrNotFound {
		return nil, fmt.Errorf("pool attachment: refusing launch: platform level: %w", err)
	}

	if err := answer.strategy.Validate(); err != nil {
		return nil, fmt.Errorf("pool attachment: refusing launch: %s level: %w", answer.level, err)
	}
	if err := validatePoolTriggers(answer.strategy.Triggers); err != nil {
		return nil, fmt.Errorf("pool attachment: refusing launch: %s level: %w", answer.level, err)
	}
	ok, err := s.credPool.HasPool(ctx, answer.poolID)
	if err != nil {
		return nil, fmt.Errorf("pool attachment: refusing launch: %s level: pool %s: %w", answer.level, answer.poolID, err)
	}
	if !ok {
		return nil, fmt.Errorf("pool attachment: refusing launch: %s level names pool %q, which is disabled or gone", answer.level, answer.poolID)
	}

	triggers := effectivePoolTriggers(answer.strategy.Triggers, ceiling)
	return &store.RunPoolAttachment{
		PoolID:  answer.poolID,
		ScopeID: answer.scopeID,
		Strategy: store.RunPoolStrategy{
			Mode:      string(answer.strategy.Mode),
			MarginPct: answer.strategy.MarginPct,
			Triggers:  triggers,
			ResetHold: answer.strategy.ResetHold,
		},
		Limits: store.RunPoolDrawLimits{MaxRunsPerDay: answer.limits.MaxRunsPerDay, MaxUSDPerDay: answer.limits.MaxUSDPerDay},
	}, nil
}

// validatePoolTriggers checks a trigger list against the llmroute closed
// vocabulary — the VALUES the routing policy shares; the platform ceiling
// narrows, never widens, and an unknown name refuses rather than folding
// around (the routing fold's garbage rule).
func validatePoolTriggers(triggers []string) error {
	for _, t := range triggers {
		known := false
		for _, v := range llmroute.Triggers {
			if v == t {
				known = true
				break
			}
		}
		if !known {
			return fmt.Errorf("unknown trigger %q (the vocabulary is %v)", t, llmroute.Triggers)
		}
	}
	return nil
}

// effectivePoolTriggers resolves the frozen trigger list: the winner's
// list (empty = the default [usage_window] the pool exists for) narrowed
// to the platform ceiling — the levels below may only narrow what the
// platform allows them to fire.
func effectivePoolTriggers(winner, ceiling []string) []string {
	base := winner
	if len(base) == 0 {
		base = []string{"usage_window"}
	}
	// An empty ceiling is the FULL vocabulary: no platform attachment, no
	// constraint. A populated one narrows.
	out := make([]string, 0, len(base))
	for _, t := range base {
		if len(ceiling) == 0 {
			out = append(out, t)
			continue
		}
		for _, c := range ceiling {
			if c == t {
				out = append(out, t)
				break
			}
		}
	}
	if out == nil {
		out = []string{}
	}
	return out
}
