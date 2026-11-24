package plugin

import (
	"fmt"
	"sort"
)

// RegisteredPlugin wraps a Plugin with its enabled state.
type RegisteredPlugin struct {
	Plugin  Plugin
	Enabled bool
}

// PluginSummary is a displayable plugin summary.
type PluginSummary struct {
	Metadata PluginMetadata
	Enabled  bool
}

// PluginRegistry manages a sorted collection of plugins.
type PluginRegistry struct {
	plugins []RegisteredPlugin
}

// NewPluginRegistry creates a registry and sorts plugins by ID.
func NewPluginRegistry(plugins []RegisteredPlugin) *PluginRegistry {
	sorted := make([]RegisteredPlugin, len(plugins))
	copy(sorted, plugins)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].Plugin.Metadata().ID < sorted[j].Plugin.Metadata().ID
	})
	return &PluginRegistry{plugins: sorted}
}

// Plugins returns all registered plugins.
func (r *PluginRegistry) Plugins() []RegisteredPlugin {
	return r.plugins
}

// Get returns the registered plugin with the given ID, or nil if not found.
func (r *PluginRegistry) Get(pluginID string) *RegisteredPlugin {
	for i := range r.plugins {
		if r.plugins[i].Plugin.Metadata().ID == pluginID {
			return &r.plugins[i]
		}
	}
	return nil
}

// Contains returns true if a plugin with the given ID is registered.
func (r *PluginRegistry) Contains(pluginID string) bool {
	return r.Get(pluginID) != nil
}

// Summaries returns a displayable summary for each plugin.
func (r *PluginRegistry) Summaries() []PluginSummary {
	summaries := make([]PluginSummary, len(r.plugins))
	for i, rp := range r.plugins {
		summaries[i] = PluginSummary{
			Metadata: rp.Plugin.Metadata(),
			Enabled:  rp.Enabled,
		}
	}
	return summaries
}

// AggregatedHooks merges all enabled plugins' hooks. Each enabled plugin is
// validated first; returns an error if any validation fails (matching Rust).
func (r *PluginRegistry) AggregatedHooks() (PluginHooks, error) {
	var agg PluginHooks
	for _, rp := range r.plugins {
		if !rp.Enabled {
			continue
		}
		if err := rp.Plugin.Validate(); err != nil {
			return PluginHooks{}, err
		}
		h := rp.Plugin.Hooks()
		agg.PreToolUse = append(agg.PreToolUse, h.PreToolUse...)
		agg.PostToolUse = append(agg.PostToolUse, h.PostToolUse...)
		agg.PostToolUseFailure = append(agg.PostToolUseFailure, h.PostToolUseFailure...)
	}
	return agg, nil
}

// AggregatedTools collects all enabled plugins' tools. Returns an error
// immediately on the first tool name conflict, matching Rust behavior.
func (r *PluginRegistry) AggregatedTools() ([]PluginTool, error) {
	var tools []PluginTool
	seen := make(map[string]string) // tool name -> plugin ID

	for _, rp := range r.plugins {
		if !rp.Enabled {
			continue
		}
		if err := rp.Plugin.Validate(); err != nil {
			return nil, err
		}
		meta := rp.Plugin.Metadata()

		// Get tool definitions from the plugin
		toolDefs := rp.Plugin.Tools()

		// Get manifest tools for command/args/permission info
		var manifestTools []PluginToolManifest
		switch p := rp.Plugin.(type) {
		case *BundledPlugin:
			manifestTools = p.Manifest.Tools
		case *ExternalPlugin:
			manifestTools = p.Manifest.Tools
		}

		// Build a lookup from manifest tools by name
		manifestByName := make(map[string]PluginToolManifest)
		for _, mt := range manifestTools {
			manifestByName[mt.Name] = mt
		}

		for _, td := range toolDefs {
			if prevPlugin, exists := seen[td.Name]; exists {
				return nil, &PluginError{
					Kind:    ErrInvalidManifest,
					Message: fmt.Sprintf("plugin tool %q is defined by both %q and %q", td.Name, prevPlugin, meta.ID),
				}
			}
			seen[td.Name] = meta.ID

			pt := PluginTool{
				PluginID:   meta.ID,
				PluginName: meta.Name,
				Definition: td,
				Root:       meta.Root,
			}

			if mt, ok := manifestByName[td.Name]; ok {
				pt.Command = mt.Command
				pt.Args = mt.Args
				pt.RequiredPermission = mt.RequiredPermission
			}

			tools = append(tools, pt)
		}
	}

	return tools, nil
}

// Initialize initializes all enabled plugins.
func (r *PluginRegistry) Initialize() error {
	for _, rp := range r.plugins {
		if !rp.Enabled {
			continue
		}
		if err := rp.Plugin.Initialize(); err != nil {
			return fmt.Errorf("failed to initialize plugin %q: %w", rp.Plugin.Metadata().ID, err)
		}
	}
	return nil
}

// Shutdown shuts down all enabled plugins in reverse order.
func (r *PluginRegistry) Shutdown() error {
	for i := len(r.plugins) - 1; i >= 0; i-- {
		rp := r.plugins[i]
		if !rp.Enabled {
			continue
		}
		if err := rp.Plugin.Shutdown(); err != nil {
			return fmt.Errorf("failed to shutdown plugin %q: %w", rp.Plugin.Metadata().ID, err)
		}
	}
	return nil
}
