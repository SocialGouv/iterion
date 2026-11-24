package forge_test

import (
	"context"
	"errors"
	"github.com/SocialGouv/iterion/pkg/cloudsched"
	"github.com/SocialGouv/iterion/pkg/forge"
	"github.com/SocialGouv/iterion/pkg/llmroute"
	"github.com/SocialGouv/iterion/pkg/webhooks"
	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"os"
	"testing"
	"time"
)

func TestProvisionStoreTwins(t *testing.T) {
	t.Run("memory", func(t *testing.T) {
		provisionStoreContract(t, "memory", forge.NewMemoryRepoIntegrationStore(), webhooks.NewMemoryConfigStore(), nil)
	})
	t.Run("mongo", func(t *testing.T) {
		uri := os.Getenv("ITERION_TEST_MONGO_URI")
		if uri == "" {
			t.Skip("ITERION_TEST_MONGO_URI not set")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		client, err := mongo.Connect(options.Client().ApplyURI(uri))
		if err != nil {
			t.Fatal(err)
		}
		db := client.Database("iterion_provision_" + uuid.NewString())
		t.Cleanup(func() {
			cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := db.Drop(cleanup); err != nil {
				t.Error(err)
			}
			if err := client.Disconnect(cleanup); err != nil {
				t.Error(err)
			}
		})
		integrations := forge.NewMongoRepoIntegrationStore(db)
		if err := integrations.EnsureSchema(ctx); err != nil {
			t.Fatal(err)
		}
		if err := webhooks.EnsureSchema(ctx, db); err != nil {
			t.Fatal(err)
		}
		provisionStoreContract(t, "mongo", integrations, webhooks.NewMongoStores(db).Configs, db)
	})
}
func provisionStoreContract(t *testing.T, name string, is forge.RepoIntegrationStore, cs webhooks.ConfigStore, db *mongo.Database) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	count := 0
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	ck := func(ok bool, format string, args ...any) {
		t.Helper()
		count++
		if !ok {
			t.Fatalf(format, args...)
		}
	}
	t0 := time.Date(2026, 10, 26, 10, 0, 0, 0, time.UTC)
	t1 := t0.Add(time.Hour)
	t2 := t1.Add(time.Hour)
	c := webhooks.Config{ID: "c", TenantID: "t", Name: "$operator", TokenHash: "hash", AuthorAllowlist: []string{"a"}, LabelAllowlist: []string{"l"}, HoldLabels: []string{"h"}, BotIDs: []string{"bot"}, HMACSecretSealed: []byte{1}, LaunchVars: map[string]string{"x": "$literal"}, Routing: &llmroute.Policy{Providers: []string{"openai"}}, LastUsedAt: &t0}
	must(cs.Create(ctx, c))
	c.BotIDs[0] = "bad-create"
	r, e := cs.Get(ctx, "c")
	must(e)
	ck(r.BotIDs[0] == "bot", "%s config create aliases", name)
	stale := r
	must(cs.MarkUsed(ctx, "c", t1))
	r.Name = "$price"
	r.AuthorAllowlist = []string{}
	r.LabelAllowlist = []string{}
	r.HoldLabels = []string{}
	r.HMACSecretSealed = nil
	r.LaunchVars = nil
	r.Routing = nil
	r.LastUsedAt = &t2
	must(cs.Update(ctx, r))
	r.BotIDs[0] = "bad-update"
	saved, e := cs.Get(ctx, "c")
	must(e)
	ck(saved.Version == 1 && saved.Name == "$price" && saved.LastUsedAt.Equal(t1), "%s config v0 CAS/stamp/literal %+v", name, saved)
	ck(saved.BotIDs[0] == "bot", "%s config update aliases", name)
	ck(saved.AuthorAllowlist != nil && len(saved.AuthorAllowlist) == 0 && saved.LabelAllowlist != nil && saved.HoldLabels != nil, "%s config explicit-empty collapse", name)
	ck(saved.LaunchVars == nil && saved.Routing == nil && len(saved.HMACSecretSealed) == 0, "%s config omitempty clear", name)
	ck(errors.Is(cs.Update(ctx, stale), webhooks.ErrConflict), "%s config stale accepted", name)
	saved.BotIDs[0] = "bad-get"
	*saved.LastUsedAt = t2
	rows, e := cs.ListByTenant(ctx, "t")
	must(e)
	ck(rows[0].BotIDs[0] == "bot" && rows[0].LastUsedAt.Equal(t1), "%s config get aliases", name)
	rows[0].BotIDs[0] = "bad-list"
	again, e := cs.Get(ctx, "c")
	must(e)
	ck(again.BotIDs[0] == "bot", "%s config list aliases", name)
	if db != nil {
		_, e = db.Collection("webhook_configs").UpdateOne(ctx, bson.M{"_id": "c"}, bson.M{"$unset": bson.M{"version": ""}})
		must(e)
		legacy, e := cs.Get(ctx, "c")
		must(e)
		ck(legacy.Version == 0, "mongo legacy config decode")
		must(cs.Update(ctx, legacy))
		ck(errors.Is(cs.Update(ctx, legacy), webhooks.ErrConflict), "mongo legacy config second write accepted")
	}
	ri := forge.RepoIntegration{ID: "i", TenantID: "t", ConnectionID: "conn", RepoFullName: "o/r", WebhookID: "wh", ProjectBot: &forge.ProjectBot{TokenID: "old", ExpiresAt: &t2}, ManagedSecretID: "sec", LaunchVars: map[string]string{"a": "old"}, WebhookSecretSealed: []byte{1}, CandidateSecretSealed: []byte{2}, HoldLabels: []string{"hold"}, Routing: &llmroute.Policy{Providers: []string{"openai"}}}
	must(is.Create(ctx, ri))
	ri.ProjectBot.TokenID = "bad-create"
	g, e := is.Get(ctx, "i")
	must(e)
	ck(g.ProjectBot.TokenID == "old", "%s integration create aliases", name)
	old := g
	must(is.StampSynced(ctx, "i", t1))
	g.LaunchVars = nil
	g.HoldLabels = nil
	g.Routing = nil
	g.CandidateSecretSealed = nil
	g.LastSyncedAt = t2
	must(is.Update(ctx, g))
	g.ProjectBot.TokenID = "bad-update"
	g.WebhookSecretSealed[0] = 99
	got, e := is.Get(ctx, "i")
	must(e)
	ck(got.Version == 1 && got.LastSyncedAt.Equal(t1), "%s integration CAS/stamp", name)
	ck(got.LaunchVars == nil && got.HoldLabels == nil && got.Routing == nil && len(got.CandidateSecretSealed) == 0, "%s integration clear", name)
	ck(got.ProjectBot.TokenID == "old" && got.WebhookSecretSealed[0] == 1, "%s integration update aliases", name)
	ck(errors.Is(is.Update(ctx, old), forge.ErrIntegrationConflict), "%s integration stale accepted", name)
	got.SyncIssuesEnabled = true
	must(is.Update(ctx, got))
	newbot := &forge.ProjectBot{TokenID: "new", ExpiresAt: &t2}
	ck(errors.Is(is.RecordProjectBot(ctx, "i", "wrong", "old", newbot, t2), forge.ErrIntegrationConflict), "%s record accepts wrong secret", name)
	ck(errors.Is(is.RecordProjectBot(ctx, "i", "sec", "wrong", newbot, t2), forge.ErrIntegrationConflict), "%s record accepts wrong token", name)
	must(is.RecordProjectBot(ctx, "i", "sec", "old", newbot, t2))
	newbot.TokenID = "bad-record"
	z, e := is.Get(ctx, "i")
	must(e)
	ck(z.Version == 3 && z.SyncIssuesEnabled && z.LastSyncedAt.Equal(t1) && z.ProjectBot.TokenID == "new", "%s record dropped metadata or aliases: %+v", name, z)
	ck(errors.Is(is.Update(ctx, got), forge.ErrIntegrationConflict), "%s snapshot can overwrite recorded token", name)
	ck(errors.Is(is.RecordProjectBot(ctx, "i", "sec", "old", newbot, t2), forge.ErrIntegrationConflict), "%s record accepts old token twice", name)
	if db != nil {
		_, e = db.Collection(forge.RepoIntegrationsCollectionName).UpdateOne(ctx, bson.M{"_id": "i"}, bson.M{"$unset": bson.M{"version": ""}})
		must(e)
		legacy, e := is.Get(ctx, "i")
		must(e)
		ck(legacy.Version == 0, "mongo legacy integration decode")
		must(is.Update(ctx, legacy))
		ck(errors.Is(is.Update(ctx, legacy), forge.ErrIntegrationConflict), "mongo legacy integration second write accepted")
		_, e = db.Collection(forge.RepoIntegrationsCollectionName).UpdateOne(ctx, bson.M{"_id": "i"}, bson.M{"$unset": bson.M{"version": ""}})
		must(e)
		must(is.RecordProjectBot(ctx, "i", "sec", "new", &forge.ProjectBot{TokenID: "newer"}, t2))
		legacy, e = is.Get(ctx, "i")
		must(e)
		ck(legacy.Version == 1 && legacy.ProjectBot.TokenID == "newer", "mongo record missing version inc")
	}
	for _, v := range []struct {
		id                                 string
		pending, retired, webhook, visible bool
	}{{"active", false, false, true, true}, {"pending", true, false, false, false}, {"pendingActive", true, false, true, true}, {"retired", false, true, true, false}} {
		x := forge.RepoIntegration{ID: v.id, TenantID: "visibility", ConnectionID: "conn", RepoFullName: v.id, ProvisionPending: v.pending, Retired: v.retired, SyncIssuesEnabled: true, ManagedSecretID: "sec", ProjectBot: &forge.ProjectBot{TokenID: "old", ExpiresAt: &t0}}
		if v.webhook {
			x.WebhookID = "shared"
		}
		must(is.Create(ctx, x))
		_, e = is.Get(ctx, v.id)
		ck((e == nil) == v.visible, "%s %s Get visibility=%v", name, v.id, e)
		_, e = is.GetByConnRepo(ctx, "visibility", "conn", v.id)
		ck((e == nil) == v.visible, "%s %s GetBy visibility", name, v.id)
		_, e = is.GetProvision(ctx, v.id)
		must(e)
		_, e = is.GetProvisionByConnRepo(ctx, "visibility", "conn", v.id)
		must(e)
		e = is.RecordProjectBot(ctx, v.id, "sec", "old", &forge.ProjectBot{TokenID: "next", ExpiresAt: &t0}, t1)
		ck((e == nil) == v.visible, "%s %s Record visibility %v", name, v.id, e)
		if !v.visible {
			ck(errors.Is(e, forge.ErrIntegrationNotFound), "%s %s wrong hidden record error", name, v.id)
		}
	}
	vis, e := is.ListByTenant(ctx, "visibility")
	must(e)
	ck(len(vis) == 2, "%s visible list=%d", name, len(vis))
	raw, e := is.ListProvisionsByConnection(ctx, "visibility", "conn")
	must(e)
	ck(len(raw) == 4, "%s raw list=%d", name, len(raw))
	by, e := is.ListByConnection(ctx, "visibility", "conn")
	must(e)
	ck(len(by) == 2, "%s connection visible list", name)
	by, e = is.ListByWebhook(ctx, "visibility", "shared")
	must(e)
	ck(len(by) == 2, "%s webhook visible list", name)
	for _, r := range raw {
		r.ProjectBot.TokenID = "bad-list"
	}
	r0, e := is.GetProvision(ctx, "retired")
	must(e)
	ck(r0.ProjectBot.TokenID == "old", "%s raw list aliases", name)
	if db != nil {
		var doc bson.M
		must(db.Collection("webhook_configs").FindOne(ctx, bson.M{"_id": "c"}).Decode(&doc))
		_, exists := doc["launch_vars"]
		ck(!exists, "mongo pipeline retained cleared omitempty field")
		t.Log("mongo raw absent field verified")
	}
	t.Logf("%s passed %d assertions", name, count)
	projectionStoreContract(t, is, db)
}

