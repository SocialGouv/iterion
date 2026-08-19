package dispatcher

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

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
			run.Source = &store.RunSource{Kind: store.RunSourceKindDispatcher, IssueID: "native:fixture"}
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
			// An out-of-band resume can pause while an earlier dispatcher
			// retry is still pending. The persisted pause supersedes it.
			retryTimer := time.AfterFunc(time.Hour, func() {})
			c.state.retries[iss.ID] = &retryEntry{
				IssueID: iss.ID, Identifier: iss.ID, Attempt: 3, Timer: retryTimer,
			}
			t.Cleanup(func() { retryTimer.Stop() })

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
			if _, retrying := c.state.retries[iss.ID]; retrying {
				t.Errorf("re-park must consume a stale pending retry")
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

// TestDispatch_MintsFreshWhenLastRunFinished: a finished last_run does
// NOT hold the card. Dragging it back to an eligible column is the
// operator's deliberate re-queue gesture — and since no surface ever
// clears last_run_id, forbidding fresh here would make a new run for
// that card unobtainable forever (review R16dca9).
func TestDispatch_MintsFreshWhenLastRunFinished(t *testing.T) {
	fx := newLastRunFixture(t, store.RunStatusFinished, native.StateInProgress)
	fx.disp.dispatch(context.Background(), tracker.Issue{
		ID: fx.issue.ID, Identifier: fx.issue.ID, Title: fx.issue.Title,
		WorkflowState: native.StateInProgress,
	})
	fx.disp.workersWG.Wait()
	if fx.launched != 1 {
		t.Fatalf("finished last_run: launched %d time(s), want 1 fresh run", fx.launched)
	}
	if fx.lastSpec.RunID == "" || fx.lastSpec.RunID == fx.runID {
		t.Errorf("spec run = %q, want a freshly minted id (not %q)", fx.lastSpec.RunID, fx.runID)
	}
	if fx.lastSpec.ResumeFromRunID != "" {
		t.Errorf("spec resumeFrom = %q, want empty (fresh run, not a resume)", fx.lastSpec.ResumeFromRunID)
	}
}

// TestDispatch_PausedLastRunNotDispatcherOwnedStaysPut: a paused run
// launched from the pipelines control center or the studio console
// (no dispatcher RunSource) must NOT have its card yanked into
// awaiting_input — the admission sweep only reconciles in_progress
// tickets, so the move would strand it. The card stays put; the guard
// still refuses the fresh sibling.
func TestDispatch_PausedLastRunNotDispatcherOwnedStaysPut(t *testing.T) {
	fx := newLastRunFixture(t, store.RunStatusPausedWaitingHuman, native.StateInProgress)
	run, err := fx.runStore.LoadRun(context.Background(), fx.runID)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	run.Source = nil // pipelines / studio launch
	if err := fx.runStore.SaveRun(context.Background(), run); err != nil {
		t.Fatalf("SaveRun: %v", err)
	}

	fx.disp.dispatch(context.Background(), tracker.Issue{
		ID: fx.issue.ID, Identifier: fx.issue.ID, Title: fx.issue.Title,
		WorkflowState: native.StateInProgress,
	})
	if fx.launched != 0 {
		t.Fatalf("paused last_run launched %d fresh run(s), want 0", fx.launched)
	}
	got, err := fx.board.Get(fx.issue.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.State != native.StateInProgress {
		t.Errorf("card state = %q, want %q (foreign paused run must not be re-parked)", got.State, native.StateInProgress)
	}
	if got.AwaitingInput {
		t.Error("awaiting-input badge must not be set on a foreign card")
	}
	if got.Claim != "" {
		t.Errorf("foreign paused hold must happen before Claim, got %q", got.Claim)
	}
	if _, skipped := fx.disp.state.dispatchSkips[fx.issue.ID]; !skipped {
		t.Error("the refused mint must surface as a dispatch skip")
	}
}

// TestDispatch_PromotesOrphanedRunningLastRun covers the --no-server
// deployment: no runview orphan reaper is in-process, so the dispatcher
// itself must promote a run left "running" by a SIGKILL/host crash
// (lock free, past the grace window) instead of holding the ticket
// forever. No checkpoint → failed → a fresh run is legitimate.
func TestDispatch_PromotesOrphanedRunningLastRun(t *testing.T) {
	fx := newLastRunFixture(t, store.RunStatusRunning, native.StateInProgress)
	run, err := fx.runStore.LoadRun(context.Background(), fx.runID)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	run.CreatedAt = time.Now().Add(-3 * time.Minute) // past the grace window
	if err := fx.runStore.SaveRun(context.Background(), run); err != nil {
		t.Fatalf("SaveRun: %v", err)
	}

	fx.disp.dispatch(context.Background(), tracker.Issue{
		ID: fx.issue.ID, Identifier: fx.issue.ID, Title: fx.issue.Title,
		WorkflowState: native.StateInProgress,
	})
	fx.disp.workersWG.Wait()
	if fx.launched != 1 {
		t.Fatalf("orphaned running last_run (no checkpoint): launched %d, want 1 fresh run", fx.launched)
	}
	got, err := fx.runStore.LoadRun(context.Background(), fx.runID)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if got.Status != store.RunStatusFailed {
		t.Errorf("orphan status = %q, want %q (no checkpoint)", got.Status, store.RunStatusFailed)
	}
}

// TestDispatch_ResumesOrphanedRunningLastRunWithCheckpoint: same dead
// owner, but the run has a checkpoint — promotion lands on
// failed_resumable and the dispatcher resumes the SAME run id.
func TestDispatch_ResumesOrphanedRunningLastRunWithCheckpoint(t *testing.T) {
	fx := newLastRunFixture(t, store.RunStatusRunning, native.StateInProgress)
	run, err := fx.runStore.LoadRun(context.Background(), fx.runID)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	run.CreatedAt = time.Now().Add(-3 * time.Minute)
	run.Checkpoint = &store.Checkpoint{}
	if err := fx.runStore.SaveRun(context.Background(), run); err != nil {
		t.Fatalf("SaveRun: %v", err)
	}
	if _, _, err := fx.disp.workspaces.CreateForRun(fx.issue.ID, fx.runID); err != nil {
		t.Fatalf("CreateForRun: %v", err)
	}

	fx.disp.dispatch(context.Background(), tracker.Issue{
		ID: fx.issue.ID, Identifier: fx.issue.ID, Title: fx.issue.Title,
		WorkflowState: native.StateInProgress,
	})
	fx.disp.workersWG.Wait()
	if fx.launched != 1 {
		t.Fatalf("orphaned running last_run (checkpoint): launched %d, want 1 resume", fx.launched)
	}
	if fx.lastSpec.RunID != fx.runID || fx.lastSpec.ResumeFromRunID != fx.runID {
		t.Errorf("spec run=%q resumeFrom=%q, want both %q", fx.lastSpec.RunID, fx.lastSpec.ResumeFromRunID, fx.runID)
	}
}

// TestDispatch_HoldsLiveLockedRun: a running/queued last_run whose lock
// is held by a live owner must be held — the dead-owner probe must
// never clobber in-flight work, even past the grace window.
func TestDispatch_HoldsLiveLockedRun(t *testing.T) {
	for _, status := range []store.RunStatus{store.RunStatusRunning, store.RunStatusQueued} {
		t.Run(string(status), func(t *testing.T) {
			fx := newLastRunFixture(t, status, native.StateInProgress)
			run, err := fx.runStore.LoadRun(context.Background(), fx.runID)
			if err != nil {
				t.Fatalf("LoadRun: %v", err)
			}
			run.CreatedAt = time.Now().Add(-3 * time.Minute)
			if err := fx.runStore.SaveRun(context.Background(), run); err != nil {
				t.Fatalf("SaveRun: %v", err)
			}
			lock, err := fx.runStore.LockRun(context.Background(), fx.runID)
			if err != nil {
				t.Fatalf("LockRun: %v", err)
			}
			defer func() { _ = lock.Unlock() }()

			fx.disp.dispatch(context.Background(), tracker.Issue{
				ID: fx.issue.ID, Identifier: fx.issue.ID, Title: fx.issue.Title,
				WorkflowState: native.StateInProgress,
			})
			if fx.launched != 0 {
				t.Fatalf("live %s last_run launched %d fresh run(s), want 0", status, fx.launched)
			}
			got, err := fx.runStore.LoadRun(context.Background(), fx.runID)
			if err != nil {
				t.Fatalf("LoadRun: %v", err)
			}
			if got.Status != status {
				t.Errorf("live run status = %q, want %q (untouched by the probe)", got.Status, status)
			}
			card, err := fx.board.Get(fx.issue.ID)
			if err != nil {
				t.Fatalf("Get card: %v", err)
			}
			if card.Claim != "" {
				t.Errorf("live-run hold must happen before Claim, got %q", card.Claim)
			}
		})
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

func TestDispatch_MintsFreshWhenResumableWorkspaceIsMissing(t *testing.T) {
	fx := newLastRunFixture(t, store.RunStatusFailedResumable, native.StateInProgress)
	// No workspace shape exists: the store survived but the workspace root
	// was ephemeral or removed. There is no foreign path to protect, and the
	// old run cannot be resumed, so dispatch must start an isolated fresh run.
	fx.disp.dispatch(context.Background(), tracker.Issue{
		ID: fx.issue.ID, Identifier: fx.issue.ID, Title: fx.issue.Title,
		WorkflowState: native.StateInProgress,
	})
	fx.disp.workersWG.Wait()
	if fx.launched != 1 {
		t.Fatalf("missing resume workspace launched %d time(s), want 1 fresh run", fx.launched)
	}
	if fx.lastSpec.RunID == "" || fx.lastSpec.RunID == fx.runID {
		t.Errorf("spec run = %q, want a fresh id distinct from %q", fx.lastSpec.RunID, fx.runID)
	}
	if fx.lastSpec.ResumeFromRunID != "" {
		t.Errorf("spec resumeFrom = %q, want empty for missing workspace", fx.lastSpec.ResumeFromRunID)
	}
}

func TestDispatch_RefusesFreshSiblingWhenWorkspaceUnmanaged(t *testing.T) {
	fx := newLastRunFixture(t, store.RunStatusFailedResumable, native.StateInProgress)
	// A directory exists but no v2 ownership marker — the historic
	// "starting fresh" path. Must defer, not mint.
	legacy := fx.disp.workspaces.PathForRun(fx.issue.ID, fx.runID)
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
	if got.Claim != "" {
		t.Errorf("unmanaged workspace hold must happen before Claim, got claim %q", got.Claim)
	}
}

type lastRunFixture struct {
	disp     *Dispatcher
	board    *native.Store
	runStore *store.FilesystemRunStore
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
	// Dispatcher-spawned runs carry a RunSource stamp — the re-park paths
	// key off it to leave pipelines/studio-launched cards to their owner.
	run.Source = &store.RunSource{Kind: store.RunSourceKindDispatcher, IssueID: "native:fixture"}
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
	fx := &lastRunFixture{board: ns, issue: iss, runID: runID, runStore: runStore}
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
