package native

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
)

// setRescanInterval pins the fallback net's interval for one test; 0
// disables the net.
func setRescanInterval(t *testing.T, d time.Duration) {
	t.Helper()
	seamMu.Lock()
	prev := rescanIntervalOverride
	rescanIntervalOverride = &d
	seamMu.Unlock()
	t.Cleanup(func() {
		seamMu.Lock()
		rescanIntervalOverride = prev
		seamMu.Unlock()
	})
}

// TestReconcile_DoesNotRevertAWriteThatRacedTheScan pins the two
// properties of the net's rebuild: the disk scan holds no store lock (or
// a Create could not land while it runs), and a write that lands after
// the scan and before the swap survives the swap (or the net would revert
// the daemon's own writes every tick on a degraded host).
func TestReconcile_DoesNotRevertAWriteThatRacedTheScan(t *testing.T) {
	refuseWatch(t)
	setRescanInterval(t, 0)

	dir := t.TempDir()
	s, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if _, err := s.Create(Issue{Title: "Before the scan", State: "backlog"}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	var duringScan, afterScan *Issue
	var scanningOnce, scannedOnce sync.Once
	scanning := func(*Store) {
		scanningOnce.Do(func() {
			// From another goroutine, bounded: a Create that cannot take
			// the lock while the scan runs is the failure named.
			done := make(chan struct{})
			go func() {
				defer close(done)
				iss, err := s.Create(Issue{Title: "Landed during the scan", State: "backlog"})
				if err != nil {
					t.Errorf("Create during the scan: %v", err)
					return
				}
				duringScan = iss
			}()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("a Create blocked while Reconcile was scanning: the scan holds the store mutex across its disk I/O")
			}
		})
	}
	scanned := func(*Store) {
		scannedOnce.Do(func() {
			iss, err := s.Create(Issue{Title: "Landed after the scan, before the swap", State: "backlog"})
			if err != nil {
				t.Fatalf("Create after the scan: %v", err)
			}
			afterScan = iss
		})
	}
	setScanHooks(t, scanning, scanned)

	if err := s.Reconcile(); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if duringScan == nil || afterScan == nil {
		t.Fatal("test setup: a scan hook did not run")
	}
	if _, err := s.Get(duringScan.ID); err != nil {
		t.Fatalf("the write that landed during the scan is not in the index: %v", err)
	}
	if _, err := s.Get(afterScan.ID); err != nil {
		t.Fatalf("the write that landed between the scan and the swap was reverted by the swap: %v", err)
	}
}

// TestReconcile_DoesNotRevertAWatcherUpdateThatRacedTheScan is the
// out-of-process half of the test above, and the regression guard for the
// case that actually ships: a kernel-queue overflow starts a Reconcile
// while the watch is STILL armed (rebuildAsync), the watcher loop keeps
// draining the events the overflow queued, and applyEvent writes them
// into the index during the scan's mutex-free window. Unless applyEvent
// marks those ids dirty the swap reverts both directions — a card created
// after the scan's ReadDir is dropped, a card the loop deleted is
// resurrected — and an armed-watch store has no rescan ticker to correct
// it afterwards.
//
// reconcileScanned is a deterministic sync point (Reconcile calls it
// after the scan, before the swap, with the store mutex released), so
// this pins the invariant with no timing window.
func TestReconcile_DoesNotRevertAWatcherUpdateThatRacedTheScan(t *testing.T) {
	refuseWatch(t)
	setRescanInterval(t, 0)

	dir := t.TempDir()
	s, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	// A card the scan WILL see, which the watcher loop then removes.
	doomed, err := s.Create(Issue{Title: "Removed by an event the overflow queued", State: "backlog"})
	if err != nil {
		t.Fatalf("Create doomed: %v", err)
	}

	const createdID = "native:created-after-the-readdir"
	var once sync.Once
	setScanHooks(t, nil, func(*Store) {
		once.Do(func() {
			// Exactly what the watcher loop does while the rebuild scans:
			// a create the ReadDir was too early to see...
			writeExternal(t, dir, createdID, "Created after the ReadDir")
			applyEvent(s, createdID, fsnotify.Create)
			// ...and a remove the scan was too early to see.
			if err := os.Remove(s.issuePath(doomed.ID)); err != nil {
				t.Errorf("remove doomed: %v", err)
				return
			}
			applyEvent(s, doomed.ID, fsnotify.Remove)
		})
	})

	if err := s.Reconcile(); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if _, err := s.Get(createdID); err != nil {
		t.Errorf("the swap dropped a card the watcher created during the scan — on an armed-watch store nothing rescans, so it stays invisible until restart: %v", err)
	}
	if _, err := s.Get(doomed.ID); err == nil {
		t.Error("the swap resurrected a card the watcher had removed during the scan")
	}
}

