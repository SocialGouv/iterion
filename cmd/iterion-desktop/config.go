package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// configSchemaVersion is bumped whenever the on-disk Config shape changes
// in a non-additive way. Migrations live in migrateConfig().
const configSchemaVersion = 1

// recentProjectsCap bounds the MRU list — beyond this we drop the oldest.
const recentProjectsCap = 20

// ChannelStable / ChannelPrerelease are the two release channels the
// updater understands.
const (
	ChannelStable     = "stable"
	ChannelPrerelease = "prerelease"
)

// Config is the on-disk shape of the desktop app's user preferences.
// Stored at <UserConfigDir>/Iterion/config.json. Atomic writes via
// tempfile + rename. All public Read/Write APIs are mutex-guarded.
type Config struct {
	Version          int            `json:"version"`
	RecentProjects   []Project      `json:"recent_projects"`
	CurrentProjectID string         `json:"current_project_id"`
	Window           WindowState    `json:"window"`
	Updater          UpdaterPrefs   `json:"updater"`
	Telemetry        TelemetryPrefs `json:"telemetry"`
	FirstRunDone     bool           `json:"first_run_done"`

	// path is the resolved on-disk location. Not serialised.
	path string `json:"-"`
	// mu guards every mutation + the in-memory copy that's about to be
	// serialised. Acquired by Save().
	mu sync.Mutex `json:"-"`
}

// Project is a registered iterion project that the user has interacted
// with. Stored in the MRU list and selected via CurrentProjectID.
type Project struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	Dir        string    `json:"dir"`
	StoreDir   string    `json:"store_dir,omitempty"`
	LastOpened time.Time `json:"last_opened"`
	Color      string    `json:"color,omitempty"`
}

// WindowState is the persisted geometry restored on next startup.
type WindowState struct {
	Width      int  `json:"width"`
	Height     int  `json:"height"`
	X          int  `json:"x"`
	Y          int  `json:"y"`
	Maximised  bool `json:"maximised"`
	Fullscreen bool `json:"fullscreen"`
}

// UpdaterPrefs controls auto-update behaviour.
type UpdaterPrefs struct {
	Channel       string    `json:"channel"`    // stable / prerelease
	AutoCheck     bool      `json:"auto_check"` // default true
	LastCheckedAt time.Time `json:"last_checked_at"`
	LastAppliedAt time.Time `json:"last_applied_at"`
	LastSeenVer   string    `json:"last_seen_version"`
}

// TelemetryPrefs is reserved for v2; default disabled.
type TelemetryPrefs struct {
	Enabled bool `json:"enabled"`
}

// NewConfig returns a Config with v1 defaults. Callers wanting persistence
// should prefer LoadConfig.
func NewConfig() *Config {
	return &Config{
		Version: configSchemaVersion,
		Window:  WindowState{Width: 1400, Height: 900},
		Updater: UpdaterPrefs{Channel: ChannelStable, AutoCheck: true},
	}
}

// configPath returns the canonical on-disk location.
func configPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("user config dir: %w", err)
	}
	d := filepath.Join(dir, "Iterion")
	if err := os.MkdirAll(d, 0o755); err != nil {
		return "", fmt.Errorf("mkdir %q: %w", d, err)
	}
	return filepath.Join(d, "config.json"), nil
}

// LoadConfig reads (or initialises) the config file. Missing file → fresh
// Config; corrupt JSON → return an error so the caller can decide whether
// to overwrite (the desktop App falls back to NewConfig() and logs).
func LoadConfig() (*Config, error) {
	path, err := configPath()
	if err != nil {
		return nil, err
	}
	return loadConfigFrom(path)
}

func loadConfigFrom(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			c := NewConfig()
			c.path = path
			return c, nil
		}
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	c.path = path
	migrateConfig(&c)
	return &c, nil
}

