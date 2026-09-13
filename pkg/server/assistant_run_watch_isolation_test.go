package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/runwatch"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/trigger"
)

// This adapter models Mongo's fail-closed query boundary. Only candidate
// enumeration/loading may be privileged; reverse queries and events may not.
type tenantWatchRunStore struct {
	store.RunStore
	t               *testing.T
	privilegedLists int
	privilegedLoads int
}

func (s *tenantWatchRunStore) scoped(ctx context.Context) string {
	s.t.Helper()
	tenant, ok := store.TenantFromContext(ctx)
	if !ok || store.IsWithoutTenantFilter(ctx) {
		s.t.Fatalf("tenant-scoped operation received an unscoped/privileged context")
	}
	return tenant
}

func (s *tenantWatchRunStore) LoadRun(ctx context.Context, id string) (*store.Run, error) {
	r, err := s.RunStore.LoadRun(ctx, id)
	if store.IsWithoutTenantFilter(ctx) {
		s.privilegedLoads++
		return r, err
	}
	tenant := s.scoped(ctx)
	if err == nil && r.TenantID != tenant {
		return nil, store.ErrRunNotFound
	}
	return r, err
}

func (s *tenantWatchRunStore) filter(ctx context.Context, ids []string) []string {
	tenant := s.scoped(ctx)
	var out []string
	for _, id := range ids {
		r, err := s.RunStore.LoadRun(ctx, id)
		if err == nil && r.TenantID == tenant {
			out = append(out, id)
		}
	}
	return out
}

func (s *tenantWatchRunStore) ListRuns(ctx context.Context) ([]string, error) {
	ids, err := s.RunStore.ListRuns(ctx)
	if store.IsWithoutTenantFilter(ctx) {
		s.privilegedLists++
		return ids, err
	}
	return s.filter(ctx, ids), err
}

func (s *tenantWatchRunStore) ListRunsBySourceIssue(ctx context.Context, issue string) ([]string, error) {
	ids, err := s.RunStore.ListRunsBySourceIssue(ctx, issue)
	return s.filter(ctx, ids), err
}

func (s *tenantWatchRunStore) ListChildRuns(ctx context.Context, parent string) ([]string, error) {
	ids, err := s.RunStore.ListChildRuns(ctx, parent)
	return s.filter(ctx, ids), err
}

func (s *tenantWatchRunStore) AppendEvent(ctx context.Context, id string, ev store.Event) (*store.Event, error) {
	if _, err := s.LoadRun(ctx, id); err != nil {
		return nil, err
	}
	return s.RunStore.AppendEvent(ctx, id, ev)
}

func (s *tenantWatchRunStore) LoadEventsRange(ctx context.Context, id string, from, to int64, limit int) ([]*store.Event, error) {
	if _, err := s.LoadRun(ctx, id); err != nil {
		return nil, err
	}
	return s.RunStore.LoadEventsRange(ctx, id, from, to, limit)
}

type tenantWatchStore struct {
	runwatch.Store
	runs *tenantWatchRunStore
}

func (s tenantWatchStore) CreateWatch(ctx context.Context, w runwatch.Watch) error {
	if s.runs.scoped(ctx) != w.TenantID {
		s.runs.t.Fatal("foreign watch write")
	}
	return s.Store.CreateWatch(ctx, w)
}

func (s tenantWatchStore) CreateEpisode(ctx context.Context, ep runwatch.Episode) (bool, error) {
	if s.runs.scoped(ctx) != ep.TenantID {
		s.runs.t.Fatal("foreign episode write")
	}
	return s.Store.CreateEpisode(ctx, ep)
}

