package runview

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/runview/runstream"
	"github.com/SocialGouv/iterion/pkg/store"
)

// A bound log stream that a local producer's failure ends, ends typed: the
// failure is permanent whatever its own cause, and the E3 settle never
// concludes over it. These witnesses drive the real producer
// (prepareRunLog) and the real settle branch.

// captureFailureCause makes the capture of prepareRunLog fail for a cause
// that is not permanent by itself, and returns that cause.
type captureFailureCause struct {
	name string
	// prepare runs prepareRunLog on run r with its capture failing.
	prepare func(t *testing.T, svc *Service, native *store.FilesystemRunStore) *RunLogBuffer
	want    error
}

var captureFailureCauses = []captureFailureCause{
	{
		name: "run.json unreadable",
		prepare: func(t *testing.T, svc *Service, native *store.FilesystemRunStore) *RunLogBuffer {
			t.Helper()
			if os.Geteuid() == 0 {
				t.Skip("permissions do not bind root")
			}
			meta := filepath.Join(native.Root(), "runs", "r", "run.json")
			if err := os.Chmod(meta, 0); err != nil {
				t.Fatal(err)
			}
			restore := sync.OnceFunc(func() {
				if err := os.Chmod(meta, 0o644); err != nil {
					t.Errorf("restore permissions of %s: %v", meta, err)
				}
			})
			t.Cleanup(restore)
			buf, _ := svc.prepareRunLog(t.Context(), "r")
			restore()
			return buf
		},
		want: fs.ErrPermission,
	},
	{
		name: "launch context cancelled",
		prepare: func(t *testing.T, svc *Service, _ *store.FilesystemRunStore) *RunLogBuffer {
			t.Helper()
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			buf, _ := svc.prepareRunLog(ctx, "r")
			return buf
		},
		want: context.Canceled,
	},
}

// requireCaptureFailed asserts the capture really failed for want, its own
// cause, which no stream classifies as permanent.
func requireCaptureFailed(t *testing.T, svc *Service, want error) {
	t.Helper()
	if runstream.IsPermanentError(want) {
		t.Fatalf("fixture: %v is already permanent", want)
	}
	if got := svc.localLogProducerFailure("r"); !errors.Is(got, want) {
		t.Fatalf("recorded capture failure = %v, want its cause %v", got, want)
	}
}

// permanentEnd returns the first permanent error a stream ended with: a
// socket turns it into log_stream_failed, and a stream with none into a clean
// log_terminated.
func permanentEnd(errs []error) error {
	for _, err := range errs {
		if runstream.IsPermanentError(err) {
			return err
		}
	}
	return nil
}

// A capture failure is the run's typed end through the real producer: the
// bound stream ends permanent with the capture's own cause in its chain,
// whether it subscribes after the capture (refused at setup) or was already
// open (ended by its re-arm).
func TestACaptureFailureEndsTheBoundStreamPermanent(t *testing.T) {
	for _, cause := range captureFailureCauses {
		t.Run(cause.name+"/subscribed after", func(t *testing.T) {
			svc, native := newLogLaneService(t)
			tightenLogLaneWait(t, 100*time.Millisecond)
			seedLogLaneRun(t, native, "r", store.RunStatusRunning)
			buf := cause.prepare(t, svc, native)
			t.Cleanup(func() { svc.dropRunLog("r", buf) })
			requireCaptureFailed(t, svc, cause.want)

			text, errs := newLogSink(subscribeBoundLogs(t, svc, "r")).awaitEnd(t, 5*time.Second)
			end := permanentEnd(errs)
			if end == nil {
				t.Fatalf("the stream ended with %v (text %q), none permanent: a socket reads a clean log_terminated over a capture failure", errs, text)
			}
			if !errors.Is(end, cause.want) {
				t.Fatalf("permanent end %v lost the capture's cause %v", end, cause.want)
			}
		})
		t.Run(cause.name+"/open before", func(t *testing.T) {
			svc, native := newLogLaneService(t)
			tightenLogLaneWait(t, 100*time.Millisecond)
			tightenBoundLogPoll(t, time.Hour)
			seedLogLaneRun(t, native, "r", store.RunStatusRunning)
			writeNativeRunLog(t, native, "r", "before\n")
			polled := make(chan struct{}, 1)
			svc.liveEvents.beforeLogStatusPoll = func() {
				select {
				case polled <- struct{}{}:
				default:
				}
			}
			sink := newLogSink(subscribeBoundLogs(t, svc, "r"))
			sink.awaitText(t, "before\n", 5*time.Second)
			select {
			case <-polled:
			case <-time.After(5 * time.Second):
				t.Fatal("the merge never ran its first status read")
			}
			buf := cause.prepare(t, svc, native)
			t.Cleanup(func() { svc.dropRunLog("r", buf) })
			requireCaptureFailed(t, svc, cause.want)

			text, errs := sink.awaitEnd(t, 5*time.Second)
			end := permanentEnd(errs)
			if end == nil {
				t.Fatalf("the open stream ended with %v (text %q), none permanent: a socket reads a clean log_terminated over a capture failure", errs, text)
			}
			if !errors.Is(end, cause.want) {
				t.Fatalf("permanent end %v lost the capture's cause %v", end, cause.want)
			}
		})
	}
}

