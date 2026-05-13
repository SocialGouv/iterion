//go:build desktop

package main

import (
	"context"
	"log"
	"os"
	"sync"
	"time"

	wruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

// App is the Wails-bound struct exposed to the frontend. Every public method
// on App becomes a JS-callable binding under window.go.main.App.<MethodName>.
//
// Lifecycle: onStartup → onDomReady → (interactive) → onBeforeClose →
// onShutdown. The HTTP server is brought up in onStartup and torn down in
// onShutdown.
type App struct {
	ctx       context.Context
	server    serverController
	serverURL string
	// usingDaemon is true when the GUI attached to an externally-owned
	// HTTP server (iterion-desktop --server-only). In that mode the GUI
	// must NOT stop the server on shutdown and must NOT mutate the
	// daemon's desktop.json discovery file — both belong to the daemon.
	usingDaemon bool

	mu       sync.RWMutex
	config   *Config
	keychain secretStore
	updater  *Updater
	instLock *SingleInstance

	// shellEnvKeys records API-key env vars that existed before Iterion
	// injected keychain values. Only these keys shadow Settings/keychain
	// edits; values inserted by Iterion remain mutable for this session.
	shellEnvKeys     map[string]bool
	injectedEnvKeys  map[string]bool
	injectedEnvValue map[string]string
}

// NewApp creates a new desktop App. Heavy lifting is deferred to onStartup
// (which gets the Wails context).
func NewApp() *App {
	return &App{}
}

func (a *App) onStartup(ctx context.Context) {
	a.ctx = ctx

	// Apply macOS PATH fix BEFORE looking up external CLIs. On Finder/Dock
	// launches PATH is minimal; without this fix `claude`/`codex`/`git`
	// from Homebrew/asdf/devbox would be invisible to the spawned tools.
	if err := applyMacOSPathFix(); err != nil {
		log.Printf("desktop: macOS PATH fix failed (non-fatal): %v", err)
	}

	cfg, err := LoadConfig()
	if err != nil {
		log.Printf("desktop: failed to load config (using defaults): %v", err)
		cfg = NewConfig()
	}
	a.config = cfg

	// Single-instance guard. If another iterion-desktop is already running,
	// signal it to focus and exit silently.
	il, err := AcquireSingleInstanceLock()
	if err != nil {
		log.Printf("desktop: another instance is already running; signalling focus and exiting")
		_ = SignalExistingInstance()
		// Give Wails a chance to render then quit. We can't return from
		// onStartup so call Quit on the next tick.
		go func() {
			time.Sleep(50 * time.Millisecond)
			wruntime.Quit(ctx)
		}()
		return
	}
	a.instLock = il
	il.Listen(func() {
		// Another instance asked us to come to front.
		wruntime.WindowShow(a.ctx)
		wruntime.WindowUnminimise(a.ctx)
	})

	// Apply persisted window state, if any.
	if a.config.Window.Width > 0 && a.config.Window.Height > 0 {
		applyWindowState(ctx, a.config.Window)
	}

	// Wire up keychain and inject any stored secrets into env (env vars
	// already set by the launching shell take precedence — we never
	// overwrite an existing env entry).
	a.keychain = NewKeychain()
	a.applyKeychainToEnv()

	// Phase 1 daemon attach is OPT-IN: the GUI only piggy-backs on a
	// running `iterion-desktop --server-only` when ITERION_DESKTOP_ATTACH_DAEMON=1
	// (or =true). Default behaviour stays "own embedded server" so
	// project switching keeps working out of the box.
	//
	// Why opt-in: Phase 1 daemon mode disables project switching from the
	// GUI (the daemon doesn't yet expose a HTTP API for switch — Phase 2).
	// Auto-attaching whenever a daemon was visible silently broke
	// everyday workflows for operators who started a daemon for one
	// purpose and then forgot it was running. Phase 2 will lift the
	// project-switch limitation and we can flip this to opt-out.
	if attachDaemonEnabled() {
		if url, ok := detectDaemonURL(); ok {
			a.serverURL = url
			a.usingDaemon = true
			log.Printf("desktop: attached to existing daemon at %s", url)
		}
	} else {
		if url, ok := detectDaemonURL(); ok {
			log.Printf("desktop: headless daemon detected at %s (ignored — set ITERION_DESKTOP_ATTACH_DAEMON=1 to attach)", url)
		}
	}

	if !a.usingDaemon {
		// No daemon — bring up the embedded server for the current project
		// (or no project on first run — the editor SPA's useDesktop hook
		// routes to /welcome based on IsFirstRunPending).
		a.server = NewServerHost()
		if err := a.startServerForCurrentProject(ctx); err != nil {
			log.Printf("desktop: server failed to start: %v", err)
			// Surface the error: GetServerURL stays "" and the AssetServer
			// reverse-proxy returns 503 to the WebView until the next
			// successful start (e.g. after a project switch).
			return
		}
	}

	// Wire the auto-updater. CheckForUpdate is non-blocking; if the user
	// opted in to auto-check it runs in a goroutine and emits an event
	// when a release is found.
	a.updater = NewUpdater(a.config)
	if a.config.Updater.AutoCheck {
		go a.checkForUpdateAsync(ctx)
		go a.updateCheckTicker(ctx)
	}
}

func (a *App) onDomReady(_ context.Context) {}

// onBeforeClose lets us persist the window state before Wails tears down.
// Returning true would cancel the close — we always allow it.
func (a *App) onBeforeClose(ctx context.Context) bool {
	a.persistWindowState(ctx)
	return false
}

func (a *App) onShutdown(_ context.Context) {
	// Order matters: stop the embedded server first so in-flight runs flip
	// to failed_resumable, then drop the single-instance lock. Clear the
	// discovery file last so the iterion-control MCP doesn't keep
	// pointing at a dead port after the desktop exits.
	//
	// When we're attached to an external daemon (a.usingDaemon), the
	// daemon OWNS the server + the desktop.json discovery file — leave
	// them alone so the next GUI launch can re-attach and operator-side
	// run continues across GUI rebuild cycles.
	if a.usingDaemon {
		if a.instLock != nil {
			_ = a.instLock.Release()
		}
		return
	}
	if a.server != nil {
		a.server.Stop()
	}
	removeDesktopURLFile()
	if a.instLock != nil {
		_ = a.instLock.Release()
	}
}

// currentProjectServerDirs returns the editor working directory/store pair
// for the project currently selected in config. On first-run / empty config
// it uses a small, sandboxed directory under the user config dir so the
// editor server can start (the SPA's onboarding flow then prompts for a
// real project). Falling back to $HOME makes the recursive file watcher
// crawl the entire home tree, exhausting inotify limits and stalling the
// SPA's initial bootstrap behind permission-denied warnings.
func (a *App) currentProjectServerDirsLocked() (dir, storeDir string) {
	if p := a.config.CurrentProject(); p != nil {
		dir = p.Dir
		storeDir = p.StoreDir
	}
	if dir == "" {
		dir = defaultFallbackProjectDir()
	}
	return dir, storeDir
}

// defaultFallbackProjectDir returns the directory the editor server points
// to when no project is selected. We anchor it inside the user config dir
// (e.g. ~/.config/Iterion on Linux) so it's small, writable, and never
// leaks the user's home tree to the file watcher.
func defaultFallbackProjectDir() string {
	cfgDir, err := os.UserConfigDir()
	if err == nil {
		fallback := cfgDir + string(os.PathSeparator) + "Iterion" + string(os.PathSeparator) + "default-project"
		if mkErr := os.MkdirAll(fallback, 0o755); mkErr == nil {
			return fallback
		}
	}
	// Last-resort fallback: empty string. server.go runs without WorkDir,
	// the SPA stays on /welcome until the user picks a project.
	return ""
}

// startServerForCurrentProject brings up the HTTP server for the project
// currently selected in config.
func (a *App) startServerForCurrentProject(ctx context.Context) error {
	a.mu.Lock()
	dir, storeDir := a.currentProjectServerDirsLocked()
	a.mu.Unlock()

	addr, err := a.server.Start(ctx, dir, storeDir)
	if err != nil {
		return err
	}
	a.mu.Lock()
	a.serverURL = "http://" + addr + "/"
	url := a.serverURL
	a.mu.Unlock()
	writeDesktopURLFile(url)
	return nil
}

// restartServerForCurrentProject stops any running editor server and starts
// a new one pointed at the current config project. The HTTP server binds a
// fresh random ephemeral port on every Start (Port=-1), so any in-memory
// SPA state (open WebSockets, react-query caches, file watchers) refers to
// a now-dead listener. We drive a full re-bootstrap via WindowReloadApp,
// which navigates the WebView back to the AssetServer start URL — and the
// AssetServer reverse-proxy (cmd/iterion-desktop/asset_proxy.go) routes
// the fresh load to the NEW serverURL because its cached *ReverseProxy
// invalidates on serverURL change. The SPA re-mounts on the new server
// with /wails/runtime.js + /wails/ipc.js still injected (page origin is
// still the AssetServer's), and the proxy attaches the canonical session
// cookie to every forwarded request.
//
// It intentionally does not emit project:switched: the WindowReloadApp
// call already tears down the SPA, so callers should rely on reload (not
// on a JS-side window.location.reload — which is now a no-op anyway,
// since the page origin doesn't change between restarts).
func (a *App) restartServerForCurrentProject(ctx context.Context) (*Project, error) {
	// In daemon-attached mode, the server lives in a separate process
	// the GUI doesn't own. Soft no-op: return the daemon's current
	// project (read from shared config) and skip the server bounce —
	// the SPA's on-mount sync calls this and would otherwise fail
	// every GUI launch with a hard error. A genuine project change
	// against a daemon still requires restarting the daemon (Phase 2
	// will add a daemon-side HTTP API for this).
	a.mu.Lock()
	p := a.config.CurrentProject()
	var current *Project
	if p != nil {
		cp := *p
		current = &cp
	}
	if a.usingDaemon {
		a.mu.Unlock()
		log.Printf("desktop: restartServerForCurrentProject is a no-op in daemon mode; restart daemon to change project")
		return current, nil
	}
	dir, storeDir := a.currentProjectServerDirsLocked()
	a.mu.Unlock()

	a.server.Stop()
	addr, err := a.server.Start(ctx, dir, storeDir)
	if err != nil {
		return nil, err
	}
	a.mu.Lock()
	a.serverURL = "http://" + addr + "/"
	url := a.serverURL
	a.mu.Unlock()
	writeDesktopURLFile(url)
	a.reloadWindowApp(ctx)
	return current, nil
}

// reloadWindowApp re-bootstraps the Wails webview against the Wails
// AssetServer start URL. Split out so tests can override it (the prod build
// uses wruntime.WindowReloadApp; the in-package test build replaces it via
// the windowReloader var below). The ctx may be a.ctx in production; a nil
// or never-completed ctx is safe — Wails accepts it as an opaque handle.
func (a *App) reloadWindowApp(ctx context.Context) {
	if windowReloader != nil {
		windowReloader(ctx)
	}
}

// windowReloader is the Wails primitive that navigates the webview back to
// the AssetServer's start URL, triggering re-bootstrap through the embedded
// stub (which re-fetches GetServerURL/GetSessionToken and navigates to the
// new port + token). Indirected through a package var so tests can stub it
// without booting Wails. Defaulted to wruntime.WindowReloadApp at init.
var windowReloader = func(ctx context.Context) {
	wruntime.WindowReloadApp(ctx)
}

// applyKeychainToEnv reads every known API key from the OS keychain and
// places it in the process env, but never overwrites an env var that was
// present in the launching shell. Iterion-injected values are tracked
// separately so Settings can edit/delete them during the current session.
func (a *App) applyKeychainToEnv() {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.shellEnvKeys == nil {
		a.shellEnvKeys = make(map[string]bool, len(KnownAPIKeys))
	}
	if a.injectedEnvKeys == nil {
		a.injectedEnvKeys = make(map[string]bool, len(KnownAPIKeys))
	}
	if a.injectedEnvValue == nil {
		a.injectedEnvValue = make(map[string]string, len(KnownAPIKeys))
	}

	for _, k := range KnownAPIKeys {
		if _, set := os.LookupEnv(k); set {
			a.shellEnvKeys[k] = true
			continue
		}
		if a.keychain == nil {
			continue
		}
		v, err := a.keychain.Get(k)
		if err != nil || v == "" {
			continue
		}
		_ = os.Setenv(k, v)
		a.injectedEnvKeys[k] = true
		a.injectedEnvValue[k] = v
	}
}

func (a *App) checkForUpdateAsync(ctx context.Context) {
	rel, err := a.updater.CheckForUpdate(ctx, a.config.Updater.Channel)
	if err != nil {
		log.Printf("desktop: update check failed: %v", err)
		return
	}
	if rel == nil {
		return
	}
	wruntime.EventsEmit(ctx, eventUpdateAvailable, rel)
}

func (a *App) updateCheckTicker(ctx context.Context) {
	t := time.NewTicker(4 * time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.checkForUpdateAsync(ctx)
		}
	}
}

func (a *App) persistWindowState(ctx context.Context) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.config == nil {
		return
	}
	a.config.Window = readWindowState(ctx, a.config.Window)
	if err := a.config.Save(); err != nil {
		log.Printf("desktop: failed to persist window state: %v", err)
	}
}
