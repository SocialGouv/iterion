package runview

import (
	"context"
	"maps"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// alertPollFrame names the alert manager's stall-poll goroutine,
// pipelineSchedulerFrame the pipeline scheduler's. Both workers own their
// goroutine — production closes nothing when either returns — so the frame is
// the only oracle available from here. A frame is matched as frame+"(" against
// the goroutine's OWN stack: a "created by" line names the PARENT's frame, so
// the worker's children carry it without being workers, and a bare substring
// matches a sibling whose name carries the frame as a PREFIX
// (startPipelineScheduler.func1 inside startPipelineScheduler.func11) — the
// rename that flipped the count-based sibling of this verdict.
const (
	alertPollFrame         = "alert.(*Manager).Start.func1"
	pipelineSchedulerFrame = "runview.(*Service).startPipelineScheduler.func1"
)

// goroutinesCarrying returns the IDS of the goroutines whose own stack carries
// frame. The runtime never reuses a goroutine id, so two readings are
// comparable as SETS — the shape the verdict below needs: a count cannot tell
// "this service's worker is gone" from "this service's worker is alive and an
// ambient one left in between".
func goroutinesCarrying(t *testing.T, frame string) map[int]bool {
	t.Helper()
	buf := make([]byte, 1<<20)
	n := runtime.Stack(buf, true)
	for n == len(buf) {
		buf = make([]byte, 2*len(buf))
		n = runtime.Stack(buf, true)
	}
	ids := map[int]bool{}
	for _, block := range strings.Split(string(buf[:n]), "\n\n") {
		head, stack, ok := strings.Cut(block, "\n")
		if !ok || !strings.HasPrefix(head, "goroutine ") {
			continue
		}
		// A "created by" line names the frame of the goroutine's PARENT.
		own, _, _ := strings.Cut(stack, "\ncreated by ")
		if !strings.Contains(own, frame+"(") {
			continue
		}
		fields := strings.Fields(head)
		if len(fields) < 2 {
			t.Fatalf("unreadable goroutine header %q — this oracle reads ids out of the runtime's dump", head)
		}
		id, err := strconv.Atoi(fields[1])
		if err != nil {
			t.Fatalf("unreadable goroutine id in %q: %v", head, err)
		}
		ids[id] = true
	}
	return ids
}

// stableWorkers returns a set two readings apart agree on, so a worker already
// winding down from an earlier test is not taken as standing — a baseline
// taken on one sample is a baseline nothing can come back to. Falls back to
// the last reading when nothing settles in budget.
func stableWorkers(t *testing.T, frame string) map[int]bool {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	prev := goroutinesCarrying(t, frame)
	for time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
		got := goroutinesCarrying(t, frame)
		if maps.Equal(got, prev) {
			return got
		}
		prev = got
	}
	return prev
}

// ownWorkers polls until at least one goroutine this test's service started —
// one the ambient set does not hold — carries frame, or the budget runs out.
// That set is the one the stop verdict is drawn from, so an empty set is a
// defeated premise, never a pass.
func ownWorkers(t *testing.T, frame string, ambient map[int]bool) map[int]bool {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		own := map[int]bool{}
		for id := range goroutinesCarrying(t, frame) {
			if !ambient[id] {
				own[id] = true
			}
		}
		if len(own) > 0 || !time.Now().Before(deadline) {
			return own
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// settleWorkersGone polls until no id of ids still carries frame — a stopped
// goroutine returns shortly after its signal, not at the instant of it.
// Returns the surviving subset.
func settleWorkersGone(t *testing.T, frame string, ids map[int]bool) map[int]bool {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		survivors := map[int]bool{}
		for id := range goroutinesCarrying(t, frame) {
			if ids[id] {
				survivors[id] = true
			}
		}
		if len(survivors) == 0 || !time.Now().Before(deadline) {
			return survivors
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// StopBackground is the seam a caller that REPLACES a service without
// draining it needs (the studio's project hot-swap). Both halves are
// load-bearing: every periodic worker must stop, and the runs already in
// flight must be left strictly alone — a Drain-shaped stop would cancel the
// engines that are still writing to the store they started on.
func TestStopBackground_StopsEveryWorkerAndLeavesRunsAlone(t *testing.T) {
	// Who is standing BEFORE this test's service exists. Goroutine ids are
	// never reused, so every worker this service starts is one these sets do
	// not hold — and an ambient worker leaving while the stop runs cannot
	// mask a survivor, the way a COUNT returning to baseline could (the
	// substring-count sibling of this verdict was green on exactly that:
	// the service's own worker still scanning, one of another service's
	// gone, the count back where it started).
	ambient := map[string]map[int]bool{
		pipelineSchedulerFrame: stableWorkers(t, pipelineSchedulerFrame),
		alertPollFrame:         stableWorkers(t, alertPollFrame),
	}
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

	// The SERVED pair: this service, and the workers IT started. The verdict
	// below is drawn from these ids — a survivor must be one of them to
	// convict, so the pair is identified before anything is stopped, and its
	// being empty fails the premise rather than passing the stop.
	own := map[string]map[int]bool{}
	for _, frame := range []string{pipelineSchedulerFrame, alertPollFrame} {
		own[frame] = ownWorkers(t, frame, ambient[frame])
		if len(own[frame]) == 0 {
			t.Fatalf("premise: no %q worker of this test's own service is live — the set the verdict is drawn from would be empty", frame)
		}
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
	}{
		{"pipeline scheduler", pipelineSchedulerFrame},
		{"alert stall poll", alertPollFrame},
	} {
		if survivors := settleWorkersGone(t, w.frame, own[w.frame]); len(survivors) > 0 {
			t.Errorf("%s: goroutine(s) %v this service started still live after StopBackground — the worker is still scanning a store the caller has re-pointed away from",
				w.kind, slices.Sorted(maps.Keys(survivors)))
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
