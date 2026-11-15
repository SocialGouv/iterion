package store

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

func runFileEntryName(name string) error {
	if name == "" || name == "." || name == ".." || filepath.IsAbs(name) || filepath.Base(name) != name || strings.ContainsRune(name, 0) {
		return os.ErrInvalid
	}
	return nil
}

// OpenRunFileDirectoryChild borrows the parent and returns an owned checked
// child. Names may contain ordinary tool filename characters, including dots.
func OpenRunFileDirectoryChild(ctx context.Context, parent *os.Root, name string) (*os.Root, error) {
	if ctx == nil {
		return nil, ErrRunDataBindingRequired
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := runFileEntryName(name); err != nil {
		return nil, err
	}
	child, err := openIncarnationChildRoot(parent, name, false)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, errors.Join(err, child.Close())
	}
	return child, nil
}

// OpenRegularRunFileRoot opens a single entry without following links or
// blocking on a substituted FIFO. The caller owns the returned file.
func OpenRegularRunFileRoot(ctx context.Context, root *os.Root, name string) (*os.File, fs.FileInfo, error) {
	if ctx == nil {
		return nil, nil, ErrRunDataBindingRequired
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if err := runFileEntryName(name); err != nil {
		return nil, nil, err
	}
	f, info, err := openRegularRunFileRetained(root, name)
	if err != nil {
		return nil, nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, errors.Join(err, f.Close())
	}
	return f, info, nil
}
