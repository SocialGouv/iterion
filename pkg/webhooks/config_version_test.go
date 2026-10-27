package webhooks

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/llmroute"
	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestConfigVersionRejectsStaleSnapshots(t *testing.T) {
	onConfigStoreTwins(t, func(t *testing.T, s ConfigStore) {
		ctx := context.Background()
		mark := time.Unix(1700000000, 0).UTC()
		cfg := Config{ID: "cas", TenantID: "tenant", TokenHash: "hash", Name: "original", BotIDs: []string{"trusted"}, LastUsedAt: &mark,
			SecretOverrides: map[string]string{"forge_token": "trusted"},
			BotRules:        []BotRule{{BotID: "trusted", LaunchVars: map[string]string{"mode": "trusted"}}},
			CommandMap:      map[string][]CommandRoute{"revi": {{BotID: "trusted", ContextVars: map[string]string{"key": "trusted"}}}},
			Routing:         &llmroute.Policy{ModelClasses: map[string]map[string]string{"top": {"family": "trusted"}}},
		}
		if err := s.Create(ctx, cfg); err != nil {
			t.Fatal(err)
		}
		// Exercise old Mongo documents that predate the version field.
		if ms, ok := s.(*MongoConfigStore); ok {
			if _, err := ms.kit.Coll().UpdateOne(ctx, bson.M{"_id": cfg.ID}, bson.M{"$unset": bson.M{"version": ""}}); err != nil {
				t.Fatal(err)
			}
		}
		cfg.SecretOverrides["forge_token"] = "input-alias"
		stale, err := s.Get(ctx, cfg.ID)
		if err != nil {
			t.Fatal(err)
		}
		if stale.SecretOverrides["forge_token"] != "trusted" {
			t.Fatal("Create retained caller-owned map")
		}
		winner, _ := s.Get(ctx, cfg.ID)
		winner.Name = "winner"
		used := mark.Add(time.Hour)
		if err := s.MarkUsed(ctx, cfg.ID, used); err != nil {
			t.Fatal(err)
		}
		if err := s.Update(ctx, winner); err != nil {
			t.Fatal(err)
		}
		winner.BotIDs[0] = "write-alias"
		stale.BotIDs[0] = "stale"
		stale.SecretOverrides["forge_token"] = "stale"
		stale.BotRules[0].LaunchVars["mode"] = "stale"
		stale.CommandMap["revi"][0].ContextVars["key"] = "stale"
		stale.Routing.ModelClasses["top"]["family"] = "stale"
		*stale.LastUsedAt = mark.Add(24 * time.Hour)
		if err := s.Update(ctx, stale); !errors.Is(err, ErrConflict) {
			t.Fatalf("stale snapshot accepted: %v", err)
		}
		got, _ := s.Get(ctx, cfg.ID)
		if got.Version != 1 || got.Name != "winner" || got.BotIDs[0] != "trusted" || got.SecretOverrides["forge_token"] != "trusted" || got.BotRules[0].LaunchVars["mode"] != "trusted" || got.CommandMap["revi"][0].ContextVars["key"] != "trusted" || got.Routing.ModelClasses["top"]["family"] != "trusted" {
			t.Fatal("rejected stale snapshot mutated durable nested settings")
		}
		if got.LastUsedAt == nil || !got.LastUsedAt.Equal(used) {
			t.Fatal("config replacement lost independent usage stamp")
		}
		rows, _ := s.ListByTenant(ctx, "tenant")
		rows[0].BotIDs[0] = "list-alias"
		again, _ := s.Get(ctx, cfg.ID)
		if again.BotIDs[0] != "trusted" {
			t.Fatal("list retained store-owned slice")
		}
	})
}

func TestConfigMigrationFreezeTwins(t *testing.T) {
	onConfigStoreTwins(t, func(t *testing.T, s ConfigStore) {
		ctx := context.Background()
		c := Config{ID: "frozen", TenantID: "tenant", TokenHash: "frozen-unique-hash", ProvisionIntegrationID: "integration", Enabled: true}
		if err := s.Create(ctx, c); err != nil {
			t.Fatal(err)
		}
		c.ProvisionFrozen = true
		if err := s.Update(ctx, c); err != nil {
			t.Fatal(err)
		}
		c, err := s.Get(ctx, c.ID)
		if err != nil {
			t.Fatal(err)
		}
		c.LabelAllowlist = []string{"lost-restriction"}
		c.ProvisionFrozen = false
		if err = s.Update(ctx, c); !errors.Is(err, ErrConflict) {
			t.Fatalf("frozen policy accepted edit: %v", err)
		}
		if err = s.DisableProvision(ctx, c.ID, "another-integration"); !errors.Is(err, ErrConflict) {
			t.Fatalf("foreign authority disabled endpoint: %v", err)
		}
		if err = s.DisableProvision(ctx, c.ID, "integration"); err != nil {
			t.Fatal(err)
		}
		got, err := s.Get(ctx, c.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Enabled || !got.ProvisionFrozen || len(got.LabelAllowlist) != 0 {
			t.Fatalf("disable replaced frozen policy: %+v", got)
		}
	})
}
