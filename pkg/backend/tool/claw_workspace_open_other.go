//go:build !unix

package tool

import (
	"fmt"
	"os"
	"path/filepath"
)

func openWorkspaceFileAt(root string, parts []string) (*os.File, error) {
	r, err := os.OpenRoot(root)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	path := ""
	for _, part := range parts {
		path = filepath.Join(path, part)
		info, err := r.Lstat(path)
		if err != nil {
			return nil, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("workspace path changed to a symlink")
		}
	}
	return r.Open(path)
}
