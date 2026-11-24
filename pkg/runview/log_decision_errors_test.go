package runview

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/runview/runstream"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The file selection continues to use the native store. Gate only the merge's
// bound metadata recheck, returning the result that a native read can return.
type gatedLogOriginStore struct {
	*store.FilesystemRunStore
	entered chan struct{}
	once    sync.Once
}

func (s *gatedLogOriginStore) LoadRun(ctx context.Context, id string) (*store.Run, error) {
	waited := false
	if _, bound := store.RunDataBindingFrom(ctx); bound {
		s.once.Do(func() { close(s.entered); waited = true })
	}
	if waited {
		<-ctx.Done()
	}
	return s.FilesystemRunStore.LoadRun(ctx, id)
}

func TestLogStopDuringRetainedProducerFailureCheck(t *testing.T) {
	svc, native := newLogLaneService(t)
	seedLogLaneRun(t, native, "r", store.RunStatusRunning)
	writeNativeRunLog(t, native, "r", "before\n")
	bound := boundOriginCtx(t, svc, "r")
	tightenBoundLogPoll(t, time.Hour)
	// The first load through the wrapper belongs to the merge's initial status
	// poll. Arm the late failure at its entry, where its watch cannot win first.
	gated := &gatedLogOriginStore{FilesystemRunStore: native, entered: make(chan struct{})}
	svc.store = gated
	svc.liveEvents.beforeLogStatusPoll = func() { svc.recordLogProducerFailure("r", store.ErrRunDataIdentityChanged) }
	out := runstream.NewLogPipe()
	if err := svc.liveEvents.subscribeLogs(bound, "r", 0, out); err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	sink := newLogSink(out)
	stopCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	select {
	case <-gated.entered:
	case <-stopCtx.Done():
		t.Fatal("merge did not enter bound origin check")
	}
	if err := svc.liveEvents.stop(stopCtx); err != nil {
		t.Fatal(err)
	}
	_, errs := sink.awaitEnd(t, time.Second)
	by, _ := svc.liveEvents.logEndBy("r")
	if len(errs) != 1 || !errors.Is(errs[0], runstream.ErrSourceClosed) || by != logEndByStop {
		t.Fatalf("end=%q errors=%v; want only ErrSourceClosed from stop", by, errs)
	}
}

func TestLogStopDuringDirectLaneFailureCheck(t *testing.T) {
	svc, native := newLogLaneService(t)
	seedLogLaneRun(t, native, "r", store.RunStatusRunning)
	writeNativeRunLog(t, native, "r", "before\n")
	bound := boundOriginCtx(t, svc, "r")
	tightenBoundLogPoll(t, time.Hour)
	buf, _ := svc.prepareRunLog(t.Context(), "r")
	defer svc.dropRunLog("r", buf)
	gated := &gatedLogOriginStore{FilesystemRunStore: native, entered: make(chan struct{})}
	svc.store = gated
	out := runstream.NewLogPipe()
	if err := svc.liveEvents.subscribeLogs(bound, "r", 0, out); err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	sink := newLogSink(out)
	svc.recordLogProducerFailure("r", store.ErrRunDataIdentityChanged)
	stopCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	select {
	case <-gated.entered:
	case <-stopCtx.Done():
		t.Fatal("merge did not enter direct lane origin check")
	}
	if err := svc.liveEvents.stop(stopCtx); err != nil {
		t.Fatal(err)
	}
	_, errs := sink.awaitEnd(t, time.Second)
	by, _ := svc.liveEvents.logEndBy("r")
	if len(errs) != 1 || !errors.Is(errs[0], runstream.ErrSourceClosed) || by != logEndByStop {
		t.Fatalf("end=%q errors=%v; want only ErrSourceClosed from stop", by, errs)
	}
}

type transientLogOriginStore struct {
	*store.FilesystemRunStore
	once      atomic.Bool
	transient error
}

func (s *transientLogOriginStore) LoadRun(ctx context.Context, id string) (*store.Run, error) {
	if s.once.CompareAndSwap(false, true) {
		return nil, s.transient
	}
	return s.FilesystemRunStore.LoadRun(ctx, id)
}

func TestLogStatusReentryRetriesTransientOriginCheck(t *testing.T) {
	svc, native := newLogLaneService(t)
	if err := seedOriginRun(t.Context(), native, "r", "before", store.LegacyValueFormat, store.RunStatusRunning, 0); err != nil {
		t.Fatal(err)
	}
	stale, err := svc.CaptureRunOrigin(t.Context(), "r")
	if err != nil {
		t.Fatal(err)
	}
	if err := replaceOriginRun(t.Context(), native, "r", "after", store.LegacyValueFormat, store.RunStatusRunning, 0); err != nil {
		t.Fatal(err)
	}
	bound := boundOriginCtx(t, svc, "r")
	writeNativeRunLog(t, native, "r", "before\n")
	tightenBoundLogPoll(t, time.Hour)
	transient := errors.New("temporary metadata I/O unavailable")
	svc.store = &transientLogOriginStore{FilesystemRunStore: native, transient: transient}
	svc.liveEvents.beforeLogStatusPoll = func() {
		svc.logRegistry.acquire("r", stale, logDurable)
	}
	out := runstream.NewLogPipe()
	if err := svc.liveEvents.subscribeLogs(bound, "r", 0, out); err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	sink := newLogSink(out)
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for sink.textSoFar() != "before\n" {
		select {
		case <-sink.done:
			t.Fatalf("one transient read closed stream; errors=%v", sink.errorsSoFar())
		case <-deadline.C:
			t.Fatalf("no data shipped; errors=%v", sink.errorsSoFar())
		case <-tick.C:
		}
	}
	sink.assertOpen(t, 10*time.Millisecond)
}

func TestLogCaptureFailureSurvivesFailedProducerTeardown(t *testing.T) {
	svc, native := newLogLaneService(t)
	seedLogLaneRun(t, native, "r", store.RunStatusRunning)
	seedLogLaneRun(t, native, "other", store.RunStatusRunning)
	bound := boundOriginCtx(t, svc, "r")
	foreign := boundOriginCtx(t, svc, "other")
	old, _ := NewRunLogBuffer("")
	lane := svc.registerRunLogOrigin(t.Context(), "r", old, logDurable)
	if lane == nil {
		t.Fatal("missing predecessor lane")
	}
	firstCause := errors.New("predecessor tee failed")
	svc.recordLogTeeFailure(lane, firstCause)
	svc.dropRunLog("r", old)
	next, _ := NewRunLogBuffer("")
	if lane := svc.registerRunLogOrigin(foreign, "r", next, logDurable); lane != nil {
		t.Fatal("unexpected capture success")
	}
	if _, _, err := svc.observedLogLane(bound, "r", false); err == nil || !errors.Is(err, store.ErrRunDataCapability) {
		t.Fatalf("before drop: got %v, want newer capture failure", err)
	}
	svc.dropRunLog("r", next)
	out, err := svc.StreamSource().SubscribeLogs(bound, "r", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	_, errs := newLogSink(out).awaitEnd(t, 3*time.Second)
	if len(errs) != 1 || !errors.Is(errs[0], store.ErrRunDataCapability) || errors.Is(errs[0], firstCause) {
		t.Fatalf("after drop: errors=%v; want newer capture failure, not predecessor's cause", errs)
	}
}
