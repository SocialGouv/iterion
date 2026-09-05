package server

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/forge"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// The REFLECT half: a native card that moved must move its card on the forge
// board. It rides the same pass as the import, and its whole correctness rests
// on one comparison — "does the board still say what we last recorded?" — which
// is also the echo suppressor. That double duty is why neither direction needs
// to maintain a separate "who moved last" flag on every write.

// testBinding is the binding a bind against testProject() produces. Its option
// ids are READ from that fixture rather than invented: the reflect writes by
// id and compares by id, so a binding whose ids match no column on the board it
// is bound to cannot exercise either.
func testBinding() *forge.BoardBinding {
	project := testProject()
	field, ok := project.Field(forge.ProjectStatusFieldName)
	if !ok {
		panic("the project fixture must carry a " + forge.ProjectStatusFieldName + " field")
	}
	mapping := forge.DefaultStatusMapping()
	opts := make(map[string]string, len(mapping))
	for _, m := range mapping {
		opt, ok := field.Option(m.Status)
		if !ok {
			panic("the project fixture must carry the " + m.Status + " column")
		}
		opts[m.State] = opt.ID
	}
	return &forge.BoardBinding{
		TenantID: "team-a", Provider: forge.ProviderGitHub,
		Owner: "SocialGouv", OwnerKind: forge.ProjectOwnerOrg, Number: 203,
		ConnectionID: "conn-1", ProjectID: project.ID, StatusFieldID: field.ID,
		StatusOptions: opts,
		StatusMapping: mapping,
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

// cardStateAt is the card's REAL transition time — the store stamps it at
// every state write, and it is one of the two sides the conflict rule
// compares. A fixture that pins the board's timestamp to a calendar date while
// the card's is whenever the test ran is not expressing an ordering at all, so
// every conflict fixture below positions the board RELATIVE to this.
func cardStateAt(t *testing.T, board native.BoardStore, id string) time.Time {
	t.Helper()
	at := mustGet(t, board, id).StateAt
	if at.IsZero() {
		t.Fatal("the store must stamp a card's transition time; the conflict rule has nothing to compare otherwise")
	}
	return at
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
	if w.ProjectID != "PVT_p" || w.ItemID != "PVTI_1" || w.FieldID != "PVTSSF_status" || w.OptionID != optionID(t, testProject(), "In progress") {
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
		item("PVTI_1", 613, statusValue("Blocked", cardStateAt(t, board, id).Add(time.Hour))),
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
	// Recorded "Planned"; the board has since moved to "In progress" (at
	// `older`), and iterion moved the card to blocked later (at `newer` — the
	// card's REAL transition, which is what the conflict rule compares).
	id := seedSynced(t, board, 613, native.StateBlocked, "Planned", time.Time{})
	newer := cardStateAt(t, board, id)
	older := newer.Add(-2 * time.Hour)
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
	if len(bc.writes) != 1 || bc.writes[0].OptionID != optionID(t, testProject(), "Blocked") {
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

// countCardEvents returns how many events the store holds that the TRIGGER
// SPINE would consume (trigger.IsCardEvent's set). Those are the ones that
// cost money: a `kind: board` subscription matching on labels relaunches its
// bot on each one.
func countCardEvents(t *testing.T, board *native.Store) int {
	t.Helper()
	n := 0
	if err := board.ScanEvents(func(e *native.Event) bool {
		switch e.Type {
		case native.EvtIssueCreated, native.EvtIssueState, native.EvtIssueUpdated:
			n++
		}
		return true
	}); err != nil {
		t.Fatalf("ScanEvents: %v", err)
	}
	return n
}

// TestSyncProjectBoardQuietPassWritesNothing is the cost guard. The pass runs
// every 2 minutes over every card; if it rewrites each one unconditionally it
// bumps UpdatedAt, rewrites the record, and emits EvtIssueUpdated — which the
// trigger spine consumes as `card.updated`. A 200-item board would emit ~144k
// card events a day and relaunch every label-matching board subscription every
// two minutes, burning LLM budget for nothing.
//
// So: a pass that decides nothing must write nothing and emit nothing.
func TestSyncProjectBoardQuietPassWritesNothing(t *testing.T) {
	board := newTestBoard(t)
	at := time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC)
	// A card already fully in sync: recorded status == the board's, and the
	// card's column agrees with it.
	id := seedSynced(t, board, 613, native.StateReady, "Planned", at)
	bc := &fakeBoardClient{project: testProject(), pages: [][]forge.ProjectItem{{
		item("PVTI_1", 613, statusValue("Planned", at)),
	}}}
	opts := &ProjectImportOptions{Binding: testBinding()}

	before := mustGet(t, board, id)
	eventsBefore := countCardEvents(t, board)

	for pass := 1; pass <= 2; pass++ {
		res, err := ImportProjectBoard(context.Background(), bc, testProjectRef, forge.ProviderGitHub, board, opts)
		if err != nil {
			t.Fatalf("pass %d: %v", pass, err)
		}
		if res.Moved != 0 || res.Reflected != 0 || res.Labelled != 0 || res.Conflicts != 0 {
			t.Fatalf("pass %d decided something it should not have: %+v", pass, res)
		}
	}

	if got := countCardEvents(t, board); got != eventsBefore {
		t.Errorf("card events = %d, want %d — a quiet pass must emit none (the trigger spine consumes them)",
			got, eventsBefore)
	}
	after := mustGet(t, board, id)
	if !after.UpdatedAt.Equal(before.UpdatedAt) {
		t.Errorf("UpdatedAt churned (%v → %v) on a pass that changed nothing", before.UpdatedAt, after.UpdatedAt)
	}
}

// TestSyncProjectBoardStillWritesWhenSomethingChanged is the other half: the
// guard must not be so eager it stops recording real work.
func TestSyncProjectBoardStillWritesWhenSomethingChanged(t *testing.T) {
	board := newTestBoard(t)
	at := time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC)
	id := seedCard(t, board, 613, native.StateInbox)
	bc := &fakeBoardClient{project: testProject(), pages: [][]forge.ProjectItem{{
		item("PVTI_1", 613, statusValue("In progress", at)),
	}}}

	res, err := ImportProjectBoard(context.Background(), bc, testProjectRef, forge.ProviderGitHub, board, nil)
	if err != nil {
		t.Fatalf("ImportProjectBoard: %v", err)
	}
	if res.Moved != 1 {
		t.Fatalf("Moved = %d, want 1", res.Moved)
	}
	got := mustGet(t, board, id)
	if got.External == nil || got.External.Project == nil {
		t.Fatal("the sync state must still be recorded when the pass did something")
	}
	if got.External.Project.Status != "In progress" {
		t.Errorf("recorded status = %q", got.External.Project.Status)
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

// TestSyncProjectBoardNeverReflectsARefusedMove pins the other half of the
// terminal sink: a move iterion COULD NOT apply must not be recorded as
// synced. Recording it makes the next pass read "the board already agrees",
// which fires the reflect and writes the card's own terminal column back onto
// the forge — undoing the operator's move, permanently, on the one gesture the
// sink exists to leave divergent.
//
// Two passes, because the defect only shows on the second: the first records
// the status it failed to apply, the second acts on that record.
func TestSyncProjectBoardNeverReflectsARefusedMove(t *testing.T) {
	board := newTestBoard(t)
	at := time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC)
	// A closed card, agreeing with the board, that an operator then drags out
	// of Done on the forge.
	id := seedSynced(t, board, 613, native.StateDone, "Done", at)
	bc := &fakeBoardClient{project: testProject(), pages: [][]forge.ProjectItem{{
		item("PVTI_1", 613, statusValue("Planned", cardStateAt(t, board, id).Add(time.Minute))),
	}}}
	opts := &ProjectImportOptions{Binding: testBinding()}

	for pass := 1; pass <= 2; pass++ {
		res, err := ImportProjectBoard(context.Background(), bc, testProjectRef, forge.ProviderGitHub, board, opts)
		if err != nil {
			t.Fatalf("pass %d: %v", pass, err)
		}
		if res.RefusedTerminal != 1 {
			t.Errorf("pass %d: RefusedTerminal = %d, want 1 — the divergence must be re-derived every pass (%+v)",
				pass, res.RefusedTerminal, res)
		}
		if res.Reflected != 0 {
			t.Errorf("pass %d: Reflected = %d, want 0", pass, res.Reflected)
		}
	}
	if len(bc.writes) != 0 {
		t.Fatalf("writes = %+v, want none — a refused move must never be pushed back onto the board", bc.writes)
	}
	if got := mustGet(t, board, id).State; got != native.StateDone {
		t.Errorf("state = %q, want %q untouched", got, native.StateDone)
	}
	// And the record still says what the board was last KNOWN to agree on, so
	// the next pass re-derives the same conflict instead of a false no-op.
	if p := mustGet(t, board, id).External.Project; p.Status != "Done" {
		t.Errorf("recorded status = %q, want %q — a status iterion could not apply is not a synced status", p.Status, "Done")
	}
}

// TestSyncProjectBoardConflictReadsTheNativeTransitionTime pins the "newer
// state change wins" rule against the card's REAL transition time.
//
// The rule was resolved against `sync.StateAt` — the moment of iterion's last
// sync WRITE, which only this package sets. A native move made anywhere else
// (studio drag, dispatcher, board MCP tool) does not touch it, so every native
// move inside one interval was systematically under-dated and lost: the
// effective policy was "the board wins", and the reflect branch guarded on the
// native-wins decision was near-unreachable.
func TestSyncProjectBoardConflictReadsTheNativeTransitionTime(t *testing.T) {
	board := newTestBoard(t)
	// The board's status moved an hour ago; iterion last synced then too.
	old := time.Now().UTC().Add(-time.Hour)
	id := seedSynced(t, board, 613, native.StateInbox, "Inbox", old)
	// Now somebody moves the card natively — through the store, like every
	// other surface does. Nothing updates the card's project sync state.
	if _, _, err := board.SetStateFrom(id, native.StateInbox, native.StateReady); err != nil {
		t.Fatalf("native move: %v", err)
	}
	// The board says Blocked, stamped BEFORE the native move.
	bc := &fakeBoardClient{project: testProject(), pages: [][]forge.ProjectItem{{
		item("PVTI_1", 613, statusValue("Blocked", cardStateAt(t, board, id).Add(-30*time.Minute))),
	}}}

	res, err := ImportProjectBoard(context.Background(), bc, testProjectRef, forge.ProviderGitHub, board,
		&ProjectImportOptions{Binding: testBinding()})
	if err != nil {
		t.Fatalf("ImportProjectBoard: %v", err)
	}
	if res.Conflicts != 1 {
		t.Fatalf("Conflicts = %d, want 1 (%+v)", res.Conflicts, res)
	}
	if got := mustGet(t, board, id).State; got != native.StateReady {
		t.Errorf("state = %q, want %q — the NEWER change is the native one", got, native.StateReady)
	}
	if res.Reflected != 1 || len(bc.writes) != 1 {
		t.Fatalf("Reflected = %d writes = %+v, want the native move pushed to the board", res.Reflected, bc.writes)
	}
	if w := bc.writes[0]; w.OptionID != optionID(t, testProject(), "Planned") {
		t.Errorf("wrote option %q, want the Planned option for `ready`", w.OptionID)
	}
}

// The mirror: a board move NEWER than the card's transition still wins, so the
// fix is a corrected comparison rather than an inverted one.
func TestSyncProjectBoardConflictStillLetsANewerBoardWin(t *testing.T) {
	board := newTestBoard(t)
	old := time.Now().UTC().Add(-time.Hour)
	id := seedSynced(t, board, 613, native.StateInbox, "Inbox", old)
	if _, _, err := board.SetStateFrom(id, native.StateInbox, native.StateReady); err != nil {
		t.Fatalf("native move: %v", err)
	}
	// The board moved AFTER the native transition.
	bc := &fakeBoardClient{project: testProject(), pages: [][]forge.ProjectItem{{
		item("PVTI_1", 613, statusValue("Blocked", cardStateAt(t, board, id).Add(time.Hour))),
	}}}

	res, err := ImportProjectBoard(context.Background(), bc, testProjectRef, forge.ProviderGitHub, board,
		&ProjectImportOptions{Binding: testBinding()})
	if err != nil {
		t.Fatalf("ImportProjectBoard: %v", err)
	}
	if res.Conflicts != 1 || res.Moved != 1 {
		t.Errorf("res = %+v, want one conflict the board won", res)
	}
	if got := mustGet(t, board, id).State; got != native.StateBlocked {
		t.Errorf("state = %q, want %q", got, native.StateBlocked)
	}
	if len(bc.writes) != 0 {
		t.Errorf("writes = %+v, want none — the board already shows what it decided", bc.writes)
	}
}

// TestSyncProjectBoardConvergesWhenBothSidesAgree pins that a native-wins
// conflict CONVERGES even when the reflect has nothing to write.
//
// A native-wins conflict deliberately declines to record the board's status,
// on the promise that the reflect will overwrite it with what it wrote. The
// reflect has exits that write nothing — sharpest here: both sides moved
// independently and landed on the SAME column, so there is nothing to push.
// The promise is then unkept, the record stays stale, and the next pass feeds
// decideProjectStatus identical inputs and re-derives the same conflict: a
// Warn and a Conflicts++ per card, on every tick, for two boards that already
// agree.
func TestSyncProjectBoardConvergesWhenBothSidesAgree(t *testing.T) {
	board := newTestBoard(t)
	old := time.Now().UTC().Add(-time.Hour)
	// Recorded "Planned". The card moved natively to blocked, and somebody
	// moved the board to Blocked too — independently, and EARLIER, so the
	// native side wins the conflict and the reflect finds nothing to do.
	id := seedSynced(t, board, 613, native.StateInbox, "Planned", old)
	if _, _, err := board.SetStateFrom(id, native.StateInbox, native.StateBlocked); err != nil {
		t.Fatalf("native move: %v", err)
	}
	boardAt := cardStateAt(t, board, id).Add(-time.Minute)
	bc := &fakeBoardClient{project: testProject(), pages: [][]forge.ProjectItem{{
		item("PVTI_1", 613, statusValue("Blocked", boardAt)),
	}}}
	var logs bytes.Buffer
	opts := &ProjectImportOptions{Binding: testBinding(), Logger: iterlog.New(iterlog.LevelWarn, &logs)}

	res1, err := ImportProjectBoard(context.Background(), bc, testProjectRef, forge.ProviderGitHub, board, opts)
	if err != nil {
		t.Fatalf("pass 1: %v", err)
	}
	if res1.Conflicts != 1 {
		t.Fatalf("pass 1: Conflicts = %d, want 1 (%+v)", res1.Conflicts, res1)
	}
	if len(bc.writes) != 0 {
		t.Fatalf("pass 1: writes = %+v, want none — the board already shows the card's column", bc.writes)
	}

	logs.Reset()
	res2, err := ImportProjectBoard(context.Background(), bc, testProjectRef, forge.ProviderGitHub, board, opts)
	if err != nil {
		t.Fatalf("pass 2: %v", err)
	}
	if res2.Conflicts != 0 {
		t.Errorf("pass 2: Conflicts = %d, want 0 — two agreeing boards must stop being a conflict (%+v)", res2.Conflicts, res2)
	}
	if logs.Len() != 0 {
		t.Errorf("pass 2 logged %q, want silence — a resolved divergence must not warn on every tick", logs.String())
	}
	if len(bc.writes) != 0 {
		t.Errorf("writes = %+v, want none across both passes", bc.writes)
	}
	if got := mustGet(t, board, id).State; got != native.StateBlocked {
		t.Errorf("state = %q, want it untouched", got)
	}
}

// The same non-convergence through the reflect's other silent exits: an
// unmapped native state, and a bound board with no column for the state.
func TestSyncProjectBoardConvergesWhenTheReflectCannotWrite(t *testing.T) {
	for _, tc := range []struct {
		name    string
		state   string
		bind    func(*forge.BoardBinding)
		project func(*testing.T) forge.Project
	}{
		{name: "an unmapped native state is inert, not a standing conflict", state: native.StateReview},
		{
			name: "a board with no column for the state is reported once, not every tick", state: native.StateBlocked,
			// Both halves, or the fixture contradicts itself: dropping the id
			// alone describes a binding the pass's reconciliation would
			// repair from the board's still-present column.
			bind:    func(b *forge.BoardBinding) { delete(b.StatusOptions, native.StateBlocked) },
			project: func(t *testing.T) forge.Project { return deletedColumn(t, "Blocked") },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			board := newTestBoard(t)
			old := time.Now().UTC().Add(-time.Hour)
			id := seedSynced(t, board, 613, native.StateInbox, "Planned", old)
			if _, _, err := board.SetStateFrom(id, native.StateInbox, tc.state); err != nil {
				t.Fatalf("native move: %v", err)
			}
			bind := testBinding()
			if tc.bind != nil {
				tc.bind(bind)
			}
			project := testProject()
			if tc.project != nil {
				project = tc.project(t)
			}
			bc := &fakeBoardClient{project: project, pages: [][]forge.ProjectItem{{
				item("PVTI_1", 613, statusValue("Done", cardStateAt(t, board, id).Add(-time.Minute))),
			}}}
			opts := &ProjectImportOptions{Binding: bind}

			if _, err := ImportProjectBoard(context.Background(), bc, testProjectRef, forge.ProviderGitHub, board, opts); err != nil {
				t.Fatalf("pass 1: %v", err)
			}
			res2, err := ImportProjectBoard(context.Background(), bc, testProjectRef, forge.ProviderGitHub, board, opts)
			if err != nil {
				t.Fatalf("pass 2: %v", err)
			}
			if res2.Conflicts != 0 {
				t.Errorf("pass 2: Conflicts = %d, want 0 — a divergence nothing can resolve must be derived once, not every tick (%+v)",
					res2.Conflicts, res2)
			}
		})
	}
}
