//go:build unix

package kubernetes

import (
	"errors"
	"golang.org/x/sys/unix"
	"os"
)

func openDrainRootFromRetained(root *os.Root, displayPath string) (*drainDir, error) {
	f, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	fd, dupErr := unix.FcntlInt(f.Fd(), unix.F_DUPFD_CLOEXEC, 0)
	closeErr := f.Close()
	if err = errors.Join(dupErr, closeErr); err != nil {
		if dupErr == nil {
			_ = unix.Close(fd)
		}
		return nil, err
	}
	return &drainDir{fd: fd, path: displayPath, spelling: &drainSpelling{keys: map[string]bool{}}}, nil
}