// TestRebuildAsync_DefersAnOverflowThatArrivesMidRebuild: coalescing must
// collapse N requests into one SUBSEQUENT pass, not swallow the ones that
// arrive mid-pass. The in-flight scan cannot cover them — its ReadDir
// happened before they did — and the events an overflow dropped are never
// resent, so a swallowed request leaves those cards stale until something
// else touches them or the daemon restarts. Consecutive overflows inside
// one ~4-19 ms scan are the expected shape of an issue import or a mass
// label pass, not an exotic race.
func TestRebuildAsync_DefersAnOverflowThatArrivesMidRebuild(t *testing.T) {
	// No watcher and no ticker: every scan this store performs is one
	// this test asked for, so the count below is exact.
	refuseWatch(t)
	setRescanInterval(t, 0)

	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	var scans atomic.Int32
	release := make(chan struct{})
	var releaseOnce sync.Once
	closeRelease := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(closeRelease)
	setScanHooks(t, func(st *Store) {
		if st != s {
			return // a store another test leaked may still be ticking
		}
		if scans.Add(1) == 1 {
			<-release // park the first rebuild inside its scan
		}
	}, nil)

	s.rebuildAsync("first overflow")
	deadline := time.Now().Add(fastPathBudget)
	for scans.Load() < 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if scans.Load() != 1 {
		t.Fatal("test setup: the first rebuild never reached its scan")
	}

	// Two more overflows arrive while that rebuild is parked. They must
	// collapse into one follow-up pass — not zero, and not two.
	s.rebuildAsync("second overflow")
	s.rebuildAsync("third overflow")
	closeRelease()

	deadline = time.Now().Add(fastPathBudget)
	for s.rebuildInFlight() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if s.rebuildInFlight() {
		t.Fatal("the rebuild goroutine never settled")
	}

	switch got := scans.Load(); got {
	case 2:
	case 1:
		t.Fatal("both overflows that arrived during the rebuild were dropped: no follow-up scan ran, and the events they signalled are never resent")
	default:
		t.Fatalf("two overflows during one rebuild produced %d scans; want exactly 2 (one in flight + one coalesced follow-up)", got)
	}
}

// TestWatcher_ReconcilesOnKernelQueueOverflow: a full kernel queue drops
// events that are never resent, so the overflow signal rebuilds the index
// from disk instead of leaving the board stale until the next event.
func TestWatcher_ReconcilesOnKernelQueueOverflow(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	watcher, _, watcherErr := watchState(s)
	if watcher == nil {
		t.Skipf("this host refused a watch (%v); the overflow path needs one", watcherErr)
	}

	now := time.Now().UTC().Truncate(time.Second)
	iss := Issue{ID: "native:dropped-by-overflow", Title: "Dropped", State: "backlog", CreatedAt: now, UpdatedAt: now}
	data, err := json.MarshalIndent(&iss, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, issuesDir, encodeID(iss.ID)+".json"), data, filePerm); err != nil {
		t.Fatalf("write external issue: %v", err)
	}
	waitForIndex(t, s, func() bool {
		_, ok := s.index[iss.ID]
		return ok
	}, "external create before the overflow")

	// The event the kernel dropped: the file is on disk, the index has
	// forgotten it.
	s.mu.Lock()
	delete(s.index, iss.ID)
	s.mu.Unlock()

	watcher.w.Errors <- fsnotify.ErrEventOverflow

	waitForIndex(t, s, func() bool {
		_, ok := s.index[iss.ID]
		return ok
	}, "index rebuilt after the overflow")
}

// TestRescanInterval_ReadsTheEnvironmentWhenTheNetStarts: the interval is
// resolved when a store arms its net, so a value set after process start
// (t.Setenv, an embedding process) is honoured — resolving it at package
// init would freeze whatever the environment held when the binary loaded.
func TestRescanInterval_ReadsTheEnvironmentWhenTheNetStarts(t *testing.T) {
	refuseWatch(t)

	t.Setenv("ITERION_NATIVE_INDEX_RESCAN", "off")
	off, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = off.Close() })
	if _, rescanner, _ := watchState(off); rescanner != nil {
		t.Fatal("ITERION_NATIVE_INDEX_RESCAN=off set before NewStore was not honoured: the interval was resolved at package init")
	}

	t.Setenv("ITERION_NATIVE_INDEX_RESCAN", "50ms")
	on, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = on.Close() })
	_, rescanner, _ := watchState(on)
	if rescanner == nil || rescanner.interval != 50*time.Millisecond {
		t.Fatalf("ITERION_NATIVE_INDEX_RESCAN=50ms not honoured: rescanner=%+v", rescanner)
	}
}
