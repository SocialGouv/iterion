//go:build !unix

package bundle

import "os"

// openFileNoFollow falls back to a plain OpenFile on platforms that
// don't expose O_NOFOLLOW. assertNoEscapingSymlink still guards
// intermediate components; the leaf TOCTOU window remains but only
// affects platforms (notably Windows) where the bundle extractor is
// already a secondary use case.
func openFileNoFollow(path string, mode os.FileMode) (*os.File, error) {
	return os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
}
