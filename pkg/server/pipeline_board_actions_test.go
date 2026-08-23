package server

import (
	"bytes"
	"context"
	"net/http"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/runview"
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

func closeTask(t *testing.T, env *pipelineBoardTestEnv, id string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost,
		env.http.URL+"/api/v1/pipeline-board/tasks/"+id+"/close", bytes.NewReader([]byte(`{}`)))
	if err != nil {
		t.Fatalf("build POST close: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST close: %v", err)
	}
	return resp
}

// Close is the release valve for the needs-attention lane: it cancels what
// is still alive, files the ticket as ABANDONED, and — because the lane and
// the reservation are both derived from that state — hands the concurrency
// slot back in the same move.
func TestPipelineBoardTaskCloseFilesTicketAndReleasesSlot(t *testing.T) {
	env := newPipelineBoardTestEnv(t)
	issue, err := env.board.Create(native.Issue{Title: "Broken pipeline", State: native.StateInProgress, Bot: "review"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	env.seedRun(t, "close-root", "review", store.RunStatusFailedResumable, func(run *store.Run) {
		run.FilePath = env.botPath
		run.Error = "kaboom"
		run.Checkpoint = &store.Checkpoint{NodeID: "implement"}
	})
	if err := env.board.SetLastRun(issue.ID, "close-root", ""); err != nil {
		t.Fatalf("SetLastRun: %v", err)
	}

	// Precondition: the card sits in the lane and holds a slot.
	before := findPipelineCard(t, env.projection(t).Cards, "run:close-root")
	if before.ColumnID != pipelineColumnNeedsAttention || !before.ReservesSlot {
		t.Fatalf("before close: column=%q reserves=%v, want needs_attention + reserved",
			before.ColumnID, before.ReservesSlot)
	}

	resp := closeTask(t, env, issue.ID)
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("close status = %d, want 200", resp.StatusCode)
	}
	resp.Body.Close()

	// ABANDONED, never done: native.BlockerSatisfied counts only `done`, so
	// closing as done would release every ticket waiting on this one into
	// work whose input was never produced.
	updated, err := env.board.Get(issue.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if updated.State != native.StateBlocked {
		t.Fatalf("closed state = %q, want %q (abandoned)", updated.State, native.StateBlocked)
	}
	// failed_resumable is terminal but NOT retired — Close must still flip it,
	// or a resumable run would keep shadowing a closed ticket.
	run, err := env.runStore(t).LoadRun(context.Background(), "close-root")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if run.Status != store.RunStatusCancelled {
		t.Errorf("run status = %q, want cancelled", run.Status)
	}
	// A terminal ticket state evicts the card from the lane, which is what
	// releases the slot.
	after := findPipelineCard(t, env.projection(t).Cards, "run:close-root")
	if after.ColumnID != pipelineColumnClosed {
		t.Errorf("after close: column = %q, want closed", after.ColumnID)
	}
	if after.ReservesSlot {
		t.Error("after close: card still reserves a slot — the valve does not release")
	}
}

// Closing twice must not double-apply anything (the studio polls every 3s,
// so a double-click is ordinary).
func TestPipelineBoardTaskCloseIsIdempotent(t *testing.T) {
	env := newPipelineBoardTestEnv(t)
	issue, err := env.board.Create(native.Issue{Title: "Twice", State: native.StateInProgress, Bot: "review"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	for i := 0; i < 2; i++ {
		resp := closeTask(t, env, issue.ID)
		code := resp.StatusCode
		resp.Body.Close()
		if code != http.StatusOK {
			t.Fatalf("close #%d status = %d, want 200", i+1, code)
		}
	}
	updated, err := env.board.Get(issue.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if updated.State != native.StateBlocked {
		t.Errorf("state = %q, want %q", updated.State, native.StateBlocked)
	}
}

// A failure iterion caused itself (drain / boot orphan sweep) still shows in
// the lane — the operator must know the run was cut — but must NOT reserve.
// Without this, every studio restart with N pipelines in flight would hold N
// slots, and under `task studio:dev` a restart is every .go save.
func TestPipelineBoardInterruptedRunDoesNotReserveASlot(t *testing.T) {
	env := newPipelineBoardTestEnv(t)
	issue, err := env.board.Create(native.Issue{Title: "Cut by a restart", State: native.StateInProgress, Bot: "review"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	env.seedRun(t, "drained-root", "review", store.RunStatusFailedResumable, func(run *store.Run) {
		run.FilePath = env.botPath
		run.Error = runview.ReasonServerDrained
	})
	if err := env.board.SetLastRun(issue.ID, "drained-root", ""); err != nil {
		t.Fatalf("SetLastRun: %v", err)
	}
	card := findPipelineCard(t, env.projection(t).Cards, "run:drained-root")
	if card.ColumnID != pipelineColumnNeedsAttention {
		t.Errorf("column = %q, want needs_attention (the operator must see it was cut)", card.ColumnID)
	}
	if card.ReservesSlot {
		t.Error("an interruption-caused failure reserved a slot — every restart would wedge the board")
	}
}

// Close must NEVER file a ticket as `done`, including through its fallback.
//
// native.DefaultBoard lists done (index 7) before blocked (index 8), so a
// plain "first terminal state" scan resolves to done the moment a board
// lacks `blocked` — and UpgradeBoardSchema backfills inbox/waiting_deps/
// awaiting_input but never blocked, while preserving customised boards.
// Landing on done fires PromoteUnblockedDependents, releasing every ticket
// parked behind a pipeline that never delivered — the precise outcome the
// confirm dialog promises will not happen.
func TestPipelineCloseTargetStateNeverResolvesToDone(t *testing.T) {
	cases := []struct {
		name   string
		states []native.State
		want   string
		wantOK bool
	}{
		{
			name:   "default board prefers blocked",
			states: native.DefaultBoard().States,
			want:   native.StateBlocked,
			wantOK: true,
		},
		{
			name: "board without blocked falls through done to the next terminal",
			states: []native.State{
				{Name: native.StateReady, Eligible: true},
				{Name: native.StateDone, Terminal: true},
				{Name: "archived", Terminal: true},
			},
			want:   "archived",
			wantOK: true,
		},
		{
			name: "board whose only terminal state is done refuses",
			states: []native.State{
				{Name: native.StateReady, Eligible: true},
				{Name: native.StateDone, Terminal: true},
			},
			wantOK: false,
		},
		{
			name: "a non-terminal blocked is not a close target",
			states: []native.State{
				{Name: native.StateBlocked},
				{Name: native.StateDone, Terminal: true},
			},
			wantOK: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := pipelineCloseTargetState(&native.Board{States: c.states})
			if ok != c.wantOK {
				t.Fatalf("ok = %v, want %v (got state %q)", ok, c.wantOK, got)
			}
			if ok && got != c.want {
				t.Errorf("state = %q, want %q", got, c.want)
			}
			if got == native.StateDone {
				t.Errorf("Close resolved to `done` — dependents would be auto-promoted")
			}
		})
	}
	if _, ok := pipelineCloseTargetState(nil); ok {
		t.Error("a nil board must not yield a close target")
	}
}

// A TICKET-backed failure enters the lane and reserves; a standalone one
// does not. The split is the whole membership rule, and each half protects
// something different: the lane must not fill with cards nobody can act on,
// and a reservation must always have a way to be released.
func TestPipelineBoardTicketFailureEntersLaneStandaloneDoesNot(t *testing.T) {
	env := newPipelineBoardTestEnv(t)
	issue, err := env.board.Create(native.Issue{Title: "Owned", State: native.StateInProgress, Bot: "review"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	env.seedRun(t, "owned-run", "review", store.RunStatusFailed, func(run *store.Run) {
		run.FilePath = env.botPath
		run.Error = "kaboom"
	})
	if err := env.board.SetLastRun(issue.ID, "owned-run", ""); err != nil {
		t.Fatalf("SetLastRun: %v", err)
	}
	env.seedRun(t, "loose-run", "review", store.RunStatusFailed, func(run *store.Run) {
		run.FilePath = env.botPath
		run.Error = "kaboom"
	})

	cards := env.projection(t).Cards
	owned := findPipelineCard(t, cards, "run:owned-run")
	if owned.ColumnID != pipelineColumnNeedsAttention || !owned.ReservesSlot {
		t.Errorf("ticket-backed: column=%q reserves=%v, want needs_attention + reserved",
			owned.ColumnID, owned.ReservesSlot)
	}
	loose := findPipelineCard(t, cards, "run:loose-run")
	if loose.ColumnID != pipelineColumnClosed {
		t.Errorf("standalone: column = %q, want closed (nothing on the board can act on it)", loose.ColumnID)
	}
	if loose.ReservesSlot {
		t.Error("standalone: reserved a slot with no affordance that could ever release it")
	}
}

// The Close dialog names the tickets that will stay parked. That guard is
// only as real as card.Blocking, which attachDeps fills — and attachDeps
// used to run for task cards only, so it was empty on exactly the lanes
// Close serves.
func TestPipelineBoardRunCardCarriesItsDependents(t *testing.T) {
	env := newPipelineBoardTestEnv(t)
	blocker, err := env.board.Create(native.Issue{Title: "Migrate the store", State: native.StateInProgress, Bot: "review"})
	if err != nil {
		t.Fatalf("Create blocker: %v", err)
	}
	dependent, err := env.board.Create(native.Issue{
		Title:    "Index the runs",
		State:    native.StateWaitingDeps,
		Bot:      "review",
		Blockers: []string{blocker.ID},
	})
	if err != nil {
		t.Fatalf("Create dependent: %v", err)
	}
	env.seedRun(t, "blocker-run", "review", store.RunStatusFailed, func(run *store.Run) {
		run.FilePath = env.botPath
		run.Error = "kaboom"
	})
	if err := env.board.SetLastRun(blocker.ID, "blocker-run", ""); err != nil {
		t.Fatalf("SetLastRun: %v", err)
	}

	card := findPipelineCard(t, env.projection(t).Cards, "run:blocker-run")
	if card.ColumnID != pipelineColumnNeedsAttention {
		t.Fatalf("column = %q, want needs_attention", card.ColumnID)
	}
	if len(card.Blocking) != 1 || card.Blocking[0].ID != dependent.ID {
		t.Fatalf("Blocking = %+v, want the one dependent %s — the Close dialog would confirm blind",
			card.Blocking, dependent.ID)
	}
	if card.Blocking[0].Title != "Index the runs" {
		t.Errorf("Blocking title = %q, want the dependent's title", card.Blocking[0].Title)
	}
}

// Retry must not lose the slot it was holding.
//
// Retry does not launch — it restages the ticket to Ready and the admission
// tick starts it up to 2s later. Releasing the reservation at restage time
// would open exactly the window the feature exists to close: another ready
// ticket, or a FIFO waiter, takes the slot the operator just freed their fix
// into. So a restaged ticket keeps holding, and its relaunch spends its own
// reservation via LaunchSpec.PipelineTicketID.
func TestPipelineBoardRestagedTicketKeepsItsSlot(t *testing.T) {
	env := newPipelineBoardTestEnv(t)
	issue, err := env.board.Create(native.Issue{Title: "Retried", State: native.StateInProgress, Bot: "review"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	env.seedRun(t, "retried-run", "review", store.RunStatusFailed, func(run *store.Run) {
		run.FilePath = env.botPath
		run.Error = "kaboom"
	})
	if err := env.board.SetLastRun(issue.ID, "retried-run", ""); err != nil {
		t.Fatalf("SetLastRun: %v", err)
	}

	// Operator hits Retry → the ticket is restaged, the card moves to Opened.
	if _, err := env.board.SetState(issue.ID, native.StateReady); err != nil {
		t.Fatalf("SetState(ready): %v", err)
	}
	card := findPipelineCard(t, env.projection(t).Cards, "task:"+issue.ID)
	if card.ColumnID != pipelineColumnOpened {
		t.Fatalf("column = %q, want opened (restaged for relaunch)", card.ColumnID)
	}
	if !card.ReservesSlot {
		t.Error("the restaged ticket dropped its slot before relaunching — another card can take it during the admission window")
	}

	// Parked on a dependency instead: it is not going to launch soon, so
	// holding capacity for it would be an unbounded leak.
	if _, err := env.board.SetState(issue.ID, native.StateWaitingDeps); err != nil {
		t.Fatalf("SetState(waiting_deps): %v", err)
	}
	parked := findPipelineCard(t, env.projection(t).Cards, "task:"+issue.ID)
	if parked.ReservesSlot {
		t.Error("a ticket parked on deps held a slot it cannot use")
	}
}

// A dispatcher GIVE-UP is not an operator's filing. The dispatcher's own
// failed_state (default "blocked") and the board's Close target are the same
// terminal state, so before the give-up stamp the projection could not tell
// them apart and filed an unattended failure as acknowledged history — the
// one class of failure the lane exists for was the one it never showed
// (issue #494). Each sub-case pins one half of the discrimination.
func TestPipelineBoardDispatcherGiveUpEntersLane(t *testing.T) {
	cases := []struct {
		name       string
		stamp      func(runID string) *native.GiveUp
		state      string
		wantColumn string
		wantStamp  bool
	}{
		{
			name: "give-up on this run in this state surfaces",
			stamp: func(runID string) *native.GiveUp {
				return &native.GiveUp{RunID: runID, State: native.StateBlocked, Attempts: 3}
			},
			state:      native.StateBlocked,
			wantColumn: pipelineColumnNeedsAttention,
			wantStamp:  true,
		},
		{
			name:       "no stamp: the operator filed it, so it stays history",
			stamp:      func(string) *native.GiveUp { return nil },
			state:      native.StateBlocked,
			wantColumn: pipelineColumnClosed,
		},
		{
			// An older attempt was given up on; the card shows a NEWER run.
			// Attributing the old give-up to it would explain the wrong run.
			name: "stamp for another run does not travel to this card",
			stamp: func(string) *native.GiveUp {
				return &native.GiveUp{RunID: "some-older-run", State: native.StateBlocked, Attempts: 3}
			},
			state:      native.StateBlocked,
			wantColumn: pipelineColumnClosed,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			env := newPipelineBoardTestEnv(t)
			issue, err := env.board.Create(native.Issue{Title: "Deterministic failure", State: native.StateInProgress, Bot: "review"})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			env.seedRun(t, "gaveup-run", "review", store.RunStatusFailedResumable, func(run *store.Run) {
				run.FilePath = env.botPath
				run.Error = `[EXECUTION_FAILED] node "record_contracts_incomplete"`
			})
			if err := env.board.SetLastRun(issue.ID, "gaveup-run", ""); err != nil {
				t.Fatalf("SetLastRun: %v", err)
			}
			// File the ticket FIRST, then stamp — the dispatcher's own order,
			// and the only one that survives: moving a ticket expires any
			// stamp on it.
			if _, err := env.board.SetState(issue.ID, c.state); err != nil {
				t.Fatalf("SetState(%s): %v", c.state, err)
			}
			if stamp := c.stamp("gaveup-run"); stamp != nil {
				if err := env.board.SetGaveUp(issue.ID, stamp); err != nil {
					t.Fatalf("SetGaveUp: %v", err)
				}
			}

			card := findPipelineCard(t, env.projection(t).Cards, "run:gaveup-run")
			if card.ColumnID != c.wantColumn {
				t.Errorf("column = %q, want %q (ticket %s)", card.ColumnID, c.wantColumn, c.state)
			}
			// Whatever the lane, a TERMINAL ticket reserves nothing: nothing
			// will relaunch it until the operator acts, so holding capacity
			// for it would leak a slot with no bound.
			if card.ReservesSlot {
				t.Error("a terminal ticket reserved a concurrency slot — an unreleasable hold")
			}
			if (card.GaveUp != nil) != c.wantStamp {
				t.Fatalf("card.GaveUp = %+v, want present=%v", card.GaveUp, c.wantStamp)
			}
			if c.wantStamp && card.GaveUp.Attempts != 3 {
				t.Errorf("card.GaveUp.Attempts = %d, want the 3 attempts that were burned", card.GaveUp.Attempts)
			}
		})
	}
}

// Close is the acknowledgement a give-up never got. It files the ticket into
// the state the give-up ALREADY wrote, so the state change is a no-op there
// and only an explicit clear of the stamp can move the card out of the lane.
func TestPipelineBoardCloseAcknowledgesAGiveUp(t *testing.T) {
	env := newPipelineBoardTestEnv(t)
	issue, err := env.board.Create(native.Issue{Title: "Given up on", State: native.StateInProgress, Bot: "review"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// `failed` (NOT failed_resumable): the close sweep leaves an already-hard-
	// failed run alone, so nothing but the cleared stamp can change the lane.
	env.seedRun(t, "ack-run", "review", store.RunStatusFailed, func(run *store.Run) {
		run.FilePath = env.botPath
		run.Error = "kaboom"
	})
	if err := env.board.SetLastRun(issue.ID, "ack-run", ""); err != nil {
		t.Fatalf("SetLastRun: %v", err)
	}
	if _, err := env.board.SetState(issue.ID, native.StateBlocked); err != nil {
		t.Fatalf("SetState: %v", err)
	}
	if err := env.board.SetGaveUp(issue.ID, &native.GiveUp{RunID: "ack-run", State: native.StateBlocked, Attempts: 3}); err != nil {
		t.Fatalf("SetGaveUp: %v", err)
	}
	if card := findPipelineCard(t, env.projection(t).Cards, "run:ack-run"); card.ColumnID != pipelineColumnNeedsAttention {
		t.Fatalf("column before close = %q, want needs_attention", card.ColumnID)
	}

	resp := closeTask(t, env, issue.ID)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("close status = %d, want 200", resp.StatusCode)
	}

	filed, err := env.board.Get(issue.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if filed.GaveUp != nil {
		t.Errorf("give-up stamp survived Close: %+v — the card would never leave the lane", filed.GaveUp)
	}
	// The response must not contradict the write it just made: an API client
	// reading `task` would otherwise still see the stamp this call dropped.
	var body struct {
		Task *native.Issue `json:"task"`
	}
	decodeJSONResp(t, resp, &body)
	if body.Task == nil || body.Task.GaveUp != nil {
		t.Errorf("close response task = %+v, want the cleared ticket", body.Task)
	}
	if card := findPipelineCard(t, env.projection(t).Cards, "run:ack-run"); card.ColumnID != pipelineColumnClosed {
		t.Errorf("column after close = %q, want closed", card.ColumnID)
	}
}

// The projection's own staleness guard, exercised directly.
//
// It cannot be reached through the store — SetGaveUp records the state the
// ticket is actually in, and any move expires the stamp — which is exactly
// why it is worth pinning here: the lane must not depend on the store having
// remembered. A stamp naming a state the ticket is not in describes nothing,
// and a card it pinned to the lane would be unexplainable.
func TestPipelineLaneStaleGiveUpStampDoesNotPinTheLane(t *testing.T) {
	terminal := map[string]struct{}{
		native.StateBlocked: {},
		native.StateDone:    {},
	}
	root := &store.Run{ID: "run-a", Status: store.RunStatusFailedResumable}
	cases := []struct {
		name  string
		issue *native.Issue
		want  string
	}{
		{
			name: "stamp describes this ticket and this run",
			issue: &native.Issue{
				ID: "i", Bot: "review", State: native.StateBlocked,
				GaveUp: &native.GiveUp{RunID: "run-a", State: native.StateBlocked, Attempts: 3},
			},
			want: pipelineColumnNeedsAttention,
		},
		{
			name: "stamp names a state the ticket is not in",
			issue: &native.Issue{
				ID: "i", Bot: "review", State: native.StateDone,
				GaveUp: &native.GiveUp{RunID: "run-a", State: native.StateBlocked, Attempts: 3},
			},
			want: pipelineColumnClosed,
		},
		{
			name: "stamp names another run",
			issue: &native.Issue{
				ID: "i", Bot: "review", State: native.StateBlocked,
				GaveUp: &native.GiveUp{RunID: "run-older", State: native.StateBlocked, Attempts: 3},
			},
			want: pipelineColumnClosed,
		},
		{
			name: "stamp names no run at all",
			issue: &native.Issue{
				ID: "i", Bot: "review", State: native.StateBlocked,
				GaveUp: &native.GiveUp{State: native.StateBlocked, Attempts: 3},
			},
			want: pipelineColumnClosed,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			column, reserves := pipelineLaneForRoot(root, c.issue, terminal, nil)
			if column != c.want {
				t.Errorf("column = %q, want %q", column, c.want)
			}
			if reserves {
				t.Error("a terminal ticket reserved a slot nothing can release")
			}
		})
	}
}
