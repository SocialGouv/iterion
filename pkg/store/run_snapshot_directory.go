package store

import (
	"os"
	"path/filepath"
)

// snapshotDirectory keeps a whole scan/read operation on one selected tree.
type snapshotDirectory interface {
	ReadDir() ([]os.DirEntry, error)
	ReadFile(string) ([]byte, error)
}

// historicalSnapshotDirectory is confined to legacy global erasure and
// raw-path fixture helpers. Incarnation operations use runPayloadDirectory.
type historicalSnapshotDirectory string

func (d historicalSnapshotDirectory) ReadDir() ([]os.DirEntry, error) { return os.ReadDir(string(d)) }
func (d historicalSnapshotDirectory) ReadFile(name string) ([]byte, error) {
	return os.ReadFile(filepath.Join(string(d), name))
}
