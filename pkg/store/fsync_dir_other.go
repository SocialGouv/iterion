//go:build !windows

package store

import (
	"errors"
	"os"
	"syscall"
)

// fsyncDir opens dir and calls Sync on the descriptor so the directory
// entry update from the most recent rename/create reaches the platter.
// A file system that cannot sync a directory (EINVAL, ENOTSUP) makes it a
// best-effort no-op.
func fsyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	syncErr := d.Sync()
	closeErr := d.Close()
	if syncErr != nil {
		if errors.Is(syncErr, syscall.EINVAL) || errors.Is(syncErr, syscall.ENOTSUP) {
			return closeErr
		}
		return syncErr
	}
	return closeErr
}