// A producer's failure is never the reader's origin verdict. Its cause may
// carry one — a capture whose load answered an absence, a deletion or another
// birth — and keeps it in its text, but errors.Is finds none through the
// retained failure, at either storage point (no epoch yet, an open epoch it
// ends): a socket closes on the origin loss of its own bound reads
// (scope_lost), never on a producer's. The failure stays permanent.
func TestAProducerFailureIsNeverTheReadersOriginVerdict(t *testing.T) {
	for _, verdict := range []error{store.ErrRunNotFound, store.ErrRunDeleted, store.ErrRunDataIdentityChanged} {
		for _, epoch := range []string{"no epoch", "open epoch"} {
			t.Run(epoch+"/"+verdict.Error(), func(t *testing.T) {
				svc, native := newLogLaneService(t)
				if epoch == "open epoch" {
					seedLogLaneRun(t, native, "r", store.RunStatusRunning)
					buf, _ := svc.prepareRunLog(t.Context(), "r")
					t.Cleanup(func() { svc.dropRunLog("r", buf) })
					if _, _, ok := svc.logRegistry.currentFor("r"); !ok {
						t.Fatal("fixture: the producer opened no epoch")
					}
				}
				svc.recordLogProducerFailure("r", fmt.Errorf("runview: capture the log origin of r: store: load run r: %w", verdict))
				got := svc.localLogProducerFailure("r")
				if errors.Is(got, verdict) {
					t.Fatalf("retained failure %q answers errors.Is(%v): a socket would close on a producer's failure as its own origin's loss", got, verdict)
				}
				if !runstream.IsPermanentError(got) {
					t.Fatalf("retained failure %q is not permanent", got)
				}
				if !strings.Contains(got.Error(), verdict.Error()) {
					t.Fatalf("retained failure %q lost its cause's text %q", got, verdict)
				}
			})
		}
	}
}

// The real path (the round-1 probe PF): a producer's capture found no run
// under the ID, then a run is created under it — another birth — and a reader
// bound to that run subscribes. Its stream ends permanent with the capture's
// text, never with the origin verdict the capture's own load answered.
func TestACaptureThatFoundNoRunIsNotTheSuccessorsOriginLoss(t *testing.T) {
	svc, native := newLogLaneService(t)
	ctx := t.Context()
	tightenLogLaneWait(t, 100*time.Millisecond)
	buf, _ := svc.prepareRunLog(ctx, "r")
	svc.dropRunLog("r", buf)
	if got := svc.localLogProducerFailure("r"); got == nil || !strings.Contains(got.Error(), store.ErrRunNotFound.Error()) {
		t.Fatalf("fixture: the retained capture failure = %v, want its load's %q", got, store.ErrRunNotFound)
	}
	if err := seedOriginRun(ctx, native, "r", "B", store.LegacyValueFormat, store.RunStatusRunning, 0); err != nil {
		t.Fatal(err)
	}

	text, errs := newLogSink(subscribeBoundLogs(t, svc, "r")).awaitEnd(t, 5*time.Second)
	end := permanentEnd(errs)
	if end == nil {
		t.Fatalf("the successor's stream ended with %v (text %q), none permanent", errs, text)
	}
	if hasError(errs, store.ErrRunNotFound) {
		t.Fatalf("the successor's stream ended with %v: errors.Is finds ErrRunNotFound, so its healthy socket would close with scope_lost", errs)
	}
	if !strings.Contains(end.Error(), store.ErrRunNotFound.Error()) {
		t.Fatalf("permanent end %q lost the capture's text %q", end, store.ErrRunNotFound)
	}
}

