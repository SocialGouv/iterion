//go:build linux || darwin

package server

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

func openAuthoringDirectoryFile(path string, info os.FileInfo) (*os.File, error) {
	return openDependencyDirectory(path, info)
}
func mkdirAuthoringAt(parent *os.File, name string) error {
	return unix.Mkdirat(int(parent.Fd()), name, 0o700)
}
func openAuthoringFileAt(parent *os.File, name string, flags int, mode os.FileMode) (*os.File, error) {
	fd, err := unix.Openat(int(parent.Fd()), name, flags|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, uint32(mode.Perm()))
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), filepath.Join(parent.Name(), name)), nil
}
func renameAuthoringAt(from *os.File, fromName string, to *os.File, toName string) error {
	return renameDependencyAt(int(from.Fd()), fromName, int(to.Fd()), toName)
}
func requireAuthoringSameFilesystem(a, b *os.File) error {
	var left, right unix.Stat_t
	if err := unix.Fstat(int(a.Fd()), &left); err != nil {
		return err
	}
	if err := unix.Fstat(int(b.Fd()), &right); err != nil {
		return err
	}
	if left.Dev != right.Dev {
		return fmt.Errorf("authoring recovery storage must share the destination filesystem: %w", unix.EXDEV)
	}
	return nil
}
func validateAuthoringOwnership(f *os.File, private bool) error {
	var st unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &st); err != nil {
		return err
	}
	if st.Uid != uint32(os.Geteuid()) || st.Mode&0o022 != 0 || (private && st.Mode&0o077 != 0) {
		return fmt.Errorf("unsafe authoring ownership or permissions: %s", f.Name())
	}
	return nil
}
func syncAuthoringDirectory(f *os.File) error       { return f.Sync() }
func normalizeAuthoringLockName(name string) string { return name }
func authoringLockOpenFlags() int                   { return os.O_RDWR | unix.O_NOFOLLOW | unix.O_NONBLOCK }
