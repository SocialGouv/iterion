//go:build unix

package parser

import (
	"syscall"
	"testing"
	"time"
)

// processCPUTime returns the CPU time, user and system, the test binary has
// consumed so far. A timing claim on a shared runner reads it rather than
// the wall clock: the wall clock also counts every other process the machine
// schedules — the parallel test binaries of a CI shard among them — while
// the process's own CPU time is what the code under test spent.
func processCPUTime(t testing.TB) (time.Duration, bool) {
	t.Helper()
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		t.Fatalf("getrusage: %v", err)
	}
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano()), true
}
