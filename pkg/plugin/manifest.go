// Package plugin implements iterion's plugin ecosystem: declarative,
// out-of-process extensions described by a `plugin.yaml` manifest with typed
// contribution points. iterion cannot use Go's `plugin` package (it ships
// static CGO_ENABLED=0 binaries that are bind-mounted into sandbox containers),
// so a plugin never injects Go code — it declares WHAT to contribute and the
// runtime wires it into iterion's existing seams:
//
//	rewriters   → command-output compressors (the rtk generalization) applied
//	              on the three shell surfaces (claude_code hook, claw builtin,
//	              tool node), composable as an ordered chain.
//	mcp_servers → MCP servers merged into the workflow MCP catalog (the natural
//	              home for knowledge-graph explorers like repo-falcon).
//	skills      → markdown skills mirrored into <workspace>/.claude/skills/.
//	lifecycle   → index/refresh commands (e.g. build/refresh a code graph).
//
// Plugins load from two sources: builtins embedded in the binary (rtk enabled
// by default; graphify + repo-falcon shipped disabled) and installed plugins
// under ~/.iterion/plugins/<name>/. Enable/disable state lives in
// ~/.iterion/plugins.yaml; the marketplace installs third-party plugins into
// the same directory.
package plugin

import (
	"fmt"
	"strings"

	yaml "go.yaml.in/yaml/v2"
)

// SchemaVersion is the current plugin.yaml schema. Unknown future versions are
// rejected by ParseManifest so an old binary fails loudly rather than silently
// dropping contributions it cannot honour.
const SchemaVersion = 1

// Manifest is a parsed `plugin.yaml`.
type Manifest struct {
	// Name is the plugin's unique id (kebab-case). It is also the directory
	// name under ~/.iterion/plugins/ and the enable/disable key.
	Name string `yaml:"name"`
	// Version is free-form (semver recommended). Surfaced in `plugin list`.
	Version string `yaml:"version"`
	// Description is the one-line summary shown in listings.
	Description string `yaml:"description"`
	// Author is free-form attribution.
	Author string `yaml:"author"`
	// SchemaVersion defaults to 1 when omitted.
	SchemaVersion int `yaml:"schema_version"`
	// DefaultEnabled is the enable state when the operator has expressed no
	// preference in plugins.yaml. rtk ships true; KG explorers ship false.
	DefaultEnabled bool `yaml:"default_enabled"`
	// AutoIndex, when true, runs the lifecycle `index` command before a run if
	// the plugin is enabled and contributes a lifecycle.
	AutoIndex bool `yaml:"auto_index"`
	// Contributes lists the typed extension points this plugin provides.
	Contributes Contributes `yaml:"contributes"`
}

// Contributes is the set of typed contribution points.
//
// Skills, Commands and Agents are markdown files mirrored into the workspace's
// .claude/<skills|commands|agents>/ directory at run start (claude_code
// discovers them via --setting-sources project; the claw backend reads the
// same dirs). They share one mirror mechanism and one collision policy.
type Contributes struct {
	Rewriters  []RewriterSpec  `yaml:"rewriters"`
	MCPServers []MCPServerSpec `yaml:"mcp_servers"`
	Skills     []string        `yaml:"skills"`
	Commands   []string        `yaml:"commands"`
	Agents     []string        `yaml:"agents"`
	Lifecycle  *LifecycleSpec  `yaml:"lifecycle"`
}

// RewriterSpec declares a command-output rewriter backed by an external binary.
// It fully captures, declaratively, what was hardcoded for rtk: how to locate
// the binary, how to invoke it, which exit codes mean "apply the rewrite", and
// the per-mode transform of the produced rewrite.
type RewriterSpec struct {
	// ID is the rewriter's id within the chain (usually equals the plugin name
	// for a single-rewriter plugin).
	ID string `yaml:"id"`
	// Locate resolves the binary path.
	Locate LocateSpec `yaml:"locate"`
	// Invoke describes the subprocess contract.
	Invoke InvokeSpec `yaml:"invoke"`
	// SandboxMount, when set, is the in-container path the host binary is
	// bind-mounted to for sandboxed runs (e.g. /usr/local/bin/rtk).
	SandboxMount string `yaml:"sandbox_mount"`
}

// LocateSpec resolves a binary: env override first, then PATH (Bin), then the
// conventional install Paths. Empty fields are skipped.
type LocateSpec struct {
	Env   string   `yaml:"env"`
	Bin   string   `yaml:"bin"`
	Paths []string `yaml:"paths"`
}

// InvokeSpec is the subprocess contract for a rewriter.
type InvokeSpec struct {
	// Argv is the argument vector. Exactly one element must contain the
	// "{{command}}" placeholder, substituted with the full shell command line.
	Argv []string `yaml:"argv"`
	// Env are extra environment variables set on every invocation (merged over
	// the inherited process env, which wins on conflict for operator override).
	Env map[string]string `yaml:"env"`
	// TimeoutMs bounds a single invocation; 0 → DefaultRewriteTimeoutMs.
	TimeoutMs int `yaml:"timeout_ms"`
	// ApplyExitCodes are the exit codes whose stdout is taken as the rewrite.
	// Empty defaults to {0}. (rtk uses {0,3}: Default verdict maps to Ask=3.)
	ApplyExitCodes []int `yaml:"apply_exit_codes"`
	// Modes maps a generic intensity level (on|ultra) to its transform. A mode
	// absent here is still accepted (it simply applies no extra transform).
	Modes map[string]ModeSpec `yaml:"modes"`
}

