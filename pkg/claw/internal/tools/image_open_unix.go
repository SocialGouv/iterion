//go:build !windows

package tools

import (
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// openFileNoFollow opens path for reading without following any symlink, in
// the final component or in any directory along the way. The path is walked
// component by component, each step an openat relative to the directory
// descriptor held from the previous one, so a component swapped for a
// symlink between an upstream gate's check and this open fails with ELOOP
// instead of being followed outside the gate's scope. The caller reads from
// the returned descriptor itself — there is no second open by path.
func openFileNoFollow(path string) (*os.File, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	parts := strings.Split(strings.TrimPrefix(filepath.Clean(abs), "/"), "/")

	dirfd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = unix.Close(dirfd) }()

	for _, part := range parts[:len(parts)-1] {
		next, err := unix.Openat(dirfd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if err != nil {
			return nil, err
		}
		_ = unix.Close(dirfd)
		dirfd = next
	}
	// Nonblocking open prevents a FIFO planted at the path from hanging the
	// tool; it is a no-op for regular files.
	fd, err := unix.Openat(dirfd, parts[len(parts)-1], unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), abs), nil
}
