package mcp

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	// DefaultCacheTTL is the default time-to-live for cached tool discovery.
	DefaultCacheTTL = 1 * time.Hour
	// EnvCacheTTL controls the cache TTL. Set to "0" to disable caching.
	EnvCacheTTL = "ITERION_MCP_CACHE_TTL"
)

// ToolCache provides persistent caching of MCP tool discovery results.
// Cache files are stored as JSON in a subdirectory, keyed by server name
// and a hash of the server configuration.
type ToolCache struct {
	dir string
	ttl time.Duration
}

type cachedTools struct {
	Tools    []ToolInfo `json:"tools"`
	CachedAt time.Time  `json:"cached_at"`
}

// NewToolCache creates a cache that stores tool schemas under dir/mcp-cache/.
func NewToolCache(dir string, ttl time.Duration) *ToolCache {
	return &ToolCache{dir: dir, ttl: ttl}
}

// WithToolCache sets the tool discovery cache on the manager.
func WithToolCache(cache *ToolCache) ManagerOption {
	return func(m *Manager) { m.cache = cache }
}

// Get returns cached tool info for the given server if available and fresh.
// The config hash is encoded in the filename, so a config change automatically
// maps to a different (nonexistent) cache file.
func (c *ToolCache) Get(serverName string, cfg *ServerConfig) ([]ToolInfo, bool) {
	path := c.cacheFile(serverName, cfg)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	var cached cachedTools
	if err := json.Unmarshal(data, &cached); err != nil {
		return nil, false
	}
	if time.Since(cached.CachedAt) > c.ttl {
		return nil, false
	}
	return cached.Tools, true
}

// Set persists tool info for the given server.
func (c *ToolCache) Set(serverName string, cfg *ServerConfig, tools []ToolInfo) error {
	path := c.cacheFile(serverName, cfg)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	cached := cachedTools{
		Tools:    tools,
		CachedAt: time.Now(),
	}
	data, err := json.Marshal(cached)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func (c *ToolCache) cacheFile(serverName string, cfg *ServerConfig) string {
	hash := configHash(cfg)
	return filepath.Join(c.dir, "mcp-cache", fmt.Sprintf("%s-%s.json", serverName, hash))
}

// configHash produces a deterministic short hash of a ServerConfig.
func configHash(cfg *ServerConfig) string {
	h := sha256.New()
	data, _ := json.Marshal(cfg)
	h.Write(data)
	return fmt.Sprintf("%x", h.Sum(nil))[:16]
}

// ResolveCacheTTL reads the cache TTL from environment. Returns 0 to disable.
func ResolveCacheTTL() time.Duration {
	val := strings.TrimSpace(os.Getenv(EnvCacheTTL))
	if val == "0" || strings.EqualFold(val, "false") {
		return 0
	}
	if val != "" {
		if d, err := time.ParseDuration(val); err == nil {
			return d
		}
	}
	return DefaultCacheTTL
}
