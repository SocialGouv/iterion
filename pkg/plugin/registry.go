package plugin

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	yaml "go.yaml.in/yaml/v2"

	"github.com/SocialGouv/iterion/pkg/store"
)

//go:embed all:builtin
var builtinFS embed.FS

// ManifestFile is the manifest filename at a plugin's root.
const ManifestFile = "plugin.yaml"

// stateFile is the enable/disable state file under the iterion home.
const stateFile = "plugins.yaml"

// pluginsSubdir is the install directory under the iterion home.
const pluginsSubdir = "plugins"

// Plugin is a loaded plugin: its manifest plus where it came from and how to
// read its bundled files (skills).
type Plugin struct {
	Manifest Manifest
	// Builtin is true for plugins embedded in the binary.
	Builtin bool
	// Dir is the absolute install directory for an installed plugin; "" for a
	// builtin (its files live in the embedded FS).
	Dir string
	// Enabled is the resolved enable state (operator state || default_enabled).
	Enabled bool

	// fsys roots skill-file reads, relative to the plugin root.
	fsys fs.FS
}

// Name returns the plugin's manifest name.
func (p *Plugin) Name() string { return p.Manifest.Name }

// SkillFile is a resolved skill: its base name and content.
type SkillFile struct {
	Name    string
	Content []byte
}

// MirrorKind names a markdown contribution kind mirrored into a workspace
// .claude/<dir>/ directory.
type MirrorKind struct {
	Name string // "skill" | "command" | "agent"
	Dir  string // ".claude/<dir>" leaf: "skills" | "commands" | "agents"
}

// MirrorKinds is the set of markdown contribution kinds, each mirrored into its
// own .claude/ subdir with the shared collision policy.
var MirrorKinds = []MirrorKind{
	{Name: "skill", Dir: "skills"},
	{Name: "command", Dir: "commands"},
	{Name: "agent", Dir: "agents"},
}

// contribPaths returns the relative paths a plugin contributes for a kind.
func (p *Plugin) contribPaths(kind MirrorKind) []string {
	switch kind.Name {
	case "skill":
		return p.Manifest.Contributes.Skills
	case "command":
		return p.Manifest.Contributes.Commands
	case "agent":
		return p.Manifest.Contributes.Agents
	}
	return nil
}

// MirrorFiles reads the plugin's contributed files for a markdown kind.
func (p *Plugin) MirrorFiles(kind MirrorKind) ([]SkillFile, error) {
	var out []SkillFile
	for _, rel := range p.contribPaths(kind) {
		data, err := fs.ReadFile(p.fsys, filepath.ToSlash(rel))
		if err != nil {
			return nil, fmt.Errorf("plugin %q: read %s %q: %w", p.Name(), kind.Name, rel, err)
		}
		out = append(out, SkillFile{Name: filepath.Base(rel), Content: data})
	}
	return out, nil
}

// SkillFiles reads the plugin's contributed skill files (back-compat shorthand).
func (p *Plugin) SkillFiles() ([]SkillFile, error) {
	return p.MirrorFiles(MirrorKind{Name: "skill", Dir: "skills"})
}

// Registry is the loaded set of plugins (builtins + installed) with resolved
// enable state. It is read-mostly; SetEnabled / Remove mutate persisted state.
type Registry struct {
	home    string
	plugins []*Plugin
	state   map[string]bool // explicit enable/disable overrides
	// config holds per-plugin operator config values (plugin name → key → value),
	// persisted alongside enable state in plugins.yaml.
	config map[string]map[string]string
}

// Load builds a registry from the embedded builtins and the installed plugins
// under <iterion-home>/plugins/, applying the persisted enable state. A
// malformed installed plugin is skipped (logged by the caller via the returned
// error slice); a malformed builtin is a programming error and fails the load.
func Load() (*Registry, error) {
	home := store.GlobalIterionDataDir()
	r := &Registry{home: home}
	if err := r.loadState(); err != nil {
		return nil, err
	}
	if err := r.loadBuiltins(); err != nil {
		return nil, err
	}
	r.loadInstalled()
	r.resolveEnabled()
	return r, nil
}

