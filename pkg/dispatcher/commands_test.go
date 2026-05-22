package dispatcher

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dispatcher/tracker"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// lastRunCall captures one SetLastRun invocation so the test can
// assert on the (id, run, workdir) trio without coupling to the
// native package.
type lastRunCall struct {
	id      string
	runID   string
	workdir string
}

// lastRunTracker wraps fakeTracker and implements the optional
// SetLastRun shape the dispatcher type-asserts to. Records each call
// so the test can assert ordering + content.
type lastRunTracker struct {
	*fakeTracker

	mu    sync.Mutex
	calls []lastRunCall
}

func newLastRunTracker() *lastRunTracker {
	return &lastRunTracker{fakeTracker: newFakeTracker()}
}

func (t *lastRunTracker) SetLastRun(id, runID, workdir string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.calls = append(t.calls, lastRunCall{id: id, runID: runID, workdir: workdir})
	return nil
}

func (t *lastRunTracker) lastRunCalls() []lastRunCall {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]lastRunCall, len(t.calls))
	copy(out, t.calls)
	return out
}

// TestFinishRun_StampsLastRunOnNativeTracker exercises the stamp path
// via the type-assertion: the fake implements SetLastRun, the
// dispatcher's finishRun must call it with the right (issue, run,
// workdir) tuple regardless of whether the run finished cleanly or
// failed (best-effort: the operator always wants the link back to
// the most recent run that processed the issue).
func TestFinishRun_StampsLastRunOnNativeTracker(t *testing.T) {
	ft := newLastRunTracker()

	dir := t.TempDir()
	wsDir := filepath.Join(dir, "ws")
	storeDir := filepath.Join(dir, "store")
	cfg := &Config{
		Name:      "test",
		Workflow:  t.TempDir() + "/fake.iter",
		Tracker:   TrackerConfig{Kind: "fake"},
		Polling:   PollingConfig{IntervalMS: 50},
		Agent:     AgentConfig{MaxConcurrent: 4, MaxRetryBackoffMS: 1000, RunningState: "in_progress"},
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
		Runner:     &StubRunner{},
		Workspaces: ws,
		Logger:     iterlog.New(iterlog.LevelError, &bytes.Buffer{}),
		HostMarker: "test",
		StoreDir:   storeDir,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Plant a fake run.json so resolveRunWorkdir picks up the canonical
	// WorkDir (mirrors the worktree:auto case where Run.WorkDir is
	// swapped to the worktree path mid-run).
	runID := "run-stamp-1"
	runDir := filepath.Join(storeDir, "runs", runID)
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatalf("mkdir run dir: %v", err)
	}
	wtPath := filepath.Join(storeDir, "worktrees", runID)
	runJSON, _ := json.Marshal(map[string]any{
		"id":       runID,
		"work_dir": wtPath,
	})
	if err := os.WriteFile(filepath.Join(runDir, "run.json"), runJSON, 0o644); err != nil {
		t.Fatalf("write run.json: %v", err)
	}

	// Seed a running entry directly and invoke finishRun on the actor
	// goroutine via the cmd channel — this exercises the same path
	// cmdRunFinished.apply uses, without spinning up the full polling
	// loop. ctx parent for the entry is background; revertTransition's
	// internal release uses its own timeout-bound ctx.
	issueID := "fake:stamp-1"
	ft.add(tracker.Issue{
		ID: issueID, Identifier: "fake#stamp-1",
		Title: "go", WorkflowState: "in_progress",
	})

	c.state.running[issueID] = &runningEntry{
		IssueID:       issueID,
		Identifier:    "fake#stamp-1",
		RunID:         runID,
		WorkflowState: "in_progress",
		WorkspacePath: filepath.Join(wsDir, "fake_stamp-1"),
		StartedAt:     time.Now(),
	}

	c.finishRun(context.Background(), issueID, nil)

	calls := ft.lastRunCalls()
	if len(calls) != 1 {
		t.Fatalf("SetLastRun calls = %d, want 1", len(calls))
	}
	got := calls[0]
	if got.id != issueID {
		t.Fatalf("call id = %q, want %q", got.id, issueID)
	}
	if got.runID != runID {
		t.Fatalf("call runID = %q, want %q", got.runID, runID)
	}
	// The dispatcher must prefer the run.json work_dir over the
	// dispatcher's WorkspacePath — that's the load-bearing piece for
	// worktree:auto runs whose diff lives outside the workspace dir.
	if got.workdir != wtPath {
		t.Fatalf("call workdir = %q, want %q (from run.json)", got.workdir, wtPath)
	}
}

// TestFinishRun_StampsOnFailureToo asserts the stamp lands even when
// the run failed — the operator still wants to inspect the partial
// diff via the worktree link.
func TestFinishRun_StampsOnFailureToo(t *testing.T) {
	ft := newLastRunTracker()

	dir := t.TempDir()
	wsDir := filepath.Join(dir, "ws")
	cfg := &Config{
		Name:      "test",
		Workflow:  t.TempDir() + "/fake.iter",
		Tracker:   TrackerConfig{Kind: "fake"},
		Polling:   PollingConfig{IntervalMS: 50},
		Agent:     AgentConfig{MaxConcurrent: 4, MaxRetryBackoffMS: 1000, RunningState: "in_progress"},
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
		Runner:     &StubRunner{},
		Workspaces: ws,
		Logger:     iterlog.New(iterlog.LevelError, &bytes.Buffer{}),
		HostMarker: "test",
		// No StoreDir → resolveRunWorkdir returns "" → falls back to
		// WorkspacePath. Asserts the fallback path is wired.
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	issueID := "fake:stamp-2"
	wsPath := filepath.Join(wsDir, "fake_stamp-2")
	ft.add(tracker.Issue{
		ID: issueID, Identifier: "fake#stamp-2",
		Title: "go", WorkflowState: "in_progress",
	})
	c.state.running[issueID] = &runningEntry{
		IssueID:               issueID,
		Identifier:            "fake#stamp-2",
		RunID:                 "run-fail-1",
		WorkflowState:         "in_progress",
		WorkspacePath:         wsPath,
		StartedAt:             time.Now(),
		TransitionedFromState: "ready",
	}

	// Use a non-cancellation error so we hit the failure (not soft-stop)
	// branch — both branches stamp, but explicitly exercising the
	// non-cancel path guards against a regression where stampLastRun
	// gets accidentally tucked under the err==nil arm.
	c.finishRun(context.Background(), issueID, errPermanentFailure{})

	calls := ft.lastRunCalls()
	if len(calls) != 1 {
		t.Fatalf("SetLastRun calls = %d, want 1", len(calls))
	}
	got := calls[0]
	if got.workdir != wsPath {
		t.Fatalf("fallback workdir = %q, want WorkspacePath %q", got.workdir, wsPath)
	}
	if got.runID != "run-fail-1" {
		t.Fatalf("runID = %q, want run-fail-1", got.runID)
	}
}

// errPermanentFailure is a tagged error that fakeTracker doesn't
// recognise as cancellation — used to drive the failure branch of
// finishRun above.
type errPermanentFailure struct{}

func (errPermanentFailure) Error() string { return "permanent failure" }
