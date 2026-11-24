package forge

import (
	"context"
	"errors"
	"maps"
	"slices"
	"testing"
)

var errOverlapRefused = errors.New("rows overlap")

// F2: on a spine-routed integration, a provision whose projection is refused
// fails BEFORE it writes anything — the bot set (full path) and the
// bot_triggers (short-circuit path) stay as they were, and the rows in force
// stay the integration's own.
//
// Mutation: Provision skips its Subscriptions.Check → red on "the failed
// provision wrote".
func TestProvision_SpineRefusalWritesNothing(t *testing.T) {
	t.Run("full path: a bot added", func(t *testing.T) {
		s := newSpineOrch(t)
		res := s.provision(t, ProvisionRequest{BotIDs: []string{"nestor"}})
		s.flipToSpine(t, res)
		before, beforeCfg := s.stored(t, res)
		s.hour()
		s.projector.setRefuse(func(in SubscriptionInput) (SubscriptionReport, error) {
			if slices.Contains(in.Bots, "saboteur") {
				return SubscriptionReport{}, errOverlapRefused
			}
			return SubscriptionReport{}, nil
		})
		_, err := s.Provision(context.Background(), ProvisionRequest{TenantID: "t1", ConnectionID: "conn-1", RepoFullName: "group/api",
			BotIDs: []string{"nestor", "saboteur"}, ActorID: "u1"})
		if !errors.Is(err, ErrSubscriptionProjection) || !errors.Is(err, errOverlapRefused) {
			t.Fatalf("provision = %v, want the projection's refusal", err)
		}
		after, afterCfg := s.stored(t, res)
		if !slices.Equal(after.BotIDs, before.BotIDs) || !slices.Equal(afterCfg.BotIDs, beforeCfg.BotIDs) ||
			after.Version != before.Version || afterCfg.Version != beforeCfg.Version {
			t.Fatalf("the failed provision wrote: integration bots %v→%v (version %d→%d), config bots %v→%v (version %d→%d)",
				before.BotIDs, after.BotIDs, before.Version, after.Version, beforeCfg.BotIDs, afterCfg.BotIDs, beforeCfg.Version, afterCfg.Version)
		}
	})
	t.Run("short-circuit: bot_triggers changed", func(t *testing.T) {
		s := newSpineOrch(t)
		res := s.provision(t, ProvisionRequest{BotIDs: []string{"nestor"}})
		s.flipToSpine(t, res)
		before, _ := s.stored(t, res)
		s.hour()
		s.projector.setRefuse(func(in SubscriptionInput) (SubscriptionReport, error) {
			if tr, ok := in.Integration.BotTriggers["nestor"]; ok && tr.Labels != nil {
				return SubscriptionReport{Overlaps: []string{"nestor: a ∩ b"}}, errOverlapRefused
			}
			return SubscriptionReport{}, nil
		})
		_, err := s.Provision(context.Background(), ProvisionRequest{TenantID: "t1", ConnectionID: "conn-1", RepoFullName: "group/api",
			BotIDs: []string{"nestor"}, ActorID: "u1", BotTriggers: map[string]BotTrigger{"nestor": {Labels: []string{"ship"}}}})
		if !errors.Is(err, ErrSubscriptionProjection) {
			t.Fatalf("provision = %v, want the projection's refusal", err)
		}
		after, _ := s.stored(t, res)
		if after.BotTriggers["nestor"].Labels != nil || after.Version != before.Version {
			t.Fatalf("the failed provision wrote: bot_triggers %+v (version %d→%d), want none stored", after.BotTriggers, before.Version, after.Version)
		}
		// A later plain re-provision is not poisoned by a half-written state.
		s.projector.setRefuse(nil)
		if _, err := s.Provision(context.Background(), ProvisionRequest{TenantID: "t1", ConnectionID: "conn-1", RepoFullName: "group/api",
			BotIDs: []string{"nestor"}, ActorID: "u1"}); err != nil {
			t.Fatalf("a later plain re-provision: %v", err)
		}
	})
}

// F2: the check judges the input the provision then projects — the desired
// bots and bot_triggers on the integration, the rebuilt bot scope and
// launch vars on the config.
//
// Mutation: provisionProjectionInput leaves cfg.BotIDs as stored on the full
// path → red on "the check judged".
func TestProvision_SpineCheckJudgesTheProjectedInput(t *testing.T) {
	s := newSpineOrch(t)
	res := s.provision(t, ProvisionRequest{BotIDs: []string{"nestor"}})
	s.flipToSpine(t, res)
	s.hour()
	s.provision(t, ProvisionRequest{BotIDs: []string{"nestor", "saboteur"},
		BotTriggers: map[string]BotTrigger{"saboteur": {Labels: []string{"ship"}}}})
	s.projector.mu.Lock()
	defer s.projector.mu.Unlock()
	if len(s.projector.checks) == 0 || len(s.projector.syncs) == 0 {
		t.Fatalf("checks = %d syncs = %d, want the provision checked then projected", len(s.projector.checks), len(s.projector.syncs))
	}
	check, sync := s.projector.checks[len(s.projector.checks)-1], s.projector.syncs[len(s.projector.syncs)-1]
	if !slices.Equal(check.Bots, sync.Bots) || !slices.Equal(check.Integration.BotIDs, sync.Integration.BotIDs) ||
		!slices.Equal(check.Config.BotIDs, sync.Config.BotIDs) || check.Config.DefaultBotID != sync.Config.DefaultBotID ||
		check.Config.WildcardBots != sync.Config.WildcardBots || !maps.Equal(check.Config.LaunchVars, sync.Config.LaunchVars) ||
		!botTriggersEqual(check.Integration.BotTriggers, sync.Integration.BotTriggers) {
		t.Fatalf("the check judged bots %v (integration %v, config %v default %q, triggers %+v), the provision projected bots %v (integration %v, config %v default %q, triggers %+v)",
			check.Bots, check.Integration.BotIDs, check.Config.BotIDs, check.Config.DefaultBotID, check.Integration.BotTriggers,
			sync.Bots, sync.Integration.BotIDs, sync.Config.BotIDs, sync.Config.DefaultBotID, sync.Integration.BotTriggers)
	}
}

// R-F9, the orchestrator's half: another team's integration is not found
// before this server's wiring is judged.
//
// Mutation: SyncSubscriptions checks its projector first → red on "want
// ErrIntegrationNotFound".
func TestSyncSubscriptions_TenantBeforeWiring(t *testing.T) {
	s := newSpineOrch(t)
	res := s.provision(t, ProvisionRequest{BotIDs: []string{"nestor"}})
	s.Subscriptions = nil
	if _, err := s.SyncSubscriptions(context.Background(), "t2", res.IntegrationID); !errors.Is(err, ErrIntegrationNotFound) {
		t.Fatalf("another team's sync = %v, want ErrIntegrationNotFound", err)
	}
	if _, err := s.SyncSubscriptions(context.Background(), "t1", res.IntegrationID); !errors.Is(err, ErrSubscriptionProjection) {
		t.Fatalf("the owning team's sync with no projector = %v, want ErrSubscriptionProjection", err)
	}
}
