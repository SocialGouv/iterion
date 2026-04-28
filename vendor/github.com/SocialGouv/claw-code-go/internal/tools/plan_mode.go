package tools

import (
	"bytes"
	"github.com/SocialGouv/claw-code-go/internal/api"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

func EnterPlanModeTool() api.Tool {
	return api.Tool{
		Name:        "enter_plan_mode",
		Description: "Enter plan mode. Tools will be described but not executed.",
		InputSchema: api.InputSchema{
			Type:       "object",
			Properties: map[string]api.Property{},
		},
	}
}

func ExitPlanModeTool() api.Tool {
	return api.Tool{
		Name:        "exit_plan_mode",
		Description: "Exit plan mode. Resume normal tool execution.",
		InputSchema: api.InputSchema{
			Type:       "object",
			Properties: map[string]api.Property{},
		},
	}
}

// planModeState is the persisted state for plan mode, tracking whether a local
// override existed before entering plan mode and what value it had.
type planModeState struct {
	HadLocalOverride  bool             `json:"hadLocalOverride"`
	PreviousLocalMode *json.RawMessage `json:"previousLocalMode,omitempty"`
}

// planModeOutput is the typed JSON response for plan mode operations.
// All 10 fields are always serialized (matching Rust's #[derive(Serialize)]
// which never omits fields). settingsPath/statePath serialize as "" when
// empty; previousLocalMode/currentLocalMode serialize as null when nil.
type planModeOutput struct {
	Success           bool             `json:"success"`
	Operation         string           `json:"operation"`
	Changed           bool             `json:"changed"`
	Active            bool             `json:"active"`
	Managed           bool             `json:"managed"`
	Message           string           `json:"message"`
	SettingsPath      string           `json:"settingsPath"`
	StatePath         string           `json:"statePath"`
	PreviousLocalMode *json.RawMessage `json:"previousLocalMode"`
	CurrentLocalMode  *json.RawMessage `json:"currentLocalMode"`
}

// permissionDefaultModePath is the JSON path to the permissions.defaultMode
// setting in settings.local.json.
var permissionDefaultModePath = []string{"permissions", "defaultMode"}

func ExecuteEnterPlanMode(planModeActive *bool, stateDir string) (string, error) {
	if planModeActive == nil {
		return "", fmt.Errorf("enter_plan_mode: plan mode state not available")
	}

	settingsPath := settingsLocalPath(stateDir)
	statePath := planModeStatePath(stateDir)

	// Read current local mode from settings.local.json.
	currentLocalMode := readNestedValue(settingsPath, permissionDefaultModePath)
	currentIsPlan := isStringValue(currentLocalMode, "plan")

	// If already active (either via in-memory flag or settings), handle idempotently.
	if *planModeActive || currentIsPlan {
		if stateDir == "" {
			// No stateDir — use simple in-memory semantics.
			return marshalPlanModeOutput(planModeOutput{
				Success:   true,
				Operation: "enter",
				Managed:   true,
				Active:    true,
				Changed:   false,
				Message:   "Plan mode is already active",
			})
		}
		// Check if we have a state file (managed by us).
		existingState, err := readPlanModeStateFile(statePath)
		if err != nil {
			return "", fmt.Errorf("enter_plan_mode: %w", err)
		}
		if currentIsPlan && existingState != nil {
			// Already active and managed by us.
			return marshalPlanModeOutput(planModeOutput{
				Success:           true,
				Operation:         "enter",
				Changed:           false,
				Active:            true,
				Managed:           true,
				Message:           "Plan mode override is already active for this worktree.",
				SettingsPath:      settingsPath,
				StatePath:         statePath,
				PreviousLocalMode: existingState.PreviousLocalMode,
				CurrentLocalMode:  currentLocalMode,
			})
		}
		if currentIsPlan {
			// Already plan mode but not managed by us.
			return marshalPlanModeOutput(planModeOutput{
				Success:          true,
				Operation:        "enter",
				Changed:          false,
				Active:           true,
				Managed:          false,
				Message:          "Worktree-local plan mode is already enabled outside EnterPlanMode; leaving it unchanged.",
				SettingsPath:     settingsPath,
				StatePath:        statePath,
				CurrentLocalMode: currentLocalMode,
			})
		}
		// planModeActive is true but settings don't reflect plan mode.
		// Fall through to set it up properly.
	}

	// Save state before mutation, then set plan mode in settings.local.json.
	state := planModeState{
		HadLocalOverride:  currentLocalMode != nil,
		PreviousLocalMode: rawMessagePtr(currentLocalMode),
	}
	if stateDir != "" {
		// Clear any stale state before writing, matching Rust's two-step
		// clear + write sequence for auditability.
		if err := clearPlanModeStateFile(statePath); err != nil {
			return "", fmt.Errorf("enter_plan_mode: failed to clear stale state: %w", err)
		}
		if err := writePlanModeStateFile(statePath, &state); err != nil {
			return "", fmt.Errorf("enter_plan_mode: failed to write state: %w", err)
		}
		if err := setNestedValue(settingsPath, permissionDefaultModePath, "plan"); err != nil {
			return "", fmt.Errorf("enter_plan_mode: failed to write settings: %w", err)
		}
	}

	*planModeActive = true

	// Re-read current local mode after mutation.
	newLocalMode := readNestedValue(settingsPath, permissionDefaultModePath)

	return marshalPlanModeOutput(planModeOutput{
		Success:           true,
		Operation:         "enter",
		Changed:           true,
		Active:            true,
		Managed:           true,
		Message:           "Enabled worktree-local plan mode override.",
		SettingsPath:      settingsPath,
		StatePath:         statePath,
		PreviousLocalMode: state.PreviousLocalMode,
		CurrentLocalMode:  newLocalMode,
	})
}

func ExecuteExitPlanMode(planModeActive *bool, stateDir string) (string, error) {
	if planModeActive == nil {
		return "", fmt.Errorf("exit_plan_mode: plan mode state not available")
	}

	// Simple in-memory path when no stateDir is configured.
	if stateDir == "" {
		if !*planModeActive {
			return marshalPlanModeOutput(planModeOutput{
				Success:   true,
				Operation: "exit",
				Message:   "Plan mode is not active",
			})
		}
		*planModeActive = false
		return marshalPlanModeOutput(planModeOutput{
			Success:   true,
			Operation: "exit",
			Changed:   true,
			Message:   "Plan mode deactivated. Normal tool execution resumed.",
		})
	}

	settingsPath := settingsLocalPath(stateDir)
	statePath := planModeStatePath(stateDir)

	currentLocalMode := readNestedValue(settingsPath, permissionDefaultModePath)
	currentIsPlan := isStringValue(currentLocalMode, "plan")

	// Read existing state file.
	existingState, err := readPlanModeStateFile(statePath)
	if err != nil {
		return "", fmt.Errorf("exit_plan_mode: %w", err)
	}

	if existingState == nil {
		// No state file — not managed by us.
		if !*planModeActive && !currentIsPlan {
			return marshalPlanModeOutput(planModeOutput{
				Success:          true,
				Operation:        "exit",
				Message:          "No EnterPlanMode override is active for this worktree.",
				SettingsPath:     settingsPath,
				StatePath:        statePath,
				CurrentLocalMode: currentLocalMode,
			})
		}
		*planModeActive = false
		return marshalPlanModeOutput(planModeOutput{
			Success:          true,
			Operation:        "exit",
			Active:           currentIsPlan,
			Message:          "No EnterPlanMode override is active for this worktree.",
			SettingsPath:     settingsPath,
			StatePath:        statePath,
			CurrentLocalMode: currentLocalMode,
		})
	}

	if !currentIsPlan {
		// State exists but plan mode is not active — stale state, clean up.
		if err := clearPlanModeStateFile(statePath); err != nil {
			return "", fmt.Errorf("exit_plan_mode: failed to clear state: %w", err)
		}
		*planModeActive = false
		return marshalPlanModeOutput(planModeOutput{
			Success:           true,
			Operation:         "exit",
			Message:           "Cleared stale EnterPlanMode state because plan mode was already changed outside the tool.",
			SettingsPath:      settingsPath,
			StatePath:         statePath,
			PreviousLocalMode: existingState.PreviousLocalMode,
			CurrentLocalMode:  currentLocalMode,
		})
	}

	// Restore prior settings (stateDir is guaranteed non-empty here).
	// Only restore the previous value if it existed and is valid JSON;
	// otherwise remove the key entirely.
	var restored bool
	if existingState.HadLocalOverride && existingState.PreviousLocalMode != nil {
		var prevValue any
		if json.Unmarshal(*existingState.PreviousLocalMode, &prevValue) == nil {
			if err := setNestedValue(settingsPath, permissionDefaultModePath, prevValue); err != nil {
				return "", fmt.Errorf("exit_plan_mode: failed to restore settings: %w", err)
			}
			restored = true
		}
	}
	if !restored {
		if err := removeNestedValue(settingsPath, permissionDefaultModePath); err != nil {
			return "", fmt.Errorf("exit_plan_mode: failed to remove setting: %w", err)
		}
	}

	if err := clearPlanModeStateFile(statePath); err != nil {
		return "", fmt.Errorf("exit_plan_mode: failed to clear state: %w", err)
	}
	*planModeActive = false

	newLocalMode := readNestedValue(settingsPath, permissionDefaultModePath)

	return marshalPlanModeOutput(planModeOutput{
		Success:           true,
		Operation:         "exit",
		Changed:           true,
		Message:           "Restored the prior worktree-local plan mode setting.",
		SettingsPath:      settingsPath,
		StatePath:         statePath,
		PreviousLocalMode: existingState.PreviousLocalMode,
		CurrentLocalMode:  newLocalMode,
	})
}

// marshalPlanModeOutput serializes a planModeOutput to pretty-printed JSON.
func marshalPlanModeOutput(o planModeOutput) (string, error) {
	out, err := json.MarshalIndent(o, "", "  ")
	if err != nil {
		return "", fmt.Errorf("plan mode: failed to marshal output: %w", err)
	}
	return string(out), nil
}

// planModeStatePath returns the path to the plan mode state file.
func planModeStatePath(stateDir string) string {
	if stateDir == "" {
		return ""
	}
	return filepath.Join(stateDir, "tool-state", "plan-mode.json")
}

// settingsLocalPath returns the path to settings.local.json.
func settingsLocalPath(stateDir string) string {
	if stateDir == "" {
		return ""
	}
	return filepath.Join(stateDir, "settings.local.json")
}

// LoadPersistedPlanMode reads the persisted plan mode state from disk.
// Returns (true, nil) if a state file exists (indicating plan mode was entered
// via ExecuteEnterPlanMode). Returns (false, nil) if stateDir is empty or the
// file doesn't exist. Returns (false, error) for real filesystem failures
// (permission denied, I/O errors), matching Rust's error propagation.
func LoadPersistedPlanMode(stateDir string) (bool, error) {
	if stateDir == "" {
		return false, nil
	}
	state, err := readPlanModeStateFile(planModeStatePath(stateDir))
	if err != nil {
		return false, fmt.Errorf("load persisted plan mode: %w", err)
	}
	return state != nil, nil
}

// --- Settings file helpers ---

// readNestedValue reads a nested JSON value from a file at the given path.
// Returns nil if the file doesn't exist or the path doesn't exist in the JSON.
func readNestedValue(filePath string, jsonPath []string) *json.RawMessage {
	if filePath == "" {
		return nil
	}
	data, err := os.ReadFile(filePath)
	if err != nil {
		return nil
	}
	var doc map[string]any
	if json.Unmarshal(data, &doc) != nil {
		return nil
	}
	val := getNestedValueFromMap(doc, jsonPath)
	if val == nil {
		return nil
	}
	raw, err := json.Marshal(val)
	if err != nil {
		return nil
	}
	rm := json.RawMessage(raw)
	return &rm
}

// setNestedValue sets a nested JSON value in a file at the given path.
// Returns an error if the directory cannot be created or the file cannot be written.
func setNestedValue(filePath string, jsonPath []string, value any) error {
	if filePath == "" || len(jsonPath) == 0 {
		return nil
	}
	var doc map[string]any
	data, err := os.ReadFile(filePath)
	if err != nil {
		doc = make(map[string]any)
	} else if json.Unmarshal(data, &doc) != nil {
		doc = make(map[string]any)
	}

	setNestedValueInMap(doc, jsonPath, value)

	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal settings: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(filePath), 0o755); err != nil {
		return fmt.Errorf("create settings dir: %w", err)
	}
	if err := os.WriteFile(filePath, out, 0o644); err != nil {
		return fmt.Errorf("write settings file: %w", err)
	}
	return nil
}

