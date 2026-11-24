// Bind sources — the filesystem half of a bind mount's host path.
//
// A bind mount's SOURCE is resolved by the kernel on the HOST, following
// every symlink in it. So a bind whose source lives under a directory the
// sandbox itself writes is a bind whose source the SANDBOX chooses: the run
// plants `<store>/<name> -> /home/op/.ssh` and the next run's mount plan
// hands that target to the container runtime, read-write, under a name the
// audit still spells `<store>`. The overlays and copies laid elsewhere are
// bypassed entirely, because nothing about this mount's STRING looks wrong.
//
// The same question is asked at TWO moments, and both asks are load-bearing:
// once when the mount plan is built, and once immediately before the
// container is created — the daemon resolves the source at creation, so a
// plan-time check alone answers for a tree a concurrent run can have swapped
// since (the package doctrine above, at mount-plan scale).

package safepath

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// RealBindSource reports the error when path is not literally root plus a
// chain of real directories — i.e. when a symlink under root could make the
// bind reach a host path the sandbox chose. nil means the source is the one
// the caller named.
//
// The root is resolved first (EvalSymlinks) and the comparison is made against
// the resolved form: an operator whose home is itself reached through a symlink
// (/home -> /mnt/home, macOS /var -> /private/var) must not be refused for the
// filesystem's own doing, only for what the sandbox added underneath. A
// component that does not exist yet cannot be a symlink, so a path about to be
// created passes — [EnsureRealBindSourceDir] re-asks after creating it, which
// is what closes the window a `MkdirAll` through a link would open.
func RealBindSource(root, path string) error {
	if root == "" || path == "" {
		return fmt.Errorf("bind source %q under root %q: both must be named", path, root)
	}
	root, path = filepath.Clean(root), filepath.Clean(path)
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return fmt.Errorf("bind source %s is not under %s", path, root)
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return fmt.Errorf("resolve the store root %s: %w", root, err)
	}
	// The exact comparison, for the path that already exists: the resolved
	// source must be the resolved root plus the same tail. safepath's walk
	// answers for the components that do not exist yet, and names the one
	// component that is a link when one is.
	if _, statErr := os.Lstat(path); statErr == nil {
		resolved, evalErr := filepath.EvalSymlinks(path)
		if evalErr != nil {
			return fmt.Errorf("resolve the bind source %s: %w", path, evalErr)
		}
		if want := filepath.Join(realRoot, rel); resolved != want {
			return fmt.Errorf("the bind source %s resolves to %s, not to %s — a symlink under %s decides what this mount reaches", path, resolved, want, root)
		}
	}
	return AssertNoEscapingSymlink(realRoot, filepath.Join(realRoot, rel))
}

// EnsureRealBindSourceDir creates the bind source as a real directory under
// root, with the check on BOTH sides of the creation: before, so MkdirAll does
// not follow a planted link, and after, so a link planted between the two does
// not survive into the mount plan. Every error names the store and is the
// caller's reason to skip the bind.
func EnsureRealBindSourceDir(root, path string, perm os.FileMode) error {
	if err := RealBindSource(root, path); err != nil {
		return err
	}
	if err := os.MkdirAll(path, perm); err != nil {
		return fmt.Errorf("create the bind source %s: %w", path, err)
	}
	return RealBindSource(root, path)
}
