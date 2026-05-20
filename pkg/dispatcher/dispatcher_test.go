package dispatcher

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dispatcher/tracker"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// fakeTracker is a minimal in-memory Tracker used by the dispatcher
// tests. Safe for concurrent use.
type fakeTracker struct {
	mu        sync.Mutex
	issues    map[string]*tracker.Issue
	claims    map[string]string
	listCalls atomic.Int64
}

func newFakeTracker() *fakeTracker {
	return &fakeTracker{
		issues: map[string]*tracker.Issue{},
		claims: map[string]string{},
	}
}

func (f *fakeTracker) add(iss tracker.Issue) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if iss.CreatedAt.IsZero() {
		iss.CreatedAt = time.Now()
	}
	f.issues[iss.ID] = &iss
}

func (f *fakeTracker) Name() string { return "fake" }

func (f *fakeTracker) ListCandidates(_ context.Context) ([]tracker.Issue, error) {
	f.listCalls.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]tracker.Issue, 0, len(f.issues))
	for id, iss := range f.issues {
		if iss.WorkflowState != "ready" {
			continue
		}
		if _, claimed := f.claims[id]; claimed {
			continue
		}
		out = append(out, *iss)
	}
	return out, nil
}

func (f *fakeTracker) RefreshStates(_ context.Context, ids []string) (map[string]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[string]string, len(ids))
	for _, id := range ids {
		if iss, ok := f.issues[id]; ok {
			out[id] = iss.WorkflowState
		}
	}
	return out, nil
}

func (f *fakeTracker) UpdateState(_ context.Context, id, newState string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	iss, ok := f.issues[id]
	if !ok {
		return tracker.ErrNotFound
	}
	iss.WorkflowState = newState
	return nil
}

func (f *fakeTracker) Comment(_ context.Context, _, _ string) error { return tracker.ErrNotSupported }

func (f *fakeTracker) Claim(_ context.Context, id, marker string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if cur, ok := f.claims[id]; ok && cur != marker {
		return tracker.ErrClaimConflict
	}
	f.claims[id] = marker
	return nil
}

func (f *fakeTracker) Release(_ context.Context, id, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.claims, id)
	return nil
}

