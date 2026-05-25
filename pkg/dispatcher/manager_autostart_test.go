package dispatcher

import (
	"os"
	"path/filepath"
	"testing"
)

// seedManagerFixture writes the minimum on-disk state for NewManager to
// load a valid Config that Start() will accept: a writable workflow
// file + a config.json pointing at it with tracker:native. Returns the
// store dir so the caller can inspect persisted artifacts (e.g.
// runtime.json) after NewManager runs.
func seedManagerFixture(t *testing.T) string {
	t.Helper()
	dir := newTestStoreDir(t)
	wfPath := filepath.Join(dir, "flow.iter")
	// Minimal tool-only workflow — Manager.Start calls NewRoutingRunner
	// which compiles the workflow file at boot. A bare-bones tool
	// definition + a single edge to done is the smallest legal form
	// (no schemas, no prompts, no LLM).
	wfBody := `tool noop:
  command: "true"

workflow minimal:
  entry: noop
  noop -> done
`
	if err := os.WriteFile(wfPath, []byte(wfBody), 0o644); err != nil {
		t.Fatalf("write workflow: %v", err)
	}
	cfgPath := filepath.Join(dir, "dispatcher", "dispatcher.json")
	cfgBody := []byte(`{"workflow":"` + wfPath + `","tracker":{"kind":"native"}}`)
	if err := os.WriteFile(cfgPath, cfgBody, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return dir
}

// TestNewManager_AutoStartsOnFirstBootWithConfig captures the primary
// UX win: cold-boot a studio with a saved dispatcher config and no
// runtime.json, expect the actor to be running without operator
// intervention.
func TestNewManager_AutoStartsOnFirstBootWithConfig(t *testing.T) {
	t.Setenv("ITERION_DISPATCHER_AUTOSTART", "")
	dir := seedManagerFixture(t)
	m, err := NewManager(ManagerOptions{
		StoreDir:    dir,
		NativeStore: newTestNativeStore(t, dir),
		Logger:      newTestLogger(),
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	defer m.Stop()
	if got := m.Status().State; got != ManagerStateRunning {
		t.Errorf("state = %q, want running (auto-start on first boot)", got)
	}
	// Persisted intent now reflects the running state so a follow-up
	// cold boot replays it cleanly.
	gotDesired, err := loadDesiredState(runtimeStatePath(dir))
	if err != nil {
		t.Fatalf("loadDesiredState: %v", err)
	}
	if gotDesired != DesiredRunning {
		t.Errorf("persisted desired = %q, want %q", gotDesired, DesiredRunning)
	}
}

// TestNewManager_RespectsAutoStartOptOut covers the CI / multi-tenant
// case where the operator does NOT want the studio claiming dispatcher
// resources on startup.
func TestNewManager_RespectsAutoStartOptOut(t *testing.T) {
	t.Setenv("ITERION_DISPATCHER_AUTOSTART", "0")
	dir := seedManagerFixture(t)
	m, err := NewManager(ManagerOptions{
		StoreDir:    dir,
		NativeStore: newTestNativeStore(t, dir),
		Logger:      newTestLogger(),
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if got := m.Status().State; got != ManagerStateIdle {
		t.Errorf("state = %q, want idle (autostart opted out)", got)
	}
}

// TestNewManager_RestoresPersistedStoppedState ensures a previously-
// stopped dispatcher does not resurrect itself across restarts. The
// operator's explicit intent always wins over the auto-start default.
func TestNewManager_RestoresPersistedStoppedState(t *testing.T) {
	t.Setenv("ITERION_DISPATCHER_AUTOSTART", "")
	dir := seedManagerFixture(t)
	if err := saveDesiredState(runtimeStatePath(dir), DesiredStopped); err != nil {
		t.Fatalf("seed runtime state: %v", err)
	}
	m, err := NewManager(ManagerOptions{
		StoreDir:    dir,
		NativeStore: newTestNativeStore(t, dir),
		Logger:      newTestLogger(),
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if got := m.Status().State; got != ManagerStateIdle {
		t.Errorf("state = %q, want idle (persisted stopped)", got)
	}
}

// TestNewManager_RestoresPersistedPausedState ensures that pausing in
// one session survives a restart — the operator finds the dispatcher
// in the same suspended state they left it. The actor IS started
// (so resume is a fast in-process flip rather than a full re-init).
func TestNewManager_RestoresPersistedPausedState(t *testing.T) {
	t.Setenv("ITERION_DISPATCHER_AUTOSTART", "")
	dir := seedManagerFixture(t)
	if err := saveDesiredState(runtimeStatePath(dir), DesiredPaused); err != nil {
		t.Fatalf("seed runtime state: %v", err)
	}
	m, err := NewManager(ManagerOptions{
		StoreDir:    dir,
		NativeStore: newTestNativeStore(t, dir),
		Logger:      newTestLogger(),
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	defer m.Stop()
	if got := m.Status().State; got != ManagerStatePaused {
		t.Errorf("state = %q, want paused (persisted paused)", got)
	}
}

// TestManager_PersistsDesiredOnLifecycleTransitions verifies that every
// operator-driven state change writes the corresponding DesiredState so
// a crash mid-session (or a routine restart) replays the right intent.
func TestManager_PersistsDesiredOnLifecycleTransitions(t *testing.T) {
	t.Setenv("ITERION_DISPATCHER_AUTOSTART", "0")
	dir := seedManagerFixture(t)
	m, err := NewManager(ManagerOptions{
		StoreDir:    dir,
		NativeStore: newTestNativeStore(t, dir),
		Logger:      newTestLogger(),
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	defer m.Stop()
	runtimePath := runtimeStatePath(dir)

	if err := m.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got, _ := loadDesiredState(runtimePath); got != DesiredRunning {
		t.Errorf("after Start: persisted = %q, want running", got)
	}

	if err := m.Pause(); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	if got, _ := loadDesiredState(runtimePath); got != DesiredPaused {
		t.Errorf("after Pause: persisted = %q, want paused", got)
	}

	if err := m.Resume(); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if got, _ := loadDesiredState(runtimePath); got != DesiredRunning {
		t.Errorf("after Resume: persisted = %q, want running", got)
	}

	m.Stop()
	if got, _ := loadDesiredState(runtimePath); got != DesiredStopped {
		t.Errorf("after Stop: persisted = %q, want stopped", got)
	}
}
