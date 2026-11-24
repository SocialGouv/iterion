package runview

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/runview/runstream"
	"github.com/SocialGouv/iterion/pkg/store"
)

// A retained epoch belongs to the generation BEFORE the one starting now: the
// failure of the new producer must not be swallowed by it, and a subscriber
// must see that failure rather than the predecessor's retained end.
func TestRetainedEpochDoesNotSwallowANewerProducerFailure(t *testing.T) {
	svc, native := newLogLaneService(t)
	ctx := t.Context()
	seedLogLaneRun(t, native, "r", store.RunStatusRunning)
	bound := boundOriginCtx(t, svc, "r")

	first, _ := svc.prepareRunLog(ctx, "r")
	svc.dropRunLog("r", first) // the epoch is closed, retained, and served next

	marker := fmt.Errorf("the producer of the new generation failed")
	svc.recordLogProducerFailure("r", marker)

	if got := svc.localLogProducerFailure("r"); got == nil {
		t.Fatal("the failure of the new generation was swallowed by the retained epoch")
	}
	sub, _, err := svc.observedLogLane(bound, "r", false)
	if err == nil && sub != nil {
		sub.Cancel()
		t.Fatal("the subscriber adopted the retained end of the predecessor instead of the failure")
	}
	if err != nil && !errors.Is(err, marker) {
		t.Fatalf("the adoption refused with %v, want the producer's failure", err)
	}
}

// A Service stop is a typed end for the consumer, never a clean completion the
// client cannot tell from a finished run.
func TestReArmPublishesTheStopCause(t *testing.T) {
	svc, native := newLogLaneService(t)
	seedLogLaneRun(t, native, "r", store.RunStatusRunning)
	tightenLogLaneWait(t, 20*time.Millisecond)

	pipe := runstream.NewLogPipe()
	ctx, cancel := context.WithCancelCause(t.Context())
	cancel(runstream.ErrSourceClosed)
	m := &liveLogMerge{l: svc.liveEvents, ctx: ctx, runID: "r", out: pipe, entry: &liveLogEntry{}}
	if !m.rearm() {
		t.Fatal("rearm kept the merge alive on a cancelled context")
	}
	go pipe.Finish(nil)
	var got []error
	for err := range pipe.Errors() {
		got = append(got, err)
	}
	for _, err := range got {
		if errors.Is(err, runstream.ErrSourceClosed) {
			return
		}
	}
	t.Fatalf("the merge ended without a cause: errors = %v, want ErrSourceClosed", got)
}

// E3 is a promise about a run nobody produces here, not a verdict frozen when
// its window opened: a run resumed inside the window keeps its stream.
func TestSettleReReadsTheRunBeforeEnding(t *testing.T) {
	prevSettle, prevEvery := logTerminalSettle, logTerminalSettleEvery
	logTerminalSettle, logTerminalSettleEvery = 250*time.Millisecond, 10*time.Millisecond
	t.Cleanup(func() { logTerminalSettle, logTerminalSettleEvery = prevSettle, prevEvery })
	tightenBoundLogPoll(t, 20*time.Millisecond)
	tightenLogLaneWait(t, 20*time.Millisecond)

	svc, native := newLogLaneService(t)
	ctx := t.Context()
	seedLogLaneRun(t, native, "r", store.RunStatusFinished)
	writeNativeRunLog(t, native, "r", "before\n")

	sub := subscribeBoundLogs(t, svc, "r")
	sink := newLogSink(sub)
	sink.awaitText(t, "before\n", 2*time.Second)

	// The run resumes inside the settle window, and its producer writes again.
	if err := native.UpdateRunStatus(ctx, "r", store.RunStatusRunning, ""); err != nil {
		t.Fatalf("UpdateRunStatus: %v", err)
	}
	writeNativeRunLog(t, native, "r", "before\nafter\n")

	sink.awaitText(t, "before\nafter\n", 2*time.Second)
	sink.assertOpen(t, 300*time.Millisecond)
}

