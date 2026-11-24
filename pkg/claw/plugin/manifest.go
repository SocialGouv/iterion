package plugin

import (
	"encoding/json"
	"fmt"
	"os"
)

// PluginManifest represents a plugin.json file.
type PluginManifest struct {
	Name           string                  `json:"name"`
	Version        string                  `json:"version"`
	Description    string                  `json:"description"`
	Permissions    []PluginPermission      `json:"permissions"`
	DefaultEnabled bool                    `json:"defaultEnabled"`
	Hooks          PluginHooks             `json:"hooks"`
	Lifecycle      PluginLifecycle         `json:"lifecycle"`
	Tools          []PluginToolManifest    `json:"tools"`
	Commands       []PluginCommandManifest `json:"commands"`

	// RawJSON holds the original bytes for contract validation.
	RawJSON json.RawMessage `json:"-"`
}

// PluginHooks contains hook command lists keyed by event type.
type PluginHooks struct {
	PreToolUse         []string `json:"PreToolUse"`
	PostToolUse        []string `json:"PostToolUse"`
	PostToolUseFailure []string `json:"PostToolUseFailure"`
}

// PluginLifecycle contains lifecycle commands.
type PluginLifecycle struct {
	Init     []string `json:"Init"`
	Shutdown []string `json:"Shutdown"`
}

// PluginToolManifest describes a tool provided by a plugin.
type PluginToolManifest struct {
	Name               string               `json:"name"`
	Description        string               `json:"description"`
	InputSchema        json.RawMessage      `json:"inputSchema"`
	Command            string               `json:"command"`
	Args               []string             `json:"args"`
	RequiredPermission PluginToolPermission `json:"requiredPermission"`
}

// PluginCommandManifest describes a command provided by a plugin.
type PluginCommandManifest struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Command     string `json:"command"`
}

// LoadManifest reads and parses a plugin.json file.
func LoadManifest(path string) (*PluginManifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, &PluginError{
			Kind:    ErrIO,
			Message: fmt.Sprintf("failed to read manifest: %s", path),
			Cause:   err,
		}
	}

	var m PluginManifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, &PluginError{
			Kind:    ErrJSON,
			Message: fmt.Sprintf("failed to parse manifest: %s", path),
			Cause:   err,
		}
	}

	m.RawJSON = json.RawMessage(data)
	return &m, nil
}
