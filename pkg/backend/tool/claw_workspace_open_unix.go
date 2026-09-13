//go:build unix

package tool

import (
	"os"

	"golang.org/x/sys/unix"
)

// Open every component relative to the held directory descriptor. Nonblocking
// open prevents a FIFO substituted after validation from hanging the tool.
func openWorkspaceFileAt(root string, parts []string) (*os.File, error) {
	fd, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = unix.Close(fd) }()
	for _, part := range parts[:len(parts)-1] {
		next, err := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if err != nil {
			return nil, err
		}
		_ = unix.Close(fd)
		fd = next
	}
	out, err := unix.Openat(fd, parts[len(parts)-1], unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(out), parts[len(parts)-1]), nil
}
