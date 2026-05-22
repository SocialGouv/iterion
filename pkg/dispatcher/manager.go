package dispatcher

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/dispatcher/tracker"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// ManagerState describes where the Manager is in its lifecycle.
//
//	idle    — no dispatcher process; config may or may not be present
//	running — dispatcher active, dispatching candidates each tick
//	paused  — dispatcher active, dispatch suspended (Pause was called)
//	error   — last Start attempt failed; LastError carries the cause
type ManagerState string

const (
	ManagerStateIdle    ManagerState = "idle"
	ManagerStateRunning ManagerState = "running"
	ManagerStatePaused  ManagerState = "paused"
	ManagerStateError   ManagerState = "error"
)

// ManagerOptions configures NewManager.
type ManagerOptions struct {
	StoreDir    string // the iterion store dir (e.g. ./.iterion)
	NativeStore *native.Store
	Logger      *iterlog.Logger
	// DefaultBotsPaths seeds the dispatcher's bot registry when the
	// persisted config (dispatcher.json) does not set its own. Used
	// by the studio so the SPA's per-ticket bot picker and the
	// dispatcher's per-ticket bot resolution share the same view of
	// the host's bots without the operator having to configure them
	// twice.
	DefaultBotsPaths []string
	// DefaultsFn builds a zero-config dispatcher Config from the
	// host environment (typically by extracting the embedded bot
	// catalogue under <storeDir>/dispatcher/bots/). When set, the
	// Manager exposes POST /defaults/apply which saves the returned
	// config and starts the dispatcher in one call. nil = the
	// endpoint returns 501 Not Implemented.
	//
	// Injected from pkg/cli (where the embed.FS lives) so the
	// pkg/dispatcher package stays free of the templates dep.
	DefaultsFn func() (*Config, error)
}

// Manager owns the lifecycle of an in-process Dispatcher instance.
// It is the surface the studio talks to (HTTP) so an operator can
// configure, start, pause, and stop dispatching entirely from the SPA
// without dropping into a terminal.
//
// One Manager per studio server. Safe for concurrent use.
type Manager struct {
	storeDir         string
	configPath       string
	nativeStore      *native.Store
	logger           *iterlog.Logger
	defaultBotsPaths []string
	defaultsFn       func() (*Config, error)

	mu        sync.Mutex
	cfg       *Config
	state     ManagerState
	cur       *Dispatcher
	runner    ManagedRunner
	cancel    context.CancelFunc
	lastErr   error
	startedAt time.Time
}

// NewManager constructs a Manager and loads any persisted config from
// <store-dir>/dispatcher/dispatcher.json. The Manager starts in the
// idle state; call Start when ready.
func NewManager(opts ManagerOptions) (*Manager, error) {
	if opts.StoreDir == "" {
		return nil, errors.New("manager: store dir required")
	}
	if opts.NativeStore == nil {
		return nil, errors.New("manager: native store required")
	}
	if opts.Logger == nil {
		return nil, errors.New("manager: logger required")
	}
	m := &Manager{
		storeDir:         opts.StoreDir,
		configPath:       filepath.Join(opts.StoreDir, "dispatcher", "dispatcher.json"),
		nativeStore:      opts.NativeStore,
		logger:           opts.Logger,
		defaultBotsPaths: append([]string(nil), opts.DefaultBotsPaths...),
		defaultsFn:       opts.DefaultsFn,
		state:            ManagerStateIdle,
	}
	if cfg, err := loadConfigJSON(m.configPath); err == nil {
		m.cfg = cfg
	} else if !errors.Is(err, fs.ErrNotExist) {
		opts.Logger.Warn("manager: load persisted config (%s): %v", m.configPath, err)
	}
	if m.cfg != nil && len(m.cfg.Bots.Paths) == 0 {
		m.cfg.Bots.Paths = append([]string(nil), m.defaultBotsPaths...)
	}
	// Sweep stale claims left over from a dead local PID at server
	// boot, before any operator has clicked Start on the dispatcher.
	// Without this, /board would show "claimed by rog-<dead-pid>"
	// labels for the entire window between backend restart and the
	// operator manually starting the dispatcher (ticket 7221c7be).
	m.sweepStaleLocalClaimsAtBoot()
	return m, nil
}

