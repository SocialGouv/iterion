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
