package privacy

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

// vaultVersion is the on-disk schema version. A mismatch on read
// is a hard error — the operator either rolled back the binary or
// hand-edited the file.
const vaultVersion = 1

const (
	// vaultDirPerm matches the iterion store's directory mode.
	vaultDirPerm os.FileMode = 0o700
	// vaultFilePerm restricts the vault to the owning user only;
	// raw PII lives in this file.
	vaultFilePerm os.FileMode = 0o600
)

// vaultEntry is the JSON shape of one placeholder→value mapping.
type vaultEntry struct {
	Value       string `json:"value"`
	Category    string `json:"category"`
	FirstSeenAt string `json:"first_seen_at"`
}

// vaultFile is the on-disk shape of pii_vault.json.
type vaultFile struct {
	Version   int                   `json:"version"`
	RunID     string                `json:"run_id"`
	CreatedAt string                `json:"created_at"`
	Entries   map[string]vaultEntry `json:"entries"`
}

// Vault stores token→raw value mappings for a single run.
//
// All mutating operations are serialised by an internal mutex;
// concurrent calls from different goroutines on the same run are
// safe. Different runs use different files (different paths) and
// can write in parallel.
type Vault struct {
	mu    sync.Mutex
	path  string
	runID string
	data  vaultFile
}

// OpenOrCreate opens (or creates) the per-run vault at
// <storeDir>/runs/<runID>/pii_vault.json.
//
// The file is created lazily on the first Add — OpenOrCreate
// only reads existing state. A non-existent vault is treated as
// an empty one, which is what privacy_unfilter wants when called
// before privacy_filter.
//
// Returns an error if the on-disk version is not the expected
// vaultVersion; the operator must reconcile the schema before
// proceeding.
func OpenOrCreate(runID, storeDir string) (*Vault, error) {
	if storeDir == "" {
		return nil, fmt.Errorf("vault: store dir must not be empty")
	}
	if err := store.SanitizePathComponent("run ID", runID); err != nil {
		return nil, fmt.Errorf("vault: %w", err)
	}

	dir := filepath.Join(storeDir, "runs", runID)
	path := filepath.Join(dir, "pii_vault.json")

	v := &Vault{
		path:  path,
		runID: runID,
		data: vaultFile{
			Version:   vaultVersion,
			RunID:     runID,
			CreatedAt: time.Now().UTC().Format(time.RFC3339),
			Entries:   map[string]vaultEntry{},
		},
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return v, nil
		}
		return nil, fmt.Errorf("vault: read %s: %w", path, err)
	}
	var existing vaultFile
	if err := json.Unmarshal(raw, &existing); err != nil {
		return nil, fmt.Errorf("vault: decode %s: %w", path, err)
	}
	if existing.Version != vaultVersion {
		return nil, fmt.Errorf("vault: unsupported version %d (expected %d) at %s", existing.Version, vaultVersion, path)
	}
	if existing.Entries == nil {
		existing.Entries = map[string]vaultEntry{}
	}
	v.data = existing
	return v, nil
}

// Entry is a single placeholder→value mapping the caller wants to
// persist. Used by AddBatch to avoid an fsync per token in
// redact-heavy workflows.
type Entry struct {
	Token    string
	Value    string
	Category string
}

// Add inserts a token→value mapping and persists the vault.
// Subsequent calls with the same token are no-ops (the first
// FirstSeenAt is preserved) — the makeToken helper is
// deterministic, so two redact calls on the same value within a
// run will hit the same token.
func (v *Vault) Add(token, value, category string) error {
	return v.AddBatch([]Entry{{Token: token, Value: value, Category: category}})
}

// AddBatch inserts many entries and persists the vault once. A
// single fsync covers the whole batch, so a redact call with N
// detected spans incurs one disk write instead of N.
//
// Entries with empty tokens are rejected. Duplicate tokens are
// silently ignored (FirstSeenAt is preserved).
func (v *Vault) AddBatch(entries []Entry) error {
	if len(entries) == 0 {
		return nil
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	now := time.Now().UTC().Format(time.RFC3339)
	dirty := false
	for _, e := range entries {
		if e.Token == "" {
			return fmt.Errorf("vault: token must not be empty")
		}
		if _, exists := v.data.Entries[e.Token]; exists {
			continue
		}
		v.data.Entries[e.Token] = vaultEntry{
			Value:       e.Value,
			Category:    e.Category,
			FirstSeenAt: now,
		}
		dirty = true
	}
	if !dirty {
		return nil
	}
	return v.saveLocked()
}

// Get looks up a value by token. Returns ok=false when the token
// is not in the vault — callers can then apply their missing
// policy (leave / remove / error).
func (v *Vault) Get(token string) (value, category string, ok bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	e, present := v.data.Entries[token]
	if !present {
		return "", "", false
	}
	return e.Value, e.Category, true
}

// Path returns the on-disk path of the vault file. Useful for
// tests to stat the permissions or assert the file location.
func (v *Vault) Path() string { return v.path }

// Len returns the number of entries currently in the vault.
func (v *Vault) Len() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return len(v.data.Entries)
}

// saveLocked writes the vault atomically. The caller must hold
// v.mu. Delegates to store.WriteFileAtomic which uses the
// same temp+rename+fsync pattern as run.json so the algorithms
// stay in sync.
func (v *Vault) saveLocked() error {
	if err := os.MkdirAll(filepath.Dir(v.path), vaultDirPerm); err != nil {
		return fmt.Errorf("vault: mkdir: %w", err)
	}
	body, err := json.MarshalIndent(v.data, "", "  ")
	if err != nil {
		return fmt.Errorf("vault: marshal: %w", err)
	}
	if err := store.WriteFileAtomic(v.path, body, vaultFilePerm); err != nil {
		return fmt.Errorf("vault: %w", err)
	}
	// Defence in depth: ensure the final file mode is restrictive
	// even if the OS umask widened it during create.
	_ = os.Chmod(v.path, vaultFilePerm)
	return nil
}
