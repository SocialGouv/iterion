package server

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
)

// TestWatchCoordinator_FansStateChangeToSubscribedRun is the MVP3b
// integration guard: a watched issue's board transition enqueues a
// message onto the subscribing run and only that run.
func TestWatchCoordinator_FansStateChangeToSubscribedRun(t *testing.T) {
	ctx := context.Background()
	rs, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	svc, err := runview.NewService("", runview.WithStore(rs))
	if err != nil {
		t.Fatalf("runview.NewService: %v", err)
	}

	ns, err := native.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("native.NewStore: %v", err)
	}
	t.Cleanup(func() { _ = ns.Close() })
	iss, err := ns.Create(native.Issue{Title: "watched ticket", State: "ready"})
	if err != nil {
		t.Fatalf("native Create: %v", err)
	}

	mustCreateRunning := func(id string, watch []string) {
		if _, err := rs.CreateRun(ctx, id, "wf", nil); err != nil {
			t.Fatalf("CreateRun %s: %v", id, err)
		}
		if err := rs.UpdateRunStatus(ctx, id, store.RunStatusRunning, ""); err != nil {
			t.Fatalf("UpdateRunStatus %s: %v", id, err)
		}
		if _, err := rs.AddWatchedIssues(ctx, id, watch); err != nil {
			t.Fatalf("AddWatchedIssues %s: %v", id, err)
		}
	}
	mustCreateRunning("run-watcher", []string{iss.ID})
	mustCreateRunning("run-other", []string{"native:unrelated"})

	logger := iterlog.New(iterlog.LevelError, os.Stderr)
	wc := startWatchCoordinator(svc, ns, logger)
	if wc == nil {
		t.Skip("watch coordinator unavailable on host (events tail/fsnotify)")
	}
	t.Cleanup(wc.Close)

	if _, err := ns.SetState(iss.ID, "in_progress"); err != nil {
		t.Fatalf("SetState: %v", err)
	}

	// The subscribing run receives the notification; the other run does not.
	waitForQueued(t, rs, "run-watcher", 1)
	msgs, err := rs.LoadPendingQueuedMessages(ctx, "run-watcher")
	if err != nil {
		t.Fatalf("LoadPendingQueuedMessages: %v", err)
	}
	if len(msgs) == 0 || !strings.Contains(msgs[0].Text, iss.ID) {
		t.Fatalf("watcher message missing issue ref: %#v", msgs)
	}
	if !strings.Contains(msgs[0].Text, "in_progress") {
		t.Errorf("watcher message missing target state: %q", msgs[0].Text)
	}

	other, err := rs.LoadPendingQueuedMessages(ctx, "run-other")
	if err != nil {
		t.Fatalf("LoadPendingQueuedMessages other: %v", err)
	}
	if len(other) != 0 {
		t.Errorf("non-subscribed run got %d messages, want 0", len(other))
	}
}

// TestWatchCoordinator_SkipsTerminalRuns confirms a finished run watching
// the issue is not notified — a queued message would never be consumed.
func TestWatchCoordinator_SkipsTerminalRuns(t *testing.T) {
	ctx := context.Background()
	rs, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	svc, err := runview.NewService("", runview.WithStore(rs))
	if err != nil {
		t.Fatalf("runview.NewService: %v", err)
	}
	ns, err := native.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("native.NewStore: %v", err)
	}
	t.Cleanup(func() { _ = ns.Close() })
	iss, err := ns.Create(native.Issue{Title: "t", State: "ready"})
	if err != nil {
		t.Fatalf("native Create: %v", err)
	}

	if _, err := rs.CreateRun(ctx, "run-done", "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if _, err := rs.AddWatchedIssues(ctx, "run-done", []string{iss.ID}); err != nil {
		t.Fatalf("AddWatchedIssues: %v", err)
	}
	if err := rs.UpdateRunStatus(ctx, "run-done", store.RunStatusFinished, ""); err != nil {
		t.Fatalf("UpdateRunStatus: %v", err)
	}

	logger := iterlog.New(iterlog.LevelError, os.Stderr)
	wc := startWatchCoordinator(svc, ns, logger)
	if wc == nil {
		t.Skip("watch coordinator unavailable on host (events tail/fsnotify)")
	}
	t.Cleanup(wc.Close)

	if _, err := ns.SetState(iss.ID, "in_progress"); err != nil {
		t.Fatalf("SetState: %v", err)
	}
	time.Sleep(200 * time.Millisecond) // allow any (buggy) delivery to land

	msgs, err := rs.LoadPendingQueuedMessages(ctx, "run-done")
	if err != nil {
		t.Fatalf("LoadPendingQueuedMessages: %v", err)
	}
	if len(msgs) != 0 {
		t.Errorf("terminal run got %d messages, want 0", len(msgs))
	}
}

func waitForQueued(t *testing.T, rs store.RunStore, runID string, want int) {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		msgs, err := rs.LoadPendingQueuedMessages(ctx, runID)
		if err == nil && len(msgs) >= want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("run %s did not receive %d queued message(s) within deadline", runID, want)
}
