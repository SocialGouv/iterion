package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	hooks "github.com/SocialGouv/claw-code-go/internal/hooks"
)

// PluginManagerConfig holds discovery configuration.
type PluginManagerConfig struct {
	ConfigHome     string
	EnabledPlugins map[string]bool
	ExternalDirs   []string
	InstallRoot    string // defaults to ConfigHome/plugins/installed
	RegistryPath   string // defaults to ConfigHome/plugins/installed.json
	BundledRoot    string
}

// InstallOutcome records a successful installation.
type InstallOutcome struct {
	PluginID    string
	Version     string
	InstallPath string
}

// UpdateOutcome records a successful update.
type UpdateOutcome struct {
	PluginID    string
	OldVersion  string
	NewVersion  string
	InstallPath string
}

// PluginInstallSource identifies where a plugin was installed from.
// Serialized as a tagged union matching Rust's serde(tag="type") layout.
type PluginInstallSource struct {
	Type string `json:"type"`           // "local_path" or "git_url"
	Path string `json:"path,omitempty"` // for local_path
	URL  string `json:"url,omitempty"`  // for git_url
}

// LocalPathSource creates an install source for a local directory.
func LocalPathSource(path string) PluginInstallSource {
	return PluginInstallSource{Type: "local_path", Path: path}
}

// GitURLSource creates an install source for a git repository.
func GitURLSource(url string) PluginInstallSource {
	return PluginInstallSource{Type: "git_url", URL: url}
}

// SourcePath returns the filesystem path for local_path sources,
// or empty string for git_url sources.
func (s PluginInstallSource) SourcePath() string {
	if s.Type == "local_path" {
		return s.Path
	}
	return ""
}

// InstalledPluginRecord persists info about an installed plugin.
type InstalledPluginRecord struct {
	Kind          PluginKind          `json:"kind"`
	ID            string              `json:"id"`
	Name          string              `json:"name"`
	Version       string              `json:"version"`
	Description   string              `json:"description"`
	InstallPath   string              `json:"install_path"`
	Source        PluginInstallSource `json:"source"`
	InstalledAtMs int64               `json:"installed_at_unix_ms"`
	UpdatedAtMs   int64               `json:"updated_at_unix_ms"`
}

// InstalledPluginRegistry persists all installed plugin records.
type InstalledPluginRegistry struct {
	Plugins map[string]InstalledPluginRecord `json:"plugins"`
}

// PluginManager handles plugin discovery, installation, and lifecycle.
type PluginManager struct {
	config   PluginManagerConfig
	registry InstalledPluginRegistry
	hooks    *hooks.Runner
}

// ManagerOption configures a PluginManager at construction time.
type ManagerOption func(*PluginManager)

// WithHooks attaches a lifecycle hooks Runner to the PluginManager. The
// runner receives Pre/Post Install and Pre/Post Uninstall events. Passing
// nil is a documented no-op (matches default behavior).
func WithHooks(r *hooks.Runner) ManagerOption {
	return func(m *PluginManager) {
		m.hooks = r
	}
}

// NewPluginManager creates a plugin manager and loads the installed plugin registry.
func NewPluginManager(config PluginManagerConfig, opts ...ManagerOption) (*PluginManager, error) {
	if config.InstallRoot == "" {
		config.InstallRoot = filepath.Join(config.ConfigHome, "plugins", "installed")
	}
	if config.RegistryPath == "" {
		config.RegistryPath = filepath.Join(config.ConfigHome, "plugins", "installed.json")
	}

	m := &PluginManager{
		config: config,
		registry: InstalledPluginRegistry{
			Plugins: make(map[string]InstalledPluginRecord),
		},
	}

	// Load existing registry if it exists
	if data, err := os.ReadFile(config.RegistryPath); err == nil {
		if err := json.Unmarshal(data, &m.registry); err != nil {
			return nil, &PluginError{
				Kind:    ErrJSON,
				Message: "failed to parse installed plugin registry",
				Cause:   err,
			}
		}
		if m.registry.Plugins == nil {
			m.registry.Plugins = make(map[string]InstalledPluginRecord)
		}
	}

	// Load persisted enabled state if caller didn't provide explicit overrides.
	if config.EnabledPlugins == nil {
		m.config.EnabledPlugins = make(map[string]bool)
		settingsPath := m.enabledStatePath()
		if data, err := os.ReadFile(settingsPath); err == nil {
			var saved map[string]bool
			if err := json.Unmarshal(data, &saved); err == nil {
				m.config.EnabledPlugins = saved
			}
		}
	}

	for _, opt := range opts {
		opt(m)
	}

	return m, nil
}

