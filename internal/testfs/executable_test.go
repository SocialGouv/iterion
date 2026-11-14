//go:build !js && !plan9 && !wasip1

package testfs

import (
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// The guarantee, driven the way the flake happens: fresh executables are
// written and exec'd while other goroutines of the same process fork without
// pause. Each fork that lands between the open and the close of a write would
// copy the descriptor into its child, and the exec that follows would answer
// ETXTBSY. It needs two Ps to race at all (at GOMAXPROCS=1 it passes without
// the lock); the FIFO test beside it proves the mechanism for the open and
// the write at any count, and the close is this test's alone. Only
// ETXTBSY is its assertion: a fork refused for lack of processes (EAGAIN,
// ENOMEM under a pid ceiling) is retried, and an exec that never ran is not
// judged.
func TestWriteExecutableSurvivesConcurrentForks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("ETXTBSY is a Unix exec refusal")
	}
	dir := t.TempDir()
	stop := make(chan struct{})
	var forks atomic.Int64
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if err := exec.Command("true").Run(); err == nil {
					forks.Add(1)
				}
			}
		}()
	}
	defer func() {
		close(stop)
		wg.Wait()
	}()
	judged, retries := 0, 0
	for i := range 300 {
		path := filepath.Join(dir, fmt.Sprintf("fake-%d", i))
		WriteExecutable(t, path, []byte("#!/bin/sh\nexit 0\n"))
		// Only an exec right after the write can see the race: one that
		// needed a retry ran too late to judge anything.
		for attempt := 0; attempt < 20; attempt++ {
			out, err := exec.Command(path).CombinedOutput()
			if err == nil {
				if attempt == 0 {
					judged++
				}
				break
			}
			retries++
			if errors.Is(err, syscall.ETXTBSY) {
				t.Fatalf("exec #%d of a freshly written executable (%d concurrent forks so far): %v\n%s", i, forks.Load(), err, out)
			}
			if !errors.Is(err, syscall.EAGAIN) && !errors.Is(err, syscall.ENOMEM) {
				t.Fatalf("exec #%d failed for a reason this test does not judge: %v\n%s", i, err, out)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	t.Logf("judged %d of 300 execs (%d retries) against %d concurrent forks", judged, retries, forks.Load())
	if judged < 100 {
		t.Skipf("only %d of 300 execs ran at the first attempt (fork refused for lack of processes): too few to judge here", judged)
	}
	if forks.Load() == 0 {
		t.Fatal("no concurrent fork ran: the test proved nothing")
	}
}
