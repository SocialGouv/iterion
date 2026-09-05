package server

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/forge"
)

// The REFLECT half: a native card that moved must move its card on the forge
// board. It rides the same pass as the import, and its whole correctness rests
// on one comparison — "does the board still say what we last recorded?" — which
// is also the echo suppressor. That double duty is why neither direction needs
// to maintain a separate "who moved last" flag on every write.

func testBinding() *forge.BoardBinding {
	return &forge.BoardBinding{
		TenantID: "team-a", Provider: forge.ProviderGitHub,
		Owner: "SocialGouv", OwnerKind: forge.ProjectOwnerOrg, Number: 203,
		ConnectionID: "conn-1", ProjectID: "PVT_p", StatusFieldID: "PVTSSF_status",
		StatusOptions: map[string]string{
			"inbox": "o_inbox", "ready": "o_planned", "in_progress": "o_prog",
			"blocked": "o_blocked", "done": "o_done",
		},
		StatusMapping: forge.DefaultStatusMapping(),
	}
}

// seedSynced creates a card already recorded as synced with the board.
func seedSynced(t *testing.T, board native.BoardStore, number int, state, recordedStatus string, at time.Time) string {
	t.Helper()
	id := seedCard(t, board, number, state)
	if _, err := board.Update(id, native.Patch{External: &native.ExternalRef{
		Provider: "github", Repo: "SocialGouv/iterion", Number: number,
		Project: &native.ExternalProject{
			Owner: "SocialGouv", Number: 203, ItemID: "PVTI_1",
			Status: recordedStatus, StatusAt: at, StateAt: at,
		},
	}}); err != nil {
		t.Fatalf("seed sync state: %v", err)
	}
	return id
}

func TestSyncProjectBoardReflectsANativeMove(t *testing.T) {
	board := newTestBoard(t)
	at := time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC)
	// The card moved to in_progress natively; the board still says Planned,
	// which is exactly what we recorded — so nobody moved it there but us.
	id := seedSynced(t, board, 613, native.StateInProgress, "Planned", at)
	bc := &fakeBoardClient{project: testProject(), pages: [][]forge.ProjectItem{{
		item("PVTI_1", 613, statusValue("Planned", at)),
	}}}

	res, err := ImportProjectBoard(context.Background(), bc, testProjectRef, forge.ProviderGitHub, board,
		&ProjectImportOptions{Binding: testBinding()})
	if err != nil {
		t.Fatalf("ImportProjectBoard: %v", err)
	}
	if res.Reflected != 1 {
		t.Fatalf("Reflected = %d, want 1 (%+v)", res.Reflected, res)
	}
	if len(bc.writes) != 1 {
		t.Fatalf("writes = %+v, want one Status write", bc.writes)
	}
	w := bc.writes[0]
	if w.ProjectID != "PVT_p" || w.ItemID != "PVTI_1" || w.FieldID != "PVTSSF_status" || w.OptionID != "o_prog" {
		t.Errorf("write = %+v, want the In progress option on PVTI_1", w)
	}
	// The card's state must NOT change — the reflect pushes, it does not pull.
	if got := mustGet(t, board, id).State; got != native.StateInProgress {
		t.Errorf("state = %q, want it untouched", got)
	}
	// And the recorded status advances, so the next pass is a no-op.
	p := mustGet(t, board, id).External.Project
	if p.Status != "In progress" {
		t.Errorf("recorded status = %q, want the value we just wrote — otherwise every pass rewrites it", p.Status)
	}
}

// TestSyncProjectBoardIsIdempotent: two passes with nothing moving in between
// must write exactly once. A reflect that rewrote every pass would burn the
// API budget and stamp a fresh updatedAt that then wins every conflict.
func TestSyncProjectBoardIsIdempotent(t *testing.T) {
	board := newTestBoard(t)
	at := time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC)
	seedSynced(t, board, 613, native.StateInProgress, "Planned", at)
	bc := &fakeBoardClient{project: testProject(), pages: [][]forge.ProjectItem{{
		item("PVTI_1", 613, statusValue("Planned", at)),
	}}}
	opts := &ProjectImportOptions{Binding: testBinding()}

	if _, err := ImportProjectBoard(context.Background(), bc, testProjectRef, forge.ProviderGitHub, board, opts); err != nil {
		t.Fatalf("pass 1: %v", err)
	}
	// The board now answers what the reflect wrote.
	bc.pages = [][]forge.ProjectItem{{item("PVTI_1", 613, statusValue("In progress", at.Add(time.Minute)))}}
	res, err := ImportProjectBoard(context.Background(), bc, testProjectRef, forge.ProviderGitHub, board, opts)
	if err != nil {
		t.Fatalf("pass 2: %v", err)
	}
	if res.Reflected != 0 || res.Moved != 0 {
		t.Errorf("pass 2 must be a no-op, got %+v", res)
	}
	if len(bc.writes) != 1 {
		t.Errorf("writes = %d, want still 1 — the second pass must not rewrite", len(bc.writes))
	}
}

