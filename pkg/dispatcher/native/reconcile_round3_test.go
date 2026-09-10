package native

import (
	"sync/atomic"
	"testing"
	"time"
)

// waitRebuildIdle blocks until no overflow rebuild is pending.
func waitRebuildIdle(t *testing.T, s *Store) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for s.rebuildPending.Load() {
		if time.Now().After(deadline) {
			t.Fatal("the overflow rebuild never finished")
		}
		time.Sleep(time.Millisecond)
	}
}

// An update the watcher applies while an overflow rebuild has scanned and
// not yet swapped is marked dirty like an in-process write, so the swap
// keeps it: the scan read the directory before the event's file existed,
// and without the mark the swap would put the index back to before it.
func TestApplyEvent_DuringARebuildIsNotReverted(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if w, _, werr := s.watchState(); w == nil {
		t.Skipf("this host refused a watch (%v); the overflow path needs one", werr)
	}

	scanned := make(chan struct{})
	release := make(chan struct{})
	var once atomic.Bool
	setSeam(t, &reconcileScanned, func(st *Store) {
		if st == s && once.CompareAndSwap(false, true) {
			close(scanned)
			<-release
		}
	})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
		waitRebuildIdle(t, s)
		_ = s.Close()
	})

	s.rebuildAsync("test")
	<-scanned // the scan is done and saw no such file; the swap waits

	// The event the watcher applies in between.
	writeExternal(t, dir, "native:applied-mid-rebuild", "Applied between the scan and the swap")
	deadline := time.Now().Add(fastPathBudget)
	for {
		s.mu.Lock()
		_, ok := s.index["native:applied-mid-rebuild"]
		s.mu.Unlock()
		if ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the fast path did not deliver the create while the rebuild waited")
		}
		time.Sleep(5 * time.Millisecond)
	}

	close(release)
	waitRebuildIdle(t, s)
	if _, err := s.Get("native:applied-mid-rebuild"); err != nil {
		t.Fatalf("the rebuild's swap reverted a card the watcher applied after the scan: %v", err)
	}
}

// A request that arrives while a rebuild runs is deferred to one more
// pass, never dropped: the running scan listed the directory before the
// change that prompted the request.
func TestRebuildAsync_RerunsForARequestThatArrivedMidPass(t *testing.T) {
	refuseWatch(t)
	setRescanInterval(t, 0)
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	var scans atomic.Int32
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	setSeam(t, &reconcileScanning, func(st *Store) {
		if st != s {
			return
		}
		if scans.Add(1) == 1 {
			entered <- struct{}{}
			<-release
		}
	})

	s.rebuildAsync("first")
	<-entered
	s.rebuildAsync("second, mid-pass") // must not be dropped
	close(release)
	waitRebuildIdle(t, s)
	if got := scans.Load(); got != 2 {
		t.Fatalf("a request that arrived mid-pass produced %d scans in total, want 2 (the running pass plus one more)", got)
	}
}

// An explicit disable in any spelling is honoured: "0s" and "-1" are not
// "unparsable, use the default".
func TestRescanInterval_HonoursEveryZeroSpelling(t *testing.T) {
	refuseWatch(t)
	for _, raw := range []string{"off", "0", "0s", "0ms", "-1", "-5s"} {
		t.Setenv("ITERION_NATIVE_INDEX_RESCAN", raw)
		s, err := NewStore(t.TempDir())
		if err != nil {
			t.Fatalf("NewStore: %v", err)
		}
		_, r, _ := s.watchState()
		_ = s.Close()
		if r != nil {
			t.Fatalf("ITERION_NATIVE_INDEX_RESCAN=%q armed a %s net; an explicit disable was replaced by the default", raw, r.interval)
		}
	}
	t.Setenv("ITERION_NATIVE_INDEX_RESCAN", "1500ms")
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if _, r, _ := s.watchState(); r == nil || r.interval != 1500*time.Millisecond {
		t.Fatalf("1500ms not honoured: %+v", r)
	}
}
