package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/sandbox"
	"github.com/SocialGouv/iterion/pkg/store"
)

// Setup runs on the same ctx the node loop does, so the same two
// interruptions reach it — and mean the same thing. Marked terminal, a
// drained run cannot be resumed at all, and a review that owed a merge gate
// never gets to post one.
func TestSetupFailureStatus(t *testing.T) {
	boom := errors.New("kubectl exec: signal: killed")

	interrupted, cancelInterrupted := context.WithCancelCause(context.Background())
	cancelInterrupted(ErrRunInterrupted)
	operator, cancelOperator := context.WithCancel(context.Background())
	cancelOperator()

	for _, tc := range []struct {
		name     string
		ctx      context.Context
		want     store.RunStatus
		wantCode store.FailureCode
	}{
		{"a runner drain is resumable", interrupted, store.RunStatusFailedResumable, store.FailureInterrupted},
		{"an operator cancel says so", operator, store.RunStatusCancelled, store.FailureCancelled},
		{"a real setup error still fails", context.Background(), store.RunStatusFailed, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, msg, code := setupFailureStatus(tc.ctx, "sandbox start", boom)
			if got != tc.want {
				t.Fatalf("status = %q, want %q (msg %q)", got, tc.want, msg)
			}
			if code != tc.wantCode {
				t.Errorf("code = %q, want %q", code, tc.wantCode)
			}
			if !strings.Contains(msg, "sandbox start") || !strings.Contains(msg, boom.Error()) {
				t.Errorf("the message must keep the phase and the cause; got %q", msg)
			}
		})
	}
}

// A sandbox phase timeout is a transient infrastructure stall (a stuck
// kubectl-exec pipe, a rescheduled apiserver): the child ctx expired
// while the run ctx stayed live. It must classify to failed_resumable +
// SANDBOX_SETUP_TIMEOUT so the redelivery resumes on a healthy pod —
// not the default RunStatusFailed the live-ctx arm produces, which the
// queue then drops as a stale delivery.
func TestSetupFailureStatus_PhaseTimeoutIsResumable(t *testing.T) {
	// Wrap like runWithPhaseTimeout does: sandbox.ErrPhaseTimeout +
	// context.DeadlineExceeded + inner cause via errors.Join.
	inner := errors.New("kubectl-exec pipe stalled")
	cause := fmt.Errorf("kubernetes: workspace copy phase timed out: %w",
		errors.Join(sandbox.ErrPhaseTimeout, context.DeadlineExceeded, inner))

	got, msg, code := setupFailureStatus(context.Background(), "sandbox start", cause)
	if got != store.RunStatusFailedResumable {
		t.Fatalf("phase-timeout status = %q, want failed_resumable — the runner would ACK the delivery and hard-fail a run a peer pod would resume in seconds (E6)", got)
	}
	if code != store.FailureSandboxSetupTimeout {
		t.Fatalf("phase-timeout code = %q, want SANDBOX_SETUP_TIMEOUT — the retry lane needs the taxonomy entry", code)
	}
	if !strings.Contains(msg, "sandbox setup phase timed out") {
		t.Fatalf("message must say what happened, got %q", msg)
	}
}

// A pod that never got placed executed NOTHING: no node started, no
// command ran. Classified `failed` it is dead on arrival — an hourly
// sentinel silently loses its tick and a campaign loses its launch (#699,
// measured in production with every worker at 88–94 % CPU requested).
func TestSetupFailureStatus_CapacityIsResumable(t *testing.T) {
	cause := fmt.Errorf("kubernetes: wait for pod ready: %w",
		errors.Join(sandbox.ErrCapacity, errors.New("0/12 nodes are available: 11 Insufficient cpu")))

	got, msg, code := setupFailureStatus(context.Background(), "sandbox start", cause)
	if got != store.RunStatusFailedResumable {
		t.Fatalf("capacity status = %q, want failed_resumable — nothing retries a terminal failure, and the pod did no work", got)
	}
	if code != store.FailureSandboxCapacity {
		t.Fatalf("capacity code = %q, want SANDBOX_CAPACITY — the retry lane and the gate notice cannot tell it from a generic failure", code)
	}
	if !strings.Contains(msg, "Insufficient cpu") {
		t.Fatalf("the cluster's reason must reach the run record, got %q", msg)
	}
}

