package delegate

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// lexicallyWithin reports whether target sits under base by PATH ALONE, with no
// filesystem access and no symlink resolution. That is the point: a resolving
// test answers "where does this end up", and the question here is "was this
// written to look like it stays inside the checkout".
func lexicallyWithin(base, target string) bool {
	if base == "" || target == "" {
		return false
	}
	rel, err := filepath.Rel(filepath.Clean(base), filepath.Clean(target))
	if err != nil {
		return false
	}
	return !escapes(rel)
}

// escapes reports whether a filepath.Rel result leaves the base. Shared with
// lexicallyWithin, which had the correct form: a bare HasPrefix("..") also
// matches a legitimate first component like "..cache".
func escapes(rel string) bool {
	return rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// relBelow expresses target under base, retrying against the resolved base so a
// workspace spelled through a symlink still yields a walkable path. Returns ""
// when target is not below base at all.
func relBelow(base, target string) (walkBase, rel string) {
	// Absolutise FIRST. filepath.Rel refuses a relative base against an absolute
	// target, and EvalSymlinks on a relative path returns it still relative — so
	// without this a relative WorkDir made every retry fail and turned the whole
	// component walk into a no-op, in exactly the configuration where
	// pathInsideCheckout (which does absolutise) says "guard". A repo-planted
	// `.iterion` symlink then redirected the credential out of the checkout.
	if abs, err := filepath.Abs(base); err == nil {
		base = abs
	}
	if r, err := filepath.Rel(base, target); err == nil && !escapes(r) {
		return base, r
	}
	realBase, err := filepath.EvalSymlinks(base)
	if err != nil {
		return "", ""
	}
	r, err := filepath.Rel(realBase, target)
	if err != nil || escapes(r) {
		return "", ""
	}
	// Walk from the RESOLVED base, since that is what `r` is relative to.
	return realBase, r
}

// refuseSymlinkedPath rejects target when any component below base is a
// symlink, checked with Lstat so nothing is followed. Components that do not
// exist yet are fine — they are what MkdirAll is about to create.
//
// base itself is not inspected: it is iterion's own worktree path, and a
// symlink there is the engine's business, not the repo's.
func refuseSymlinkedPath(base, target, backend, subject string) error {
	// Containment is the CALLER's decision (Task.StateDir), and it answers on
	// resolved paths — so a target that is genuinely in the tree can still be
	// spelled outside the base lexically, e.g. a workspace reached through a
	// symlink. When that happens there is nothing below `base` for this guard to
	// walk, which is a no-op, NOT a refusal: erroring here silently killed the
	// credential seed (piCodexSeed swallows the error) and the node then died on
	// "No API key found for openai-codex". The leaf symlink is covered
	// separately by piGuardWriteRoot.
	walkBase, rel := relBelow(base, target)
	if rel == "" {
		return nil
	}
	cur := walkBase
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		if part == "" || part == "." {
			continue
		}
		cur = filepath.Join(cur, part)
		info, err := os.Lstat(cur)
		if os.IsNotExist(err) {
			return nil // nothing below it exists either
		}
		if err != nil {
			return fmt.Errorf("inspect %s: %w", cur, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%s: refusing to write %s through %s: it is a symlink, "+
				"and the workspace is a checkout of the target repository", backend, subject, cur)
		}
	}
	return nil
}

// refuseNonRegular rejects a path that exists but is not a plain file, checked
// with Lstat so nothing is followed. An absent path is fine — it is what the
// caller is about to create.
//
// Both ignore guards go through this. They write DIFFERENTLY on purpose (the
// seed root is iterion's, so appending is right; `<WorkDir>/.iterion` can be
// the operator's own store, so their file is left untouched) — but the check is
// the same, and hand-rolling it twice is exactly how the same symlink hole came
// to exist at two levels.
func refuseNonRegular(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return nil
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("refusing to use %s: it is not a regular file, and the workspace "+
			"is a checkout of the target repository", path)
	}
	return nil
}
