package server

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/dispatcher/tracker"
	"github.com/SocialGouv/iterion/pkg/forge"
)

// The terminal sink, seen from the project pass (ADR-097 §7, ADR-096 §5).
//
// The sink refuses every MACHINE exit from a done/blocked column. A drag on
// the bound roadmap board is not one: it is a person's decision, arriving
// through the only channel they have. These tests pin where that line falls —
// a parked card the operator un-parks is honoured as the explicit reopen the
// sink demands, a finished one is still refused, and the refusal is a fact on
// the card and on the binding rather than a line in the server log.

// boundOpts builds a write-authority pass over a fresh binding store, and
// hands back both so a test can read the binding's health afterwards.
func boundOpts(t *testing.T, now time.Time) (*ProjectImportOptions, *forge.MemoryBoardBindingStore, *forge.BoardBinding) {
	t.Helper()
	bindings := forge.NewMemoryBoardBindingStore()
	b := testBinding()
	if err := bindings.Upsert(context.Background(), *b); err != nil {
		t.Fatalf("seed binding: %v", err)
	}
	return &ProjectImportOptions{
		Binding:  b,
		Bindings: bindings,
		Now:      func() time.Time { return now },
	}, bindings, b
}

// lastStateEvent returns the most recent EvtIssueState payload for a card.
func lastStateEvent(t *testing.T, board *native.Store, id string) map[string]any {
	t.Helper()
	var out map[string]any
	if err := board.ScanEvents(func(e *native.Event) bool {
		if e.Type == native.EvtIssueState && e.IssueID == id {
			out = e.Payload
		}
		return true
	}); err != nil {
		t.Fatalf("scan events: %v", err)
	}
	return out
}

// TestImportProjectBoardHumanMoveOutOfBlockedReopens: an operator dragging a
// parked card out of Blocked on the roadmap board IS the explicit reopen the
// sink demands. The pass honours it — through Reopen, the one sanctioned exit
// — and records it, so the two boards converge instead of diverging for ever
// behind a `refused_terminal` counter nobody reads.
func TestImportProjectBoardHumanMoveOutOfBlockedReopens(t *testing.T) {
	board := newTestBoard(t)
	at := time.Date(2026, 9, 6, 11, 0, 0, 0, time.UTC)
	now := at.Add(time.Minute)
	// The two boards agreed on Blocked; then the human moved the item.
	id := seedSynced(t, board, 613, native.StateBlocked, "Blocked", at)
	opts, bindings, binding := boundOpts(t, now)
	bc := &fakeBoardClient{project: testProject(), pages: [][]forge.ProjectItem{{
		item("PVTI_1", 613, statusValue("Inbox", at.Add(30*time.Second))),
	}}}

	res, err := ImportProjectBoard(context.Background(), bc, testProjectRef, forge.ProviderGitHub, board, opts)
	if err != nil {
		t.Fatalf("ImportProjectBoard: %v", err)
	}
	iss := mustGet(t, board, id)
	if iss.State != native.StateInbox {
		t.Errorf("state = %q, want %q — a human's move out of a parked column is the reopen", iss.State, native.StateInbox)
	}
	if res.ReopenedTerminal != 1 {
		t.Errorf("ReopenedTerminal = %d, want 1 (%+v)", res.ReopenedTerminal, res)
	}
	if res.RefusedTerminal != 0 {
		t.Errorf("RefusedTerminal = %d, want 0 — the move landed", res.RefusedTerminal)
	}
	if res.Moved != 0 {
		t.Errorf("Moved = %d, want 0 — a reopen has its own bucket, so the counters stay disjoint", res.Moved)
	}

	// Recorded as a board-originated reopen, on the card.
	if iss.External == nil || iss.External.Project == nil {
		t.Fatal("the sync state must survive a reopen")
	}
	p := iss.External.Project
	if p.ReopenedAt.IsZero() {
		t.Error("ReopenedAt is zero — the reopen must be attributable to the board move that caused it")
	}
	if p.SyncConflict != nil {
		t.Errorf("SyncConflict = %+v, want none — nothing was refused", p.SyncConflict)
	}
	if p.Status != "Inbox" {
		t.Errorf("recorded status = %q, want %q so the next pass is quiet", p.Status, "Inbox")
	}
	// …and in the board's own audit trail, as a reopen and not an ordinary move.
	if ev := lastStateEvent(t, board, id); ev == nil || ev["reopened"] != true {
		t.Errorf("state event = %v, want a reopened marker", ev)
	}
	// The binding is healthy: nothing was refused.
	got, err := bindings.GetByTenant(context.Background(), binding.TenantID)
	if err != nil {
		t.Fatalf("GetByTenant: %v", err)
	}
	if got.SyncConflictReason != "" {
		t.Errorf("SyncConflictReason = %q, want none", got.SyncConflictReason)
	}
}

