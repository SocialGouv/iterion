// The border question of the plan-to-create window (#2069): a bind source
// is resolved by the daemon at container CREATION, but verified when the
// plan is built — before the driver pulls the image, which is minutes. The
// plan names what it verified (Spec.BindSourceChecks); the driver re-asks
// each entry at that border, and the runtime witnesses replay the same
// question over the plan's own entries, so the invariant "a plan names only
// sources its own border accepts" is judged on THIS question, not on a
// weaker neighbor.

package sandbox

import (
	"fmt"
	"os"

	"github.com/SocialGouv/iterion/internal/safepath"
)

// AnswerBindSourceCheck re-asks one recorded check: is the source still the
// path the plan verified, under the root the plan compared it against? The
// answer parts ways on the SHAPE of the source at this border:
//
//   - MustExist, the engine-laid workspace: identity, never creation —
//     nothing may stand in for the tree the engine laid, and a source gone
//     since the plan refuses instead of being recreated.
//   - a directory: the store ask it passed at plan time, re-asked. The
//     directory is already laid (the plan creates the stores it mounts),
//     so the ask's create half is a no-op kept for the re-ask it performs
//     after it.
//   - a regular file — a copy seed (plugins.yaml, a worktree's .git/config)
//     or a host-bound state file: identity only, never creation. MkdirAll
//     on an existing file is ENOTDIR (every honest container refused), and
//     on a file gone missing it would stand a DIRECTORY where the operator
//     keeps a file — the corruption class that closes here.
//   - absent, whatever it was at plan time: refuse. The plan verified this
//     exact source, and the border recreates nothing the window removed;
//     the cold recreate is the PLAN's move, and the retry replans.
//
// A symlink swapped in for the source fails the identity ask in every
// branch — a Lstat of a link is never a directory, so links land in the
// file branch, whose ask resolves them — which is the swap this window
// exists to catch.
func AnswerBindSourceCheck(c BindSourceCheck) error {
	if c.MustExist {
		if err := safepath.RealBindSource(c.Root, c.Path); err != nil {
			return fmt.Errorf("the bind source %s is not the path the plan verified under %s (%v) — swapped between plan and creation", c.Path, c.Root, err)
		}
		if _, err := os.Stat(c.Path); err != nil {
			return fmt.Errorf("the bind source %s the plan verified under %s is gone (%v) — it is the engine-laid tree, not a store to recreate", c.Path, c.Root, err)
		}
		return nil
	}
	info, err := os.Lstat(c.Path)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("the bind source %s the plan verified under %s is gone (%v) — the plan verified this exact source, and the border recreates nothing the window removed", c.Path, c.Root, err)
		}
		return fmt.Errorf("the bind source %s under %s cannot be asked (%v)", c.Path, c.Root, err)
	}
	if !info.IsDir() {
		if err := safepath.RealBindSource(c.Root, c.Path); err != nil {
			return fmt.Errorf("the bind source %s is not the path the plan verified under %s (%v) — swapped between plan and creation", c.Path, c.Root, err)
		}
		return nil
	}
	// 0o700 matches the only writer of store-backed sources (host_state's
	// store dirs, owner-only).
	if err := safepath.EnsureRealBindSourceDir(c.Root, c.Path, 0o700); err != nil {
		return fmt.Errorf("the bind source %s is not the path the plan verified under %s (%v) — swapped between plan and creation", c.Path, c.Root, err)
	}
	return nil
}
