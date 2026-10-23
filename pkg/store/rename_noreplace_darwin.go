//go:build darwin

package store

import "golang.org/x/sys/unix"

func renameNoReplace(from, to string) error {
	return wrapRenameErr(from, to, unix.RenameatxNp(unix.AT_FDCWD, from, unix.AT_FDCWD, to, unix.RENAME_EXCL))
}