// TestImportProjectBoardRefusedMoveOutOfDoneIsVisibleInTheTool: reopening a
// FINISHED card stays a deliberate native gesture (a done card satisfies its
// dependents' blockers). The refusal is unchanged — what changes is that the
// operator can see it without `kubectl logs`: it is written on the card and on
// the binding's health.
func TestImportProjectBoardRefusedMoveOutOfDoneIsVisibleInTheTool(t *testing.T) {
	board := newTestBoard(t)
	at := time.Date(2026, 9, 6, 11, 0, 0, 0, time.UTC)
	now := at.Add(time.Minute)
	id := seedSynced(t, board, 613, native.StateDone, "Done", at)
	opts, bindings, binding := boundOpts(t, now)
	bc := &fakeBoardClient{project: testProject(), pages: [][]forge.ProjectItem{{
		item("PVTI_1", 613, statusValue("Inbox", at.Add(30*time.Second))),
	}}}

	res, err := ImportProjectBoard(context.Background(), bc, testProjectRef, forge.ProviderGitHub, board, opts)
	if err != nil {
		t.Fatalf("ImportProjectBoard: %v", err)
	}
	if got := mustGet(t, board, id).State; got != native.StateDone {
		t.Errorf("state = %q: reopening a finished card must stay a native gesture", got)
	}
	if res.RefusedTerminal != 1 || res.ReopenedTerminal != 0 {
		t.Errorf("RefusedTerminal = %d / ReopenedTerminal = %d, want 1 / 0 (%+v)",
			res.RefusedTerminal, res.ReopenedTerminal, res)
	}

	iss := mustGet(t, board, id)
	if iss.External == nil || iss.External.Project == nil || iss.External.Project.SyncConflict == nil {
		t.Fatalf("the refusal must be recorded on the card: %+v", iss.External)
	}
	c := iss.External.Project.SyncConflict
	if c.From != native.StateDone || c.To != native.StateInbox {
		t.Errorf("SyncConflict from/to = %q/%q, want %q/%q", c.From, c.To, native.StateDone, native.StateInbox)
	}
	if c.Status != "Inbox" || c.ItemID != "PVTI_1" {
		t.Errorf("SyncConflict must name the board column and item: %+v", c)
	}
	if !c.At.Equal(now) {
		t.Errorf("SyncConflict.At = %v, want %v", c.At, now)
	}
	if c.Reason == "" {
		t.Error("SyncConflict.Reason is empty — the operator needs to read WHY it did not land")
	}

	// …and on the binding's health, which is what `GET /api/teams/{id}/board-binding`
	// and `iterion remote board show` read.
	got, err := bindings.GetByTenant(context.Background(), binding.TenantID)
	if err != nil {
		t.Fatalf("GetByTenant: %v", err)
	}
	if !got.SyncConflicted() || got.SyncConflictAt == nil {
		t.Fatalf("the binding must carry the refusal: %+v", got)
	}
	if binding.SyncConflictReason == "" {
		t.Error("the in-memory binding must carry it too — the CLI one-shot reads the value it passed in")
	}

	// A repeat of the SAME refusal must not rewrite the card: the pass runs on
	// its interval, and a rewritten ExternalRef bumps UpdatedAt and emits
	// card.updated, which relaunches every label-matching board subscription.
	before := mustGet(t, board, id).UpdatedAt
	opts.Now = func() time.Time { return now.Add(2 * time.Minute) }
	if _, err := ImportProjectBoard(context.Background(), bc, testProjectRef, forge.ProviderGitHub, board, opts); err != nil {
		t.Fatalf("second pass: %v", err)
	}
	again := mustGet(t, board, id)
	if !again.UpdatedAt.Equal(before) {
		t.Errorf("the card was rewritten by a repeat of the same refusal (%v → %v)", before, again.UpdatedAt)
	}
	if c2 := again.External.Project.SyncConflict; c2 == nil || !c2.At.Equal(now) {
		t.Errorf("SyncConflict.At must stay the FIRST observation, got %+v", c2)
	}

	// The operator reopens the card natively — the sanctioned gesture. The next
	// pass applies the board's status and clears both readouts.
	if _, err := board.Reopen(id, native.StateInbox); err != nil {
		t.Fatalf("native reopen: %v", err)
	}
	if _, err := ImportProjectBoard(context.Background(), bc, testProjectRef, forge.ProviderGitHub, board, opts); err != nil {
		t.Fatalf("third pass: %v", err)
	}
	if p := mustGet(t, board, id).External.Project; p.SyncConflict != nil {
		t.Errorf("SyncConflict survived the remedy: %+v", p.SyncConflict)
	}
	healthy, err := bindings.GetByTenant(context.Background(), binding.TenantID)
	if err != nil {
		t.Fatalf("GetByTenant: %v", err)
	}
	if healthy.SyncConflicted() {
		t.Errorf("the binding still reads conflicted after the remedy: %q", healthy.SyncConflictReason)
	}
}

