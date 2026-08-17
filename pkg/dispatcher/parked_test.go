package dispatcher

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/dispatcher/tracker"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// TestReconcileParked guards the parked-card sweep: every resume surface
// (CLI `iterion resume`, studio run console, answer-from-board) completes
// a parked run OUTSIDE the dispatcher, so only the sweep can move the
// awaiting-input card to its final state. Without it the card strands in
// awaiting_input with its claim retained forever.
func TestReconcileParked(t *testing.T) {
	cases := []struct {
		name      string
		runStatus store.RunStatus
		wantState string // "" = card must stay parked
		wantClaim bool
	}{
		{"finished run moves card to completed", store.RunStatusFinished, "review", false},
		{"hard-failed run moves card to failed", store.RunStatusFailed, "blocked", false},
		{"still-paused run stays parked", store.RunStatusPausedWaitingHuman, "", true},
		{"failed-resumable run stays parked (operator can resume)", store.RunStatusFailedResumable, "", true},
		{"cancelled run stays parked (checkpoint resumable)", store.RunStatusCancelled, "", true},
		{"running resume-in-flight stays parked", store.RunStatusRunning, "", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()

			// Real run store with a run in the scenario's status.
			runStore, err := store.New(dir)
			if err != nil {
				t.Fatalf("store.New: %v", err)
			}
			run, err := runStore.CreateRun(context.Background(), "run-parked-1", "wf", nil)
			if err != nil {
				t.Fatalf("CreateRun: %v", err)
			}
			run.Status = tc.runStatus
			if err := runStore.SaveRun(context.Background(), run); err != nil {
				t.Fatalf("SaveRun: %v", err)
			}

			// Real native tracker with a card parked in awaiting_input,
			// claimed by this dispatcher, last_run pointing at the run.
			ns, err := native.NewStore(filepath.Join(dir, "dispatcher"))
			if err != nil {
				t.Fatalf("native.NewStore: %v", err)
			}
			iss, err := ns.Create(native.Issue{Title: "parked", State: native.StateAwaitingInput})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			if err := ns.Claim(iss.ID, "test-host-1"); err != nil {
				t.Fatalf("Claim: %v", err)
			}
			if err := ns.SetLastRun(iss.ID, "run-parked-1", ""); err != nil {
				t.Fatalf("SetLastRun: %v", err)
			}
			if err := ns.SetAwaitingInput(iss.ID, true); err != nil {
				t.Fatalf("SetAwaitingInput: %v", err)
			}

			cfg := &Config{
				Name:     "test",
				Workflow: filepath.Join(t.TempDir(), "fake.bot"),
				Tracker:  TrackerConfig{Kind: "native"},
				Polling:  PollingConfig{IntervalMS: 50},
				Agent: AgentConfig{
					MaxConcurrent:  4,
					RunningState:   "in_progress",
					CompletedState: "review",
					FailedState:    "blocked",
				},
				Workspace: WorkspaceConfig{Root: filepath.Join(dir, "ws")},
			}
			cfg.applyDefaults()
			ws, err := NewWorkspaces(cfg.Workspace.Root)
			if err != nil {
				t.Fatalf("NewWorkspaces: %v", err)
			}
			c, err := New(Options{
				Config:     cfg,
				Tracker:    native.NewAdapter(ns),
				Runner:     &StubRunner{},
				Workspaces: ws,
				Logger:     iterlog.New(iterlog.LevelError, &bytes.Buffer{}),
				HostMarker: "test-host-1",
				StoreDir:   dir,
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}

			c.reconcileParked(context.Background())

			got, err := ns.Get(iss.ID)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			wantState := tc.wantState
			if wantState == "" {
				wantState = native.StateAwaitingInput
			}
			if got.State != wantState {
				t.Errorf("card state = %q, want %q", got.State, wantState)
			}
			if claimed := got.Claim != ""; claimed != tc.wantClaim {
				t.Errorf("card claimed = %v, want %v", claimed, tc.wantClaim)
			}
			if tc.wantState != "" && got.AwaitingInput {
				t.Errorf("awaiting-input badge must be cleared on the terminal transition")
			}
			if tc.wantState == "" && !got.AwaitingInput {
				t.Errorf("awaiting-input badge must survive while the card stays parked")
			}
		})
	}
}

