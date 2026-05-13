//go:build desktop

package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/SocialGouv/iterion/pkg/cli"
)

// attachDaemonEnabled reports whether the GUI should auto-attach to an
// existing headless daemon at startup. Opt-in via ITERION_DESKTOP_ATTACH_DAEMON
// because Phase 1 daemon mode disables project switching from the GUI;
// surprising operators with that limitation just because a daemon was
// running broke too much muscle memory. Phase 2 (daemon-side
// project-switch HTTP API) will make auto-attach safe to flip back to
// opt-out.
func attachDaemonEnabled() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("ITERION_DESKTOP_ATTACH_DAEMON")))
	return v == "1" || v == "true" || v == "yes" || v == "on"
}

// runHeadless is the entry point when `iterion-desktop --server-only`
// (or `--headless`) is set. It runs the HTTP server in the foreground
// without Wails, writes the canonical discovery file ~/.iterion/desktop.json
// (same path the GUI publishes when it owns the server), and blocks until
// SIGTERM / SIGINT.
//
// The daemon is the run-owner: launched once by the operator (or by a
// future GUI auto-spawn), it keeps running across GUI rebuild + relaunch
// cycles so in-flight `secured-renovacy.iter` runs survive the operator
// closing the desktop window or installing a newer binary on top.
//
// State sharing with a co-running GUI process:
//   - the daemon writes desktop.json with its URL + pid + started_at
//   - the GUI on startup reads desktop.json, sees the daemon is alive,
//     skips starting its embedded server, and points its AssetServer
//     proxy at the daemon's URL (wiring lives in app.go onStartup)
//   - on GUI close (Wails window-close), the GUI's onShutdown only tears
//     down GUI-local resources; the daemon process is untouched
//
// The daemon holds a separate single-instance lock from the GUI
// (acquireDaemonLock) so the two processes coexist. Project switching
// and other server-mutating App methods route through the daemon's HTTP
// API in this mode; the GUI's local mutation methods become advisory.
func runHeadless() {
	cfg, err := LoadConfig()
	if err != nil {
		log.Printf("daemon: failed to load config (using defaults): %v", err)
		cfg = NewConfig()
	}

	// Use a daemon-specific instance lock so the GUI's single_instance
	// guard doesn't preempt us (and vice-versa).
	lock, err := acquireDaemonLock()
	if err != nil {
		log.Fatalf("daemon: another instance is already running: %v", err)
	}
	defer func() { _ = lock.Release() }()

	// Resolve the project directory the daemon will host. Mirror the
	// GUI's fallback path so first-run daemons get a small, safe dir
	// rather than dumping a recursive watcher on $HOME.
	dir, storeDir := currentProjectDirsFromConfig(cfg)
	if dir == "" {
		dir = defaultFallbackProjectDir()
	}

	// Bring up the server. We use cli.RunEditor directly (rather than the
	// ServerHost wrapper the GUI uses for restart-on-project-switch) since
	// the daemon doesn't need restart semantics — config edits that
	// require a project switch should bounce the daemon explicitly.
	rootCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	addrCh := make(chan string, 1)
	errCh := make(chan error, 1)
	go func() {
		printer := cli.NewPrinter(cli.OutputJSON)
		opts := cli.EditorOptions{
			Port:      -1,
			Bind:      "127.0.0.1",
			Dir:       dir,
			StoreDir:  storeDir,
			NoBrowser: true,
			OnReady: func(addr string) {
				select {
				case addrCh <- addr:
				default:
				}
			},
		}
		if err := cli.RunEditor(rootCtx, opts, printer); err != nil {
			select {
			case errCh <- err:
			default:
			}
		}
	}()

	var addr string
	select {
	case addr = <-addrCh:
	case err := <-errCh:
		log.Fatalf("daemon: server failed to start: %v", err)
	}

	url := "http://" + addr + "/"
	writeDesktopURLFile(url)
	defer removeDesktopURLFile()
	log.Printf("daemon: serving on %s (pid=%d, project=%s)", url, os.Getpid(), dir)

	// Block until the operator (or systemd / launchd / a manager) sends
	// SIGTERM or SIGINT. The deferred cancel + removeDesktopURLFile tear
	// down cleanly so the next daemon launch starts from a clean slate.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	sig := <-sigCh
	log.Printf("daemon: received %s, shutting down", sig)
}

// currentProjectDirsFromConfig returns the (dir, storeDir) pair for the
// project currently selected in config, or ("", "") if no project is
// selected. Mirrors App.currentProjectServerDirsLocked but without
// requiring an App instance.
func currentProjectDirsFromConfig(cfg *Config) (string, string) {
	if cfg == nil {
		return "", ""
	}
	if p := cfg.CurrentProject(); p != nil {
		return p.Dir, p.StoreDir
	}
	return "", ""
}
