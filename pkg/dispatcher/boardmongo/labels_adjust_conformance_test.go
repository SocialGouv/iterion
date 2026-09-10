package boardmongo_test

import (
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/dispatcher/tracker"
)

// countUpdatedEvents counts EvtIssueUpdated records for id.
func countUpdatedEvents(t *testing.T, s native.BoardStore, id string) int {
	t.Helper()
	n := 0
	if err := s.ScanEvents(func(e *native.Event) bool {
		if e.Type == native.EvtIssueUpdated && e.IssueID == id {
			n++
		}
		return true
	}); err != nil {
		t.Fatalf("ScanEvents: %v", err)
	}
	return n
}

// runAdjustLabelsSuite pins BoardStore.AdjustLabels on both twins: the
// RELATIVE label operations behind add_labels / remove_labels. Their
// whole point is atomicity against the card as it is — a consumed
// one-shot must not come back, and concurrent adds must not lose each
// other — which is why the Mongo twin is one conditional write and the
// FS twin one critical section, and why the suite runs on both.
func runAdjustLabelsSuite(t *testing.T, store native.BoardStore) {
	t.Helper()
	iss, err := store.Create(native.Issue{Title: "labels", State: native.StateInbox, Labels: []string{"a", "b"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// Add: existing kept in order, new appended, duplicates collapsed,
	// blanks ignored, present ones not re-added.
	got, changed, err := store.AdjustLabels(iss.ID, []string{"b", "c", "c", " d ", ""}, nil)
	if err != nil || !changed {
		t.Fatalf("AdjustLabels(add): changed=%v err=%v", changed, err)
	}
	want := []string{"a", "b", "c", "d"}
	if !slices.Equal(got.Labels, want) {
		t.Fatalf("returned labels = %v, want %v", got.Labels, want)
	}
	if cur, _ := store.Get(iss.ID); !slices.Equal(cur.Labels, want) || !cur.UpdatedAt.After(iss.UpdatedAt) {
		t.Fatalf("stored labels = %v (updated %s vs created %s), want %v and a bumped UpdatedAt", cur.Labels, cur.UpdatedAt, iss.UpdatedAt, want)
	}
	events := countUpdatedEvents(t, store, iss.ID)

	// A no-op add writes nothing: no event, no UpdatedAt churn.
	before, _ := store.Get(iss.ID)
	if got, changed, err := store.AdjustLabels(iss.ID, []string{"a", "b"}, nil); err != nil || changed || !slices.Equal(got.Labels, want) {
		t.Fatalf("no-op add: changed=%v err=%v labels=%v", changed, err, got.Labels)
	}
	if after, _ := store.Get(iss.ID); !after.UpdatedAt.Equal(before.UpdatedAt) {
		t.Fatalf("a no-op add bumped UpdatedAt: %s → %s", before.UpdatedAt, after.UpdatedAt)
	}
	if n := countUpdatedEvents(t, store, iss.ID); n != events {
		t.Fatalf("a no-op add emitted %d event(s)", n-events)
	}

	// Remove: the rest stays in place; an absent label is a no-op.
	got, changed, err = store.AdjustLabels(iss.ID, nil, []string{"a", "zzz"})
	if err != nil || !changed || !slices.Equal(got.Labels, []string{"b", "c", "d"}) {
		t.Fatalf("AdjustLabels(remove): changed=%v err=%v labels=%v", changed, err, got.Labels)
	}
	// Overlap: a label named in both is removed, not added.
	got, changed, err = store.AdjustLabels(iss.ID, []string{"e", "b"}, []string{"b"})
	if err != nil || !changed || !slices.Equal(got.Labels, []string{"c", "d", "e"}) {
		t.Fatalf("AdjustLabels(overlap): changed=%v err=%v labels=%v", changed, err, got.Labels)
	}
	// The event carries the delta the audit tail wants, under the same
	// type set_labels emits (the trigger spine's card.updated).
	var last map[string]any
	if err := store.ScanEvents(func(e *native.Event) bool {
		if e.Type == native.EvtIssueUpdated && e.IssueID == iss.ID {
			last = e.Payload
		}
		return true
	}); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(last["labels_added"]) != "[e]" || fmt.Sprint(last["labels_removed"]) != "[b]" {
		t.Fatalf("event payload = %v, want labels_added=[e] labels_removed=[b]", last)
	}

	// A missing card is an error, never a silent no-op.
	if _, _, err := store.AdjustLabels("native:00000000-0000-0000-0000-000000000000", []string{"x"}, nil); !errors.Is(err, tracker.ErrNotFound) {
		t.Fatalf("missing card: err=%v, want ErrNotFound", err)
	}

	// The defect that motivated the op: a one-shot consumed between a
	// bot's read and its write must NOT come back through a relative add.
	shot, err := store.Create(native.Issue{Title: "one-shot", State: native.StateInbox, Labels: []string{"triage:auto", "kind:bug"}})
	if err != nil {
		t.Fatal(err)
	}
	consumed := []string{"kind:bug"} // the spine's consume (Update on FS, ConsumeLabels on Mongo)
	if _, err := store.Update(shot.ID, native.Patch{Labels: &consumed}); err != nil {
		t.Fatal(err)
	}
	if got, _, err := store.AdjustLabels(shot.ID, []string{"source:issue-triage"}, nil); err != nil || slices.Contains(got.Labels, "triage:auto") {
		t.Fatalf("a relative add re-armed a consumed one-shot: labels=%v err=%v", got.Labels, err)
	}

	// No lost update: N concurrent adds of distinct labels all land.
	race, err := store.Create(native.Issue{Title: "concurrent", State: native.StateInbox})
	if err != nil {
		t.Fatal(err)
	}
	const n = 16
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, _, err := store.AdjustLabels(race.ID, []string{fmt.Sprintf("l%02d", i)}, nil); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent AdjustLabels: %v", err)
	}
	final, _ := store.Get(race.ID)
	if len(final.Labels) != n {
		t.Fatalf("concurrent adds lost updates: %d of %d labels landed: %v", len(final.Labels), n, final.Labels)
	}
}
