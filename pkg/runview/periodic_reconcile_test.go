package runview

import (
	"context"
	"testing"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// TestPeriodicReconcileFlipsLateOrphan proves the TICK (not the boot
// scan) reconciles: a run that goes orphan AFTER the service is
// constructed — a CLI run sharing the store whose process crashed —
// flips to failed within the tick budget instead of staying `running`
// until the next server restart.
func TestPeriodicReconcileFlipsLateOrphan(t *testing.T) {
	t.Setenv("ITERION_ORPHAN_RECONCILE_INTERVAL", "20ms")
	dir := t.TempDir()
	logger := iterlog.Nop()

	svc, err := NewService(dir, WithLogger(logger))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	defer svc.Stop(context.Background())

	// Created AFTER the boot scan: status=running, no flock held, no .pid
	// — exactly what a crashed CLI run leaves behind.
	seed, err := store.New(dir, store.WithLogger(logger))
	if err != nil {
		t.Fatalf("seed store: %v", err)
	}
	const id = "run-late-orphan"
	if _, err := seed.CreateRun(context.Background(), id, "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	backdateRun(t, seed, id)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		r, err := svc.store.LoadRun(context.Background(), id)
		if err == nil && r.Status != store.RunStatusRunning {
			if r.Status != store.RunStatusFailed {
				t.Fatalf("status = %q, want failed (no checkpoint)", r.Status)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("periodic reconcile never flipped the late orphan (still `running` after 5s)")
}

// TestPeriodicReconcileStopsOnTeardown pins the goroutine lifecycle:
// after Drain (and a redundant Stop — both may run in one teardown) no
// further reconcile happens and nothing panics.
func TestPeriodicReconcileStopsOnTeardown(t *testing.T) {
	t.Setenv("ITERION_ORPHAN_RECONCILE_INTERVAL", "10ms")
	dir := t.TempDir()
	logger := iterlog.Nop()

	svc, err := NewService(dir, WithLogger(logger))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	drainCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	svc.Drain(drainCtx)
	svc.Stop(context.Background()) // double-teardown must not panic

	// A run going orphan after teardown stays untouched.
	seed, err := store.New(dir, store.WithLogger(logger))
	if err != nil {
		t.Fatalf("seed store: %v", err)
	}
	const id = "run-post-teardown"
	if _, err := seed.CreateRun(context.Background(), id, "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	time.Sleep(100 * time.Millisecond) // several would-be ticks
	r, err := svc.store.LoadRun(context.Background(), id)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if r.Status != store.RunStatusRunning {
		t.Fatalf("status = %q — reconcile ran after teardown", r.Status)
	}
}

// TestOrphanReconcileIntervalEnv pins the env contract: unset → default,
// parseable → parsed, "0" → disabled, junk → default.
func TestOrphanReconcileIntervalEnv(t *testing.T) {
	t.Setenv("ITERION_ORPHAN_RECONCILE_INTERVAL", "")
	if got := orphanReconcileInterval(); got != defaultOrphanReconcileInterval {
		t.Fatalf("unset = %v, want default %v", got, defaultOrphanReconcileInterval)
	}
	t.Setenv("ITERION_ORPHAN_RECONCILE_INTERVAL", "5m")
	if got := orphanReconcileInterval(); got != 5*time.Minute {
		t.Fatalf("5m = %v", got)
	}
	t.Setenv("ITERION_ORPHAN_RECONCILE_INTERVAL", "0")
	if got := orphanReconcileInterval(); got > 0 {
		t.Fatalf("0 = %v, want disabled (<= 0)", got)
	}
	t.Setenv("ITERION_ORPHAN_RECONCILE_INTERVAL", "junk")
	if got := orphanReconcileInterval(); got != defaultOrphanReconcileInterval {
		t.Fatalf("junk = %v, want default", got)
	}
}