// TestReconcileParked_SkipsOtherDaemonsClaims: a card parked by ANOTHER
// live daemon sharing the store must be left for its owner.
func TestReconcileParked_SkipsOtherDaemonsClaims(t *testing.T) {
	dir := t.TempDir()
	runStore, err := store.New(dir)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	run, err := runStore.CreateRun(context.Background(), "run-other-1", "wf", nil)
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	run.Status = store.RunStatusFinished
	if err := runStore.SaveRun(context.Background(), run); err != nil {
		t.Fatalf("SaveRun: %v", err)
	}
	ns, err := native.NewStore(filepath.Join(dir, "dispatcher"))
	if err != nil {
		t.Fatalf("native.NewStore: %v", err)
	}
	iss, err := ns.Create(native.Issue{Title: "theirs", State: native.StateAwaitingInput})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := ns.Claim(iss.ID, "other-host-9"); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if err := ns.SetLastRun(iss.ID, "run-other-1", ""); err != nil {
		t.Fatalf("SetLastRun: %v", err)
	}

	cfg := &Config{
		Name:     "test",
		Workflow: filepath.Join(t.TempDir(), "fake.bot"),
		Tracker:  TrackerConfig{Kind: "native"},
		Polling:  PollingConfig{IntervalMS: 50},
		Agent: AgentConfig{
			MaxConcurrent: 4, RunningState: "in_progress",
			CompletedState: "review", FailedState: "blocked",
		},
		Workspace: WorkspaceConfig{Root: filepath.Join(dir, "ws")},
	}
	cfg.applyDefaults()
	ws, err := NewWorkspaces(cfg.Workspace.Root)
	if err != nil {
		t.Fatalf("NewWorkspaces: %v", err)
	}
	c, err := New(Options{
		Config: cfg, Tracker: native.NewAdapter(ns), Runner: &StubRunner{},
		Workspaces: ws, Logger: iterlog.New(iterlog.LevelError, &bytes.Buffer{}),
		HostMarker: "test-host-1", StoreDir: dir,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	c.reconcileParked(context.Background())

	got, err := ns.Get(iss.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.State != native.StateAwaitingInput || got.Claim != "other-host-9" {
		t.Errorf("another daemon's parked card must be untouched, got state=%q claim=%q", got.State, got.Claim)
	}
}

// TestDispatch_RefusesFreshRunWhenLastRunIsPaused is the reboot
// regression: a human/operator pause survived on disk, the ticket is
// still in_progress (claim died with the old PID), and dispatch must
// re-park instead of minting a planner from init_film.
func TestDispatch_RefusesFreshRunWhenLastRunIsPaused(t *testing.T) {
	for _, status := range []store.RunStatus{
		store.RunStatusPausedWaitingHuman,
		store.RunStatusPausedOperator,
	} {
		t.Run(string(status), func(t *testing.T) {
			dir := t.TempDir()
			runStore, err := store.New(dir)
			if err != nil {
				t.Fatalf("store.New: %v", err)
			}
			run, err := runStore.CreateRun(context.Background(), "run-paused-keep", "wf", nil)
			if err != nil {
				t.Fatalf("CreateRun: %v", err)
			}
			run.Status = status
			if err := runStore.SaveRun(context.Background(), run); err != nil {
				t.Fatalf("SaveRun: %v", err)
			}

			ns, err := native.NewStore(filepath.Join(dir, "dispatcher"))
			if err != nil {
				t.Fatalf("native.NewStore: %v", err)
			}
			iss, err := ns.Create(native.Issue{Title: "ulysse", State: native.StateInProgress})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			if err := ns.SetLastRun(iss.ID, "run-paused-keep", ""); err != nil {
				t.Fatalf("SetLastRun: %v", err)
			}

			launched := 0
			runner := &StubRunner{Handler: func(context.Context, DispatchSpec) error {
				launched++
				return nil
			}}
			cfg := &Config{
				Name:     "test",
				Workflow: filepath.Join(t.TempDir(), "fake.bot"),
				Tracker:  TrackerConfig{Kind: "native"},
				Polling:  PollingConfig{IntervalMS: 50},
				Agent: AgentConfig{
					MaxConcurrent:  4,
					RunningState:   "in_progress",
					CompletedState: "review",
					FailedState:    "blocked",
				},
				Workspace: WorkspaceConfig{Root: filepath.Join(dir, "ws")},
			}
			cfg.applyDefaults()
			ws, err := NewWorkspaces(cfg.Workspace.Root)
			if err != nil {
				t.Fatalf("NewWorkspaces: %v", err)
			}
			c, err := New(Options{
				Config: cfg, Tracker: native.NewAdapter(ns), Runner: runner,
				Workspaces: ws, Logger: iterlog.New(iterlog.LevelError, &bytes.Buffer{}),
				HostMarker: "test-host-1", StoreDir: dir,
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}

			c.dispatch(context.Background(), tracker.Issue{
				ID: iss.ID, Identifier: iss.ID, Title: iss.Title,
				WorkflowState: native.StateInProgress,
			})

			if launched != 0 {
				t.Fatalf("dispatcher launched %d fresh run(s), want 0", launched)
			}
			got, err := ns.Get(iss.ID)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if got.State != native.StateAwaitingInput {
				t.Errorf("card state = %q, want %q", got.State, native.StateAwaitingInput)
			}
			if got.LastRunID != "run-paused-keep" {
				t.Errorf("last_run = %q, want the paused run", got.LastRunID)
			}
			if got.Claim == "" {
				t.Errorf("re-parked card must keep the new claim so it is not a candidate")
			}
			if !got.AwaitingInput {
				t.Errorf("awaiting-input badge must be set")
			}
			if _, running := c.state.running[iss.ID]; running {
				t.Errorf("issue must not be tracked as a live worker")
			}
		})
	}
}

func TestReconcileStrandedPaused_ReparksInProgressEvenWhenPaused(t *testing.T) {
	fx := newLastRunFixture(t, store.RunStatusPausedWaitingHuman, native.StateInProgress)

	fx.disp.paused.Store(true)
	fx.disp.tick(context.Background())

	if fx.launched != 0 {
		t.Fatalf("paused tick launched %d run(s), want 0", fx.launched)
	}
	got, err := fx.board.Get(fx.issue.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.State != native.StateAwaitingInput {
		t.Errorf("card state = %q, want %q", got.State, native.StateAwaitingInput)
	}
	if got.LastRunID != fx.runID {
		t.Errorf("last_run = %q, want %q", got.LastRunID, fx.runID)
	}
	if got.Claim == "" {
		t.Error("re-parked card must be claimed")
	}
	if !got.AwaitingInput {
		t.Error("awaiting-input badge must be set")
	}
}

func TestDispatch_RefusesFreshRunWhenLastRunIsStillLive(t *testing.T) {
	for _, status := range []store.RunStatus{
		store.RunStatusRunning,
		store.RunStatusQueued,
	} {
		t.Run(string(status), func(t *testing.T) {
			fx := newLastRunFixture(t, status, native.StateInProgress)
			fx.disp.dispatch(context.Background(), tracker.Issue{
				ID: fx.issue.ID, Identifier: fx.issue.ID, Title: fx.issue.Title,
				WorkflowState: native.StateInProgress,
			})
			if fx.launched != 0 {
				t.Fatalf("status %s launched %d fresh run(s), want 0", status, fx.launched)
			}
			got, err := fx.board.Get(fx.issue.ID)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if got.LastRunID != fx.runID {
				t.Errorf("last_run = %q, want the existing run", got.LastRunID)
			}
			if _, running := fx.disp.state.running[fx.issue.ID]; running {
				t.Error("issue must not be tracked as a live worker")
			}
		})
	}
}

func TestDispatch_FilesFinishedLastRunInsteadOfMinting(t *testing.T) {
	fx := newLastRunFixture(t, store.RunStatusFinished, native.StateInProgress)
	fx.disp.dispatch(context.Background(), tracker.Issue{
		ID: fx.issue.ID, Identifier: fx.issue.ID, Title: fx.issue.Title,
		WorkflowState: native.StateInProgress,
	})
	if fx.launched != 0 {
		t.Fatalf("finished last_run launched %d fresh run(s), want 0", fx.launched)
	}
	got, err := fx.board.Get(fx.issue.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.State != "review" {
		t.Errorf("card state = %q, want review (completed_state)", got.State)
	}
	if got.LastRunID != fx.runID {
		t.Errorf("last_run = %q, want the finished run", got.LastRunID)
	}
}

func TestDispatch_ResumesFailedResumableSameID(t *testing.T) {
	fx := newLastRunFixture(t, store.RunStatusFailedResumable, native.StateInProgress)
	if _, _, err := fx.disp.workspaces.CreateForRun(fx.issue.ID, fx.runID); err != nil {
		t.Fatalf("CreateForRun: %v", err)
	}
	fx.disp.dispatch(context.Background(), tracker.Issue{
		ID: fx.issue.ID, Identifier: fx.issue.ID, Title: fx.issue.Title,
		WorkflowState: native.StateInProgress,
	})
	fx.disp.workersWG.Wait()
	if fx.launched != 1 {
		t.Fatalf("resumable last_run launched %d time(s), want 1 resume", fx.launched)
	}
	if fx.lastSpec.RunID != fx.runID || fx.lastSpec.ResumeFromRunID != fx.runID {
		t.Errorf("spec run=%q resumeFrom=%q, want both %q", fx.lastSpec.RunID, fx.lastSpec.ResumeFromRunID, fx.runID)
	}
}

func TestDispatch_RefusesFreshSiblingWhenWorkspaceUnmanaged(t *testing.T) {
	fx := newLastRunFixture(t, store.RunStatusFailedResumable, native.StateInProgress)
	// A directory exists but no v2 ownership marker — the historic
	// "starting fresh" path. Must defer, not mint.
	legacy := filepath.Join(fx.disp.workspaces.root, "not-a-v2-workspace")
	if err := os.MkdirAll(legacy, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	fx.disp.dispatch(context.Background(), tracker.Issue{
		ID: fx.issue.ID, Identifier: fx.issue.ID, Title: fx.issue.Title,
		WorkflowState: native.StateInProgress,
	})
	if fx.launched != 0 {
		t.Fatalf("unmanaged workspace launched %d fresh run(s), want 0", fx.launched)
	}
	if _, skipped := fx.disp.state.dispatchSkips[fx.issue.ID]; !skipped {
		t.Error("unmanaged resume must surface as a dispatch skip")
	}
	got, err := fx.board.Get(fx.issue.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.LastRunID != fx.runID {
		t.Errorf("last_run = %q, want the existing run", got.LastRunID)
	}
}

type lastRunFixture struct {
	disp     *Dispatcher
	board    *native.Store
	issue    *native.Issue
	runID    string
	launched int
	lastSpec DispatchSpec
}

func newLastRunFixture(t *testing.T, status store.RunStatus, cardState string) *lastRunFixture {
	t.Helper()
	dir := t.TempDir()
	runStore, err := store.New(dir)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	const runID = "run-keep-me"
	run, err := runStore.CreateRun(context.Background(), runID, "wf", nil)
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	run.Status = status
	if err := runStore.SaveRun(context.Background(), run); err != nil {
		t.Fatalf("SaveRun: %v", err)
	}
	ns, err := native.NewStore(filepath.Join(dir, "dispatcher"))
	if err != nil {
		t.Fatalf("native.NewStore: %v", err)
	}
	iss, err := ns.Create(native.Issue{Title: "ulysse", State: cardState})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := ns.SetLastRun(iss.ID, runID, ""); err != nil {
		t.Fatalf("SetLastRun: %v", err)
	}
	fx := &lastRunFixture{board: ns, issue: iss, runID: runID}
	runner := &StubRunner{Handler: func(_ context.Context, spec DispatchSpec) error {
		fx.lastSpec = spec
		fx.launched++
		return nil
	}}
	cfg := &Config{
		Name:     "test",
		Workflow: filepath.Join(t.TempDir(), "fake.bot"),
		Tracker:  TrackerConfig{Kind: "native"},
		Polling:  PollingConfig{IntervalMS: 50},
		Agent: AgentConfig{
			MaxConcurrent:  4,
			RunningState:   "in_progress",
			CompletedState: "review",
			FailedState:    "blocked",
		},
		Workspace: WorkspaceConfig{Root: filepath.Join(dir, "ws")},
	}
	cfg.applyDefaults()
	ws, err := NewWorkspaces(cfg.Workspace.Root)
	if err != nil {
		t.Fatalf("NewWorkspaces: %v", err)
	}
	c, err := New(Options{
		Config: cfg, Tracker: native.NewAdapter(ns), Runner: runner,
		Workspaces: ws, Logger: iterlog.New(iterlog.LevelError, &bytes.Buffer{}),
		HostMarker: "test-host-1", StoreDir: dir,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	fx.disp = c
	return fx
}