// DiscoverPlugins scans all configured directories for plugins.
func (m *PluginManager) DiscoverPlugins() ([]RegisteredPlugin, []LoadFailure) {
	var plugins []RegisteredPlugin
	var failures []LoadFailure

	// Discover bundled plugins
	if m.config.BundledRoot != "" {
		bp, bf := m.discoverInDir(m.config.BundledRoot, KindBundled, "bundled")
		plugins = append(plugins, bp...)
		failures = append(failures, bf...)
	}

	// Discover installed plugins
	if _, err := os.Stat(m.config.InstallRoot); err == nil {
		ip, ifl := m.discoverInDir(m.config.InstallRoot, KindExternal, "installed")
		plugins = append(plugins, ip...)
		failures = append(failures, ifl...)
	}

	// Discover external plugins from extra dirs
	for _, dir := range m.config.ExternalDirs {
		ep, ef := m.discoverInDir(dir, KindExternal, dir)
		plugins = append(plugins, ep...)
		failures = append(failures, ef...)
	}

	return plugins, failures
}

func (m *PluginManager) discoverInDir(dir string, kind PluginKind, source string) ([]RegisteredPlugin, []LoadFailure) {
	var plugins []RegisteredPlugin
	var failures []LoadFailure

	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		pluginRoot := filepath.Join(dir, entry.Name())
		manifestPath := filepath.Join(pluginRoot, "plugin.json")

		manifest, err := LoadManifest(manifestPath)
		if err != nil {
			// Fallback: try .claude-plugin/plugin.json (packaged convention)
			altPath := filepath.Join(pluginRoot, ".claude-plugin", "plugin.json")
			manifest, err = LoadManifest(altPath)
			if err != nil {
				failures = append(failures, LoadFailure{
					PluginRoot: pluginRoot,
					Kind:       kind,
					Source:     source,
					Err:        err,
				})
				continue
			}
		}

		id := pluginID(manifest.Name, kind.Marketplace())
		var enabled bool
		if e, ok := m.config.EnabledPlugins[id]; ok {
			enabled = e
		} else if kind == KindExternal {
			enabled = false // Rust: external plugins default to disabled
		} else {
			enabled = manifest.DefaultEnabled // Builtin/Bundled: use manifest default
		}

		meta := PluginMetadata{
			ID:             id,
			Name:           manifest.Name,
			Version:        manifest.Version,
			Description:    manifest.Description,
			Kind:           kind,
			Source:         source,
			DefaultEnabled: manifest.DefaultEnabled,
			Root:           pluginRoot,
		}

		var p Plugin
		switch kind {
		case KindBundled:
			p = &BundledPlugin{Meta: meta, Manifest: *manifest}
		default:
			p = &ExternalPlugin{Meta: meta, Manifest: *manifest}
		}

		plugins = append(plugins, RegisteredPlugin{Plugin: p, Enabled: enabled})
	}

	return plugins, failures
}

// Install installs a plugin from a source (local path or git URL).
// Equivalent to InstallContext(context.Background(), source).
func (m *PluginManager) Install(source string) (*InstallOutcome, error) {
	return m.InstallContext(context.Background(), source)
}