// sweepStaleLocalClaimsAtBoot runs Adapter.SweepStaleClaims directly
// against the native store so the board is clean from the first
// /api/native/issues request, regardless of whether the dispatcher
// has been started yet. Best-effort: any error is logged at warn.
// Same isStale predicate as the dispatcher's start-time sweep.
func (m *Manager) sweepStaleLocalClaimsAtBoot() {
	adapter := native.NewAdapter(m.nativeStore)
	host, _ := osHostname()
	if host == "" {
		host = "dispatcher"
	}
	cleared, err := adapter.SweepStaleClaims(func(marker string) bool {
		return isStaleLocalMarker(marker, host)
	})
	if err != nil {
		m.logger.Warn("manager: boot-time stale-claim sweep failed: %v", err)
		return
	}
	if len(cleared) > 0 {
		m.logger.Info("manager: released %d stale claim(s) at boot from dead local PIDs: %v", len(cleared), cleared)
	}
}

// ManagerStatus is the snapshot the SPA reads via GET /status.
type ManagerStatus struct {
	State     ManagerState `json:"state"`
	HasConfig bool         `json:"has_config"`
	StartedAt *time.Time   `json:"started_at,omitempty"`
	LastError string       `json:"last_error,omitempty"`
}

// Status returns the current lifecycle state and any last error.
func (m *Manager) Status() ManagerStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := ManagerStatus{
		State:     m.state,
		HasConfig: m.cfg != nil,
		LastError: errString(m.lastErr),
	}
	if !m.startedAt.IsZero() {
		t := m.startedAt
		s.StartedAt = &t
	}
	return s
}

// Config returns the currently-active configuration (or nil if none
// has been written yet).
func (m *Manager) Config() *Config {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cfg
}

