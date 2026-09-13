package native

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// A request that arrives after the rebuild goroutine's last check and
// before it releases the pending flag used to be lost: it failed its own
// claim and relied on a pass that was already leaving. The goroutine now
// looks once more after releasing, and re-claims.
func TestRebuildAsync_DoesNotLoseARequestAtTheEndOfAPass(t *testing.T) {
	refuseWatch(t)
	setRescanInterval(t, 0)
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	var scans atomic.Int32
	setSeam(t, &reconcileScanning, func(st *Store) {
		if st == s {
			scans.Add(1)
		}
	})

	var late atomic.Bool
	setSeam(t, &rebuildPassEnding, func(st *Store) {
		if st == s && late.CompareAndSwap(false, true) {
			s.rebuildAsync("arrived as the pass was ending") // the lost-wakeup window
		}
	})

	s.rebuildAsync("first")
	waitRebuildIdle(t, s)
	if got := scans.Load(); got != 2 {
		t.Fatalf("a request that arrived as the pass was ending produced %d scans in total, want 2", got)
	}
	if s.rebuildRerun.Load() {
		t.Fatal("the rerun flag is left set with no goroutine to honour it")
	}
}

// Nothing of a store runs after Close returns: a rebuild in flight is
// waited for, one asked for afterwards returns at once.
func TestClose_WaitsForARebuildInFlightAndRefusesALaterOne(t *testing.T) {
	refuseWatch(t)
	setRescanInterval(t, 0)
	dir := t.TempDir()
	s, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if _, err := s.Create(Issue{Title: "kept", State: "backlog"}); err != nil {
		t.Fatal(err)
	}

	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	setSeam(t, &reconcileScanning, func(st *Store) {
		if st == s {
			select {
			case entered <- struct{}{}:
			default:
			}
			<-release
		}
	})

	s.rebuildAsync("in flight at Close")
	<-entered
	closed := make(chan struct{})
	go func() {
		defer close(closed)
		_ = s.Close()
	}()
	select {
	case <-closed:
		t.Fatal("Close returned while a rebuild was still scanning")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close never returned after the rebuild finished")
	}
	waitRebuildIdle(t, s)

	// After Close, a scan is refused before it touches the disk — and says
	// so: a caller must not read "the index is fresh" from a nil.
	setSeam(t, &reconcileScanning, func(st *Store) {
		if st == s {
			t.Error("a scan ran on a closed store")
		}
	})
	if err := s.Reconcile(); !errors.Is(err, ErrStoreClosed) {
		t.Fatalf("Reconcile on a closed store returned %v, want ErrStoreClosed", err)
	}
}

// A locked rebuild (the panic-recovery path) that lands while an unlocked
// scan is in flight is NEWER than that scan: the scan's swap must not put
// the index back to before it.
func TestReconcile_DoesNotLandAnOlderScanOverTheLockedRebuild(t *testing.T) {
	refuseWatch(t)
	setRescanInterval(t, 0)
	dir := t.TempDir()
	s, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	var once atomic.Bool
	setSeam(t, &reconcileScanned, func(st *Store) {
		if st != s || !once.CompareAndSwap(false, true) {
			return
		}
		// Between the scan and the swap: a card appears on disk and the
		// locked rebuild (as recoverMutator would run it) picks it up.
		writeExternal(t, dir, "native:seen-by-the-locked-rebuild", "Newer than the scan")
		s.mu.Lock()
		err := s.reconcileLocked()
		s.mu.Unlock()
		if err != nil {
			t.Errorf("reconcileLocked: %v", err)
		}
	})

	if err := s.Reconcile(); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if _, err := s.Get("native:seen-by-the-locked-rebuild"); err != nil {
		t.Fatalf("the older unlocked scan landed over the locked rebuild and dropped the card: %v", err)
	}
}

// Close waits for the rebuild GOROUTINE, not only for the scan inside it.
// Its work does not end when Reconcile returns: there is a tail — fire the
// pass-ending seam, release the pending flag, look once more for a request
// that raced it — and reconcileMu, which Reconcile releases on its way out,
// says nothing about that. Waiting on reconcileMu alone let Close return
// with the goroutine still running, which is how a test's own hook gets
// called after the test completed (a t.Errorf from a dead test panics the
// binary) and how a leak checker sees a goroutine a closed store still owns.
func TestClose_WaitsForTheRebuildGoroutineNotOnlyItsScan(t *testing.T) {
	refuseWatch(t)
	setRescanInterval(t, 0)
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	inTail := make(chan struct{}, 1)
	release := make(chan struct{})
	var once atomic.Bool
	setSeam(t, &rebuildPassEnding, func(st *Store) {
		if st != s || !once.CompareAndSwap(false, true) {
			return
		}
		inTail <- struct{}{}
		<-release
	})

	s.rebuildAsync("in flight at Close")
	<-inTail // the scan is over and reconcileMu is released; the goroutine lives on

	closed := make(chan struct{})
	go func() {
		defer close(closed)
		_ = s.Close()
	}()
	select {
	case <-closed:
		t.Fatal("Close returned while the rebuild goroutine was still running its tail")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close never returned after the rebuild goroutine finished")
	}

	// And a request that arrives after Close starts no goroutine at all —
	// it must also hand back the pending flag it claimed, or the store
	// would look forever busy to anything watching it.
	setSeam(t, &reconcileScanning, func(st *Store) {
		if st == s {
			t.Error("a scan ran on a closed store")
		}
	})
	s.rebuildAsync("after Close")
	if s.rebuildPending.Load() {
		t.Fatal("a rebuild refused after Close left the pending flag set")
	}
}
