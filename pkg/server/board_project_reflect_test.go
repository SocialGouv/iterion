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

// TestSyncProjectBoardReflectsWhenNativeWinsTheConflict is the case the
// conflict rule EXISTS to serve, and the one that was silently dead: both
// sides moved, iterion's move is newer, so iterion's state must reach the
// board. Skipping the push there left the two boards divergent AND re-derived
// the identical conflict on every pass forever — the recorded status is
// deliberately not advanced on that branch, so the inputs never change.
func TestSyncProjectBoardReflectsWhenNativeWinsTheConflict(t *testing.T) {
	board := newTestBoard(t)
	older := time.Date(2026, 9, 5, 9, 0, 0, 0, time.UTC)
	newer := time.Date(2026, 9, 5, 11, 0, 0, 0, time.UTC)

	// Recorded "Planned"; the board has since moved to "In progress" (at
	// `older`), and iterion moved the card to blocked later (at `newer`).
	id := seedSynced(t, board, 613, native.StateBlocked, "Planned", older)
	if _, err := board.Update(id, native.Patch{External: &native.ExternalRef{
		Provider: "github", Repo: "SocialGouv/iterion", Number: 613,
		Project: &native.ExternalProject{
			Owner: "SocialGouv", Number: 203, ItemID: "PVTI_1",
			Status: "Planned", StatusAt: older, StateAt: newer,
		},
	}}); err != nil {
		t.Fatalf("seed sync state: %v", err)
	}
	bc := &fakeBoardClient{project: testProject(), pages: [][]forge.ProjectItem{{
		item("PVTI_1", 613, statusValue("In progress", older)),
	}}}
	opts := &ProjectImportOptions{Binding: testBinding()}

	res, err := ImportProjectBoard(context.Background(), bc, testProjectRef, forge.ProviderGitHub, board, opts)
	if err != nil {
		t.Fatalf("ImportProjectBoard: %v", err)
	}
	if res.Conflicts != 1 {
		t.Errorf("Conflicts = %d, want 1", res.Conflicts)
	}
	if res.Moved != 0 {
		t.Errorf("Moved = %d: the native side won, so the card must not follow the board", res.Moved)
	}
	if res.Reflected != 1 {
		t.Fatalf("Reflected = %d, want 1 — the winner's state must reach the board (%+v)", res.Reflected, res)
	}
	if len(bc.writes) != 1 || bc.writes[0].OptionID != "o_blocked" {
		t.Fatalf("writes = %+v, want the Blocked option", bc.writes)
	}
	if got := mustGet(t, board, id).State; got != native.StateBlocked {
		t.Errorf("state = %q, want it untouched", got)
	}

	// And it CONVERGES: the board now answers what was pushed, so the next
	// pass is a no-op instead of re-deriving the same conflict.
	bc.pages = [][]forge.ProjectItem{{item("PVTI_1", 613, statusValue("Blocked", newer.Add(time.Minute)))}}
	res2, err := ImportProjectBoard(context.Background(), bc, testProjectRef, forge.ProviderGitHub, board, opts)
	if err != nil {
		t.Fatalf("pass 2: %v", err)
	}
	if res2.Conflicts != 0 || res2.Reflected != 0 || res2.Moved != 0 {
		t.Fatalf("pass 2 must be a no-op, got %+v", res2)
	}
	if len(bc.writes) != 1 {
		t.Errorf("writes = %d, want still 1", len(bc.writes))
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

// TestIssueImportPreservesTheProjectSyncState is the class guard on the OTHER
// writer of ExternalRef. The issue import replaces the whole ref with a fresh
// one built from the forge issue; if it drops the project half, every plain
// `iterion issue import` silently resets the sync state, the next project pass
// reads "first sight", and any native move not yet reflected is overwritten by
// the board instead of pushed to it.
func TestIssueImportPreservesTheProjectSyncState(t *testing.T) {
	board := newTestBoard(t)
	at := time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC)
	id := seedSynced(t, board, 613, native.StateInProgress, "Planned", at)

	// The issue-import upsert path, on a card that already exists.
	b := board.Board()
	_, updated, err := upsertForgeCardForTest(board, b, forge.IssueRef{
		Number: 613, Title: "t", Body: "b", State: "open", URL: "u",
	})
	if err != nil {
		t.Fatalf("upsertForgeCard: %v", err)
	}
	if updated != 1 {
		t.Fatalf("updated = %d, want 1", updated)
	}

	got := mustGet(t, board, id)
	if got.External == nil {
		t.Fatal("ExternalRef dropped entirely")
	}
	if got.External.Project == nil {
		t.Fatal("the issue import dropped the project sync state — the next project pass would read 'first sight' and overwrite an unreflected native move")
	}
	if got.External.Project.Status != "Planned" || got.External.Project.ItemID != "PVTI_1" {
		t.Errorf("project sync state mangled: %+v", got.External.Project)
	}
	// And the forge half is still refreshed from the issue.
	if got.External.URL != "u" {
		t.Errorf("the forge half must still be updated: %+v", got.External)
	}
}

// upsertForgeCardForTest calls the import's upsert with this suite's fixture
// repo/provider, so the class guard exercises the REAL writer.
func upsertForgeCardForTest(board native.BoardStore, b *native.Board, is forge.IssueRef) (int, int, error) {
	return upsertForgeCard(board, b, native.StateInbox, native.StateDone,
		forge.ProviderGitHub, "", "SocialGouv/iterion", is, true)
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