// removeNestedValue removes a nested JSON value from a file.
// Returns an error if the file cannot be written after modification.
func removeNestedValue(filePath string, jsonPath []string) error {
	if filePath == "" || len(jsonPath) == 0 {
		return nil
	}
	data, err := os.ReadFile(filePath)
	if err != nil {
		return nil // file doesn't exist, nothing to remove
	}
	var doc map[string]any
	if json.Unmarshal(data, &doc) != nil {
		return nil // invalid JSON, nothing to remove
	}
	removeNestedValueFromMap(doc, jsonPath)

	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal settings: %w", err)
	}
	if err := os.WriteFile(filePath, out, 0o644); err != nil {
		return fmt.Errorf("write settings file: %w", err)
	}
	return nil
}

func getNestedValueFromMap(m map[string]any, path []string) any {
	if len(path) == 0 {
		return nil
	}
	val, ok := m[path[0]]
	if !ok {
		return nil
	}
	if len(path) == 1 {
		return val
	}
	sub, ok := val.(map[string]any)
	if !ok {
		return nil
	}
	return getNestedValueFromMap(sub, path[1:])
}

func setNestedValueInMap(m map[string]any, path []string, value any) {
	if len(path) == 0 {
		return
	}
	if len(path) == 1 {
		m[path[0]] = value
		return
	}
	sub, ok := m[path[0]].(map[string]any)
	if !ok {
		sub = make(map[string]any)
		m[path[0]] = sub
	}
	setNestedValueInMap(sub, path[1:], value)
}