// TestTerminalSinkStillRefusesMachineExits is the guard that keeps the reopen
// above from becoming "terminal states are not terminal any more". The narrow
// path is the HUMAN's move on the bound board, attributable because the board
// side changed and the native side did not; everything else still meets the
// sink.
func TestTerminalSinkStillRefusesMachineExits(t *testing.T) {
	t.Run("an automated writer cannot leave a sink", func(t *testing.T) {
		board := newTestBoard(t)
		id := seedCard(t, board, 613, native.StateBlocked)
		if _, _, err := board.SetStateFrom(id, native.StateBlocked, native.StateInbox); !errors.Is(err, tracker.ErrTerminalStateExit) {
			t.Fatalf("SetStateFrom out of blocked: want ErrTerminalStateExit, got %v", err)
		}
		if _, err := board.SetState(id, native.StateInbox); !errors.Is(err, tracker.ErrTerminalStateExit) {
			t.Fatalf("SetState out of blocked: want ErrTerminalStateExit, got %v", err)
		}
		if got := mustGet(t, board, id).State; got != native.StateBlocked {
			t.Fatalf("state = %q, want it parked", got)
		}
	})

	// The reopen is attributable ONLY when the native side did not move. When
	// BOTH moved, the board's newer value still wins the arbitration — but a
	// move iterion itself made out of a working column is not a gesture the
	// board can overrule into a resurrection.
	t.Run("a contested move out of a sink is still refused", func(t *testing.T) {
		board := newTestBoard(t)
		at := time.Date(2026, 9, 6, 11, 0, 0, 0, time.UTC)
		// Recorded "In progress"; a run has since filed the card blocked, so
		// the native side moved too.
		id := seedSynced(t, board, 613, native.StateInProgress, "In progress", at)
		if _, err := board.SetState(id, native.StateBlocked); err != nil {
			t.Fatalf("file blocked: %v", err)
		}
		opts, _, _ := boundOpts(t, at.Add(time.Hour))
		bc := &fakeBoardClient{project: testProject(), pages: [][]forge.ProjectItem{{
			item("PVTI_1", 613, statusValue("Inbox", time.Now().UTC().Add(time.Hour))),
		}}}

		res, err := ImportProjectBoard(context.Background(), bc, testProjectRef, forge.ProviderGitHub, board, opts)
		if err != nil {
			t.Fatalf("ImportProjectBoard: %v", err)
		}
		if got := mustGet(t, board, id).State; got != native.StateBlocked {
			t.Errorf("state = %q, want %q — only an uncontested board move reopens", got, native.StateBlocked)
		}
		if res.ReopenedTerminal != 0 || res.RefusedTerminal != 1 {
			t.Errorf("ReopenedTerminal = %d / RefusedTerminal = %d, want 0 / 1 (%+v)",
				res.ReopenedTerminal, res.RefusedTerminal, res)
		}
	})

	// FIRST SIGHT is not a move. A card this pass has never synchronized has
	// no recorded status to have CHANGED, so the column the roadmap happens to
	// show it in is not evidence anybody moved it — and binding a board would
	// otherwise drag every parked card out of Blocked at once.
	t.Run("first sight of a card is not a move", func(t *testing.T) {
		board := newTestBoard(t)
		id := seedCard(t, board, 613, native.StateBlocked)
		opts, _, _ := boundOpts(t, time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC))
		bc := &fakeBoardClient{project: testProject(), pages: [][]forge.ProjectItem{{
			item("PVTI_1", 613, statusValue("Inbox", time.Date(2026, 9, 6, 11, 0, 0, 0, time.UTC))),
		}}}

		res, err := ImportProjectBoard(context.Background(), bc, testProjectRef, forge.ProviderGitHub, board, opts)
		if err != nil {
			t.Fatalf("ImportProjectBoard: %v", err)
		}
		if got := mustGet(t, board, id).State; got != native.StateBlocked {
			t.Errorf("state = %q, want %q", got, native.StateBlocked)
		}
		if res.ReopenedTerminal != 0 || res.RefusedTerminal != 1 {
			t.Errorf("ReopenedTerminal = %d / RefusedTerminal = %d, want 0 / 1", res.ReopenedTerminal, res.RefusedTerminal)
		}
	})
}