// InstallContext installs a plugin and threads ctx through any registered
// lifecycle hooks. PrePluginInstall fires before any filesystem mutation; a
// Block decision aborts the install. PostPluginInstall always fires (success
// or failure) so observers can audit the outcome.
func (m *PluginManager) InstallContext(ctx context.Context, source string) (*InstallOutcome, error) {
	if ctx == nil {
		ctx = context.Background()
	}

	if strings.HasPrefix(source, "http://") || strings.HasPrefix(source, "https://") ||
		strings.HasPrefix(source, "git@") || strings.HasSuffix(source, ".git") {
		return nil, &PluginError{
			Kind:    ErrIO,
			Message: "git-based plugin installation is not yet supported",
		}
	}

	source, err := filepath.Abs(source)
	if err != nil {
		return nil, &PluginError{Kind: ErrIO, Message: "invalid source path", Cause: err}
	}

	manifestPath := filepath.Join(source, "plugin.json")
	manifest, err := LoadManifest(manifestPath)
	if err != nil {
		return nil, err
	}

	id := pluginID(manifest.Name, externalMarketplace)
	installDir := filepath.Join(m.config.InstallRoot, sanitizePluginID(id))

	preInfo := &hooks.PluginInfo{
		ID:          id,
		Name:        manifest.Name,
		Version:     manifest.Version,
		Description: manifest.Description,
		InstallPath: installDir,
		Source:      source,
	}
	if dec, _ := m.fireHook(ctx, hooks.PrePluginInstall, preInfo); dec.Action == hooks.ActionBlock {
		return nil, &PluginError{
			Kind:    ErrCommandFailed,
			Message: fmt.Sprintf("plugin install blocked by hook: %s", dec.Reason),
		}
	}

	outcome, installErr := m.doInstall(source, manifest, id, installDir)

	postInfo := *preInfo
	postInfo.Error = installErr
	_, _ = m.fireHook(ctx, hooks.PostPluginInstall, &postInfo)

	if installErr != nil {
		return nil, installErr
	}
	return outcome, nil
}

// doInstall performs the filesystem + registry mutations for an install. It
// is split out so InstallContext can wrap it with Pre/Post hook firings and
// guarantee Post fires whether or not the install succeeds.
func (m *PluginManager) doInstall(source string, manifest *PluginManifest, id, installDir string) (*InstallOutcome, error) {
	if installDir != "" {
		_ = os.RemoveAll(installDir)
	}

	if err := copyDir(source, installDir); err != nil {
		return nil, &PluginError{
			Kind:    ErrIO,
			Message: fmt.Sprintf("failed to copy plugin to %s", installDir),
			Cause:   err,
		}
	}

	now := time.Now().UnixMilli()
	record := InstalledPluginRecord{
		Kind:          KindExternal,
		ID:            id,
		Name:          manifest.Name,
		Version:       manifest.Version,
		Description:   manifest.Description,
		InstallPath:   installDir,
		Source:        LocalPathSource(source),
		InstalledAtMs: now,
		UpdatedAtMs:   now,
	}
	m.registry.Plugins[id] = record

	if err := m.saveRegistry(); err != nil {
		return nil, err
	}

	if err := m.Enable(id); err != nil {
		return nil, err
	}

	return &InstallOutcome{
		PluginID:    id,
		Version:     manifest.Version,
		InstallPath: installDir,
	}, nil
}

// Uninstall removes an installed plugin.
// Equivalent to UninstallContext(context.Background(), pluginID).
func (m *PluginManager) Uninstall(pluginID string) error {
	return m.UninstallContext(context.Background(), pluginID)
}

// UninstallContext removes an installed plugin and threads ctx through any
// registered lifecycle hooks. A PrePluginUninstall Block decision aborts the
// uninstall (the plugin remains installed). PostPluginUninstall always fires.
func (m *PluginManager) UninstallContext(ctx context.Context, pluginID string) error {
	if ctx == nil {
		ctx = context.Background()
	}

	record, ok := m.registry.Plugins[pluginID]
	if !ok {
		return &PluginError{
			Kind:    ErrNotFound,
			Message: fmt.Sprintf("plugin %q is not installed", pluginID),
		}
	}

	preInfo := &hooks.PluginInfo{
		ID:          record.ID,
		Name:        record.Name,
		Version:     record.Version,
		Description: record.Description,
		InstallPath: record.InstallPath,
		Source:      record.Source.SourcePath(),
	}
	if dec, _ := m.fireHook(ctx, hooks.PrePluginUninstall, preInfo); dec.Action == hooks.ActionBlock {
		return &PluginError{
			Kind:    ErrCommandFailed,
			Message: fmt.Sprintf("plugin uninstall blocked by hook: %s", dec.Reason),
		}
	}

	uninstallErr := m.doUninstall(record, pluginID)

	postInfo := *preInfo
	postInfo.Error = uninstallErr
	_, _ = m.fireHook(ctx, hooks.PostPluginUninstall, &postInfo)

	return uninstallErr
}

