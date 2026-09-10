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
	"github.com/SocialGouv/iterion/pkg/runtime"
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
		Workflow:  dir + "/dummy.bot",
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
	t.Parallel()
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
	case <-time.After(10 * time.Second):
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
	waitUntil(t, 10*time.Second, "the dispatcher to free the slot and release the claim",
		func() bool {
			if len(c.Snapshot().Running) != 0 {
				return false
			}
			refreshed, _ := ns.Get(iss.ID)
			return refreshed.Claim == ""
		},
		func() string { return fmt.Sprintf("snapshot=%+v", c.Snapshot()) })
}

func TestDispatcherE2E_RetryAfterFailure(t *testing.T) {
	t.Parallel()
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

	waitUntil(t, 4*time.Second, "a failed dispatch to be retried at least once",
		func() bool { return calls.Load() >= 2 },
		func() string {
			return fmt.Sprintf("attempts=%d want>=2, snapshot=%+v", calls.Load(), c.Snapshot())
		})
}

func TestDispatcherE2E_CancelInFlight(t *testing.T) {
	t.Parallel()
	c, ns, runner, cleanup := newDispatcherFixture(t, 50*time.Millisecond)
	defer cleanup()

	started := make(chan string, 1)
	var calls atomic.Int32
	var handlerReturned atomic.Bool
	runner.Handler = func(ctx context.Context, spec dispatcher.DispatchSpec) error {
		// Report only the first run: a regressed redispatch must not block the
		// notification send and prevent fixture cleanup from cancelling it.
		if calls.Add(1) == 1 {
			started <- spec.RunID
		}
		<-ctx.Done()
		handlerReturned.Store(true)
		// Match EngineRunner's operator-cancel outcome. context.Canceled means
		// force-reap to finishRun: it releases the claim and permits a new run.
		// The old stub took that path, so Running was empty only until the next
		// poll and a delayed observer waited forever on a SECOND run (#943).
		return runtime.ErrRunCancelled
	}

	iss, err := ns.Create(native.Issue{Title: "hangs", State: "ready"})
	if err != nil {
		t.Fatal(err)
	}
	var firstRunID string
	select {
	case firstRunID = <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("worker never started")
	}

	c.Cancel(iss.ID)

	// An operator cancel frees the slot AND leaves the issue held. Requiring
	// that stable outcome rejects the transient empty snapshot between a
	// force-reap and its next dispatch. Keep the timeout as a hang detector.
	waitUntil(t, 10*time.Second, "cancel to free the slot and hold the issue",
		func() bool {
			snap := c.Snapshot()
			if len(snap.Running) != 0 {
				return false
			}
			for _, skip := range snap.DispatchSkips {
				if skip.IssueID == iss.ID && strings.Contains(skip.Reason, "cancelled by the operator") {
					return true
				}
			}
			return false
		},
		func() string {
			return fmt.Sprintf("first run=%s, starts=%d, handler returned=%v, snapshot=%+v",
				firstRunID, calls.Load(), handlerReturned.Load(), c.Snapshot())
		})
	if got := calls.Load(); got != 1 {
		t.Fatalf("dispatches = %d, want one cancelled run", got)
	}
	held, err := ns.Get(iss.ID)
	if err != nil {
		t.Fatal(err)
	}
	if held.Claim == "" {
		t.Fatal("operator cancel released the claim; a future tick could redispatch")
	}
}

func TestDispatcherE2E_RespectsTerminalStateChange(t *testing.T) {
	t.Parallel()
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

	waitUntil(t, 10*time.Second, "the dispatch to start",
		func() bool { return len(c.Snapshot().Running) == 1 })

	// Externally move issue to a terminal state — dispatcher should cancel.
	if _, err := ns.SetState(iss.ID, "done"); err != nil {
		t.Fatalf("SetState: %v", err)
	}

	waitUntil(t, 10*time.Second, "the dispatcher to honor the external state change",
		func() bool { return len(c.Snapshot().Running) == 0 })
}

func TestDispatcherE2E_HTTPSurface(t *testing.T) {
	t.Parallel()
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
	waitUntil(t, 10*time.Second, "the HTTP round-trip dispatch+release to complete",
		func() bool {
			l, _ := ns.List(native.ListFilter{})
			return len(l) == 1 && l[0].Claim == ""
		},
		func() string { return fmt.Sprintf("snapshot=%+v", c.Snapshot()) })
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
	t.Parallel()
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
