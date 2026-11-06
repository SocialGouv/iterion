//go:build windows

package store

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

// fsyncDir flushes a directory's entries to disk on Windows.
//
// os.Open returns a read-only handle, and FlushFileBuffers refuses one with
// ERROR_ACCESS_DENIED. Every atomic write then failed after its rename had
// already published the file. FlushFileBuffers needs FILE_WRITE_DATA (for a
// directory, the right to add a file — which the write that just renamed into
// it already exercised), and FILE_FLAG_BACKUP_SEMANTICS opens a directory
// without any privilege. GENERIC_WRITE would also ask for attribute and
// subdirectory rights a create-only ACL withholds. A file system that cannot
// flush a directory (ERROR_INVALID_FUNCTION, ERROR_NOT_SUPPORTED) makes it a
// best-effort no-op, as EINVAL/ENOTSUP do on the other platforms.
func fsyncDir(dir string) error {
	name, err := windows.UTF16PtrFromString(extendedLengthPath(dir))
	if err != nil {
		return &os.PathError{Op: "open", Path: dir, Err: err}
	}
	h, err := windows.CreateFile(name, windows.FILE_WRITE_DATA,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return &os.PathError{Op: "open", Path: dir, Err: err}
	}
	syncErr := windows.FlushFileBuffers(h)
	closeErr := windows.CloseHandle(h)
	if syncErr != nil {
		if errors.Is(syncErr, windows.ERROR_INVALID_FUNCTION) || errors.Is(syncErr, windows.ERROR_NOT_SUPPORTED) {
			return closeErr
		}
		return &os.PathError{Op: "sync", Path: dir, Err: syncErr}
	}
	return closeErr
}

// extendedLengthPath gives CreateFile the \\?\ form of a path too long for
// MAX_PATH, as the os package does for its own calls: without it a host
// that has not enabled long paths would refuse to sync a directory the os
// package has just written into. Like os, it measures the absolute path
// (a short relative name under a deep working directory is long too) and
// leaves a path that already carries a \\?\, \??\ or \\.\ prefix alone.
func extendedLengthPath(p string) string {
	const maxDirPath = 248 // MAX_PATH less room for an 8.3 file name, as os uses
	if hasExtendedPrefix(p) {
		return p
	}
	abs, err := filepath.Abs(p)
	if err != nil || len(abs) < maxDirPath {
		return p
	}
	if strings.HasPrefix(abs, `\\`) {
		return `\\?\UNC\` + abs[2:]
	}
	return `\\?\` + abs
}

// hasExtendedPrefix reports a path os would pass through unprefixed:
// \??\, or \\?\ and \\.\ written with either separator.
func hasExtendedPrefix(p string) bool {
	if len(p) < 4 {
		return false
	}
	if p[:4] == `\??\` {
		return true
	}
	sep := func(c byte) bool { return c == '\\' || c == '/' }
	return sep(p[0]) && sep(p[1]) && (p[2] == '?' || p[2] == '.') && sep(p[3])
}
