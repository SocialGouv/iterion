//go:build desktop

package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
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
	ctx          context.Context
	server       serverController
	serverURL    string
	sessionToken string

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

	// Generate a one-time session token for the HTTP server. Used by the
	// frontend stub to set the HttpOnly cookie and by every subsequent
	// request to authorise itself. 32 bytes hex = 64 chars, ~256 bits of
	// entropy. crypto/rand.Read on Linux/macOS uses getrandom(2);
	// non-fatal on a flaky read because we'd rather crash loudly than
	// run unauthenticated.
	tok := make([]byte, 32)
	if _, err := rand.Read(tok); err != nil {
		log.Fatalf("desktop: failed to generate session token: %v", err)
	}
	a.sessionToken = hex.EncodeToString(tok)

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

	// Bring up the HTTP server for the current project (or no project on
	// first run — the editor SPA's useDesktop hook routes to /welcome based
	// on IsFirstRunPending).
	a.server = NewServerHost()
	if err := a.startServerForCurrentProject(ctx); err != nil {
		log.Printf("desktop: server failed to start: %v", err)
		// Surface the error: GetServerURL stays "" and the AssetServer
		// reverse-proxy returns 503 to the WebView until the next
		// successful start (e.g. after a project switch).
		return
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
	// Order matters: stop the server first so in-flight runs flip to
	// failed_resumable, then drop the single-instance lock.
	if a.server != nil {
		a.server.Stop()
	}
	if a.instLock != nil {
		_ = a.instLock.Release()
	}
}

// currentProjectServerDirs returns the editor working directory/store pair
// for the project currently selected in config. On first-run / empty config
// it uses the user's home directory as the working dir; the SPA's onboarding
// flow then prompts for a real project.
func (a *App) currentProjectServerDirsLocked() (dir, storeDir string) {
	if p := a.config.CurrentProject(); p != nil {
		dir = p.Dir
		storeDir = p.StoreDir
	}
	if dir == "" {
		// Fallback: home dir (so the SPA can mount and onboarding can run).
		home, err := os.UserHomeDir()
		if err == nil {
			dir = home
		}
	}
	return dir, storeDir
}

// startServerForCurrentProject brings up the HTTP server for the project
// currently selected in config.
func (a *App) startServerForCurrentProject(ctx context.Context) error {
	a.mu.Lock()
	dir, storeDir := a.currentProjectServerDirsLocked()
	a.mu.Unlock()

	addr, err := a.server.Start(ctx, dir, storeDir, a.sessionToken)
	if err != nil {
		return err
	}
	a.mu.Lock()
	a.serverURL = "http://" + addr + "/"
	a.mu.Unlock()
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
	a.mu.Lock()
	p := a.config.CurrentProject()
	var current *Project
	if p != nil {
		cp := *p
		current = &cp
	}
	dir, storeDir := a.currentProjectServerDirsLocked()
	a.mu.Unlock()

	a.server.Stop()
	addr, err := a.server.Start(ctx, dir, storeDir, a.sessionToken)
	if err != nil {
		return nil, err
	}
	a.mu.Lock()
	a.serverURL = "http://" + addr + "/"
	a.mu.Unlock()
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
