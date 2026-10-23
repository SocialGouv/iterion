//go:build !linux && !darwin && !windows

package store

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
)

// renameNoReplace has no rename that refuses an existing name here: it
// checks the name is free, then renames. A file or an empty directory
// created at to between the two is replaced — rename(2) refuses only a
// directory that holds something.
func renameNoReplace(from, to string) error {
	if _, err := os.Lstat(to); err == nil {
		return wrapRenameErr(from, to, fmt.Errorf("%s exists: %w", to, fs.ErrExist))
	} else if !errors.Is(err, fs.ErrNotExist) {
		return wrapRenameErr(from, to, err)
	}
	return wrapRenameErr(from, to, os.Rename(from, to))
}