func (r *Registry) loadState() error {
	r.state = map[string]bool{}
	r.config = map[string]map[string]string{}
	data, err := os.ReadFile(filepath.Join(r.home, stateFile))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("plugin: read %s: %w", stateFile, err)
	}
	var s struct {
		Enabled map[string]bool              `yaml:"enabled"`
		Config  map[string]map[string]string `yaml:"config"`
	}
	if err := yaml.Unmarshal(data, &s); err != nil {
		return fmt.Errorf("plugin: parse %s: %w", stateFile, err)
	}
	if s.Enabled != nil {
		r.state = s.Enabled
	}
	if s.Config != nil {
		r.config = s.Config
	}
	return nil
}

func (r *Registry) loadBuiltins() error {
	entries, err := fs.ReadDir(builtinFS, "builtin")
	if err != nil {
		return fmt.Errorf("plugin: read embedded builtins: %w", err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		root := "builtin/" + e.Name()
		data, err := fs.ReadFile(builtinFS, root+"/"+ManifestFile)
		if err != nil {
			return fmt.Errorf("plugin: builtin %q missing %s: %w", e.Name(), ManifestFile, err)
		}
		m, err := ParseManifest(data)
		if err != nil {
			return fmt.Errorf("plugin: builtin %q: %w", e.Name(), err)
		}
		sub, err := fs.Sub(builtinFS, root)
		if err != nil {
			return err
		}
		r.plugins = append(r.plugins, &Plugin{Manifest: *m, Builtin: true, fsys: sub})
	}
	return nil
}

// loadInstalled scans <home>/plugins/*/plugin.yaml. Errors on individual
// plugins are swallowed (a broken third-party plugin must not brick iterion);
// a builtin of the same name takes precedence and the installed copy is skipped.
func (r *Registry) loadInstalled() {
	base := filepath.Join(r.home, pluginsSubdir)
	entries, err := os.ReadDir(base)
	if err != nil {
		return
	}
	have := map[string]bool{}
	for _, p := range r.plugins {
		have[p.Name()] = true
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(base, e.Name())
		data, err := os.ReadFile(filepath.Join(dir, ManifestFile))
		if err != nil {
			continue
		}
		m, err := ParseManifest(data)
		if err != nil || have[m.Name] {
			continue
		}
		have[m.Name] = true
		r.plugins = append(r.plugins, &Plugin{Manifest: *m, Dir: dir, fsys: os.DirFS(dir)})
	}
}

func (r *Registry) resolveEnabled() {
	for _, p := range r.plugins {
		if v, ok := r.state[p.Name()]; ok {
			p.Enabled = v
		} else {
			p.Enabled = p.Manifest.DefaultEnabled
		}
	}
	sort.SliceStable(r.plugins, func(i, j int) bool {
		return r.plugins[i].Name() < r.plugins[j].Name()
	})
}

// Get returns the plugin with the given name.
func (r *Registry) Get(name string) (*Plugin, bool) {
	for _, p := range r.plugins {
		if p.Name() == name {
			return p, true
		}
	}
	return nil, false
}

// IsEnabled reports whether the named plugin is enabled.
func (r *Registry) IsEnabled(name string) bool {
	p, ok := r.Get(name)
	return ok && p.Enabled
}

// Enabled returns the enabled plugins, sorted by name (stable chain order).
func (r *Registry) Enabled() []*Plugin {
	var out []*Plugin
	for _, p := range r.plugins {
		if p.Enabled {
			out = append(out, p)
		}
	}
	return out
}

// SetEnabled persists an enable/disable decision for a plugin and updates the
// in-memory state. It errors if the plugin is unknown.
func (r *Registry) SetEnabled(name string, enabled bool) error {
	p, ok := r.Get(name)
	if !ok {
		return fmt.Errorf("plugin %q not found", name)
	}
	r.state[name] = enabled
	p.Enabled = enabled
	return r.saveState()
}

func (r *Registry) saveState() error {
	if err := os.MkdirAll(r.home, 0o700); err != nil {
		return err
	}
	// Drop empty per-plugin config maps so an unconfigured plugin leaves no
	// noise in plugins.yaml.
	cfg := map[string]map[string]string{}
	for name, vals := range r.config {
		if len(vals) > 0 {
			cfg[name] = vals
		}
	}
	data, err := yaml.Marshal(struct {
		Enabled map[string]bool              `yaml:"enabled"`
		Config  map[string]map[string]string `yaml:"config,omitempty"`
	}{Enabled: r.state, Config: cfg})
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(r.home, stateFile), data, 0o600)
}

