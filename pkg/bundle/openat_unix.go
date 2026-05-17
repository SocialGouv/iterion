//go:build unix

package bundle

import (
	"os"
	"syscall"
)

// openFileNoFollow opens (or creates + truncates) a regular file at
// path with O_NOFOLLOW set on the final component. Used by the tar
// extractor to close the TOCTOU window between safeJoin's symlink
// audit and the actual open: a process with write access to the
// destination could otherwise drop a symlink at the leaf between the
// two operations and redirect the write outside the bundle root.
func openFileNoFollow(path string, mode os.FileMode) (*os.File, error) {
	return os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|syscall.O_NOFOLLOW, mode)
}
