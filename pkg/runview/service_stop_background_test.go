package runview

import (
	"context"
	"runtime"
	"strings"
	"testing"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// alertPollFrame names the alert manager's stall-poll goroutine,
// pipelineSchedulerFrame the pipeline scheduler's. Both workers own their
// goroutine — production closes nothing when either returns — so the frame is
// the only oracle available from here.
const (
	alertPollFrame         = "alert.(*Manager).Start.func1"
	pipelineSchedulerFrame = "runview.(*Service).startPipelineScheduler.func1"
)

func liveGoroutines(frame string) int {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			return strings.Count(string(buf[:n]), frame)
		}
		buf = make([]byte, 2*len(buf))
	}
}

// settlePollGoroutines polls until the count reaches want or the budget runs
// out — a stopped goroutine returns shortly after its signal, not at the
// instant of it. Returns the last count read.
func settlePollGoroutines(frame string, want int) int {
	deadline := time.Now().Add(3 * time.Second)
	got := liveGoroutines(frame)
	for got != want && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
		got = liveGoroutines(frame)
	}
	return got
}

// stableGoroutines returns a count that two readings apart agree on, so a
// worker already winding down from an earlier test is not counted as
// standing — a baseline taken on one sample is a baseline nothing can come
// back to. Falls back to the last reading when nothing settles in budget.
func stableGoroutines(frame string) int {
	deadline := time.Now().Add(3 * time.Second)
	prev := liveGoroutines(frame)
	for time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
		got := liveGoroutines(frame)
		if got == prev {
			return got
		}
		prev = got
	}
	return prev
}

// StopBackground is the seam a caller that REPLACES a service without
// draining it needs (the studio's project hot-swap). Both halves are
// load-bearing: every periodic worker must stop, and the runs already in
// flight must be left strictly alone — a Drain-shaped stop would cancel the
// engines that are still writing to the store they started on.
func TestStopBackground_StopsEveryWorkerAndLeavesRunsAlone(t *testing.T) {
	alertBase := stableGoroutines(alertPollFrame)
	schedBase := stableGoroutines(pipelineSchedulerFrame)
	svc, err := NewService(t.TempDir(),
		WithLogger(iterlog.Nop()),
		WithMaxConcurrentPipelines(2),
		WithAlerts(AlertSettings{StallTimeout: time.Minute}),
	)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	if svc.pipelineQueue == nil || svc.alertManager == nil || svc.reconcileDone == nil {
		t.Fatalf("a worker was never started, so stopping it proves nothing: queue=%v alerts=%v reconcile=%v",
			svc.pipelineQueue != nil, svc.alertManager != nil, svc.reconcileDone != nil)
	}

	// One run in flight, registered exactly as spawnRun leaves it.
	const runID = "run-hot-swap"
	if _, err := svc.store.CreateRun(context.Background(), runID, "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	runCtx, regErr := svc.manager.Register(context.Background(), runID)
	if regErr != nil {
		t.Fatalf("Register: %v", regErr)
	}

	stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	svc.StopBackground(stopCtx)

	// The reconcile goroutine closes reconcileDone on its way out and
	// StopBackground AWAITS that close (stopPeriodicReconcile, bounded by
	// stopCtx), so the fact is already true here: a channel still open is the
	// defect, not a report in flight — and a timer arm would only make the
	// verdict one a starved host can flip.
	select {
	case <-svc.reconcileDone:
	default:
		t.Errorf("orphan reconcile is still running after StopBackground")
	}
	// pipelineStop is the scheduler's stop SIGNAL, closed by StopBackground
	// itself, so reading it says the signal was sent and nothing about the
	// worker reading it. Two separate verdicts then: the signal here, and
	// below the goroutine — which is the one that matters, and which neither
	// worker reports on its own.
	select {
	case <-svc.pipelineStop:
	default:
		t.Errorf("the pipeline scheduler was never signalled: StopBackground left pipelineStop open")
	}
	for _, w := range []struct {
		kind  string
		frame string
		base  int
	}{
		{"pipeline scheduler", pipelineSchedulerFrame, schedBase},
		{"alert stall poll", alertPollFrame, alertBase},
	} {
		if n := settlePollGoroutines(w.frame, w.base); n != w.base {
			t.Errorf("%s: %d live after StopBackground, want %d — the worker is still scanning a store the caller has re-pointed away from",
				w.kind, n, w.base)
		}
	}

	// The run keeps its context and its handle: nothing was drained.
	if err := runCtx.Err(); err != nil {
		t.Errorf("the in-flight run was cancelled by a background stop: %v", err)
	}
	if !svc.Active(runID) {
		t.Errorf("the in-flight run lost its handle to a background stop")
	}
	r, err := svc.store.LoadRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if r.Status == store.RunStatusFailedResumable {
		t.Errorf("the in-flight run was flipped to %q — StopBackground must not touch persisted state", r.Status)
	}
	// And the service still refuses nothing: draining was never set.
	if svc.draining.Load() {
		t.Errorf("StopBackground set the draining flag — later launches would be refused on a service that was only re-pointed")
	}
}