func (m *PluginManager) doUninstall(record InstalledPluginRecord, pluginID string) error {
	if err := os.RemoveAll(record.InstallPath); err != nil {
		return &PluginError{
			Kind:    ErrIO,
			Message: fmt.Sprintf("failed to remove plugin directory: %s", record.InstallPath),
			Cause:   err,
		}
	}

	delete(m.registry.Plugins, pluginID)
	return m.saveRegistry()
}

// fireHook is a nil-safe wrapper around hooks.Runner.Fire. When no Runner is
// installed it returns ActionContinue without invoking anything.
func (m *PluginManager) fireHook(ctx context.Context, event hooks.Event, info *hooks.PluginInfo) (hooks.Decision, error) {
	if m == nil || m.hooks == nil {
		return hooks.Decision{Action: hooks.ActionContinue}, nil
	}
	return m.hooks.Fire(ctx, hooks.Context{Event: event, Plugin: info})
}

// Enable marks a plugin as enabled and persists the state.
func (m *PluginManager) Enable(pluginID string) error {
	if m.config.EnabledPlugins == nil {
		m.config.EnabledPlugins = make(map[string]bool)
	}
	m.config.EnabledPlugins[pluginID] = true
	return m.saveEnabledState()
}

// Disable marks a plugin as disabled and persists the state.
func (m *PluginManager) Disable(pluginID string) error {
	if m.config.EnabledPlugins == nil {
		m.config.EnabledPlugins = make(map[string]bool)
	}
	m.config.EnabledPlugins[pluginID] = false
	return m.saveEnabledState()
}

// Update re-reads a plugin's manifest from its original source, copies updated
// files, and refreshes the registry record.
func (m *PluginManager) Update(pluginID string) (*UpdateOutcome, error) {
	record, ok := m.registry.Plugins[pluginID]
	if !ok {
		return nil, &PluginError{
			Kind:    ErrNotFound,
			Message: fmt.Sprintf("plugin %q is not installed", pluginID),
		}
	}

	// Re-read manifest from the original source.
	sourcePath := record.Source.SourcePath()
	if sourcePath == "" {
		return nil, &PluginError{
			Kind:    ErrIO,
			Message: fmt.Sprintf("plugin %q has a non-local source; update from git is not yet supported", pluginID),
		}
	}
	manifestPath := filepath.Join(sourcePath, "plugin.json")
	manifest, err := LoadManifest(manifestPath)
	if err != nil {
		return nil, err
	}

	oldVersion := record.Version

	// Replace the installed files.
	if err := os.RemoveAll(record.InstallPath); err != nil {
		return nil, &PluginError{
			Kind:    ErrIO,
			Message: fmt.Sprintf("failed to clean install path: %s", record.InstallPath),
			Cause:   err,
		}
	}
	if err := copyDir(sourcePath, record.InstallPath); err != nil {
		return nil, &PluginError{
			Kind:    ErrIO,
			Message: fmt.Sprintf("failed to copy updated plugin to %s", record.InstallPath),
			Cause:   err,
		}
	}

	// Update the registry record.
	record.Version = manifest.Version
	record.Description = manifest.Description
	record.Name = manifest.Name
	record.UpdatedAtMs = time.Now().UnixMilli()
	m.registry.Plugins[pluginID] = record

	if err := m.saveRegistry(); err != nil {
		return nil, err
	}

	return &UpdateOutcome{
		PluginID:    pluginID,
		OldVersion:  oldVersion,
		NewVersion:  manifest.Version,
		InstallPath: record.InstallPath,
	}, nil
}

