package orgsweep

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/identity"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/event"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/mongo/writeconcern"
)

func orgStorageDB(t *testing.T) (*mongo.Database, context.Context) {
	t.Helper()
	uri := os.Getenv("ITERION_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("Mongo fixture required")
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	t.Cleanup(cancel)
	client, err := mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatal(err)
	}
	db := client.Database(fmt.Sprintf("r53_org_storage_%d", time.Now().UnixNano()))
	t.Cleanup(func() {
		c, stop := context.WithTimeout(context.Background(), 15*time.Second)
		defer stop()
		_ = db.Drop(c)
		_ = client.Disconnect(c)
	})
	return db, ctx
}

func TestOrgStorageDeleteFailureRetainsIdentity(t *testing.T) {
	for _, coll := range []string{"runs", "run_tags", "run_data_v1_seq", "org_usage"} {
		t.Run(coll, func(t *testing.T) {
			db, ctx := orgStorageDB(t)
			// A real Mongo view refuses DeleteMany. No fake delete function or swallowed test error.
			if err := db.RunCommand(ctx, bson.D{{Key: "create", Value: coll}, {Key: "viewOn", Value: "view_source"}, {Key: "pipeline", Value: bson.A{}}}).Err(); err != nil {
				t.Fatal(err)
			}
			st := identity.NewMemoryStore()
			org := pendingOrg(t, st, "purged", "tenant")
			if coll != "runs" {
				if _, err := db.Collection("runs").InsertOne(ctx, bson.M{"tenant_id": "tenant"}); err != nil {
					t.Fatal(err)
				}
			}
			if coll != "org_usage" {
				if _, err := db.Collection("org_usage").InsertOne(ctx, bson.M{"_id": "org|purged|month"}); err != nil {
					t.Fatal(err)
				}
			}
			cascades := 0
			p := &Purger{DB: db, Store: st, Runs: newFakeRunStore(), ForceSweep: map[string]bool{"purged": true}, Cascade: func(context.Context, string) error { cascades++; return nil }}
			n, err := p.PurgeOrg(ctx, org.ID)
			want := int64(0)
			if coll != "runs" {
				want = 1
			}
			if err == nil || !strings.Contains(err.Error(), coll) || cascades != 0 || n != want {
				t.Fatalf("failed collection=%s: n=%d want=%d err=%v cascades=%d; expected incomplete purge", coll, n, want, err, cascades)
			}
			if _, err := st.GetOrg(ctx, org.ID); err != nil {
				t.Fatal("identity lost", err)
			}
			retryWant := int64(0)
			if coll != "org_usage" {
				retryWant = 1
				if got, e := db.Collection("org_usage").CountDocuments(ctx, bson.M{"_id": "org|purged|month"}); e != nil || got != 1 {
					t.Fatalf("later collection was touched: %d %v", got, e)
				}
			}
			if err := db.Collection(coll).Drop(ctx); err != nil {
				t.Fatal(err)
			}
			if n, err = p.PurgeOrg(ctx, org.ID); err != nil || n != retryWant || cascades != 1 {
				t.Fatalf("retry n=%d err=%v cascades=%d", n, err, cascades)
			}
		})
	}
}

