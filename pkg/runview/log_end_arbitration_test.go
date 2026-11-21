package runview

import (
	"errors"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/runview/runstream"
	"github.com/SocialGouv/iterion/pkg/store"
)

func TestLogSettleAdoptsProducerRegisteredAtDecision(t *testing.T) {
	svc, native := newLogLaneService(t)
	tightenBoundLogPoll(t, time.Hour)
	seedLogLaneRun(t, native, "r", store.RunStatusFinished)
	writeNativeRunLog(t, native, "r", "existing\n")
	ready := make(chan *RunLogBuffer, 1)
	svc.liveEvents.beforeLogSettleEnd = func() {
		buf, _ := svc.prepareRunLog(t.Context(), "r")
		ready <- buf
	}
	sink := newLogSink(subscribeBoundLogs(t, svc, "r"))
	var buf *RunLogBuffer
	select {
	case buf = <-ready:
	case <-time.After(5 * time.Second):
		t.Fatal("settle never reached its final decision")
	}
	defer svc.dropRunLog("r", buf)
	if _, err := buf.Write([]byte("finalize\n")); err != nil {
		t.Fatal(err)
	}
	sink.awaitText(t, "existing\nfinalize\n", 5*time.Second)
	svc.dropRunLog("r", buf)
	text, errs := sink.awaitEnd(t, 5*time.Second)
	by, applies := svc.liveEvents.logEndBy("r")
	if by == logEndBySettle || applies != 1 || text != "existing\nfinalize\n" || len(errs) != 0 {
		t.Fatalf("end = %q (%d applications), text = %q, errors = %v; want the producer's end and all its bytes", by, applies, text, errs)
	}
}

func TestLogRetainedTerminalYieldsToNewerCaptureFailure(t *testing.T) {
	svc, native := newLogLaneService(t)
	tightenBoundLogPoll(t, time.Hour)
	seedLogLaneRun(t, native, "r", store.RunStatusFinished)
	marker := errors.New("replacement capture failed")
	for _, first := range []string{"retained", "watch"} {
		t.Run(first, func(t *testing.T) {
			buf, _ := svc.prepareRunLog(t.Context(), "r")
			svc.dropRunLog("r", buf)
			ctx := boundOriginCtx(t, svc, "r")
			sub, _, err := svc.observedLogLane(ctx, "r", false)
			if err != nil || sub == nil || !sub.Closed {
				t.Fatalf("fixture: expected retained terminal lane, got %v, %v", sub, err)
			}
			defer sub.Cancel()
			m := &liveLogMerge{l: svc.liveEvents, runID: "r", ctx: ctx, entry: &liveLogEntry{}, out: runstream.NewLogPipe()}
			m.adoptLane(sub)
			svc.recordLogProducerFailure("r", marker)
			var stop bool
			if first == "retained" {
				stop = m.handleLaneEnd(retainedEnd(sub))
			} else {
				stop = m.rearm()
			}
			m.out.Finish(nil)
			if end := <-m.out.Errors(); !stop || !errors.Is(end, marker) || m.applies != 1 {
				t.Fatalf("stop = %v, end = %v, applications = %d; want the newer capture failure exactly once", stop, end, m.applies)
			}
		})
	}
}
