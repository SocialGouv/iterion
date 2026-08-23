package native

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dispatcher/tracker"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	s, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return s
}

func TestNewStoreInitializesBoard(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "board.json")); err != nil {
		t.Fatalf("board.json not created: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "issues")); err != nil {
		t.Fatalf("issues dir not created: %v", err)
	}
	b := s.Board()
	if len(b.States) == 0 {
		t.Fatal("board has no states")
	}
}

func TestNewStorePrependsInboxToLegacyBoard(t *testing.T) {
	// Simulate an existing operator's board.json that predates the
	// `inbox` state — the upgrade path must prepend inbox so bots
	// emitting findings (state=inbox) keep working.
	dir := t.TempDir()
	legacy := Board{
		States: []State{
			{Name: StateBacklog, Display: "Backlog"},
			{Name: StateReady, Display: "Ready", Eligible: true},
			{Name: StateDone, Display: "Done", Terminal: true},
		},
		UpdatedAt: time.Now().UTC(),
	}
	data, err := json.MarshalIndent(&legacy, "", "  ")
	if err != nil {
		t.Fatalf("marshal legacy board: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "board.json"), data, 0o644); err != nil {
		t.Fatalf("write legacy board: %v", err)
	}

	s, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	got := s.Board().States
	// Upgrade also inserts waiting_deps after ready → 5 states.
	if len(got) != 5 {
		t.Fatalf("want 5 states after schema upgrade, got %d: %+v", len(got), got)
	}
	if got[0].Name != StateInbox {
		t.Fatalf("want inbox as first state, got %q", got[0].Name)
	}
	if got[2].Name != StateReady || got[3].Name != StateWaitingDeps {
		t.Fatalf("want waiting_deps after ready, got %+v", got)
	}

	// Re-load to confirm the upgrade was persisted (idempotent: a
	// second NewStore must not prepend twice).
	s2, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore (second pass): %v", err)
	}
	if len(s2.Board().States) != 5 {
		t.Fatalf("schema upgrade ran twice: %+v", s2.Board().States)
	}
}

func TestNewStoreInsertsAwaitingInputAfterInProgress(t *testing.T) {
	// Simulate an existing operator's board.json that predates the
	// `awaiting_input` state — the upgrade path must insert it right
	// after `in_progress` so the dispatcher's paused-run parking
	// (moveToAwaitingInput) works without manual board.json edits.
	dir := t.TempDir()
	legacy := Board{
		States: []State{
			{Name: StateInbox, Display: "Inbox"},
			{Name: StateReady, Display: "Ready", Eligible: true},
			{Name: StateInProgress, Display: "In progress", Eligible: true},
			{Name: StateDone, Display: "Done", Terminal: true},
		},
		UpdatedAt: time.Now().UTC(),
	}
	data, err := json.MarshalIndent(&legacy, "", "  ")
	if err != nil {
		t.Fatalf("marshal legacy board: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "board.json"), data, 0o644); err != nil {
		t.Fatalf("write legacy board: %v", err)
	}

	s, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	got := s.Board().States
	// Upgrade inserts waiting_deps (after ready) + awaiting_input (after in_progress).
	// Legacy had: inbox, ready, in_progress, done → +2 = 6.
	if len(got) != 6 {
		t.Fatalf("want 6 states after schema upgrade, got %d: %+v", len(got), got)
	}
	// ready → waiting_deps → in_progress → awaiting_input
	if got[1].Name != StateReady || got[2].Name != StateWaitingDeps {
		t.Fatalf("want waiting_deps right after ready, got %+v", got)
	}
	if got[2].Eligible || got[2].Terminal {
		t.Fatalf("waiting_deps must be non-eligible, non-terminal: %+v", got[2])
	}
	if got[3].Name != StateInProgress || got[4].Name != StateAwaitingInput {
		t.Fatalf("want awaiting_input right after in_progress, got %+v", got)
	}
	if got[4].Eligible || got[4].Terminal {
		t.Fatalf("awaiting_input must be non-eligible, non-terminal: %+v", got[4])
	}

	// Re-load to confirm the upgrade was persisted and is idempotent.
	s2, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore (second pass): %v", err)
	}
	if len(s2.Board().States) != 6 {
		t.Fatalf("schema upgrade inserted twice: %+v", s2.Board().States)
	}
}

func TestUpgradeBoardSchema(t *testing.T) {
	// Pure-helper contract — the Mongo store applies this on READ (no
	// persistence), so the filesystem-store tests above don't cover it.
	legacy := &Board{States: []State{
		{Name: StateBacklog, Display: "Backlog"},
		{Name: StateReady, Display: "Ready", Eligible: true},
		{Name: StateInProgress, Display: "In progress", Eligible: true},
		{Name: StateDone, Display: "Done", Terminal: true},
	}}
	if !UpgradeBoardSchema(legacy) {
		t.Fatal("legacy board must report changed")
	}
	names := make([]string, 0, len(legacy.States))
	for _, s := range legacy.States {
		names = append(names, s.Name)
	}
	want := []string{StateInbox, StateBacklog, StateReady, StateWaitingDeps, StateInProgress, StateAwaitingInput, StateDone}
	if len(names) != len(want) {
		t.Fatalf("states = %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("states = %v, want %v", names, want)
		}
	}
	if UpgradeBoardSchema(legacy) {
		t.Fatal("second upgrade must be a no-op")
	}
	if UpgradeBoardSchema(DefaultBoard()) {
		t.Fatal("DefaultBoard must not need upgrading")
	}
}