// ModeSpec is the per-mode transform applied to a successful rewrite.
type ModeSpec struct {
	// InjectFlag, when set, is inserted right after the binary name in the
	// produced rewrite (e.g. rtk "ultra" → insert "--ultra-compact").
	InjectFlag string `yaml:"inject_flag"`
}

// MCPServerSpec declares an MCP server contribution. It mirrors the runtime
// mcp.ServerConfig shape; placeholders ({{workspace}}, {{plugin.dir}},
// {{plugin.cache}}) are expanded at activation time.
type MCPServerSpec struct {
	Name      string            `yaml:"name"`
	Transport string            `yaml:"transport"`
	Command   string            `yaml:"command"`
	Args      []string          `yaml:"args"`
	URL       string            `yaml:"url"`
	Headers   map[string]string `yaml:"headers"`
	Env       map[string]string `yaml:"env"`
}

// LifecycleSpec declares index/refresh commands for graph-building plugins.
// Commands are shell strings run via `sh -c` with placeholders expanded.
type LifecycleSpec struct {
	Index   string `yaml:"index"`
	Refresh string `yaml:"refresh"`
}

// CommandPlaceholder is the token in a rewriter argv replaced by the full
// shell command line at rewrite time.
const CommandPlaceholder = "{{command}}"

// ParseManifest decodes and validates a plugin.yaml document.
func ParseManifest(data []byte) (*Manifest, error) {
	var m Manifest
	if err := yaml.UnmarshalStrict(data, &m); err != nil {
		return nil, fmt.Errorf("plugin: parse manifest: %w", err)
	}
	if m.SchemaVersion == 0 {
		m.SchemaVersion = SchemaVersion
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return &m, nil
}

// Validate checks the manifest is well-formed and that every contribution is
// usable. It is intentionally strict — a malformed plugin should fail at load,
// not silently contribute nothing.
func (m *Manifest) Validate() error {
	if strings.TrimSpace(m.Name) == "" {
		return fmt.Errorf("plugin: manifest missing name")
	}
	if m.SchemaVersion > SchemaVersion {
		return fmt.Errorf("plugin %q: schema_version %d newer than supported %d (upgrade iterion)", m.Name, m.SchemaVersion, SchemaVersion)
	}
	c := m.Contributes
	if len(c.Rewriters) == 0 && len(c.MCPServers) == 0 && len(c.Skills) == 0 &&
		len(c.Commands) == 0 && len(c.Agents) == 0 && c.Lifecycle == nil {
		return fmt.Errorf("plugin %q: contributes nothing", m.Name)
	}
	for i := range c.Rewriters {
		if err := c.Rewriters[i].validate(m.Name); err != nil {
			return err
		}
	}
	for i := range c.MCPServers {
		if err := c.MCPServers[i].validate(m.Name); err != nil {
			return err
		}
	}
	return nil
}

func (r *RewriterSpec) validate(plugin string) error {
	if strings.TrimSpace(r.ID) == "" {
		return fmt.Errorf("plugin %q: rewriter missing id", plugin)
	}
	if strings.TrimSpace(r.Locate.Bin) == "" && len(r.Locate.Paths) == 0 && strings.TrimSpace(r.Locate.Env) == "" {
		return fmt.Errorf("plugin %q rewriter %q: locate has no env/bin/paths", plugin, r.ID)
	}
	seen := false
	for _, a := range r.Invoke.Argv {
		if strings.Contains(a, CommandPlaceholder) {
			seen = true
		}
	}
	if !seen {
		return fmt.Errorf("plugin %q rewriter %q: invoke.argv must contain %s", plugin, r.ID, CommandPlaceholder)
	}
	return nil
}

func (s *MCPServerSpec) validate(plugin string) error {
	if strings.TrimSpace(s.Name) == "" {
		return fmt.Errorf("plugin %q: mcp server missing name", plugin)
	}
	t := s.Transport
	if t == "" {
		t = "stdio"
	}
	switch t {
	case "stdio":
		if strings.TrimSpace(s.Command) == "" {
			return fmt.Errorf("plugin %q mcp server %q: stdio transport needs a command", plugin, s.Name)
		}
	case "http", "sse":
		if strings.TrimSpace(s.URL) == "" {
			return fmt.Errorf("plugin %q mcp server %q: %s transport needs a url", plugin, s.Name, t)
		}
	default:
		return fmt.Errorf("plugin %q mcp server %q: unknown transport %q", plugin, s.Name, t)
	}
	return nil
}

// ApplyExitCodesOrDefault returns the configured apply exit codes, defaulting
// to {0} when none are declared.
func (r *RewriterSpec) ApplyExitCodesOrDefault() []int {
	if len(r.Invoke.ApplyExitCodes) == 0 {
		return []int{0}
	}
	return r.Invoke.ApplyExitCodes
}
