//go:build windows

package tools

import "os"

// openFileNoFollow falls back to a plain open on Windows: the
// component-by-component O_NOFOLLOW walk is Unix-only, so a symlink swapped
// in between an upstream gate's check and this open is not detected here.
func openFileNoFollow(path string) (*os.File, error) {
	return os.Open(path)
}
