package secrets

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/SocialGouv/iterion/pkg/internal/mongotest"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The bot-binding store has two twins and one contract. What they must agree
// on is not CRUD — it is what they REFUSE: the memory twin used to accept a
// second binding on a name already bound, which the Mongo unique index has
// refused since the collection existed. Every in-memory test of that conflict
// passed while production answered a raw driver error, the class this repo
// has already paid for (#2401 item 26). A binding kind only widens the
// surface, so the conformance runs on both twins or it proves nothing.
//
// The Mongo half is gated the way every conformance suite here is: CI's
// mongo-conformance job sets ITERION_TEST_MONGO_URI, and a local run without
// it skips with a reason instead of passing quietly.
type bindingTwin struct {
	name  string
	store BotSecretBindingStore
	// rawInsert writes a document the Go type did not produce, so the suite
	// can read what a binding stored BEFORE kinds existed looks like. The
	// memory twin has no wire form, so it stores the zero-kind struct.
	rawInsert func(t *testing.T, ctx context.Context, id, tenantID, botID, name string)
}

func bindingTwins(t *testing.T) []bindingTwin {
	t.Helper()
	mem := NewMemoryBotSecretBindingStore()
	twins := []bindingTwin{{
		name:  "memory",
		store: mem,
		rawInsert: func(t *testing.T, ctx context.Context, id, tenantID, botID, name string) {
			t.Helper()
			if err := mem.Create(ctx, BotSecretBinding{
				ID: id, TenantID: tenantID, BotID: botID,
				SecretID: "sec-legacy", SecretNameForWorkflow: name,
			}); err != nil {
				t.Fatalf("memory raw insert: %v", err)
			}
		},
	}}

	uri := os.Getenv("ITERION_TEST_MONGO_URI")
	if uri == "" {
		t.Log("ITERION_TEST_MONGO_URI not set; the Mongo twin of the binding conformance is skipped")
		return twins
	}
	ctx, cancel := mongotest.Ctx(t)
	t.Cleanup(cancel)
	client, err := mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatalf("mongo connect: %v", err)
	}
	nonce := make([]byte, 4)
	_, _ = rand.Read(nonce)
	db := client.Database("iterion_bindings_" + hex.EncodeToString(nonce))
	t.Cleanup(func() {
		drop, dropCancel := mongotest.TeardownCtx()
		defer dropCancel()
		_ = db.Drop(drop)
		_ = client.Disconnect(drop)
	})
	mongoStore := NewMongoBotSecretBindingStore(db)
	if err := mongoStore.EnsureSchema(ctx); err != nil {
		t.Fatalf("ensure schema: %v", err)
	}
	return append(twins, bindingTwin{
		name:  "mongo",
		store: mongoStore,
		rawInsert: func(t *testing.T, ctx context.Context, id, tenantID, botID, name string) {
			t.Helper()
			// No `kind` field at all: the document a binding written before
			// kinds existed leaves on disk.
			if _, err := db.Collection(BotSecretBindingsCollectionName).InsertOne(ctx, bson.M{
				"_id": id, "tenant_id": tenantID, "bot_id": botID,
				"secret_id": "sec-legacy", "secret_name_for_workflow": name,
				"created_at": time.Now().UTC(), "updated_at": time.Now().UTC(),
			}); err != nil {
				t.Fatalf("mongo raw insert: %v", err)
			}
		},
	})
}

func contextBindingFixture(id, tenantID, botID, name string) BotSecretBinding {
	now := time.Now().UTC().Truncate(time.Millisecond)
	return BotSecretBinding{
		ID:                    id,
		TenantID:              tenantID,
		BotID:                 botID,
		Kind:                  BindingKindContext,
		SecretNameForWorkflow: name,
		Context: &ContextSource{
			GitURL:   "https://example.invalid/org/context.git",
			Ref:      "v1.4.2",
			Path:     "handbook/pricing.pdf",
			SecretID: "sec-read",
		},
		CreatedBy: "alice",
		CreatedAt: now,
		UpdatedAt: now,
	}
}

