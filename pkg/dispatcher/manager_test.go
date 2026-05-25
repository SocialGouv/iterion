package dispatcher

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
)

// Pure-helper + lifecycle coverage for pkg/dispatcher/manager.go.
// Lifecycle paths that require a real tracker (Start → Reload → Stop
// chain, runner orchestration) are intentionally not covered here —
// they're integration-shaped and need either a real GitHub adapter or
// a fully-stubbed tracker.Factory wiring.

func newTestStoreDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "dispatcher"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	return dir
}

func newTestNativeStore(t *testing.T, root string) *native.Store {
	t.Helper()
	s, err := native.NewStore(filepath.Join(root, "board"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return s
}

// newTestLogger is defined in hooks_test.go; reuse it.

// ---- NewManager ----

func TestNewManager_RequiresStoreDir(t *testing.T) {
	_, err := NewManager(ManagerOptions{
		NativeStore: nil,
		Logger:      newTestLogger(),
	})
	if err == nil || !contains(err.Error(), "store dir") {
		t.Errorf("expected store-dir error, got %v", err)
	}
}

func TestNewManager_RequiresNativeStore(t *testing.T) {
	_, err := NewManager(ManagerOptions{
		StoreDir: t.TempDir(),
		Logger:   newTestLogger(),
	})
	if err == nil || !contains(err.Error(), "native store") {
		t.Errorf("expected native-store error, got %v", err)
	}
}

func TestNewManager_RequiresLogger(t *testing.T) {
	dir := newTestStoreDir(t)
	_, err := NewManager(ManagerOptions{
		StoreDir:    dir,
		NativeStore: newTestNativeStore(t, dir),
	})
	if err == nil || !contains(err.Error(), "logger") {
		t.Errorf("expected logger error, got %v", err)
	}
}

func TestNewManager_StartsIdle(t *testing.T) {
	dir := newTestStoreDir(t)
	m, err := NewManager(ManagerOptions{
		StoreDir:    dir,
		NativeStore: newTestNativeStore(t, dir),
		Logger:      newTestLogger(),
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	status := m.Status()
	if status.State != ManagerStateIdle {
		t.Errorf("state = %q, want idle", status.State)
	}
	if status.HasConfig {
		t.Error("expected no config persisted on a fresh store")
	}
	if status.LastError != "" {
		t.Errorf("unexpected LastError: %q", status.LastError)
	}
	if m.Config() != nil {
		t.Error("expected Config() == nil on fresh manager")
	}
	if m.Current() != nil {
		t.Error("expected Current() == nil before Start")
	}
}

func TestNewManager_LoadsPersistedConfig(t *testing.T) {
	// This test exercises config-load semantics in isolation; the
	// auto-start path is covered separately (see manager_autostart_test.go).
	t.Setenv("ITERION_DISPATCHER_AUTOSTART", "0")
	dir := newTestStoreDir(t)
	// Create the workflow file Validate() will stat.
	wfPath := filepath.Join(dir, "flow.iter")
	if err := os.WriteFile(wfPath, []byte("workflow x: x -> done\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Pre-populate the config file the manager will load.
	cfgPath := filepath.Join(dir, "dispatcher", "dispatcher.json")
	cfgBody := []byte(`{"workflow":"` + wfPath + `","tracker":{"kind":"native"}}`)
	if err := os.WriteFile(cfgPath, cfgBody, 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := NewManager(ManagerOptions{
		StoreDir:    dir,
		NativeStore: newTestNativeStore(t, dir),
		Logger:      newTestLogger(),
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if cfg := m.Config(); cfg == nil || cfg.Workflow != wfPath {
		t.Errorf("config not loaded from disk: %+v", cfg)
	}
	if !m.Status().HasConfig {
		t.Error("HasConfig should be true after loading persisted file")
	}
}

func TestNewManager_TolerateMalformedConfig(t *testing.T) {
	// Malformed config should NOT block manager creation — it logs and
	// keeps the manager in idle/no-config state so the operator can fix
	// it through the UI.
	dir := newTestStoreDir(t)
	cfgPath := filepath.Join(dir, "dispatcher", "dispatcher.json")
	if err := os.WriteFile(cfgPath, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := NewManager(ManagerOptions{
		StoreDir:    dir,
		NativeStore: newTestNativeStore(t, dir),
		Logger:      newTestLogger(),
	})
	if err != nil {
		t.Fatalf("NewManager should tolerate malformed config: %v", err)
	}
	if m.Config() != nil {
		t.Errorf("Config should remain nil for malformed JSON: %+v", m.Config())
	}
}

// ---- errString ----

func TestErrString(t *testing.T) {
	if got := errString(nil); got != "" {
		t.Errorf("nil → %q, want empty", got)
	}
	if got := errString(errors.New("boom")); got != "boom" {
		t.Errorf("got %q, want boom", got)
	}
}

// ---- redactedConfig ----

func TestRedactedConfig_MasksGitHubToken(t *testing.T) {
	cfg := &Config{
		Workflow: "./flow.iter",
		Tracker: TrackerConfig{
			Kind:   TrackerKindGitHub,
			GitHub: &GitHubTrackerConfig{Repo: "org/repo", Token: "ghp_secret123"},
		},
	}
	out := redactedConfig(cfg)
	if out.Tracker.GitHub == nil {
		t.Fatal("expected GitHub block")
	}
	if out.Tracker.GitHub.Token != "***" {
		t.Errorf("Token not masked: %q", out.Tracker.GitHub.Token)
	}
	if out.Tracker.GitHub.Repo != "org/repo" {
		t.Errorf("Repo should pass through: %q", out.Tracker.GitHub.Repo)
	}
	// Original config must not be mutated — important since SaveConfig
	// holds the live map.
	if cfg.Tracker.GitHub.Token != "ghp_secret123" {
		t.Errorf("source mutated: %q", cfg.Tracker.GitHub.Token)
	}
}

func TestRedactedConfig_MasksForgejoToken(t *testing.T) {
	cfg := &Config{
		Workflow: "./flow.iter",
		Tracker: TrackerConfig{
			Kind:    TrackerKindForgejo,
			Forgejo: &ForgejoTrackerConfig{Host: "https://forge.example", Repo: "g/r", Token: "fj_secret"},
		},
	}
	out := redactedConfig(cfg)
	if out.Tracker.Forgejo == nil {
		t.Fatal("expected Forgejo block")
	}
	if out.Tracker.Forgejo.Token != "***" {
		t.Errorf("Token not masked: %q", out.Tracker.Forgejo.Token)
	}
	if cfg.Tracker.Forgejo.Token != "fj_secret" {
		t.Errorf("source mutated: %q", cfg.Tracker.Forgejo.Token)
	}
}

func TestRedactedConfig_EmptyTokenLeftAlone(t *testing.T) {
	// Token empty → don't replace with "***" (that would imply a secret
	// where there is none and confuse the operator).
	cfg := &Config{
		Workflow: "./flow.iter",
		Tracker: TrackerConfig{
			Kind:   TrackerKindGitHub,
			GitHub: &GitHubTrackerConfig{Repo: "org/repo"},
		},
	}
	out := redactedConfig(cfg)
	if out.Tracker.GitHub.Token != "" {
		t.Errorf("empty Token mutated to %q", out.Tracker.GitHub.Token)
	}
}

func TestRedactedConfig_NoTrackerBlocks(t *testing.T) {
	// Native tracker has no token to redact — no panic, faithful copy.
	cfg := &Config{
		Workflow: "./flow.iter",
		Tracker:  TrackerConfig{Kind: TrackerKindNative},
	}
	out := redactedConfig(cfg)
	if out.Tracker.Kind != TrackerKindNative {
		t.Errorf("Kind lost: %q", out.Tracker.Kind)
	}
}

// ---- loadConfigJSON + saveConfigJSON round-trip ----

func TestSaveAndLoadConfigJSON_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	// Create the workflow file Validate() will stat.
	wfPath := filepath.Join(dir, "flow.iter")
	if err := os.WriteFile(wfPath, []byte("workflow x: x -> done\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "dispatcher.json")
	cfg := &Config{
		Workflow: wfPath,
		Tracker: TrackerConfig{
			Kind:   TrackerKindGitHub,
			GitHub: &GitHubTrackerConfig{Repo: "org/repo", Token: "tok"},
		},
	}
	if err := saveConfigJSON(path, cfg); err != nil {
		t.Fatalf("saveConfigJSON: %v", err)
	}
	got, err := loadConfigJSON(path)
	if err != nil {
		t.Fatalf("loadConfigJSON: %v", err)
	}
	if got.Workflow != wfPath {
		t.Errorf("Workflow: got %q", got.Workflow)
	}
	if got.Tracker.Kind != TrackerKindGitHub {
		t.Errorf("Kind: got %q", got.Tracker.Kind)
	}
	if got.Tracker.GitHub == nil || got.Tracker.GitHub.Token != "tok" {
		t.Errorf("GitHub block lost round-trip: %+v", got.Tracker.GitHub)
	}
}

func TestLoadConfigJSON_MissingFileReturnsErrNotExist(t *testing.T) {
	_, err := loadConfigJSON(filepath.Join(t.TempDir(), "does-not-exist.json"))
	if err == nil {
		t.Fatal("expected an error for missing file")
	}
	if !os.IsNotExist(err) && !errors.Is(err, os.ErrNotExist) {
		t.Errorf("error should be ErrNotExist-shaped: %v", err)
	}
}

func TestSaveConfigJSON_CreatesParentDirs(t *testing.T) {
	dir := t.TempDir()
	// Nested path that doesn't exist yet.
	path := filepath.Join(dir, "a", "b", "dispatcher.json")
	cfg := &Config{Workflow: "./flow.iter", Tracker: TrackerConfig{Kind: TrackerKindNative}}
	if err := saveConfigJSON(path, cfg); err != nil {
		t.Fatalf("saveConfigJSON: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("file not created: %v", err)
	}
}

// ---- helpers ----

func contains(haystack, needle string) bool {
	return haystack != "" && needle != "" && len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
