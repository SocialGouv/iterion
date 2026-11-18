package alert

import (
	"runtime"
	"strings"
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
