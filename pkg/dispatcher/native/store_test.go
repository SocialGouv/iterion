package native

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
	mk("d", nil)                  // no labels
	mk("e", []string{""})         // empty label string ignored
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
