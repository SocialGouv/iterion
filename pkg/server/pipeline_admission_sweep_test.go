package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/SocialGouv/iterion/pkg/runview"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

// TestLaunchTicketNow_ParkedClaimWithALapsedLeaseStaysRelaunchable: the
// dispatcher PARKS an awaiting-input card with its claim RETAINED and its
// heartbeat STOPPED (ADR-014), and DecideStuckCard conserves that claim
// for ever on a paused/cancelled run — no watchdog will ever take it. A
// bare `Claim != ""` guard therefore closed the operator's own escape
// hatch (this endpoint has no Ready precondition precisely to serve a
// needs-attention relaunch) and told them to wait for something that
// cannot come. LIVE is the lease, not the marker.
func TestLaunchTicketNow_ParkedClaimWithALapsedLeaseStaysRelaunchable(t *testing.T) {
	dir := t.TempDir()
	board, err := native.NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	iss, err := board.Create(native.Issue{Title: "parked", State: native.StateReady, Bot: "feature-dev"})
	if err != nil {
		t.Fatal(err)
	}
	tok, err := board.Claim(iss.ID, "dispatcher-host-a")
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if _, err := board.SetStateOwned(iss.ID, native.StateAwaitingInput, tok); err != nil {
		t.Fatalf("park: %v", err)
	}
	// Age the lease on disk, then reopen: the heartbeat is stopped, so in
	// production the lease simply lapses.
	matches, _ := filepath.Glob(filepath.Join(dir, "issues", "*.json"))
	if len(matches) != 1 {
		t.Fatalf("want exactly one issue file, got %v", matches)
	}
	p := matches[0]
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	doc["claim_lease_until"] = time.Now().UTC().Add(-time.Hour).Format(time.RFC3339Nano)
	out, _ := json.Marshal(doc)
	if err := os.WriteFile(p, out, 0o644); err != nil {
		t.Fatal(err)
	}
	board2, err := native.NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	cur, err := board2.Get(iss.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cur.Claim == "" || cur.ClaimLeaseUntil.After(time.Now().UTC()) {
		t.Fatalf("precondition: want a retained claim with a LAPSED lease, got claim=%q lease=%s", cur.Claim, cur.ClaimLeaseUntil)
	}

	s := newSweepTestServer()
	_, err = s.launchTicketNow(nil, board2, cur)
	if err != nil && strings.Contains(err.Error(), "claimed by") {
		t.Fatalf("the operator's relaunch of a parked, claim-retained card is refused: %v — nothing else ever frees that claim", err)
	}
}

// TestLaunchTicketNow_LegacyUnleasedClaimIsRelaunchable: a claim with NO
// lease is what a release N-1 binary writes — the population the
// expand/contract rollout GUARANTEES during release N — and it has zero
// release path (the FS reaper has no un-leased arm; the cloud one lists
// it gate ON only). Refusing it bricked the operator's escape hatch for
// ever, with an error message naming a lease that does not exist and a
// watchdog that would never come.
func TestLaunchTicketNow_LegacyUnleasedClaimIsRelaunchable(t *testing.T) {
	dir := t.TempDir()
	board, err := native.NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	iss, err := board.Create(native.Issue{Title: "parked pre-ADR-096", State: native.StateReady, Bot: "feature-dev"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := board.Claim(iss.ID, "dispatcher-host-a"); err != nil {
		t.Fatal(err)
	}
	// Rewrite the issue document the way the OLD binary persisted it: a
	// bare marker, no lease family at all. Reload so the index reads it.
	entries, err := os.ReadDir(filepath.Join(dir, "issues"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("issue dir: entries=%d err=%v", len(entries), err)
	}
	p := filepath.Join(dir, "issues", entries[0].Name())
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read issue doc: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	delete(doc, "claim_lease_until")
	delete(doc, "claim_epoch")
	delete(doc, "claimed_at")
	out, _ := json.Marshal(doc)
	if err := os.WriteFile(p, out, 0o644); err != nil {
		t.Fatal(err)
	}
	board2, err := native.NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	cur, _ := board2.Get(iss.ID)
	if cur.Claim == "" || !cur.ClaimLeaseUntil.IsZero() {
		t.Fatalf("precondition: want a legacy no-lease claim, got claim=%q lease=%v", cur.Claim, cur.ClaimLeaseUntil)
	}

	s := newSweepTestServer()
	_, err = s.launchTicketNow(nil, board2, cur)
	if err != nil && strings.Contains(err.Error(), "claimed by") {
		t.Fatalf("a legacy no-lease claim was refused by the guard: %v — no lease will ever lapse and no watchdog lists this card", err)
	}
}

// TestHandleDeleteRun_LiveRunAnswers409: the lifecycle refusal must not
// ride the 404 arm — a refusal made in the name of "the tombstone is
// proof of absence" answering with the HTTP proof of absence told the
// API/MCP caller the exact opposite of its own reason.
func TestHandleDeleteRun_LiveRunAnswers409(t *testing.T) {
	dir := t.TempDir()
	rs, err := store.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := rs.CreateRun(ctx, "run-live", "wf", nil); err != nil {
		t.Fatal(err)
	}
	r0, _ := rs.LoadRun(ctx, "run-live")
	r0.Status = store.RunStatusRunning
	if err := rs.SaveRun(ctx, r0); err != nil {
		t.Fatal(err)
	}
	svc, err := runview.NewService("", runview.WithStore(rs))
	if err != nil {
		t.Fatal(err)
	}
	s := newSweepTestServer()
	s.runs = svc

	req := httptest.NewRequest(http.MethodDelete, "/api/runs/run-live", nil)
	req.SetPathValue("id", "run-live")
	w := httptest.NewRecorder()
	s.handleDeleteRun(w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("DELETE on a RUNNING run answered %d (%s) — want 409: 404 is the proof-of-absence the refusal exists to protect", w.Code, w.Body.String())
	}

	// And a genuinely missing run stays 404.
	req = httptest.NewRequest(http.MethodDelete, "/api/runs/run-gone", nil)
	req.SetPathValue("id", "run-gone")
	w = httptest.NewRecorder()
	s.handleDeleteRun(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("DELETE on a missing run answered %d, want 404", w.Code)
	}
}

// clockedBoard hands the guard a board-side clock distinct from the
// pod's — the shape of the Mongo twin, whose leases are stamped $$NOW.
type clockedBoard struct {
	native.BoardStore
	now time.Time
}

func (c clockedBoard) ServerNow(context.Context) (time.Time, error) { return c.now, nil }

// TestLaunchTicketNow_LeaseIsMeasuredWithTheBoardClock: the lease is
// stamped by the DATABASE clock, so measuring it with the pod's re-opens
// the cross-clock hole from the other end — a pod running Δ fast reads
// every lease younger than Δ as lapsed and launches past a LIVE holder
// (at Δ ≥ the 15m lease the guard is fully disarmed).
func TestLaunchTicketNow_LeaseIsMeasuredWithTheBoardClock(t *testing.T) {
	dir := t.TempDir()
	board, err := native.NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	iss, err := board.Create(native.Issue{Title: "held", State: native.StateReady, Bot: "feature-dev"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := board.Claim(iss.ID, "dispatcher-host-a"); err != nil {
		t.Fatal(err)
	}
	// Rewrite the lease into the window that separates the two clocks:
	// LAPSED by the pod's clock (until = pod-1m), LIVE by the board's
	// (server clock runs 30m behind the pod's).
	entries, err := os.ReadDir(filepath.Join(dir, "issues"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("issue dir: %v", err)
	}
	p := filepath.Join(dir, "issues", entries[0].Name())
	raw, _ := os.ReadFile(p)
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	doc["claim_lease_until"] = time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano)
	out, _ := json.Marshal(doc)
	if err := os.WriteFile(p, out, 0o644); err != nil {
		t.Fatal(err)
	}
	board2, err := native.NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	cb := clockedBoard{BoardStore: board2, now: time.Now().UTC().Add(-30 * time.Minute)}

	s := newSweepTestServer()
	_, lerr := s.launchTicketNow(nil, cb, func() *native.Issue { c, _ := board2.Get(iss.ID); return c }())
	if lerr == nil || !strings.Contains(lerr.Error(), "claimed by") {
		t.Fatalf("a lease LIVE on the board's clock was admitted because the pod's clock ran fast: err=%v", lerr)
	}
}

type erroringClockBoard struct {
	native.BoardStore
}

func (erroringClockBoard) ServerNow(context.Context) (time.Time, error) {
	return time.Time{}, errors.New("mongo: server selection timeout")
}

// The launch guard's clock degradation is LOGGED, once per edge — the
// reaper's own rule, copied here WITH the property this time (round 13
// had copied the sibling minus its logging).
func TestBoardNow_DegradationWarnsOnItsEdge(t *testing.T) {
	board, err := native.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	s := &Server{logger: iterlog.New(iterlog.LevelWarn, &buf)}
	cb := erroringClockBoard{BoardStore: board}
	s.boardNow(cb)
	s.boardNow(cb)
	if warns := strings.Count(buf.String(), "board clock unavailable"); warns != 1 {
		t.Fatalf("clock degradation warned %d time(s) over 2 calls, want once (edge) — silent measurement against a suspect clock", warns)
	}
	// FS twin (no ServerNow): silent by design.
	buf.Reset()
	s.boardNow(board)
	if buf.Len() != 0 {
		t.Fatalf("the FS twin's local-clock fallback must stay silent, got %q", buf.String())
	}
}