// SaveConfig validates and persists a new configuration. If a
// dispatcher is running, it is reloaded with the new settings; if it's
// idle, the config is saved for the next Start.
func (m *Manager) SaveConfig(cfg *Config) error {
	if cfg == nil {
		return errors.New("manager: config is nil")
	}
	cfg.SourcePath = m.configPath
	cfg.applyEnvAndPaths()
	cfg.applyDefaults()
	if len(cfg.Bots.Paths) == 0 {
		cfg.Bots.Paths = append([]string(nil), m.defaultBotsPaths...)
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	// Serialise the disk write + in-memory swap under m.mu so two
	// concurrent SaveConfig calls don't end with disk reflecting one
	// race winner and m.cfg the other (F-CD-8). The actor's Reload is
	// fired after the lock release because Reload itself locks; we
	// snapshot the running dispatcher pointer under the lock so a
	// concurrent Stop+Start doesn't make us call Reload on a freshly-
	// killed actor.
	m.mu.Lock()
	if err := saveConfigJSON(m.configPath, cfg); err != nil {
		m.mu.Unlock()
		return err
	}
	m.cfg = cfg
	cur := m.cur
	m.mu.Unlock()
	if cur != nil {
		cur.Reload(cfg)
	}
	return nil
}

// Start spins up a fresh Dispatcher from the persisted config. Returns
// an error and stays in StateError when the start sequence fails (bad
// workflow, missing tracker creds, port conflict, …).
func (m *Manager) Start() error {
	m.mu.Lock()
	if m.cur != nil {
		m.mu.Unlock()
		return errors.New("manager: already running")
	}
	cfg := m.cfg
	m.mu.Unlock()
	if cfg == nil {
		return errors.New("manager: no config — save one first")
	}

	// NewRoutingRunner returns a plain EngineRunner when
	// cfg.AssigneeWorkflows is empty (backward-compatible) or a
	// RoutingRunner that dispatches per assignee with cfg.Workflow as
	// the fallback.
	runner, err := NewRoutingRunner(cfg, m.logger)
	if err != nil {
		m.setError(err)
		return err
	}
	trk, err := buildTracker(cfg, m.nativeStore)
	if err != nil {
		_ = runner.Close()
		m.setError(err)
		return err
	}
	wsRoot := cfg.Workspace.Root
	if wsRoot == "" {
		wsRoot = filepath.Join(m.storeDir, "dispatcher", "workspaces")
	}
	workspaces, err := NewWorkspaces(wsRoot)
	if err != nil {
		_ = runner.Close()
		m.setError(err)
		return err
	}
	c, err := New(Options{
		Config:     cfg,
		Tracker:    trk,
		Runner:     runner,
		Workspaces: workspaces,
		Logger:     m.logger,
		StoreDir:   m.storeDir,
	})
	if err != nil {
		_ = runner.Close()
		m.setError(err)
		return err
	}

	ctx, cancel := context.WithCancel(context.Background())
	c.Start(ctx)

	m.mu.Lock()
	m.cur = c
	m.runner = runner
	m.cancel = cancel
	m.state = ManagerStateRunning
	m.lastErr = nil
	m.startedAt = time.Now().UTC()
	m.mu.Unlock()
	m.logger.Info("manager: dispatcher started (workflow=%s, tracker=%s)", cfg.Workflow, trk.Name())
	return nil
}

// Stop tears down the active Dispatcher. No-op when idle.
func (m *Manager) Stop() {
	m.mu.Lock()
	cur, runner, cancel := m.cur, m.runner, m.cancel
	m.cur, m.runner, m.cancel = nil, nil, nil
	m.state = ManagerStateIdle
	m.startedAt = time.Time{}
	m.mu.Unlock()
	if cur != nil {
		cur.Stop()
	}
	if cancel != nil {
		cancel()
	}
	if runner != nil {
		if err := runner.Close(); err != nil {
			m.logger.Warn("manager: runner close: %v", err)
		}
	}
}

// Pause suspends new dispatches on the active Dispatcher. Runs in flight
// continue. Returns an error when no dispatcher is running.
func (m *Manager) Pause() error {
	m.mu.Lock()
	cur := m.cur
	if cur == nil {
		m.mu.Unlock()
		return errors.New("manager: not running")
	}
	m.state = ManagerStatePaused
	m.mu.Unlock()
	cur.Pause()
	return nil
}

// Resume undoes Pause.
func (m *Manager) Resume() error {
	m.mu.Lock()
	cur := m.cur
	if cur == nil {
		m.mu.Unlock()
		return errors.New("manager: not running")
	}
	m.state = ManagerStateRunning
	m.mu.Unlock()
	cur.Resume()
	return nil
}

// Current returns the active Dispatcher (or nil when idle). Callers
// must not retain the pointer past the next Stop.
func (m *Manager) Current() *Dispatcher {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cur
}

// CancelRun signals the active Dispatcher to cancel a run by its RunID.
// Returns true when a matching in-flight run was found. Falsey when the
// dispatcher is idle or the runID is unknown to it — typical when the
// runID belongs to a manual studio launch (handled by the runview
// Manager) or is already terminal.
func (m *Manager) CancelRun(runID string) bool {
	cur := m.Current()
	if cur == nil {
		return false
	}
	return cur.CancelByRunID(runID)
}

func (m *Manager) setError(err error) {
	m.mu.Lock()
	m.state = ManagerStateError
	m.lastErr = err
	m.mu.Unlock()
}

// ---------------------------------------------------------------------------
// tracker factory
// ---------------------------------------------------------------------------

// buildTracker constructs a tracker.Tracker matching cfg.Tracker.Kind.
// Used by both the studio's Manager and the standalone `iterion
// dispatch` CLI so the wiring stays in one place.
func buildTracker(cfg *Config, ns *native.Store) (tracker.Tracker, error) {
	switch cfg.Tracker.Kind {
	case TrackerKindNative:
		return native.NewAdapter(ns), nil
	case TrackerKindGitHub:
		return buildGitHubTracker(cfg.Tracker.GitHub)
	case TrackerKindForgejo:
		return buildForgejoTracker(cfg.Tracker.Forgejo)
	default:
		return nil, fmt.Errorf("dispatcher: unsupported tracker kind %q", cfg.Tracker.Kind)
	}
}

// buildGitHubTracker / buildForgejoTracker are defined in pkg/cli/dispatch.go
// — declared here as variables so the package compiles when pkg/cli
// isn't wired. Production wiring overrides them in package init.
var (
	buildGitHubTracker = func(*GitHubTrackerConfig) (tracker.Tracker, error) {
		return nil, errors.New("github tracker factory not registered")
	}
	buildForgejoTracker = func(*ForgejoTrackerConfig) (tracker.Tracker, error) {
		return nil, errors.New("forgejo tracker factory not registered")
	}
)

// RegisterTrackerFactories installs the production GitHub + Forgejo
// factories. Called from pkg/cli or any consumer that wants those
// adapters available; v1 only wires it from there.
func RegisterTrackerFactories(
	gh func(*GitHubTrackerConfig) (tracker.Tracker, error),
	fj func(*ForgejoTrackerConfig) (tracker.Tracker, error),
) {
	if gh != nil {
		buildGitHubTracker = gh
	}
	if fj != nil {
		buildForgejoTracker = fj
	}
}

// ---------------------------------------------------------------------------
// config persistence
// ---------------------------------------------------------------------------

func loadConfigJSON(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("config json: %w", err)
	}
	cfg.SourcePath = path
	cfg.applyEnvAndPaths()
	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func saveConfigJSON(path string, cfg *Config) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("config json: mkdir: %w", err)
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("config json: marshal: %w", err)
	}
	if err := store.WriteFileAtomic(path, data, 0o644); err != nil {
		return fmt.Errorf("config json: write: %w", err)
	}
	return nil
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// ---------------------------------------------------------------------------
// HTTP
// ---------------------------------------------------------------------------

