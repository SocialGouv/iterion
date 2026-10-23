package cloudsched

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/internal/mongotest"
	"github.com/SocialGouv/iterion/pkg/lease"
	"github.com/SocialGouv/iterion/pkg/schedgate"
	"github.com/SocialGouv/iterion/pkg/store"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

func assertScheduleExclusion(t *testing.T, st Store) {
	t.Helper()
	ctx := context.Background()
	var old context.Context
	ok, err := st.WithSchedule(ctx, "one", func(ctx context.Context) {
		old = context.WithoutCancel(ctx)
		if ok, err := st.WithSchedule(ctx, "one", func(context.Context) { t.Error("same schedule entered twice") }); err != nil || ok {
			t.Errorf("contending schedule: %v %v", ok, err)
		}
		if ok, err := st.WithSchedule(ctx, "two", func(context.Context) {}); err != nil || !ok {
			t.Errorf("independent schedule blocked: %v %v", ok, err)
		}
	})
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	wrote := false
	err = store.PersistLaunch(old, func(context.Context) error { wrote = true; return nil })
	if !errors.Is(err, schedgate.ErrMaintenanceHold) || wrote {
		t.Fatalf("released owner persisted: err=%v wrote=%v", err, wrote)
	}
}

func TestScheduleExclusionMemory(t *testing.T) { assertScheduleExclusion(t, NewMemoryStore()) }

func scheduleMongo(t *testing.T) *mongo.Database {
	t.Helper()
	uri := os.Getenv("ITERION_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("ITERION_TEST_MONGO_URI not set; requires replica set")
	}
	ctx, cancel := mongotest.Ctx(t)
	defer cancel()
	client, err := mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatal(err)
	}
	db := client.Database(fmt.Sprintf("iterion_schedule_fence_%d", time.Now().UnixNano()))
	t.Cleanup(func() {
		ctx, cancel := mongotest.TeardownCtx()
		defer cancel()
		_ = db.Drop(ctx)
		_ = client.Disconnect(ctx)
	})
	if err := EnsureSchema(ctx, db); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestScheduleExclusionMongo(t *testing.T) {
	assertScheduleExclusion(t, NewMongoStore(scheduleMongo(t)))
}

func TestScheduleMongoExpiredOwnerCannotPersist(t *testing.T) {
	db := scheduleMongo(t)
	st := NewMongoStore(db)
	ctx, cancel := mongotest.Ctx(t)
	defer cancel()
	_, err := st.WithSchedule(ctx, "expired", func(term context.Context) {
		// Simulate a paused process: do not depend on the renewer's goroutine
		// cancelling it. The persistence transaction itself must reject its token.
		_, err := db.Collection(lease.Collection).UpdateOne(ctx, bson.M{"_id": "schedule-launch:expired"}, bson.M{"$set": bson.M{"owner": "successor"}})
		if err != nil {
			t.Fatal(err)
		}
		err = store.PersistLaunch(context.WithoutCancel(term), func(tx context.Context) error {
			_, err := db.Collection("runs").InsertOne(tx, bson.M{"_id": "stale"})
			return err
		})
		if !errors.Is(err, schedgate.ErrMaintenanceHold) {
			t.Fatalf("stale owner: %v", err)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if n, err := db.Collection("runs").CountDocuments(ctx, bson.M{}); err != nil || n != 0 {
		t.Fatalf("stale run count=%d err=%v", n, err)
	}
}

func TestScheduleMongoReceiptAndRunCommitTogether(t *testing.T) {
	db := scheduleMongo(t)
	st := NewMongoStore(db)
	ctx, cancel := mongotest.Ctx(t)
	defer cancel()
	failure := errors.New("injected run write failure")
	_, err := st.WithSchedule(ctx, "atomic", func(term context.Context) {
		err := store.PersistLaunch(term, func(tx context.Context) error {
			if mongo.SessionFromContext(tx) == nil {
				t.Fatal("write has no transaction")
			}
			if _, err := db.Collection("runs").InsertOne(tx, bson.M{"_id": "aborted"}); err != nil {
				return err
			}
			return failure
		})
		if !errors.Is(err, schedgate.ErrMaintenanceHold) {
			t.Fatalf("failed transaction: %v", err)
		}
		var row bson.M
		if err := db.Collection(lease.Collection).FindOne(ctx, bson.M{"_id": "schedule-launch:atomic"}).Decode(&row); err != nil {
			t.Fatal(err)
		}
		if _, exists := row["launch_commits"]; exists {
			t.Fatalf("receipt survived aborted run: %v", row)
		}
		err = store.PersistLaunch(term, func(tx context.Context) error {
			_, err := db.Collection("runs").InsertOne(tx, bson.M{"_id": "committed"})
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if n, err := db.Collection("runs").CountDocuments(ctx, bson.M{}); err != nil || n != 1 {
		t.Fatalf("committed run count=%d err=%v", n, err)
	}
}
