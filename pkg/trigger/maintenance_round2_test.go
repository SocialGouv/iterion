package trigger

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"
)

type heldLauncher struct {
	hold  bool
	plans []LaunchPlan
}

func (l *heldLauncher) Launch(_ context.Context, p LaunchPlan) (string, error) {
	if l.hold {
		return "", ErrMaintenanceHold
	}
	l.plans = append(l.plans, p)
	return "run", nil
}

// Deliver once through Handle; the worker must recover without republishing.
func TestRound2BusEventSurvivesHold(t *testing.T) {
	ctx := context.Background()
	out := NewMemoryEffectOutbox()
	subs := seed(t, Subscription{ID: "chain", BotID: "next", TenantID: "t1", Enabled: true, Mode: "direct", Match: Matcher{Sources: []Source{SourceRun}}, ArgsVar: "prompt"})
	l := &heldLauncher{hold: true}
	e := NewEvaluator(subs, WithLauncher(l), WithEffectOutbox(func(string) EffectOutbox { return out }))
	ev := Event{ID: "finished-1", TenantID: "t1", Source: SourceRun, Kind: "run.finished", Repo: "acme/repo", Payload: map[string]any{"args": "original", PayloadVars: map[string]string{"sha": "abc"}}}
	if err := e.Handle(ctx, ev); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	w := EffectWorker{Outbox: out, Subs: subs, Evaluator: e, Now: func() time.Time { return now }}
	w.Tick(ctx, 10)
	row, ok := out.Row(EffectID(ev.ID, "chain"))
	if !ok || row.State != EffectPending {
		t.Fatalf("held event has no pending effect: %+v", row)
	}
	l.hold = false
	now = now.Add(time.Minute)
	// A fresh worker/evaluator has no memory of the delivery.
	w.Evaluator = NewEvaluator(subs, WithLauncher(l))
	w.Tick(ctx, 10)
	w.Tick(ctx, 10)
	if len(l.plans) != 1 || l.plans[0].Vars["prompt"] != "original" || l.plans[0].Vars["sha"] != "abc" || !reflect.DeepEqual(l.plans[0].Event, ev) {
		t.Fatalf("recovered plans: %+v", l.plans)
	}
}

func TestRound2LocalScheduleRecoversBeforeNextSlot(t *testing.T) {
	ctx := context.Background()
	subs := seed(t, Subscription{ID: "daily", BotID: "daily", Enabled: true, Invocation: "schedule", Cron: "0 9 * * *", Vars: map[string]string{"data": "original"}})
	now := time.Date(2026, 10, 23, 8, 59, 0, 0, time.UTC)
	l := &heldLauncher{hold: true}
	s := NewScheduler(subs, l, WithSchedulerClock(func() time.Time { return now }))
	s.tick(ctx, now)
	now = now.Add(time.Minute)
	s.tick(ctx, now)
	l.hold = false
	now = now.Add(time.Minute)
	s.tick(ctx, now)
	s.tick(ctx, now)
	if len(l.plans) != 1 || l.plans[0].Vars["data"] != "original" {
		t.Fatalf("09:01 recovered launches: %+v", l.plans)
	}
	current, err := subs.Get(ctx, "daily")
	if err != nil || current.LastError != "" {
		t.Fatalf("recovered schedule still unhealthy: %+v %v", current, err)
	}
}

type failConsumedOnce struct {
	*MemoryEffectOutbox
	failed bool
}

func (o *failConsumedOnce) MarkConsumed(ctx context.Context, id, claim string) error {
	if !o.failed {
		o.failed = true
		return errors.New("injected marker write failure")
	}
	return o.MemoryEffectOutbox.MarkConsumed(ctx, id, claim)
}

// Receipt lives with the label mutation, independently of the worker marker.
type receiptedBoard struct {
	stubConsumingBoard
	receipts map[string]bool
}