// RegisterRoutes wires the Manager's REST surface onto mux. It owns
// every /api/v1/dispatcher/* endpoint: lifecycle (start/pause/resume/
// stop/status), config (GET/PUT), and the operational endpoints
// (state/refresh/issues/.../cancel/ws) which delegate to the active
// Dispatcher when one is running.
func (m *Manager) RegisterRoutes(mux *http.ServeMux, prefix string) {
	m.RegisterRoutesWithMiddleware(mux, prefix, nil)
}

// RegisterRoutesWithMiddleware mounts the routes through a caller-
// supplied wrapper (typically the studio server's requireAuth) so
// every operator endpoint is gated when the server binds non-loopback.
// nil falls back to the identity wrap (same as RegisterRoutes).
func (m *Manager) RegisterRoutesWithMiddleware(mux *http.ServeMux, prefix string, wrap func(http.Handler) http.Handler) {
	p := strings.TrimSuffix(prefix, "/")
	if wrap == nil {
		wrap = func(h http.Handler) http.Handler { return h }
	}
	mux.Handle("GET "+p+"/status", wrap(http.HandlerFunc(m.handleStatus)))
	mux.Handle("GET "+p+"/config", wrap(http.HandlerFunc(m.handleGetConfig)))
	mux.Handle("PUT "+p+"/config", wrap(http.HandlerFunc(m.handlePutConfig)))
	mux.Handle("POST "+p+"/start", wrap(http.HandlerFunc(m.handleStart)))
	mux.Handle("POST "+p+"/stop", wrap(http.HandlerFunc(m.handleStop)))
	mux.Handle("POST "+p+"/pause", wrap(http.HandlerFunc(m.handlePause)))
	mux.Handle("POST "+p+"/resume", wrap(http.HandlerFunc(m.handleResume)))
	mux.Handle("POST "+p+"/defaults/apply", wrap(http.HandlerFunc(m.handleApplyDefaults)))

	mux.Handle("GET "+p+"/state", wrap(http.HandlerFunc(m.handleSnapshot)))
	mux.Handle("POST "+p+"/refresh", wrap(http.HandlerFunc(m.handleRefresh)))
	mux.Handle("POST "+p+"/reload", wrap(http.HandlerFunc(m.handleReload)))
	mux.Handle("GET "+p+"/issues/{id}", wrap(http.HandlerFunc(m.handleIssueDetail)))
	mux.Handle("POST "+p+"/issues/{id}/cancel", wrap(http.HandlerFunc(m.handleIssueCancel)))
	mux.Handle("GET "+p+"/ws", wrap(http.HandlerFunc(m.handleWS)))
}

