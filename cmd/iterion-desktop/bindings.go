//go:build desktop

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"

	cli "github.com/SocialGouv/iterion/pkg/cli"
	wruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

// AppInfo is the public shape of GetAppInfo().
type AppInfo struct {
	Version       string `json:"version"`
	Commit        string `json:"commit"`
	OS            string `json:"os"`
	Arch          string `json:"arch"`
	License       string `json:"license"`
	Homepage      string `json:"homepage"`
	IssueTracker  string `json:"issue_tracker"`
	Documentation string `json:"documentation"`
}

// GetServerURL returns the absolute http://127.0.0.1:<port>/ address the
// embedded editor server is bound to, or "" if the server failed to start.
// The editor SPA uses this binding for absolute URLs that cannot be
// proxied through Wails' AssetServer — most notably WebSocket dialer URLs
// for /api/ws and /api/ws/runs/* (Wails AssetServer rejects WS upgrades
// with 501). HTTP API calls flow through the AssetServer reverse-proxy
// (cmd/iterion-desktop/asset_proxy.go) and don't need this binding.
func (a *App) GetServerURL() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.serverURL
}

// GetSessionToken returns the one-time-per-launch session token. The
// AssetServer reverse-proxy attaches this as the iterion_session cookie
// on every forwarded HTTP request, so the SPA never has to learn it for
// same-origin proxied calls. The SPA does need it for cross-origin
// WebSocket connections (it goes on the ?t=<token> query string the
// session middleware accepts on non-bootstrap paths).
func (a *App) GetSessionToken() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.sessionToken
}

// GetAppInfo returns version + commit + platform metadata for the About tab.
func (a *App) GetAppInfo() AppInfo {
	return AppInfo{
		Version:       cli.RawVersion(),
		Commit:        cli.RawCommit(),
		OS:            goruntime.GOOS,
		Arch:          goruntime.GOARCH,
		License:       "MIT",
		Homepage:      "https://github.com/SocialGouv/iterion",
		IssueTracker:  "https://github.com/SocialGouv/iterion/issues",
		Documentation: "https://github.com/SocialGouv/iterion/tree/main/docs",
	}
}

// Quit asks Wails to terminate the app cleanly. Triggers OnBeforeClose
// then OnShutdown.
func (a *App) Quit() {
	wruntime.Quit(a.ctx)
}

// OpenExternal opens the given URL in the user's default browser.
func (a *App) OpenExternal(url string) error {
	wruntime.BrowserOpenURL(a.ctx, url)
	return nil
}

// RevealInFinder reveals the given path in the OS file manager (Finder /
// Explorer / xdg-open).
func (a *App) RevealInFinder(path string) error {
	if path == "" {
		return fmt.Errorf("empty path")
	}
	var cmd *exec.Cmd
	switch goruntime.GOOS {
	case "darwin":
		cmd = exec.Command("open", "-R", path)
	case "windows":
		cmd = exec.Command("explorer", "/select,", path)
	default:
		cmd = exec.Command("xdg-open", filepath.Dir(path))
	}
	return cmd.Start()
}

// ── Project bindings ─────────────────────────────────────────────────────

// ListProjects returns the recent projects, MRU-ordered.
func (a *App) ListProjects() []Project {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.config == nil {
		return nil
	}
	out := make([]Project, len(a.config.RecentProjects))
	copy(out, a.config.RecentProjects)
	return out
}

// GetCurrentProject returns the active project, or nil.
func (a *App) GetCurrentProject() *Project {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.config == nil {
		return nil
	}
	return a.config.CurrentProject()
}

// addProject registers the given directory as the current project and
// persists config. The caller must validate dir before calling this helper.
func (a *App) addProject(abs string) (*Project, error) {
	a.mu.Lock()
	p := a.config.AddProject(abs)
	if err := a.config.Save(); err != nil {
		a.mu.Unlock()
		return nil, err
	}
	a.mu.Unlock()
	return &p, nil
}

// addAndSwitchProject registers the given directory as a project and switches
// the embedded editor server to it before returning. Keeping the
// config/current project and backend workdir in lockstep is required for both
// first-run onboarding and the normal project switcher; otherwise the SPA can
// show the new currentProject while /api/* still writes to the previous
// workdir.
func (a *App) addAndSwitchProject(dir string) (*Project, error) {
	abs, err := validateProjectDir(dir)
	if err != nil {
		return nil, err
	}
	p, err := a.addProject(abs)
	if err != nil {
		return nil, err
	}
	if _, err := a.restartServerForCurrentProject(a.ctx); err != nil {
		return nil, err
	}
	return p, nil
}

