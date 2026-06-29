package plugin

import (
	"embed"
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

// SkillFiles reads the plugin's contributed skill files.
func (p *Plugin) SkillFiles() ([]SkillFile, error) {
	var out []SkillFile
	for _, rel := range p.Manifest.Contributes.Skills {
		data, err := fs.ReadFile(p.fsys, filepath.ToSlash(rel))
		if err != nil {
			return nil, fmt.Errorf("plugin %q: read skill %q: %w", p.Name(), rel, err)
		}
		out = append(out, SkillFile{Name: filepath.Base(rel), Content: data})
	}
	return out, nil
}

// Registry is the loaded set of plugins (builtins + installed) with resolved
// enable state. It is read-mostly; SetEnabled / Remove mutate persisted state.
type Registry struct {
	home    string
	plugins []*Plugin
	state   map[string]bool // explicit operator overrides
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
	data, err := os.ReadFile(filepath.Join(r.home, stateFile))
	if err != nil {
		if os.IsNotExist(err) {
			r.state = map[string]bool{}
			return nil
		}
		return fmt.Errorf("plugin: read %s: %w", stateFile, err)
	}
	var s struct {
		Enabled map[string]bool `yaml:"enabled"`
	}
	if err := yaml.Unmarshal(data, &s); err != nil {
		return fmt.Errorf("plugin: parse %s: %w", stateFile, err)
	}
	if s.Enabled == nil {
		s.Enabled = map[string]bool{}
	}
	r.state = s.Enabled
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

// List returns all loaded plugins sorted by name.
func (r *Registry) List() []*Plugin { return r.plugins }

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
	data, err := yaml.Marshal(struct {
		Enabled map[string]bool `yaml:"enabled"`
	}{Enabled: r.state})
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
	if m.Contributes.Lifecycle != nil {
		k = append(k, "lifecycle")
	}
	return k
}

// View projects a plugin to its listing form.
func (p *Plugin) View() View {
	return View{
		Name:        p.Name(),
		Version:     p.Manifest.Version,
		Description: p.Manifest.Description,
		Author:      p.Manifest.Author,
		Enabled:     p.Enabled,
		Builtin:     p.Builtin,
		Kinds:       p.Manifest.Kinds(),
	}
}

// Views returns the listing form of every loaded plugin.
func (r *Registry) Views() []View {
	out := make([]View, 0, len(r.plugins))
	for _, p := range r.plugins {
		out = append(out, p.View())
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
}

// Expand substitutes {{workspace}}, {{plugin.dir}} and {{plugin.cache}} in s.
func (e ExpandContext) Expand(s string) string {
	rep := strings.NewReplacer(
		"{{workspace}}", e.Workspace,
		"{{plugin.dir}}", e.PluginDir,
		"{{plugin.cache}}", e.CacheDir,
	)
	return rep.Replace(s)
}

// ExpandContextFor builds an ExpandContext for the named plugin in a workspace.
func (r *Registry) ExpandContextFor(name, workspace string) ExpandContext {
	dir := ""
	if p, ok := r.Get(name); ok {
		dir = p.Dir
	}
	return ExpandContext{Workspace: workspace, PluginDir: dir, CacheDir: r.CacheDir(name)}
}
