//go:build linux || darwin

package authoringlock

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// OpenDirectory opens path as a directory and pins it to expected: the
// descriptor it answers is the directory that Lstat saw, or it answers
// nothing. The open never follows a symlink at the leaf, and the identity
// and mode are compared afterwards, so a namespace substitution between the
// two is caught instead of served.
func OpenDirectory(path string, expected os.FileInfo) (*os.File, error) {
	if expected == nil {
		return nil, fmt.Errorf("%w: %s", ErrDirectoryChanged, path)
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	info, err := file.Stat()
	if err != nil || !os.SameFile(expected, info) || info.Mode() != expected.Mode() {
		_ = file.Close()
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("%w: %s", ErrDirectoryChanged, path)
	}
	return file, nil
}

// MkdirAt creates a control directory relative to an opened parent, at the
// private directory mode. Being relative to the descriptor is what keeps a
// renamed parent from redirecting the creation.
func MkdirAt(parent *os.File, name string) error {
	return unix.Mkdirat(int(parent.Fd()), name, dirMode)
}

// OpenFileAt opens a control file relative to an opened parent, never
// following a symlink and never blocking on a fifo.
func OpenFileAt(parent *os.File, name string, flags int, mode os.FileMode) (*os.File, error) {
	fd, err := unix.Openat(int(parent.Fd()), name, flags|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, uint32(mode.Perm()))
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), filepath.Join(parent.Name(), name)), nil
}

// ValidateOwnership holds an opened control object to belonging to this user
// and to being unwritable by anyone else — and, when private, unreadable by
// anyone else. Its message is the one the server's save answers with.
func ValidateOwnership(f *os.File, private bool) error {
	var st unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &st); err != nil {
		return err
	}
	if st.Uid != uint32(os.Geteuid()) || st.Mode&0o022 != 0 || (private && st.Mode&0o077 != 0) {
		return fmt.Errorf("unsafe authoring ownership or permissions: %s", f.Name())
	}
	return nil
}

// SyncDirectory flushes a directory's entries, so a creation a concurrent
// taker is about to look for is there to be found.
func SyncDirectory(f *os.File) error { return f.Sync() }

// NormalizeName is the platform's own folding of a file name before the
// lock's fold: nothing to do where every byte but the separator is a name.
func NormalizeName(name string) string { return name }

// OpenFlags is how the lock file itself is opened: never through a symlink,
// never blocking.
func OpenFlags() int { return os.O_RDWR | unix.O_NOFOLLOW | unix.O_NONBLOCK }
