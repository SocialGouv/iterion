package delegate

import (
	"fmt"
	"os"
	"path/filepath"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// PrepareStateRoot runs the guards that must pass before a backend writes
// anything under a [Task.StateDir] root, and returns the error that must abort
// the node when one fails.
//
// The guards are keyed on the StateLocation the caller was HANDED, never on a
// re-derived one: every defect found in this area so far came from a caller
// recomputing containment and disagreeing with StateDir.
//
//   - plantable root (in-checkout or on the shared mount) — refuse a symlinked
//     leaf. Someone other than the operator could have put it there, and both
//     os.MkdirAll and the backends follow one.
//   - in-checkout root — additionally refuse a symlink at ANY component below
//     the workspace (a symlinked ancestor redirects the whole root while the
//     leaf still looks plain), and make the workspace's `.iterion` unstageable
//     so a campaign agent's `git add -A` cannot land our files on the
//     operator's branch.
//
// subject names, in the operator's terms, what would be written through a
// hijacked path — it is the difference between a message they can act on and
// "refusing to run".
func PrepareStateRoot(task Task, root string, loc StateLocation, backend, subject string, logger *iterlog.Logger) error {
	if loc.Plantable() {
		if err := guardStateRootLeaf(root, backend, subject); err != nil {
			return err
		}
	}
	if loc == StateInCheckout {
		if err := refuseSymlinkedPath(task.WorkDir, root, backend, subject); err != nil {
			return err
		}
		hideWorkspaceStateDir(task.WorkDir, logger, backend, subject)
	}
	return nil
}

// guardStateRootLeaf refuses to write under root when root ITSELF is a symlink
// the target repository (or another run sharing the host_state mount) supplied.
//
// .gitignore does not stop a TRACKED symlink from being checked out, and both
// os.MkdirAll and the backends follow one — so a repo could redirect whatever
// the backend writes to a host path of its choosing, creating directories
// along the way.
//
// Only the LEAF is refused, not a symlinked `.iterion` above it. That asymmetry
// is deliberate: the leaf is a directory iterion creates and names, so a
// symlink there is never the operator's doing — whereas `.iterion` IS theirs.
// Without `worktree: auto`, WorkDir is the operator's own repo root and
// `.iterion` is the conventional store dir, which they may legitimately have
// pointed at another volume; refusing that would fail working setups to close a
// narrower hole. Callers that need the ancestors covered pass through
// PrepareStateRoot, which walks them for an in-checkout root.
//
// The residue is stated rather than hidden: a repo that commits `.iterion`
// ITSELF as a symlink is not caught here, because at that point it is
// impersonating the operator's own store convention and the two are
// indistinguishable from this side.
func guardStateRootLeaf(root, backend, subject string) error {
	if root == "" {
		return nil
	}
	info, err := os.Lstat(root)
	if err != nil {
		return nil // absent, or unreadable for a reason MkdirAll will report
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s: refusing to run: %s is a symlink, and the workspace is a "+
			"checkout of the target repository — %s would be written wherever it points",
			backend, root, subject)
	}
	return nil
}

// hideWorkspaceStateDir makes the workspace's `.iterion` invisible to the
// target repo's git, by dropping a self-ignoring .gitignore the way devbox does
// for its generated profile.
//
// Without it, everything a backend writes there is an untracked file inside the
// worktree, so it makes workdirIsClean false and rides finalizeWorktree's
// `git add -A` into a wip-bank commit — meaning the run lands a commit full of
// engine scratch on the operator's branch.
//
// Guarded whenever there is a workspace at all, not only when the state root
// happens to land under it: several backends write into `<WorkDir>/.iterion/`
// unconditionally, so keying on the resolved root leaves those unguarded.
//
// Failure only warns. Being unable to hide the directory is not a reason to
// abort a node — the operator sees the warning and the worst case is scratch in
// a commit, which is recoverable; refusing to run is not.
func hideWorkspaceStateDir(workDir string, logger *iterlog.Logger, backend, subject string) {
	if workDir == "" {
		return
	}
	root := filepath.Join(workDir, ".iterion")
	warn := func(format string, args ...any) {
		if logger != nil {
			logger.Warn(backend+": "+format+" — %s may ride a `git add -A`", append(args, subject)...)
		}
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		warn("cannot create %s: %v", root, err)
		return
	}
	// `*` also ignores this file, so nothing under .iterion/ is ever staged.
	guard := filepath.Join(root, ".gitignore")
	// Lstat, never a FOLLOWING Stat. A repo can ship `.iterion/.gitignore` as a
	// tracked symlink, and following it fails both ways: a DANGLING link makes
	// os.WriteFile create an attacker-chosen host file, and a link to any
	// existing path makes the Lstat below succeed so this returns as if the
	// workspace were guarded — silently leaving the backend's scratch stageable
	// by a campaign agent's `git add -A`.
	//
	// The write policy deliberately differs from piWriteIgnoreGuard's: this path
	// can be the OPERATOR's own store dir, where appending `*` would re-ignore
	// everything their rules had negated (last match wins). Their file is left
	// exactly as it is.
	if err := refuseNonRegular(guard); err != nil {
		warn("%v", err)
		return
	}
	if _, err := os.Lstat(guard); err == nil {
		return // an operator's own guard: never overwrite
	}
	if err := os.WriteFile(guard, []byte("*\n"), 0o644); err != nil {
		warn("cannot write %s: %v", guard, err)
	}
}
