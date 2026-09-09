package native

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// setWatchCheckInterval pins how often the watcher loop asks fsnotify
// whether its watch still exists.
func setWatchCheckInterval(t *testing.T, d time.Duration) {
	t.Helper()
	seamMu.Lock()
	prev := watchCheckIntervalOverride
	watchCheckIntervalOverride = &d
	seamMu.Unlock()
	t.Cleanup(func() {
		seamMu.Lock()
		watchCheckIntervalOverride = prev
		seamMu.Unlock()
	})
}

// writeExternal drops an issue file under issues/ behind the store's
// back, the way `iterion __mcp-board` does.
func writeExternal(t *testing.T, dir, id, title string) {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Second)
	iss := Issue{ID: id, Title: title, State: "backlog", CreatedAt: now, UpdatedAt: now}
	data, err := json.MarshalIndent(&iss, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, issuesDir, encodeID(id)+".json"), data, filePerm); err != nil {
		t.Fatalf("write external issue: %v", err)
	}
}

// The issues/ directory going away under a running store is an error
// that leaves the index as it was — never an empty board swapped in
// without a word, which is what a "missing directory = empty board"
// reading did on exactly the host the net runs on.
func TestReconcile_KeepsTheIndexWhenTheIssuesDirectoryVanishes(t *testing.T) {
	refuseWatch(t)
	setRescanInterval(t, 0)

	dir := t.TempDir()
	s, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	for i := 0; i < 3; i++ {
		if _, err := s.Create(Issue{Title: fmt.Sprintf("card %d", i), State: "backlog"}); err != nil {
			t.Fatalf("Create: %v", err)
		}
	}
	if err := os.RemoveAll(filepath.Join(dir, issuesDir)); err != nil {
		t.Fatal(err)
	}
	if err := s.Reconcile(); err == nil {
		t.Fatal("a vanished issues/ directory was read as an empty board, not reported")
	}
	got, err := s.List(ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("the board was wiped by a vanished directory: %d cards left", len(got))
	}
}

