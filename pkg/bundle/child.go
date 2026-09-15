package bundle

import (
	"os"
	"path/filepath"
)

// collectionOf is a bundle's root and the collection that holds it, both
// absolute and symlink-resolved: the confinement every reader of a bundle's
// children shares (MaxSyntaxRequirementsDir, ResolveChild). A root that
// cannot be resolved is not there.
func collectionOf(dir string) (root, collection string, ok bool) {
	root, err := realPath(dir)
	if err != nil {
		return "", "", false
	}
	return root, filepath.Dir(root), true
}

// realPath is the absolute, symlink-resolved form of a path — the one
// namespace two paths must share before one is held within the other.
func realPath(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(abs)
}

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
	_, collection, ok := collectionOf(dir)
	if !ok {
		return "", nil, false
	}
	// The parent's directory is resolved before the source is joined to it:
	// joined first, a `..` folds lexically across a symlinked directory
	// (`link/parent/../sib` → `link/sib`) and names a file the OS does not
	// reach from the parent.
	from, err := realPath(filepath.Dir(parent))
	if err != nil {
		return "", nil, false
	}
	real, err := filepath.EvalSymlinks(filepath.Join(from, filepath.FromSlash(source)))
	if err != nil || !within(real, collection) {
		return "", nil, false
	}
	src, err = os.ReadFile(real)
	if err != nil {
		return "", nil, false
	}
	return real, src, true
}