// Remove deletes an installed plugin's directory and clears its state. Builtin
// plugins cannot be removed (disable them instead).
func (r *Registry) Remove(name string) error {
	p, ok := r.Get(name)
	if !ok {
		return fmt.Errorf("plugin %q not found", name)
	}
	if p.Builtin {
		return fmt.Errorf("plugin %q is builtin and cannot be uninstalled (disable it instead)", name)
	}
	if err := os.RemoveAll(p.Dir); err != nil {
		return fmt.Errorf("plugin: remove %q: %w", name, err)
	}
	delete(r.state, name)
	// Drop from in-memory list.
	out := r.plugins[:0]
	for _, q := range r.plugins {
		if q.Name() != name {
			out = append(out, q)
		}
	}
	r.plugins = out
	return r.saveState()
}

// InstallDir returns the directory an installed plugin lives in (or would).
func (r *Registry) InstallDir(name string) string {
	return filepath.Join(r.home, pluginsSubdir, name)
}

// CacheDir returns a per-plugin cache directory under the iterion home, used to
// expand the {{plugin.cache}} placeholder.
func (r *Registry) CacheDir(name string) string {
	return filepath.Join(r.home, pluginsSubdir, name, "cache")
}

// View is the listing-facing projection of a plugin (name, enable state, and
// which contribution kinds it provides). Shared by the CLI and the HTTP server
// so both render plugins identically without an import cycle.
type View struct {
	Name        string   `json:"name"`
	Version     string   `json:"version,omitempty"`
	Description string   `json:"description,omitempty"`
	Author      string   `json:"author,omitempty"`
	Enabled     bool     `json:"enabled"`
	Builtin     bool     `json:"builtin"`
	Kinds       []string `json:"kinds"`
	// ConfigSchema is the plugin's declared config fields (empty when the plugin
	// has no config block). ConfigValues carries the current values for the
	// NON-secret fields; ConfigSecretSet names the secret fields that currently
	// have a value (their value is never sent to the studio). The registry fills
	// the value-bearing fields (Plugin.View only knows the schema).
	ConfigSchema    []ConfigField     `json:"config_schema,omitempty"`
	ConfigValues    map[string]string `json:"config_values,omitempty"`
	ConfigSecretSet []string          `json:"config_secret_set,omitempty"`
}

// Kinds summarises which contribution points a manifest provides.
func (m Manifest) Kinds() []string {
	var k []string
	if len(m.Contributes.Rewriters) > 0 {
		k = append(k, "rewriter")
	}
	if len(m.Contributes.MCPServers) > 0 {
		k = append(k, "mcp")
	}
	if len(m.Contributes.Skills) > 0 {
		k = append(k, "skill")
	}
	if len(m.Contributes.Commands) > 0 {
		k = append(k, "command")
	}
	if len(m.Contributes.Agents) > 0 {
		k = append(k, "agent")
	}
	if len(m.Contributes.Hooks) > 0 {
		k = append(k, "hook")
	}
	if m.Contributes.Lifecycle != nil {
		k = append(k, "lifecycle")
	}
	return k
}

