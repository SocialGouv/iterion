package bundle

import (
	"os"
	"path/filepath"
)

// ResolveChild resolves a `subbot source:` of the file at parent, within
// the collection that holds the bundle at dir — the same confinement as
// MaxSyntaxRequirementsDir: a child beyond the collection, through `..` or
// a symlink, is not read (C253 names it). It returns the child's resolved
// path and its bytes, or ok false when the child is beyond the collection
// or not there.
func ResolveChild(dir, parent, source string) (path string, src []byte, ok bool) {
	if source == "" || filepath.IsAbs(source) {
		return "", nil, false
	}
	root := filepath.Clean(dir)
	if real, err := filepath.EvalSymlinks(root); err == nil {
		root = real
	}
	collection := filepath.Dir(root)
	real, err := filepath.EvalSymlinks(filepath.Join(filepath.Dir(parent), filepath.FromSlash(source)))
	if err != nil || !within(real, collection) {
		return "", nil, false
	}
	src, err = os.ReadFile(real)
	if err != nil {
		return "", nil, false
	}
	return real, src, true
}