func TestCaptureFailureRefusesBoundReplayEvenWhenTheProducerWrites(t *testing.T) {
	svc, native := newLogLaneService(t)
	ctx := t.Context()
	seedLogLaneRun(t, native, "r", store.RunStatusRunning)
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	buf, _ := svc.prepareRunLog(cancelled, "r")
	t.Cleanup(func() { svc.dropRunLog("r", buf) })
	requireCaptureFailed(t, svc, context.Canceled)
	if _, err := buf.Write([]byte("persisted\n")); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(native.Root(), "runs", "r", "run.log"))
	if err != nil || string(data) != "persisted\n" {
		t.Fatalf("run.log = %q, %v; want persisted bytes", data, err)
	}
	text, errs := newLogSink(subscribeBoundLogs(t, svc, "r")).awaitEnd(t, 5*time.Second)
	if text != "" || !errors.Is(permanentEnd(errs), store.ErrRunDataCapability) {
		t.Fatalf("bound replay = %q, %v; want no bytes and a capability refusal", text, errs)
	}
}

func TestCaptureFailureRechecksTheDeletedBoundRun(t *testing.T) {
	svc, native := newLogLaneService(t)
	ctx := t.Context()
	tightenBoundLogPoll(t, time.Hour)
	seedLogLaneRun(t, native, "r", store.RunStatusRunning)
	writeNativeRunLog(t, native, "r", "before\n")
	// Hold a real epoch so the status poll cannot independently refuse the
	// stream; the failed capture ends that epoch and delivers its own cause.
	original, _ := svc.prepareRunLog(ctx, "r")
	t.Cleanup(func() { svc.dropRunLog("r", original) })
	sink := newLogSink(subscribeBoundLogs(t, svc, "r"))
	sink.awaitText(t, "before\n", 5*time.Second)
	if err := native.DeleteRun(ctx, "r"); err != nil {
		t.Fatal(err)
	}
	if lane := svc.registerRunLogOrigin(ctx, "r", &RunLogBuffer{}, logDurable); lane != nil {
		t.Fatal("capture unexpectedly registered a lane after deletion")
	}
	_, errs := sink.awaitEnd(t, 5*time.Second)
	if end := permanentEnd(errs); !originRefused(end) {
		t.Fatalf("end = %v, want the reader's own origin refusal", end)
	}
}

// The typing is the bound reader's: an unbound subscription to the producer
// whose capture failed reads its live buffer as before and ends clean when
// the buffer is dropped.
func TestACaptureFailureLeavesAnUnboundStreamAsItWas(t *testing.T) {
	svc, native := newLogLaneService(t)
	seedLogLaneRun(t, native, "r", store.RunStatusRunning)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	buf, _ := svc.prepareRunLog(ctx, "r")
	requireCaptureFailed(t, svc, context.Canceled)
	if _, err := buf.Write([]byte("live\n")); err != nil {
		t.Fatal(err)
	}
	sub, err := svc.StreamSource().SubscribeLogs(t.Context(), "r", 0)
	if err != nil {
		t.Fatalf("unbound log subscription: %v", err)
	}
	t.Cleanup(func() { _ = sub.Close() })
	sink := newLogSink(sub)
	sink.awaitText(t, "live\n", 5*time.Second)
	svc.dropRunLog("r", buf)
	if text, errs := sink.awaitEnd(t, 5*time.Second); len(errs) != 0 {
		t.Fatalf("the unbound stream ended with %v (text %q), want a clean end", errs, text)
	}
}

// E3 never concludes over a producer failure recorded while its window ran.
// The seam records it at the last moment the settle branch is served, its
// watch token still pending behind the branch: the stream ends with that
// failure, typed, never clean on the settle.
func TestTheSettleNeverConcludesOverARecordedProducerFailure(t *testing.T) {
	svc, native := newLogLaneService(t)
	tightenLogLaneWait(t, 100*time.Millisecond)
	tightenBoundLogPoll(t, time.Hour)
	seedLogLaneRun(t, native, "r", store.RunStatusFinished)
	writeNativeRunLog(t, native, "r", "finished\n")
	marker := errors.New("a producer failed while the settle window ran")
	var served atomic.Int32
	svc.liveEvents.beforeLogSettleEnd = func() {
		if served.Add(1) == 1 {
			svc.recordLogProducerFailure("r", fmt.Errorf("runview: capture the log origin of r: %w", marker))
		}
	}

	text, errs := newLogSink(subscribeBoundLogs(t, svc, "r")).awaitEnd(t, 5*time.Second)
	if served.Load() == 0 {
		t.Fatalf("the settle branch was never served (end %v, text %q)", errs, text)
	}
	by, applies := svc.liveEvents.logEndBy("r")
	if !hasError(errs, marker) {
		t.Fatalf("the stream ended with %v (text %q, end by %q): the settle concluded over a recorded producer failure", errs, text, by)
	}
	if permanentEnd(errs) == nil {
		t.Fatalf("the failure ended the stream untyped: %v", errs)
	}
	if by == logEndBySettle || applies != 1 {
		t.Fatalf("end by %q applied %d times, want one application that is not the settle's", by, applies)
	}
}
