package mcp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"

	"github.com/SocialGouv/iterion/tool"
)

// SchemaChange describes a detected change in an MCP tool's input schema.
type SchemaChange struct {
	QualifiedName       string
	Server              string
	ToolName            string
	PreviousFingerprint string
	CurrentFingerprint  string
	IsNew               bool
}

// FingerprintStore persists and compares schema fingerprints across runs.
type FingerprintStore struct {
	mu   sync.Mutex
	dir  string
	data map[string]string // qualifiedName -> hex fingerprint
}

// NewFingerprintStore loads (or creates) a fingerprint store.
// Stored at dir/mcp-cache/schema-fingerprints.json
func NewFingerprintStore(dir string) *FingerprintStore {
	fs := &FingerprintStore{
		dir:  dir,
		data: make(map[string]string),
	}
	path := fs.filePath()
	raw, err := os.ReadFile(path)
	if err != nil {
		return fs
	}
	var loaded map[string]string
	if err := json.Unmarshal(raw, &loaded); err != nil {
		return fs
	}
	fs.data = loaded
	return fs
}

// Check compares current schema against stored fingerprint.
// Returns SchemaChange if different/new, nil if unchanged.
// Updates in-memory state but does NOT persist — call Save().
func (fs *FingerprintStore) Check(qualifiedName, server, toolName string, schema json.RawMessage) *SchemaChange {
	current := tool.SchemaFingerprint(schema)
	if current == "" {
		return nil
	}

	fs.mu.Lock()
	defer fs.mu.Unlock()

	prev, exists := fs.data[qualifiedName]
	fs.data[qualifiedName] = current

	if !exists {
		return &SchemaChange{
			QualifiedName:      qualifiedName,
			Server:             server,
			ToolName:           toolName,
			CurrentFingerprint: current,
			IsNew:              true,
		}
	}
	if prev == current {
		return nil
	}
	return &SchemaChange{
		QualifiedName:       qualifiedName,
		Server:              server,
		ToolName:            toolName,
		PreviousFingerprint: prev,
		CurrentFingerprint:  current,
	}
}

// Save persists fingerprints to disk.
func (fs *FingerprintStore) Save() error {
	fs.mu.Lock()
	snapshot := make(map[string]string, len(fs.data))
	for k, v := range fs.data {
		snapshot[k] = v
	}
	fs.mu.Unlock()

	path := fs.filePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	raw, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0o644)
}

func (fs *FingerprintStore) filePath() string {
	return filepath.Join(fs.dir, "mcp-cache", "schema-fingerprints.json")
}

// WithFingerprintStore sets the fingerprint store on the manager.
func WithFingerprintStore(fs *FingerprintStore) ManagerOption {
	return func(m *Manager) { m.fingerprints = fs }
}
