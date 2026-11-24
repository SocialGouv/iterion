package runview

import (
	"context"
	"errors"
	"github.com/SocialGouv/iterion/pkg/runview/runstream"
	"github.com/SocialGouv/iterion/pkg/store"
	"sync/atomic"
	"testing"
	"time"
)

func TestLogTransientLaneEndPreservesTypedCause(t *testing.T) {
	for _, name := range []string{"E1", "E2"} {
		t.Run(name, func(t *testing.T) {
			svc, native := newLogLaneService(t)
			seedLogLaneRun(t, native, "r", store.RunStatusRunning)
			writeNativeRunLog(t, native, "r", "before\n")
			bound := boundOriginCtx(t, svc, "r")
			tightenBoundLogPoll(t, 15*time.Millisecond)
			buf, _ := svc.prepareRunLog(t.Context(), "r")
			defer svc.dropRunLog("r", buf)
			transient := errors.New("one-shot origin read error")
			svc.store = &transientLogOriginStore{FilesystemRunStore: native, transient: transient}
			if name == "E2" {
				svc.recordLogProducerFailure("r", store.ErrRunDataIdentityChanged)
			}
			out := runstream.NewLogPipe()
			if err := svc.liveEvents.subscribeLogs(bound, "r", 0, out); err != nil {
				t.Fatal(err)
			}
			defer out.Close()
			sink := newLogSink(out)
			if name == "E1" {
				sink.awaitText(t, "before\n", time.Second)
				svc.recordLogProducerFailure("r", store.ErrRunDataIdentityChanged)
			}
			_, errs := sink.awaitEnd(t, time.Second)
			by, applies := svc.liveEvents.logEndBy("r")
			capability := false
			for _, err := range errs {
				capability = capability || errors.Is(err, store.ErrRunDataCapability)
			}
			if !capability || applies != 1 {
				t.Fatalf("%s ended via %q (%d applies) with %v; want typed producer refusal after retry", name, by, applies, errs)
			}
		})
	}
}
func TestLogInitialCaptureRefusalDecisionRetriesTransientOriginRead(t *testing.T) {
	svc, native := newLogLaneService(t)
	seedLogLaneRun(t, native, "r", store.RunStatusRunning)
	writeNativeRunLog(t, native, "r", "before\n")
	bound := boundOriginCtx(t, svc, "r")
	tightenBoundLogPoll(t, 15*time.Millisecond)
	transient := errors.New("one-shot origin read error")
	svc.store = &countingLogOriginStore{FilesystemRunStore: native, fn: func(ctx context.Context, id string, n int32) (*store.Run, error) {
		if n == 2 {
			return nil, transient
		}
		return native.LoadRun(ctx, id)
	}}
	svc.recordLogProducerFailure("r", store.ErrRunDataIdentityChanged)
	out, err := svc.StreamSource().SubscribeLogs(bound, "r", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	_, errs := newLogSink(out).awaitEnd(t, time.Second)
	for _, err := range errs {
		if errors.Is(err, store.ErrRunDataCapability) {
			return
		}
	}
	t.Fatalf("initial decision returned %v and closed; want typed producer refusal after retry", errs)
}

type countingLogOriginStore struct {
	*store.FilesystemRunStore
	calls atomic.Int32
	fn    func(context.Context, string, int32) (*store.Run, error)
}

func (s *countingLogOriginStore) LoadRun(ctx context.Context, id string) (*store.Run, error) {
	return s.fn(ctx, id, s.calls.Add(1))
}
func TestLogHeldLaneStatusRetriesConsumedWatch(t *testing.T) {
	svc, native := newLogLaneService(t)
	if err := seedOriginRun(t.Context(), native, "r", "before", store.LegacyValueFormat, store.RunStatusRunning, 0); err != nil {
		t.Fatal(err)
	}
	stale, err := svc.CaptureRunOrigin(t.Context(), "r")
	if err != nil {
		t.Fatal(err)
	}
	if err := replaceOriginRun(t.Context(), native, "r", "current", store.LegacyValueFormat, store.RunStatusRunning, 0); err != nil {
		t.Fatal(err)
	}
	bound := boundOriginCtx(t, svc, "r")
	writeNativeRunLog(t, native, "r", "before\n")
	buf, _ := svc.prepareRunLog(t.Context(), "r")
	defer svc.dropRunLog("r", buf)
	tightenBoundLogPoll(t, 15*time.Millisecond)
	transient := errors.New("one-shot held lane origin read error")
	svc.store = &countingLogOriginStore{FilesystemRunStore: native, fn: func(ctx context.Context, id string, n int32) (*store.Run, error) {
		if n == 1 {
			return nil, transient
		}
		return nil, store.ErrRunDataIdentityChanged
	}}
	out := runstream.NewLogPipe()
	if err := svc.liveEvents.subscribeLogs(bound, "r", 0, out); err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	sink := newLogSink(out)
	sink.awaitText(t, "before\n", time.Second)
	foreign := svc.logRegistry.acquire("r", stale, logDurable)
	defer foreign.Close(logEndLost)
	_, errs := sink.awaitEnd(t, time.Second)
	if !hasError(errs, transient) || !hasError(errs, store.ErrRunDataIdentityChanged) {
		t.Fatalf("errors=%v; want a warning then typed refusal despite held lane", errs)
	}
}
func TestLogSettleRechecksTerminalAfterForeignLaneRead(t *testing.T) {
	svc, native := newLogLaneService(t)
	if err := seedOriginRun(t.Context(), native, "r", "before", store.LegacyValueFormat, store.RunStatusRunning, 0); err != nil {
		t.Fatal(err)
	}
	stale, err := svc.CaptureRunOrigin(t.Context(), "r")
	if err != nil {
		t.Fatal(err)
	}
	if err := replaceOriginRun(t.Context(), native, "r", "current", store.LegacyValueFormat, store.RunStatusFinished, 0); err != nil {
		t.Fatal(err)
	}
	bound := boundOriginCtx(t, svc, "r")
	writeNativeRunLog(t, native, "r", "before\n")
	tightenBoundLogPoll(t, time.Hour)
	resumed := make(chan error, 1)
	svc.store = &countingLogOriginStore{FilesystemRunStore: native, fn: func(ctx context.Context, id string, n int32) (*store.Run, error) {
		if n == 3 {
			resumed <- native.UpdateRunStatus(ctx, id, store.RunStatusRunning, "")
		}
		return native.LoadRun(ctx, id)
	}}
	svc.liveEvents.beforeLogSettleEnd = func() { svc.logRegistry.acquire("r", stale, logDurable) }
	out := runstream.NewLogPipe()
	if err := svc.liveEvents.subscribeLogs(bound, "r", 0, out); err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	sink := newLogSink(out)
	select {
	case err := <-resumed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("did not reach E3 origin check")
	}
	select {
	case <-sink.done:
		by, count := svc.liveEvents.logEndBy("r")
		run, err := native.LoadRun(bound, "r")
		if err != nil {
			t.Fatal(err)
		}
		t.Fatalf("stream closed via %q (%d applies), status=%s, errors=%v; want open resumed stream", by, count, run.Status, sink.errorsSoFar())
	case <-time.After(250 * time.Millisecond):
	}
}
