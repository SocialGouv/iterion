//go:build !linux && !darwin

package server

import (
	"errors"
	"os"
)

func renameDependencyNoReplace(_, _ string, _, _ os.FileInfo) error {
	return errors.New("dependency transactions require non-overwriting directory renames on this platform")
}
func requireDependencySameFilesystem(_, _ string) error {
	return errors.New("dependency transactions require filesystem identity checks on this platform")
}
