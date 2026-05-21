// E2E coverage for the dispatcher layer: native tracker + adapter +
// actor + dispatch end-to-end against a StubRunner. No external CLI,
// no LLM, no network — just the iterion dispatcher pipeline.

package e2e

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dispatcher"
	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/google/uuid"
)

// newDispatcherFixture wires a native tracker + workspaces + StubRunner
// + Dispatcher on a temporary directory. The returned cleanup function
// stops the actor and removes timers.
func newDispatcherFixture(t *testing.T, polling time.Duration) (
	*dispatcher.Dispatcher,
	*native.Store,
	*dispatcher.StubRunner,
	func(),
) {
	t.Helper()
	dir := t.TempDir()

	ns, err := native.NewStore(dir + "/dispatcher")
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	ws, err := dispatcher.NewWorkspaces(dir + "/dispatcher/workspaces")
	if err != nil {
		t.Fatalf("NewWorkspaces: %v", err)
	}

	cfg := &dispatcher.Config{
		Name:      "e2e",
		Workflow:  dir + "/dummy.iter",
		Tracker:   dispatcher.TrackerConfig{Kind: "native"},
		Polling:   dispatcher.PollingConfig{IntervalMS: int(polling.Milliseconds())},
		Agent:     dispatcher.AgentConfig{MaxConcurrent: 2, MaxRetryBackoffMS: 500},
		Workspace: dispatcher.WorkspaceConfig{Root: dir + "/dispatcher/workspaces"},
		Stall:     dispatcher.StallConfig{TimeoutMS: 0},
	}
	// Apply defaults manually so the cfg is internally consistent
	// without going through Load (which checks the workflow file).
	if cfg.Polling.IntervalMS == 0 {
		cfg.Polling.IntervalMS = 50
	}

	logger := iterlog.New(iterlog.LevelError, &bytes.Buffer{})
	runner := &dispatcher.StubRunner{}
	c, err := dispatcher.New(dispatcher.Options{
		Config:     cfg,
		Tracker:    native.NewAdapter(ns),
		Runner:     runner,
		Workspaces: ws,
		Logger:     logger,
		StoreDir:   dir,
		HostMarker: "e2e",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	c.Start(ctx)
	cleanup := func() {
		cancel()
		c.Stop()
	}
	return c, ns, runner, cleanup
}

func TestDispatcherE2E_DispatchAndRelease(t *testing.T) {
	dispatched := make(chan dispatcher.DispatchSpec, 4)
	c, ns, runner, cleanup := newDispatcherFixture(t, 50*time.Millisecond)
	defer cleanup()

	runner.Handler = func(_ context.Context, spec dispatcher.DispatchSpec) error {
		dispatched <- spec
		return nil
	}

	iss, err := ns.Create(native.Issue{Title: "do the thing", State: "ready", Priority: 7})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	var got dispatcher.DispatchSpec
	select {
	case got = <-dispatched:
	case <-time.After(2 * time.Second):
		t.Fatal("dispatch never fired")
	}
	parsed, err := uuid.Parse(got.RunID)
	if err != nil {
		t.Fatalf("runID is not a valid UUID: %s (%v)", got.RunID, err)
	}
	if parsed.Version() != 7 {
		t.Fatalf("runID is not UUIDv7: %s (version=%d)", got.RunID, parsed.Version())
	}
	if got.WorkspacePath == "" {
		t.Fatal("workspace path missing")
	}

	// Wait for the actor to drain the cmdRunFinished + release the claim.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(c.Snapshot().Running) == 0 {
			refreshed, _ := ns.Get(iss.ID)
			if refreshed.Claim == "" {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("dispatcher did not release claim. snapshot=%+v", c.Snapshot())
}

func TestDispatcherE2E_RetryAfterFailure(t *testing.T) {
	var calls atomic.Int32
	c, ns, runner, cleanup := newDispatcherFixture(t, 50*time.Millisecond)
	defer cleanup()

	runner.Handler = func(_ context.Context, _ dispatcher.DispatchSpec) error {
		calls.Add(1)
		return errors.New("transient failure")
	}

	if _, err := ns.Create(native.Issue{Title: "flaky", State: "ready"}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		if calls.Load() >= 2 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("expected at least 2 attempts, saw %d (snapshot=%+v)", calls.Load(), c.Snapshot())
}

func TestDispatcherE2E_CancelInFlight(t *testing.T) {
	c, ns, runner, cleanup := newDispatcherFixture(t, 50*time.Millisecond)
	defer cleanup()

	started := make(chan struct{}, 1)
	runner.Handler = func(ctx context.Context, _ dispatcher.DispatchSpec) error {
		started <- struct{}{}
		<-ctx.Done()
		return ctx.Err()
	}

	iss, _ := ns.Create(native.Issue{Title: "hangs", State: "ready"})

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("worker never started")
	}

	c.Cancel(iss.ID)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(c.Snapshot().Running) == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("cancel did not flush running entry")
}

func TestDispatcherE2E_RespectsTerminalStateChange(t *testing.T) {
	c, ns, runner, cleanup := newDispatcherFixture(t, 50*time.Millisecond)
	defer cleanup()

	hold := make(chan struct{})
	runner.Handler = func(ctx context.Context, _ dispatcher.DispatchSpec) error {
		select {
		case <-hold:
		case <-ctx.Done():
		}
		return ctx.Err()
	}
	defer close(hold)

	iss, _ := ns.Create(native.Issue{Title: "movable", State: "ready"})

	// Wait for dispatch to start.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(c.Snapshot().Running) == 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(c.Snapshot().Running) != 1 {
		t.Fatal("dispatch never started")
	}

	// Externally move issue to a terminal state — dispatcher should cancel.
	if _, err := ns.SetState(iss.ID, "done"); err != nil {
		t.Fatalf("SetState: %v", err)
	}

	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(c.Snapshot().Running) == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("dispatcher did not honor external state change")
}

func TestDispatcherE2E_HTTPSurface(t *testing.T) {
	c, ns, runner, cleanup := newDispatcherFixture(t, 50*time.Millisecond)
	defer cleanup()

	runner.Handler = func(_ context.Context, _ dispatcher.DispatchSpec) error { return nil }

	mux := http.NewServeMux()
	c.RegisterRoutes(mux, "/api/v1/dispatcher")
	ns.RegisterRoutes(mux, "/api/v1/native")
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// Native: create an issue via REST.
	r, err := http.Post(srv.URL+"/api/v1/native/issues", "application/json",
		strings.NewReader(`{"title":"via REST","state":"ready"}`))
	if err != nil {
		t.Fatalf("POST issues: %v", err)
	}
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("status %d", r.StatusCode)
	}
	r.Body.Close()

	// Dispatcher: refresh tick + state.
	if r, err := http.Post(srv.URL+"/api/v1/dispatcher/refresh", "", nil); err != nil || r.StatusCode != http.StatusAccepted {
		t.Fatalf("POST refresh: %v %d", err, statusOrZero(r))
	} else {
		r.Body.Close()
	}

	// Wait for at least one dispatch then release.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		l, _ := ns.List(native.ListFilter{})
		if len(l) == 1 && l[0].Claim == "" {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("HTTP round-trip dispatch+release never completed (snapshot=%+v)", c.Snapshot())
}

func statusOrZero(r *http.Response) int {
	if r == nil {
		return 0
	}
	return r.StatusCode
}

// Sanity check that the dispatcher's snapshot stays JSON-stable across
// ticks even when nothing is dispatched.
func TestDispatcherE2E_SnapshotShape(t *testing.T) {
	c, _, _, cleanup := newDispatcherFixture(t, 50*time.Millisecond)
	defer cleanup()
	snap := c.Snapshot()
	if snap.Tracker != "native" {
		t.Fatalf("tracker: %s", snap.Tracker)
	}
	if snap.Slots.GlobalMax != 2 {
		t.Fatalf("global max: %d", snap.Slots.GlobalMax)
	}
	if snap.Name != "e2e" {
		t.Fatalf("name: %s", snap.Name)
	}
	_ = fmt.Sprint(snap) // touch all the fields to catch nil-pointer mistakes
}