func (m *Manager) handleStatus(w http.ResponseWriter, _ *http.Request) {
	WriteJSON(w, http.StatusOK, m.Status())
}

func (m *Manager) handleGetConfig(w http.ResponseWriter, _ *http.Request) {
	cfg := m.Config()
	if cfg == nil {
		WriteJSON(w, http.StatusNotFound, map[string]string{"error": "no config persisted yet"})
		return
	}
	WriteJSON(w, http.StatusOK, redactedConfig(cfg))
}

// redactedConfig returns a deep-ish copy of cfg with secret values
// (tracker tokens) masked. Without this masking, GET /config
// surfaces the raw OAuth/PAT tokens to anyone who can reach the
// management port — they're loaded from env at boot but stay
// in-memory in their literal form.
func redactedConfig(cfg *Config) *Config {
	out := *cfg
	if out.Tracker.GitHub != nil {
		gh := *out.Tracker.GitHub
		if gh.Token != "" {
			gh.Token = "***"
		}
		out.Tracker.GitHub = &gh
	}
	if out.Tracker.Forgejo != nil {
		fj := *out.Tracker.Forgejo
		if fj.Token != "" {
			fj.Token = "***"
		}
		out.Tracker.Forgejo = &fj
	}
	return &out
}

func (m *Manager) handlePutConfig(w http.ResponseWriter, r *http.Request) {
	var cfg Config
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		WriteErr(w, http.StatusBadRequest, fmt.Errorf("parse body: %w", err))
		return
	}
	if err := m.SaveConfig(&cfg); err != nil {
		WriteErr(w, http.StatusBadRequest, err)
		return
	}
	WriteJSON(w, http.StatusOK, m.Config())
}

func (m *Manager) handleStart(w http.ResponseWriter, _ *http.Request) {
	if err := m.Start(); err != nil {
		WriteErr(w, http.StatusBadRequest, err)
		return
	}
	WriteJSON(w, http.StatusAccepted, m.Status())
}

// handleApplyDefaults builds the host's default Config via the
// injected DefaultsFn, persists it, and starts the dispatcher. The
// endpoint is the studio's "auto-configure & start" one-click path;
// the bot also calls it (via the future dispatcher.control capability)
// after moving tickets to `ready` when no config exists yet.
//
// Returns 501 if no DefaultsFn was injected (out-of-process or test
// builds), 409 if a config already exists (callers should DELETE or
// PUT to overwrite — applying defaults over an existing config would
// silently clobber operator edits), 400 if the defaults builder fails
// (typically a binary built before the templates were embedded),
// 202 + ManagerStatus on success.
func (m *Manager) handleApplyDefaults(w http.ResponseWriter, _ *http.Request) {
	if m.defaultsFn == nil {
		WriteErr(w, http.StatusNotImplemented, errors.New("dispatcher defaults: not wired in this build"))
		return
	}
	if m.Config() != nil {
		WriteErr(w, http.StatusConflict, errors.New("dispatcher defaults: a config already exists; PUT /config to overwrite or DELETE first"))
		return
	}
	cfg, err := m.defaultsFn()
	if err != nil {
		WriteErr(w, http.StatusBadRequest, err)
		return
	}
	if err := m.SaveConfig(cfg); err != nil {
		WriteErr(w, http.StatusBadRequest, err)
		return
	}
	if err := m.Start(); err != nil {
		WriteErr(w, http.StatusBadRequest, err)
		return
	}
	WriteJSON(w, http.StatusAccepted, m.Status())
}