// TestImportProjectBoardReopensWithoutABinding: the reopen's oracle is the
// CARD's own sync record, not the binding — so the local one-shot
// (`iterion issue import --project`, no write authority on the forge side)
// honours an operator's move exactly like the cloud reconciliation. Only the
// health readout needs a binding.
func TestImportProjectBoardReopensWithoutABinding(t *testing.T) {
	board := newTestBoard(t)
	at := time.Date(2026, 9, 6, 11, 0, 0, 0, time.UTC)
	id := seedSynced(t, board, 613, native.StateBlocked, "Blocked", at)
	bc := &fakeBoardClient{project: testProject(), pages: [][]forge.ProjectItem{{
		item("PVTI_1", 613, statusValue("Inbox", at.Add(time.Minute))),
	}}}

	res, err := ImportProjectBoard(context.Background(), bc, testProjectRef, forge.ProviderGitHub, board, nil)
	if err != nil {
		t.Fatalf("ImportProjectBoard: %v", err)
	}
	if got := mustGet(t, board, id).State; got != native.StateInbox {
		t.Errorf("state = %q, want %q", got, native.StateInbox)
	}
	if res.ReopenedTerminal != 1 || res.RefusedTerminal != 0 {
		t.Errorf("ReopenedTerminal = %d / RefusedTerminal = %d, want 1 / 0 (%+v)",
			res.ReopenedTerminal, res.RefusedTerminal, res)
	}
}
