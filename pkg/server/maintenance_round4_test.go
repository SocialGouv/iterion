package server

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"testing"

	"github.com/SocialGouv/iterion/pkg/schedgate"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/webhooks/prforge"
)

// Count the persisted runs actually created by the launch seam, including
// queued runs, using the same predicate as the cloud counter.
type round4CountingStore struct{ store.RunStore }

func (s round4CountingStore) CountActiveRunsByTenant(ctx context.Context, tenant string) (int, error) {
	ids, err := s.ListRuns(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, id := range ids {
		r, err := s.LoadRun(ctx, id)
		if err != nil {
			return 0, err
		}
		if r.TenantID == tenant && (r.Status == store.RunStatusRunning || r.Status == store.RunStatusQueued) {
			n++
		}
	}
	return n, nil
}
func TestRound4ReplacementWaitsForSlotTransfer(t *testing.T) {
	for _, cancelMode := range []string{"failed", "async", "released"} {
		t.Run(cancelMode, func(t *testing.T) {
			ctx := context.Background()
			s := newWebhookTestServer(t)
			team, err := s.authStore().GetTeam(ctx, "t1")
			if err != nil {
				t.Fatal(err)
			}
			team.MaxConcurrentRuns = 1
			if err := s.authStore().UpdateTeam(ctx, team); err != nil {
				t.Fatal(err)
			}
			runs, err := store.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			counter := round4CountingStore{RunStore: runs}
			s.cfg.Store = counter
			launches, cancels := 0, 0
			s.webhookLaunchBot = func(ctx context.Context, bot string, vars map[string]string, _, _, _ string, _, _ map[string]string) (string, error) {
				launches++
				r, err := runs.CreateRun(ctx, fmt.Sprintf("run-%d", launches), bot, nil)
				if err != nil {
					return "", err
				}
				r.TenantID, r.Status = "t1", store.RunStatusRunning
				return r.ID, runs.SaveRun(ctx, r)
			}
			release := func(id string) error {
				r, err := runs.LoadRun(ctx, id)
				if err != nil {
					return err
				}
				r.Status = store.RunStatusCancelled
				return runs.SaveRun(ctx, r)
			}
			s.webhookCancelRun = func(id string) error {
				cancels++
				switch cancelMode {
				case "failed":
					return errors.New("cancel failed")
				case "released":
					return release(id)
				}
				return nil
			}
			cfg, pt := ghConfig(t, s)
			cfg.ReviewOnSync = true
			cfg.Overlap = schedgate.OverlapSupersede
			if err := s.webhookConfigs.Create(ctx, cfg); err != nil {
				t.Fatal(err)
			}
			deliver := func(sha string) {
				w := httptest.NewRecorder()
				s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), ghSyncPayload(sha), prforge.EventHeaderPullRequest, pt))
			}
			deliver("old-sha")
			if launches != 1 {
				t.Fatal("seed launch missing")
			}
			fence := armTestFence(t, s)
			disarmFence(t, fence)
			flip := &flipMaintenance{Store: fence}
			s.maintenanceStore = flip
			deliver("new-sha")
			active, err := counter.CountActiveRunsByTenant(ctx, "t1")
			if err != nil {
				t.Fatal(err)
			}
			want := 1
			if cancelMode == "released" {
				want = 2
			}
			if active != 1 || launches != want || cancels != 1 || flip.reads != 1 {
				t.Fatalf("active=%d launches=%d cancels=%d maintenance reads=%d, want 1/%d/1/1", active, launches, cancels, flip.reads, want)
			}
			if cancelMode != "released" {
				if err := release("run-1"); err != nil {
					t.Fatal(err)
				}
				s.maintenanceStore = fence
				disarmFence(t, fence)
				deliver("new-sha")
				active, err = counter.CountActiveRunsByTenant(ctx, "t1")
				if err != nil {
					t.Fatal(err)
				}
				if active != 1 || launches != 2 {
					t.Fatalf("retry after release: active=%d launches=%d", active, launches)
				}
			}
		})
	}
}
