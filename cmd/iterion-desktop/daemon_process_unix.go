//go:build desktop && !windows

package main

import (
	"os"
	"syscall"
)

// processAlive probes a pid with signal 0: delivered to nobody, but
// refused when the pid is gone.
func processAlive(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}

// stopDaemonProcess asks a daemon to shut down. SIGTERM reaches the
// signal handler runHeadless waits on, which drains the server and clears
// the discovery file.
func stopDaemonProcess(pid int) error {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return proc.Signal(syscall.SIGTERM)
}

// daemonStopRequests is the stop channel for platforms without SIGTERM.
// Here SIGTERM is the stop request, so it never fires.
func daemonStopRequests() <-chan struct{} { return nil }
