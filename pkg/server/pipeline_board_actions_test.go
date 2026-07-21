package server

import (
	"bytes"
	"context"
	"net/http"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The first lane is "opened" (pairs with "closed"); readiness is a per-card
// badge, not a separate lane.
func TestPipelineBoardOpenedLaneTitle(t *testing.T) {
	env := newPipelineBoardTestEnv(t)
	for _, column := range env.projection(t).Columns {
		if column.ID == pipelineColumnOpened {
			if column.Title != "Opened" {
				t.Errorf("opened lane title = %q, want %q", column.Title, "Opened")
			}
			return
		}
	}
	t.Fatalf("no %q column in projection", pipelineColumnOpened)
}

func deleteTask(t *testing.T, env *pipelineBoardTestEnv, id string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodDelete, env.http.URL+"/api/v1/pipeline-board/tasks/"+id, nil)
	if err != nil {
		t.Fatalf("build DELETE: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE: %v", err)
	}
	return resp
}

func resetTask(t *testing.T, env *pipelineBoardTestEnv, id string) *http.Response {
	t.Helper()
	resp, err := http.Post(env.http.URL+"/api/v1/pipeline-board/tasks/"+id+"/reset", "application/json", bytes.NewBufferString(`{}`))
	if err != nil {
		t.Fatalf("POST reset: %v", err)
	}
	return resp
}

