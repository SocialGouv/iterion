//go:build desktop

package main

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"sync"
	"time"
)

// spawnDaemonForProject starts a new headless daemon for the given
// project directory and returns its URL once the discovery file shows
// it's accepting connections. The child is fully detached via setsid
// so it survives the GUI's exit — that's the whole point of the
// daemon split.
//
// Logs go to ~/.iterion/daemons/<key>.log so the operator can
// post-mortem a crashed daemon without scraping syslog.
//
// Timeout is generous (20s) because cli.RunStudio's startup includes
// store migrations and file-watcher setup that can take a few seconds
// on cold caches. We poll the discovery file every 200ms so the GUI
// doesn't block on the spawn longer than necessary.
//
// Spawns of one project are serialized: startup, a project switch, a pane
// and a run deep-link each find-then-spawn, and of two concurrent spawns
// the second child dies on the project lock while the first is still
// starting. Under the lock the registry is read again, so the second
// caller gets the first daemon instead of that child's failure.
func spawnDaemonForProject(projectDir string) (string, error) {
	if projectDir == "" {
		return "", fmt.Errorf("spawnDaemonForProject: empty project dir")
	}
	mu := projectSpawnLock(projectDir)
	mu.Lock()
	defer mu.Unlock()
	if url, ok := findDaemonForProject(projectDir); ok {
		return url, nil
	}
	exited, pid, logPath, err := startDaemonChild(projectDir)
	if err != nil {
		return "", err
	}
	url, err := awaitDaemon(projectDir, exited, 20*time.Second)
	if err != nil {
		return "", fmt.Errorf("%w (daemon log: %s)", err, logPath)
	}
	log.Printf("desktop: daemon for %s at %s (spawned pid=%d)", projectDir, url, pid)
	return url, nil
}

var (
	spawnLocksMu sync.Mutex
	spawnLocks   = map[string]*sync.Mutex{}
)

// projectSpawnLock is the per-project spawn mutex, keyed like the
// project's daemon lock file — case-folded on Windows, where two spellings
// of a directory name the same lock and registry files.
func projectSpawnLock(projectDir string) *sync.Mutex {
	key := encodeProjectDirKey(projectDir)
	if goruntime.GOOS == "windows" {
		key = strings.ToLower(key)
	}
	spawnLocksMu.Lock()
	defer spawnLocksMu.Unlock()
	mu, ok := spawnLocks[key]
	if !ok {
		mu = &sync.Mutex{}
		spawnLocks[key] = mu
	}
	return mu
}

// startDaemonChild launches the detached daemon process and reports its
// exit on the returned channel. A variable so tests can stand in a child.
var startDaemonChild = func(projectDir string) (exited <-chan error, pid int, logPath string, err error) {
	self, err := selfExecPath()
	if err != nil {
		return nil, 0, "", fmt.Errorf("resolve self exec: %w", err)
	}
	logFile, err := openDaemonLogFile(projectDir)
	if err != nil {
		// Non-fatal: spawn anyway, daemon logs to /dev/null.
		log.Printf("desktop: daemon log open failed: %v (non-fatal)", err)
		logFile = nil
	}

	cmd := exec.Command(self, "--server-only", "--project="+projectDir)
	cmd.Env = os.Environ()
	logPath = "(no log)"
	if logFile != nil {
		cmd.Stdout = logFile
		cmd.Stderr = logFile
		logPath = logFile.Name()
	}
	detachChild(cmd) // platform-specific: setsid on unix, equivalent on windows
	if err := cmd.Start(); err != nil {
		if logFile != nil {
			_ = logFile.Close()
		}
		return nil, 0, "", fmt.Errorf("daemon spawn: %w", err)
	}
	// Close our copy of the log FD now that Start has succeeded: the
	// child inherited its own dup'd descriptor for stdout/stderr, so the
	// parent's handle is redundant and would otherwise leak for the GUI's
	// lifetime (one per spawned daemon).
	if logFile != nil {
		_ = logFile.Close()
	}
	// The child outlives the GUI on purpose; waiting on it only reaps it
	// if it exits first, and tells awaitDaemon it did.
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	return done, cmd.Process.Pid, logPath, nil
}

// awaitDaemon polls the project's discovery file until a live daemon
// answers, the spawned child exits, or the timeout passes. A child that
// exits is not necessarily a failure: it dies on the project lock when a
// daemon already serves the project, so the registry is read once more
// before giving up.
func awaitDaemon(projectDir string, exited <-chan error, timeout time.Duration) (string, error) {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for {
		if url, ok := findDaemonForProject(projectDir); ok {
			return url, nil
		}
		select {
		case err := <-exited:
			if url, ok := findDaemonForProject(projectDir); ok {
				return url, nil
			}
			if err == nil {
				return "", fmt.Errorf("daemon for %s exited before serving", projectDir)
			}
			return "", fmt.Errorf("daemon for %s exited before serving: %w", projectDir, err)
		case <-deadline.C:
			return "", fmt.Errorf("daemon for %s did not come up within %s", projectDir, timeout)
		case <-tick.C:
		}
	}
}

// selfExecPath returns an absolute path to the running iterion-desktop
// binary. os.Executable handles the "argv[0] was relative or a bare
// name" case correctly across platforms (Linux: /proc/self/exe).
func selfExecPath() (string, error) {
	p, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.Clean(p), nil
}

// openDaemonLogFile opens (creating + appending) the per-project daemon
// log file at ~/.iterion/daemons/<key>.log. The caller wires it to the
// child's stdout/stderr and may close its own copy once cmd.Start has
// succeeded — the child inherits an independent dup'd descriptor, so the
// parent's handle is no longer needed.
func openDaemonLogFile(projectDir string) (*os.File, error) {
	dir := daemonRegistryDir()
	if dir == "" {
		return nil, fmt.Errorf("no data dir resolved")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, encodeProjectDirKey(projectDir)+".log")
	return os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
}
