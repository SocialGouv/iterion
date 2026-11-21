package alert

import (
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// waitParkedInBailClose returns once a goroutine sits in Bail.Close blocked on
// a mutex — the holder whose release races the manager's own.
func waitParkedInBailClose(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		for _, g := range strings.Split(string(buf[:n]), "\n\n") {
			header, _, _ := strings.Cut(g, "\n")
			if strings.Contains(header, "sync.Mutex.Lock") && strings.Contains(g, "alert.(*Bail).Close(") {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("fixture: Bail.Close never parked on the manager lock")
		}
		time.Sleep(time.Millisecond)
	}
}

// A holder closing its bail while the manager releases the run's bails under
// its lock (a service shutdown's ReleaseAll, a reap's ReleaseRun, a
// replacement) must not deadlock: the bail's closed state lives under the
// manager's lock alone, so neither side waits on a lock the other holds. The
// test holds the manager lock itself to park Close at the point the race
// reaches, then runs the release.
func TestBailCloseRacingReleaseNeverDeadlocks(t *testing.T) {
	m := NewManager()
	lane := newLane()
	b, err := m.Register("r", mustObs(t, "r", "a"), lane)
	if err != nil {
		t.Fatalf("register: %v", err)
	}

	m.mu.Lock()
	closed := make(chan struct{})
	go func() {
		b.Close()
		close(closed)
	}()
	waitParkedInBailClose(t)

	released := make(chan struct{})
	go func() {
		m.releaseAllLocked("r")
		close(released)
	}()
	select {
	case <-released:
	case <-time.After(5 * time.Second):
		t.Fatal("releasing the run's bails under the manager lock blocked behind a Close waiting for that lock: lock-order deadlock")
	}
	m.mu.Unlock()

	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Bail.Close never returned after the release")
	}
	if lane.closeCalls != 1 {
		t.Fatalf("lane closed %d times, want exactly once", lane.closeCalls)
	}
}

// parkingCloseLane is a recordingLane whose Close announces itself on
// closeEntered and waits for closeRelease before closing: the release that
// closes it is parked inside the manager's critical section.
type parkingCloseLane struct {
	*recordingLane
	closeEntered chan struct{}
	closeRelease chan struct{}
}

func (l *parkingCloseLane) Close() error {
	l.closeEntered <- struct{}{}
	<-l.closeRelease
	return l.recordingLane.Close()
}

// A holder closing its bail while a replacement registration closes the
// episode's old bails must not deadlock either: the replacement closes them
// through the same helper, under the manager's lock alone. The first old
// lane parks the replacement inside that lock, the second old bail's holder
// then closes it and parks on the lock, and the replacement resumes.
func TestBailCloseRacingReplacementNeverDeadlocks(t *testing.T) {
	m := NewManager()
	obsA, obsB := mustObs(t, "r", "a"), mustObs(t, "r", "b")
	first := &parkingCloseLane{recordingLane: newLane(), closeEntered: make(chan struct{}), closeRelease: make(chan struct{})}
	second := newLane()
	if _, err := m.Register("r", obsA, first); err != nil {
		t.Fatalf("register the first holder: %v", err)
	}
	secondBail, err := m.Register("r", obsA, second)
	if err != nil {
		t.Fatalf("register the second holder: %v", err)
	}

	replaced := make(chan error, 1)
	go func() {
		_, err := m.Register("r", obsB, newLane())
		replaced <- err
	}()
	select {
	case <-first.closeEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("the replacement never reached the first old lane's close: it blocked under the manager lock")
	}
	closed := make(chan struct{})
	go func() {
		secondBail.Close()
		close(closed)
	}()
	waitParkedInBailClose(t)
	close(first.closeRelease)

	select {
	case err := <-replaced:
		if err != nil {
			t.Fatalf("replacement: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the replacement closing the old bails blocked behind a Close waiting for the manager lock: lock-order deadlock")
	}
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Bail.Close never returned after the replacement")
	}
	if first.closeCalls != 1 || second.closeCalls != 1 {
		t.Fatalf("old lanes closed first=%d second=%d, want each exactly once", first.closeCalls, second.closeCalls)
	}
}

// Concurrent Close calls on one bail close its lane exactly once: the bail's
// closed state lives under the manager's lock, so the race detector sees
// every access ordered by it.
func TestBailConcurrentCloseClosesTheLaneOnce(t *testing.T) {
	for i := 0; i < 500; i++ {
		m := NewManager()
		lane := newLane()
		b, err := m.Register("r", mustObs(t, "r", "a"), lane)
		if err != nil {
			t.Fatalf("register: %v", err)
		}
		var wg sync.WaitGroup
		for g := 0; g < 8; g++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				b.Close()
			}()
		}
		wg.Wait()
		m.mu.Lock()
		n := lane.closeCalls
		m.mu.Unlock()
		if n != 1 {
			t.Fatalf("iteration %d: eight concurrent Close calls closed the lane %d times, want 1", i, n)
		}
	}
}
