//go:build unix

package store

import (
	"io/fs"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// openRunFileAt opens components beneath root using openat-style descriptor
// traversal. Every intermediate component must be a real directory (not a
// symlink), and the final component must be a real file (not a symlink).
func openRunFileAt(root string, components []string) (*os.File, fs.FileInfo, error) {
	rootFD, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, nil, err
	}
	defer unix.Close(rootFD)

	dirFD := rootFD
	for _, component := range components[:len(components)-1] {
		nextFD, err := unix.Openat(dirFD, component, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if dirFD != rootFD {
			_ = unix.Close(dirFD)
		}
		if err != nil {
			return nil, nil, err
		}
		dirFD = nextFD
	}
	defer func() {
		if dirFD != rootFD {
			_ = unix.Close(dirFD)
		}
	}()

	finalFD, err := unix.Openat(dirFD, components[len(components)-1], unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, nil, err
	}
	f := os.NewFile(uintptr(finalFD), components[len(components)-1])
	if f == nil {
		_ = unix.Close(finalFD)
		return nil, nil, os.ErrInvalid
	}
	info, err := f.Stat()
	if err != nil || info.IsDir() {
		_ = f.Close()
		if err != nil {
			return nil, nil, err
		}
		return nil, nil, syscall.EISDIR
	}
	return f, info, nil
}
