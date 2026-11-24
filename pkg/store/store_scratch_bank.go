package store

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

var _ ScratchBankStore = (*FilesystemRunStore)(nil)

// PutScratchBank streams body to the run's bank through a temporary file
// in the same directory, renamed into place once size bytes landed: a
// reader cut short leaves the previous bank, never a truncated one.
func (s *FilesystemRunStore) PutScratchBank(ctx context.Context, runID string, body io.Reader, size int64) error {
	if err := sanitizePathComponent("run ID", runID); err != nil {
		return err
	}
	if body == nil || size < 0 {
		return fmt.Errorf("store: put scratch bank: no body or a negative size (%d)", size)
	}
	bound, staged, n, err := s.stageRunMetadataBody(ctx, runID, body, nil)
	if err != nil {
		return fmt.Errorf("store: stage scratch bank: %w", err)
	}
	defer staged.Close()
	if n != size {
		return fmt.Errorf("store: put scratch bank: wrote %d bytes, the bank announced %d", n, size)
	}
	_, unlock, err := s.lockRunMetadataMutation(bound, runID)
	if err != nil {
		return err
	}
	defer unlock()
	dst, err := s.openRunPayloadFile(bound, runID, "scratch-bank.tgz", true)
	if err != nil {
		return err
	}
	defer dst.Close()
	if err = staged.publish(bound, dst, filePerm); err != nil {
		return fmt.Errorf("store: publish scratch bank: %w", err)
	}
	return nil
}

func (s *FilesystemRunStore) OpenScratchBank(ctx context.Context, runID string) (io.ReadCloser, error) {
	if err := sanitizePathComponent("run ID", runID); err != nil {
		return nil, err
	}
	p, err := s.openRunPayloadFile(ctx, runID, "scratch-bank.tgz", false)
	if err != nil {
		return nil, err
	}
	defer p.Close()
	f, err := p.Open()
	if err != nil {
		return nil, fmt.Errorf("store: open scratch bank: %w", err)
	}
	if err := ctx.Err(); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

func (s *FilesystemRunStore) DeleteScratchBank(ctx context.Context, runID string) error {
	if err := sanitizePathComponent("run ID", runID); err != nil {
		return err
	}
	_, unlock, err := s.lockRunMetadataMutation(ctx, runID)
	if err != nil {
		return err
	}
	defer unlock()
	payload, err := s.openRunPayloadFile(ctx, runID, "scratch-bank.tgz", false)
	if err != nil {
		return err
	}
	defer payload.Close()
	dir := &runPayloadDirectory{parent: &runPayloadFile{key: filepath.Dir(payload.key), root: payload.root, strict: payload.strict, name: "."}}
	if err := dir.Remove(ctx, "scratch-bank.tgz"); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("store: delete scratch bank: %w", err)
	}
	return nil
}
