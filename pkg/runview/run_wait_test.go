package runview

import (
	"context"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

const (
	// waitSlowdown is how much slower a contended runner is than the quiet
	// machine a wait was measured on. 6 covers the ~7x observed between this
	// repo's developer boxes and a five-group merge build (3.6 ms vs 27 ms
	// per card on the same store sweep).
	waitSlowdown = 6
	// waitDeadlineMargin is what a wait leaves of `-timeout` for the failure
	// to be reported and the package to unwind — a run goroutine joined, a
	// service stopped, a t.TempDir removed.
	waitDeadlineMargin = 30 * time.Second
	// runWaitCeiling bounds ONE operation. t.Deadline() is set once per test
	// BINARY, so it alone would hand every wait the whole package's remaining
	// `-timeout`: a single wait that never satisfies would burn ~10 min (the
	// unit job's default) or ~30 min (the race job's -timeout 1800s) and hand
	// every test scheduled after it a near-expired deadline. This is the
	// package's own scale-up of the 30s figure these waits carried.
	runWaitCeiling = 30 * time.Second * waitSlowdown
)

// runWaitTimeout derives ONE wait's bound: the earlier of the per-operation
// ceiling and what is left of the harness deadline minus the unwind margin.
//
// bounded=false means the harness has no deadline (`go test -timeout=0`), and
// the caller keeps its documented "no wall-clock ceiling" semantics. A
// non-positive duration means there is no room left to both wait and unwind:
// the caller must fail NOW, as its own named assertion, rather than run into a
// package-wide harness panic naming whichever test was in flight. Subtracting
// the margin can never push the bound past the harness deadline — the margin
// is a constant, so a deadline already inside (or past) it yields <= 0, not a
// wait that outlives the harness.
func runWaitTimeout(deadline time.Time, hasDeadline bool, now time.Time) (time.Duration, bool) {
	if !hasDeadline {
		return 0, false
	}
	return min(runWaitCeiling, deadline.Sub(now)-waitDeadlineMargin), true
}

// runWaitContext bounds each real-process wait by the operation ceiling and
// the test harness. Its oracle is a persisted state or a joined goroutine.
// Part of the harness time remains for cancellation, diagnostics and cleanup.
// An explicit go test -timeout=0 keeps its meaning: no wall-clock ceiling.
func runWaitContext(t *testing.T) context.Context {
	t.Helper()
	deadline, ok := t.Deadline()
	within, bounded := runWaitTimeout(deadline, ok, time.Now())
	if !bounded {
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		return ctx
	}
	// A non-positive `within` yields an already-expired context, so the
	// caller reports its own assertion instead of hanging into the harness.
	ctx, cancel := context.WithTimeout(context.Background(), within)
	t.Cleanup(cancel)
	return ctx
}

// TestRunWaitTimeoutIsBoundedByTheOperationAndNeverOutlivesTheHarness pins the
// two properties a harness deadline alone does not give: a wait may not spend
// the whole package's `-timeout`, and it may never end AFTER the deadline it
// exists to fail inside of (the regression a `remaining/10` margin shipped —
// with the deadline already passed it went negative and pushed the wait past
// the harness, exactly when the margin matters).
func TestRunWaitTimeoutIsBoundedByTheOperationAndNeverOutlivesTheHarness(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name        string
		deadline    time.Time
		hasDeadline bool
		want        time.Duration
		wantBounded bool
	}{
		{
			name:        "no harness deadline keeps -timeout=0 unlimited",
			hasDeadline: false,
			wantBounded: false,
		},
		{
			name:        "a long -timeout is capped by the per-operation ceiling",
			deadline:    now.Add(30 * time.Minute),
			hasDeadline: true,
			want:        runWaitCeiling,
			wantBounded: true,
		},
		{
			name:        "a nearer harness deadline shortens the operation",
			deadline:    now.Add(time.Minute),
			hasDeadline: true,
			want:        time.Minute - waitDeadlineMargin,
			wantBounded: true,
		},
		{
			name:        "a deadline inside the unwind margin leaves no room to wait",
			deadline:    now.Add(waitDeadlineMargin - 10*time.Second),
			hasDeadline: true,
			want:        -10 * time.Second,
			wantBounded: true,
		},
		{
			name:        "an already-expired deadline is not inverted into extra time",
			deadline:    now.Add(-10 * time.Second),
			hasDeadline: true,
			want:        -waitDeadlineMargin - 10*time.Second,
			wantBounded: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, bounded := runWaitTimeout(tc.deadline, tc.hasDeadline, now)
			if bounded != tc.wantBounded {
				t.Fatalf("bounded = %v, want %v", bounded, tc.wantBounded)
			}
			if !bounded {
				return
			}
			if got != tc.want {
				t.Fatalf("timeout = %s, want %s", got, tc.want)
			}
			if got > 0 && now.Add(got).After(tc.deadline) {
				t.Fatalf("wait ends at %s, after the harness deadline %s", now.Add(got), tc.deadline)
			}
		})
	}
}

// stopService joins the service's background workers and every run goroutine
// before the test's t.TempDir goes — a service left running writes into a
// directory RemoveAll is already walking.
//
// The context is what BOUNDS that join: Manager.Stop waits per handle on
// `select { case <-h.done: case <-ctx.Done(): return }`, and with
// context.Background() the second arm is nil and can never fire. A wedged run
// goroutine — a subbot child registered mid-flight, for one — would then block
// teardown forever and surface only as a package-wide harness panic naming
// whichever test was in flight. waitDeadlineMargin is what every wait in this
// package already reserves for exactly this unwind.
func stopService(t *testing.T, svc *Service) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), waitDeadlineMargin)
	defer cancel()
	svc.Stop(ctx)
	if err := ctx.Err(); err != nil {
		t.Errorf("service teardown exceeded %s: %v", waitDeadlineMargin, err)
	}
}

func awaitRunCompletion(t *testing.T, done <-chan struct{}, what string) {
	t.Helper()
	ctx := runWaitContext(t)
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatalf("%s: %v", what, ctx.Err())
	}
}

func waitForSubbotStatus(t *testing.T, svc *Service, parentID string, want store.RunStatus) string {
	t.Helper()
	ctx := runWaitContext(t)
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		ids, err := svc.store.ListChildRuns(ctx, parentID)
		if err != nil {
			t.Fatalf("list children of %s: %v", parentID, err)
		}
		for _, id := range ids {
			child, err := svc.store.LoadRun(ctx, id)
			if err != nil {
				t.Fatalf("load child %s: %v", id, err)
			}
			if child.Status == want {
				return id
			}
			if child.Status.IsTerminal() {
				t.Fatalf("child %s reached %s (%s), want %s", id, child.Status, child.Error, want)
			}
		}
		parent, err := svc.store.LoadRun(ctx, parentID)
		if err != nil {
			t.Fatalf("load parent %s: %v", parentID, err)
		}
		if parent.Status.IsTerminal() {
			t.Fatalf("parent %s reached %s (%s) before its child reached %s", parentID, parent.Status, parent.Error, want)
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatalf("child of %s never reached %s (parent %s, error %q): %v", parentID, want, parent.Status, parent.Error, ctx.Err())
		}
	}
}
