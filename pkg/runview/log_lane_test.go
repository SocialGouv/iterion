package runview

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The log lane's producer core (R54 slice 4, phase 1): one retained epoch per
// producer registration, wakes without payload, and exactly one end value.
// These tests exercise the production seams — the buffer, the owner, the
// detached exit watcher and the run-log registration — never a hand-built
// registry.

func newLogLaneService(t *testing.T) (*Service, *store.FilesystemRunStore) {
	t.Helper()
	dir := t.TempDir()
	native, err := store.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	svc, err := NewService(dir, WithLogger(iterlog.Nop()), WithRecoveryPassive())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		svc.Stop(ctx)
	})
	return svc, native
}

func seedLogLaneRun(t *testing.T, native *store.FilesystemRunStore, runID string, status store.RunStatus) {
	t.Helper()
	ctx := t.Context()
	run, err := native.CreateRunWithValueFormat(ctx, store.RunCreateSpec{ID: runID, WorkflowName: "wf", Format: store.LegacyValueFormat})
	if err != nil {
		t.Fatal(err)
	}
	if status != "" && status != run.Status {
		if err := native.UpdateRunStatus(ctx, runID, status, ""); err != nil {
			t.Fatal(err)
		}
	}
}

// epochState reads a topic's bail count and settlement under the registry
// lock: tests observe the producer's accounting, never the wire flavour.
func epochState(r *logRegistry, topic *logTopic) (bails int, closed bool, end logEnd) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return topic.bails, topic.closed, topic.end
}

