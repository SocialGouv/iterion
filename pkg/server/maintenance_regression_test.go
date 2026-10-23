package server

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/platformcfg"
	"github.com/SocialGouv/iterion/pkg/schedgate"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/trigger"
	"github.com/SocialGouv/iterion/pkg/webhooks"
	"github.com/SocialGouv/iterion/pkg/webhooks/prforge"
)

func disarmFence(t *testing.T, st *platformcfg.MemoryStore[platformcfg.Maintenance]) {
	t.Helper()
	if err := st.Put(context.Background(), platformcfg.Maintenance{Reason: "rollout complete"}); err != nil {
		t.Fatal(err)
	}
}

func TestFenceEffectWorkerPreservesPendingUntilDisarmed(t *testing.T) {
	ctx := spineCtx(t)
	pub := &spinePublisher{}
	s := newGatedSpineServer(t, gateSpec{id: "t1"}, pub)
	st := armTestFence(t, s)
	subs := trigger.NewMemorySubscriptionStore()
	sub := trigger.Subscription{ID: "s1", TenantID: "t1", BotID: "probe", Enabled: true,
		Mode: bundle.ExecutionDirect, Match: trigger.Matcher{Sources: []trigger.Source{trigger.SourceCustom}}}
	if err := subs.Create(ctx, sub); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	rows, err := trigger.MaterializeEffects(ctx, subs, nil, spinePlan("t1").Event, now)
	if err != nil || len(rows) != 1 {
		t.Fatalf("materialize = %v, %v", rows, err)
	}
	out := trigger.NewMemoryEffectOutbox()
	if err := out.UpsertPending(ctx, rows); err != nil {
		t.Fatal(err)
	}
	w := trigger.EffectWorker{Outbox: out, Subs: subs,
		Evaluator: trigger.NewEvaluator(subs, trigger.WithLauncher(s.triggerLauncher())), Now: func() time.Time { return now }}
	for i := 0; i < trigger.MaxEffectAttempts+3; i++ {
		if n := w.Tick(ctx, 10); n != 1 {
			t.Fatalf("cycle %d: claimed %d", i, n)
		}
		row, _ := out.Row(rows[0].ID)
		if row.State != trigger.EffectPending || row.Attempts != 0 || pub.count() != 0 {
			t.Fatalf("cycle %d: state=%s attempts=%d published=%d", i, row.State, row.Attempts, pub.count())
		}
		now = row.NotBefore.Add(time.Second)
	}
	disarmFence(t, st)
	w.Tick(ctx, 10)
	row, _ := out.Row(rows[0].ID)
	if row.State != trigger.EffectDone || pub.count() != 1 {
		t.Fatalf("disarmed: state=%s published=%d", row.State, pub.count())
	}
}

func TestFenceBoardPreservesReadyUntilDisarmed(t *testing.T) {
	pub := &countingPublisher{}
	s, rs := newGatedBoardServer(t, gateSpec{id: "t1"}, pub)
	st := armTestFence(t, s)
	f := newFakeBoardCoord(readyCard("native:1", "probe"))
	d := newBoardDispatcher(f, s.processBoardCard, "replica-fence", 4, iterlog.Nop())
	d.launchAttemptCap = 3
	for i := 0; i < d.launchAttemptCap+3; i++ {
		if n := d.tick(boundedCtx(t)); n != 1 {
			t.Fatalf("cycle %d: claimed=%d", i, n)
		}
		d.wg.Wait()
		if f.states["native:1"] != native.StateReady || f.refusals["native:1"] != nil || len(f.gaveUps) != 0 || len(f.claimed) != 0 || pub.count() != 0 {
			t.Fatalf("cycle %d: state=%s refusal=%+v claims=%v publishes=%d", i, f.states["native:1"], f.refusals["native:1"], f.claimed, pub.count())
		}
	}
	disarmFence(t, st)
	pub.onLaunch = func(id string) { finishRunAs(t, rs, id, store.RunStatusFinished) }
	d.tick(boundedCtx(t))
	d.wg.Wait()
	if pub.count() != 1 || f.states["native:1"] != d.doneState {
		t.Fatalf("disarmed: published=%d state=%s", pub.count(), f.states["native:1"])
	}
}

type fenceDeferredStore struct {
	webhooks.DeferredLaunchStore
	attempts []int
	deleted  int
}

func (s *fenceDeferredStore) Reschedule(ctx context.Context, key string, generation int64, at time.Time, attempts int) error {
	s.attempts = append(s.attempts, attempts)
	return s.DeferredLaunchStore.Reschedule(ctx, key, generation, at, attempts)
}

func (s *fenceDeferredStore) Delete(ctx context.Context, key string, generation int64) error {
	s.deleted++
	return s.DeferredLaunchStore.Delete(ctx, key, generation)
}