func TestNewStoreLeavesCustomBoardWithoutInProgressUntouched(t *testing.T) {
	// A fully custom board with no `in_progress` state gets NO
	// awaiting_input insert — the dispatcher's "stays in place"
	// fallback covers it.
	dir := t.TempDir()
	custom := Board{
		States: []State{
			{Name: StateInbox, Display: "Inbox"},
			{Name: "triage", Display: "Triage", Eligible: true},
			{Name: "shipped", Display: "Shipped", Terminal: true},
		},
		UpdatedAt: time.Now().UTC(),
	}
	data, err := json.MarshalIndent(&custom, "", "  ")
	if err != nil {
		t.Fatalf("marshal custom board: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "board.json"), data, 0o644); err != nil {
		t.Fatalf("write custom board: %v", err)
	}

	s, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if got := s.Board().States; len(got) != 3 {
		t.Fatalf("custom board must be untouched, got %+v", got)
	}
	if s.Board().StateByName(StateAwaitingInput) != nil {
		t.Fatal("awaiting_input must not be inserted into a board without in_progress")
	}
}

func TestCreateAndGet(t *testing.T) {
	s := newTestStore(t)
	iss, err := s.Create(Issue{Title: "first", State: "ready"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !strings.HasPrefix(iss.ID, "native:") {
		t.Fatalf("ID should be native:<uuid>, got %q", iss.ID)
	}
	if iss.CreatedAt.IsZero() {
		t.Fatal("CreatedAt zero")
	}
	got, err := s.Get(iss.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Title != "first" || got.State != "ready" {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
}

func TestCreateRejectsUnknownState(t *testing.T) {
	s := newTestStore(t)
	_, err := s.Create(Issue{Title: "x", State: "noplace"})
	if err == nil || !strings.Contains(err.Error(), "unknown state") {
		t.Fatalf("want unknown state error, got %v", err)
	}
}

func TestCreateDefaultsToFirstState(t *testing.T) {
	s := newTestStore(t)
	iss, err := s.Create(Issue{Title: "x"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if iss.State != s.Board().States[0].Name {
		t.Fatalf("default state mismatch: got %q want %q", iss.State, s.Board().States[0].Name)
	}
}

func TestCreateRequiresTitle(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.Create(Issue{}); err == nil {
		t.Fatal("expected error for empty title")
	}
}

func TestCreateRejectsInvalidID(t *testing.T) {
	s := newTestStore(t)
	escape := filepath.Join(filepath.Dir(s.root), "escape.json")
	if _, err := s.Create(Issue{ID: "../../escape", Title: "x", State: "ready"}); err == nil {
		t.Fatal("expected error for invalid id")
	}
	if _, err := os.Stat(escape); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("path traversal wrote %q: %v", escape, err)
	}
	if _, err := s.Create(Issue{ID: "native:not-a-uuid", Title: "x", State: "ready"}); err == nil {
		t.Fatal("expected error for non-uuid native id")
	}
}

func TestCreateRejectsDuplicateID(t *testing.T) {
	s := newTestStore(t)
	id := "native:11111111-1111-1111-1111-111111111111"
	if _, err := s.Create(Issue{ID: id, Title: "first", State: "ready"}); err != nil {
		t.Fatalf("Create first: %v", err)
	}
	if _, err := s.Create(Issue{ID: id, Title: "second", State: "ready"}); err == nil {
		t.Fatal("expected duplicate id error")
	}
}

func TestGetNotFound(t *testing.T) {
	s := newTestStore(t)
	_, err := s.Get("native:does-not-exist")
	if !errors.Is(err, tracker.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestListFilterAndSort(t *testing.T) {
	s := newTestStore(t)
	a, _ := s.Create(Issue{Title: "A", State: "ready", Priority: 1, Labels: []string{"x"}})
	b, _ := s.Create(Issue{Title: "B", State: "ready", Priority: 10, Labels: []string{"x", "y"}})
	c, _ := s.Create(Issue{Title: "C", State: "in_progress", Priority: 5, Assignee: "alice"})

	all, err := s.List(ListFilter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("want 3 issues, got %d", len(all))
	}
	// priority desc → B(10), C(5), A(1)
	if all[0].ID != b.ID || all[1].ID != c.ID || all[2].ID != a.ID {
		t.Fatalf("sort order wrong: %s %s %s", all[0].ID, all[1].ID, all[2].ID)
	}

	ready, _ := s.List(ListFilter{States: []string{"ready"}})
	if len(ready) != 2 {
		t.Fatalf("want 2 ready, got %d", len(ready))
	}

	withY, _ := s.List(ListFilter{Labels: []string{"y"}})
	if len(withY) != 1 || withY[0].ID != b.ID {
		t.Fatalf("label filter wrong: %v", withY)
	}

	alice, _ := s.List(ListFilter{Assignee: "alice"})
	if len(alice) != 1 || alice[0].ID != c.ID {
		t.Fatalf("assignee filter wrong: %v", alice)
	}
}

func TestUpdateChangesAndEvents(t *testing.T) {
	s := newTestStore(t)
	iss, _ := s.Create(Issue{Title: "old", State: "ready"})

	newTitle := "new"
	prio := 7
	updated, err := s.Update(iss.ID, Patch{Title: &newTitle, Priority: &prio})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if updated.Title != "new" || updated.Priority != 7 {
		t.Fatalf("patch not applied: %+v", updated)
	}

	var changed []string
	_ = s.ScanEvents(func(e *Event) bool {
		if e.Type == EvtIssueUpdated && e.IssueID == iss.ID {
			if c, ok := e.Payload["changed"].([]any); ok {
				for _, v := range c {
					changed = append(changed, v.(string))
				}
			}
		}
		return true
	})
	if len(changed) != 2 {
		t.Fatalf("expected 2 changed fields, got %v", changed)
	}
}

func TestUpdateFieldsValidates(t *testing.T) {
	s := newTestStore(t)
	if err := s.SetBoard(&Board{
		States: []State{{Name: "ready"}},
		Fields: []Field{{Name: "sev", Type: FieldEnum, EnumValues: []string{"low", "high"}}},
	}); err != nil {
		t.Fatalf("SetBoard: %v", err)
	}
	iss, _ := s.Create(Issue{Title: "x", State: "ready"})
	if _, err := s.Update(iss.ID, Patch{Fields: map[string]any{"sev": "boom"}}); err == nil {
		t.Fatal("expected enum validation error")
	}
	if _, err := s.Update(iss.ID, Patch{Fields: map[string]any{"sev": "high"}}); err != nil {
		t.Fatalf("Update: %v", err)
	}
}

func TestSetStateTransition(t *testing.T) {
	s := newTestStore(t)
	iss, _ := s.Create(Issue{Title: "x", State: "ready"})

	if _, err := s.SetState(iss.ID, "noplace"); !errors.Is(err, tracker.ErrTransitionRejected) {
		t.Fatalf("want ErrTransitionRejected, got %v", err)
	}
	upd, err := s.SetState(iss.ID, "in_progress")
	if err != nil {
		t.Fatalf("SetState: %v", err)
	}
	if upd.State != "in_progress" {
		t.Fatalf("state not updated")
	}

	// no-op when state unchanged
	same, err := s.SetState(iss.ID, "in_progress")
	if err != nil || same.State != "in_progress" {
		t.Fatalf("no-op SetState mishandled: %v %v", err, same)
	}
}

func TestDelete(t *testing.T) {
	s := newTestStore(t)
	iss, _ := s.Create(Issue{Title: "x", State: "ready"})
	if err := s.Delete(iss.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Get(iss.ID); !errors.Is(err, tracker.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	if err := s.Delete(iss.ID); !errors.Is(err, tracker.ErrNotFound) {
		t.Fatalf("second delete want ErrNotFound, got %v", err)
	}
}

func TestClaimRelease(t *testing.T) {
	s := newTestStore(t)
	iss, _ := s.Create(Issue{Title: "x", State: "ready"})

	if err := s.Claim(iss.ID, "host-1"); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	got, _ := s.Get(iss.ID)
	if got.Claim != "host-1" {
		t.Fatalf("claim not stored: %q", got.Claim)
	}

	// same marker is idempotent
	if err := s.Claim(iss.ID, "host-1"); err != nil {
		t.Fatalf("re-claim same marker: %v", err)
	}

	// different marker → conflict
	if err := s.Claim(iss.ID, "host-2"); !errors.Is(err, tracker.ErrClaimConflict) {
		t.Fatalf("want ErrClaimConflict, got %v", err)
	}

	// release with wrong marker → conflict
	if err := s.Release(iss.ID, "host-2"); !errors.Is(err, tracker.ErrClaimConflict) {
		t.Fatalf("want ErrClaimConflict on release, got %v", err)
	}

	// correct marker releases
	if err := s.Release(iss.ID, "host-1"); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if g, _ := s.Get(iss.ID); g.Claim != "" {
		t.Fatalf("claim should be cleared")
	}

	// release on unclaimed is a no-op
	if err := s.Release(iss.ID, "host-1"); err != nil {
		t.Fatalf("Release on unclaimed: %v", err)
	}
}

func TestSetLastRunWritesAndIsIdempotent(t *testing.T) {
	s := newTestStore(t)
	iss, _ := s.Create(Issue{Title: "x", State: "ready"})

	// First write stamps the values and emits one issue_last_run_updated event.
	if err := s.SetLastRun(iss.ID, "run-42", "/tmp/iterion/worktrees/run-42"); err != nil {
		t.Fatalf("SetLastRun: %v", err)
	}
	got, err := s.Get(iss.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.LastRunID != "run-42" || got.LastWorkdir != "/tmp/iterion/worktrees/run-42" {
		t.Fatalf("stamp not persisted: %+v", got)
	}

	// Idempotency: same values must not emit a second event.
	var lastRunEvents int
	countEvents := func() int {
		n := 0
		_ = s.ScanEvents(func(e *Event) bool {
			if e.Type == EvtIssueLastRun && e.IssueID == iss.ID {
				n++
			}
			return true
		})
		return n
	}
	lastRunEvents = countEvents()
	if lastRunEvents != 1 {
		t.Fatalf("first SetLastRun should emit one event, got %d", lastRunEvents)
	}
	if err := s.SetLastRun(iss.ID, "run-42", "/tmp/iterion/worktrees/run-42"); err != nil {
		t.Fatalf("idempotent SetLastRun: %v", err)
	}
	if got := countEvents(); got != 1 {
		t.Fatalf("idempotent call should not emit a new event, got %d", got)
	}

	// Different values overwrite and emit a fresh event.
	if err := s.SetLastRun(iss.ID, "run-43", "/tmp/iterion/worktrees/run-43"); err != nil {
		t.Fatalf("second SetLastRun: %v", err)
	}
	got2, _ := s.Get(iss.ID)
	if got2.LastRunID != "run-43" || got2.LastWorkdir != "/tmp/iterion/worktrees/run-43" {
		t.Fatalf("second stamp not persisted: %+v", got2)
	}
	if got := countEvents(); got != 2 {
		t.Fatalf("second SetLastRun should add one event, got %d", got)
	}

	// Round-trips through reopen — confirms the fields are tagged.
	dir := s.root
	s2, err := NewStore(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	got3, err := s2.Get(iss.ID)
	if err != nil {
		t.Fatalf("Get after reopen: %v", err)
	}
	if got3.LastRunID != "run-43" || got3.LastWorkdir != "/tmp/iterion/worktrees/run-43" {
		t.Fatalf("reopen lost last-run stamp: %+v", got3)
	}
}

func TestSetAwaitingInputWritesAndIsIdempotent(t *testing.T) {
	s := newTestStore(t)
	iss, _ := s.Create(Issue{Title: "x", State: "ready"})

	countEvents := func() int {
		n := 0
		_ = s.ScanEvents(func(e *Event) bool {
			if e.Type == EvtIssueUpdated && e.IssueID == iss.ID {
				n++
			}
			return true
		})
		return n
	}

	// Set true → flag persisted, one event.
	if err := s.SetAwaitingInput(iss.ID, true); err != nil {
		t.Fatalf("SetAwaitingInput(true): %v", err)
	}
	if got, _ := s.Get(iss.ID); !got.AwaitingInput {
		t.Fatalf("awaiting_input not set: %+v", got)
	}
	if got := countEvents(); got != 1 {
		t.Fatalf("first SetAwaitingInput should emit one event, got %d", got)
	}

	// Idempotent: same value → no new event.
	if err := s.SetAwaitingInput(iss.ID, true); err != nil {
		t.Fatalf("idempotent SetAwaitingInput: %v", err)
	}
	if got := countEvents(); got != 1 {
		t.Fatalf("idempotent call should not emit a new event, got %d", got)
	}

	// Clear false → flag cleared, fresh event.
	if err := s.SetAwaitingInput(iss.ID, false); err != nil {
		t.Fatalf("SetAwaitingInput(false): %v", err)
	}
	if got, _ := s.Get(iss.ID); got.AwaitingInput {
		t.Fatalf("awaiting_input not cleared: %+v", got)
	}
	if got := countEvents(); got != 2 {
		t.Fatalf("clear should add one event, got %d", got)
	}

	// Round-trips through reopen — confirms the field is tagged.
	if err := s.SetAwaitingInput(iss.ID, true); err != nil {
		t.Fatalf("SetAwaitingInput(true) again: %v", err)
	}
	s2, err := NewStore(s.root)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if got, _ := s2.Get(iss.ID); !got.AwaitingInput {
		t.Fatalf("reopen lost awaiting_input flag: %+v", got)
	}
}

func TestAddCommentPersistsAndEmits(t *testing.T) {
	s := newTestStore(t)
	iss, _ := s.Create(Issue{Title: "x", State: "ready"})

	updated, c, err := s.AddComment(iss.ID, "operator", "/willy-rgaa fix the contrast issues")
	if err != nil {
		t.Fatalf("AddComment: %v", err)
	}
	if c.ID == "" || c.Author != "operator" || c.Body == "" {
		t.Fatalf("comment not populated: %+v", c)
	}
	if len(updated.Comments) != 1 || updated.Comments[0].ID != c.ID {
		t.Fatalf("comment not appended to issue: %+v", updated.Comments)
	}

	// Empty body rejected.
	if _, _, err := s.AddComment(iss.ID, "operator", "   "); err == nil {
		t.Fatal("empty comment body should be rejected")
	}
	// Unknown issue → ErrNotFound.
	if _, _, err := s.AddComment("native:nope", "operator", "hi"); !errors.Is(err, tracker.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}

	// One EvtIssueComment event recorded.
	n := 0
	_ = s.ScanEvents(func(e *Event) bool {
		if e.Type == EvtIssueComment && e.IssueID == iss.ID {
			n++
		}
		return true
	})
	if n != 1 {
		t.Fatalf("want 1 issue_comment_added event, got %d", n)
	}

	// Round-trips through reopen.
	s2, err := NewStore(s.root)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	got, err := s2.Get(iss.ID)
	if err != nil {
		t.Fatalf("Get after reopen: %v", err)
	}
	if len(got.Comments) != 1 || got.Comments[0].Body != "/willy-rgaa fix the contrast issues" {
		t.Fatalf("reopen lost comment: %+v", got.Comments)
	}
}

func TestSetLastRunAppendsRunHistory(t *testing.T) {
	s := newTestStore(t)
	iss, _ := s.Create(Issue{Title: "x", State: "ready"})

	// Two different run ids → two RunRefs, newest-last.
	if err := s.SetLastRun(iss.ID, "run-1", "/tmp/wd-1"); err != nil {
		t.Fatalf("SetLastRun run-1: %v", err)
	}
	if err := s.SetLastRun(iss.ID, "run-2", "/tmp/wd-2"); err != nil {
		t.Fatalf("SetLastRun run-2: %v", err)
	}
	got, _ := s.Get(iss.ID)
	if len(got.Runs) != 2 {
		t.Fatalf("expected 2 run refs, got %d: %+v", len(got.Runs), got.Runs)
	}
	if got.Runs[0].RunID != "run-1" || got.Runs[1].RunID != "run-2" {
		t.Fatalf("run history not newest-last: %+v", got.Runs)
	}
	if got.Runs[1].Workdir != "/tmp/wd-2" {
		t.Fatalf("workdir not captured: %+v", got.Runs[1])
	}
	if got.Runs[0].At.IsZero() {
		t.Fatalf("At not stamped: %+v", got.Runs[0])
	}

	// Same run id again with a new workdir → still 2 refs, updated in place.
	if err := s.SetLastRun(iss.ID, "run-1", "/tmp/wd-1-moved"); err != nil {
		t.Fatalf("SetLastRun run-1 again: %v", err)
	}
	got2, _ := s.Get(iss.ID)
	if len(got2.Runs) != 2 {
		t.Fatalf("re-stamping same run id must not append, got %d: %+v", len(got2.Runs), got2.Runs)
	}
	if got2.Runs[0].RunID != "run-1" || got2.Runs[0].Workdir != "/tmp/wd-1-moved" {
		t.Fatalf("dedup-update failed: %+v", got2.Runs)
	}

	// History survives reopen (field is tagged / persisted).
	s2, err := NewStore(s.root)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	got3, _ := s2.Get(iss.ID)
	if len(got3.Runs) != 2 || got3.Runs[1].RunID != "run-2" {
		t.Fatalf("reopen lost run history: %+v", got3.Runs)
	}
}

func TestSetLastRunUnknownIssue(t *testing.T) {
	s := newTestStore(t)
	if err := s.SetLastRun("native:nope", "run-x", "/tmp/x"); !errors.Is(err, tracker.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestEventSequenceMonotonic(t *testing.T) {
	s := newTestStore(t)
	iss, _ := s.Create(Issue{Title: "x", State: "ready"})
	_, _ = s.SetState(iss.ID, "in_progress")
	_ = s.Claim(iss.ID, "marker")
	_ = s.Release(iss.ID, "marker")
	_ = s.Delete(iss.ID)

	var seqs []int64
	if err := s.ScanEvents(func(e *Event) bool {
		seqs = append(seqs, e.Seq)
		return true
	}); err != nil {
		t.Fatalf("ScanEvents: %v", err)
	}
	if len(seqs) != 5 {
		t.Fatalf("want 5 events, got %d (%v)", len(seqs), seqs)
	}
	for i := 1; i < len(seqs); i++ {
		if seqs[i] != seqs[i-1]+1 {
			t.Fatalf("seq not monotonic: %v", seqs)
		}
	}
}

func TestReopenPersists(t *testing.T) {
	dir := t.TempDir()
	s1, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	iss, _ := s1.Create(Issue{Title: "persist me", State: "ready"})

	s2, err := NewStore(dir)
	if err != nil {
		t.Fatalf("reopen NewStore: %v", err)
	}
	got, err := s2.Get(iss.ID)
	if err != nil {
		t.Fatalf("Get after reopen: %v", err)
	}
	if got.Title != "persist me" {
		t.Fatalf("round trip after reopen mismatch")
	}

	// sequence continues from the existing log
	_, _ = s2.SetState(iss.ID, "in_progress")
	var seqs []int64
	_ = s2.ScanEvents(func(e *Event) bool {
		seqs = append(seqs, e.Seq)
		return true
	})
	if len(seqs) < 2 || seqs[len(seqs)-1] <= seqs[0] {
		t.Fatalf("seq did not advance after reopen: %v", seqs)
	}
}

func TestSetBoardValidation(t *testing.T) {
	s := newTestStore(t)
	bad := &Board{States: []State{{Name: "a"}, {Name: "a"}}}
	if err := s.SetBoard(bad); err == nil {
		t.Fatal("expected validation error")
	}
}

// TestLabelVocabularyOps: rename / merge / delete touch the right
// issues, emit the right events, and are idempotent. Also covers the
// edge cases (empty input, from == to, label already present on the
// target during a rename).
func TestLabelVocabularyOps(t *testing.T) {
	s := newTestStore(t)
	mk := func(title string, labels []string) string {
		iss, err := s.Create(Issue{Title: title, State: "backlog", Labels: labels})
		if err != nil {
			t.Fatalf("Create %q: %v", title, err)
		}
		return iss.ID
	}
	mk("a", []string{"old", "keep"})
	mk("b", []string{"old"})
	idC := mk("c", []string{"keep", "new"}) // already has the rename target
	mk("d", []string{"keep"})

	// Rename old → new: 3 issues touched (a + b adopt new; c drops a
	// stale 'old' if it had one — it doesn't, but the rewrite path is
	// idempotent so verifying it touches only a, b is enough). c also
	// has 'new' already, so when a + b adopt it the set stays unique.
	n, err := s.RenameLabel("old", "new")
	if err != nil {
		t.Fatalf("RenameLabel: %v", err)
	}
	if n != 2 {
		t.Errorf("rename touched %d, want 2", n)
	}
	got := s.AggregateLabels()
	want := map[string]int{"new": 3, "keep": 3}
	for _, u := range got {
		if exp, ok := want[u.Label]; ok && u.Count != exp {
			t.Errorf("after rename: %q count = %d, want %d", u.Label, u.Count, exp)
		}
	}
	if _, present := labelMap(got)["old"]; present {
		t.Errorf("rename did not remove 'old' from the board")
	}

	// Idempotent: re-running rename is a no-op now that nothing
	// carries 'old' anymore.
	if n, err := s.RenameLabel("old", "new"); err != nil || n != 0 {
		t.Errorf("rename idempotent: n=%d err=%v, want 0/nil", n, err)
	}

	// Merge new → keep: every issue carrying 'new' ends up with
	// 'keep' (deduped) and no longer 'new'.
	if _, err := s.MergeLabels("new", "keep"); err != nil {
		t.Fatalf("MergeLabels: %v", err)
	}
	got = s.AggregateLabels()
	if _, present := labelMap(got)["new"]; present {
		t.Errorf("merge did not remove 'new' from the board")
	}
	keepRow := labelMap(got)["keep"]
	if keepRow.Count != 4 {
		t.Errorf("after merge: 'keep' count = %d, want 4", keepRow.Count)
	}

	// Delete 'keep' (now the only label): board becomes empty of labels.
	if _, err := s.DeleteLabel("keep"); err != nil {
		t.Fatalf("DeleteLabel: %v", err)
	}
	if len(s.AggregateLabels()) != 0 {
		t.Errorf("delete did not clear: %+v", s.AggregateLabels())
	}

	// Edge cases.
	if _, err := s.RenameLabel("", "x"); err != ErrLabelEmpty {
		t.Errorf("rename empty from: err = %v, want ErrLabelEmpty", err)
	}
	if _, err := s.RenameLabel("x", ""); err != ErrLabelEmpty {
		t.Errorf("rename empty to: err = %v, want ErrLabelEmpty", err)
	}
	if n, err := s.RenameLabel("same", "same"); err != nil || n != 0 {
		t.Errorf("rename same→same: n=%d err=%v, want 0/nil", n, err)
	}
	if _, err := s.DeleteLabel(""); err != ErrLabelEmpty {
		t.Errorf("delete empty: err = %v, want ErrLabelEmpty", err)
	}

	// Audit-trail events for the rename op should land for each touched
	// issue. Read directly from disk; the in-memory store doesn't expose
	// an event tail.
	var renameEvents int
	_ = s.ScanEvents(func(e *Event) bool {
		if e.Type == EvtLabelRename {
			renameEvents++
		}
		return true
	})
	if renameEvents != 2 {
		t.Errorf("EvtLabelRename emitted %d times, want 2 (one per touched issue)", renameEvents)
	}
	_ = idC // silence unused — kept for readability of the table-style fixture
}

func labelMap(usage []LabelUsage) map[string]LabelUsage {
	m := make(map[string]LabelUsage, len(usage))
	for _, u := range usage {
		m[u.Label] = u
	}
	return m
}

// TestAggregateLabels: counts and orders the distinct labels across
// the store. Issues with empty Labels contribute nothing; the order is
// (count desc, label asc); duplicates within one issue's label slice
// count once per issue.
func TestAggregateLabels(t *testing.T) {
	s := newTestStore(t)
	mk := func(title string, labels []string) {
		if _, err := s.Create(Issue{Title: title, State: "backlog", Labels: labels}); err != nil {
			t.Fatalf("Create %q: %v", title, err)
		}
	}
	mk("a", []string{"source:whats-next", "horizon:short-term"})
	mk("b", []string{"source:whats-next", "horizon:next-action", "epic:battle-tested"})
	mk("c", []string{"epic:battle-tested"})
	mk("d", nil)          // no labels
	mk("e", []string{""}) // empty label string ignored
	mk("f", []string{"source:whats-next"})

	got := s.AggregateLabels()
	if len(got) != 4 {
		t.Fatalf("got %d distinct labels, want 4: %+v", len(got), got)
	}
	// Order: source:whats-next (3) > epic:battle-tested (2) >
	// horizon:next-action (1, "h" alphabetically before "horizon:short-term") >
	// horizon:short-term (1).
	wantOrder := []string{
		"source:whats-next",
		"epic:battle-tested",
		"horizon:next-action",
		"horizon:short-term",
	}
	for i, w := range wantOrder {
		if got[i].Label != w {
			t.Errorf("position %d: got %q, want %q", i, got[i].Label, w)
		}
	}
	if got[0].Count != 3 || got[1].Count != 2 || got[2].Count != 1 || got[3].Count != 1 {
		t.Errorf("counts: %+v", got)
	}
	for i, u := range got {
		if u.LastUsedAt == "" {
			t.Errorf("row %d (%s) missing last_used_at", i, u.Label)
		}
	}
}

// The give-up stamp is what lets a reader tell the dispatcher filing a ticket
// (retry budget exhausted) from an operator filing the same terminal state by
// hand. It must persist, expire on its own, and not churn the card on
// re-stamps — a stamp rewritten on every poll would bump UpdatedAt and defeat
// the board's `?since=` pruning.
func TestSetGaveUpStampsExpiresAndIsIdempotent(t *testing.T) {
	s := newTestStore(t)
	iss, _ := s.Create(Issue{Title: "doomed", State: StateInProgress})

	countEvents := func() int {
		n := 0
		_ = s.ScanEvents(func(e *Event) bool {
			if e.Type == EvtIssueGaveUp && e.IssueID == iss.ID {
				n++
			}
			return true
		})
		return n
	}

	// The dispatcher's order: file the ticket, THEN stamp what filed it.
	if _, err := s.SetState(iss.ID, StateBlocked); err != nil {
		t.Fatalf("SetState: %v", err)
	}
	stamp := &GiveUp{RunID: "run-7", State: StateBlocked, Attempts: 3}
	if err := s.SetGaveUp(iss.ID, stamp); err != nil {
		t.Fatalf("SetGaveUp: %v", err)
	}
	got, err := s.Get(iss.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.GaveUp == nil || got.GaveUp.RunID != "run-7" || got.GaveUp.Attempts != 3 {
		t.Fatalf("stamp not persisted: %+v", got.GaveUp)
	}
	if got.GaveUp.At.IsZero() {
		t.Error("SetGaveUp must timestamp a stamp that arrives without one")
	}
	if !got.GaveUp.Current(got.State, "run-7") {
		t.Error("the stamp must describe the ticket it was just written on")
	}
	// Expiry, with nobody having to clear anything: another run's card is not
	// this give-up, and a ticket someone has moved since is no longer it.
	if got.GaveUp.Current(got.State, "run-8") {
		t.Error("a stamp claimed a run it was not written for")
	}
	if got.GaveUp.Current(StateDone, "run-7") {
		t.Error("a stamp survived the ticket being moved elsewhere")
	}

	if got := countEvents(); got != 1 {
		t.Fatalf("first SetGaveUp emitted %d events, want 1", got)
	}
	// Idempotent on the fields that decide behaviour — the timestamp is
	// provenance, not identity.
	if err := s.SetGaveUp(iss.ID, &GiveUp{RunID: "run-7", State: StateBlocked, Attempts: 3}); err != nil {
		t.Fatalf("re-stamp: %v", err)
	}
	if got := countEvents(); got != 1 {
		t.Fatalf("re-stamping the same give-up emitted %d events, want 1", got)
	}

	// Clearing is the explicit acknowledgement path (board Close).
	if err := s.SetGaveUp(iss.ID, nil); err != nil {
		t.Fatalf("SetGaveUp(nil): %v", err)
	}
	if got, _ := s.Get(iss.ID); got.GaveUp != nil {
		t.Fatalf("stamp survived the clear: %+v", got.GaveUp)
	}
	if got := countEvents(); got != 2 {
		t.Fatalf("clearing emitted no event (%d total), want 2", got)
	}
	// Clearing an unstamped issue is a no-op, not a third event.
	if err := s.SetGaveUp(iss.ID, nil); err != nil {
		t.Fatalf("SetGaveUp(nil) twice: %v", err)
	}
	if got := countEvents(); got != 2 {
		t.Fatalf("clearing twice emitted %d events, want 2", got)
	}
}

// SetLastRun's own contract says empty strings clear the pointer — the
// operator's escape hatch from a dead run that keeps being resumed. A cleared
// pointer is not a run that happened, so it must not land in the card's run
// history as a blank row.
func TestSetLastRunClearDoesNotAppendABlankRun(t *testing.T) {
	s := newTestStore(t)
	iss, _ := s.Create(Issue{Title: "x", State: StateReady})
	if err := s.SetLastRun(iss.ID, "run-1", "/tmp/wd"); err != nil {
		t.Fatalf("SetLastRun: %v", err)
	}
	if err := s.SetLastRun(iss.ID, "", ""); err != nil {
		t.Fatalf("SetLastRun clear: %v", err)
	}
	got, err := s.Get(iss.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.LastRunID != "" || got.LastWorkdir != "" {
		t.Fatalf("pointer not cleared: %+v", got)
	}
	if len(got.Runs) != 1 || got.Runs[0].RunID != "run-1" {
		t.Fatalf("run history = %+v, want the one real run and no blank row", got.Runs)
	}
}

// Once the ticket moves, the give-up stamp is gone for GOOD. Read-time state
// comparison alone would be reversible: a dispatcher retry resumes the SAME
// run id, so a ticket that leaves the stamped state and later returns to it
// would make the stamp live again — and the board would re-file an
// operator's own decision as an unattended give-up, the mirror image of the
// bug the stamp exists to fix.
func TestGiveUpStampDoesNotComeBackWhenTheTicketReturns(t *testing.T) {
	s := newTestStore(t)
	iss, _ := s.Create(Issue{Title: "doomed", State: StateInProgress})
	if _, err := s.SetState(iss.ID, StateBlocked); err != nil {
		t.Fatalf("SetState(blocked): %v", err)
	}
	if err := s.SetGaveUp(iss.ID, &GiveUp{RunID: "run-7", State: StateBlocked, Attempts: 3}); err != nil {
		t.Fatalf("SetGaveUp: %v", err)
	}

	// The operator retries: the ticket is restaged, which supersedes the
	// give-up. Any write in the new state expires the stamp.
	if _, err := s.SetState(iss.ID, StateReady); err != nil {
		t.Fatalf("SetState(ready): %v", err)
	}
	if got, _ := s.Get(iss.ID); got.GaveUp != nil {
		t.Fatalf("stamp survived the ticket leaving its state: %+v", got.GaveUp)
	}
	// …and the operator files it by hand later, same run still current.
	if _, err := s.SetState(iss.ID, StateBlocked); err != nil {
		t.Fatalf("SetState(blocked) again: %v", err)
	}
	got, err := s.Get(iss.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.GaveUp != nil {
		t.Fatalf("stamp came back to life on a filing a human made: %+v", got.GaveUp)
	}
}

// A give-up whose ticket has already MOVED is superseded: an operator got
// there between the terminal move and the stamp, so recording it would put
// their own choice under a "the dispatcher gave up and filed this ticket
// as …" banner — and, on a non-terminal target, badge a live card with it.
func TestSetGaveUpSkipsAGiveUpTheTicketHasMovedPast(t *testing.T) {
	s := newTestStore(t)
	iss, _ := s.Create(Issue{Title: "raced", State: StateInProgress})
	// The dispatcher believes it filed the ticket as blocked; the operator
	// dragged it back to ready first.
	if _, err := s.SetState(iss.ID, StateReady); err != nil {
		t.Fatalf("SetState: %v", err)
	}
	if err := s.SetGaveUp(iss.ID, &GiveUp{RunID: "run-9", State: StateBlocked, Attempts: 2}); err != nil {
		t.Fatalf("SetGaveUp: %v", err)
	}
	got, err := s.Get(iss.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.GaveUp != nil {
		t.Errorf("a superseded give-up was recorded: %+v", got.GaveUp)
	}
	n := 0
	_ = s.ScanEvents(func(e *Event) bool {
		if e.Type == EvtIssueGaveUp && e.IssueID == iss.ID {
			n++
		}
		return true
	})
	if n != 0 {
		t.Errorf("give-up events = %d, want 0 — nothing was recorded", n)
	}
}

// A stamp that arrives without a state is filled in from the issue, so the
// value compared for idempotence and the value written are the same thing.
func TestSetGaveUpFillsAMissingStateFromTheTicket(t *testing.T) {
	s := newTestStore(t)
	iss, _ := s.Create(Issue{Title: "doomed", State: StateBlocked})
	if err := s.SetGaveUp(iss.ID, &GiveUp{RunID: "run-9", Attempts: 2}); err != nil {
		t.Fatalf("SetGaveUp: %v", err)
	}
	got, err := s.Get(iss.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.GaveUp == nil || got.GaveUp.State != StateBlocked {
		t.Fatalf("stamp = %+v, want it filled in with %q", got.GaveUp, StateBlocked)
	}
	if !got.GaveUp.Current(got.State, "run-9") {
		t.Error("a freshly written stamp must describe the ticket it was written on")
	}
}

// The audit record must name the state that was STAMPED. SetGaveUp overrides
// a caller's state with the ticket's real one (so the stamp is not born
// stale), and events.jsonl exists precisely so an operator can reconstruct why
// a ticket ended where it did — reporting the discarded value would make the
// one durable record of a give-up disagree with the issue it describes.
func TestSetGaveUpEventReportsTheStateItStamped(t *testing.T) {
	s := newTestStore(t)
	iss, _ := s.Create(Issue{Title: "raced", State: StateInProgress})
	// No state from the caller: the store fills it in, and the event must
	// report what it filled rather than the empty value it was handed.
	if err := s.SetGaveUp(iss.ID, &GiveUp{RunID: "run-9", Attempts: 2}); err != nil {
		t.Fatalf("SetGaveUp: %v", err)
	}
	var payload map[string]any
	_ = s.ScanEvents(func(e *Event) bool {
		if e.Type == EvtIssueGaveUp && e.IssueID == iss.ID {
			payload = e.Payload
		}
		return true
	})
	if payload == nil {
		t.Fatal("no issue_gave_up event recorded")
	}
	if got := payload["state"]; got != StateInProgress {
		t.Errorf("event state = %v, want the stamped %q", got, StateInProgress)
	}
	if got := payload["run_id"]; got != "run-9" {
		t.Errorf("event run_id = %v, want run-9", got)
	}
}

// Re-stamping the same give-up must stay a no-op even when the caller left the
// state out — the store fills it in, so comparing the caller's value would
// re-write and re-emit on every call, churning the card's UpdatedAt (and with
// it the board's `?since=` pruning) on every dispatcher tick.
func TestSetGaveUpIsIdempotentAgainstWhatItWouldWrite(t *testing.T) {
	s := newTestStore(t)
	iss, _ := s.Create(Issue{Title: "raced", State: StateInProgress})
	// No state from the caller — the case where comparing the caller's value
	// instead of the written one would re-write on every call.
	stamp := func() *GiveUp {
		return &GiveUp{RunID: "run-9", Attempts: 2}
	}
	if err := s.SetGaveUp(iss.ID, stamp()); err != nil {
		t.Fatalf("SetGaveUp: %v", err)
	}
	first, _ := s.Get(iss.ID)
	if err := s.SetGaveUp(iss.ID, stamp()); err != nil {
		t.Fatalf("SetGaveUp again: %v", err)
	}
	again, _ := s.Get(iss.ID)
	if !again.UpdatedAt.Equal(first.UpdatedAt) {
		t.Errorf("a repeat stamp rewrote the issue: %s → %s", first.UpdatedAt, again.UpdatedAt)
	}
	n := 0
	_ = s.ScanEvents(func(e *Event) bool {
		if e.Type == EvtIssueGaveUp && e.IssueID == iss.ID {
			n++
		}
		return true
	})
	if n != 1 {
		t.Errorf("give-up events = %d, want 1 — the repeat call re-emitted", n)
	}
}
