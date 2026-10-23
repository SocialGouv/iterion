package server

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dispatcher/boardmongo"
	"github.com/SocialGouv/iterion/pkg/eventbus"
	"github.com/SocialGouv/iterion/pkg/internal/mongotest"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/trigger"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// Synchronous delivery makes the assertion happen after the coordinator's
// actual registered handler returns, without relying on sleeps or polling.
type round3CloudBus struct{ handler eventbus.Handler }

func (b *round3CloudBus) Publish(ctx context.Context, ev trigger.Event) error {
	return b.handler(ctx, ev)
}

func (b *round3CloudBus) Subscribe(_ string, _ trigger.Matcher, h eventbus.Handler) (func(context.Context), error) {
	b.handler = h
	return func(context.Context) { b.handler = nil }, nil
}

// Goes through the PUBLIC cloud constructor, bus subscription, real Mongo
// outbox, tenant discovery and source tick, and the server's gated launcher.
// An evaluator-only test cannot detect omission of WithEffectOutbox in the
// cloud constructor or failure to drain tenants with no board subscriptions.
func TestRound3CloudCoordinatorBusHoldRecovery(t *testing.T) {
	uri := os.Getenv("ITERION_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("ITERION_TEST_MONGO_URI not set; requires Mongo conformance harness")
	}
	t.Setenv("ITERION_CLOUD_BOARD_TICK", "1h") // drive the real source tick explicitly
	ctx, cancel := mongotest.Ctx(t)
	defer cancel()
	client, err := mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatal(err)
	}
	db := client.Database(fmt.Sprintf("iterion_cloud_fence_%d", time.Now().UnixNano()))
	t.Cleanup(func() {
		ctx, cancel := mongotest.TeardownCtx()
		defer cancel()
		_ = db.Drop(ctx)
		_ = client.Disconnect(ctx)
	})
	if err := boardmongo.EnsureSchema(ctx, db); err != nil {
		t.Fatal(err)
	}
	for _, source := range []trigger.Source{trigger.SourceCustom, trigger.SourceRun} {
		t.Run(string(source), func(t *testing.T) {
			tenant := "tenant-" + string(source)
			pub := &spinePublisher{}
			s := newGatedSpineServer(t, gateSpec{id: tenant}, pub)
			fence := armTestFence(t, s)
			subs := trigger.NewMongoSubscriptionStore(db)
			sub := trigger.Subscription{ID: "sub-" + tenant, TenantID: tenant, BotID: "probe", Enabled: true,
				Mode: "direct", Match: trigger.Matcher{Sources: []trigger.Source{source}}}
			if err := subs.Create(ctx, sub); err != nil {
				t.Fatal(err)
			}
			ev := trigger.Event{ID: "custom-" + tenant, TenantID: tenant, Source: source, Kind: "probe",
				Payload: map[string]any{"args": "preserve me"}}
			if source == trigger.SourceRun {
				run, err := s.cfg.Store.CreateRun(ctx, "finished-"+tenant, "probe", nil)
				if err != nil {
					t.Fatal(err)
				}
				run.TenantID, run.Status = tenant, store.RunStatusFinished
				if err := s.cfg.Store.SaveRun(ctx, run); err != nil {
					t.Fatal(err)
				}
				ev = trigger.BuildRunOutcome(ctx, s.cfg.Store, run.ID, nil)
			}
			bus := &round3CloudBus{}
			coord := boardmongo.NewCoordinator(db)
			start := func() *CloudTriggerCoordinator {
				c := StartCloudTriggerCoordinator(coord, subs, s.triggerLauncher(), nil, bus, iterlog.Nop(), s.cfg.Store)
				if c == nil {
					t.Fatal("cloud coordinator did not start")
				}
				return c
			}
			c := start()
			defer func() { c.Close(context.Background()) }()
			if err := bus.Publish(ctx, ev); err != nil {
				t.Fatal(err)
			}
			id := trigger.EffectID(ev.ID, sub.ID)
			readRow := func() trigger.EffectRow {
				var row trigger.EffectRow
				if err := db.Collection(boardmongo.EffectsCollection).FindOne(ctx, bson.M{"_id": id}).Decode(&row); err != nil {
					t.Fatalf("bus delivery has no durable effect: %v", err)
				}
				return row
			}
			row := readRow()
			if row.State != trigger.EffectPending || pub.count() != 0 {
				t.Fatalf("delivery: row=%+v launches=%d", row, pub.count())
			}
			// No board subscription: only DistinctEffectTenants can find it.
			c.source.ticks = effectDrainEveryNTicks - 1
			c.source.tickOnce()
			row = readRow()
			if row.State != trigger.EffectPending || row.Attempts != 0 || row.LastError == "" || pub.count() != 0 {
				t.Fatalf("hold: row=%+v launches=%d", row, pub.count())
			}
			c.Close(ctx)
			c = start() // no republish: recover only the durable obligation
			disarmFence(t, fence)
			// Expire just the retry deadline instead of sleeping for the backoff.
			if err := coord.StoreFor(tenant).MarkRetry(ctx, row.ID, row.ClaimID, row.Attempts, time.Now().Add(-time.Second), row.LastError); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 2; i++ {
				c.source.ticks = effectDrainEveryNTicks - 1
				c.source.tickOnce()
			}
			row = readRow()
			if row.State != trigger.EffectDone || pub.count() != 1 {
				t.Fatalf("recovery: row=%+v launches=%d", row, pub.count())
			}
		})
	}
}