func TestFenceDeferredWebhookPreservesPendingUntilDisarmed(t *testing.T) {
	s := newWebhookTestServer(t)
	deferred := &fenceDeferredStore{DeferredLaunchStore: webhooks.NewMemoryDeferredLaunchStore()}
	s.webhookDeferred = deferred
	s.syncDebounce = time.Minute
	got := fanoutLauncher(s)
	cfg, pt := ghConfig(t, s)
	cfg.ReviewOnSync = true
	if err := s.webhookConfigs.Create(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), ghSyncPayload("sha-1"), prforge.EventHeaderPullRequest, pt))
	if w.Code != 202 {
		t.Fatalf("park: %d %s", w.Code, w.Body.String())
	}
	st := armTestFence(t, s)
	now := time.Now().UTC().Add(time.Hour)
	for i := 0; i < webhookDeferMaxAttempts+3; i++ {
		s.sweepDeferredWebhookLaunches(context.Background(), now)
		if len(deferred.attempts) != i+1 || deferred.attempts[i] != 0 || deferred.deleted != 0 || len(*got) != 0 {
			t.Fatalf("cycle %d: attempts=%v deleted=%d launches=%d", i, deferred.attempts, deferred.deleted, len(*got))
		}
		now = now.Add(time.Hour)
	}
	disarmFence(t, st)
	s.sweepDeferredWebhookLaunches(context.Background(), now)
	if len(*got) != 1 || deferred.deleted != 1 {
		t.Fatalf("disarmed: launches=%d deleted=%d", len(*got), deferred.deleted)
	}
	s.sweepDeferredWebhookLaunches(context.Background(), now.Add(time.Hour))
	if len(*got) != 1 {
		t.Fatal("duplicate launch after release")
	}
}

func TestFenceSupersedePreservesLiveRun(t *testing.T) {
	for _, deferred := range []bool{false, true} {
		name := "immediate"
		if deferred {
			name = "deferred"
		}
		t.Run(name, func(t *testing.T) {
			s := newWebhookTestServer(t)
			got := fanoutLauncher(s)
			cancelled := 0
			s.webhookCancelRun = func(string) error { cancelled++; return nil }
			cfg, pt := ghConfig(t, s)
			cfg.ReviewOnSync = true
			cfg.Overlap = schedgate.OverlapSupersede
			if err := s.webhookConfigs.Create(context.Background(), cfg); err != nil {
				t.Fatal(err)
			}
			deliver := func(sha string) *httptest.ResponseRecorder {
				w := httptest.NewRecorder()
				s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), ghSyncPayload(sha), prforge.EventHeaderPullRequest, pt))
				return w
			}
			deliver("old-sha")
			if len(*got) != 1 {
				t.Fatalf("seed launches=%d", len(*got))
			}
			if deferred {
				s.webhookDeferred = webhooks.NewMemoryDeferredLaunchStore()
				s.syncDebounce = time.Minute
			}
			st := armTestFence(t, s)
			w := deliver("new-sha")
			if cancelled != 0 || len(*got) != 1 {
				t.Fatalf("armed: cancelled=%d launches=%d", cancelled, len(*got))
			}
			if deferred && w.Code != 202 {
				t.Fatalf("park: %d %s", w.Code, w.Body.String())
			}
			disarmFence(t, st)
			if deferred {
				s.sweepDeferredWebhookLaunches(context.Background(), time.Now().Add(time.Hour))
			} else {
				deliver("new-sha")
			}
			if cancelled != 1 || len(*got) != 2 {
				t.Fatalf("disarmed: cancelled=%d launches=%d", cancelled, len(*got))
			}
		})
	}
}

// The cloud bootstrap needs Mongo/NATS; pin its actual server.New argument
// separately from the constructor/API integration test. Removing the production
// MaintenanceSettings field must fail even if Config-based tests still pass.
func TestFenceCloudBootstrapWiresMaintenance(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "../../cmd/iterion/server.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	wired := false
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		fn, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || fn.Sel.Name != "New" {
			return true
		}
		pkg, ok := fn.X.(*ast.Ident)
		if !ok || pkg.Name != "server" {
			return true
		}
		cfg, ok := call.Args[0].(*ast.CompositeLit)
		if !ok {
			return true
		}
		for _, elt := range cfg.Elts {
			kv, ok := elt.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			key, ok := kv.Key.(*ast.Ident)
			if !ok || key.Name != "MaintenanceSettings" {
				continue
			}
			v, ok := kv.Value.(*ast.SelectorExpr)
			if !ok || v.Sel.Name != "maintenance" {
				continue
			}
			obj, ok := v.X.(*ast.Ident)
			wired = ok && obj.Name == "stores"
		}
		return true
	})
	if !wired {
		t.Fatal("cloud server.New must receive MaintenanceSettings: stores.maintenance")
	}
}