// The status and the error have to agree: the runner classifies the
// sentinel itself, so a capacity failure that lost it would be ACKed and
// dropped by the queue while the run record claims it is resumable.
func TestSetupErr_CapacityKeepsItsOwnSentinel(t *testing.T) {
	capacity := fmt.Errorf("runtime: sandbox: %w",
		errors.Join(sandbox.ErrCapacity, errors.New("unschedulable")))
	err := setupErrFrom(store.RunStatusFailedResumable, store.FailureSandboxCapacity, capacity)
	if !errors.Is(err, sandbox.ErrCapacity) {
		t.Fatalf("capacity identity lost after setupErrFrom; got %v", err)
	}
	if errors.Is(err, ErrRunInterrupted) {
		t.Fatalf("capacity was dressed up as an interruption: %v — the runner would exempt it from the DLQ park", err)
	}
}

// setupErrFrom hands the phase timeout to the runner under its OWN
// sentinel: the runner's ack policy classifies sandbox.ErrPhaseTimeout
// itself. It must not be dressed up as ErrRunInterrupted — an interruption
// is exempt from the DLQ park, so a stall repeating through every
// permitted delivery would nak into nothing instead of parking and being
// announced.
func TestSetupErr_PhaseTimeoutKeepsItsOwnSentinel(t *testing.T) {
	inner := errors.New("stalled")
	phaseTimeout := fmt.Errorf("kubernetes: workspace copy phase timed out: %w",
		errors.Join(sandbox.ErrPhaseTimeout, context.DeadlineExceeded, inner))
	err := setupErrFrom(store.RunStatusFailedResumable, store.FailureSandboxSetupTimeout, phaseTimeout)
	if !errors.Is(err, sandbox.ErrPhaseTimeout) {
		t.Fatalf("phase-timeout identity lost after setupErrFrom; got %v", err)
	}
	if errors.Is(err, ErrRunInterrupted) {
		t.Fatalf("phase-timeout was dressed up as an interruption: %v — the runner would exempt it from the DLQ park", err)
	}
}

// The status and the error have to agree: the runner NAKs on
// ErrRunInterrupted (redelivered to a healthy pod) and ACKs otherwise, so a
// run recorded resumable whose error lost the marker would be dropped by the
// queue and never come back.
func TestSetupErr_KeepsTheInterruptionMarker(t *testing.T) {
	inner := errors.New("worktree half-written")

	if err := setupErrFrom(store.RunStatusFailedResumable, store.FailureInterrupted, inner); !errors.Is(err, ErrRunInterrupted) {
		t.Fatalf("a drained setup must stay recognisable to the runner, got %v", err)
	}
	if err := setupErrFrom(store.RunStatusFailed, "", inner); errors.Is(err, ErrRunInterrupted) {
		t.Fatalf("an ordinary setup error must not claim an interruption: %v", err)
	}
}

// The composition: the engine driven with a drained ctx must leave the run
// resumable in the STORE and return an error the runner will nak.
func TestEngineRun_SetupDrainLeavesTheRunResumable(t *testing.T) {
	s := tmpStore(t)
	eng := New(devboxTestWorkflow(), s, newStubExecutor(),
		WithWorkDir(t.TempDir()),
		WithSandboxOverride("none"),
		WithLogger(iterlog.Nop()),
	)

	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(ErrRunInterrupted)

	const runID = "run-drained-in-setup"
	err := eng.Run(ctx, runID, nil)
	if err == nil {
		t.Fatal("a drained run must not report success")
	}
	if !errors.Is(err, ErrRunInterrupted) {
		t.Fatalf("the runner naks on ErrRunInterrupted; got %v", err)
	}
	run, lerr := s.LoadRun(context.Background(), runID)
	if lerr != nil {
		t.Fatalf("load run: %v", lerr)
	}
	if run.Status != store.RunStatusFailedResumable {
		t.Fatalf("status = %q, want failed_resumable (a drain is not a workflow failure)", run.Status)
	}
}

// #2862 — one decision for the recorded status and the returned error.

