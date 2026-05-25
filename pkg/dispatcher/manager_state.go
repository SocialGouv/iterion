package dispatcher

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// DesiredState is the operator's last-known intent for the dispatcher
// actor, persisted across studio restarts. It tracks what the operator
// asked for, NOT the live ManagerState — those drift when a Start
// attempt fails (operator asked for "running", actual state is "error").
type DesiredState string

const (
	// DesiredRunning means: bring the actor up + dispatch. Replays as
	// Start() on cold boot.
	DesiredRunning DesiredState = "running"
	// DesiredPaused means: actor up, dispatch suspended. Replays as
	// Start() then Pause() on cold boot, so the operator finds the same
	// state they left when reopening the studio.
	DesiredPaused DesiredState = "paused"
	// DesiredStopped means: actor down, no auto-start. The operator
	// must explicitly Start from the UI to resume polling.
	DesiredStopped DesiredState = "stopped"
)

// runtimeStateFile is the JSON file persisting DesiredState alongside
// dispatcher.json. Kept separate so user-editable config (workflow path,
// tracker creds) stays decoupled from the runtime intent we manage on
// the operator's behalf.
type runtimeStateFile struct {
	Desired DesiredState `json:"desired"`
}

// runtimeStatePath returns the persistence path for the manager's
// runtime desired state, given the same store-dir-relative layout used
// for the persisted config (`dispatcher/dispatcher.json` →
// `dispatcher/runtime.json`).
func runtimeStatePath(storeDir string) string {
	return filepath.Join(storeDir, "dispatcher", "runtime.json")
}

// loadDesiredState reads the persisted DesiredState. Returns the zero
// value ("" — meaning "no persisted preference") when the file does not
// exist, distinct from an explicit DesiredStopped. Read errors other
// than missing-file bubble up so the caller can log them.
func loadDesiredState(path string) (DesiredState, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil
		}
		return "", fmt.Errorf("dispatcher: read runtime state %s: %w", path, err)
	}
	var f runtimeStateFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return "", fmt.Errorf("dispatcher: parse runtime state %s: %w", path, err)
	}
	switch f.Desired {
	case DesiredRunning, DesiredPaused, DesiredStopped:
		return f.Desired, nil
	case "":
		return "", nil
	default:
		// Unknown future value — treat as no preference. Don't error
		// because that would lock the dispatcher idle until the
		// operator hand-edits the file.
		return "", nil
	}
}

// saveDesiredState writes the operator's intent atomically (write to
// .tmp, then rename) so a crash mid-write can't leave a half-flushed
// JSON file that loadDesiredState would reject.
//
// Best-effort: the parent dir is created if missing. A nil error
// means the desired state is durably persisted; callers can ignore
// errors when they merely reflect a missing-store edge case (e.g.
// the studio booted without a store-dir).
func saveDesiredState(path string, desired DesiredState) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("dispatcher: mkdir for runtime state: %w", err)
	}
	body, err := json.MarshalIndent(runtimeStateFile{Desired: desired}, "", "  ")
	if err != nil {
		return fmt.Errorf("dispatcher: marshal runtime state: %w", err)
	}
	body = append(body, '\n')
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, body, 0o644); err != nil {
		return fmt.Errorf("dispatcher: write runtime state tmp: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("dispatcher: rename runtime state: %w", err)
	}
	return nil
}

// autoStartEnabled returns true when the first-boot auto-start behaviour
// is permitted. Defaults to true; flip via the env var when the operator
// (or a CI environment) explicitly does NOT want the studio to claim
// dispatcher resources on startup.
//
// Recognised falsy values: "0", "false", "no", "off" (case-insensitive).
// Anything else (including unset) means auto-start is on.
func autoStartEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("ITERION_DISPATCHER_AUTOSTART"))) {
	case "0", "false", "no", "off":
		return false
	default:
		return true
	}
}

// resolveBootIntent decides whether to call Start (and optionally Pause)
// at NewManager time. The logic is:
//
//   - When a persisted DesiredState exists, replay it verbatim. The
//     operator's intent always wins, so a previously-stopped dispatcher
//     does not resurrect itself across restarts.
//   - When no persisted state exists AND a config is present, default
//     to DesiredRunning — the first-boot auto-start that eliminates the
//     "why isn't my dispatcher polling?" surprise (see finding
//     2026-05-25-dispatcher-paused-vs-started-ux-confusion). Disable
//     via `ITERION_DISPATCHER_AUTOSTART=0`.
//   - When no config is present, return DesiredStopped — there's
//     nothing to dispatch yet; the SPA will prompt the operator to
//     save one.
func resolveBootIntent(persisted DesiredState, hasConfig bool) DesiredState {
	if persisted != "" {
		return persisted
	}
	if !hasConfig {
		return DesiredStopped
	}
	if !autoStartEnabled() {
		return DesiredStopped
	}
	return DesiredRunning
}