func TestOrgStorageOrphanRowsAndCounters(t *testing.T) {
	db, ctx := orgStorageDB(t)
	st := identity.NewMemoryStore()
	org := pendingOrg(t, st, "purged", "tenant")
	// Public IDs deliberately collide; only the canonical tenant selects rows.
	payloads := []string{"run_data_v1_events", "run_data_v1_logs", "run_data_v1_plans", "run_data_v1_notes"}
	for _, coll := range payloads {
		for _, tenant := range []string{"tenant", "foreign"} {
			for _, birth := range []string{"retired", "orphan"} {
				_, err := db.Collection(coll).InsertOne(ctx, bson.M{"tenant_id": tenant, "run_id": "same", "data_namespace": tenant + "/" + birth, "seq": 0})
				if err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	for _, coll := range []string{"run_seq", "run_data_v1_seq"} {
		for _, tenant := range []string{"tenant", "foreign"} {
			for _, reversed := range []bool{false, true} {
				key := bson.D{{Key: "tenant_id", Value: tenant}, {Key: "data_namespace", Value: "birth"}}
				if coll == "run_seq" {
					key[1] = bson.E{Key: "run_id", Value: "same"}
				}
				if reversed {
					key[0], key[1] = key[1], key[0]
				}
				if _, err := db.Collection(coll).InsertOne(ctx, bson.M{"_id": key, "next_note_seq": 10}); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	// This old unscoped key cannot be attributed without its parent; never sweep globally.
	if _, err := db.Collection("run_seq").InsertOne(ctx, bson.M{"_id": "unattributable", "next_seq": 5}); err != nil {
		t.Fatal(err)
	}
	cascades := 0
	p := &Purger{DB: db, Store: st, Runs: newFakeRunStore(), Cascade: func(context.Context, string) error { cascades++; return nil }}
	n, err := p.PurgeOrg(ctx, org.ID)
	if err != nil || n != 12 || cascades != 1 {
		t.Fatalf("purge n=%d want12 err=%v cascades=%d", n, err, cascades)
	}
	for _, coll := range payloads {
		for _, tenant := range []string{"tenant", "foreign"} {
			want := int64(0)
			if tenant == "foreign" {
				want = 2
			}
			n, err := db.Collection(coll).CountDocuments(ctx, bson.M{"tenant_id": tenant})
			if err != nil || n != want {
				t.Fatalf("%s/%s n=%d want%d err%v", coll, tenant, n, want, err)
			}
		}
	}
	for _, coll := range []string{"run_seq", "run_data_v1_seq"} {
		for _, tenant := range []string{"tenant", "foreign"} {
			want := int64(0)
			if tenant == "foreign" {
				want = 2
			}
			n, err := db.Collection(coll).CountDocuments(ctx, bson.M{"_id.tenant_id": tenant})
			if err != nil || n != want {
				t.Fatalf("%s/%s n=%d want%d err%v", coll, tenant, n, want, err)
			}
		}
	}
	if n, err := db.Collection("run_seq").CountDocuments(ctx, bson.M{"_id": "unattributable"}); err != nil || n != 1 {
		t.Fatal(n, err)
	}
}

func TestOrgStorageUnacknowledgedRetainsIdentity(t *testing.T) {
	db, ctx := orgStorageDB(t)
	st := identity.NewMemoryStore()
	org := pendingOrg(t, st, "purged", "tenant")
	unack := db.Client().Database(db.Name(), options.Database().SetWriteConcern(writeconcern.Unacknowledged()))
	cascades := 0
	p := &Purger{DB: unack, Store: st, Runs: newFakeRunStore(), Cascade: func(context.Context, string) error { cascades++; return nil }}
	n, err := p.PurgeOrg(ctx, org.ID)
	if err == nil || !strings.Contains(err.Error(), "acknowledged") || n != 0 || cascades != 0 {
		t.Fatalf("unack purge n=%d err=%v cascades=%d", n, err, cascades)
	}
}

func TestOrgStorageCancellationRetainsIdentity(t *testing.T) {
	for _, last := range []bool{false, true} {
		t.Run(fmt.Sprint(last), func(t *testing.T) {
			db, ctx := orgStorageDB(t)
			if _, err := db.Collection("runs").InsertOne(ctx, bson.M{"tenant_id": "tenant"}); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Collection("org_usage").InsertOne(ctx, bson.M{"_id": "org|purged|month"}); err != nil {
				t.Fatal(err)
			}
			cancelCtx, cancel := context.WithCancel(ctx)
			defer cancel()
			var selected sync.Map
			monitor := &event.CommandMonitor{
				Started: func(_ context.Context, e *event.CommandStartedEvent) {
					if e.CommandName == "delete" && (!last || e.Command.Lookup("delete").StringValue() == "org_usage") {
						selected.Store(e.RequestID, true)
					}
				},
				Succeeded: func(_ context.Context, e *event.CommandSucceededEvent) {
					if _, ok := selected.LoadAndDelete(e.RequestID); ok {
						cancel()
					}
				}}
			client, err := mongo.Connect(options.Client().ApplyURI(os.Getenv("ITERION_TEST_MONGO_URI")).SetMonitor(monitor))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				c, stop := context.WithTimeout(context.Background(), 15*time.Second)
				defer stop()
				_ = client.Disconnect(c)
			})
			st := identity.NewMemoryStore()
			org := pendingOrg(t, st, "purged", "tenant")
			cascades := 0
			p := &Purger{DB: client.Database(db.Name()), Store: st, Runs: newFakeRunStore(), Cascade: func(context.Context, string) error { cascades++; return nil }}
			n, err := p.PurgeOrg(cancelCtx, org.ID)
			want := int64(1)
			left := int64(1)
			if last {
				want = 2
				left = 0
			}
			if !errors.Is(err, context.Canceled) || n != want || cascades != 0 {
				t.Fatalf("cancelled purge n=%d want%d err=%v cascades=%d", n, want, err, cascades)
			}
			if n, err := db.Collection("org_usage").CountDocuments(ctx, bson.M{}); err != nil || n != left {
				t.Fatal(n, err)
			}
		})
	}
}

func TestOrgStoragePoolAttachments(t *testing.T) {
	db, ctx := orgStorageDB(t)
	st := identity.NewMemoryStore()
	org := pendingOrg(t, st, "purged", "team.+")
	ids := []string{"team:team.+", "bot:team.+/one", "bot:team.+/two/deep", "team:foreign", "bot:teamX+/one", "bot:team.+other/one", "platform", "org:foreign"}
	for _, id := range ids {
		if _, err := db.Collection("pool_attachments").InsertOne(ctx, bson.M{"_id": id}); err != nil {
			t.Fatal(err)
		}
	}
	cascades := 0
	p := &Purger{DB: db, Store: st, Runs: newFakeRunStore(), Cascade: func(context.Context, string) error { cascades++; return nil }}
	n, err := p.PurgeOrg(ctx, org.ID)
	if err != nil || n != 3 || cascades != 1 {
		t.Fatalf("pool purge n=%d want3 err=%v cascades=%d", n, err, cascades)
	}
	for i, id := range ids {
		want := int64(1)
		if i < 3 {
			want = 0
		}
		n, err := db.Collection("pool_attachments").CountDocuments(ctx, bson.M{"_id": id})
		if err != nil || n != want {
			t.Fatalf("attachment %s n=%d want%d err=%v", id, n, want, err)
		}
	}
}
