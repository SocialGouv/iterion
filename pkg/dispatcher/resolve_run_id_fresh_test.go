package dispatcher

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/dispatcher/tracker"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// TestResolveRunID_LastRunPointerDecidesResumeVsFresh drives the REAL
// resolution path — resolveRunID → LastRunForIssue (native.Adapter) →
// resumableRunID (real run store) — over a real native board, so the answer
// comes from the producer rather than a stub.
//
// It is the second half of the board's `fresh` reset: the endpoint writes the
// persisted shape (last_run_id dropped), and THIS is what that shape buys —
// the dispatcher stops resuming the dead run and mints a new id, agreeing
// with the studio admission loop instead of racing it.
func TestResolveRunID_LastRunPointerDecidesResumeVsFresh(t *testing.T) {
	dir := t.TempDir()

	runStore, err := store.New(dir)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	run, err := runStore.CreateRun(context.Background(), "run-deterministic-fail", "wf", nil)
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	// The #494 shape: a failure a resume cannot cure, parked resumable.
	run.Status = store.RunStatusFailedResumable
	run.Checkpoint = &store.Checkpoint{NodeID: "record_contracts_incomplete"}
	if err := runStore.SaveRun(context.Background(), run); err != nil {
		t.Fatalf("SaveRun: %v", err)
	}

	ns, err := native.NewStore(filepath.Join(dir, "dispatcher"))
	if err != nil {
		t.Fatalf("native.NewStore: %v", err)
	}
	iss, err := ns.Create(native.Issue{Title: "deterministic", State: native.StateReady, Bot: "review"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := ns.SetLastRun(iss.ID, "run-deterministic-fail", ""); err != nil {
		t.Fatalf("SetLastRun: %v", err)
	}

	cfg := &Config{
		Name:      "test",
		Workflow:  filepath.Join(t.TempDir(), "fake.bot"),
		Tracker:   TrackerConfig{Kind: "native"},
		Polling:   PollingConfig{IntervalMS: 50},
		Agent:     AgentConfig{MaxConcurrent: 4, RunningState: "in_progress"},
		Workspace: WorkspaceConfig{Root: filepath.Join(dir, "ws")},
	}
	cfg.applyDefaults()
	ws, err := NewWorkspaces(cfg.Workspace.Root)
	if err != nil {
		t.Fatalf("NewWorkspaces: %v", err)
	}
	// A resume target with no workspace restarts fresh for its own reason
	// (missingResumeWorkspace), which would mask the pointer's effect —
	// materialise the generation the run owns.
	if _, _, err := ws.CreateForRun(iss.ID, "run-deterministic-fail"); err != nil {
		t.Fatalf("CreateForRun: %v", err)
	}
	c, err := New(Options{
		Config:     cfg,
		Tracker:    native.NewAdapter(ns),
		Runner:     &StubRunner{},
		Workspaces: ws,
		Logger:     iterlog.New(iterlog.LevelError, &bytes.Buffer{}),
		StoreDir:   dir,
		HostMarker: "test-host-1",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	trackerIssue := tracker.Issue{ID: iss.ID, Identifier: iss.ID, Title: iss.Title, WorkflowState: native.StateReady}

	// With the pointer: the dispatcher RESUMES the dead run. This is what
	// makes a board Retry mean "resume" on a dispatcher-owned board.
	runID, resumeFrom, _, ok := c.resolveRunID(context.Background(), trackerIssue)
	if !ok {
		t.Fatalf("resolveRunID refused the dispatch outright (runID=%q resumeFrom=%q)", runID, resumeFrom)
	}
	if resumeFrom != "run-deterministic-fail" {
		t.Fatalf("resumeFromRunID = %q, want %q — the resume half of the race is not being exercised",
			resumeFrom, "run-deterministic-fail")
	}
	if runID != "run-deterministic-fail" {
		t.Fatalf("runID = %q, want the resumed run", runID)
	}

	// The board's `fresh` reset writes exactly this: SetLastRun with an
	// empty run id (native.BoardStore's documented clear).
	if err := ns.SetLastRun(iss.ID, "", ""); err != nil {
		t.Fatalf("SetLastRun clear: %v", err)
	}

	runID, resumeFrom, _, ok = c.resolveRunID(context.Background(), trackerIssue)
	if !ok {
		t.Fatalf("resolveRunID refused after the clear (runID=%q resumeFrom=%q) — the ticket is stuck, not fresh", runID, resumeFrom)
	}
	if resumeFrom != "" {
		t.Fatalf("resumeFromRunID = %q, want \"\" — the cleared pointer must not resume anything", resumeFrom)
	}
	if runID == "" || runID == "run-deterministic-fail" {
		t.Fatalf("runID = %q, want a freshly minted id", runID)
	}
}