// T-LEAK: a closed epoch is RETAINED. A subscriber that parks across the close
// still reads its end, a late subscription is created already closed with the
// retained value, and only a newer registration replaces the epoch.
func TestLogLaneClosedEpochIsRetained(t *testing.T) {
	svc, native := newLogLaneService(t)
	ctx := t.Context()
	seedLogLaneRun(t, native, "r", store.RunStatusRunning)

	buf, _ := svc.prepareRunLog(ctx, "r")
	obs, topic, ok := svc.logRegistry.currentFor("r")
	if !ok {
		t.Fatal("no epoch was registered for the producer")
	}
	if bails, closed, _ := epochState(svc.logRegistry, topic); bails != 1 || closed {
		t.Fatalf("fresh epoch: bails %d closed %v, want 1 open", bails, closed)
	}
	sub, ok := svc.logRegistry.subscribe(obs)
	if !ok || sub.Closed || !sub.Durable {
		t.Fatalf("subscription = ok %v closed %v durable %v, want an open durable one", ok, sub.Closed, sub.Durable)
	}
	defer sub.Cancel()

	// A producer write publishes a payload-free wake (the bytes stay in the
	// store; the wake only says "read again").
	if _, err := buf.Write([]byte("first\n")); err != nil {
		t.Fatal(err)
	}
	select {
	case w := <-sub.C:
		if w.End != logEndNone {
			t.Fatalf("producer write delivered %+v, want a wake", w)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the producer's write never woke its subscriber")
	}

	// The last bail publishes the end; the channel closes right after.
	svc.dropRunLog("r", buf)
	select {
	case w, open := <-sub.C:
		if !open || w.End != logEndTerminal {
			t.Fatalf("after the close: %+v open=%v, want the terminal end", w, open)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the epoch's end was never delivered")
	}
	if _, open := <-sub.C; open {
		t.Fatal("the wake channel outlived the epoch's end")
	}

	// E2: the topic stays current (retained) after its last bail.
	_, retained, ok := svc.logRegistry.currentFor("r")
	if !ok || retained != topic {
		t.Fatal("the closed epoch was removed at its last bail")
	}
	if bails, closed, end := epochState(svc.logRegistry, retained); bails != 0 || !closed || end != logEndTerminal {
		t.Fatalf("retained epoch: bails %d closed %v end %v", bails, closed, end)
	}
	late, ok := svc.logRegistry.subscribe(obs)
	if !ok || !late.Closed {
		t.Fatal("a retained epoch no longer answers a subscription")
	}
	if w := <-late.C; w.End != logEndTerminal {
		t.Fatalf("retained subscription delivered %+v", w)
	}
	if _, open := <-late.C; open {
		t.Fatal("a retained subscription's channel stays open")
	}
	late.Cancel()

	// A newer producer is a NEW epoch; it serves, the retained one still
	// answers its own subscribers.
	next, _ := svc.prepareRunLog(ctx, "r")
	_, second, ok := svc.logRegistry.currentFor("r")
	if !ok || second == retained || second.epoch <= retained.epoch {
		t.Fatal("a new producer did not take a new epoch")
	}
	if _, closed, _ := epochState(svc.logRegistry, second); closed {
		t.Fatal("the new epoch starts closed")
	}
	svc.dropRunLog("r", next)
}

// T-STALE: a replaced buffer returns its bail. The predecessor's epoch ends
// and stops holding the run; the replacement serves.
func TestLogBufferReplacementReleasesItsBail(t *testing.T) {
	svc, native := newLogLaneService(t)
	ctx := t.Context()
	seedLogLaneRun(t, native, "r", store.RunStatusRunning)

	_, _ = svc.prepareRunLog(ctx, "r")
	_, firstTopic, ok := svc.logRegistry.currentFor("r")
	if !ok {
		t.Fatal("no epoch for the first producer")
	}
	second, _ := svc.prepareRunLog(ctx, "r")

	// A buffer still installed is a producer that never classified its end: the
	// replacement must publish LOST, not the run as finished (A-F3).
	if bails, closed, end := epochState(svc.logRegistry, firstTopic); bails != 0 || !closed || end != logEndLost {
		t.Fatalf("replaced epoch: bails %d closed %v end %v, want the released LOST end", bails, closed, end)
	}
	if got := svc.GetLogBuffer("r"); got != second {
		t.Fatal("the replacement buffer does not serve the run")
	}
	_, secondTopic, ok := svc.logRegistry.currentFor("r")
	if !ok || secondTopic == firstTopic {
		t.Fatal("the replacement reused the predecessor's epoch")
	}

	if _, err := second.Write([]byte("second-epoch\n")); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(native.Root(), "runs", "r", "run.log"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "second-epoch\n") {
		t.Fatalf("the replacement buffer does not write run.log: %q", data)
	}
	svc.dropRunLog("r", second)
}

// T-OWN(a): spawnRun's own refusal returns the producer's bail — the site
// that used to leave a lane with no producer at all.
func TestSpawnRunRefusalReturnsTheLogBail(t *testing.T) {
	svc, native := newLogLaneService(t)
	ctx := t.Context()
	seedLogLaneRun(t, native, "r", store.RunStatusRunning)

	runLog, _ := svc.prepareRunLog(ctx, "r")
	owner := svc.ownRunLog(ctx, "r", runLog, logMemoryOnly)
	// The producer registered, then the run vanished (a retirement racing the
	// launch): spawnRun's own LoadRun refuses before anything is created.
	if err := native.UpdateRunStatus(ctx, "r", store.RunStatusFinished, ""); err != nil {
		t.Fatal(err)
	}
	if err := native.DeleteRun(ctx, "r"); err != nil {
		t.Fatal(err)
	}
	if _, err := native.PruneDeletionMarkers(ctx, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	// The refusal under test happens on spawnRun's first store read, before
	// the executor is ever used; a zero executor is enough (its Close is
	// nil-safe).
	ex := &model.ClawExecutor{}
	_, err := svc.spawnRun(ctx, "r", &ir.Workflow{Name: "wf", Nodes: map[string]ir.Node{}}, "", "", "", nil,
		finalizationOpts{}, callbackOpts{}, ex, iterlog.Nop(), runLog, owner, 0, false, nil, "", nil, "", false,
		launchExtras{}, native, func(context.Context, context.Context, *runtime.Engine) error {
			t.Error("a launch whose run vanished started its body")
			return nil
		})
	if err == nil {
		t.Fatal("spawnRun accepted a run that no longer exists")
	}

	_, topic, ok := svc.logRegistry.currentFor("r")
	if !ok {
		t.Fatal("the producer never registered an epoch")
	}
	if bails, closed, end := epochState(svc.logRegistry, topic); bails != 0 || !closed || end != logEndLost {
		t.Fatalf("after the refusal: bails %d closed %v end %v, want a released, LOST epoch", bails, closed, end)
	}
	// A launcher's late defer is a no-op: the bail was already returned.
	owner.Abandon()
	if bails, _, _ := epochState(svc.logRegistry, topic); bails != 0 {
		t.Fatal("the second Abandon released a bail twice")
	}
}

// T-OWN(b): a refused RegisterDetached at reattach returns the bail — no lane
// without a producer, no buffer left behind.
func TestReattachRefusalReturnsTheLogBail(t *testing.T) {
	svc, native := newLogLaneService(t)
	seedLogLaneRun(t, native, "r", store.RunStatusRunning)
	svc.manager.Stop(context.Background())

	svc.reattachDetached("r", 424242)

	_, topic, ok := svc.logRegistry.currentFor("r")
	if !ok {
		t.Fatal("the reattach never registered an epoch")
	}
	if bails, closed, _ := epochState(svc.logRegistry, topic); bails != 0 || !closed {
		t.Fatalf("after the refused reattach: bails %d closed %v, want the bail returned", bails, closed)
	}
	if got := svc.GetLogBuffer("r"); got != nil {
		t.Fatal("a refused reattach left a buffer installed")
	}
}

// T-OWN(c): transfer is synchronous and final. Once the bail changed hands the
// launcher's Abandon cannot release it, and Transfer itself is idempotent.
func TestRunLogOwnerTransferIsSynchronous(t *testing.T) {
	svc, native := newLogLaneService(t)
	ctx := t.Context()
	seedLogLaneRun(t, native, "r", store.RunStatusRunning)

	buf, _ := svc.prepareRunLog(ctx, "r")
	owner := svc.ownRunLog(ctx, "r", buf, logMemoryOnly)
	_, topic, ok := svc.logRegistry.currentFor("r")
	if !ok {
		t.Fatal("no epoch was registered")
	}

	owner.Transfer()
	owner.Transfer()
	owner.Abandon()

	if bails, closed, _ := epochState(svc.logRegistry, topic); bails != 1 || closed {
		t.Fatalf("after a transferred bail: bails %d closed %v, want the run's own bail still held", bails, closed)
	}

	// The run's own teardown is what releases it.
	svc.dropRunLog("r", buf)
	if bails, closed, end := epochState(svc.logRegistry, topic); bails != 0 || !closed || end != logEndTerminal {
		t.Fatalf("after the run's teardown: bails %d closed %v end %v", bails, closed, end)
	}
}

// T3b: the detached exit watcher classifies the status the runner left. It is
// the function production calls, with only cmd.Wait injected.
func TestWaitDetachedExitClassifiesTheRunStatus(t *testing.T) {
	svc, native := newLogLaneService(t)
	for _, tc := range []struct {
		name   string
		status store.RunStatus
		want   logEnd
	}{
		{"paused", store.RunStatusPausedWaitingHuman, logEndPaused},
		{"killed-while-running", store.RunStatusRunning, logEndLost},
		{"finished", store.RunStatusFinished, logEndTerminal},
		{"failed", store.RunStatusFailed, logEndTerminal},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runID := "r-" + tc.name
			seedLogLaneRun(t, native, runID, tc.status)
			buf := svc.prepareRunLogNoFile(t.Context(), runID)
			owner := svc.ownRunLog(t.Context(), runID, buf, svc.detachedLogDurability())
			owner.Transfer()

			waited := false
			waitDetachedExit(svc, runID, buf, func() { waited = true })

			if !waited {
				t.Fatal("the watcher did not wait for the process")
			}
			_, topic, ok := svc.logRegistry.currentFor(runID)
			if !ok {
				t.Fatal("the detached producer never registered an epoch")
			}
			if bails, closed, end := epochState(svc.logRegistry, topic); bails != 0 || !closed || end != tc.want {
				t.Fatalf("status %s: bails %d closed %v end %v, want %v", tc.status, bails, closed, end, tc.want)
			}
		})
	}
}

// T4d: an in-process abandon before the run's claim is classified by the run's
// status — the seam every early return of the launch/resume path goes through.
func TestInProcessAbandonClassifiesTheRunStatus(t *testing.T) {
	svc, native := newLogLaneService(t)
	for _, tc := range []struct {
		name   string
		status store.RunStatus
		want   logEnd
	}{
		{"paused-resume-refused", store.RunStatusPausedWaitingHuman, logEndPaused},
		{"vanished", store.RunStatusRunning, logEndLost},
		{"already-terminal", store.RunStatusFinished, logEndTerminal},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runID := "r-" + tc.name
			seedLogLaneRun(t, native, runID, tc.status)
			buf, _ := svc.prepareRunLog(t.Context(), runID)
			owner := svc.ownRunLog(t.Context(), runID, buf, logMemoryOnly)

			owner.Abandon()

			_, topic, ok := svc.logRegistry.currentFor(runID)
			if !ok {
				t.Fatal("no epoch was registered")
			}
			if bails, closed, end := epochState(svc.logRegistry, topic); bails != 0 || !closed || end != tc.want {
				t.Fatalf("status %s: bails %d closed %v end %v, want %v", tc.status, bails, closed, end, tc.want)
			}
		})
	}
}

