package runview

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// gatedReconcileStore parks the periodic reconcile inside its scan and
// records every status write that lands after the service has reported
// itself stopped. The boot scan is the first ListRuns; the tick's scan is
// the second, and only that one is gated.
type gatedReconcileStore struct {
	store.RunStore
	scans     atomic.Int64
	entered   chan struct{}
	release   chan struct{}
	stopped   atomic.Bool
	lateWrite atomic.Bool
}

func (g *gatedReconcileStore) ListRuns(ctx context.Context) ([]string, error) {
	if g.scans.Add(1) == 2 {
		close(g.entered)
		<-g.release
	}
	return g.RunStore.ListRuns(ctx)
}

func (g *gatedReconcileStore) UpdateRunStatusCoded(ctx context.Context, id string, status store.RunStatus, runErr string, code store.FailureCode) error {
	if g.stopped.Load() {
		g.lateWrite.Store(true)
	}
	return g.RunStore.UpdateRunStatusCoded(ctx, id, status, runErr, code)
}

// TestStopWaitsForInFlightReconcile: Stop must not return while the
// reconcile goroutine is still scanning. A shutdown that only closes the
// stop channel leaves the reconciler mid-scan, and its next write lands in
// a store the caller already considers settled — on the server that is a
// status flip racing the drain, in a test it is a write into a TempDir
// being removed.
func TestStopWaitsForInFlightReconcile(t *testing.T) {
	t.Setenv("ITERION_ORPHAN_RECONCILE_INTERVAL", "20ms")
	dir := t.TempDir()
	logger := iterlog.Nop()

	base, err := store.New(dir, store.WithLogger(logger))
	if err != nil {
		t.Fatalf("base store: %v", err)
	}
	gated := &gatedReconcileStore{
		RunStore: base,
		entered:  make(chan struct{}),
		release:  make(chan struct{}),
	}
	svc, err := NewService(dir, WithLogger(logger), WithStore(gated))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	select {
	case <-gated.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the periodic reconcile never entered its scan")
	}
	// Seeded while the tick's scan is parked at its entry: the boot scan is
	// already past, so this orphan is resolved by the parked scan alone.
	const id = "run-inflight-orphan"
	if _, err := base.CreateRun(context.Background(), id, "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	backdateRun(t, base, id)

	// Released while Stop is in flight: a Stop that waits observes every
	// write of that scan, a Stop that only signals returns before them.
	go func() {
		time.Sleep(100 * time.Millisecond)
		close(gated.release)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	svc.Stop(ctx)
	gated.stopped.Store(true)

	// Give the (un)awaited goroutine every chance to write.
	time.Sleep(500 * time.Millisecond)
	if gated.lateWrite.Load() {
		t.Fatal("the reconcile wrote a run status after Stop returned — the shutdown does not await the scan it interrupted")
	}
}

// TestDrainWaitsForInFlightReconcile: the production shutdown path is
// Drain, not Stop, and carries the same promise — after it returns the
// store belongs to no one.
func TestDrainWaitsForInFlightReconcile(t *testing.T) {
	t.Setenv("ITERION_ORPHAN_RECONCILE_INTERVAL", "20ms")
	dir := t.TempDir()
	logger := iterlog.Nop()

	base, err := store.New(dir, store.WithLogger(logger))
	if err != nil {
		t.Fatalf("base store: %v", err)
	}
	gated := &gatedReconcileStore{
		RunStore: base,
		entered:  make(chan struct{}),
		release:  make(chan struct{}),
	}
	svc, err := NewService(dir, WithLogger(logger), WithStore(gated))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	select {
	case <-gated.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the periodic reconcile never entered its scan")
	}
	const id = "run-inflight-orphan-drain"
	if _, err := base.CreateRun(context.Background(), id, "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	backdateRun(t, base, id)

	go func() {
		time.Sleep(100 * time.Millisecond)
		close(gated.release)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	svc.Drain(ctx)
	gated.stopped.Store(true)

	time.Sleep(500 * time.Millisecond)
	if gated.lateWrite.Load() {
		t.Fatal("the reconcile wrote a run status after Drain returned — the shutdown does not await the scan it interrupted")
	}
}

// TestStopBoundsTheReconcileWaitOnItsContext: an awaited scan must not
// turn the shutdown into a hang. Stop returns when the caller's context
// expires, whatever the reconcile is doing.
func TestStopBoundsTheReconcileWaitOnItsContext(t *testing.T) {
	t.Setenv("ITERION_ORPHAN_RECONCILE_INTERVAL", "20ms")
	dir := t.TempDir()
	logger := iterlog.Nop()

	base, err := store.New(dir, store.WithLogger(logger))
	if err != nil {
		t.Fatalf("base store: %v", err)
	}
	gated := &gatedReconcileStore{
		RunStore: base,
		entered:  make(chan struct{}),
		release:  make(chan struct{}),
	}
	svc, err := NewService(dir, WithLogger(logger), WithStore(gated))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	select {
	case <-gated.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the periodic reconcile never entered its scan")
	}
	defer close(gated.release) // never released: the scan stays parked

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	svc.Stop(ctx)
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("Stop waited %v on a 200ms context — the wait is unbounded", elapsed)
	}
}
