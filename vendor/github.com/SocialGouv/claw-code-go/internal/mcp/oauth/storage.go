// Package oauth implements an OAuth 2.0 Authorization Code + PKCE broker for
// authenticated remote MCP servers, with an on-disk token cache.
//
// Storage is a simple JSON map keyed by server name. Writes are atomic
// (temp file + rename) and the file mode is 0600. Encryption at rest is
// out of scope for now; future work could plug a keyring or libsecret
// backend behind the same interface.
package oauth

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"
)

// Token is the persisted form of an OAuth token for a single MCP server.
//
// AccessToken is the bearer token used in Authorization headers.
// RefreshToken is optional and, when present, allows silent renewal.
// ExpiresAt is the absolute UTC time the access token stops being valid.
// Scope is the space-separated list returned by the server (informational).
type Token struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	TokenType    string    `json:"token_type,omitempty"`
	ExpiresAt    time.Time `json:"expires_at,omitempty"`
	Scope        string    `json:"scope,omitempty"`
}

// IsExpired reports whether the token will expire within the given skew.
// A zero ExpiresAt is treated as "never expires" (some auth servers omit
// expires_in for long-lived tokens).
func (t Token) IsExpired(skew time.Duration) bool {
	if t.ExpiresAt.IsZero() {
		return false
	}
	return time.Now().Add(skew).After(t.ExpiresAt)
}

// storageFile is the on-disk JSON layout. The version field is included so
// that future schema changes can be detected without surprising users.
type storageFile struct {
	Version int              `json:"version"`
	Tokens  map[string]Token `json:"tokens"`
}

const storageVersion = 1

// Storage is a thread-safe, file-backed token cache.
type Storage struct {
	path string
	mu   sync.Mutex
}

// NewStorage creates a Storage rooted at path. The file and parent directory
// are created lazily on first write.
func NewStorage(path string) *Storage {
	return &Storage{path: path}
}

// DefaultStoragePath returns the platform-conventional cache path.
//
//	$XDG_CONFIG_HOME/claw/mcp-tokens.json   (Linux/BSD)
//	~/Library/Application Support/claw/mcp-tokens.json   (Darwin)
//	%AppData%/claw/mcp-tokens.json   (Windows)
//	~/.config/claw/mcp-tokens.json   (fallback)
func DefaultStoragePath() (string, error) {
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "claw", "mcp-tokens.json"), nil
	}
	if dir, err := os.UserConfigDir(); err == nil && dir != "" {
		return filepath.Join(dir, "claw", "mcp-tokens.json"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("oauth: cannot determine home directory: %w", err)
	}
	if runtime.GOOS == "windows" {
		return filepath.Join(home, "AppData", "Roaming", "claw", "mcp-tokens.json"), nil
	}
	return filepath.Join(home, ".config", "claw", "mcp-tokens.json"), nil
}

// Load returns the cached token for serverName. Returns (Token{}, false, nil)
// if no token is cached. Returns an error only on I/O or decode failures.
func (s *Storage) Load(serverName string) (Token, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	file, err := s.readLocked()
	if err != nil {
		return Token{}, false, err
	}
	tok, ok := file.Tokens[serverName]
	return tok, ok, nil
}

// Save writes (or replaces) the token for serverName. The file is created
// with mode 0600 and the parent directory with 0700 if either is missing.
func (s *Storage) Save(serverName string, tok Token) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	file, err := s.readLocked()
	if err != nil {
		return err
	}
	if file.Tokens == nil {
		file.Tokens = make(map[string]Token)
	}
	file.Tokens[serverName] = tok
	file.Version = storageVersion
	return s.writeLocked(file)
}

// Delete removes any token cached for serverName. Missing entries are not
// an error.
func (s *Storage) Delete(serverName string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	file, err := s.readLocked()
	if err != nil {
		return err
	}
	if _, ok := file.Tokens[serverName]; !ok {
		return nil
	}
	delete(file.Tokens, serverName)
	file.Version = storageVersion
	return s.writeLocked(file)
}

// readLocked reads and decodes the on-disk storage. A missing file is
// treated as an empty store. Caller must hold s.mu.
func (s *Storage) readLocked() (storageFile, error) {
	f, err := os.Open(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return storageFile{Version: storageVersion, Tokens: map[string]Token{}}, nil
		}
		return storageFile{}, fmt.Errorf("oauth storage: open %s: %w", s.path, err)
	}
	defer f.Close()

	data, err := io.ReadAll(f)
	if err != nil {
		return storageFile{}, fmt.Errorf("oauth storage: read %s: %w", s.path, err)
	}
	if len(data) == 0 {
		return storageFile{Version: storageVersion, Tokens: map[string]Token{}}, nil
	}
	var file storageFile
	if err := json.Unmarshal(data, &file); err != nil {
		return storageFile{}, fmt.Errorf("oauth storage: decode %s: %w", s.path, err)
	}
	if file.Tokens == nil {
		file.Tokens = make(map[string]Token)
	}
	return file, nil
}

// writeLocked atomically writes file by creating a temp file in the same
// directory and renaming it over the destination. Caller must hold s.mu.
func (s *Storage) writeLocked(file storageFile) error {
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("oauth storage: mkdir %s: %w", dir, err)
	}
	data, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return fmt.Errorf("oauth storage: marshal: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".mcp-tokens.*.tmp")
	if err != nil {
		return fmt.Errorf("oauth storage: create temp: %w", err)
	}
	tmpPath := tmp.Name()
	// Ensure 0600 mode regardless of umask, even before any data is written.
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("oauth storage: chmod temp: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("oauth storage: write temp: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("oauth storage: fsync temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("oauth storage: close temp: %w", err)
	}
	if err := os.Rename(tmpPath, s.path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("oauth storage: rename %s -> %s: %w", tmpPath, s.path, err)
	}
	// Belt-and-braces: ensure final file has 0600 perms. Some platforms (Windows
	// in particular) do not honour permissions through Chmod, so this is best
	// effort and any error is non-fatal there. We still propagate Unix errors.
	if runtime.GOOS != "windows" {
		if err := os.Chmod(s.path, 0o600); err != nil {
			return fmt.Errorf("oauth storage: chmod %s: %w", s.path, err)
		}
	}
	return nil
}