// migrateConfig brings older on-disk shapes up to the current
// configSchemaVersion. Each step is idempotent; new versions append a new
// case. v0 (zero-value Version field) is treated as "fresh from an older
// build that didn't write the field".
func migrateConfig(c *Config) {
	if c.Version == 0 {
		// v0 → v1: ensure default Window + Updater are populated when
		// loading a config written by a build that didn't have them.
		if c.Window.Width == 0 || c.Window.Height == 0 {
			c.Window.Width = 1400
			c.Window.Height = 900
		}
		if c.Updater.Channel == "" {
			c.Updater.Channel = ChannelStable
			c.Updater.AutoCheck = true
		}
		c.Version = 1
	}
}

// Save serialises and atomically writes the config. Tempfile + rename so
// a crash mid-write can never leave a half-written file.
func (c *Config) Save() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.saveLocked()
}

func (c *Config) saveLocked() error {
	if c.path == "" {
		path, err := configPath()
		if err != nil {
			return err
		}
		c.path = path
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(c.path)
	tmp, err := os.CreateTemp(dir, "config.json.tmp.*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	return os.Rename(tmpName, c.path)
}

// CurrentProject returns a pointer to the project matching CurrentProjectID,
// or nil if none. Pointer is into the slice; do not mutate without holding mu.
func (c *Config) CurrentProject() *Project {
	if c.CurrentProjectID == "" {
		return nil
	}
	for i := range c.RecentProjects {
		if c.RecentProjects[i].ID == c.CurrentProjectID {
			return &c.RecentProjects[i]
		}
	}
	return nil
}

// AddProject inserts (or refreshes) the project with the given absolute
// directory and returns its canonical struct. Updates LastOpened, MRU
// ordering, and CurrentProjectID. Caller is responsible for Save().
func (c *Config) AddProject(absDir string) Project {
	c.mu.Lock()
	defer c.mu.Unlock()
	// Look up by Dir — if already registered, refresh.
	for i := range c.RecentProjects {
		if c.RecentProjects[i].Dir == absDir {
			c.RecentProjects[i].LastOpened = time.Now().UTC()
			c.CurrentProjectID = c.RecentProjects[i].ID
			c.sortByMRU()
			return c.RecentProjects[i]
		}
	}
	p := Project{
		ID:         randomID(),
		Name:       filepath.Base(absDir),
		Dir:        absDir,
		LastOpened: time.Now().UTC(),
	}
	c.RecentProjects = append(c.RecentProjects, p)
	c.CurrentProjectID = p.ID
	c.sortByMRU()
	c.capRecents()
	return p
}

// RemoveProject drops the project with the given ID. Returns true if it
// was present. If the removed project was current, CurrentProjectID is
// cleared and the new MRU head (if any) becomes current.
func (c *Config) RemoveProject(id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	idx := -1
	for i := range c.RecentProjects {
		if c.RecentProjects[i].ID == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		return false
	}
	c.RecentProjects = append(c.RecentProjects[:idx], c.RecentProjects[idx+1:]...)
	if c.CurrentProjectID == id {
		c.CurrentProjectID = ""
		if len(c.RecentProjects) > 0 {
			c.CurrentProjectID = c.RecentProjects[0].ID
		}
	}
	return true
}

// SetCurrentProject flips CurrentProjectID. Returns true on success, false
// if the id is not in RecentProjects.
func (c *Config) SetCurrentProject(id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := range c.RecentProjects {
		if c.RecentProjects[i].ID == id {
			c.RecentProjects[i].LastOpened = time.Now().UTC()
			c.CurrentProjectID = id
			c.sortByMRU()
			return true
		}
	}
	return false
}

func (c *Config) sortByMRU() {
	sort.SliceStable(c.RecentProjects, func(i, j int) bool {
		return c.RecentProjects[i].LastOpened.After(c.RecentProjects[j].LastOpened)
	})
}

func (c *Config) capRecents() {
	if len(c.RecentProjects) > recentProjectsCap {
		c.RecentProjects = c.RecentProjects[:recentProjectsCap]
	}
}

func randomID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand failure: fall back to a timestamp-based ID. Not
		// cryptographically random but unique-enough for an MRU key.
		return fmt.Sprintf("p-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}
