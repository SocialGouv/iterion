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

// attachDaemonEnabled was the Phase 1 env gate for GUI-side daemon
// attach. Phase 2 makes attach the default (daemons are per-project and
// project switching from the GUI works again), but the gate stays as an
// escape hatch: ITERION_DESKTOP_ATTACH_DAEMON=0 / false / off forces
// the GUI to start its own embedded server even when a daemon is alive.
func attachDaemonEnabled() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("ITERION_DESKTOP_ATTACH_DAEMON")))
	switch v {
	case "0", "false", "no", "off":
		return false
	default:
		return true
	}
}

// runHeadless is the entry point when `iterion-desktop --server-only`
// (or `--headless`) is set. It runs the HTTP server in the foreground
// without Wails, writes both the per-project daemon registry file
// (~/.iterion/daemons/<key>.json) and the legacy global desktop.json
// (~/.iterion/desktop.json — kept for the iterion-control MCP), then
// blocks until SIGTERM / SIGINT, or on Windows until the GUI sets its
// stop event (stopDaemonProcess).
//
// Multi-project: each daemon hosts ONE project. The GUI auto-spawns a
// daemon per project the operator opens, and attaches by reading the
// per-project file. A separate `iterion-desktop-daemon-<key>.lock`
// flock guarantees at most one daemon per project; many can coexist.
func runHeadless() {
	projectDir, projectStoreDir := resolveDaemonProject()

	cfg, err := LoadConfig()
	if err != nil {
		log.Printf("daemon: failed to load config (using defaults): %v", err)
		cfg = NewConfig()
	}
	_ = cfg

	if projectDir == "" {
		log.Fatalf("daemon: no project resolved (pass --project=<dir> or set ITERION_PROJECT, or select a current project in config)")
	}

	lock, err := acquireDaemonLockForProject(projectDir)
	if err != nil {
		log.Fatalf("daemon: another daemon is already running for project %s: %v", projectDir, err)
	}
	defer func() { _ = lock.Release() }()

	rootCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	addrCh := make(chan string, 1)
	errCh := make(chan error, 1)
	go func() {
		printer := cli.NewPrinter(cli.OutputJSON)
		opts := cli.StudioOptions{
			Port:      -1,
			Bind:      "127.0.0.1",
			Dir:       projectDir,
			StoreDir:  projectStoreDir,
			NoBrowser: true,
			OnReady: func(addr string) {
				select {
				case addrCh <- addr:
				default:
				}
			},
			// Daemon is the actual HTTP server the GUI proxies all
			// /api/* calls to, so the credentials Refresh button hits
			// this process — wire ReloadIterionEnvFile here too,
			// mirroring server_host.go for the GUI-embedded server.
			OnForceRefresh: ReloadIterionEnvFile,
		}
		if err := cli.RunStudio(rootCtx, opts, printer); err != nil {
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
	// The stop channel exists before the daemon is discoverable, so a GUI
	// that finds it can always stop it.
	stopCh := daemonStopRequests()
	if err := writeDaemonInfo(projectDir, url); err != nil {
		log.Fatalf("daemon: %v — the GUI could not find this daemon, so it exits instead of holding the project", err)
	}
	writeDesktopURLFile(url) // legacy global discovery file for MCP
	defer removeDaemonInfo(projectDir)
	defer removeDesktopURLFile()
	log.Printf("daemon: serving on %s (pid=%d, project=%s)", url, os.Getpid(), projectDir)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	select {
	case sig := <-sigCh:
		log.Printf("daemon: received %s, shutting down", sig)
	case <-stopCh:
		log.Printf("daemon: stop requested by the GUI, shutting down")
	}
}

// resolveDaemonProject picks the project directory the daemon will host.
// Precedence:
//  1. CLI flag --project=<dir>  (highest — most explicit)
//  2. ITERION_PROJECT env var
//  3. Currently-selected project from the GUI config
//
// The store dir is derived the same way (--project-store, env, config).
// Returning "" lets runHeadless surface a clear "no project" error
// before binding any port or grabbing any lock.
func resolveDaemonProject() (string, string) {
	var dir, storeDir string
	for _, a := range os.Args[1:] {
		switch {
		case strings.HasPrefix(a, "--project="):
			dir = strings.TrimPrefix(a, "--project=")
		case strings.HasPrefix(a, "--project-store="):
			storeDir = strings.TrimPrefix(a, "--project-store=")
		}
	}
	if dir == "" {
		dir = os.Getenv("ITERION_PROJECT")
	}
	if storeDir == "" {
		storeDir = os.Getenv("ITERION_PROJECT_STORE")
	}
	if dir == "" {
		// Fall back to the GUI config's current project so a bare
		// `iterion-desktop --server-only` Just Works for the operator's
		// most-recently-opened project (matches the GUI's own startup
		// behaviour).
		if cfg, err := LoadConfig(); err == nil {
			d, sd := currentProjectDirsFromConfig(cfg)
			if d != "" {
				dir = d
			}
			if storeDir == "" && sd != "" {
				storeDir = sd
			}
		}
	}
	return dir, storeDir
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
