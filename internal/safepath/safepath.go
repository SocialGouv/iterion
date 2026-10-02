// Package safepath turns a name taken from an archive — a string chosen by
// whoever wrote the archive — into a path that is safe to write under a root.
//
// Two defences, and both are needed. The lexical one refuses an absolute
// name, a "..", a non-portable separator. The filesystem one refuses a path
// whose existing components lead out of the root through a symlink: the OS
// follows a symlink at open time, so a pre-existing `root/foo -> /etc` turns
// the innocent name `foo/bar` into a write outside the root that no amount of
// string checking sees.
//
// Everything here answers for ONE entry at a time, and re-reads the
// filesystem for each: an archive can create a symlink and then send a member
// through it, so a check made once before the extract answers for a tree that
// no longer exists by the time the member lands.
package safepath

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// GuardName checks an archive entry's name for the lexical bans — absolute
// paths, "..", non-portable separators — before any filesystem operation.
func GuardName(name string) error {
	clean := filepath.ToSlash(filepath.Clean(name))
	if clean == "" || clean == "." {
		return nil
	}
	if strings.HasPrefix(clean, "/") {
		return fmt.Errorf("absolute path not allowed: %s", name)
	}
	for _, part := range strings.Split(clean, "/") {
		if part == ".." {
			return fmt.Errorf("path traversal not allowed: %s", name)
		}
	}
	return nil
}

// Join joins root and rel and verifies the result stays under root, both
// lexically and through every existing component of the path. root must be
// absolute and already cleaned.
func Join(root, rel string) (string, error) {
	joined := filepath.Join(root, filepath.FromSlash(rel))
	abs, err := filepath.Abs(joined)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", rel, err)
	}
	if abs != root && !strings.HasPrefix(abs, root+string(os.PathSeparator)) {
		return "", fmt.Errorf("entry escapes root: %s", rel)
	}
	if err := AssertNoEscapingSymlink(root, abs); err != nil {
		return "", err
	}
	return abs, nil
}

// AssertNoEscapingSymlink walks every existing prefix of abs (root..abs) and
// refuses the path if a component is a symlink whose resolved target escapes
// root. Components that do not exist yet are ignored — they cannot be
// symlinks.
func AssertNoEscapingSymlink(root, abs string) error {
	if !strings.HasPrefix(abs, root) {
		return fmt.Errorf("internal: abs %s outside root %s", abs, root)
	}
	rel, err := filepath.Rel(root, abs)
	if err != nil {
		return fmt.Errorf("rel %s: %w", abs, err)
	}
	cur := root
	for _, part := range strings.Split(filepath.ToSlash(rel), "/") {
		if part == "" || part == "." {
			continue
		}
		cur = filepath.Join(cur, part)
		info, err := os.Lstat(cur)
		if os.IsNotExist(err) {
			return nil // the remaining suffix does not exist yet
		}
		if err != nil {
			return fmt.Errorf("stat %s: %w", cur, err)
		}
		if info.Mode()&os.ModeSymlink == 0 {
			continue
		}
		resolved, err := filepath.EvalSymlinks(cur)
		if err != nil {
			return fmt.Errorf("eval symlink %s: %w", cur, err)
		}
		if resolved != root && !strings.HasPrefix(resolved, root+string(os.PathSeparator)) {
			return fmt.Errorf("refusing entry: component %s is a symlink escaping root", cur)
		}
	}
	return nil
}

// Landing is the path a write to abs actually reaches, relative to root and
// slash-separated: every existing symlink of the ancestor chain resolved, the
// elements that do not exist yet re-appended as named. A caller that decides
// from a name — an exclusion list, an allow-list — must decide from this
// instead, because a link member can make `a/x/file` land in `a/hooks/file`
// while the name it carries matches nothing.
//
// abs must already have passed Join for the same root.
func Landing(root, abs string) (string, error) {
	parent, base := filepath.Split(abs)
	parent = filepath.Clean(parent)
	// The DEEPEST EXISTING ancestor, not the parent: a directory that does
	// not exist yet is created later (MkdirAll) through whatever link sits
	// above it, so stopping at a missing parent answers about a path nobody
	// will write to.
	existing, missing := parent, ""
	for {
		if _, err := os.Lstat(existing); err == nil {
			break
		}
		up := filepath.Dir(existing)
		if up == existing || len(existing) <= len(root) {
			break
		}
		missing = filepath.Join(filepath.Base(existing), missing)
		existing = up
	}
	resolved, err := filepath.EvalSymlinks(existing)
	if err != nil {
		return "", fmt.Errorf("eval %s: %w", existing, err)
	}
	rel, err := filepath.Rel(root, filepath.Join(resolved, missing, base))
	if err != nil {
		return "", fmt.Errorf("rel %s: %w", abs, err)
	}
	rel = filepath.ToSlash(rel)
	if rel == ".." || strings.HasPrefix(rel, "../") {
		return "", fmt.Errorf("entry lands outside root: %s", abs)
	}
	return rel, nil
}
