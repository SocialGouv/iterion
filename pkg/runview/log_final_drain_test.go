package runview

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/runview/runstream"
	"github.com/SocialGouv/iterion/pkg/store"
)

var errFinalLogRead = errors.New("one recoverable final log read fault")

type finalDrainLogStore struct {
	*store.FilesystemRunStore
	armed        atomic.Bool
	failOnce     atomic.Bool
	finalReads   atomic.Int32
	finished     atomic.Bool
	ordinary     chan struct{}
	ordinaryOnce atomic.Bool
	final        chan struct{}
	outcome      string
}

type finalDrainLogSelection struct {
	store.FilesystemStreamSelection
	outer context.Context
	fault *finalDrainLogStore
}

func (s *finalDrainLogStore) OpenFilesystemStream(ctx context.Context, id string, opts store.FilesystemStreamOpenOptions) (store.FilesystemStreamSelection, bool, error) {
	sel, observed, err := s.FilesystemRunStore.OpenFilesystemStream(ctx, id, opts)
	if err != nil || sel == nil {
		return sel, observed, err
	}
	return &finalDrainLogSelection{FilesystemStreamSelection: sel, outer: ctx, fault: s}, observed, nil
}

func (s *finalDrainLogSelection) Validate(ctx context.Context) error {
	if s.fault.armed.Load() && !s.fault.finished.Load() {
		if ctx != s.outer {
			if s.fault.ordinaryOnce.CompareAndSwap(false, true) {
				close(s.fault.ordinary)
			}
			<-ctx.Done()
			return ctx.Err()
		}
		if s.fault.finalReads.Add(1) == 1 {
			close(s.fault.final)
		}
		switch s.fault.outcome {
		case "stop":
			<-ctx.Done()
			return ctx.Err()
		case "permanent":
			return store.ErrRunDataCapability
		case "recover-after-grace":
			if s.fault.finalReads.Load() < 3 {
				return errFinalLogRead
			}
		}
		if s.fault.failOnce.CompareAndSwap(true, false) {
			return errFinalLogRead
		}
		s.fault.finished.Store(true)
	}
	return s.FilesystemStreamSelection.Validate(ctx)
}

func TestCommittedLogEndDeliversPersistedBytes(t *testing.T) {
	for _, path := range []string{"E1", "E2", "E3"} {
		for _, name := range []string{"admitted", "permanent", "one-transient", "recover-after-grace", "stop"} {
			t.Run(path+"/"+name, func(t *testing.T) {
				if name == "recover-after-grace" {
					tightenBoundLogPoll(t, 20*time.Millisecond)
					previous := logProducerStallWarn
					logProducerStallWarn = 100 * time.Millisecond
					t.Cleanup(func() { logProducerStallWarn = previous })
				}
				svc, native := newLogLaneService(t)
				status := store.RunStatusRunning
				if path == "E3" {
					status = store.RunStatusFinished
				}
				seedLogLaneRun(t, native, "r", status)
				writeNativeRunLog(t, native, "r", "stored suffix\n")
				bound := boundOriginCtx(t, svc, "r")
				fault := &finalDrainLogStore{FilesystemRunStore: native, outcome: name, ordinary: make(chan struct{}), final: make(chan struct{})}
				fault.armed.Store(true)
				fault.failOnce.Store(name == "one-transient")
				svc.liveEvents.files = runstream.NewFileSource(fault, "", svc.logger)
				obs, err := svc.CaptureRunOrigin(t.Context(), "r")
				if err != nil {
					t.Fatal(err)
				}
				if path == "E2" {
					svc.logRegistry.acquire("r", obs, logDurable).Close(logEndTerminal)
				}
				var lane *logLane
				if path == "E1" {
					lane = svc.logRegistry.acquire("r", obs, logDurable)
					defer lane.Close(logEndLost)
				}
				sub, err := svc.StreamSource().SubscribeLogs(bound, "r", 0)
				if err != nil {
					t.Fatal(err)
				}
				defer sub.Close()
				sink := newLogSink(sub)
				if lane != nil {
					select {
					case <-fault.ordinary:
					case <-time.After(time.Second):
						t.Fatal("ordinary read did not enter")
					}
					lane.Close(logEndTerminal)
				}
				if name == "stop" {
					select {
					case <-fault.final:
					case <-time.After(2 * time.Second):
						t.Fatal("final read did not enter")
					}
					stop, cancel := context.WithTimeout(t.Context(), time.Second)
					defer cancel()
					svc.StopBackground(stop)
					if stop.Err() != nil {
						t.Fatal(stop.Err())
					}
				}
				text, errs := sink.awaitEnd(t, 4*time.Second)
				by, applies := svc.liveEvents.logEndBy("r")
				t.Logf("bytes=%q errors=%v end=%s/%d post-end reads=%d", text, errs, by, applies, fault.finalReads.Load())
				want := map[string]string{"E1": logEndByLane, "E2": logEndByRetained, "E3": logEndBySettle}[path]
				if by != want || applies != 1 || fault.finalReads.Load() < 1 {
					t.Fatalf("did not exercise committed %s: %q/%d, %d final reads", path, by, applies, fault.finalReads.Load())
				}
				var wantErr error
				switch name {
				case "stop":
					wantErr = runstream.ErrSourceClosed
				case "permanent":
					wantErr = store.ErrRunDataCapability
				}
				if wantErr != nil {
					if text != "" || len(errs) != 1 || !errors.Is(errs[0], wantErr) {
						t.Fatalf("got %q/%v, want typed %v and no bytes", text, errs, wantErr)
					}
					return
				}
				if text != "stored suffix\n" {
					t.Fatalf("committed %s closed before returning stored bytes: got %q, errors=%v", path, text, errs)
				}
				for _, err := range errs {
					if runstream.IsPermanentError(err) || !errors.Is(err, errFinalLogRead) {
						t.Errorf("unexpected error: %v", err)
					}
				}
			})
		}
	}
}