// AddProject registers the given directory as a project, switches the backend
// to it, and emits project:switched so the already-mounted editor SPA reloads
// onto the new project's server state. Use AddProjectSilently during first-run
// onboarding, where the Welcome flow must continue through API keys / CLI
// checks before the final deliberate reload.
func (a *App) AddProject(dir string) (*Project, error) {
	p, err := a.addAndSwitchProject(dir)
	if err != nil {
		return nil, err
	}
	wruntime.EventsEmit(a.ctx, eventProjectSwitched, p)
	return p, nil
}

// AddProjectSilently registers the given directory as the current project and
// restarts the embedded editor server without emitting project:switched. This
// is intentionally scoped to first-run onboarding: selecting/scaffolding a
// project must not reload the Welcome SPA before FirstRunDone is persisted.
func (a *App) AddProjectSilently(dir string) (*Project, error) {
	return a.addAndSwitchProject(dir)
}

func validateProjectDir(dir string) (string, error) {
	if dir == "" {
		return "", fmt.Errorf("empty directory")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("invalid path: %w", err)
	}
	st, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("path not found: %w", err)
	}
	if !st.IsDir() {
		return "", fmt.Errorf("not a directory: %s", abs)
	}
	return abs, nil
}

// RemoveProject drops the project from recents. Filesystem unaffected.
func (a *App) RemoveProject(id string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.config.RemoveProject(id) {
		return fmt.Errorf("project not found: %s", id)
	}
	return a.config.Save()
}

// SwitchProject restarts the editor server pointing at the given project.
// The frontend reloads the window via the "project:switched" event.
func (a *App) SwitchProject(id string) error {
	a.mu.Lock()
	if !a.config.SetCurrentProject(id) {
		a.mu.Unlock()
		return fmt.Errorf("project not found: %s", id)
	}
	if err := a.config.Save(); err != nil {
		a.mu.Unlock()
		return err
	}
	a.mu.Unlock()

	// Stop & restart server. We do this OUTSIDE the lock since Stop is
	// blocking and Start re-acquires it briefly.
	current, err := a.restartServerForCurrentProject(a.ctx)
	if err != nil {
		return err
	}
	wruntime.EventsEmit(a.ctx, eventProjectSwitched, current)
	return nil
}

// PickProjectDirectory opens a native folder picker. Returns "" if the user
// cancelled.
func (a *App) PickProjectDirectory() (string, error) {
	dir, err := wruntime.OpenDirectoryDialog(a.ctx, wruntime.OpenDialogOptions{
		Title: "Choose a project folder",
	})
	if err != nil {
		return "", err
	}
	return dir, nil
}

// ScaffoldProject runs the same logic as `iterion init` against the given
// directory. Used by the onboarding "Create new project here" flow.
func (a *App) ScaffoldProject(dir string) error {
	printer := cli.NewPrinter(cli.OutputJSON)
	return cli.RunInit(cli.InitOptions{Dir: dir}, printer)
}

// ── Keychain bindings ────────────────────────────────────────────────────

// GetKnownSecretKeys returns the canonical list of API key names the UI
// should present to the user.
func (a *App) GetKnownSecretKeys() []string {
	out := make([]string, len(KnownAPIKeys))
	copy(out, KnownAPIKeys)
	return out
}

// SecretStatus is the wire shape used by Settings UI. We deliberately
// never expose the secret value to JS — only presence flags.
type SecretStatus struct {
	Key      string `json:"key"`
	Stored   bool   `json:"stored"`
	Shadowed bool   `json:"shadowed"`
}

// GetSecretStatuses returns presence/shadow status for every known key.
// Values are NEVER returned to JS — see SecretStatus.
func (a *App) GetSecretStatuses() []SecretStatus {
	a.mu.RLock()
	kc := a.keychain
	shadowedByShell := make(map[string]bool, len(a.shellEnvKeys))
	for k, v := range a.shellEnvKeys {
		shadowedByShell[k] = v
	}
	a.mu.RUnlock()
	out := make([]SecretStatus, 0, len(KnownAPIKeys))
	for _, k := range KnownAPIKeys {
		stored := false
		if kc != nil {
			if v, err := kc.Get(k); err == nil && v != "" {
				stored = true
			}
		}
		out = append(out, SecretStatus{Key: k, Stored: stored, Shadowed: shadowedByShell[k]})
	}
	return out
}

