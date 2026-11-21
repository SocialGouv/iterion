//go:build !unix

package store

import (
	"io/fs"
	"os"
)

// Historical reads do not require strict atomic publication. The retained root
// confines lookup; no byte is read until the opened regular file matches the
// observed entry. Unix keeps its nonblocking/no-follow implementation.
func openStreamRegular(root *os.Root, name string) (*os.File, fs.FileInfo, error) {
	before, e := root.Lstat(name)
	if e != nil {
		return nil, nil, e
	}
	if !before.Mode().IsRegular() {
		return nil, nil, ErrRunDataIdentityChanged
	}
	f, e := root.Open(name)
	if e != nil {
		return nil, nil, e
	}
	after, e := f.Stat()
	if e != nil {
		f.Close()
		return nil, nil, streamNodeError(e)
	}
	if !after.Mode().IsRegular() || !os.SameFile(before, after) {
		f.Close()
		return nil, nil, ErrRunDataIdentityChanged
	}
	current, e := root.Lstat(name)
	if e != nil {
		f.Close()
		return nil, nil, streamNodeError(e)
	}
	if !current.Mode().IsRegular() || !os.SameFile(after, current) {
		f.Close()
		return nil, nil, ErrRunDataIdentityChanged
	}
	return f, after, nil
}
