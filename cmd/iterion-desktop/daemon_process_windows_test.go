//go:build desktop && windows

package main

import (
	"os"
	"os/exec"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

const exit259HelperEnv = "ITERION_DESKTOP_TEST_EXIT259_HELPER"

// Two spellings of one directory share its lock and registry files on
// Windows, so they must share the spawn mutex too.
func TestProjectSpawnLock_IgnoresCaseOnWindows(t *testing.T) {
	if projectSpawnLock(`C:\Work\Proj`) != projectSpawnLock(`c:\work\PROJ`) {
		t.Fatal("two spellings of one directory got two spawn mutexes")
	}
}

// TestHelperExit259 is not a test: run as a child process, it exits with
// 259, the value of STILL_ACTIVE.
func TestHelperExit259(t *testing.T) {
	if os.Getenv(exit259HelperEnv) != "1" {
		t.Skip("helper process only")
	}
	os.Exit(259)
}

// A process that exited with 259 while someone still holds its handle
// reports STILL_ACTIVE as its exit code; it is dead all the same.
func TestProcessAlive_AProcessThatExitedWith259(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperExit259$")
	cmd.Env = append(os.Environ(), exit259HelperEnv+"=1")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Wait() }()
	pid := cmd.Process.Pid
	h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(h)
	if ev, err := windows.WaitForSingleObject(h, 10_000); err != nil || ev != windows.WAIT_OBJECT_0 {
		t.Fatalf("helper did not exit: %d %v", ev, err)
	}
	if processAlive(pid) {
		t.Fatalf("processAlive(%d) = true for a process that exited with 259", pid)
	}
}

// An event of the daemon's name that already exists is still the one the
// GUI sets: the daemon waits on it rather than becoming unstoppable, and a
// signal set before the daemon was discoverable does not stop it.
func TestDaemonStopRequests_WaitsOnAnEventThatAlreadyExisted(t *testing.T) {
	name, err := windows.UTF16PtrFromString(daemonStopEventName(os.Getpid()))
	if err != nil {
		t.Fatal(err)
	}
	pre, err := windows.CreateEvent(nil, 1, 1, name) // already signalled
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(pre)

	stopCh := daemonStopRequests()
	if stopCh == nil {
		t.Fatal("daemonStopRequests gave up on an existing event")
	}
	select {
	case <-stopCh:
		t.Fatal("a signal set before the daemon started stopped it")
	case <-time.After(300 * time.Millisecond):
	}
	if err := stopDaemonProcess(os.Getpid()); err != nil {
		t.Fatalf("stopDaemonProcess(own pid) = %v", err)
	}
	select {
	case <-stopCh:
	case <-time.After(5 * time.Second):
		t.Fatal("the stop request did not reach the daemon waiting on the existing event")
	}
}