// SetSecret stores the secret in the OS keychain and immediately injects
// it into the process env (unless an env var of the same name already exists,
// in which case the env wins — consistent with the precedence rule).
func (a *App) SetSecret(key, value string) error {
	a.mu.RLock()
	kc := a.keychain
	shadowed := a.shellEnvKeys != nil && a.shellEnvKeys[key]
	a.mu.RUnlock()
	if kc == nil {
		return fmt.Errorf("keychain unavailable")
	}
	if err := kc.Set(key, value); err != nil {
		return err
	}
	if !shadowed {
		_ = os.Setenv(key, value)
		a.mu.Lock()
		if a.injectedEnvKeys == nil {
			a.injectedEnvKeys = make(map[string]bool, len(KnownAPIKeys))
		}
		if a.injectedEnvValue == nil {
			a.injectedEnvValue = make(map[string]string, len(KnownAPIKeys))
		}
		a.injectedEnvKeys[key] = true
		a.injectedEnvValue[key] = value
		a.mu.Unlock()
	}
	return nil
}

// DeleteSecret removes the entry from the keychain. If Iterion injected
// that key into the current process env, it is removed immediately too.
// Env vars that existed in the launching shell are never unset.
func (a *App) DeleteSecret(key string) error {
	a.mu.RLock()
	kc := a.keychain
	shadowed := a.shellEnvKeys != nil && a.shellEnvKeys[key]
	a.mu.RUnlock()
	if kc == nil {
		return fmt.Errorf("keychain unavailable")
	}
	if err := kc.Delete(key); err != nil {
		return err
	}
	if !shadowed {
		a.mu.Lock()
		injected := a.injectedEnvKeys != nil && a.injectedEnvKeys[key]
		injectedValue := ""
		if a.injectedEnvValue != nil {
			injectedValue = a.injectedEnvValue[key]
		}
		if injected {
			delete(a.injectedEnvKeys, key)
			delete(a.injectedEnvValue, key)
		}
		a.mu.Unlock()
		if injected {
			if cur, ok := os.LookupEnv(key); ok && cur == injectedValue {
				_ = os.Unsetenv(key)
			}
		}
	}
	return nil
}

// ── External-CLI bindings ────────────────────────────────────────────────

// DetectExternalCLIs checks for the presence of `claude`, `codex`, `git`.
// 60s in-memory cache; pass force=true to bypass.
func (a *App) DetectExternalCLIs(force bool) []CLIStatus {
	return DetectExternalCLIs(force)
}

// ── First-run / config bindings ──────────────────────────────────────────

// IsFirstRunPending reports whether the welcome flow should be shown.
func (a *App) IsFirstRunPending() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.config == nil {
		return true
	}
	return !a.config.FirstRunDone
}

// MarkFirstRunDone flips the FirstRunDone flag and persists.
func (a *App) MarkFirstRunDone() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.config.FirstRunDone = true
	return a.config.Save()
}

// ── Updater bindings ─────────────────────────────────────────────────────

// CheckForUpdate consults the manifest. Returns nil if up-to-date.
func (a *App) CheckForUpdate() (*Release, error) {
	a.mu.RLock()
	u := a.updater
	channel := a.config.Updater.Channel
	a.mu.RUnlock()
	if u == nil {
		return nil, fmt.Errorf("updater unavailable")
	}
	if channel == "" {
		channel = ChannelStable
	}
	return u.CheckForUpdate(a.ctx, channel)
}

// DownloadAndApplyUpdate downloads + verifies + swaps the binary.
func (a *App) DownloadAndApplyUpdate() error {
	a.mu.RLock()
	u := a.updater
	a.mu.RUnlock()
	if u == nil {
		return fmt.Errorf("updater unavailable")
	}
	rel, err := u.CheckForUpdate(a.ctx, a.config.Updater.Channel)
	if err != nil {
		return err
	}
	if rel == nil {
		return fmt.Errorf("no update available")
	}
	progress := func(p float64) {
		wruntime.EventsEmit(a.ctx, eventUpdateProgress, p)
	}
	if err := u.DownloadAndApply(a.ctx, rel, progress); err != nil {
		return err
	}
	wruntime.EventsEmit(a.ctx, eventUpdateApplied, rel)
	return nil
}
