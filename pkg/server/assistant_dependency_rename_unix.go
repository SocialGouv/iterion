//go:build linux || darwin

package server

import (
	"os"
	"path/filepath"

	"github.com/SocialGouv/iterion/pkg/authoringlock"
)

// openDependencyDirectory opens a rename parent pinned to the directory the
// caller stat'd. The authoring lock's control directory is opened with the
// same guarantee, through the same code: one implementation of a pinned
// open, so a fix to it is a fix for both.
func openDependencyDirectory(path string, expected os.FileInfo) (*os.File, error) {
	return authoringlock.OpenDirectory(path, expected)
}

func renameDependencyNoReplace(from, to string, fromInfo, toInfo os.FileInfo) error {
	source, err := openDependencyDirectory(filepath.Dir(from), fromInfo)
	if err != nil {
		return err
	}
	defer func() { _ = source.Close() }()
	target, err := openDependencyDirectory(filepath.Dir(to), toInfo)
	if err != nil {
		return err
	}
	defer func() { _ = target.Close() }()
	return renameDependencyAt(int(source.Fd()), filepath.Base(from), int(target.Fd()), filepath.Base(to))
}