// newTestDispatcher builds a Dispatcher with a tmpDir-rooted workspace,
// a quiet logger, and a fake tracker.
func newTestDispatcher(t *testing.T, runner Runner, ft *fakeTracker, polling time.Duration) *Dispatcher {
	t.Helper()
	dir := t.TempDir()
	wsDir := dir + "/ws"
	cfg := &Config{
		Name:      "test",
		Workflow:  t.TempDir() + "/fake.iter",
		Tracker:   TrackerConfig{Kind: "fake"},
		Polling:   PollingConfig{IntervalMS: int(polling.Milliseconds())},
		Agent:     AgentConfig{MaxConcurrent: 4, MaxRetryBackoffMS: 1000},
		Workspace: WorkspaceConfig{Root: wsDir},
		Stall:     StallConfig{TimeoutMS: 0},
	}
	cfg.applyDefaults()
	ws, err := NewWorkspaces(wsDir)
	if err != nil {
		t.Fatalf("NewWorkspaces: %v", err)
	}
	c, err := New(Options{
		Config:     cfg,
		Tracker:    ft,
		Runner:     runner,
		Workspaces: ws,
		Logger:     iterlog.New(iterlog.LevelError, &bytes.Buffer{}),
		HostMarker: "test",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func TestDispatcherDispatchAndFinish(t *testing.T) {
	ft := newFakeTracker()
	ft.add(tracker.Issue{
		ID: "fake:1", Identifier: "fake#1",
		Title: "go", WorkflowState: "ready",
		Assignee: "feature_dev",
	})

	type capture struct {
		runID    string
		assignee string
	}
	dispatched := make(chan capture, 1)
	runner := &StubRunner{Handler: func(_ context.Context, spec DispatchSpec) error {
		dispatched <- capture{runID: spec.RunID, assignee: spec.Assignee}
		return nil
	}}

	c := newTestDispatcher(t, runner, ft, 50*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.Start(ctx)
	defer c.Stop()

	select {
	case got := <-dispatched:
		if got.runID == "" {
			t.Fatal("empty run ID")
		}
		if got.assignee != "feature_dev" {
			t.Fatalf("dispatch spec dropped issue.Assignee: got %q want %q", got.assignee, "feature_dev")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for dispatch")
	}

	// Wait for the actor to process the finish + release.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		snap := c.Snapshot()
		if len(snap.Running) == 0 {
			ft.mu.Lock()
			_, claimed := ft.claims["fake:1"]
			ft.mu.Unlock()
			if !claimed {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("dispatcher did not release claim, last snapshot=%+v", c.Snapshot())
}

func TestDispatcherRetriesOnFailure(t *testing.T) {
	ft := newFakeTracker()
	ft.add(tracker.Issue{ID: "fake:2", Identifier: "fake#2", Title: "boom", WorkflowState: "ready"})

	var calls atomic.Int32
	runner := &StubRunner{Handler: func(_ context.Context, _ DispatchSpec) error {
		calls.Add(1)
		return errors.New("simulated failure")
	}}

	c := newTestDispatcher(t, runner, ft, 50*time.Millisecond)
	// Override the backoff cap to keep the test fast.
	cfg := c.cfg.Load()
	cfg.Agent.MaxRetryBackoffMS = 100
	c.cfg.Store(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.Start(ctx)
	defer c.Stop()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if calls.Load() >= 2 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("expected at least 2 dispatch attempts, saw %d", calls.Load())
}

func TestDispatcherRespectsClaimConflict(t *testing.T) {
	ft := newFakeTracker()
	ft.add(tracker.Issue{ID: "fake:3", Identifier: "fake#3", WorkflowState: "ready"})
	// Pre-claim under a different marker — dispatcher must not dispatch.
	if err := ft.Claim(context.Background(), "fake:3", "someone-else"); err != nil {
		t.Fatalf("pre-claim: %v", err)
	}

	var calls atomic.Int32
	runner := &StubRunner{Handler: func(_ context.Context, _ DispatchSpec) error {
		calls.Add(1)
		return nil
	}}
	c := newTestDispatcher(t, runner, ft, 50*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.Start(ctx)
	defer c.Stop()

	time.Sleep(300 * time.Millisecond)
	if calls.Load() != 0 {
		t.Fatalf("dispatcher dispatched a pre-claimed issue (%d calls)", calls.Load())
	}
}

func TestDispatcherCancel(t *testing.T) {
	ft := newFakeTracker()
	ft.add(tracker.Issue{ID: "fake:4", Identifier: "fake#4", WorkflowState: "ready"})

	dispatchStarted := make(chan struct{}, 1)
	runner := &StubRunner{Handler: func(ctx context.Context, _ DispatchSpec) error {
		dispatchStarted <- struct{}{}
		<-ctx.Done()
		return ctx.Err()
	}}

	c := newTestDispatcher(t, runner, ft, 50*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.Start(ctx)
	defer c.Stop()

	select {
	case <-dispatchStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("dispatch never started")
	}

	c.Cancel("fake:4")

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(c.Snapshot().Running) == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("cancel did not remove the running entry")
}

func TestBackoffMath(t *testing.T) {
	cases := []struct {
		attempt int
		cap     time.Duration
		want    time.Duration
	}{
		{0, 0, time.Second},
		{1, 10 * time.Minute, 10 * time.Second},
		{2, 10 * time.Minute, 20 * time.Second},
		{3, 10 * time.Minute, 40 * time.Second},
		{4, 10 * time.Minute, 80 * time.Second},
		{6, 60 * time.Second, 60 * time.Second}, // capped
	}
	for _, c := range cases {
		got := computeBackoff(c.attempt, c.cap)
		if got != c.want {
			t.Errorf("computeBackoff(%d, %s) = %s, want %s", c.attempt, c.cap, got, c.want)
		}
	}
}