// A Backlog ticket with no runs deletes cleanly and leaves the board.
func TestPipelineBoardTaskDeleteRemovesBacklogTicket(t *testing.T) {
	env := newPipelineBoardTestEnv(t)
	issue, err := env.board.Create(native.Issue{Title: "Never launched", State: native.StateInbox, Bot: "review"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	resp := deleteTask(t, env, issue.ID)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204", resp.StatusCode)
	}
	if _, err := env.board.Get(issue.ID); err == nil {
		t.Errorf("issue %s still exists after delete", issue.ID)
	}
	for _, card := range env.projection(t).Cards {
		if card.IssueID == issue.ID {
			t.Errorf("deleted ticket still projected: %+v", card)
		}
	}
}

// Deleting a ticket whose run tree still has an active run is refused —
// a live pipeline can never be silently detached from its ticket. The
// guard covers the root (LastRunID) and descendants reached through
// ParentRunID.
func TestPipelineBoardTaskDeleteRefusedWhileRunActive(t *testing.T) {
	env := newPipelineBoardTestEnv(t)

	t.Run("running root", func(t *testing.T) {
		issue, err := env.board.Create(native.Issue{Title: "Live root", State: native.StateInProgress, Bot: "review"})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		env.seedRun(t, "del-live-root", "review", store.RunStatusRunning, func(run *store.Run) {
			run.FilePath = env.botPath
		})
		if err := env.board.SetLastRun(issue.ID, "del-live-root", ""); err != nil {
			t.Fatalf("SetLastRun: %v", err)
		}
		resp := deleteTask(t, env, issue.ID)
		resp.Body.Close()
		if resp.StatusCode != http.StatusConflict {
			t.Fatalf("delete status = %d, want 409", resp.StatusCode)
		}
		if _, err := env.board.Get(issue.ID); err != nil {
			t.Errorf("issue vanished despite refused delete: %v", err)
		}
	})

	t.Run("paused descendant of a terminal root", func(t *testing.T) {
		issue, err := env.board.Create(native.Issue{Title: "Live child", State: native.StateInProgress, Bot: "review"})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		env.seedRun(t, "del-done-root", "review", store.RunStatusFailed, func(run *store.Run) {
			run.FilePath = env.botPath
		})
		env.seedRun(t, "del-paused-child", "review", store.RunStatusPausedWaitingHuman, func(run *store.Run) {
			run.FilePath = env.botPath
			run.ParentRunID = "del-done-root"
			run.Checkpoint = &store.Checkpoint{NodeID: "approval", InteractionID: "int-1"}
		})
		if err := env.board.SetLastRun(issue.ID, "del-done-root", ""); err != nil {
			t.Fatalf("SetLastRun: %v", err)
		}
		resp := deleteTask(t, env, issue.ID)
		resp.Body.Close()
		if resp.StatusCode != http.StatusConflict {
			t.Fatalf("delete status = %d, want 409", resp.StatusCode)
		}
	})
}

// Once every attempt is terminal the ticket deletes; its runs stay in the
// store untouched (delete removes the ISSUE only, never a run).
func TestPipelineBoardTaskDeleteAllowedAfterTerminalRuns(t *testing.T) {
	env := newPipelineBoardTestEnv(t)
	issue, err := env.board.Create(native.Issue{Title: "Old attempt", State: native.StateInbox, Bot: "review"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	env.seedRun(t, "del-terminal", "review", store.RunStatusCancelled, func(run *store.Run) {
		run.FilePath = env.botPath
	})
	if err := env.board.SetLastRun(issue.ID, "del-terminal", ""); err != nil {
		t.Fatalf("SetLastRun: %v", err)
	}
	resp := deleteTask(t, env, issue.ID)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204", resp.StatusCode)
	}
	if _, err := env.runStore(t).LoadRun(context.Background(), "del-terminal"); err != nil {
		t.Errorf("run deleted alongside the ticket: %v", err)
	}
}

// Reset cancels every parked run in the ticket's tree (root + descendant)
// and restages the ticket to Ready so the admission loop relaunches it
// fresh.
func TestPipelineBoardTaskResetCancelsParkedTreeAndRestages(t *testing.T) {
	env := newPipelineBoardTestEnv(t)
	issue, err := env.board.Create(native.Issue{Title: "Stuck pipeline", State: native.StateInProgress, Bot: "review"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	env.seedRun(t, "reset-root", "review", store.RunStatusPausedWaitingHuman, func(run *store.Run) {
		run.FilePath = env.botPath
		run.Checkpoint = &store.Checkpoint{NodeID: "approval", InteractionID: "int-root"}
	})
	env.seedRun(t, "reset-child", "review", store.RunStatusPausedWaitingHuman, func(run *store.Run) {
		run.FilePath = env.botPath
		run.ParentRunID = "reset-root"
		run.Checkpoint = &store.Checkpoint{NodeID: "approval", InteractionID: "int-child"}
	})
	if err := env.board.SetLastRun(issue.ID, "reset-root", ""); err != nil {
		t.Fatalf("SetLastRun: %v", err)
	}

	resp := resetTask(t, env, issue.ID)
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("reset status = %d, want 200", resp.StatusCode)
	}
	var updated native.Issue
	decodeJSONResp(t, resp, &updated)
	if updated.State != native.StateReady {
		t.Errorf("reset state = %q, want %q", updated.State, native.StateReady)
	}
	rs := env.runStore(t)
	for _, id := range []string{"reset-root", "reset-child"} {
		run, err := rs.LoadRun(context.Background(), id)
		if err != nil {
			t.Fatalf("LoadRun(%s): %v", id, err)
		}
		if run.Status != store.RunStatusCancelled {
			t.Errorf("run %s status = %q, want cancelled", id, run.Status)
		}
	}
}

// A run persisted as "running" but held by no process (another process, or
// a stale crash record) cannot be cancelled from here: reset refuses and
// leaves both the ticket and the run untouched.
func TestPipelineBoardTaskResetRefusedWhenRunNotCancellable(t *testing.T) {
	env := newPipelineBoardTestEnv(t)
	issue, err := env.board.Create(native.Issue{Title: "Foreign run", State: native.StateInProgress, Bot: "review"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	env.seedRun(t, "reset-foreign", "review", store.RunStatusRunning, func(run *store.Run) {
		run.FilePath = env.botPath
	})
	if err := env.board.SetLastRun(issue.ID, "reset-foreign", ""); err != nil {
		t.Fatalf("SetLastRun: %v", err)
	}

	resp := resetTask(t, env, issue.ID)
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("reset status = %d, want 409", resp.StatusCode)
	}
	after, err := env.board.Get(issue.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if after.State != native.StateInProgress {
		t.Errorf("issue state = %q, want untouched %q", after.State, native.StateInProgress)
	}
	run, err := env.runStore(t).LoadRun(context.Background(), "reset-foreign")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if run.Status != store.RunStatusRunning {
		t.Errorf("run status = %q, want untouched running", run.Status)
	}
}

func launchTask(t *testing.T, env *pipelineBoardTestEnv, id string) *http.Response {
	t.Helper()
	resp, err := http.Post(env.http.URL+"/api/v1/pipeline-board/tasks/"+id+"/launch", "application/json", bytes.NewBufferString(`{}`))
	if err != nil {
		t.Fatalf("POST launch: %v", err)
	}
	return resp
}

// The Opened → In progress drag overrides the launch ORDER, never the
// guards that keep the board honest. A ticket already carrying an active
// run must be refused: the drop must not detach or duplicate that run.
func TestPipelineBoardTaskLaunchRefusedWhenRunActive(t *testing.T) {
	env := newPipelineBoardTestEnv(t)
	issue, err := env.board.Create(native.Issue{Title: "Already live", State: native.StateInProgress, Bot: "review"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	env.seedRun(t, "run-live", "review", store.RunStatusRunning, nil)
	if err := env.board.SetLastRun(issue.ID, "run-live", ""); err != nil {
		t.Fatal(err)
	}
	resp := launchTask(t, env, issue.ID)
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("launch status = %d, want 409 while a run is active", resp.StatusCode)
	}
}

// Hard dependencies are correctness, not ranking — the drag bypasses
// priority, never an unfinished blocker.
func TestPipelineBoardTaskLaunchRefusedWithOpenBlocker(t *testing.T) {
	env := newPipelineBoardTestEnv(t)
	blocker, err := env.board.Create(native.Issue{Title: "Upstream", State: native.StateInbox, Bot: "review"})
	if err != nil {
		t.Fatalf("Create blocker: %v", err)
	}
	issue, err := env.board.Create(native.Issue{
		Title:    "Blocked",
		State:    native.StateInbox,
		Bot:      "review",
		Blockers: []string{blocker.ID},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	resp := launchTask(t, env, issue.ID)
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("launch status = %d, want 409 with an open blocker", resp.StatusCode)
	}
	got, err := env.board.Get(issue.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != native.StateInbox {
		t.Fatalf("refused launch mutated the ticket: state = %q", got.State)
	}
}

// A ticket with no bot is not a pipeline ticket at all.
func TestPipelineBoardTaskLaunchRefusedWithoutBot(t *testing.T) {
	env := newPipelineBoardTestEnv(t)
	issue, err := env.board.Create(native.Issue{Title: "Note to self", State: native.StateInbox})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	resp := launchTask(t, env, issue.ID)
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("launch status = %d, want 409 for a bot-less ticket", resp.StatusCode)
	}
}

func TestPipelineBoardTaskLaunchUnknownTicket(t *testing.T) {
	env := newPipelineBoardTestEnv(t)
	resp := launchTask(t, env, "does-not-exist")
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("launch status = %d, want 404", resp.StatusCode)
	}
}