// T-FAIL(b): a local capture failure is a FIN, notified to a subscriber that
// already parked — the handshake is watch installed AND initial observation
// returned, never a timeout.
func TestParkedSubscriberIsNotifiedOfACaptureFailure(t *testing.T) {
	svc, native := newLogLaneService(t)
	ctx := t.Context()
	seedLogLaneRun(t, native, "r", store.RunStatusRunning)
	bound := boundOriginCtx(t, svc, "r")

	watch, cancelWatch := svc.logRegistry.watch("r")
	defer cancelWatch()
	// The initial observation: no producer yet, so no lane.
	sub, _, err := svc.observedLogLane(bound, "r", false)
	if err != nil {
		t.Fatal(err)
	}
	if sub != nil {
		t.Fatal("a lane existed before any producer registered")
	}

	// The producer's capture now fails for real: its run metadata no longer
	// decodes.
	if err := os.WriteFile(filepath.Join(native.Root(), "runs", "r", "run.json"), []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	svc.prepareRunLog(ctx, "r")

	select {
	case <-watch:
	case <-time.After(2 * time.Second):
		t.Fatal("a parked subscriber was never notified of the capture failure")
	}
	failure := svc.localLogProducerFailure("r")
	if !errors.Is(failure, store.ErrRunMetadataUndecodable) {
		t.Fatalf("retained failure = %v, want the capture refusal", failure)
	}
	if epoch, ok := svc.logRegistry.failureEpoch("r"); !ok || epoch == 0 {
		t.Fatal("a capture failure recorded before any epoch carries no generation")
	}
}

// T-FAIL(c): the failure is retained across the producer's bail (record →
// dropRunLog → observation) and ends the epoch as a typed LOST.
func TestRecordedFailureSurvivesTheProducerBail(t *testing.T) {
	svc, native := newLogLaneService(t)
	ctx := t.Context()
	seedLogLaneRun(t, native, "r", store.RunStatusRunning)

	buf, _ := svc.prepareRunLog(ctx, "r")
	obs, _, ok := svc.logRegistry.currentFor("r")
	if !ok {
		t.Fatal("no epoch was registered")
	}
	sub, ok := svc.logRegistry.subscribe(obs)
	if !ok {
		t.Fatal("the open epoch refused a subscription")
	}
	defer sub.Cancel()

	// A pending wake occupies the cap-1 channel: the end must displace it,
	// never block behind it (the publisher holds the registry lock).
	if _, err := buf.Write([]byte("pending\n")); err != nil {
		t.Fatal(err)
	}

	typed := fmt.Errorf("runview: run r log tee: %w", errors.Join(store.ErrRunDataCapability, syscall.ENOSPC))
	svc.recordLogProducerFailure("r", typed)

	select {
	case w, open := <-sub.C:
		if !open || w.End != logEndLost {
			t.Fatalf("the failure delivered %+v open=%v, want the lost end", w, open)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the parked subscriber was never told the producer failed")
	}
	if _, open := <-sub.C; open {
		t.Fatal("the wake channel outlived the failure")
	}

	svc.dropRunLog("r", buf)

	if got := svc.localLogProducerFailure("r"); !errors.Is(got, typed) {
		t.Fatalf("failure after the producer's bail = %v, want the retained cause", got)
	}
	_, topic, ok := svc.logRegistry.currentFor("r")
	if !ok || !topic.closed || topic.end != logEndLost {
		t.Fatal("the failure did not survive the producer's bail")
	}
	if epoch, ok := svc.logRegistry.failureEpoch("r"); !ok || epoch != topic.epoch {
		t.Fatalf("retained failure epoch = %d (ok %v), want the recording epoch %d", epoch, ok, topic.epoch)
	}
	late, ok := svc.logRegistry.subscribe(obs)
	if !ok || !late.Closed {
		t.Fatal("a subscriber arriving after the drop does not see the retained failure")
	}
	if w := <-late.C; w.End != logEndLost {
		t.Fatalf("retained subscription delivered %+v", w)
	}
	late.Cancel()
}

func TestLateTeeFailureDoesNotEndReplacementEpoch(t *testing.T) {
	svc, native := newLogLaneService(t)
	ctx := t.Context()
	seedLogLaneRun(t, native, "r", store.RunStatusRunning)
	readOnly, err := os.OpenFile(filepath.Join(t.TempDir(), "run.log"), os.O_CREATE|os.O_RDONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = readOnly.Close() })
	old := &RunLogBuffer{ring: make([]byte, 0, runLogRingCap), file: readOnly}
	svc.registerRunLogOrigin(ctx, "r", old, logDurable)
	old.releaseLogLane()
	current, _ := svc.prepareRunLog(ctx, "r")
	t.Cleanup(func() { svc.dropRunLog("r", current) })
	_, topic, ok := svc.logRegistry.currentFor("r")
	if !ok || current.lane == nil || topic != current.lane.topic {
		t.Fatal("replacement epoch was not registered")
	}
	_, _ = old.Write([]byte("late\n"))
	if !errors.Is(old.fileErr, syscall.EBADF) {
		t.Fatalf("late tee error = %v, want EBADF", old.fileErr)
	}
	if _, _, end := epochState(svc.logRegistry, topic); end != logEndNone {
		t.Fatalf("late tee ended the replacement epoch: %v", end)
	}
}

// T-FAIL(d): arbitrage is first-end-wins. A tee failure recorded after the
// epoch's end is ignored (the producer already registered), and a failure
// recorded first is not replaced by a later end.
func TestALateFailureNeverReplacesAnAppliedEnd(t *testing.T) {
	svc, native := newLogLaneService(t)
	ctx := t.Context()

	// End first, failure second.
	seedLogLaneRun(t, native, "ended", store.RunStatusRunning)
	buf, _ := svc.prepareRunLog(ctx, "ended")
	obs, _, ok := svc.logRegistry.currentFor("ended")
	if !ok {
		t.Fatal("no epoch was registered")
	}
	lane := buf.lane
	svc.dropRunLog("ended", buf)
	svc.recordLogTeeFailure(lane, fmt.Errorf("runview: run ended log tee: %w", store.ErrRunDataCapability))

	if got := svc.localLogProducerFailure("ended"); got != nil {
		t.Fatalf("a late failure replaced an applied end: %v", got)
	}
	late, ok := svc.logRegistry.subscribe(obs)
	if !ok || !late.Closed {
		t.Fatal("the settled epoch lost its retained end")
	}
	if w := <-late.C; w.End != logEndTerminal {
		t.Fatalf("the settled epoch delivered %+v, want its terminal end", w)
	}
	late.Cancel()

	// Failure first, end second.
	seedLogLaneRun(t, native, "failed", store.RunStatusRunning)
	buf2, _ := svc.prepareRunLog(ctx, "failed")
	obs2, _, ok := svc.logRegistry.currentFor("failed")
	if !ok {
		t.Fatal("no epoch was registered")
	}
	typed := fmt.Errorf("runview: run failed log tee: %w", store.ErrRunDataCapability)
	svc.recordLogTeeFailure(buf2.lane, typed)
	svc.dropRunLog("failed", buf2)

	if got := svc.localLogProducerFailure("failed"); !errors.Is(got, typed) {
		t.Fatalf("the first end was replaced: %v, want the recorded failure", got)
	}
	sub, ok := svc.logRegistry.subscribe(obs2)
	if !ok || !sub.Closed {
		t.Fatal("the failed epoch was not retained")
	}
	if w := <-sub.C; w.End != logEndLost {
		t.Fatalf("the failed epoch delivered %+v, want LOST", w)
	}
	sub.Cancel()
}

// The tee failure is typed at its single site and never wakes: a stream whose
// bytes stopped reaching the store must fail, not read as up to date.
func TestLogTeeFailureIsTypedAndNeverWakes(t *testing.T) {
	svc, native := newLogLaneService(t)
	ctx := t.Context()
	seedLogLaneRun(t, native, "r", store.RunStatusRunning)

	ro, err := os.OpenFile(filepath.Join(t.TempDir(), "run.log"), os.O_CREATE|os.O_RDONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ro.Close() })
	buf := &RunLogBuffer{ring: make([]byte, 0, runLogRingCap), file: ro}
	svc.registerRunLogOrigin(ctx, "r", buf, logDurable)
	obs, _, ok := svc.logRegistry.currentFor("r")
	if !ok {
		t.Fatal("no epoch was registered")
	}
	sub, ok := svc.logRegistry.subscribe(obs)
	if !ok {
		t.Fatal("the open epoch refused a subscription")
	}
	defer sub.Cancel()

	if _, err := buf.Write([]byte("bytes\n")); err != nil {
		t.Fatal(err)
	}
	failure := svc.localLogProducerFailure("r")
	if !errors.Is(failure, store.ErrRunDataCapability) {
		t.Fatalf("tee failure = %v, want the capability member", failure)
	}
	if !errors.Is(failure, syscall.EBADF) {
		t.Fatalf("tee failure = %v, want the OS cause joined too", failure)
	}
	if got := buf.persistFailure(); !errors.Is(got, syscall.EBADF) {
		t.Fatalf("persistFailure = %v, want the first failed write", got)
	}
	// The failure is the epoch's end, never a byte wake: a wake would tell
	// the reader the store holds bytes it never received.
	select {
	case w := <-sub.C:
		if w.End != logEndLost {
			t.Fatalf("a failed tee delivered %+v, want the lost end", w)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the failed tee never ended the epoch")
	}
}
