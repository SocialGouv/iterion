package dispatcher

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/dispatcher/tracker"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/runtime"
)

// TestFinishRun_PausedForInputIsParkedNotRetried is the regression guard
// for the dispatcher's handling of a run that suspends for input.
//
// eng.Run returns runtime.ErrRunPaused when a bot reaches a human node
// (status paused_waiting_human) and runtime.ErrRunPausedOperator on an
// operator soft-pause (status paused_operator). Both are valid
// checkpoints with a pending interaction — NOT failures. The dispatcher
// must NOT treat them as failures: doing so scheduled a retry that re-ran
// FRESH (paused runs aren't in resumableRunID's set), re-hit the same
// pause, and eventually exhausted attempts into FailedState ("blocked"),
// bouncing the ticket and burying the bot's escalation question.
//
// Correct behaviour: park the issue — keep the tracker claim (so the next
// tick can't re-dispatch it; ListCandidates only returns unclaimed
// issues), leave its state untouched (no revert, no move to blocked), and
// schedule no retry. The operator resumes from the run console.
func TestFinishRun_PausedForInputIsParkedNotRetried(t *testing.T) {
	const issueID = "fake:paused-1"

	cases := []struct {
		name        string
		err         error
		maxAttempts int // exercises the give-up arm (1) and the retry arm (0/5)
	}{
		{"human pause, unbounded retries", runtime.ErrRunPaused, 0},
		{"operator pause, unbounded retries", runtime.ErrRunPausedOperator, 0},
		{"human pause, attempts exhausted (old code → blocked)", runtime.ErrRunPaused, 1},
		{"human pause, attempts remain (old code → retry)", runtime.ErrRunPaused, 5},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ft := newFakeTracker()
			// The issue is mid-flight: dispatch already moved it to the
			// running state and took the claim.
			ft.add(tracker.Issue{
				ID: issueID, Identifier: "fake#paused-1",
				Title: "needs a human decision", WorkflowState: "in_progress",
			})
			if err := ft.Claim(context.Background(), issueID, "test"); err != nil {
				t.Fatalf("seed claim: %v", err)
			}

			dir := t.TempDir()
			wsDir := filepath.Join(dir, "ws")
			cfg := &Config{
				Name:     "test",
				Workflow: filepath.Join(t.TempDir(), "fake.bot"),
				Tracker:  TrackerConfig{Kind: "fake"},
				Polling:  PollingConfig{IntervalMS: 50},
				Agent: AgentConfig{
					MaxConcurrent:     4,
					MaxRetryBackoffMS: 1000,
					RunningState:      "in_progress",
					FailedState:       "blocked",
					MaxAttempts:       tc.maxAttempts,
				},
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
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}

			c.state.running[issueID] = &runningEntry{
				IssueID:               issueID,
				Identifier:            "fake#paused-1",
				RunID:                 "run-paused-1",
				WorkflowState:         "in_progress",
				WorkspacePath:         filepath.Join(wsDir, "fake_paused-1"),
				StartedAt:             time.Now(),
				Attempt:               0,
				TransitionedFromState: "ready",
			}
			c.state.slotsByState["in_progress"] = 1

			c.finishRun(context.Background(), issueID, tc.err)

			// 1) The claim must be RETAINED — this is what stops the next
			//    tick re-dispatching the parked issue.
			ft.mu.Lock()
			_, claimed := ft.claims[issueID]
			ft.mu.Unlock()
			if !claimed {
				t.Errorf("claim was released — a parked issue would be re-dispatched on the next tick")
			}

			// 2) No retry may be scheduled.
			if _, queued := c.state.retries[issueID]; queued {
				t.Errorf("a retry was scheduled for a paused run — re-runs would re-hit the same pause")
			}

			// 3) The issue moves to the dedicated "awaiting input" column —
			//    NOT to FailedState ("blocked") and NOT reverted to its source
			//    state ("ready"). A pause is not a failure.
			states, err := ft.RefreshStates(context.Background(), []string{issueID})
			if err != nil {
				t.Fatalf("RefreshStates: %v", err)
			}
			if got := states[issueID]; got != native.StateAwaitingInput {
				t.Errorf("issue state = %q, want %q (paused run moves to the awaiting-input column, not blocked/reverted)", got, native.StateAwaitingInput)
			}

			// 4) The slot must be freed and the entry dropped from running.
			if _, stillRunning := c.state.running[issueID]; stillRunning {
				t.Errorf("issue still tracked as running after pause")
			}
			if c.state.slotsByState["in_progress"] != 0 {
				t.Errorf("slot not freed: slotsByState[in_progress] = %d, want 0", c.state.slotsByState["in_progress"])
			}
		})
	}
}

// TestFinishRun_SourceChangedParksNoSibling: a doomed resume (bot
// source changed) must not mint a sibling from entry. The ticket stays
// claimed on the same last_run so the operator can resume --force.
func TestFinishRun_SourceChangedParksNoSibling(t *testing.T) {
	const issueID = "fake:source-changed-1"
	ft := newFakeTracker()
	ft.add(tracker.Issue{
		ID: issueID, Identifier: "fake#src",
		Title: "bot edited", WorkflowState: "in_progress",
	})
	if err := ft.Claim(context.Background(), issueID, "test"); err != nil {
		t.Fatalf("seed claim: %v", err)
	}

	dir := t.TempDir()
	wsDir := filepath.Join(dir, "ws")
	cfg := &Config{
		Name:     "test",
		Workflow: filepath.Join(t.TempDir(), "fake.bot"),
		Tracker:  TrackerConfig{Kind: "fake"},
		Polling:  PollingConfig{IntervalMS: 50},
		Agent: AgentConfig{
			MaxConcurrent: 4, MaxRetryBackoffMS: 1000,
			RunningState: "in_progress", FailedState: "blocked",
		},
		Workspace: WorkspaceConfig{Root: wsDir},
	}
	cfg.applyDefaults()
	ws, err := NewWorkspaces(wsDir)
	if err != nil {
		t.Fatalf("NewWorkspaces: %v", err)
	}
	c, err := New(Options{
		Config: cfg, Tracker: ft, Runner: &StubRunner{},
		Workspaces: ws, Logger: iterlog.New(iterlog.LevelError, &bytes.Buffer{}),
		HostMarker: "test",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c.state.running[issueID] = &runningEntry{
		IssueID: issueID, Identifier: "fake#src", RunID: "run-src-1",
		WorkflowState: "in_progress", WorkspacePath: filepath.Join(wsDir, "x"),
		StartedAt: time.Now(), Attempt: 0, TransitionedFromState: "ready",
	}
	c.state.slotsByState["in_progress"] = 1

	c.finishRun(context.Background(), issueID, runtime.ErrWorkflowSourceChanged)

	ft.mu.Lock()
	_, claimed := ft.claims[issueID]
	ft.mu.Unlock()
	if !claimed {
		t.Error("claim was released — a source-changed ticket would be re-dispatched")
	}
	if _, queued := c.state.retries[issueID]; queued {
		t.Error("a retry was scheduled — next attempt would mint a sibling")
	}
	if _, stillRunning := c.state.running[issueID]; stillRunning {
		t.Error("issue still tracked as running")
	}
}
