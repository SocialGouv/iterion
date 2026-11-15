//go:build unix

package store

import (
	"errors"
	"io/fs"
	"os"
	"syscall"
)

func openStreamRegular(root *os.Root, name string) (*os.File, fs.FileInfo, error) {
	f, info, e := openRegularRunFileRetained(root, name)
	if errors.Is(e, os.ErrInvalid) || errors.Is(e, syscall.ELOOP) || errors.Is(e, syscall.EISDIR) {
		e = errors.Join(ErrRunDataIdentityChanged, e)
	}
	return f, info, e
}
