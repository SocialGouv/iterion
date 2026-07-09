//go:build !unix

package store

import (
	"io/fs"
	"os"
	"path/filepath"
)

// openRunFileAt is the non-Unix fallback for OpenRunFile. The production
// hardening relies on the unix implementation above; non-Unix builds retain
// best-effort lexical traversal protection.
func openRunFileAt(root string, components []string) (*os.File, fs.FileInfo, error) {
	p := root
	for _, component := range components {
		p = filepath.Join(p, component)
	}
	f, err := os.Open(p)
	if err != nil {
		return nil, nil, err
	}
	info, err := f.Stat()
	if err != nil || info.IsDir() {
		_ = f.Close()
		if err != nil {
			return nil, nil, err
		}
		return nil, nil, os.ErrInvalid
	}
	return f, info, nil
}
