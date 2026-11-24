//go:build !linux && !darwin && !windows

package authoringlock

import (
	"errors"
	"os"
)

// ErrPlatform is a platform without the directory-bound, no-replace
// operations the contract is built on. The lock refuses rather than
// pretending to serialize.
var ErrPlatform = errors.New("local authoring requires directory-bound no-replace operations on this platform")

func OpenDirectory(string, os.FileInfo) (*os.File, error)             { return nil, ErrPlatform }
func MkdirAt(*os.File, string) error                                  { return ErrPlatform }
func OpenFileAt(*os.File, string, int, os.FileMode) (*os.File, error) { return nil, ErrPlatform }
func ValidateOwnership(*os.File, bool) error                          { return ErrPlatform }
func SyncDirectory(*os.File) error                                    { return ErrPlatform }
func NormalizeName(name string) string                                { return name }
func OpenFlags() int                                                  { return os.O_RDWR }
