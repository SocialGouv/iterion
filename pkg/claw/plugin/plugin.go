package plugin

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// PluginMetadata describes a registered plugin.
type PluginMetadata struct {
	ID             string
	Name           string
	Version        string
	Description    string
	Kind           PluginKind
	Source         string
	DefaultEnabled bool
	Root           string // empty for builtin
}

// Plugin is the interface all plugins must implement.
type Plugin interface {
	Metadata() PluginMetadata
	Hooks() PluginHooks
	Lifecycle() PluginLifecycle
	Tools() []PluginToolDefinition
	Validate() error
	Initialize() error
	Shutdown() error
}

// PluginToolDefinition is a resolved tool from a plugin.
type PluginToolDefinition struct {
	Name        string
	Description string
	InputSchema json.RawMessage // must be a JSON object
}

// --- BuiltinPlugin ---

// BuiltinPlugin is a hardcoded plugin with no filesystem root.
type BuiltinPlugin struct {
	Meta     PluginMetadata
	HooksDef PluginHooks
	LifeDef  PluginLifecycle
	ToolsDef []PluginToolDefinition
}

func (p *BuiltinPlugin) Metadata() PluginMetadata      { return p.Meta }
func (p *BuiltinPlugin) Hooks() PluginHooks            { return p.HooksDef }
func (p *BuiltinPlugin) Lifecycle() PluginLifecycle    { return p.LifeDef }
func (p *BuiltinPlugin) Tools() []PluginToolDefinition { return p.ToolsDef }
func (p *BuiltinPlugin) Validate() error               { return nil }
func (p *BuiltinPlugin) Initialize() error             { return nil }
func (p *BuiltinPlugin) Shutdown() error               { return nil }

// --- BundledPlugin ---

// BundledPlugin is a pre-packaged plugin.
type BundledPlugin struct {
	Meta     PluginMetadata
	Manifest PluginManifest
}

func (p *BundledPlugin) Metadata() PluginMetadata   { return p.Meta }
func (p *BundledPlugin) Hooks() PluginHooks         { return p.Manifest.Hooks }
func (p *BundledPlugin) Lifecycle() PluginLifecycle { return p.Manifest.Lifecycle }

func (p *BundledPlugin) Tools() []PluginToolDefinition {
	return toolDefsFromManifest(p.Manifest.Tools)
}

func (p *BundledPlugin) Validate() error {
	return validatePaths(p.Meta.Root, p.Manifest.Tools)
}

func (p *BundledPlugin) Initialize() error {
	return runLifecycleCommands(p.Meta.Root, p.Manifest.Lifecycle.Init)
}

func (p *BundledPlugin) Shutdown() error {
	return runLifecycleCommands(p.Meta.Root, p.Manifest.Lifecycle.Shutdown)
}

// --- ExternalPlugin ---

// ExternalPlugin is a user-installed plugin.
type ExternalPlugin struct {
	Meta     PluginMetadata
	Manifest PluginManifest
}

func (p *ExternalPlugin) Metadata() PluginMetadata   { return p.Meta }
func (p *ExternalPlugin) Hooks() PluginHooks         { return p.Manifest.Hooks }
func (p *ExternalPlugin) Lifecycle() PluginLifecycle { return p.Manifest.Lifecycle }

func (p *ExternalPlugin) Tools() []PluginToolDefinition {
	return toolDefsFromManifest(p.Manifest.Tools)
}

func (p *ExternalPlugin) Validate() error {
	return validatePaths(p.Meta.Root, p.Manifest.Tools)
}

func (p *ExternalPlugin) Initialize() error {
	return runLifecycleCommands(p.Meta.Root, p.Manifest.Lifecycle.Init)
}

func (p *ExternalPlugin) Shutdown() error {
	return runLifecycleCommands(p.Meta.Root, p.Manifest.Lifecycle.Shutdown)
}

// --- Helpers ---

func toolDefsFromManifest(tools []PluginToolManifest) []PluginToolDefinition {
	defs := make([]PluginToolDefinition, len(tools))
	for i, t := range tools {
		defs[i] = PluginToolDefinition{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: t.InputSchema,
		}
	}
	return defs
}

func validatePaths(root string, tools []PluginToolManifest) error {
	for _, t := range tools {
		if t.Command == "" {
			continue
		}
		// Skip PATH-resolved shell commands (e.g. "python my_script.py").
		if isLiteralCommand(t.Command) {
			continue
		}
		cmdPath := t.Command
		if !filepath.IsAbs(cmdPath) {
			cmdPath = filepath.Join(root, cmdPath)
		}
		if _, err := os.Stat(cmdPath); err != nil {
			return &PluginError{
				Kind:    ErrManifestValidation,
				Message: fmt.Sprintf("tool %q command path not found: %s", t.Name, cmdPath),
				Cause:   err,
			}
		}
	}
	return nil
}

func runLifecycleCommands(root string, commands []string) error {
	for _, cmd := range commands {
		shell, args := shellArgs(cmd)
		c := exec.Command(shell, args...)
		if root != "" {
			c.Dir = root
		}
		c.Stdout = os.Stdout
		c.Stderr = os.Stderr
		if err := c.Run(); err != nil {
			return &PluginError{
				Kind:    ErrCommandFailed,
				Message: fmt.Sprintf("lifecycle command failed: %s", cmd),
				Cause:   err,
			}
		}
	}
	return nil
}
