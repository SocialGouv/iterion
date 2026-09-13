//go:build !linux && !darwin && !windows

package server

import (
	"errors"
	"os"
)

var errAuthoringPlatform = errors.New("local authoring requires directory-bound no-replace operations on this platform")

func openAuthoringDirectoryFile(string, os.FileInfo) (*os.File, error) {
	return nil, errAuthoringPlatform
}
func mkdirAuthoringAt(*os.File, string) error { return errAuthoringPlatform }
func openAuthoringFileAt(*os.File, string, int, os.FileMode) (*os.File, error) {
	return nil, errAuthoringPlatform
}
func renameAuthoringAt(*os.File, string, *os.File, string) error { return errAuthoringPlatform }
func requireAuthoringSameFilesystem(*os.File, *os.File) error    { return errAuthoringPlatform }
func validateAuthoringOwnership(*os.File, bool) error            { return errAuthoringPlatform }
func syncAuthoringDirectory(*os.File) error                      { return errAuthoringPlatform }
func normalizeAuthoringLockName(name string) string              { return name }
func authoringLockOpenFlags() int                                { return os.O_RDWR }