// A retained PAUSE is not the truth of a run the status re-reads as terminal:
// the settle must conclude instead of parking on an epoch whose producer ended
// on another replica (round 2, F1).
func TestSettleEndsPastAStaleRetainedPause(t *testing.T) {
	prevSettle, prevEvery := logTerminalSettle, logTerminalSettleEvery
	logTerminalSettle, logTerminalSettleEvery = 120*time.Millisecond, 10*time.Millisecond
	t.Cleanup(func() { logTerminalSettle, logTerminalSettleEvery = prevSettle, prevEvery })
	tightenBoundLogPoll(t, 20*time.Millisecond)
	tightenLogLaneWait(t, 20*time.Millisecond)

	svc, native := newLogLaneService(t)
	ctx := t.Context()
	seedLogLaneRun(t, native, "r", store.RunStatusRunning)
	// A producer of THIS process paused, leaving its epoch retained.
	buf, _ := NewRunLogBuffer("")
	lane := svc.registerRunLogOrigin(ctx, "r", buf, logDurable)
	if lane == nil {
		t.Fatal("no lane was registered")
	}
	lane.Close(logEndPaused)
	// Elsewhere the run was resumed and finished: the status is terminal.
	if err := native.UpdateRunStatus(ctx, "r", store.RunStatusFinished, ""); err != nil {
		t.Fatalf("UpdateRunStatus: %v", err)
	}

	sub := subscribeBoundLogs(t, svc, "r")
	sink := newLogSink(sub)
	if _, errs := sink.awaitEnd(t, 3*time.Second); len(errs) != 0 {
		t.Fatalf("the settle published errors: %v", errs)
	}
}

// An end already applied is final: a failure recorded while the stream drains
// afterwards adds neither a second end nor an error on a finished run
// (round 2, F2).
func TestReArmAddsNothingAfterAnAppliedEnd(t *testing.T) {
	svc, native := newLogLaneService(t)
	seedLogLaneRun(t, native, "r", store.RunStatusRunning)
	pipe := runstream.NewLogPipe()
	m := &liveLogMerge{l: svc.liveEvents, ctx: t.Context(), runID: "r", out: pipe, entry: &liveLogEntry{}, endAt: time.Now()}
	svc.recordLogProducerFailure("r", fmt.Errorf("a later producer failed: %w", store.ErrRunDataCapability))
	if m.rearm() {
		t.Fatal("rearm acted after the stream's end was applied")
	}
	go pipe.Finish(nil)
	for err := range pipe.Errors() {
		t.Fatalf("a finished stream published an error: %v", err)
	}
	if m.applies != 0 {
		t.Fatalf("applies = %d, want none after the end was applied", m.applies)
	}
}

// First-end-wins holds across sources and epochs: once an end is applied, the
// settle's epoch-0 application is a no-op (round 3, F3-1).
func TestApplyEndIsIdempotentOnceAnEndIsApplied(t *testing.T) {
	svc, native := newLogLaneService(t)
	seedLogLaneRun(t, native, "r", store.RunStatusFinished)
	m := &liveLogMerge{
		l: svc.liveEvents, ctx: t.Context(), runID: "r", out: runstream.NewLogPipe(),
		entry: &liveLogEntry{}, finalHint: make(chan struct{}, 1), dataWake: make(chan struct{}, 1),
	}
	if m.applyEnd(7, logEndTerminal, logEndByLane) {
		t.Fatal("a terminal end stopped the merge before its last drain")
	}
	if m.applyEnd(0, logEndTerminal, logEndBySettle) {
		t.Fatal("the settle stopped a stream whose end was already applied")
	}
	if m.applies != 1 || m.entry.endBy != logEndByLane {
		t.Fatalf("applies = %d endBy = %q, want exactly the lane's single application", m.applies, m.entry.endBy)
	}
}
