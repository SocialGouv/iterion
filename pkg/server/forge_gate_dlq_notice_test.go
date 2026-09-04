package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/forge"
	"github.com/SocialGouv/iterion/pkg/store"
)

// dlqGateFixture: a gating run parked on the DLQ, a stub forge that records
// both the commit statuses and the PR comments.
func dlqGateFixture(t *testing.T, retry *store.RunRetryState) (*Server, string, *listingGateClient, *stubCommenter) {
	t.Helper()
	gc := &listingGateClient{fakeGateClient: fakeGateClient{headSHA: "deadbeef"}}
	s, runID := gateReconcileFixture(t, gatingInputs(), gc)
	c := &stubCommenter{}
	s.forgeIssueCommenterFor = func(context.Context, forge.Connection) (forgeIssueCommenter, error) {
		return c, nil
	}
	run, err := s.cfg.Store.LoadRun(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	run.Status = store.RunStatusFailedResumable
	run.FailureCode = store.FailureDLQParked
	run.ContinuationState = store.ContinuationFinal
	run.Error = "max deliveries exhausted: runtime: cannot resume run x with status \"running\" (parked on DLQ — replay via /api/admin/dlq)"
	run.RetryState = retry
	if err := s.cfg.Store.SaveRun(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	return s, runID, gc, c
}

func assertDLQNotice(t *testing.T, bodies []string) {
	t.Helper()
	if len(bodies) != 1 {
		t.Fatalf("want exactly one DLQ notice, got %d: %v", len(bodies), bodies)
	}
	body := bodies[0]
	if !strings.Contains(body, gateDLQNoticeMarker) {
		t.Fatalf("notice lacks the DLQ marker:\n%s", body)
	}
	if strings.Contains(body, "quota") || strings.Contains(body, "resume it **automatically") {
		t.Fatalf("DLQ park announced as a quota pause — the #669 lie:\n%s", body)
	}
	for _, want := range []string{"dead-letter queue", "iterion remote admin dlq", "marked **failed**", "relaunches the bot **once**"} {
		if !strings.Contains(body, want) {
			t.Fatalf("DLQ notice missing %q:\n%s", want, body)
		}
	}
}

// A DLQ park is final for automation whatever retry_after still says on
// the doc: the reconciler must NOT stand down on it, and the PR must be
// told the truth — the check is marked failed, a relaunch may follow, an
// operator replays the message — not "resumes automatically at HH:MM".
func TestGateReconcile_DLQParkDoesNotStandDownOnALeftoverRetry(t *testing.T) {
	at := time.Now().UTC().Add(4 * 24 * time.Hour)
	s, runID, gc, c := dlqGateFixture(t, &store.RunRetryState{RetryAfter: &at, Reason: "usage_window", Attempts: 1})

	if err := s.reconcileGateForRun(context.Background(), terminalEvent(runID)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if gc.setCalls != 1 {
		t.Fatalf("posted %d statuses, want 1 — a DLQ-parked run is dead for automation, the leftover retry_after wakes nothing", gc.setCalls)
	}
	if gc.last.State != forge.CommitStateFailure || !isSyntheticGateInterruption(gc.last.Description) {
		t.Fatalf("status = %q %q, want the synthetic failure", gc.last.State, gc.last.Description)
	}
	assertDLQNotice(t, c.bodies)

	// The sweep re-offers the same run every minute: no second comment.
	_ = s.reconcileGateForRunID(context.Background(), runID, gateTriggerSweep)
	if len(c.bodies) != 1 {
		t.Fatalf("the sweep re-posted the DLQ notice (%d comments) — one park, one notice", len(c.bodies))
	}
}

// The notice is gated on the FailureCode alone. An operator resume clears
// retry_after before the park that follows, so the #669 shape carries NO
// retry state at all — and the wording that used to key on it never posted.
func TestGateReconcile_DLQNoticePostsWithoutRetryState(t *testing.T) {
	s, runID, gc, c := dlqGateFixture(t, nil)

	if err := s.reconcileGateForRun(context.Background(), terminalEvent(runID)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	assertDLQNotice(t, c.bodies)
	if gc.setCalls != 1 {
		t.Fatalf("posted %d statuses, want 1", gc.setCalls)
	}
}

// The admission-mismatch park (a rollout's epoch/schema fence on the last
// delivery) flips the doc through FailQueuedRunIfAttempt. Driven through
// the real store twin: the code it stamps is what the notice keys on, so
// the composition — park, then reconcile — must produce the DLQ wording,
// not silence or the quota text.
func TestGateReconcile_AdmissionParkReachesTheDLQNotice(t *testing.T) {
	gc := &listingGateClient{fakeGateClient: fakeGateClient{headSHA: "deadbeef"}}
	s, runID := gateReconcileFixture(t, gatingInputs(), gc)
	c := &stubCommenter{}
	s.forgeIssueCommenterFor = func(context.Context, forge.Connection) (forgeIssueCommenter, error) {
		return c, nil
	}
	ctx := context.Background()
	// The publisher's pre-flip: the run is queued for its attempt.
	if err := s.cfg.Store.UpdateRunStatus(ctx, runID, store.RunStatusFailedResumable, "usage window shut"); err != nil {
		t.Fatal(err)
	}
	if changed, err := s.cfg.Store.UpdateRunStatusIf(ctx, runID, store.RunStatusQueued, "", []store.RunStatus{store.RunStatusFailedResumable}); err != nil || !changed {
		t.Fatalf("queue the attempt: (%v, %v)", changed, err)
	}
	queued, err := s.cfg.Store.LoadRun(ctx, runID)
	if err != nil || queued.QueuedAt == nil {
		t.Fatalf("queued marker missing: %+v %v", queued, err)
	}
	attempts := store.AsQueuedAttemptStore(s.cfg.Store)
	if attempts == nil {
		t.Fatal("the test store must implement QueuedAttemptStore")
	}
	changed, err := attempts.FailQueuedRunIfAttempt(ctx, runID,
		"schema version mismatch: v9 unsupported (queue message v9 parked on DLQ — replay via /api/admin/dlq only once the runner fleet speaks schema v9)",
		queued.QueuedAt.Add(time.Second), store.RunOutcomeMeta{Code: store.FailureDLQParked, Continuation: store.ContinuationFinal})
	if err != nil || !changed {
		t.Fatalf("admission park CAS = (%v, %v), want (true, nil)", changed, err)
	}

	if err := s.reconcileGateForRun(ctx, terminalEvent(runID)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	assertDLQNotice(t, c.bodies)
	if gc.setCalls != 1 {
		t.Fatalf("posted %d statuses, want 1 — the parked review owes the PR a verdict", gc.setCalls)
	}
}

// A run that gates nothing gets no DLQ notice, however it was parked.
func TestNoticeGateDLQParked_SilentForANonGatingRun(t *testing.T) {
	gc := &listingGateClient{fakeGateClient: fakeGateClient{headSHA: "deadbeef"}}
	s, runID := gateReconcileFixture(t, map[string]any{"pr_url": "https://github.com/o/r/pull/42", forgePublishVarToken: "tok-gate"}, gc)
	c := &stubCommenter{}
	s.forgeIssueCommenterFor = func(context.Context, forge.Connection) (forgeIssueCommenter, error) {
		return c, nil
	}
	run, _ := s.cfg.Store.LoadRun(context.Background(), runID)
	run.Status = store.RunStatusFailedResumable
	run.FailureCode = store.FailureDLQParked
	_ = s.cfg.Store.SaveRun(context.Background(), run)

	s.noticeGateDLQParked(context.Background(), run)

	if len(c.bodies) != 0 {
		t.Fatalf("a run owing no verdict posted a DLQ notice: %v", c.bodies)
	}
}
