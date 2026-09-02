package server

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

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
// in these tests (nil via the embedded interface). listCalls counts the full
// store scans — the sweep's contract is that an idle board pays none.
type fakeSweepRunStore struct {
	store.RunStore
	runs      map[string]*store.Run
	listCalls int
}

func (f *fakeSweepRunStore) LoadRun(_ context.Context, id string) (*store.Run, error) {
	r, ok := f.runs[id]
	if !ok {
		return nil, fmt.Errorf("run %s not found", id)
	}
	return r, nil
}

func (f *fakeSweepRunStore) ListRuns(_ context.Context) ([]string, error) {
	f.listCalls++
	ids := make([]string, 0, len(f.runs))
	for id := range f.runs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids, nil
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

// A dispatcher run that fails and is recovered via fork: the fork never
// becomes LastRunID on its own, so without the adoption the ticket would
// strand in in_progress (dependents parked in waiting_deps) while the
// card already reads Closed. The sweep must adopt the newest fork that
// ACTUALLY finished — a parked, never-resumed fork (cancelled, no
// FinishedAt) has delivered nothing and must not qualify.
func TestReconcileFinishedTickets_AdoptsFinishedFork(t *testing.T) {
	board, err := native.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	blocker, _ := board.Create(native.Issue{Title: "epic", State: native.StateInProgress, Bot: "town-dev"})
	dependent, _ := board.Create(native.Issue{
		Title: "next epic", State: native.StateWaitingDeps, Bot: "town-dev",
		Blockers: []string{blocker.ID},
	})
	if err := board.SetLastRun(blocker.ID, "run-parent", ""); err != nil {
		t.Fatal(err)
	}
	older := time.Now().Add(-time.Hour)
	ended := older.Add(50 * time.Minute)
	rs := &fakeSweepRunStore{runs: map[string]*store.Run{
		"run-parent": {
			ID: "run-parent", Status: store.RunStatusFailed, CreatedAt: older,
			Source: &store.RunSource{Kind: store.RunSourceKindDispatcher, IssueID: blocker.ID},
		},
		"run-fork": {
			ID: "run-fork", Status: store.RunStatusFinished, CreatedAt: older.Add(30 * time.Minute),
			FinishedAt: &ended, ForkedFrom: "run-parent",
			Source: &store.RunSource{IssueID: blocker.ID},
		},
		// Newer but parked: cancelled via Fork()'s initial SaveRun, no
		// FinishedAt — must not win over the fork that really finished.
		"run-fork-parked": {
			ID: "run-fork-parked", Status: store.RunStatusCancelled, CreatedAt: older.Add(40 * time.Minute),
			ForkedFrom: "run-fork",
			Source:     &store.RunSource{IssueID: blocker.ID},
		},
	}}

	issues, err := board.List(native.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	newSweepTestServer().reconcileFinishedTickets(context.Background(), board, rs, issues)

	got, _ := board.Get(blocker.ID)
	if got.State != native.StateDone {
		t.Fatalf("ticket state = %q, want done (the finished fork is the card's outcome)", got.State)
	}
	if got.LastRunID != "run-fork" {
		t.Errorf("LastRunID = %q, want run-fork adopted as the current attempt", got.LastRunID)
	}
	dep, _ := board.Get(dependent.ID)
	if dep.State != native.StateBacklog {
		t.Fatalf("dependent state = %q, want backlog (auto-promoted)", dep.State)
	}
}

// The parked-fork-only case: nothing actually finished, so the ticket
// stays with the operator.
func TestReconcileFinishedTickets_ParkedForkAloneDoesNotFile(t *testing.T) {
	board, err := native.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	iss, _ := board.Create(native.Issue{Title: "epic", State: native.StateInProgress, Bot: "town-dev"})
	if err := board.SetLastRun(iss.ID, "run-parent", ""); err != nil {
		t.Fatal(err)
	}
	older := time.Now().Add(-time.Hour)
	rs := &fakeSweepRunStore{runs: map[string]*store.Run{
		"run-parent": {
			ID: "run-parent", Status: store.RunStatusFailed, CreatedAt: older,
			Source: &store.RunSource{Kind: store.RunSourceKindDispatcher, IssueID: iss.ID},
		},
		"run-fork-parked": {
			ID: "run-fork-parked", Status: store.RunStatusCancelled, CreatedAt: older.Add(30 * time.Minute),
			ForkedFrom: "run-parent",
			Source:     &store.RunSource{IssueID: iss.ID},
		},
	}}

	issues, _ := board.List(native.ListFilter{})
	newSweepTestServer().reconcileFinishedTickets(context.Background(), board, rs, issues)

	got, _ := board.Get(iss.ID)
	if got.State != native.StateInProgress {
		t.Fatalf("ticket state = %q, want in_progress (a parked fork delivered nothing)", got.State)
	}
}

// The fork index is only built when a ticket is actually stuck on a
// terminal pointer — an idle or in-flight board must not pay a full
// store scan per admission tick (Rc2c8ed: K stuck tickets × one scan
// per 2s tick, forever, on an IDLE board).
func TestReconcileFinishedTickets_NoStuckTicketSkipsTheScan(t *testing.T) {
	board, err := native.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	iss, _ := board.Create(native.Issue{Title: "epic", State: native.StateInProgress, Bot: "town-dev"})
	if err := board.SetLastRun(iss.ID, "run-live", ""); err != nil {
		t.Fatal(err)
	}
	rs := &fakeSweepRunStore{runs: map[string]*store.Run{
		"run-live": {ID: "run-live", Status: store.RunStatusRunning},
	}}

	issues, _ := board.List(native.ListFilter{})
	newSweepTestServer().reconcileFinishedTickets(context.Background(), board, rs, issues)
	if rs.listCalls != 0 {
		t.Errorf("ListRuns called %d times with no stuck ticket, want 0 (idle board pays no scan)", rs.listCalls)
	}
}

// Two sweeps within the TTL share one index build, even with a stuck
// ticket present.
func TestReconcileFinishedTickets_ForkIndexIsMemoized(t *testing.T) {
	board, err := native.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	iss, _ := board.Create(native.Issue{Title: "epic", State: native.StateInProgress, Bot: "town-dev"})
	if err := board.SetLastRun(iss.ID, "run-parent", ""); err != nil {
		t.Fatal(err)
	}
	rs := &fakeSweepRunStore{runs: map[string]*store.Run{
		"run-parent": {
			ID: "run-parent", Status: store.RunStatusFailed, CreatedAt: time.Now().Add(-time.Hour),
			Source: &store.RunSource{Kind: store.RunSourceKindDispatcher, IssueID: iss.ID},
		},
	}}

	issues, _ := board.List(native.ListFilter{})
	srv := newSweepTestServer()
	srv.reconcileFinishedTickets(context.Background(), board, rs, issues)
	srv.reconcileFinishedTickets(context.Background(), board, rs, issues)
	if rs.listCalls != 1 {
		t.Errorf("ListRuns called %d times over 2 sweeps, want 1 (index memoized within the TTL)", rs.listCalls)
	}
}

// TestLaunchTicketNow_RefusesAClaimedTicket: the operator's "launch now"
// endpoint reaches launchTicketNow WITHOUT the admission loop's
// ClaimForLaunch — the guard must live at the choke both callers cross.
// A ticket claimed under a live lease already has a launcher (the
// dispatcher's move out of Ready is offloaded, so Ready+claimed is the
// normal mid-launch shape): minting a second run there double-launched
// the card.
func TestLaunchTicketNow_RefusesAClaimedTicket(t *testing.T) {
	board, err := native.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	iss, err := board.Create(native.Issue{Title: "mid-launch", State: native.StateReady, Bot: "feature-dev"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := board.Claim(iss.ID, "dispatcher-host-a"); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	s := newSweepTestServer()
	_, err = s.launchTicketNow(nil, board, iss)
	if err == nil {
		t.Fatal("launchTicketNow accepted a ticket held under a live claim — a second run was minted while its launcher was mid-launch")
	}
	// The refusal must be the CLAIM guard, not a downstream accident (an
	// empty catalog also errors — an assertion on any error certifies
	// nothing).
	if !strings.Contains(err.Error(), "claimed by") {
		t.Fatalf("refused for the wrong reason: %v — the claim guard did not fire", err)
	}
	cur, _ := board.Get(iss.ID)
	if cur.State != native.StateReady {
		t.Fatalf("the refused ticket must stay untouched, state=%q", cur.State)
	}
}

// TestPipelineTicketLaunchable_DeletedRunIsProofOfAbsence: ErrRunDeleted
// is a durable tombstone — permanent PROOF the run is gone, not the
// store blip the fail-closed exists for. Refusing it bricked a ticket
// whose operator deleted its run, with no exit from the studio (delete
// does not clear the card's LastRunID).
func TestPipelineTicketLaunchable_DeletedRunIsProofOfAbsence(t *testing.T) {
	dir := t.TempDir()
	rs, err := store.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := rs.CreateRun(ctx, "run-gone", "wf", nil); err != nil {
		t.Fatal(err)
	}
	if err := rs.DeleteRun(ctx, "run-gone"); err != nil {
		t.Fatal(err)
	}
	if _, err := rs.LoadRun(ctx, "run-gone"); !errors.Is(err, store.ErrRunDeleted) {
		t.Fatalf("precondition: want ErrRunDeleted, got %v", err)
	}
	iss := &native.Issue{ID: "card-1", Title: "c", State: native.StateReady, Bot: "feature-dev", LastRunID: "run-gone"}
	if !pipelineTicketLaunchable(ctx, rs, iss) {
		t.Fatal("a ticket whose last run was DELETED (durable tombstone) is refused for ever — proof of absence read as lack of information")
	}
}
