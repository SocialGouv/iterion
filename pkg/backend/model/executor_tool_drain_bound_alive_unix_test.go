//go:build unix

package model

import (
	"errors"
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"
)

// processAlive reports whether pid still names a live process. FindProcess
// holds a pidfd on Linux, so the signal cannot land on a process the OS built
// over a recycled pid. A zombie still answers signal 0, and the holders these
// witnesses reap are reparented to init the moment the node's shell exits —
// but where init does not reap (a bare container's PID 1), a killed holder
// would linger as a zombie, so a readable /proc gets the last word: a zombie
// is dead.
func processAlive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	defer func() { _ = p.Release() }()
	if err := p.Signal(syscall.Signal(0)); errors.Is(err, syscall.ESRCH) || errors.Is(err, os.ErrProcessDone) {
		return false
	}
	if runtime.GOOS == "linux" {
		if stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat"); err == nil {
			// "<pid> (<comm>) <state> …" — comm may hold spaces and
			// parentheses, so the state is the field after the LAST ')'.
			if i := strings.LastIndexByte(string(stat), ')'); i >= 0 && i+2 < len(stat) {
				if s := string(stat)[i+2:]; strings.HasPrefix(s, "Z") || strings.HasPrefix(s, "X") {
					return false
				}
			}
		}
	}
	return true
}
