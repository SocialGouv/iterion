//go:build windows

package store

import "golang.org/x/sys/windows"

// renameNoReplace is MoveFileEx without MOVEFILE_REPLACE_EXISTING: an
// existing name is ERROR_ALREADY_EXISTS (fs.ErrExist), never replaced.
func renameNoReplace(from, to string) error {
	f, err := windows.UTF16PtrFromString(from)
	if err != nil {
		return wrapRenameErr(from, to, err)
	}
	t, err := windows.UTF16PtrFromString(to)
	if err != nil {
		return wrapRenameErr(from, to, err)
	}
	return wrapRenameErr(from, to, windows.MoveFileEx(f, t, 0))
}