func projectionStoreContract(t *testing.T, is forge.RepoIntegrationStore, db *mongo.Database) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var schedules cloudsched.Store = cloudsched.NewMemoryStore()
	if db != nil {
		schedules = cloudsched.NewMongoStore(db)
	}
	ri := forge.RepoIntegration{ID: "projection", TenantID: "projection", ConnectionID: "conn", RepoFullName: "projection", WebhookID: "wh"}
	if err := is.Create(ctx, ri); err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- is.WithProvisionVersion(ctx, ri.ID, ri.Version, false, func(tx context.Context) error {
			if err := schedules.Create(tx, cloudsched.ScheduledBot{ID: "projected", TenantID: ri.TenantID, RepoIntegrationID: ri.ID}); err != nil {
				return err
			}
			close(entered)
			<-release
			return nil
		})
	}()
	select {
	case <-entered:
	case err := <-done:
		t.Fatalf("projection failed: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	retirement := make(chan error, 1)
	ri.Retired = true
	go func() { retirement <- is.Update(ctx, ri) }()
	select {
	case err := <-retirement:
		close(release)
		<-done
		t.Fatalf("retirement crossed in-flight projection: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := <-retirement; !errors.Is(err, forge.ErrIntegrationConflict) {
		t.Fatalf("retirement did not observe projection version: %v", err)
	}
	if err := is.WithProvisionVersion(ctx, ri.ID, 0, false, func(context.Context) error { t.Error("stale projection callback executed"); return nil }); !errors.Is(err, forge.ErrIntegrationConflict) {
		t.Fatalf("stale projection accepted: %v", err)
	}
	current, err := is.Get(ctx, ri.ID)
	if err != nil {
		t.Fatal(err)
	}
	current.Retired = true
	if err = is.Update(ctx, current); err != nil {
		t.Fatal(err)
	}
	current.Version++
	if err = is.WithProvisionVersion(ctx, ri.ID, current.Version, false, func(context.Context) error { t.Error("retired active projection executed"); return nil }); !errors.Is(err, forge.ErrIntegrationConflict) {
		t.Fatalf("retired projection accepted: %v", err)
	}
	if err = is.WithProvisionVersion(ctx, ri.ID, current.Version, true, func(tx context.Context) error {
		rows, err := schedules.ListByIntegration(tx, ri.TenantID, ri.ID)
		if err != nil {
			return err
		}
		if len(rows) != 1 {
			t.Errorf("retirement snapshot lost completed projection: %+v", rows)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