// Scenario A: fetchFailure typed the unfetchable error under a LIVE ctx,
// then the run's deadline fired before the record was written. The record
// reads terminal failed; the typed re-offer must not ride the returned
// error — the runner would re-offer a finished run after its two-minute
// delay. The message survives the strip.
func TestSetupErrFrom_TerminalRecordDropsTheReoffer(t *testing.T) {
	deadlined, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	cause := attachmentsUnfetchable("run-x", "fetch attachment spec", errors.New("the bucket is unreachable"))

	st, _, code := setupFailureStatus(deadlined, "sandbox start", cause)
	if st != store.RunStatusFailed || code != store.FailureAttachmentsUnfetchable {
		t.Fatalf("decision = %s / %s, want failed / %s", st, code, store.FailureAttachmentsUnfetchable)
	}
	mapped := setupErrFrom(st, code, fmt.Errorf("runtime: sandbox: %w", cause))
	var rt *RuntimeError
	if errors.As(mapped, &rt) {
		t.Fatalf("the typed re-offer %s rides a terminal record: the runner would re-offer a finished run", rt.Code)
	}
	if !strings.Contains(mapped.Error(), "the bucket is unreachable") {
		t.Fatalf("the cause's message is lost: %v", mapped)
	}
	// A terminal decision over a plain error changes nothing.
	plain := errors.New("worktree half-written")
	if got := setupErrFrom(store.RunStatusFailed, "", plain); !errors.Is(got, plain) {
		t.Fatalf("a plain terminal error must pass through unchanged, got %v", got)
	}
}

// The decision markFailedBestEffort returns is the one it recorded: the
// caller maps THAT to the returned error, so the pair cannot drift.
func TestMarkFailedBestEffort_ReturnsTheRecordedDecision(t *testing.T) {
	s := tmpStore(t)
	ctx := context.Background()
	const runID = "run-one-decision"
	if _, err := s.CreateRun(ctx, runID, "wf", nil); err != nil {
		t.Fatal(err)
	}
	e := New(devboxTestWorkflow(), s, newStubExecutor(), WithLogger(iterlog.Nop()))
	st, code := e.markFailedBestEffort(ctx, runID, "sandbox start", capacityStartError())
	if st != store.RunStatusFailedResumable || code != store.FailureSandboxCapacity {
		t.Fatalf("decision = %s / %s, want failed_resumable / %s", st, code, store.FailureSandboxCapacity)
	}
	run, err := s.LoadRun(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != st || run.FailureCode != code {
		t.Fatalf("the record reads %s / %s, the decision returned %s / %s", run.Status, run.FailureCode, st, code)
	}
	if err := setupErrFrom(st, code, capacityStartError()); !errors.Is(err, sandbox.ErrCapacity) {
		t.Fatalf("the sentinel the runner re-offers under is lost: %v", err)
	}
}

// Scenario B: the operator's cancel lands between the decision's read and
// the write — in cloud the publisher has already CASed the doc to
// `cancelled` with the operator's own reason. The resumable write is a
// compare-and-set from `running`, so it declines; the returned decision is
// the DOCUMENT's, and the returned error reads as the cancel, not as the
// re-offer the losing read had decided.
func TestMarkFailedBestEffort_ResumableWriteLosesToAPublishersCancel(t *testing.T) {
	s := tmpStore(t)
	ctx := context.Background()
	const runID = "run-cancelled-mid-write"
	if _, err := s.CreateRun(ctx, runID, "wf", nil); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateRunStatusCoded(ctx, runID, store.RunStatusCancelled, "operator: stop it", store.FailureCancelled); err != nil {
		t.Fatal(err)
	}
	e := New(devboxTestWorkflow(), s, newStubExecutor(), WithLogger(iterlog.Nop()))
	// The launch-side read happened under a live ctx: failed_resumable +
	// SANDBOX_CAPACITY was this attempt's decision — and it lost.
	st, code := e.markFailedBestEffort(ctx, runID, "sandbox start", capacityStartError())
	if st != store.RunStatusCancelled || code != store.FailureCancelled {
		t.Fatalf("decision = %s / %s, want the document's cancelled", st, code)
	}
	run, err := s.LoadRun(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != store.RunStatusCancelled || run.Error != "operator: stop it" {
		t.Fatalf("the publisher's record was clobbered: %s / %q", run.Status, run.Error)
	}
	mapped := setupErrFrom(st, code, capacityStartError())
	if !errors.Is(mapped, ErrRunCancelled) || errors.Is(mapped, sandbox.ErrCapacity) {
		t.Fatalf("the returned error = %v, want the cancel alone", mapped)
	}
}