// The loss that really happens: the watched directory is removed or
// replaced, the kernel drops the inotify watch (IN_IGNORED) and fsnotify
// forgets it without closing a channel or sending an error. The loop
// notices from fsnotify's own watch list and hands the store to the net.
func TestWatcher_ArmsTheNetWhenTheKernelDropsTheWatch(t *testing.T) {
	setRescanInterval(t, 20*time.Millisecond)
	setWatchCheckInterval(t, 20*time.Millisecond)

	dir := t.TempDir()
	s, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if watcher, _, watcherErr := watchState(s); watcher == nil {
		t.Skipf("this host refused a watch (%v); a watch cannot be lost", watcherErr)
	}

	issues := filepath.Join(dir, issuesDir)
	if err := os.RemoveAll(issues); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(issues, dirPerm); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		watcher, rescanner, watcherErr := watchState(s)
		if watcher == nil && rescanner != nil && watcherErr == errWatchLost {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	watcher, rescanner, _ := watchState(s)
	if armed := watcher == nil && rescanner != nil; !armed {
		t.Fatal("the kernel dropped the watch and the store still believes its fast path is armed: blind until restart")
	}

	writeExternal(t, dir, "native:after-the-loss", "Seen by the net")
	deadline = time.Now().Add(fastPathBudget)
	for time.Now().Before(deadline) {
		if _, err := s.Get("native:after-the-loss"); err == nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("after the kernel dropped the watch, an out-of-process create never became visible")
}

// A watch lost while the store is closing arms nothing, and a net armed
// just before Close does not outlive it.
func TestClose_LeavesNoNetRunningAfterALostWatch(t *testing.T) {
	setRescanInterval(t, 20*time.Millisecond)
	setWatchCheckInterval(t, 2*time.Millisecond)

	for i := 0; i < 30; i++ {
		dir := t.TempDir()
		s, err := NewStore(dir)
		if err != nil {
			t.Fatalf("NewStore: %v", err)
		}
		if watcher, _, watcherErr := watchState(s); watcher == nil {
			// Close BEFORE skipping. A refused watch is exactly when a
			// store arms a rescan ticker, and this test pins it at 20ms;
			// skipping out with it running leaks a ticker that outlives
			// the test and races the next one's seam setters.
			_ = s.Close()
			t.Skipf("this host refused a watch (%v)", watcherErr)
		}
		go func() { _ = os.RemoveAll(filepath.Join(dir, issuesDir)) }()
		time.Sleep(time.Duration(i%7) * time.Millisecond)
		if err := s.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		_, r, _ := watchState(s)
		if r != nil {
			select {
			case <-r.done:
			default:
				t.Fatalf("iteration %d: a rescanner armed by the lost watch is still running after Close", i)
			}
		}
	}

	// And a loss reported after Close arms nothing at all.
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	iw, _, watcherErr := watchState(s)
	if iw == nil {
		_ = s.Close() // as above: a refused watch means a ticker is running
		t.Skipf("this host refused a watch (%v)", watcherErr)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	s.watchLost(iw)
	if _, r, _ := watchState(s); r != nil {
		t.Fatal("a loss reported after Close armed a net")
	}
}

// Under sustained in-process writes the rebuild still scans exactly once
// per call — no retry, no fall-back to a scan under the store mutex — and
// loses no card.
func TestReconcile_ScansOnceWhateverTheWriteLoad(t *testing.T) {
	refuseWatch(t)
	setRescanInterval(t, 0)

	dir := t.TempDir()
	s, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ids := make([]string, 0, 50)
	for i := 0; i < 50; i++ {
		iss, err := s.Create(Issue{Title: fmt.Sprintf("card %d", i), State: "backlog"})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		ids = append(ids, iss.ID)
	}

	var scans atomic.Int32
	setScanHooks(t, func(st *Store) {
		if st == s { // another test's store may still be ticking down its Cleanup
			scans.Add(1)
		}
	}, nil)

	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for n := 0; ; n++ {
			select {
			case <-stop:
				return
			default:
			}
			title := fmt.Sprintf("edit %d", n)
			if _, err := s.Update(ids[n%len(ids)], Patch{Title: &title}); err != nil {
				t.Errorf("Update: %v", err)
				return
			}
			time.Sleep(200 * time.Microsecond)
		}
	}()
	const calls = 5
	for i := 0; i < calls; i++ {
		if err := s.Reconcile(); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
	}
	close(stop)
	<-done

	if got := scans.Load(); got != calls {
		t.Fatalf("%d Reconcile calls under write load performed %d scans; want exactly one scan per call", calls, got)
	}
	got, err := s.List(ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(ids) {
		t.Fatalf("cards lost under write load: %d of %d left", len(got), len(ids))
	}
}

// The "cards could not be read" warning is written when the set changes,
// not on every tick of a two-second net over one corrupt file.
func TestReconcile_WarnsAboutUnreadableCardsWhenTheSetChanges(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a 000 file; the unreadable case cannot be staged")
	}
	refuseWatch(t)
	setRescanInterval(t, 0)

	dir := t.TempDir()
	s, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	var buf bytes.Buffer
	s.SetLogger(iterlog.NewFallback(iterlog.LevelWarn, &buf))

	a, err := s.Create(Issue{Title: "a", State: "backlog"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.Create(Issue{Title: "b", State: "backlog"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Chmod(s.issuePath(a.ID), filePerm)
		_ = os.Chmod(s.issuePath(b.ID), filePerm)
	})

	count := func() int { return strings.Count(buf.String(), "could not be read") }
	if err := os.Chmod(s.issuePath(a.ID), 0); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if err := s.Reconcile(); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
	}
	if got := count(); got != 1 {
		t.Fatalf("one unreadable card over five rescans produced %d warnings, want 1\n%s", got, buf.String())
	}
	if err := os.Chmod(s.issuePath(b.ID), 0); err != nil {
		t.Fatal(err)
	}
	if err := s.Reconcile(); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got := count(); got != 2 {
		t.Fatalf("a second unreadable card did not produce a new warning (%d)", got)
	}
	_ = os.Chmod(s.issuePath(a.ID), filePerm)
	_ = os.Chmod(s.issuePath(b.ID), filePerm)
	if err := s.Reconcile(); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !strings.Contains(buf.String(), "reads again") {
		t.Fatalf("recovery was not reported\n%s", buf.String())
	}
}

// The rebuild a kernel-queue overflow asks for runs off the watcher
// goroutine: fsnotify's event channel is unbuffered, so a rebuild ON the
// loop would stop draining the very queue that just overflowed for the
// whole of the scan. While the rebuild is stalled, the fast path still
// delivers.
func TestWatcher_OverflowRebuildDoesNotStallTheEventLoop(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	watcher, _, watcherErr := watchState(s)
	if watcher == nil {
		// The store's own cleanup is only registered further down, past
		// the scan hooks this skip never reaches — close it here or a
		// refused watch leaks the store and its rescan ticker.
		_ = s.Close()
		t.Skipf("this host refused a watch (%v); the overflow path needs one", watcherErr)
	}

	stall := make(chan struct{})
	setScanHooks(t, func(st *Store) {
		if st == s { // stall only this store's rebuild
			<-stall
		}
	}, nil)
	release := func() {
		select {
		case <-stall:
		default:
			close(stall)
		}
	}
	t.Cleanup(func() {
		release()
		for s.rebuildInFlight() {
			time.Sleep(time.Millisecond)
		}
		_ = s.Close()
	})

	watcher.w.Errors <- fsnotify.ErrEventOverflow
	// The rebuild is now parked in its scan. The fast path must still work.
	writeExternal(t, dir, "native:during-the-rebuild", "Delivered while the rebuild is stalled")
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		_, ok := s.index["native:during-the-rebuild"]
		s.mu.Unlock()
		if ok {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the event loop stopped delivering while the overflow rebuild ran")
}
