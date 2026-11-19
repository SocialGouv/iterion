//go:build desktop

package main

import (
	"bufio"
	"errors"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

const stopHelperEnv = "ITERION_DESKTOP_TEST_STOP_HELPER"

// TestHelperDaemonWaitingForStop is not a test: run as a child process by
// TestStopDaemonProcess_StopsADaemonWaitingForIt, it waits for a stop the
// way runHeadless does and exits 0 when one arrives.
func TestHelperDaemonWaitingForStop(t *testing.T) {
	if os.Getenv(stopHelperEnv) != "1" {
		t.Skip("helper process only")
	}
	stopCh := daemonStopRequests()
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	os.Stdout.WriteString("ready\n")
	select {
	case <-stopCh:
	case <-sigCh:
	case <-time.After(30 * time.Second):
		os.Exit(3)
	}
	os.Exit(0)
}

func TestStopDaemonProcess_StopsADaemonWaitingForIt(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperDaemonWaitingForStop$")
	cmd.Env = append(os.Environ(), stopHelperEnv+"=1")
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if line, err := bufio.NewReader(out).ReadString('\n'); err != nil || strings.TrimSpace(line) != "ready" {
		_ = cmd.Process.Kill()
		t.Fatalf("helper did not get ready: %q, %v", line, err)
	}
	if err := stopDaemonProcess(cmd.Process.Pid); err != nil {
		_ = cmd.Process.Kill()
		t.Fatalf("stopDaemonProcess(%d) = %v, want nil", cmd.Process.Pid, err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("helper exited with %v, want a clean exit on the stop request", err)
		}
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("helper still running 10s after stopDaemonProcess")
	}
}

func TestProcessAlive_ThisProcessAndAnExitedChild(t *testing.T) {
	if !processAlive(os.Getpid()) {
		t.Fatalf("processAlive(own pid %d) = false, want true", os.Getpid())
	}
	child := exec.Command(os.Args[0], "-test.run=^$")
	if err := child.Run(); err != nil {
		t.Fatal(err)
	}
	if processAlive(child.Process.Pid) {
		t.Fatalf("processAlive(exited pid %d) = true, want false", child.Process.Pid)
	}
}

// The probe the GUI runs before attaching: a live pid with a listening
// port is a live daemon. On Windows the signal-0 probe made it false for
// every daemon, so the GUI spawned a second one per open.
func TestDaemonAlive_ALiveProcessWithAListeningPort(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	url := "http://" + ln.Addr().String() + "/"
	if !daemonAlive(os.Getpid(), url) {
		t.Fatalf("daemonAlive(own pid, %s) = false, want true", url)
	}
}

func TestWriteDaemonInfo_ReportsARegistryItCannotWrite(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ITERION_HOME", home)
	// A file where the registry directory should be: MkdirAll fails.
	if err := os.WriteFile(filepath.Join(home, daemonRegistryDirName), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeDaemonInfo(filepath.Join(home, "project"), "http://127.0.0.1:1/"); err == nil {
		t.Fatal("writeDaemonInfo into an unwritable registry = nil, want an error")
	}
}

func TestAwaitDaemon_AChildThatExitsFailsFast(t *testing.T) {
	t.Setenv("ITERION_HOME", t.TempDir())
	exited := make(chan error, 1)
	exited <- errors.New("exit status 1")
	start := time.Now()
	_, err := awaitDaemon(filepath.Join(t.TempDir(), "project"), exited, 20*time.Second)
	if err == nil || !strings.Contains(err.Error(), "exited before serving") {
		t.Fatalf("awaitDaemon after the child exited = %v, want an exited-before-serving error", err)
	}
	if waited := time.Since(start); waited > 5*time.Second {
		t.Fatalf("awaitDaemon waited %s after the child exited, want it to stop at once", waited)
	}
}

// Two opens of one project at once (startup and a pane, say): the second
// child would die on the project lock while the first is still starting.
// Serialized, the second caller finds the first daemon instead.
func TestSpawnDaemonForProject_ConcurrentSpawnsShareOneDaemon(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ITERION_HOME", home)
	project := filepath.Join(home, "project")
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	url := "http://" + ln.Addr().String() + "/"

	var starts atomic.Int32
	orig := startDaemonChild
	t.Cleanup(func() { startDaemonChild = orig })
	startDaemonChild = func(dir string) (<-chan error, int, string, error) {
		exited := make(chan error, 1)
		if starts.Add(1) == 1 {
			// The first child takes a moment to start, then serves.
			go func() {
				time.Sleep(300 * time.Millisecond)
				_ = writeDaemonInfo(dir, url)
			}()
		} else {
			// A later child loses the project lock and dies at once.
			exited <- errors.New("exit status 1")
		}
		return exited, os.Getpid(), "(test)", nil
	}

	var wg sync.WaitGroup
	got := make([]string, 2)
	errs := make([]error, 2)
	for i := range got {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got[i], errs[i] = spawnDaemonForProject(project)
		}()
	}
	wg.Wait()
	for i := range got {
		if errs[i] != nil || got[i] != url {
			t.Errorf("spawn %d = %q, %v; want %q, nil", i, got[i], errs[i], url)
		}
	}
	if n := starts.Load(); n != 1 {
		t.Errorf("%d daemon children started for one project, want 1", n)
	}
}

// The child dies on the project lock when a daemon already serves the
// project: that daemon is the answer, not an error.
func TestAwaitDaemon_AttachesToTheDaemonAlreadyServing(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ITERION_HOME", home)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	project := filepath.Join(home, "project")
	url := "http://" + ln.Addr().String() + "/"
	if err := writeDaemonInfo(project, url); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	exited <- errors.New("exit status 1")
	got, err := awaitDaemon(project, exited, 20*time.Second)
	if err != nil || got != url {
		t.Fatalf("awaitDaemon = %q, %v; want %q, nil", got, err, url)
	}
}
