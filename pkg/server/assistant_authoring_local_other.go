//go:build !linux && !darwin && !windows

package server

import (
	"os"

	"github.com/SocialGouv/iterion/pkg/authoringlock"
)

// errAuthoringPlatform is the lock's own refusal, reused by the publication
// primitives beside it: a platform without directory-bound no-replace
// operations gets no local authoring at all.
var errAuthoringPlatform = authoringlock.ErrPlatform

func renameAuthoringAt(*os.File, string, *os.File, string) error { return errAuthoringPlatform }
func replaceAuthoringAt(*os.File, string, string) error          { return errAuthoringPlatform }
func requireAuthoringSameFilesystem(*os.File, *os.File) error    { return errAuthoringPlatform }
func authoringFileLinks(*os.File) (uint64, error)                { return 0, errAuthoringPlatform }
