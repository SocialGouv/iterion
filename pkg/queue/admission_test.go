package queue

import (
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

// TestAdmit_eachRunStateAndMessageShape pins the admission rule a runner and
// the DLQ replay share: what each run state does to a launch and to a resume
// of its current attempt.
func TestAdmit_eachRunStateAndMessageShape(t *testing.T) {
	queuedAt := time.Date(2026, 10, 1, 6, 0, 0, 0, time.UTC)
	current := queuedAt.Add(time.Millisecond).Format(time.RFC3339Nano)
	for _, tc := range []struct {
		name   string
		run    store.Run
		launch Admission
		resume Admission
	}{
		{"finished", store.Run{Status: store.RunStatusFinished}, Admission{Drop: DropSettled}, Admission{Drop: DropSettled}},
		{"failed", store.Run{Status: store.RunStatusFailed}, Admission{Drop: DropSettled}, Admission{Drop: DropSettled}},
		{"paused_waiting_human", store.Run{Status: store.RunStatusPausedWaitingHuman}, Admission{Drop: DropSettled}, Admission{Drop: DropSettled}},
		{"cancelled", store.Run{Status: store.RunStatusCancelled}, Admission{Drop: DropCancelled}, Admission{Drop: DropCancelled}},
		{"failed_resumable DLQ_PARKED", store.Run{Status: store.RunStatusFailedResumable, FailureCode: store.FailureDLQParked}, Admission{Drop: DropDeliberateFailure}, Admission{Drop: DropDeliberateFailure}},
		{"failed_resumable bot code", store.Run{Status: store.RunStatusFailedResumable, FailureCode: "LOT_NOT_ACTIONABLE"}, Admission{Drop: DropDeliberateFailure}, Admission{Drop: DropDeliberateFailure}},
		{"paused_operator deterministic", store.Run{Status: store.RunStatusPausedOperator, FailureCode: store.FailureDLQParked}, Admission{Drop: DropDeliberateFailure}, Admission{Drop: DropDeliberateFailure}},
		{"failed_resumable infrastructure", store.Run{Status: store.RunStatusFailedResumable, FailureCode: store.FailureProcessOrphaned}, Admission{AsResume: true}, Admission{}},
		{"failed_resumable unknown", store.Run{Status: store.RunStatusFailedResumable}, Admission{AsResume: true}, Admission{}},
		{"paused_operator", store.Run{Status: store.RunStatusPausedOperator}, Admission{AsResume: true}, Admission{}},
		{"rewound for an explicit resume", store.Run{Status: store.RunStatusPausedOperator, ResumeRequiresExplicit: true}, Admission{Drop: DropExplicitResumeRequired}, Admission{}},
		{"queued with a checkpoint", store.Run{Status: store.RunStatusQueued, Checkpoint: &store.Checkpoint{NodeID: "a"}}, Admission{AsResume: true}, Admission{}},
		{"queued first attempt", store.Run{Status: store.RunStatusQueued}, Admission{}, Admission{}},
		{"running", store.Run{Status: store.RunStatusRunning}, Admission{}, Admission{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q := queuedAt
			run := tc.run
			run.ID, run.QueuedAt = "r", &q
			if got := Admit(&RunMessage{RunID: "r", PublishedAtRFC: current}, &run); got != tc.launch {
				t.Errorf("launch: %+v, want %+v", got, tc.launch)
			}
			if got := Admit(&RunMessage{RunID: "r", PublishedAtRFC: current, Resume: &ResumeSpec{}}, &run); got != tc.resume {
				t.Errorf("resume: %+v, want %+v", got, tc.resume)
			}
			// A message published before the run was last queued is dropped
			// as superseded, whatever the state.
			stale := &RunMessage{RunID: "r", PublishedAtRFC: queuedAt.Add(-time.Millisecond).Format(time.RFC3339Nano)}
			if got := Admit(stale, &run); got.Drop != DropSupersededAttempt {
				t.Errorf("a message published before the run was queued: %+v, want superseded", got)
			}
		})
	}
}

// TestSuperseded_identityOfAnAttempt: only a run queued AFTER the
// publication supersedes it; equal instants, a missing marker or an
// unreadable publication time tell nothing.
func TestSuperseded_identityOfAnAttempt(t *testing.T) {
	base := time.Date(2026, 10, 1, 6, 0, 0, 123456789, time.UTC)
	for _, tc := range []struct {
		name      string
		queuedAt  *time.Time
		published string
		want      bool
	}{
		{"equal", &base, base.Format(time.RFC3339Nano), false},
		{"queued 1ns after", ptr(base.Add(time.Nanosecond)), base.Format(time.RFC3339Nano), true},
		{"queued 1ns before", ptr(base.Add(-time.Nanosecond)), base.Format(time.RFC3339Nano), false},
		{"no marker", nil, base.Format(time.RFC3339Nano), false},
		{"unparsable publication", &base, "yesterday", false},
	} {
		run := &store.Run{ID: "r", QueuedAt: tc.queuedAt}
		if got := Superseded(&RunMessage{RunID: "r", PublishedAtRFC: tc.published}, run); got != tc.want {
			t.Errorf("%s: superseded = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func ptr(t time.Time) *time.Time { return &t }
