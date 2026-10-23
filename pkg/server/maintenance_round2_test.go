package server

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/budgetfloor"
	"github.com/SocialGouv/iterion/pkg/identity"
	"github.com/SocialGouv/iterion/pkg/platformcfg"
	"github.com/SocialGouv/iterion/pkg/schedgate"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/webhooks"
	"github.com/SocialGouv/iterion/pkg/webhooks/prforge"
)

type flipMaintenance struct {
	platformcfg.Store[platformcfg.Maintenance]
	reads int
}

func (s *flipMaintenance) Get(ctx context.Context) (*platformcfg.Maintenance, error) {
	s.reads++
	rec, err := s.Store.Get(ctx)
	if s.reads == 1 {
		if err := s.Put(ctx, platformcfg.Maintenance{FenceLaunches: true, Reason: "armed after first read"}); err != nil {
			return nil, err
		}
	}
	return rec, err
}

type lookupBarrier struct {
	webhooks.DeliveryStore
	entered, release chan struct{}
}

func (s *lookupBarrier) ListByWebhook(ctx context.Context, tenant, id string, limit int) ([]webhooks.Delivery, error) {
	close(s.entered)
	select {
	case <-s.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return s.DeliveryStore.ListByWebhook(ctx, tenant, id, limit)
}
func TestRound2WebhookAdmissionBeforeCancellation(t *testing.T) {
	for _, mode := range []string{"first-read", "lookup-barrier", "debounce", "debounce-first-read"} {
		t.Run(mode, func(t *testing.T) {
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
			deliver := func(sha string) {
				w := httptest.NewRecorder()
				s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), ghSyncPayload(sha), prforge.EventHeaderPullRequest, pt))
			}
			deliver("old-sha")
			if len(*got) != 1 {
				t.Fatal("seed launch missing")
			}
			st := armTestFence(t, s)
			disarmFence(t, st)
			switch mode {
			case "first-read":
				s.maintenanceStore = &flipMaintenance{Store: st}
				deliver("new-sha")
			case "lookup-barrier":
				b := &lookupBarrier{DeliveryStore: s.webhookDeliveries, entered: make(chan struct{}), release: make(chan struct{})}
				s.webhookDeliveries = b
				done := make(chan struct{})
				go func() { defer close(done); deliver("new-sha") }()
				select {
				case <-b.entered:
				case <-time.After(5 * time.Second):
					t.Fatal("no predecessor lookup")
				}
				if err := st.Put(context.Background(), platformcfg.Maintenance{FenceLaunches: true, Reason: "armed during lookup"}); err != nil {
					t.Fatal(err)
				}
				close(b.release)
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					t.Fatal("delivery stuck")
				}
			case "debounce", "debounce-first-read":
				s.webhookDeferred = webhooks.NewMemoryDeferredLaunchStore()
				s.syncDebounce = time.Minute
				deliver("new-sha")
				if cancelled != 0 {
					t.Fatalf("park cancelled predecessor before replacement admission: %d", cancelled)
				}
				if mode == "debounce-first-read" {
					s.maintenanceStore = &flipMaintenance{Store: st}
				} else {
					if err := st.Put(context.Background(), platformcfg.Maintenance{FenceLaunches: true, Reason: "armed before sweep"}); err != nil {
						t.Fatal(err)
					}
				}
				s.sweepDeferredWebhookLaunches(context.Background(), time.Now().Add(time.Hour))
			}
			if cancelled > 0 && len(*got) != 2 {
				t.Fatalf("cancelled=%d but launches=%d: replacement refused after cancellation", cancelled, len(*got))
			}
			if mode == "first-read" || mode == "debounce-first-read" {
				flip := s.maintenanceStore.(*flipMaintenance)
				if flip.reads != 1 || cancelled != 1 || len(*got) != 2 {
					t.Fatalf("admission must retain its single decision: reads=%d cancelled=%d launches=%d", flip.reads, cancelled, len(*got))
				}
			} else if cancelled != 0 || len(*got) != 1 {
				t.Fatalf("held replacement disturbed predecessor: cancelled=%d launches=%d", cancelled, len(*got))
			}
		})
	}
}

type replacementRunStore struct {
	fakeActiveStore
	run *store.Run
}

func (s replacementRunStore) LoadRun(context.Context, string) (*store.Run, error) { return s.run, nil }
func TestRound2ReplacementCreditsOnlyLiveTenantSlot(t *testing.T) {
	for _, tc := range []struct {
		name, tenant string
		status       store.RunStatus
		allow        bool
	}{
		{"live", "t1", store.RunStatusRunning, true},
		{"queued", "t1", store.RunStatusQueued, true},
		{"other-tenant", "t2", store.RunStatusRunning, false},
		{"finished", "t1", store.RunStatusFinished, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{cfg: Config{Store: replacementRunStore{fakeActiveStore: fakeActiveStore{active: 1}, run: &store.Run{TenantID: tc.tenant, Status: tc.status}}}}
			d := s.gateConcurrency(context.Background(), identity.Team{ID: "t1", MaxConcurrentRuns: 1}, budgetfloor.Policy{}, launchSubject{ReplacingRunIDs: []string{"old", "old"}})
			if (d == nil) != tc.allow {
				t.Fatalf("denial=%+v want allow=%v", d, tc.allow)
			}
		})
	}
}
