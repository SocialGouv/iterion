package testfs

import (
	"os"
	"syscall"
	"testing"
)

// WriteExecutable writes body to path and makes it executable by its owner
// (0o700, an existing file's mode included), holding syscall.ForkLock for
// reading from the open to the close. A fork copies every open descriptor
// into the child, which holds it until its own exec; a file someone holds
// for writing cannot be exec'd (ETXTBSY, "text file busy"). A fake binary
// written by one test and exec'd while a parallel test forks therefore fails
// at random. Every fork the Go standard library starts (syscall.forkExec:
// os.StartProcess, os/exec) takes ForkLock for writing, so none can start
// while the write descriptor is open; a fork made from cgo does not.
func WriteExecutable(t testing.TB, path string, body []byte) {
	t.Helper()
	syscall.ForkLock.RLock()
	err := os.WriteFile(path, body, 0o700)
	syscall.ForkLock.RUnlock()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o700); err != nil {
		t.Fatal(err)
	}
}
