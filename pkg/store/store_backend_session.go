package store

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

var _ BackendSessionStore = (*FilesystemRunStore)(nil)

// backendSessionPath is runs/<id>/backend-sessions/<ref>.
func (s *FilesystemRunStore) backendSessionPath(runID, ref string) string {
	return filepath.Join(s.runDir(runID), "backend-sessions", ref)
}

func (s *FilesystemRunStore) PutBackendSession(_ context.Context, runID, ref string, body []byte) error {
	if err := sanitizePathComponent("run ID", runID); err != nil {
		return err
	}
	if err := sanitizePathComponent("session ref", ref); err != nil {
		return err
	}
	if err := s.guardNotDeleted(runID); err != nil {
		return err
	}
	dir := filepath.Dir(s.backendSessionPath(runID, ref))
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return fmt.Errorf("store: mkdir backend-sessions: %w", err)
	}
	if err := writeFileAtomic(s.backendSessionPath(runID, ref), body, filePerm); err != nil {
		return fmt.Errorf("store: write backend session: %w", err)
	}
	return nil
}

func (s *FilesystemRunStore) GetBackendSession(_ context.Context, runID, ref string) ([]byte, error) {
	if err := sanitizePathComponent("run ID", runID); err != nil {
		return nil, err
	}
	if err := sanitizePathComponent("session ref", ref); err != nil {
		return nil, err
	}
	b, err := os.ReadFile(s.backendSessionPath(runID, ref))
	if err != nil {
		return nil, fmt.Errorf("store: read backend session: %w", err)
	}
	return b, nil
}

func (s *FilesystemRunStore) DeleteBackendSession(_ context.Context, runID, ref string) error {
	if err := sanitizePathComponent("run ID", runID); err != nil {
		return err
	}
	if err := sanitizePathComponent("session ref", ref); err != nil {
		return err
	}
	err := os.Remove(s.backendSessionPath(runID, ref))
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("store: delete backend session: %w", err)
	}
	return nil
}