func TestAssistantWatchCloudTenantIsolation(t *testing.T) {
	for _, path := range []string{"sweep", "event"} {
		t.Run(path, func(t *testing.T) {
			f := newArmFixture(t, true)
			var targets []*store.Run
			for _, tenant := range []string{"A", "B"} {
				a := f.assistant(t, "assistant-"+tenant, "shared-card-id")
				r := f.target(t, "target-"+tenant, "shared-card-id", a.CreatedAt.Add(time.Minute))
				for _, run := range []*store.Run{a, r} {
					run.TenantID, run.OwnerID = tenant, "owner-"+tenant
					if err := f.rs.SaveRun(t.Context(), run); err != nil {
						t.Fatal(err)
					}
				}
				targets = append(targets, r)
			}
			// A malformed cloud candidate must never reach a tenant operation.
			f.assistant(t, "missing-tenant", "shared-card-id")
			strict := &tenantWatchRunStore{RunStore: f.rs, t: t}
			f.srv.runs = newTestRunviewService(t, "", runview.WithStore(strict))
			strict.privilegedLists, strict.privilegedLoads = 0, 0
			f.srv.cfg.Mode = "cloud"
			f.coord.setRuntime(f.srv.runs, tenantWatchStore{f.ws, strict}, f.srv.cfg.Bots.Paths)
			deliveries := map[string]int{}
			f.coord.resumeRun = func(ctx context.Context, spec runview.ResumeSpec) (*runview.LaunchResult, error) {
				tenant := strict.scoped(ctx)
				if spec.RunID != "assistant-"+tenant {
					t.Fatalf("foreign delivery: %s / %s", tenant, spec.RunID)
				}
				deliveries[tenant]++
				return &runview.LaunchResult{}, nil
			}
			if path == "sweep" {
				f.coord.sweep(t.Context())
			} else {
				for _, target := range targets {
					ev := trigger.Event{Source: trigger.SourceRun, Kind: trigger.KindRunFailed, TenantID: target.TenantID,
						ID: trigger.RunOutcomeEventID(target.ID, string(target.Status), "", target.UpdatedAt)}
					ev.Subject.ID = target.ID
					if err := f.coord.handleEvent(t.Context(), ev); err != nil {
						t.Fatal(err)
					}
				}
			}
			watches, err := f.ws.ListActive(t.Context(), 10)
			if err != nil || len(watches) != 2 {
				t.Fatalf("watches = %+v, %v", watches, err)
			}
			for _, w := range watches {
				if w.AssistantRunID != "assistant-"+w.TenantID || w.TargetRunID != "target-"+w.TenantID {
					t.Fatalf("cross-tenant watch: %+v", w)
				}
			}
			for _, tenant := range []string{"A", "B"} {
				if deliveries[tenant] != 1 {
					t.Fatalf("deliveries: %v", deliveries)
				}
			}
			if path == "sweep" && (strict.privilegedLists != 1 || strict.privilegedLoads != 5) {
				t.Fatalf("discovery calls: %d lists, %d loads", strict.privilegedLists, strict.privilegedLoads)
			}
			if path == "event" && (strict.privilegedLists != 0 || strict.privilegedLoads != 0) {
				t.Fatal("event processing used global discovery")
			}
		})
	}
}

type blockedWatchList struct {
	runwatch.Store
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *blockedWatchList) ListActivePage(ctx context.Context, after *runwatch.WatchCursor, through runwatch.WatchCursor, limit int) ([]runwatch.Watch, error) {
	s.once.Do(func() { close(s.entered); <-s.release })
	return s.Store.ListActivePage(ctx, after, through, limit)
}

func TestAssistantWatchInFlightProjectSnapshot(t *testing.T) {
	f := newArmFixture(t, true)
	a := f.assistant(t, "assistant-old", "card")
	target := f.target(t, "target-old", "card", a.CreatedAt.Add(time.Minute))
	f.coord.armWatchesForTarget(t.Context(), target)
	blocked := &blockedWatchList{Store: f.ws, entered: make(chan struct{}), release: make(chan struct{})}
	f.coord.setRuntime(f.srv.runs, blocked, f.srv.cfg.Bots.Paths)
	f.srv.assistantWatch = f.coord
	resumed := make(chan string, 1)
	f.coord.resumeRun = func(_ context.Context, spec runview.ResumeSpec) (*runview.LaunchResult, error) {
		resumed <- spec.RunID
		return &runview.LaunchResult{}, nil
	}
	old := f.srv.captureAssistantWatch() // same capture used by an HTTP continuation
	// Future projects use their own default catalog. The suspended operation
	// must keep the already-captured catalog containing its chat manifest.
	f.srv.cfg.Bots.Paths = nil
	done := make(chan struct{})
	go func() { defer close(done); f.coord.sweep(t.Context()) }()
	<-blocked.entered
	// Complete overlapping real project swaps while the old sweep holds only
	// its captured service/store. No global lock is held during store I/O.
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		dir := t.TempDir()
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := f.srv.swapWorkDir(t.Context(), dir); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	t.Cleanup(func() { f.srv.runs.Stop(context.Background()) })
	f.srv.stateMu.RLock()
	current := f.coord.snapshot()
	if current.runs != f.srv.runs || current.store != f.srv.assistantWatches {
		t.Error("project runtime published out of order")
	}
	f.srv.stateMu.RUnlock()
	close(blocked.release)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("old sweep did not finish")
	}
	select {
	case id := <-resumed:
		if id != a.ID {
			t.Fatal(id)
		}
	default:
		t.Fatal("old runtime could not resume its assistant")
	}
	watches, _ := f.ws.ListActiveByTarget(t.Context(), "", target.ID)
	if len(watches) != 1 {
		t.Fatal("old watch was falsely stopped as target_missing")
	}
	if !current.origin.heartbeat().CompletedAt.IsZero() {
		t.Fatal("old sweep marked the new runtime healthy")
	}
	if _, err := old.runs.LoadRunCtx(t.Context(), target.ID); err != nil {
		t.Fatalf("old HTTP continuation lost its run: %v", err)
	}
	if got, _ := current.store.ListActive(t.Context(), 10); len(got) != 0 {
		t.Fatalf("old sweep wrote new project: %+v", got)
	}
	ev := trigger.Event{Source: trigger.SourceRun, Kind: trigger.KindRunFailed, ID: trigger.RunOutcomeEventID(target.ID, string(target.Status), "", target.UpdatedAt)}
	ev.Subject.ID = target.ID
	if err := f.coord.handleEvent(t.Context(), ev); err != nil {
		t.Fatalf("late old-project event: %v", err)
	}
	if got, _ := current.store.ListActive(t.Context(), 10); len(got) != 0 {
		t.Fatal("late event changed the new project")
	}
	// Returning to the original service/store permits durable reconciliation.
	f.coord.setRuntime(old.runs, old.store, old.botPaths)
	f.coord.reconcileArmedWatches(t.Context())
	if got, _ := old.store.ListActive(t.Context(), 10); len(got) != 1 {
		t.Fatal(fmt.Sprint(got))
	}
}