func (b *receiptedBoard) ConsumeMatchLabelsForEffect(ctx context.Context, tenant, id string, labels []string, key string) (bool, error) {
	if b.receipts[key] {
		return true, nil
	}
	ok, err := b.ConsumeMatchLabels(ctx, tenant, id, labels)
	if ok {
		b.receipts[key] = true
	}
	return ok, err
}
func TestRound2ConsumedMarkerFailureThenHold(t *testing.T) {
	b := &receiptedBoard{stubConsumingBoard: stubConsumingBoard{consumeLeft: 1}, receipts: map[string]bool{}}
	l := &heldLauncher{hold: true}
	w, out, ev := effectWorld(t, directConsumeSub(), b, l)
	fault := &failConsumedOnce{MemoryEffectOutbox: out}
	w.Outbox = fault
	now := time.Now().UTC()
	w.Now = func() time.Time { return now }
	ctx := context.Background()
	w.Tick(ctx, 10)
	now = now.Add(time.Minute)
	w.Tick(ctx, 10)
	l.hold = false
	now = now.Add(time.Minute)
	w.Tick(ctx, 10)
	w.Tick(ctx, 10)
	row, _ := out.Row(EffectID(ev.ID, "s1"))
	if !fault.failed || row.State != EffectDone || len(l.plans) != 1 || b.consumes != 1 {
		t.Fatalf("row=%+v launches=%d consumes=%d marker failed=%v", row, len(l.plans), b.consumes, fault.failed)
	}
}

func TestRound2FileOutboxRecovery(t *testing.T) {
	ctx := context.Background()
	path := t.TempDir() + "/effects.json"
	out := NewFileEffectOutbox(path)
	l := &heldLauncher{hold: true}
	subs := seed(t, Subscription{ID: "s", BotID: "next", Enabled: true, Mode: "direct", Match: Matcher{Sources: []Source{SourceRun}}})
	e := NewEvaluator(subs, WithLauncher(l), WithEffectOutbox(func(string) EffectOutbox { return out }))
	ev := Event{ID: "finished", Source: SourceRun, Kind: "run.finished", Payload: map[string]any{"args": "original"}}
	if err := e.Handle(ctx, ev); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	w := EffectWorker{Outbox: out, Subs: subs, Evaluator: e, Now: func() time.Time { return now }}
	w.Tick(ctx, 10)
	l.hold = false
	now = now.Add(time.Minute)
	w.Outbox = NewFileEffectOutbox(path)
	w.Evaluator = NewEvaluator(subs, WithLauncher(l))
	w.Tick(ctx, 10)
	w.Tick(ctx, 10)
	if len(l.plans) != 1 || l.plans[0].Event.Payload["args"] != "original" {
		t.Fatalf("reopened plans=%+v", l.plans)
	}
}

func TestRound2LocalScheduleIntentSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	path := t.TempDir() + "/schedule-intents.json"
	subs := seed(t, Subscription{ID: "daily", BotID: "daily", Enabled: true, Invocation: "schedule", Cron: "0 9 * * *", Vars: map[string]string{"data": "original"}})
	now := time.Date(2026, 10, 23, 8, 59, 0, 0, time.UTC)
	l := &heldLauncher{hold: true}
	newScheduler := func() *Scheduler {
		return NewScheduler(subs, l, WithSchedulerClock(func() time.Time { return now }), WithSchedulerStateFile(path))
	}
	s := newScheduler()
	s.tick(ctx, now)
	now = now.Add(time.Minute)
	s.tick(ctx, now)
	// No source event or cron slot is recreated after this point.
	s = newScheduler()
	l.hold = false
	now = now.Add(time.Minute)
	s.tick(ctx, now)
	s.tick(ctx, now)
	if len(l.plans) != 1 || l.plans[0].Vars["data"] != "original" {
		t.Fatalf("restart at 09:01: %+v", l.plans)
	}
}

func TestRound2FileOutboxConcurrentDelivery(t *testing.T) {
	out := NewFileEffectOutbox(t.TempDir() + "/effects.json")
	ctx := context.Background()
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := out.UpsertPending(ctx, []EffectRow{{ID: fmt.Sprint(i), State: EffectPending}}); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	rows, err := out.ClaimDue(ctx, time.Now().UTC(), 100)
	if err != nil || len(rows) != 32 {
		t.Fatalf("concurrent writes retained %d/32 effects: %v", len(rows), err)
	}
}
