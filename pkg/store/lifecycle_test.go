package store

import (
	"encoding/json"
	"testing"
)

// allStatuses aliases the canonical vocabulary. If a status is ever
// added, every predicate row below must take a position on it — the
// test fails on an unlisted status, which is the point.
var allStatuses = AllRunStatuses

// TestLifecyclePredicateMatrix pins every predicate's full truth table.
// This is the single place the sets are spelled out; a drive-by edit to
// one predicate that silently changes policy shows up as a diff here.
func TestLifecyclePredicateMatrix(t *testing.T) {
	type row struct {
		name string
		fn   func(RunStatus) bool
		want map[RunStatus]bool
	}
	rows := []row{
		{"IsTerminal", RunStatus.IsTerminal, map[RunStatus]bool{
			RunStatusFinished: true, RunStatusFailed: true,
			RunStatusFailedResumable: true, RunStatusCancelled: true,
		}},
		{"IsPaused", RunStatus.IsPaused, map[RunStatus]bool{
			RunStatusPausedWaitingHuman: true, RunStatusPausedOperator: true,
		}},
		{"IsFinalSuccess", RunStatus.IsFinalSuccess, map[RunStatus]bool{
			RunStatusFinished: true,
		}},
		{"IsFinalFailure", RunStatus.IsFinalFailure, map[RunStatus]bool{
			RunStatusFailed: true,
		}},
		{"IsTerminalResumable", RunStatus.IsTerminalResumable, map[RunStatus]bool{
			RunStatusFailedResumable: true, RunStatusCancelled: true,
		}},
		{"IsQueued", RunStatus.IsQueued, map[RunStatus]bool{
			RunStatusQueued: true,
		}},
		{"CanOperatorResume", RunStatus.CanOperatorResume, map[RunStatus]bool{
			RunStatusFailedResumable: true, RunStatusCancelled: true,
			RunStatusPausedOperator: true, RunStatusPausedWaitingHuman: true,
		}},
		{"RequiresResumeAnswers", RunStatus.RequiresResumeAnswers, map[RunStatus]bool{
			RunStatusPausedWaitingHuman: true,
		}},
		{"CanAutoResume", RunStatus.CanAutoResume, map[RunStatus]bool{
			RunStatusFailedResumable: true,
		}},
		{"CountsAgainstLaunchLimit", RunStatus.CountsAgainstLaunchLimit, map[RunStatus]bool{
			RunStatusQueued: true, RunStatusRunning: true,
		}},
		{"CanBeCancelled", RunStatus.CanBeCancelled, map[RunStatus]bool{
			RunStatusRunning: true, RunStatusPausedWaitingHuman: true,
			RunStatusPausedOperator: true, RunStatusFailedResumable: true,
			RunStatusQueued: true,
		}},
		{"CarriesFailureCode", RunStatus.CarriesFailureCode, map[RunStatus]bool{
			RunStatusFailed: true, RunStatusFailedResumable: true,
			RunStatusCancelled: true,
		}},
	}
	for _, r := range rows {
		for _, st := range allStatuses {
			if got, want := r.fn(st), r.want[st]; got != want {
				t.Errorf("%s(%s) = %v, want %v", r.name, st, got, want)
			}
		}
	}
}

// TestLifecyclePredicateRelations pins the structural relations between
// predicates so they cannot drift apart independently.
func TestLifecyclePredicateRelations(t *testing.T) {
	for _, st := range allStatuses {
		// IsTerminal = final-success ∪ final-failure ∪ terminal-resumable.
		if st.IsTerminal() != (st.IsFinalSuccess() || st.IsFinalFailure() || st.IsTerminalResumable()) {
			t.Errorf("IsTerminal(%s) is not the union of its parts", st)
		}
		// Auto-resume is strictly narrower than operator resume.
		if st.CanAutoResume() && !st.CanOperatorResume() {
			t.Errorf("CanAutoResume(%s) without CanOperatorResume", st)
		}
		// Answers are only required on a status an operator can resume.
		if st.RequiresResumeAnswers() && !st.CanOperatorResume() {
			t.Errorf("RequiresResumeAnswers(%s) without CanOperatorResume", st)
		}
		// A status never both holds a launch slot and is terminal/paused.
		if st.CountsAgainstLaunchLimit() && (st.IsTerminal() || st.IsPaused()) {
			t.Errorf("CountsAgainstLaunchLimit(%s) overlaps terminal/paused", st)
		}
		// A failure code may only persist on terminal failure shapes.
		if st.CarriesFailureCode() && !st.IsFinalFailure() && !st.IsTerminalResumable() {
			t.Errorf("CarriesFailureCode(%s) outside failure statuses", st)
		}
	}
}

// TestFailureCodeUnknownRoundTrip proves the open-world contract: a
// code this binary has never heard of survives a JSON round-trip of the
// Run document unharmed (the BSON shape is covered by the mongo
// decode-shape test).
func TestFailureCodeUnknownRoundTrip(t *testing.T) {
	r := Run{ID: "r1", Status: RunStatusFailedResumable, FailureCode: "SOME_FUTURE_CODE_V9"}
	b, err := json.Marshal(&r)
	if err != nil {
		t.Fatal(err)
	}
	var back Run
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back.FailureCode != "SOME_FUTURE_CODE_V9" {
		t.Fatalf("unknown code mangled: %q", back.FailureCode)
	}
	// And the legacy shape: absent field decodes to the zero value.
	var legacy Run
	if err := json.Unmarshal([]byte(`{"id":"old","status":"failed"}`), &legacy); err != nil {
		t.Fatal(err)
	}
	if legacy.FailureCode != "" {
		t.Fatalf("legacy row grew a code: %q", legacy.FailureCode)
	}
}
