package sessionboard

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/SocialGouv/iterion/pkg/store"
)

// Store loads and saves the per-run board spec. The interface keeps the
// coordinator and HTTP handlers decoupled from the on-disk layout (and
// lets a cloud backend swap in a Mongo-backed impl later, mirroring how
// the board store is abstracted).
type Store interface {
	// Load returns the run's spec, or a zero-value Spec (Version 0, no
	// widgets) when none has been persisted yet — never an error for
	// "not found".
	Load(runID string) (Spec, error)
	// Save persists the run's spec, overwriting any prior version.
	Save(runID string, spec Spec) error
}

// FileStore persists specs as <baseDir>/runs/<runID>/sessionboard.json,
// alongside the run's other artifacts. Writes are atomic (temp file +
// rename) so a concurrent reader never sees a half-written spec.
type FileStore struct {
	baseDir string
}

// NewFileStore builds a FileStore rooted at the run store's base directory
// (the same dir that holds runs/<id>/run.json).
func NewFileStore(baseDir string) (*FileStore, error) {
	if baseDir == "" {
		return nil, errors.New("sessionboard: base dir is required")
	}
	return &FileStore{baseDir: baseDir}, nil
}

func (s *FileStore) specPath(runID string) string {
	return filepath.Join(s.baseDir, "runs", runID, "sessionboard.json")
}

// Load implements Store.
func (s *FileStore) Load(runID string) (Spec, error) {
	if runID == "" {
		return Spec{}, errors.New("sessionboard: run_id is required")
	}
	data, err := os.ReadFile(s.specPath(runID))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Spec{}, nil
		}
		return Spec{}, fmt.Errorf("sessionboard: read spec: %w", err)
	}
	var spec Spec
	if err := json.Unmarshal(data, &spec); err != nil {
		return Spec{}, fmt.Errorf("sessionboard: decode spec: %w", err)
	}
	return spec, nil
}

// Save implements Store.
func (s *FileStore) Save(runID string, spec Spec) error {
	if runID == "" {
		return errors.New("sessionboard: run_id is required")
	}
	path := s.specPath(runID)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("sessionboard: mkdir: %w", err)
	}
	data, err := json.MarshalIndent(spec, "", "  ")
	if err != nil {
		return fmt.Errorf("sessionboard: encode spec: %w", err)
	}
	if err := store.WriteFileAtomic(path, data, 0o644); err != nil {
		return fmt.Errorf("sessionboard: write spec: %w", err)
	}
	return nil
}

// propsEqual reports whether two widget prop maps are structurally equal.
// JSON round-trip comparison keeps it simple and order-insensitive for
// maps; widget props are small so the cost is negligible.
func propsEqual(a, b map[string]any) bool {
	if len(a) != len(b) {
		return false
	}
	if len(a) == 0 {
		return true
	}
	ab, err1 := json.Marshal(a)
	bb, err2 := json.Marshal(b)
	if err1 != nil || err2 != nil {
		return false
	}
	return bytes.Equal(ab, bb)
}