func removeNestedValueFromMap(m map[string]any, path []string) {
	if len(path) == 0 {
		return
	}
	if len(path) == 1 {
		delete(m, path[0])
		return
	}
	sub, ok := m[path[0]].(map[string]any)
	if !ok {
		return
	}
	removeNestedValueFromMap(sub, path[1:])
}

// readPlanModeStateFile reads the plan mode state file.
// Returns (nil, nil) if the file does not exist or is empty (matching Rust's NotFound → Ok(None)).
// Returns (nil, error) for real filesystem failures (permission denied, I/O errors)
// or corrupt JSON content.
func readPlanModeStateFile(path string) (*planModeState, error) {
	if path == "" {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read plan mode state: %w", err)
	}
	// Rust treats empty/whitespace-only content as None.
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, nil
	}
	var state planModeState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("parse plan mode state: %w", err)
	}
	return &state, nil
}

// writePlanModeStateFile writes the plan mode state file.
// Returns an error if the directory cannot be created or the file cannot be written.
func writePlanModeStateFile(path string, state *planModeState) error {
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create state dir: %w", err)
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal state: %w", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("write state file: %w", err)
	}
	return nil
}

// clearPlanModeStateFile removes the plan mode state file.
// Returns nil if the file does not exist (idempotent deletion).
// Returns an error for all other filesystem failures (e.g. permission denied).
// This matches Rust's ErrorKind::NotFound → Ok(()) pattern.
func clearPlanModeStateFile(path string) error {
	if path == "" {
		return nil
	}
	err := os.Remove(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove state file: %w", err)
	}
	return nil
}

// isStringValue checks if a json.RawMessage contains a specific string value.
func isStringValue(rm *json.RawMessage, expected string) bool {
	if rm == nil {
		return false
	}
	var s string
	if json.Unmarshal(*rm, &s) != nil {
		return false
	}
	return s == expected
}

// rawMessagePtr returns a defensive copy of a *json.RawMessage.
func rawMessagePtr(rm *json.RawMessage) *json.RawMessage {
	if rm == nil {
		return nil
	}
	cp := make(json.RawMessage, len(*rm))
	copy(cp, *rm)
	return &cp
}
