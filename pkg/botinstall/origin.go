package botinstall

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const originsDirName = ".origins"

// Origin is the durable source descriptor stored beside an installed bundle.
// It intentionally lives outside the copied bundle so provenance metadata
// does not alter the bundle's logical content hash.
type Origin struct {
	Source      string    `json:"source"`
	Ref         string    `json:"ref,omitempty"`
	SourcePath  string    `json:"source_path,omitempty"`
	InstalledAt time.Time `json:"installed_at"`
}

func originPath(installedPath string) string {
	return filepath.Join(filepath.Dir(installedPath), originsDirName, filepath.Base(installedPath)+".json")
}

func writeOrigin(installedPath string, origin Origin) error {
	path := originPath(installedPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(origin, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	tmp, err := os.CreateTemp(filepath.Dir(path), ".origin-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("persist install origin: %w", err)
	}
	return nil
}

// ReadOrigin loads provenance for an installed bundle directory.
func ReadOrigin(installedPath string) (*Origin, error) {
	b, err := os.ReadFile(originPath(installedPath))
	if err != nil {
		return nil, err
	}
	var origin Origin
	if err := json.Unmarshal(b, &origin); err != nil {
		return nil, fmt.Errorf("decode install origin: %w", err)
	}
	return &origin, nil
}
