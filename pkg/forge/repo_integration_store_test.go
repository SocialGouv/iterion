package forge

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/SocialGouv/iterion/pkg/internal/mongotest"
)

// StampSynced writes the issue sync's high-water mark and NOTHING else. The
// sync pass that stamps it holds an integration it read minutes earlier — at
// the start of a paged window — so a write carrying that whole snapshot
// reverts every field an operator changed while the pass ran.
//
// The two scenarios below are a CONTRACT, run against both implementations:
// the Mongo store is the one that moves the mark in cloud, and its failure
// mode is silent — UpdateOneChecked matches the document, so a `$set` on a
// field name nothing reads returns nil, and the mark never advances again.
func stampSyncedWritesOnlyTheMark(t *testing.T, store RepoIntegrationStore) {
	t.Helper()
	ctx := context.Background()
	mark := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	ri := RepoIntegration{
		ID: "i1", TenantID: "t1", ConnectionID: "c1", RepoFullName: "org/api",
		SyncIssuesEnabled: true, MinAuthorRole: "write", WebhookID: "wh1", LastSyncedAt: mark,
	}
	if err := store.Create(ctx, ri); err != nil {
		t.Fatalf("create: %v", err)
	}
	// The operator turns issue sync off and tightens the author gate while a
	// pass that read `ri` above is still running.
	patched := ri
	patched.SyncIssuesEnabled = false
	patched.MinAuthorRole = "admin"
	if err := store.Update(ctx, patched); err != nil {
		t.Fatalf("operator patch: %v", err)
	}

	at := mark.Add(5 * time.Minute)
	if err := store.StampSynced(ctx, ri.ID, at); err != nil {
		t.Fatalf("stamp: %v", err)
	}

	got, err := store.Get(ctx, ri.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !got.LastSyncedAt.Equal(at) {
		t.Errorf("mark = %s, want %s — a stamp this store accepted without moving the mark leaves the sweep re-reading the same window for ever",
			got.LastSyncedAt, at)
	}
	if got.SyncIssuesEnabled {
		t.Error("the stamp re-enabled issue sync: the operator turned it off and the pass put its stale snapshot back")
	}
	if got.MinAuthorRole != "admin" {
		t.Errorf("min_author_role = %q, want the operator's \"admin\" — the stamp must carry no other field", got.MinAuthorRole)
	}
	if got.WebhookID != "wh1" || got.RepoFullName != "org/api" {
		t.Errorf("the stamp lost an unrelated field: %+v", got)
	}
}

// An integration deleted during the pass is refused, not silently re-created:
// a mark for a repo nobody syncs any more would resurrect nothing and hide
// that the pass was working on a deleted integration.
func stampSyncedRefusesAMissingIntegration(t *testing.T, store RepoIntegrationStore) {
	t.Helper()
	err := store.StampSynced(context.Background(), "gone", time.Now())
	if !errors.Is(err, ErrIntegrationNotFound) {
		t.Fatalf("err = %v, want ErrIntegrationNotFound", err)
	}
}

// The stamp arbitrates NOTHING: the last write wins, even one carrying an
// EARLIER instant than the stored mark. Three paths stamp the same integration
// — the leased sweep, a manual "Sync now" and the webhook projection, which
// runs on whichever replica took the delivery — so two passes in flight stamp
// in the order they FINISH, not the order they started, and the mark can step
// back by as much as their start instants differ. That is safe (every stamped
// instant is the start of a read that completed, so a mark that stepped back
// only widens the next window, which is a re-read) and it is why no caller may
// read the mark as a monotonic counter.
func stampSyncedArbitratesNothing(t *testing.T, store RepoIntegrationStore) {
	t.Helper()
	ctx := context.Background()
	// Its own repo: the Mongo twin runs every scenario on ONE store, whose
	// unique index is (tenant, connection, repo), so a second "org/api" under
	// "c1" is a duplicate-key refusal, not a second integration.
	ri := RepoIntegration{
		ID: "i-conc", TenantID: "t1", ConnectionID: "c1", RepoFullName: "org/web",
		SyncIssuesEnabled: true,
	}
	if err := store.Create(ctx, ri); err != nil {
		t.Fatalf("create: %v", err)
	}
	// A short pass that STARTED later stamps first; the long one that started
	// 10 minutes earlier finishes after it and stamps its own start.
	late := time.Date(2026, 10, 12, 9, 10, 0, 0, time.UTC)
	early := late.Add(-10 * time.Minute)
	if err := store.StampSynced(ctx, ri.ID, late); err != nil {
		t.Fatalf("stamp from the pass that started later: %v", err)
	}
	if err := store.StampSynced(ctx, ri.ID, early); err != nil {
		t.Fatalf("stamp from the long pass: %v", err)
	}

	got, err := store.Get(ctx, ri.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !got.LastSyncedAt.Equal(early) {
		t.Fatalf("mark = %s, want the instant the last stamp carried (%s): this store takes the stamp it is given, and syncOneIntegration's doc tells its readers so — a store that silently kept the greater value would make the mark the monotonic counter nothing here promises",
			got.LastSyncedAt, early)
	}
}

func TestMemoryRepoIntegrationStore_StampSyncedWritesOnlyTheMark(t *testing.T) {
	stampSyncedWritesOnlyTheMark(t, NewMemoryRepoIntegrationStore())
}

func TestMemoryRepoIntegrationStore_StampSyncedRefusesAMissingIntegration(t *testing.T) {
	stampSyncedRefusesAMissingIntegration(t, NewMemoryRepoIntegrationStore())
}

func TestMemoryRepoIntegrationStore_StampSyncedArbitratesNothing(t *testing.T) {
	stampSyncedArbitratesNothing(t, NewMemoryRepoIntegrationStore())
}

// The twin that runs in production. Gated on ITERION_TEST_MONGO_URI, like the
// board-binding suite next door, and inside the package list the REQUIRED
// mongo-conformance job runs with that URI (./pkg/forge/...).
func TestMongoRepoIntegrationStore_StampSynced(t *testing.T) {
	uri := os.Getenv("ITERION_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("ITERION_TEST_MONGO_URI not set; skipping Mongo repo-integration suite")
	}
	client, err := mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatalf("mongo.Connect: %v", err)
	}
	nonce := make([]byte, 4)
	_, _ = rand.Read(nonce)
	db := client.Database("iterion_repo_integration_" + hex.EncodeToString(nonce))
	t.Cleanup(func() {
		drop, cancel := mongotest.TeardownCtx()
		defer cancel()
		_ = db.Drop(drop)
		_ = client.Disconnect(drop)
	})

	store := NewMongoRepoIntegrationStore(db)
	ctx, cancel := mongotest.Ctx(t)
	defer cancel()
	if err := store.EnsureSchema(ctx); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}
	t.Run("writes only the mark", func(t *testing.T) { stampSyncedWritesOnlyTheMark(t, store) })
	t.Run("refuses a missing integration", func(t *testing.T) { stampSyncedRefusesAMissingIntegration(t, store) })
	t.Run("arbitrates nothing", func(t *testing.T) { stampSyncedArbitratesNothing(t, store) })
}
