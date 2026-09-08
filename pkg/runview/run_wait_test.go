package runview

import (
	"context"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

// runWaitContext bounds real-process integration tests by the test harness,
// not by an estimate of how fast git, a shell or the filesystem should run.
// Their oracle is a persisted state or a joined goroutine. Leave part of the
// harness's remaining time for cancellation, diagnostics and TempDir cleanup.
// An explicit go test -timeout=0 keeps its meaning: no wall-clock ceiling.
func runWaitContext(t *testing.T) context.Context {
	t.Helper()
	if deadline, ok := t.Deadline(); ok {
		remaining := time.Until(deadline)
		margin := min(5*time.Second, remaining/10)
		ctx, cancel := context.WithDeadline(context.Background(), deadline.Add(-margin))
		t.Cleanup(cancel)
		return ctx
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return ctx
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