// SyncBundledPlugins auto-installs new bundled plugins, updates version-changed
// ones, and prunes bundled entries that no longer exist in BundledRoot.
func (m *PluginManager) SyncBundledPlugins() error {
	if m.config.BundledRoot == "" {
		return nil
	}

	entries, err := os.ReadDir(m.config.BundledRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return &PluginError{Kind: ErrIO, Message: "failed to read bundled root", Cause: err}
	}

	// Track which bundled plugin IDs we find on disk.
	foundBundled := make(map[string]bool)

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		pluginDir := filepath.Join(m.config.BundledRoot, entry.Name())
		manifestPath := filepath.Join(pluginDir, "plugin.json")

		manifest, err := LoadManifest(manifestPath)
		if err != nil {
			continue // skip invalid bundled plugins
		}

		id := pluginID(manifest.Name, KindBundled.Marketplace())
		foundBundled[id] = true

		existing, exists := m.registry.Plugins[id]
		if !exists {
			// New bundled plugin — add to registry.
			now := time.Now().UnixMilli()
			m.registry.Plugins[id] = InstalledPluginRecord{
				Kind:          KindBundled,
				ID:            id,
				Name:          manifest.Name,
				Version:       manifest.Version,
				Description:   manifest.Description,
				InstallPath:   pluginDir,
				Source:        LocalPathSource(pluginDir),
				InstalledAtMs: now,
				UpdatedAtMs:   now,
			}
		} else if existing.Version != manifest.Version {
			// Version changed — update record.
			existing.Version = manifest.Version
			existing.Description = manifest.Description
			existing.UpdatedAtMs = time.Now().UnixMilli()
			m.registry.Plugins[id] = existing
		}
	}

	// Prune bundled entries that no longer exist on disk.
	for id, record := range m.registry.Plugins {
		if record.Kind == KindBundled && !foundBundled[id] {
			delete(m.registry.Plugins, id)
		}
	}

	return m.saveRegistry()
}

func (m *PluginManager) enabledStatePath() string {
	return filepath.Join(m.config.ConfigHome, "plugins", "settings.json")
}

func (m *PluginManager) saveEnabledState() error {
	p := m.enabledStatePath()
	dir := filepath.Dir(p)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return &PluginError{Kind: ErrIO, Message: "failed to create settings directory", Cause: err}
	}
	data, err := json.MarshalIndent(m.config.EnabledPlugins, "", "  ")
	if err != nil {
		return &PluginError{Kind: ErrJSON, Message: "failed to marshal enabled state", Cause: err}
	}
	return os.WriteFile(p, data, 0o644)
}

func (m *PluginManager) saveRegistry() error {
	dir := filepath.Dir(m.config.RegistryPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return &PluginError{Kind: ErrIO, Message: "failed to create registry directory", Cause: err}
	}

	data, err := json.MarshalIndent(m.registry, "", "  ")
	if err != nil {
		return &PluginError{Kind: ErrJSON, Message: "failed to marshal registry", Cause: err}
	}

	return os.WriteFile(m.config.RegistryPath, data, 0o644)
}

func copyDir(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)

		if info.IsDir() {
			return os.MkdirAll(target, info.Mode())
		}

		return copyFile(path, target)
	})
}

const externalMarketplace = "external"

// pluginID constructs a plugin ID in the format "name@marketplace".
func pluginID(name, marketplace string) string {
	return name + "@" + marketplace
}

// sanitizePluginID replaces filesystem-unsafe characters with hyphens,
// matching Rust sanitize_plugin_id().
func sanitizePluginID(id string) string {
	var b strings.Builder
	b.Grow(len(id))
	for _, ch := range id {
		switch ch {
		case '/', '\\', '@', ':':
			b.WriteByte('-')
		default:
			b.WriteRune(ch)
		}
	}
	return b.String()
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()

	_, err = io.Copy(out, in)
	return err
}
