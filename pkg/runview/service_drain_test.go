package runview

import (
	"context"
	"errors"
	"testing"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
)

// TestService_Drain_FlipsStatusAndEmitsEvent installs two fake run
// goroutines that respond to context cancellation, then verifies that
// Drain:
//   - cancels each handle,
//   - flips persisted run status to failed_resumable,
//   - emits a run_interrupted event in events.jsonl,
//   - causes subsequent Launch / Resume to return ErrServerDraining.
func TestService_Drain_FlipsStatusAndEmitsEvent(t *testing.T) {
	dir := t.TempDir()
	logger := iterlog.Nop()

	svc, err := NewService(dir, WithLogger(logger))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	// Seed two runs in `running` status and register them with the
	// manager + a goroutine that exits cleanly on cancel — mimicking
	// what spawnRun's body would do.
	ids := []string{"run-drain-1", "run-drain-2"}
	for _, id := range ids {
		if _, err := svc.store.CreateRun(context.Background(), id, "wf", nil); err != nil {
			t.Fatalf("CreateRun %s: %v", id, err)
		}
		ctx, regErr := svc.manager.Register(context.Background(), id)
		if regErr != nil {
			t.Fatalf("Register %s: %v", id, regErr)
		}
		go func(rid string, c context.Context) {
			<-c.Done()
			svc.manager.Deregister(rid)
		}(id, ctx)
	}

	drainCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	svc.Drain(drainCtx)

	for _, id := range ids {
		r, err := svc.store.LoadRun(context.Background(), id)
		if err != nil {
			t.Fatalf("LoadRun %s: %v", id, err)
		}
		if r.Status != store.RunStatusFailedResumable {
			t.Errorf("%s: status = %q, want %q", id, r.Status, store.RunStatusFailedResumable)
		}
		evts, err := svc.store.LoadEvents(context.Background(), id)
		if err != nil {
			t.Fatalf("LoadEvents %s: %v", id, err)
		}
		var sawInterrupted bool
		for _, e := range evts {
			if e.Type == store.EventRunInterrupted {
				sawInterrupted = true
				break
			}
		}
		if !sawInterrupted {
			t.Errorf("%s: no run_interrupted event in events.jsonl", id)
		}
	}

	// Subsequent Launch / Resume must return ErrServerDraining.
	if _, err := svc.Launch(context.Background(), LaunchSpec{FilePath: "irrelevant.iter"}); !errors.Is(err, runtime.ErrServerDraining) {
		t.Errorf("Launch after Drain = %v, want ErrServerDraining", err)
	}
	if _, err := svc.Resume(context.Background(), ResumeSpec{RunID: "run-drain-1", FilePath: "irrelevant.iter"}); !errors.Is(err, runtime.ErrServerDraining) {
		t.Errorf("Resume after Drain = %v, want ErrServerDraining", err)
	}
}

// TestService_Drain_DeadlineExceededStillFlipsStatus verifies that
// even when a fake goroutine refuses to honour the cancel signal
// within the drain budget, the persisted status still flips so the
// next server boot can offer one-click resume.
func TestService_Drain_DeadlineExceededStillFlipsStatus(t *testing.T) {
	dir := t.TempDir()
	logger := iterlog.Nop()

	svc, err := NewService(dir, WithLogger(logger))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	const id = "run-stuck"
	if _, err := svc.store.CreateRun(context.Background(), id, "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if _, regErr := svc.manager.Register(context.Background(), id); regErr != nil {
		t.Fatalf("Register: %v", regErr)
	}
	// Deliberately do NOT spawn a goroutine that calls Deregister —
	// mimicking a wedged subprocess. The handle's done channel never
	// closes within the test deadline, so Drain hits its ctx.Done()
	// path and must still flip status.
	t.Cleanup(func() { svc.manager.Deregister(id) })

	drainCtx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	svc.Drain(drainCtx)

	r, err := svc.store.LoadRun(context.Background(), id)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if r.Status != store.RunStatusFailedResumable {
		t.Errorf("status = %q, want failed_resumable (drain must flip even on deadline-exceeded)", r.Status)
	}
}

// TestService_Stop_DoesNotFlipStatus is the deliberate inverse of the
// Drain test: callers that want a quiet teardown (e.g. test fixtures)
// pick Stop and accept that the on-disk state is whatever the engine
// wrote. We don't want a regression where Stop accidentally picks up
// Drain's bookkeeping behaviour and starts mutating run state during
// non-shutdown teardowns.
func TestService_Stop_DoesNotFlipStatus(t *testing.T) {
	dir := t.TempDir()
	logger := iterlog.Nop()

	svc, err := NewService(dir, WithLogger(logger))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	const id = "run-stop"
	if _, err := svc.store.CreateRun(context.Background(), id, "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	ctx, regErr := svc.manager.Register(context.Background(), id)
	if regErr != nil {
		t.Fatalf("Register: %v", regErr)
	}
	go func() {
		<-ctx.Done()
		svc.manager.Deregister(id)
	}()

	stopCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	svc.Stop(stopCtx)

	r, err := svc.store.LoadRun(context.Background(), id)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if r.Status != store.RunStatusRunning {
		t.Errorf("status = %q, want running (Stop must not flip)", r.Status)
	}

	// Stop must NOT set the draining flag — the service should remain
	// usable for new launches in tests that compose multiple Stop/Start
	// cycles around a single Service.
	if svc.draining.Load() {
		t.Errorf("draining flag set after Stop, want false")
	}
}