// TestSyncProjectBoardDefersToAMovedBoard: when the board's status is NOT what
// we recorded, the board moved too — that is the import's conflict rule to
// resolve, not the reflect's to overwrite.
func TestSyncProjectBoardDefersToAMovedBoard(t *testing.T) {
	board := newTestBoard(t)
	at := time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC)
	id := seedSynced(t, board, 613, native.StateInProgress, "Planned", at)
	bc := &fakeBoardClient{project: testProject(), pages: [][]forge.ProjectItem{{
		item("PVTI_1", 613, statusValue("Blocked", at.Add(time.Hour))),
	}}}

	res, err := ImportProjectBoard(context.Background(), bc, testProjectRef, forge.ProviderGitHub, board,
		&ProjectImportOptions{Binding: testBinding()})
	if err != nil {
		t.Fatalf("ImportProjectBoard: %v", err)
	}
	if res.Reflected != 0 {
		t.Errorf("Reflected = %d: a board that moved is the import's business", res.Reflected)
	}
	if len(bc.writes) != 0 {
		t.Errorf("writes = %+v, want none", bc.writes)
	}
	// The board is newer, so the card follows it.
	if got := mustGet(t, board, id).State; got != native.StateBlocked {
		t.Errorf("state = %q, want %q", got, native.StateBlocked)
	}
}

func TestSyncProjectBoardDoesNotReflectAnUnmappedState(t *testing.T) {
	board := newTestBoard(t)
	at := time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC)
	// `review` has no column: the reflect logs the skip and writes nothing,
	// leaving the board showing the last true thing it was told.
	seedSynced(t, board, 613, native.StateReview, "Planned", at)
	bc := &fakeBoardClient{project: testProject(), pages: [][]forge.ProjectItem{{
		item("PVTI_1", 613, statusValue("Planned", at)),
	}}}

	res, err := ImportProjectBoard(context.Background(), bc, testProjectRef, forge.ProviderGitHub, board,
		&ProjectImportOptions{Binding: testBinding()})
	if err != nil {
		t.Fatalf("ImportProjectBoard: %v", err)
	}
	if res.Reflected != 0 || len(bc.writes) != 0 {
		t.Fatalf("an unmapped native state is inert: %+v / %+v", res, bc.writes)
	}
}

func TestSyncProjectBoardWithoutABindingIsReadOnly(t *testing.T) {
	board := newTestBoard(t)
	at := time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC)
	seedSynced(t, board, 613, native.StateInProgress, "Planned", at)
	bc := &fakeBoardClient{project: testProject(), pages: [][]forge.ProjectItem{{
		item("PVTI_1", 613, statusValue("Planned", at)),
	}}}

	res, err := ImportProjectBoard(context.Background(), bc, testProjectRef, forge.ProviderGitHub, board, nil)
	if err != nil {
		t.Fatalf("ImportProjectBoard: %v", err)
	}
	if len(bc.writes) != 0 {
		t.Fatalf("no binding means no write authority: %+v", bc.writes)
	}
	if res.Reflected != 0 {
		t.Errorf("Reflected = %d", res.Reflected)
	}
}

// TestSyncProjectBoardReflectFailureIsCountedNotFatal: one card's failed write
// must not abandon the rest of the board.
func TestSyncProjectBoardReflectFailureIsCountedNotFatal(t *testing.T) {
	board := newTestBoard(t)
	at := time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC)
	seedSynced(t, board, 613, native.StateInProgress, "Planned", at)
	id2 := seedCard(t, board, 614, native.StateInbox)
	bc := &fakeBoardClient{
		project: testProject(),
		pages: [][]forge.ProjectItem{{
			item("PVTI_1", 613, statusValue("Planned", at)),
			item("PVTI_2", 614, statusValue("Done", at)),
		}},
		setErr: errors.New("403 Resource not accessible by integration"),
	}

	res, err := ImportProjectBoard(context.Background(), bc, testProjectRef, forge.ProviderGitHub, board,
		&ProjectImportOptions{Binding: testBinding()})
	if err != nil {
		t.Fatalf("one failed write must not fail the pass: %v", err)
	}
	if res.ReflectFailed != 1 {
		t.Errorf("ReflectFailed = %d, want 1 (%+v)", res.ReflectFailed, res)
	}
	if res.Reflected != 0 {
		t.Errorf("Reflected = %d, want 0 — a failed write is not a reflect", res.Reflected)
	}
	// The other card's import still happened.
	if got := mustGet(t, board, id2).State; got != native.StateDone {
		t.Errorf("the second card was abandoned: state = %q", got)
	}
}

// TestSyncProjectBoardNeverReflectsAnUnrecordedCard: a card the board has
// never been synced with has no recorded status, so "did the board move?" has
// no answer — the first pass IMPORTS it rather than overwriting a column
// nobody has reconciled yet.
func TestSyncProjectBoardNeverReflectsAnUnrecordedCard(t *testing.T) {
	board := newTestBoard(t)
	at := time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC)
	id := seedCard(t, board, 613, native.StateInProgress)
	bc := &fakeBoardClient{project: testProject(), pages: [][]forge.ProjectItem{{
		item("PVTI_1", 613, statusValue("Planned", at)),
	}}}

	res, err := ImportProjectBoard(context.Background(), bc, testProjectRef, forge.ProviderGitHub, board,
		&ProjectImportOptions{Binding: testBinding()})
	if err != nil {
		t.Fatalf("ImportProjectBoard: %v", err)
	}
	if len(bc.writes) != 0 {
		t.Fatalf("first sight must not push: %+v", bc.writes)
	}
	if res.Moved != 1 {
		t.Errorf("Moved = %d, want 1 — the board is the authority on the join", res.Moved)
	}
	if got := mustGet(t, board, id).State; got != native.StateReady {
		t.Errorf("state = %q, want %q", got, native.StateReady)
	}
}
