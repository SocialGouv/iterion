package dispatcher

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
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
