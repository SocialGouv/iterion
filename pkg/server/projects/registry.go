// Package projects manages the editor's per-user project registry.
//
// On disk at <UserConfigDir>/Iterion/config.json — shared with the
// desktop (Wails) app's own config (cmd/iterion-desktop/config.go).
// We only own the `version`, `recent_projects` and
// `current_project_id` keys; every other top-level key (Window,
// Updater, Telemetry, FirstRunDone, …) is round-tripped opaquely so
// the server and desktop can read/write the same file without
// trampling each other's settings.
//
// Schema v1 mirrors cmd/iterion-desktop/config.go so a project added
// in either app shows up in the other.
package projects

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

	"github.com/SocialGouv/iterion/pkg/store"
)

const (
	schemaVersion = 1
	recentCap     = 20
)

// Project is the registered iterion project. Field tags match
// cmd/iterion-desktop/config.go:Project exactly.
type Project struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	Dir        string    `json:"dir"`
	StoreDir   string    `json:"store_dir,omitempty"`
	LastOpened time.Time `json:"last_opened"`
	Color      string    `json:"color,omitempty"`
}

// Config is the subset of <UserConfigDir>/Iterion/config.json that the
// server reads and writes. Extras preserves every other top-level key
// untouched across save cycles so writing this struct to disk doesn't
// drop settings owned by the desktop app.
type Config struct {
	Version          int       `json:"version"`
	RecentProjects   []Project `json:"recent_projects"`
	CurrentProjectID string    `json:"current_project_id"`

	// Extras holds the JSON-encoded value of every other top-level key
	// observed at load time. Marshalled back verbatim on Save.
	Extras map[string]json.RawMessage `json:"-"`

	// path is the resolved on-disk location (set by Load).
	path string
}

var (
	// pathMu protects the resolved on-disk path cache below.
	pathMu     sync.Mutex
	cachedPath string
)

// Path returns the canonical on-disk location, creating the parent
// directory if missing. Mirrors cmd/iterion-desktop/config.go:configPath
// so both apps target the same file.
func Path() (string, error) {
	pathMu.Lock()
	defer pathMu.Unlock()
	if cachedPath != "" {
		return cachedPath, nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("user config dir: %w", err)
	}
	d := filepath.Join(dir, "Iterion")
	if err := os.MkdirAll(d, 0o755); err != nil {
		return "", fmt.Errorf("mkdir %q: %w", d, err)
	}
	cachedPath = filepath.Join(d, "config.json")
	return cachedPath, nil
}

// Load reads the registry. Missing file → fresh Config; corrupt JSON →
// returns the error so the caller can decide whether to overwrite
// (callers typically log + start fresh to avoid wedging the editor).
func Load() (*Config, error) {
	path, err := Path()
	if err != nil {
		return nil, err
	}
	return loadFrom(path)
}

func loadFrom(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &Config{Version: schemaVersion, path: path}, nil
		}
		return nil, err
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	cfg := &Config{path: path, Extras: map[string]json.RawMessage{}}
	for k, v := range raw {
		switch k {
		case "version":
			_ = json.Unmarshal(v, &cfg.Version)
		case "recent_projects":
			_ = json.Unmarshal(v, &cfg.RecentProjects)
		case "current_project_id":
			_ = json.Unmarshal(v, &cfg.CurrentProjectID)
		default:
			cfg.Extras[k] = v
		}
	}
	if cfg.Version == 0 {
		cfg.Version = schemaVersion
	}
	return cfg, nil
}

// Save atomically writes the registry, preserving every Extras key
// untouched so desktop-owned settings (Window/Updater/Telemetry/…)
// survive the round-trip.
func (c *Config) Save() error {
	if c.path == "" {
		p, err := Path()
		if err != nil {
			return err
		}
		c.path = p
	}
	out := map[string]json.RawMessage{}
	for k, v := range c.Extras {
		out[k] = v
	}
	for key, val := range map[string]any{
		"version":            c.Version,
		"recent_projects":    c.RecentProjects,
		"current_project_id": c.CurrentProjectID,
	} {
		raw, err := json.Marshal(val)
		if err != nil {
			return fmt.Errorf("marshal %s: %w", key, err)
		}
		out[key] = raw
	}
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	return store.WriteFileAtomic(c.path, data, 0o600)
}

// AddOrTouch returns the canonical Project for the given absolute
// directory. If already registered, refreshes LastOpened and flips
// CurrentProjectID; otherwise inserts a new entry (capped at 20 MRU).
// Callers must Save() to persist.
func (c *Config) AddOrTouch(absDir string) Project {
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

// ByID looks up a project by id; returns a copy or nil.
func (c *Config) ByID(id string) *Project {
	for i := range c.RecentProjects {
		if c.RecentProjects[i].ID == id {
			cp := c.RecentProjects[i]
			return &cp
		}
	}
	return nil
}

// Current returns the active project (matching CurrentProjectID) or
// nil when none is selected.
func (c *Config) Current() *Project {
	if c.CurrentProjectID == "" {
		return nil
	}
	return c.ByID(c.CurrentProjectID)
}

// SetCurrent flips CurrentProjectID. Returns true if the id matches an
// existing entry; bumps its LastOpened so the MRU sort surfaces it.
func (c *Config) SetCurrent(id string) bool {
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

// Remove drops the project with the given id. Returns true if it was
// present. If the removed project was current, the new MRU head (if
// any) becomes current; otherwise CurrentProjectID is cleared.
func (c *Config) Remove(id string) bool {
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

func (c *Config) sortByMRU() {
	sort.SliceStable(c.RecentProjects, func(i, j int) bool {
		return c.RecentProjects[i].LastOpened.After(c.RecentProjects[j].LastOpened)
	})
}

func (c *Config) capRecents() {
	if len(c.RecentProjects) > recentCap {
		c.RecentProjects = c.RecentProjects[:recentCap]
	}
}

func randomID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("p-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}