func (m *Manager) handleStop(w http.ResponseWriter, _ *http.Request) {
	m.Stop()
	WriteJSON(w, http.StatusAccepted, m.Status())
}

func (m *Manager) handlePause(w http.ResponseWriter, _ *http.Request) {
	if err := m.Pause(); err != nil {
		WriteErr(w, http.StatusBadRequest, err)
		return
	}
	WriteJSON(w, http.StatusAccepted, m.Status())
}

func (m *Manager) handleResume(w http.ResponseWriter, _ *http.Request) {
	if err := m.Resume(); err != nil {
		WriteErr(w, http.StatusBadRequest, err)
		return
	}
	WriteJSON(w, http.StatusAccepted, m.Status())
}

// Operational endpoints — delegate to the active dispatcher.

func (m *Manager) handleSnapshot(w http.ResponseWriter, _ *http.Request) {
	cur := m.Current()
	if cur == nil {
		// Idle: return a stub snapshot so the SPA always has a
		// rendered table (empty running/retries) without 404s.
		WriteJSON(w, http.StatusOK, Snapshot{
			GeneratedAt: time.Now().UTC(),
		})
		return
	}
	WriteJSON(w, http.StatusOK, cur.Snapshot())
}

func (m *Manager) handleRefresh(w http.ResponseWriter, _ *http.Request) {
	cur := m.Current()
	if cur == nil {
		WriteErr(w, http.StatusBadRequest, errors.New("manager: not running"))
		return
	}
	cur.Refresh()
	WriteJSON(w, http.StatusAccepted, map[string]bool{"queued": true})
}

func (m *Manager) handleReload(w http.ResponseWriter, _ *http.Request) {
	cur := m.Current()
	if cur == nil {
		WriteErr(w, http.StatusBadRequest, errors.New("manager: not running"))
		return
	}
	cfg := m.Config()
	if cfg == nil {
		WriteErr(w, http.StatusBadRequest, errors.New("manager: no config"))
		return
	}
	cur.Reload(cfg)
	WriteJSON(w, http.StatusOK, m.Status())
}

func (m *Manager) handleIssueDetail(w http.ResponseWriter, r *http.Request) {
	cur := m.Current()
	if cur == nil {
		http.Error(w, "dispatcher not running", http.StatusNotFound)
		return
	}
	id := r.PathValue("id")
	snap := cur.Snapshot()
	for _, row := range snap.Running {
		if row.IssueID == id {
			WriteJSON(w, http.StatusOK, row)
			return
		}
	}
	for _, row := range snap.Retries {
		if row.IssueID == id {
			WriteJSON(w, http.StatusOK, row)
			return
		}
	}
	http.Error(w, "issue not tracked by dispatcher", http.StatusNotFound)
}

func (m *Manager) handleIssueCancel(w http.ResponseWriter, r *http.Request) {
	cur := m.Current()
	if cur == nil {
		WriteErr(w, http.StatusBadRequest, errors.New("manager: not running"))
		return
	}
	id := r.PathValue("id")
	cur.Cancel(id)
	WriteJSON(w, http.StatusAccepted, map[string]string{"issue_id": id, "status": "cancel_requested"})
}

func (m *Manager) handleWS(w http.ResponseWriter, r *http.Request) {
	cur := m.Current()
	if cur == nil {
		http.Error(w, "dispatcher not running", http.StatusServiceUnavailable)
		return
	}
	// Delegate to the dispatcher's existing WS handler, which knows
	// how to upgrade + attach to its bridge.
	cur.handleWS(w, r)
}