func TestBotBindingTwins_AContextBindingReadsBackWholeOnEitherTwin(t *testing.T) {
	for _, tw := range bindingTwins(t) {
		t.Run(tw.name, func(t *testing.T) {
			ctx := store.WithTenant(context.Background(), "team-1")
			want := contextBindingFixture("bind-ctx", "team-1", "bot-doki", "handbook")
			if err := tw.store.Create(ctx, want); err != nil {
				t.Fatalf("create: %v", err)
			}
			got, err := tw.store.Get(ctx, "bind-ctx")
			if err != nil {
				t.Fatalf("get: %v", err)
			}
			if got.EffectiveKind() != BindingKindContext {
				t.Errorf("kind = %q, want %q", got.EffectiveKind(), BindingKindContext)
			}
			if got.Context == nil {
				t.Fatalf("the pinned source did not survive the round trip: %+v", got)
			}
			if *got.Context != *want.Context {
				t.Errorf("pinned source = %+v, want %+v", *got.Context, *want.Context)
			}
			// The listing a team page reads must carry the kind too, or the
			// surface shows a secret binding where a context lives.
			list, err := tw.store.ListByTenantBot(ctx, "team-1", "bot-doki")
			if err != nil {
				t.Fatalf("list: %v", err)
			}
			if len(list) != 1 || list[0].EffectiveKind() != BindingKindContext || list[0].Context == nil {
				t.Errorf("ListByTenantBot = %+v, want one context binding with its source", list)
			}
		})
	}
}

func TestBotBindingTwins_ASecondBindingOnTheSameNameIsRefused(t *testing.T) {
	for _, tw := range bindingTwins(t) {
		t.Run(tw.name, func(t *testing.T) {
			ctx := store.WithTenant(context.Background(), "team-1")
			first := contextBindingFixture("bind-1", "team-1", "bot-doki", "handbook")
			if err := tw.store.Create(ctx, first); err != nil {
				t.Fatalf("create first: %v", err)
			}
			// Same declared name on the same bot, other kind, other payload:
			// the name is what one bot can bind once.
			second := BotSecretBinding{
				ID: "bind-2", TenantID: "team-1", BotID: "bot-doki",
				SecretID: "sec-other", SecretNameForWorkflow: "handbook",
			}
			if err := tw.store.Create(ctx, second); !errors.Is(err, ErrBindingNameConflict) {
				t.Errorf("create duplicate name = %v, want ErrBindingNameConflict", err)
			}
			// Re-saving a binding under the name it already holds is not a
			// conflict with itself — the refusal keys on the row, not on the
			// name alone, or every edit of a binding would fail.
			first.AllowedHosts = []string{"example.invalid"}
			if err := tw.store.Update(ctx, first); err != nil {
				t.Errorf("updating a binding while keeping its own name = %v, want nil", err)
			}
			// Another bot may bind the same name: the key is per bot.
			onAnotherBot := contextBindingFixture("bind-3", "team-1", "bot-nexie", "handbook")
			if err := tw.store.Create(ctx, onAnotherBot); err != nil {
				t.Errorf("the same name on another bot was refused: %v", err)
			}
		})
	}
}

func TestBotBindingTwins_ABindingStoredWithoutAKindReadsAsTheSecretKind(t *testing.T) {
	for _, tw := range bindingTwins(t) {
		t.Run(tw.name, func(t *testing.T) {
			ctx := store.WithTenant(context.Background(), "team-1")
			tw.rawInsert(t, ctx, "bind-legacy", "team-1", "bot-doki", "GITHUB_TOKEN")
			got, err := tw.store.Get(ctx, "bind-legacy")
			if err != nil {
				t.Fatalf("get: %v", err)
			}
			if got.Kind != "" {
				t.Errorf("a binding stored before kinds existed came back with Kind = %q", got.Kind)
			}
			if got.EffectiveKind() != BindingKindSecret {
				t.Errorf("EffectiveKind = %q, want %q", got.EffectiveKind(), BindingKindSecret)
			}
			if err := got.Validate(); err != nil {
				t.Errorf("a binding stored before kinds existed no longer validates: %v", err)
			}
		})
	}
}
