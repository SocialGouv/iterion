package bundle

import (
	"os"
	"path/filepath"
	"strings"
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

// ChildPath joins a relative `subbot source:` onto parentDir.
//
// A source that CLIMBS is joined onto the parent's resolved directory: folded
// lexically, a `..` crosses a symlinked directory (`link/parent/../sib` →
// `link/sib`) and names a file the OS does not reach from the parent, which is
// how two readers of one `source:` come to disagree about which file a bundle
// runs.
//
// A source that does not climb keeps the spelling the author wrote. Both forms
// name the same file, and the written one is what identity is anchored on
// downstream — the walk up to `bots.lock`, and the bot id a child's memory is
// scoped by. Canonicalising it would re-anchor both for no correctness gain.
//
// Only the parent's side is ever resolved: the child need not exist, and
// nothing here confines it. Confinement belongs to the caller, and the callers
// differ: this package refuses a child beyond the collection (C253), the pod's
// resolver refuses one outside every catalogue root, and the in-process
// resolver of a plain file reference confines nothing at all.
func ChildPath(parentDir, source string) (string, error) {
	if !climbs(source) {
		return filepath.Join(parentDir, filepath.FromSlash(source)), nil
	}
	from, err := realPath(parentDir)
	if err != nil {
		return "", err
	}
	return filepath.Join(from, filepath.FromSlash(source)), nil
}

// climbs reports whether source walks out of the directory it is joined to.
// Clean collapses interior `..`, so what survives it is a real climb.
func climbs(source string) bool {
	for _, seg := range strings.Split(filepath.ToSlash(filepath.Clean(source)), "/") {
		if seg == ".." {
			return true
		}
	}
	return false
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
	joined, err := ChildPath(filepath.Dir(parent), source)
	if err != nil {
		return "", nil, false
	}
	real, err := filepath.EvalSymlinks(joined)
	if err != nil || !within(real, collection) {
		return "", nil, false
	}
	src, err = os.ReadFile(real)
	if err != nil {
		return "", nil, false
	}
	return real, src, true
}
