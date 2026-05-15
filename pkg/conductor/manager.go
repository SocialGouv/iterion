package conductor

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

	"github.com/SocialGouv/iterion/pkg/conductor/native"
	"github.com/SocialGouv/iterion/pkg/conductor/tracker"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// ManagerState describes where the Manager is in its lifecycle.
//
//	idle    — no conductor process; config may or may not be present
//	running — conductor active, dispatching candidates each tick
//	paused  — conductor active, dispatch suspended (Pause was called)
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
}

// Manager owns the lifecycle of an in-process Conductor instance.
// It is the surface the editor talks to (HTTP) so an operator can
// configure, start, pause, and stop conducting entirely from the SPA
// without dropping into a terminal.
//
// One Manager per editor server. Safe for concurrent use.
type Manager struct {
	storeDir    string
	configPath  string
	nativeStore *native.Store
	logger      *iterlog.Logger

	mu        sync.Mutex
	cfg       *Config
	state     ManagerState
	cur       *Conductor
	runner    *EngineRunner
	cancel    context.CancelFunc
	lastErr   error
	startedAt time.Time
}

// NewManager constructs a Manager and loads any persisted config from
// <store-dir>/conductor/conductor.json. The Manager starts in the
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
		storeDir:    opts.StoreDir,
		configPath:  filepath.Join(opts.StoreDir, "conductor", "conductor.json"),
		nativeStore: opts.NativeStore,
		logger:      opts.Logger,
		state:       ManagerStateIdle,
	}
	if cfg, err := loadConfigJSON(m.configPath); err == nil {
		m.cfg = cfg
	} else if !errors.Is(err, fs.ErrNotExist) {
		opts.Logger.Warn("manager: load persisted config (%s): %v", m.configPath, err)
	}
	return m, nil
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
// conductor is running, it is reloaded with the new settings; if it's
// idle, the config is saved for the next Start.
func (m *Manager) SaveConfig(cfg *Config) error {
	if cfg == nil {
		return errors.New("manager: config is nil")
	}
	cfg.SourcePath = m.configPath
	cfg.applyEnvAndPaths()
	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		return err
	}
	if err := saveConfigJSON(m.configPath, cfg); err != nil {
		return err
	}
	m.mu.Lock()
	m.cfg = cfg
	cur := m.cur
	m.mu.Unlock()
	if cur != nil {
		cur.Reload(cfg)
	}
	return nil
}

// Start spins up a fresh Conductor from the persisted config. Returns
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

	runner, err := NewEngineRunner(cfg.Workflow, m.logger)
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
		wsRoot = filepath.Join(m.storeDir, "conductor", "workspaces")
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
	m.logger.Info("manager: conductor started (workflow=%s, tracker=%s)", cfg.Workflow, trk.Name())
	return nil
}

// Stop tears down the active Conductor. No-op when idle.
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

// Pause suspends new dispatches on the active Conductor. Runs in flight
// continue. Returns an error when no conductor is running.
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

// Current returns the active Conductor (or nil when idle). Callers
// must not retain the pointer past the next Stop.
func (m *Manager) Current() *Conductor {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cur
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
// Used by both the editor's Manager and the standalone `iterion
// conduct` CLI so the wiring stays in one place.
func buildTracker(cfg *Config, ns *native.Store) (tracker.Tracker, error) {
	switch cfg.Tracker.Kind {
	case TrackerKindNative:
		return native.NewAdapter(ns), nil
	case TrackerKindGitHub:
		return buildGitHubTracker(cfg.Tracker.GitHub)
	case TrackerKindForgejo:
		return buildForgejoTracker(cfg.Tracker.Forgejo)
	default:
		return nil, fmt.Errorf("conductor: unsupported tracker kind %q", cfg.Tracker.Kind)
	}
}

// buildGitHubTracker / buildForgejoTracker are defined in pkg/cli/conduct.go
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
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("config json: write: %w", err)
	}
	return os.Rename(tmp, path)
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
// every /api/v1/conductor/* endpoint: lifecycle (start/pause/resume/
// stop/status), config (GET/PUT), and the operational endpoints
// (state/refresh/issues/.../cancel/ws) which delegate to the active
// Conductor when one is running.
func (m *Manager) RegisterRoutes(mux *http.ServeMux, prefix string) {
	p := strings.TrimSuffix(prefix, "/")
	mux.HandleFunc("GET "+p+"/status", m.handleStatus)
	mux.HandleFunc("GET "+p+"/config", m.handleGetConfig)
	mux.HandleFunc("PUT "+p+"/config", m.handlePutConfig)
	mux.HandleFunc("POST "+p+"/start", m.handleStart)
	mux.HandleFunc("POST "+p+"/stop", m.handleStop)
	mux.HandleFunc("POST "+p+"/pause", m.handlePause)
	mux.HandleFunc("POST "+p+"/resume", m.handleResume)

	mux.HandleFunc("GET "+p+"/state", m.handleSnapshot)
	mux.HandleFunc("POST "+p+"/refresh", m.handleRefresh)
	mux.HandleFunc("POST "+p+"/reload", m.handleReload)
	mux.HandleFunc("GET "+p+"/issues/{id}", m.handleIssueDetail)
	mux.HandleFunc("POST "+p+"/issues/{id}/cancel", m.handleIssueCancel)
	mux.HandleFunc("GET "+p+"/ws", m.handleWS)
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
	WriteJSON(w, http.StatusOK, cfg)
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

// Operational endpoints — delegate to the active conductor.

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
		http.Error(w, "conductor not running", http.StatusNotFound)
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
	http.Error(w, "issue not tracked by conductor", http.StatusNotFound)
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
		http.Error(w, "conductor not running", http.StatusServiceUnavailable)
		return
	}
	// Delegate to the conductor's existing WS handler, which knows
	// how to upgrade + attach to its bridge.
	cur.handleWS(w, r)
}
