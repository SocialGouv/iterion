//go:build desktop && windows

package main

import (
	"fmt"
	"log"
	"os"

	"golang.org/x/sys/windows"
)

// processAlive asks Windows about the pid. os.Process.Signal implements
// only Kill on Windows, so the signal-0 probe the other platforms use
// answers "dead" for every daemon, live ones included.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		// ERROR_INVALID_PARAMETER is "no such pid". Anything else (access
		// denied to another user's process) proves the pid exists.
		return err != windows.ERROR_INVALID_PARAMETER
	}
	defer windows.CloseHandle(h)
	// A process object outlives its process while anyone holds a handle;
	// it is signalled once the process has exited. (The exit code cannot
	// tell: STILL_ACTIVE is also a code a process may exit with.)
	ev, err := windows.WaitForSingleObject(h, 0)
	return err != nil || ev == uint32(windows.WAIT_TIMEOUT)
}

// daemonStopEventName names the event a daemon waits on for a stop request.
// Local\ keeps it in the operator's session.
func daemonStopEventName(pid int) string {
	return fmt.Sprintf(`Local\iterion-desktop-daemon-stop-%d`, pid)
}

// stopDaemonProcess asks a daemon to shut down by setting its stop event.
// Windows has no SIGTERM, and a daemon started with DETACHED_PROCESS has
// no console to receive a Ctrl+Break, so the event is the one graceful
// path: the daemon drains its server and clears its discovery file as it
// does on SIGTERM elsewhere. A daemon that never created the event (one
// started by an older Studio) is reported, not killed.
func stopDaemonProcess(pid int) error {
	name, err := windows.UTF16PtrFromString(daemonStopEventName(pid))
	if err != nil {
		return err
	}
	h, err := windows.OpenEvent(windows.EVENT_MODIFY_STATE, false, name)
	if err != nil {
		return fmt.Errorf("daemon pid=%d accepts no stop request: %w", pid, err)
	}
	defer windows.CloseHandle(h)
	return windows.SetEvent(h)
}

// daemonStopRequests creates this daemon's stop event and returns a
// channel that fires once the GUI sets it. On failure the daemon still
// serves, and the channel never fires.
func daemonStopRequests() <-chan struct{} {
	name, err := windows.UTF16PtrFromString(daemonStopEventName(os.Getpid()))
	if err != nil {
		log.Printf("daemon: stop event: %v (a stop from the GUI will not reach this daemon)", err)
		return nil
	}
	h, err := windows.CreateEvent(nil, 1, 0, name)
	if h == 0 {
		log.Printf("daemon: stop event: %v (a stop from the GUI will not reach this daemon)", err)
		return nil
	}
	if err != nil {
		// ERROR_ALREADY_EXISTS comes with a handle to the event of that
		// name: the one the GUI sets, so this daemon waits on it all the
		// same. The GUI finds a daemon through its registry entry, which is
		// written after this, so a signal already set is stale: clear it.
		log.Printf("daemon: stop event %s already existed: %v", daemonStopEventName(os.Getpid()), err)
		_ = windows.ResetEvent(h)
	}
	ch := make(chan struct{})
	go func() {
		defer windows.CloseHandle(h)
		if ev, err := windows.WaitForSingleObject(h, windows.INFINITE); err != nil || ev != windows.WAIT_OBJECT_0 {
			log.Printf("daemon: stop event wait: %d %v", ev, err)
			return
		}
		close(ch)
	}()
	return ch
}
