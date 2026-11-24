package store

import (
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"
)

// rewriteRunJSONBirth rewrites the birth of id's run.json in place: an empty
// birth removes the key (a historical record), any other value replaces it.
func rewriteRunJSONBirth(t *testing.T, s *FilesystemRunStore, id, birth string) {
	t.Helper()
	b, err := os.ReadFile(s.runJSONPath(id))
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err = json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	delete(raw, "birth_id")
	if birth != "" {
		if raw["birth_id"], err = json.Marshal(birth); err != nil {
			t.Fatal(err)
		}
	}
	if b, err = json.Marshal(raw); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(s.runJSONPath(id), b, filePerm); err != nil {
		t.Fatal(err)
	}
	got, err := s.LoadRun(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if got.BirthID != birth {
		t.Fatalf("fixture: rewritten birth reads back as %q, want %q", got.BirthID, birth)
	}
}

// TestMergeExpectedHistoricalBirth: "" is the historical empty birth, never
// "unset". An expectation of it lands on a run.json without birth_id and is
// refused, typed, by a successor holding birth "x" — on both merge doors.
func TestMergeExpectedHistoricalBirth(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	const id = "historical"
	if _, err = s.CreateRun(ctx, id, "wf", nil); err != nil {
		t.Fatal(err)
	}
	rewriteRunJSONBirth(t, s, id, "")
	a, err := s.LoadRun(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	expect := ExpectBirthOf(a)
	if expect != (ExpectedBirth{ID: "", Check: true}) {
		t.Fatalf("fixture: expectation of a historical run = %+v", expect)
	}
	if changed, err := s.UpdateRunMergeIf(ctx, id, RunMergeUpdate{Status: MergeStatusPending, ExpectBirth: expect}, []MergeStatus{""}); err != nil || !changed {
		t.Fatalf("exit expecting the historical birth on a historical run = (%t, %v), want (true, nil)", changed, err)
	}
	notStale := time.Now().Add(-15 * time.Minute)
	claimed, _, token, err := s.ClaimMerge(ctx, id, notStale, expect)
	if err != nil || !claimed {
		t.Fatalf("claim expecting the historical birth on a historical run = (%t, %v), want (true, nil)", claimed, err)
	}

	rewriteRunJSONBirth(t, s, id, "x")
	changed, err := s.UpdateRunMergeIf(ctx, id, RunMergeUpdate{Status: MergeStatusFailed, ExpectBirth: expect, ExpectClaimedAt: token}, []MergeStatus{MergeStatusMerging})
	r, loadErr := s.LoadRun(ctx, id)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if r.MergeStatus != MergeStatusMerging || !r.MergeClaimedAt.Equal(token) {
		t.Fatalf("an exit expecting the historical birth wrote into birth %q: merge_status=%q claimed_at=%v", r.BirthID, r.MergeStatus, r.MergeClaimedAt)
	}
	if !errors.Is(err, ErrRunDataIdentityChanged) || changed {
		t.Fatalf("exit expecting the historical birth on birth \"x\" = (%t, %v), want ErrRunDataIdentityChanged", changed, err)
	}
	if claimed, _, _, err = s.ClaimMerge(ctx, id, time.Now().Add(time.Hour), expect); !errors.Is(err, ErrRunDataIdentityChanged) || claimed {
		t.Fatalf("claim expecting the historical birth on birth \"x\" = (%t, %v), want ErrRunDataIdentityChanged", claimed, err)
	}
	if r, err = s.LoadRun(ctx, id); err != nil {
		t.Fatal(err)
	}
	if !r.MergeClaimedAt.Equal(token) {
		t.Fatalf("a refused claim restamped birth \"x\": claimed_at=%v, want %v", r.MergeClaimedAt, token)
	}
}
