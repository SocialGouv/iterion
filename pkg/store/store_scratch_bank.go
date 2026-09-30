package store

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

var _ ScratchBankStore = (*FilesystemRunStore)(nil)

// scratchBankPath is runs/<id>/scratch-bank.tgz.
func (s *FilesystemRunStore) scratchBankPath(runID string) string {
	return filepath.Join(s.runDir(runID), "scratch-bank.tgz")
}

// PutScratchBank streams body to the run's bank through a temporary file
// in the same directory, renamed into place once size bytes landed: a
// reader cut short leaves the previous bank, never a truncated one.
func (s *FilesystemRunStore) PutScratchBank(_ context.Context, runID string, body io.Reader, size int64) error {
	if err := sanitizePathComponent("run ID", runID); err != nil {
		return err
	}
	if err := s.guardNotDeleted(runID); err != nil {
		return err
	}
	if body == nil || size < 0 {
		return fmt.Errorf("store: put scratch bank: no body or a negative size (%d)", size)
	}
	final := s.scratchBankPath(runID)
	if err := os.MkdirAll(filepath.Dir(final), dirPerm); err != nil {
		return fmt.Errorf("store: mkdir run dir: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(final), ".scratch-bank-*.tgz")
	if err != nil {
		return fmt.Errorf("store: put scratch bank: %w", err)
	}
	n, err := io.Copy(tmp, body)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil && n != size {
		err = fmt.Errorf("wrote %d bytes, the bank announced %d", n, size)
	}
	if err == nil {
		err = os.Chmod(tmp.Name(), filePerm)
	}
	if err == nil {
		err = os.Rename(tmp.Name(), final)
	}
	if err != nil {
		_ = os.Remove(tmp.Name())
		return fmt.Errorf("store: put scratch bank: %w", err)
	}
	return nil
}

func (s *FilesystemRunStore) OpenScratchBank(_ context.Context, runID string) (io.ReadCloser, error) {
	if err := sanitizePathComponent("run ID", runID); err != nil {
		return nil, err
	}
	f, err := os.Open(s.scratchBankPath(runID))
	if err != nil {
		return nil, fmt.Errorf("store: open scratch bank: %w", err)
	}
	return f, nil
}

func (s *FilesystemRunStore) DeleteScratchBank(_ context.Context, runID string) error {
	if err := sanitizePathComponent("run ID", runID); err != nil {
		return err
	}
	if err := os.Remove(s.scratchBankPath(runID)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("store: delete scratch bank: %w", err)
	}
	return nil
}
