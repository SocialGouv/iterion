//go:build unix

package jsonl

import (
	"os"
	"syscall"
)

// lockFile takes an exclusive advisory lock, blocking until acquired.
// Appends are single small writes, so contention windows are tiny.
func lockFile(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
}

func unlockFile(f *os.File) {
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
