package plugins

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"time"
)

// Manager glues a Marketplace + Installer + on-disk state file. It is
// the single object slash commands talk to.
type Manager struct {
	Marketplace *Marketplace
	Installer   *Installer
	StatePath   string

	mu sync.Mutex
}

// InstalledPlugin is one row of the on-disk state. We persist enough
// metadata to render a useful list / search output without re-fetching
// the catalog every time.
type InstalledPlugin struct {
	Name        string    `json:"name"`
	Version     string    `json:"version"`
	Description string    `json:"description,omitempty"`
	InstalledAt time.Time `json:"installed_at"`
	SHA256      string    `json:"sha256"`
}

type stateFile struct {
	Version  int                `json:"version"`
	Plugins  []InstalledPlugin  `json:"plugins"`
}

const stateVersion = 1

// DefaultPluginDir returns the conventional plugins root.
//
//	$XDG_DATA_HOME/claw/plugins (Linux/BSD)
//	~/Library/Application Support/claw/plugins (Darwin)
//	%AppData%/claw/plugins (Windows)
//	~/.local/share/claw/plugins (fallback)
func DefaultPluginDir() (string, error) {
	if xdg := os.Getenv("XDG_DATA_HOME"); xdg != "" {
		return filepath.Join(xdg, "claw", "plugins"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("plugins: cannot determine home: %w", err)
	}
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "claw", "plugins"), nil
	case "windows":
		return filepath.Join(home, "AppData", "Roaming", "claw", "plugins"), nil
	}
	return filepath.Join(home, ".local", "share", "claw", "plugins"), nil
}

// NewManager constructs a manager bound to the given plugin directory.
// The state file lives at <pluginDir>/state.json.
//
// Environment-driven signature configuration is applied here so the
// /store slash command and other call sites do not need to thread a
// flag through every layer:
//
//   - CLAW_REQUIRE_SIGNED=1 — forces signature verification on
//     every install; entries without signature fields are rejected.
//   - CLAW_PLUGIN_PUBLIC_KEY — path to a PEM-encoded public key used
//     for key-based verification when an entry has no certificate
//     fields.
//
// When either env var is set, a default CosignVerifier is wired into
// the installer. Programmatic users can replace it after construction
// (m.Installer.Verifier = ...).
func NewManager(pluginDir, marketplaceURL string) *Manager {
	inst := NewInstaller(pluginDir)
	if os.Getenv("CLAW_REQUIRE_SIGNED") == "1" {
		inst.RequireSigned = true
	}
	if inst.Verifier == nil {
		inst.Verifier = &CosignVerifier{
			PublicKeyFile: os.Getenv("CLAW_PLUGIN_PUBLIC_KEY"),
		}
	}
	return &Manager{
		Marketplace: New(marketplaceURL),
		Installer:   inst,
		StatePath:   filepath.Join(pluginDir, "state.json"),
	}
}

// Install resolves name in the marketplace, runs Installer.Install, and
// records the plugin in state.json. Re-installing replaces the existing
// state row.
func (m *Manager) Install(ctx context.Context, name string) (InstalledPlugin, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.Marketplace == nil || m.Installer == nil {
		return InstalledPlugin{}, errors.New("plugin manager: not configured (Marketplace/Installer nil)")
	}

	entry, ok, err := m.Marketplace.Get(ctx, name)
	if err != nil {
		return InstalledPlugin{}, err
	}
	if !ok {
		return InstalledPlugin{}, fmt.Errorf("plugin %q not in marketplace", name)
	}
	if err := m.Installer.Install(ctx, entry); err != nil {
		return InstalledPlugin{}, err
	}

	state, err := m.readStateLocked()
	if err != nil {
		return InstalledPlugin{}, err
	}
	row := InstalledPlugin{
		Name:        entry.Name,
		Version:     entry.Version,
		Description: entry.Description,
		InstalledAt: time.Now().UTC(),
		SHA256:      entry.SHA256,
	}
	state.Plugins = upsert(state.Plugins, row)
	if err := m.writeStateLocked(state); err != nil {
		return InstalledPlugin{}, err
	}
	return row, nil
}

// Uninstall removes the plugin from disk and the state file. Missing
// plugins are not an error so the slash command can be re-run safely.
func (m *Manager) Uninstall(ctx context.Context, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Installer == nil {
		return errors.New("plugin manager: not configured")
	}
	if err := m.Installer.Uninstall(ctx, name); err != nil {
		return err
	}
	state, err := m.readStateLocked()
	if err != nil {
		return err
	}
	state.Plugins = remove(state.Plugins, name)
	return m.writeStateLocked(state)
}

// List returns the installed plugins sorted by name. The list comes
// from state.json; we do not stat the plugin directory because the
// authoritative answer to "what is installed" is the state file (a
// half-extracted directory after a crash should not appear as
// installed).
func (m *Manager) List() ([]InstalledPlugin, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	state, err := m.readStateLocked()
	if err != nil {
		return nil, err
	}
	out := make([]InstalledPlugin, len(state.Plugins))
	copy(out, state.Plugins)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Search delegates to the marketplace, ignoring local install state.
func (m *Manager) Search(ctx context.Context, query string) ([]PluginEntry, error) {
	if m.Marketplace == nil {
		return nil, errors.New("plugin manager: marketplace not configured")
	}
	return m.Marketplace.Search(ctx, query)
}

// readStateLocked loads state.json, returning an empty stateFile when
// the file is missing. Caller must hold m.mu.
func (m *Manager) readStateLocked() (stateFile, error) {
	data, err := os.ReadFile(m.StatePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return stateFile{Version: stateVersion}, nil
		}
		return stateFile{}, fmt.Errorf("plugin manager: read state: %w", err)
	}
	var s stateFile
	if err := json.Unmarshal(data, &s); err != nil {
		return stateFile{}, fmt.Errorf("plugin manager: decode state: %w", err)
	}
	return s, nil
}

// writeStateLocked atomically persists state. Caller must hold m.mu.
func (m *Manager) writeStateLocked(s stateFile) error {
	s.Version = stateVersion
	if err := os.MkdirAll(filepath.Dir(m.StatePath), 0o755); err != nil {
		return fmt.Errorf("plugin manager: mkdir state: %w", err)
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("plugin manager: marshal state: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(m.StatePath), ".state.*.tmp")
	if err != nil {
		return fmt.Errorf("plugin manager: create temp: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("plugin manager: write temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("plugin manager: close temp: %w", err)
	}
	if err := os.Rename(tmpName, m.StatePath); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("plugin manager: rename: %w", err)
	}
	return nil
}

func upsert(rows []InstalledPlugin, row InstalledPlugin) []InstalledPlugin {
	for i, r := range rows {
		if r.Name == row.Name {
			rows[i] = row
			return rows
		}
	}
	return append(rows, row)
}

func remove(rows []InstalledPlugin, name string) []InstalledPlugin {
	out := rows[:0]
	for _, r := range rows {
		if r.Name == name {
			continue
		}
		out = append(out, r)
	}
	return out
}
