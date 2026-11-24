package store

import (
	"context"
	"fmt"
	"os"
)

var _ BackendSessionStore = (*FilesystemRunStore)(nil)

func (s *FilesystemRunStore) PutBackendSession(ctx context.Context, runID, ref string, body []byte) error {
	if err := sanitizePathComponent("run ID", runID); err != nil {
		return err
	}
	if err := sanitizePathComponent("session ref", ref); err != nil {
		return err
	}
	_, unlock, err := s.lockRunMetadataMutation(ctx, runID)
	if err != nil {
		return err
	}
	defer unlock()
	p, err := s.openRunPayloadPath(ctx, runID, true, "backend-sessions", ref)
	if err != nil {
		return err
	}
	defer p.Close()
	if err := p.WriteAtomic(ctx, body, filePerm); err != nil {
		return fmt.Errorf("store: write backend session: %w", err)
	}
	return nil
}

func (s *FilesystemRunStore) GetBackendSession(ctx context.Context, runID, ref string) ([]byte, error) {
	if err := sanitizePathComponent("run ID", runID); err != nil {
		return nil, err
	}
	if err := sanitizePathComponent("session ref", ref); err != nil {
		return nil, err
	}
	p, err := s.openRunPayloadPath(ctx, runID, false, "backend-sessions", ref)
	if err != nil {
		return nil, err
	}
	defer p.Close()
	b, err := p.ReadFile()
	if err != nil {
		return nil, fmt.Errorf("store: read backend session: %w", err)
	}
	return b, nil
}

func (s *FilesystemRunStore) DeleteBackendSession(ctx context.Context, runID, ref string) error {
	if err := sanitizePathComponent("run ID", runID); err != nil {
		return err
	}
	if err := sanitizePathComponent("session ref", ref); err != nil {
		return err
	}
	_, unlock, err := s.lockRunMetadataMutation(ctx, runID)
	if err != nil {
		return err
	}
	defer unlock()
	dir, err := s.openRunPayloadDirectory(ctx, runID, false, "backend-sessions")
	if err != nil {
		return err
	}
	defer dir.Close()
	err = dir.Remove(ctx, ref)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("store: delete backend session: %w", err)
	}
	return nil
}