type blockedWatchCreate struct {
	runwatch.Store
	entered, release chan struct{}
}

func (s *blockedWatchCreate) CreateWatch(ctx context.Context, w runwatch.Watch) error {
	if err := s.Store.CreateWatch(ctx, w); err != nil {
		return err
	}
	close(s.entered)
	<-s.release
	return nil
}

func TestAssistantWatchHTTPContinuationKeepsProject(t *testing.T) {
	f := newArmFixture(t, true)
	a := f.assistant(t, "http-assistant", "card")
	target := f.target(t, "http-target", "card", a.CreatedAt.Add(time.Minute))
	blocked := &blockedWatchCreate{Store: f.ws, entered: make(chan struct{}), release: make(chan struct{})}
	f.srv.assistantWatches = blocked
	f.srv.assistantWatch = f.coord
	f.coord.setRuntime(f.srv.runs, blocked, f.srv.cfg.Bots.Paths)
	f.coord.resumeRun = func(_ context.Context, spec runview.ResumeSpec) (*runview.LaunchResult, error) {
		if spec.RunID != a.ID {
			return nil, fmt.Errorf("foreign resume: %s", spec.RunID)
		}
		return &runview.LaunchResult{}, nil
	}
	req := httptest.NewRequest(http.MethodPost, "/api/runs/http-target/watching", strings.NewReader(`{"assistant_run_id":"http-assistant"}`))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", target.ID)
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { defer close(done); f.srv.handleCreateAssistantWatch(rec, req) }()
	select {
	case <-blocked.entered:
	case <-done:
		t.Fatalf("request never reached create: %d %s", rec.Code, rec.Body.String())
	case <-time.After(10 * time.Second):
		t.Fatal("create stuck")
	}
	if err := f.srv.swapWorkDir(t.Context(), t.TempDir()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.srv.runs.Stop(context.Background()) })
	close(blocked.release)
	<-done
	if rec.Code != http.StatusOK {
		t.Fatalf("HTTP create: %d %s", rec.Code, rec.Body.String())
	}
	watches, err := f.ws.ListActive(t.Context(), 10)
	if err != nil || len(watches) != 1 {
		t.Fatalf("old watches: %+v %v", watches, err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		episodes, err := f.ws.ListEpisodesByWatch(t.Context(), watches[0].ID, "", 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(episodes) == 1 && episodes[0].State == runwatch.EpisodeDone {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("HTTP continuation did not deliver in the old project: %+v", episodes)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got, _ := f.srv.assistantWatches.ListActive(t.Context(), 10); len(got) != 0 {
		t.Fatal("HTTP continuation wrote into the new project")
	}
}

func TestAssistantWatchSwitchPreservesInjectedStore(t *testing.T) {
	f := newArmFixture(t, true)
	f.srv.cfg.RunWatches = f.ws
	f.srv.assistantWatch = f.coord
	if err := f.srv.swapWorkDir(t.Context(), t.TempDir()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.srv.runs.Stop(context.Background()) })
	current := f.coord.snapshot()
	if current.store != f.ws || current.runs != f.srv.runs {
		t.Fatal("injected watch store was replaced or paired with the previous service")
	}
}
