//go:build linux

// Package proctest checks OS resources owned by a test binary.
package proctest

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// NoProcessLeaks runs a suite as a Linux child subreaper: an orphaned helper
// remains its descendant instead of escaping to init. After the suite (and
// all t.Cleanup callbacks) returns, any surviving process fails the suite and
// is reclaimed. Other test binaries and pre-existing session orphans are not
// descendants and cannot be reported or signalled by this guard.
//
// Compose it in TestMain: os.Exit(proctest.NoProcessLeaks(m.Run)). A guard
// around an existing suite wrapper works too. It never reaps during run:
// os/exec owns Wait while tests are executing.
func NoProcessLeaks(run func() int) int {
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		fmt.Fprintf(os.Stderr, "FAIL: enable test process leak guard: %v\n", err)
		return 1
	}
	code := run()
	leaked := false
	// Killing a leaked parent reparents its children to us; rescan until the
	// entire owned tree is gone. This is a teardown bound, not a test budget.
	deadline := time.Now().Add(5 * time.Second)
	for {
		children, err := childPIDs()
		if err != nil {
			fmt.Fprintf(os.Stderr, "FAIL: inspect test descendants: %v\n", err)
			return 1
		}
		if len(children) == 0 {
			// A thread can exit and hand its children to another thread
			// between /proc reads. Only the kernel's process-wide ECHILD
			// verdict proves that no owned descendant remains.
			var status unix.WaitStatus
			_, err := unix.Wait4(-1, &status, unix.WNOHANG, nil)
			if errors.Is(err, unix.ECHILD) {
				break
			}
			if err != nil && !errors.Is(err, unix.EINTR) {
				fmt.Fprintf(os.Stderr, "FAIL: inspect remaining test children: %v\n", err)
				return 1
			}
		}
		for _, pid := range children {
			p, err := os.FindProcess(pid) // Linux uses a pidfd, avoiding PID reuse on Signal.
			if err != nil {
				continue
			}
			state, parent, err := processState(pid)
			if err != nil || parent != os.Getpid() {
				_ = p.Release()
				continue
			}
			if state != "Z" && state != "X" {
				leaked = true
				name, _ := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid))
				fmt.Fprintf(os.Stderr, "FAIL: test process survived suite cleanup: pid=%d command=%s\n", pid, strings.TrimSpace(string(name)))
				if err := p.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
					fmt.Fprintf(os.Stderr, "FAIL: reclaim test process %d: %v\n", pid, err)
				}
			}
			_ = p.Release()
			// Nonblocking: a just-killed child may not be waitable yet. Only this
			// known direct child is reaped; never wait on another test's processes.
			var status unix.WaitStatus
			_, _ = unix.Wait4(pid, &status, unix.WNOHANG, nil)
		}
		if time.Now().After(deadline) {
			fmt.Fprintln(os.Stderr, "FAIL: test descendants did not exit after cleanup")
			return 1
		}
		time.Sleep(10 * time.Millisecond)
	}
	if leaked && code == 0 {
		return 1
	}
	return code
}

// Children can belong to any OS thread in this Go process, not just the
// thread-group leader. Reading its task entries is narrower than scanning
// the host process table and remains isolated from concurrent test packages.
func childPIDs() ([]int, error) {
	paths, err := filepath.Glob("/proc/self/task/*/children")
	if err != nil {
		return nil, err
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("/proc task children unavailable")
	}
	seen := map[int]bool{}
	var result []int
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			continue
		} // an OS thread exited during the scan
		if err != nil {
			return nil, err
		}
		for _, field := range strings.Fields(string(data)) {
			pid, err := strconv.Atoi(field)
			if err != nil {
				return nil, err
			}
			if !seen[pid] {
				seen[pid] = true
				result = append(result, pid)
			}
		}
	}
	return result, nil
}

func processState(pid int) (state string, parent int, err error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return "", 0, err
	}
	// comm is parenthesized and can itself contain spaces or parentheses.
	end := strings.LastIndexByte(string(data), ')')
	if end < 0 {
		return "", 0, fmt.Errorf("invalid process stat for %d", pid)
	}
	fields := strings.Fields(string(data[end+1:]))
	if len(fields) < 2 {
		return "", 0, fmt.Errorf("short process stat for %d", pid)
	}
	parent, err = strconv.Atoi(fields[1])
	return fields[0], parent, err
}
