package server

import (
	"context"
	"fmt"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The admission loop owns the full lifecycle of the tickets it launches:
// launchTicketNow moves a ticket to in_progress, and reconcileFinishedTickets
// must file it into done once its run finishes cleanly. Without that second
// half the ticket strands in in_progress forever and every dependent parks in
// waiting_deps — native.BlockerSatisfied counts ONLY done. These tests pin
// the sweep's contract against a real filesystem board.

// fakeSweepRunStore serves LoadRun from a map; everything else is unreachable
// in these tests (nil via the embedded interface).
type fakeSweepRunStore struct {
	store.RunStore
	runs map[string]*store.Run
}

func (f *fakeSweepRunStore) LoadRun(_ context.Context, id string) (*store.Run, error) {
	r, ok := f.runs[id]
	if !ok {
		return nil, fmt.Errorf("run %s not found", id)
	}
	return r, nil
}

func newSweepTestServer() *Server {
	return &Server{logger: iterlog.New(iterlog.LevelError, nil)}
}

func TestReconcileFinishedTickets_FilesDoneAndUnblocksDependents(t *testing.T) {
	board, err := native.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	blocker, _ := board.Create(native.Issue{Title: "epic", State: native.StateInProgress, Bot: "town-dev"})
	dependent, _ := board.Create(native.Issue{
		Title: "next epic", State: native.StateWaitingDeps, Bot: "town-dev",
		Blockers: []string{blocker.ID},
	})
	if err := board.SetLastRun(blocker.ID, "run-1", ""); err != nil {
		t.Fatal(err)
	}
	rs := &fakeSweepRunStore{runs: map[string]*store.Run{
		"run-1": {ID: "run-1", Status: store.RunStatusFinished},
	}}

	issues, err := board.List(native.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	newSweepTestServer().reconcileFinishedTickets(context.Background(), board, rs, issues)

	got, _ := board.Get(blocker.ID)
	if got.State != native.StateDone {
		t.Fatalf("finished ticket state = %q, want done", got.State)
	}
	// SetState(done) must cascade: the satisfied dependent leaves waiting_deps.
	dep, _ := board.Get(dependent.ID)
	if dep.State != native.StateBacklog {
		t.Fatalf("dependent state = %q, want backlog (auto-promoted)", dep.State)
	}
}

func TestReconcileFinishedTickets_LeavesNonFinishedRuns(t *testing.T) {
	statuses := map[string]store.RunStatus{
		"run-running":   store.RunStatusRunning,
		"run-paused":    store.RunStatusPausedWaitingHuman,
		"run-failed":    store.RunStatusFailed,
		"run-resumable": store.RunStatusFailedResumable,
		"run-cancelled": store.RunStatusCancelled,
	}
	for runID, st := range statuses {
		t.Run(string(st), func(t *testing.T) {
			board, err := native.NewStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			iss, _ := board.Create(native.Issue{Title: "epic", State: native.StateInProgress, Bot: "town-dev"})
			if err := board.SetLastRun(iss.ID, runID, ""); err != nil {
				t.Fatal(err)
			}
			rs := &fakeSweepRunStore{runs: map[string]*store.Run{
				runID: {ID: runID, Status: st},
			}}
			issues, _ := board.List(native.ListFilter{})
			newSweepTestServer().reconcileFinishedTickets(context.Background(), board, rs, issues)

			got, _ := board.Get(iss.ID)
			if got.State != native.StateInProgress {
				t.Fatalf("status %s: ticket state = %q, want in_progress (operator-owned)", st, got.State)
			}
		})
	}
}

func TestReconcileFinishedTickets_SkipsUnlinkedAndMissingRuns(t *testing.T) {
	board, err := native.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// In-progress but never launched: no last run to consult.
	noRun, _ := board.Create(native.Issue{Title: "manual", State: native.StateInProgress, Bot: "town-dev"})
	// Linked to a run record the store can no longer serve.
	gone, _ := board.Create(native.Issue{Title: "stale link", State: native.StateInProgress, Bot: "town-dev"})
	if err := board.SetLastRun(gone.ID, "run-vanished", ""); err != nil {
		t.Fatal(err)
	}
	rs := &fakeSweepRunStore{runs: map[string]*store.Run{}}

	issues, _ := board.List(native.ListFilter{})
	newSweepTestServer().reconcileFinishedTickets(context.Background(), board, rs, issues)

	for _, id := range []string{noRun.ID, gone.ID} {
		got, _ := board.Get(id)
		if got.State != native.StateInProgress {
			t.Fatalf("ticket %s state = %q, want in_progress (best-effort skip)", id, got.State)
		}
	}
}
