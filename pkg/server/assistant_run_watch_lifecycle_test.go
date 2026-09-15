package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/eventbus"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/runwatch"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/trigger"
)

type gatedWatchSweep struct {
	runwatch.Store
	entered   chan int
	release   chan struct{}
	calls     atomic.Int32
	completed chan string
}

func (s *gatedWatchSweep) ActiveWatchUpperBound(ctx context.Context) (*runwatch.WatchCursor, error) {
	upper, err := s.Store.ActiveWatchUpperBound(ctx)
	n := int(s.calls.Add(1))
	s.entered <- n
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.release:
	}
	return upper, err
}
func (s *gatedWatchSweep) CompleteEpisode(ctx context.Context, id, worker string, now time.Time) error {
	err := s.Store.CompleteEpisode(ctx, id, worker, now)
	if err == nil {
		s.completed <- id
	}
	return err
}
func awaitWatchSignal[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(5 * time.Second):
		t.Fatal("watch worker did not reach barrier")
		var zero T
		return zero
	}
}

func TestAssistantWatchTerminalPostBurstUsesBoundedWorker(t *testing.T) {
	f := newArmFixture(t, true)
	a := f.assistant(t, "burst-assistant", "unrelated-card")
	var targets []*store.Run
	for i, status := range []store.RunStatus{store.RunStatusFailed, store.RunStatusFinished, store.RunStatusCancelled} {
		target := f.target(t, fmt.Sprintf("burst-target-%d", i), "target-card", a.CreatedAt.Add(time.Minute))
		target.Status = status
		if err := f.rs.SaveRun(t.Context(), target); err != nil {
			t.Fatal(err)
		}
		targets = append(targets, target)
	}
	gated := &gatedWatchSweep{Store: f.ws, entered: make(chan int, 10), release: make(chan struct{}), completed: make(chan string, 10)}
	f.coord.wake = make(chan struct{}, 1)
	f.coord.setRuntime(f.srv.runs, gated, f.srv.cfg.Bots.Paths)
	f.srv.assistantWatch = f.coord
	var deliveries atomic.Int32
	f.coord.resumeRun = func(context.Context, runview.ResumeSpec) (*runview.LaunchResult, error) {
		deliveries.Add(1)
		return &runview.LaunchResult{}, nil
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); f.coord.sweepLoop(ctx, nil) }()
	t.Cleanup(func() { cancel(); awaitWatchSignal(t, done) })
	if n := awaitWatchSignal(t, gated.entered); n != 1 {
		t.Fatalf("initial pass %d", n)
	}
	for n := 0; n < 60; n++ {
		target := targets[n%len(targets)]
		req := httptest.NewRequest(http.MethodPost, "/api/runs/"+target.ID+"/watching", strings.NewReader(`{"assistant_run_id":"burst-assistant","kinds":["run.failed","run.finished","run.cancelled"]}`))
		req.SetPathValue("id", target.ID)
		rec := httptest.NewRecorder()
		f.srv.handleCreateAssistantWatch(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("POST %d: %d %s", n, rec.Code, rec.Body.String())
		}
	}
	if len(f.coord.wake) != 1 || deliveries.Load() != 0 || gated.calls.Load() != 1 {
		t.Fatalf("unbounded continuation: wake=%d deliveries=%d sweeps=%d", len(f.coord.wake), deliveries.Load(), gated.calls.Load())
	}
	watches, err := f.ws.ListActive(t.Context(), 10)
	if err != nil || len(watches) != 3 {
		t.Fatalf("durable watches=%v %v", watches, err)
	}
	for _, w := range watches {
		eps, _ := f.ws.ListEpisodesByWatch(t.Context(), w.ID, "", 10)
		if len(eps) != 0 {
			t.Fatalf("detached observer created %+v", eps)
		}
	}
	gated.release <- struct{}{}
	if n := awaitWatchSignal(t, gated.entered); n != 2 {
		t.Fatalf("coalesced pass=%d", n)
	}
	gated.release <- struct{}{}
	for range 3 {
		awaitWatchSignal(t, gated.completed)
	}
	cancel()
	awaitWatchSignal(t, done)
	// Reconciliation and exact intent retries preserve the stable outcome keys.
	f.coord.setRuntime(f.srv.runs, f.ws, f.srv.cfg.Bots.Paths)
	f.coord.sweep(t.Context())
	if deliveries.Load() != 3 {
		t.Fatalf("duplicate deliveries: %d", deliveries.Load())
	}
	for _, w := range watches {
		eps, err := f.ws.ListEpisodesByWatch(t.Context(), w.ID, "", 10)
		target, loadErr := f.rs.LoadRun(t.Context(), w.TargetRunID)
		if err != nil || loadErr != nil || len(eps) != 1 || eps[0].State != runwatch.EpisodeDone {
			t.Fatalf("completion: %+v %v %v", eps, err, loadErr)
		}
		want := trigger.RunOutcomeEventID(target.ID, string(target.Status), "", target.UpdatedAt)
		if eps[0].OutcomeEventID != want {
			t.Fatalf("outcome key=%s want=%s", eps[0].OutcomeEventID, want)
		}
	}
}

