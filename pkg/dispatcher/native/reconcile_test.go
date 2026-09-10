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

// setSeam installs a package seam for one test and puts the previous
// value back afterwards. Every seam is an atomic (see fireSeam) because
// the goroutines that read them belong to a Store and outlive the test
// that made it — a bare assignment here races the watcher loop of a store
// an earlier test left running.
func setSeam[T any](t *testing.T, p *atomic.Pointer[T], v T) {
	t.Helper()
	prev := p.Swap(&v)
	t.Cleanup(func() { p.Store(prev) })
}

// setRescanInterval pins the fallback net's interval for one test; 0
// disables the net.
func setRescanInterval(t *testing.T, d time.Duration) {
	t.Helper()
	setSeam(t, &rescanIntervalOverride, d)
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
	setSeam(t, &reconcileScanning, func(*Store) {
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
	})
	setSeam(t, &reconcileScanned, func(*Store) {
		scannedOnce.Do(func() {
			iss, err := s.Create(Issue{Title: "Landed after the scan, before the swap", State: "backlog"})
			if err != nil {
				t.Fatalf("Create after the scan: %v", err)
			}
			afterScan = iss
		})
	})

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
	w, _, werr := s.watchState()
	if w == nil {
		t.Skipf("this host refused a watch (%v); the overflow path needs one", werr)
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

	w.w.Errors <- fsnotify.ErrEventOverflow

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
	if off.rescanner != nil {
		t.Fatal("ITERION_NATIVE_INDEX_RESCAN=off set before NewStore was not honoured: the interval was resolved at package init")
	}

	t.Setenv("ITERION_NATIVE_INDEX_RESCAN", "50ms")
	on, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = on.Close() })
	if on.rescanner == nil || on.rescanner.interval != 50*time.Millisecond {
		t.Fatalf("ITERION_NATIVE_INDEX_RESCAN=50ms not honoured: rescanner=%+v", on.rescanner)
	}
}