// HookFragments reads and JSON-decodes the plugin's contributed hook settings
// fragments. Each fragment is a settings.json shape: either {"hooks": {...}}
// or the bare {<Event>: [...]} map. The returned maps are the hooks map only.
func (p *Plugin) HookFragments() ([]map[string]any, error) {
	var out []map[string]any
	for _, rel := range p.Manifest.Contributes.Hooks {
		data, err := fs.ReadFile(p.fsys, filepath.ToSlash(rel))
		if err != nil {
			return nil, fmt.Errorf("plugin %q: read hooks %q: %w", p.Name(), rel, err)
		}
		var doc map[string]any
		if jerr := json.Unmarshal(data, &doc); jerr != nil {
			return nil, fmt.Errorf("plugin %q: parse hooks %q: %w", p.Name(), rel, jerr)
		}
		// Accept a full settings fragment ({"hooks": {...}}) or a bare hooks map.
		if h, ok := doc["hooks"].(map[string]any); ok {
			out = append(out, h)
		} else {
			out = append(out, doc)
		}
	}
	return out, nil
}

// View projects a plugin to its listing form.
func (p *Plugin) View() View {
	return View{
		Name:         p.Name(),
		Version:      p.Manifest.Version,
		Description:  p.Manifest.Description,
		Author:       p.Manifest.Author,
		Enabled:      p.Enabled,
		Builtin:      p.Builtin,
		Kinds:        p.Manifest.Kinds(),
		ConfigSchema: p.Manifest.Config,
	}
}

// Views returns the listing form of every loaded plugin, each with its config
// schema + current (secret-masked) values filled in.
func (r *Registry) Views() []View {
	out := make([]View, 0, len(r.plugins))
	for _, p := range r.plugins {
		out = append(out, r.fillConfigView(p.View(), p))
	}
	return out
}

// RewriterContribution pairs a rewriter spec with its owning plugin name.
type RewriterContribution struct {
	Plugin string
	Spec   RewriterSpec
}

// EnabledRewriters returns the rewriter contributions of all enabled plugins,
// in stable plugin-name order — this is the rewrite chain applied to commands.
func (r *Registry) EnabledRewriters() []RewriterContribution {
	var out []RewriterContribution
	for _, p := range r.Enabled() {
		for _, rw := range p.Manifest.Contributes.Rewriters {
			out = append(out, RewriterContribution{Plugin: p.Name(), Spec: rw})
		}
	}
	return out
}

// ExpandContext carries the values used to expand activation-time placeholders
// in mcp_servers args/env and lifecycle commands.
type ExpandContext struct {
	Workspace string
	PluginDir string
	CacheDir  string
	// Config is the plugin's effective config (defaults overlaid with operator
	// values), exposed as {{config.<key>}} placeholders.
	Config map[string]string
}

// Expand substitutes {{workspace}}, {{plugin.dir}}, {{plugin.cache}} and
// {{config.<key>}} in s.
func (e ExpandContext) Expand(s string) string {
	pairs := []string{
		"{{workspace}}", e.Workspace,
		"{{plugin.dir}}", e.PluginDir,
		"{{plugin.cache}}", e.CacheDir,
	}
	for k, v := range e.Config {
		pairs = append(pairs, "{{config."+k+"}}", v)
	}
	return strings.NewReplacer(pairs...).Replace(s)
}

// ExpandContextFor builds an ExpandContext for the named plugin in a workspace.
func (r *Registry) ExpandContextFor(name, workspace string) ExpandContext {
	dir := ""
	if p, ok := r.Get(name); ok {
		dir = p.Dir
	}
	return ExpandContext{
		Workspace: workspace,
		PluginDir: dir,
		CacheDir:  r.CacheDir(name),
		Config:    r.EffectiveConfig(name),
	}
}