type blockingWatchRunLoad struct {
	store.RunStore
	target        string
	entered       chan struct{}
	release       chan struct{}
	ignoreContext bool
	armed         atomic.Bool
	err           error
}

func (s *blockingWatchRunLoad) LoadRun(ctx context.Context, id string) (*store.Run, error) {
	if id != s.target || !s.armed.CompareAndSwap(true, false) {
		return s.RunStore.LoadRun(ctx, id)
	}
	close(s.entered)
	if s.ignoreContext {
		<-s.release
	} else {
		<-ctx.Done()
	}
	if s.err != nil {
		return nil, s.err
	}
	return s.RunStore.LoadRun(context.WithoutCancel(ctx), id)
}

type signaledWatchSweep struct {
	runwatch.Store
	firstPass chan struct{}
	once      sync.Once
}

func (s *signaledWatchSweep) ListDueEpisodes(ctx context.Context, now time.Time, limit int) ([]runwatch.Episode, error) {
	out, err := s.Store.ListDueEpisodes(ctx, now, limit)
	s.once.Do(func() { close(s.firstPass) })
	return out, err
}

func TestAssistantWatchShutdownBoundsJoinAndStopsFollowOnEffects(t *testing.T) {
	for _, later := range []bool{false, true} {
		for _, ignore := range []bool{false, true} {
			t.Run(fmt.Sprintf("later=%v/ignores-context=%v", later, ignore), func(t *testing.T) {
				f := newArmFixture(t, true)
				a := f.assistant(t, "shutdown-assistant", "unrelated-card")
				target := f.target(t, "shutdown-target", "target-card", a.CreatedAt.Add(time.Minute))
				blocker := &blockingWatchRunLoad{RunStore: f.rs, target: target.ID, entered: make(chan struct{}), release: make(chan struct{}), ignoreContext: ignore, err: store.ErrRunNotFound}
				// Construct the service before enabling the barrier: startup itself reads runs.
				f.srv.runs = newTestRunviewService(t, "", runview.WithStore(blocker))
				f.srv.hub = NewHub(f.srv.logger)
				f.srv.cfg.EventsBus = eventbus.NewInProcBus(f.srv.logger)
				f.srv.shutdown = make(chan struct{})
				httpDrained := make(chan struct{})
				f.srv.server = &http.Server{}
				f.srv.server.RegisterOnShutdown(sync.OnceFunc(func() { close(httpDrained) }))
				w := runwatch.Watch{ID: "shutdown-watch", TargetRunID: target.ID, AssistantRunID: a.ID, State: runwatch.WatchActive, Kinds: []string{trigger.KindRunFailed}, CreatedAt: time.Now().UTC()}
				if !later {
					if err := f.ws.CreateWatch(t.Context(), w); err != nil {
						t.Fatal(err)
					}
				}
				signaled := &signaledWatchSweep{Store: f.ws, firstPass: make(chan struct{})}
				f.srv.assistantWatches = signaled
				blocker.armed.Store(!later)
				f.srv.startAssistantRunWatches()
				original := f.srv.assistantWatch
				done := f.srv.assistantWatchDone
				f.srv.startAssistantRunWatches()
				if f.srv.assistantWatch != original || f.srv.assistantWatchDone != done {
					t.Fatal("start created another worker")
				}
				if later {
					// Ensure the initial sweep has passed all reads before arming
					// the later-wake barrier.
					awaitWatchSignal(t, signaled.firstPass)
					blocker.armed.Store(true)
					if err := f.ws.CreateWatch(t.Context(), w); err != nil {
						t.Fatal(err)
					}
					f.srv.assistantWatch.nudge()
				}
				awaitWatchSignal(t, blocker.entered)
				// A real bus unsubscribe waits for its callback. An outcome burst
				// must never make that callback another blocked store observer.
				for range 100 {
					if err := f.srv.cfg.EventsBus.Publish(t.Context(), trigger.Event{Source: trigger.SourceRun, Kind: trigger.KindRunFailed, Subject: trigger.Subject{ID: target.ID}}); err != nil {
						t.Fatal(err)
					}
				}
				start := time.Now()
				if err := f.srv.Shutdown(t.Context()); err != nil {
					t.Fatal(err)
				}
				if elapsed := time.Since(start); elapsed > time.Second {
					t.Fatalf("shutdown blocked for %s", elapsed)
				}
				awaitWatchSignal(t, httpDrained)
				if ignore {
					select {
					case <-done:
						t.Fatal("context-ignoring dependency unexpectedly exited")
					default:
					}
					close(blocker.release)
				}
				awaitWatchSignal(t, done)
				got, err := f.ws.GetWatch(t.Context(), w.ID)
				if err != nil || got.State != runwatch.WatchActive {
					t.Fatalf("cancellation became target_missing: %+v %v", got, err)
				}
				eps, _ := f.ws.ListEpisodesByWatch(t.Context(), w.ID, "", 10)
				if len(eps) != 0 {
					t.Fatalf("follow-on episode: %+v", eps)
				}
				// The watch hook/join handles are consumed exactly once.
				if f.srv.assistantWatchCancel != nil || f.srv.assistantWatchDone != nil {
					t.Fatal("worker lifecycle retained after shutdown")
				}
				if err := f.srv.Shutdown(t.Context()); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

type canceledWatchLookup struct {
	runwatch.Store
	err error
}

func (s canceledWatchLookup) GetWatch(context.Context, string) (runwatch.Watch, error) {
	return runwatch.Watch{}, s.err
}
func TestAssistantWatchCanceledLookupDoesNotBlockEpisode(t *testing.T) {
	for _, err := range []error{context.Canceled, context.DeadlineExceeded} {
		t.Run(err.Error(), func(t *testing.T) {
			f := newArmFixture(t, true)
			now := time.Now().UTC()
			ep := runwatch.Episode{ID: "cancel-episode", WatchID: "watch", State: runwatch.EpisodePending, NextAttemptAt: now, CreatedAt: now}
			if _, e := f.ws.CreateEpisode(t.Context(), ep); e != nil {
				t.Fatal(e)
			}
			f.coord.setRuntime(f.srv.runs, canceledWatchLookup{f.ws, err}, nil)
			f.coord.attempt(t.Context(), ep.ID)
			episodes, e := f.ws.ListEpisodesByWatch(t.Context(), ep.WatchID, "", 10)
			if e != nil || len(episodes) != 1 || episodes[0].State == runwatch.EpisodeBlocked || episodes[0].LastError != "" {
				t.Fatalf("cancellation treated as episode failure: %+v %v", episodes, e)
			}
			if !assistantWatchCanceled(t.Context(), fmt.Errorf("wrapped: %w", err)) {
				t.Fatal("wrapped cancellation not recognized")
			}
		})
	}
	if assistantWatchCanceled(t.Context(), errors.New("other")) {
		t.Fatal("ordinary error classified as cancellation")
	}
}
