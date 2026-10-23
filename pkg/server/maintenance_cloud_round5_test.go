package server

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dispatcher/boardmongo"
	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/trigger"
)

// The constructor and worker are production code. Only their storage boundary
// changes: real atomic file outbox, no sockets or Mongo required for this proof.
type round5CloudStore struct {
	trigger.EffectOutbox
}

func (*round5CloudStore) TriggerCursor() (int64, error)                   { return 0, nil }
func (*round5CloudStore) EventsAfter(int64, int) ([]native.Event, error)  { return nil, nil }
func (*round5CloudStore) AdvanceTriggerCursor(int64, int64) (bool, error) { panic("no board events") }
func (*round5CloudStore) Get(string) (*native.Issue, error)               { panic("no board effects") }
func (*round5CloudStore) Update(string, native.Patch) (*native.Issue, error) {
	panic("no board effects")
}
func (*round5CloudStore) ConsumeLabels(string, []string) (bool, error) { panic("no board effects") }
func (*round5CloudStore) ConsumeLabelsForEffect(string, []string, string) (bool, error) {
	panic("no board effects")
}

type round5CloudStores struct {
	st     boardmongo.TriggerStore
	tenant string
}

func (c round5CloudStores) TriggerStoreFor(tenant string) boardmongo.TriggerStore {
	if tenant != c.tenant {
		panic("cross-tenant effect")
	}
	return c.st
}
func (c round5CloudStores) DistinctEffectTenants(context.Context) ([]string, error) {
	return []string{c.tenant}, nil
}

type round5CloudSubs struct{ trigger.SubscriptionStore }

func (round5CloudSubs) DistinctBoardTenants(context.Context) ([]string, error) { return nil, nil }

func TestRound5CloudConstructorDurableBusRecovery(t *testing.T) {
	t.Setenv("ITERION_CLOUD_BOARD_TICK", "1h")
	for _, source := range []trigger.Source{trigger.SourceCustom, trigger.SourceRun} {
		t.Run(string(source), func(t *testing.T) {
			ctx := context.Background()
			pub := &spinePublisher{}
			s := newGatedSpineServer(t, gateSpec{id: "t1"}, pub)
			fence := armTestFence(t, s)
			subs := round5CloudSubs{trigger.NewMemorySubscriptionStore()}
			sub := trigger.Subscription{ID: "sub", TenantID: "t1", Enabled: true, BotID: "probe", Mode: "direct", Match: trigger.Matcher{Sources: []trigger.Source{source}}}
			if err := subs.Create(ctx, sub); err != nil {
				t.Fatal(err)
			}
			ev := trigger.Event{ID: "custom-once", TenantID: "t1", Source: source, Kind: "probe"}
			if source == trigger.SourceRun {
				r, err := s.cfg.Store.CreateRun(ctx, "finished", "probe", nil)
				if err != nil {
					t.Fatal(err)
				}
				r.TenantID, r.Status = "t1", store.RunStatusFinished
				if err := s.cfg.Store.SaveRun(ctx, r); err != nil {
					t.Fatal(err)
				}
				ev = trigger.BuildRunOutcome(ctx, s.cfg.Store, r.ID, nil)
			}
			path := filepath.Join(t.TempDir(), "effects.json")
			out := trigger.NewFileEffectOutbox(path)
			stores := round5CloudStores{st: &round5CloudStore{out}, tenant: "t1"}
			bus := &round3CloudBus{}
			start := func() *CloudTriggerCoordinator {
				c := StartCloudTriggerCoordinator(stores, subs, s.triggerLauncher(), nil, bus, nil, s.cfg.Store)
				if c == nil {
					t.Fatal("coordinator not started")
				}
				return c
			}
			c := start()
			defer func() { c.Close(ctx) }()
			if err := bus.Publish(ctx, ev); err != nil {
				t.Fatal(err)
			}
			read := func() trigger.EffectRow {
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Fatal("no durable materialization:", err)
				}
				var rows map[string]trigger.EffectRow
				if err := json.Unmarshal(raw, &rows); err != nil {
					t.Fatal(err)
				}
				row, ok := rows[trigger.EffectID(ev.ID, sub.ID)]
				if !ok {
					t.Fatal("effect missing")
				}
				return row
			}
			row := read()
			if row.State != trigger.EffectPending || pub.count() != 0 {
				t.Fatalf("before execution: %+v launches=%d", row, pub.count())
			}
			c.source.ticks = effectDrainEveryNTicks - 1
			c.source.tickOnce()
			row = read()
			if row.State != trigger.EffectPending || row.Attempts != 0 || row.LastError == "" || pub.count() != 0 {
				t.Fatalf("hold: %+v launches=%d", row, pub.count())
			}
			c.Close(ctx)
			stores.st = &round5CloudStore{trigger.NewFileEffectOutbox(path)}
			c = start()
			disarmFence(t, fence)
			if err := out.MarkRetry(ctx, row.ID, row.ClaimID, row.Attempts, time.Now().Add(-time.Second), row.LastError); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 2; i++ {
				c.source.ticks = effectDrainEveryNTicks - 1
				c.source.tickOnce()
			}
			row = read()
			if row.State != trigger.EffectDone || pub.count() != 1 {
				t.Fatalf("after restart/disarm: %+v launches=%d", row, pub.count())
			}
		})
	}
}
