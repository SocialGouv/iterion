package runtime

import (
	"fmt"
	"strings"

	"github.com/SocialGouv/iterion/pkg/store"
)

// snapshotWorktree writes a git ref of the form
// `refs/iterion/runs/<runID>/turns/<nodeID>/<loopIter>/<turn>` pointing
// at a snapshot of `wtPath`'s current working-tree contents. The
// snapshot is implemented as an orphan-style commit-tree (HEAD is its
// parent so the tree stays reachable by garbage-collection rules) but
// the worktree's HEAD and index are NOT modified — the function
// stages, builds a tree, builds a commit, points the ref, then
// un-stages, leaving the worktree in the exact state it was in.
//
// Returns the snapshot's commit SHA. Empty SHA + nil error means the
// worktree had no tracked changes from HEAD AND no untracked files —
// the caller may skip persisting GitRef in that case.
//
// Why orphan-style + a namespaced ref (not `git tag`):
//   - tags pollute `git tag -l` and conflict with user-authored tags;
//   - the namespaced ref family `refs/iterion/runs/<run>/turns/...`
//     is easy to enumerate (`git for-each-ref refs/iterion/runs/<run>/`)
//     and easy to GC en-masse on run finalize + N days;
//   - HEAD-parented commits stay reachable for GC purposes even when
//     the worktree later resets to a different commit.
//
// The implementation assumes git ≥ 2.20 (which all devbox + production
// images ship). Errors from individual git invocations are wrapped
// with the command name for diagnostic clarity.
func snapshotWorktree(wtPath, ref string) (string, error) {
	if wtPath == "" {
		return "", fmt.Errorf("snapshot: empty worktree path")
	}
	if ref == "" {
		return "", fmt.Errorf("snapshot: empty ref name")
	}
	// Fast no-op path: most node boundaries leave the worktree clean
	// (judges, routers, read-only agents). `git diff-index --quiet`
	// exits 0 when the index + worktree both match HEAD, non-zero
	// otherwise. Avoiding `git add -A` here skips an O(filecount)
	// index walk on every clean node finish.
	if _, err := runGit(wtPath, "update-index", "--refresh"); err != nil {
		// --refresh exits non-zero when files need updating — that's
		// the signal to fall through to the full snapshot path; we
		// don't propagate the error.
		_ = err
	}
	clean, untracked, dirtyErr := worktreeStateClean(wtPath)
	if dirtyErr == nil && clean && !untracked {
		return "", nil
	}
	// Stage every tracked + untracked change. Reverted at the end so
	// the engine's index stays as it was before the snapshot pass.
	if out, err := runGit(wtPath, "add", "-A"); err != nil {
		return "", fmt.Errorf("snapshot: git add -A: %w\noutput: %s", err, out)
	}
	defer func() { _, _ = runGit(wtPath, "reset", "--mixed", "HEAD") }()
	treeOut, err := runGit(wtPath, "write-tree")
	if err != nil {
		return "", fmt.Errorf("snapshot: git write-tree: %w\noutput: %s", err, treeOut)
	}
	tree := strings.TrimSpace(treeOut)
	if tree == "" {
		return "", fmt.Errorf("snapshot: write-tree returned empty SHA")
	}
	parentOut, err := runGit(wtPath, "rev-parse", "HEAD")
	if err != nil {
		return "", fmt.Errorf("snapshot: git rev-parse HEAD: %w\noutput: %s", err, parentOut)
	}
	parent := strings.TrimSpace(parentOut)
	// Belt-and-braces: if the worktree was dirty only because of mode
	// changes that fold away into the same tree SHA, skip the commit.
	if headTreeOut, headErr := runGit(wtPath, "rev-parse", "HEAD^{tree}"); headErr == nil && strings.TrimSpace(headTreeOut) == tree {
		return "", nil
	}
	commitOut, err := runGit(wtPath, "commit-tree", tree, "-p", parent, "-m", "iterion turn snapshot "+ref)
	if err != nil {
		return "", fmt.Errorf("snapshot: git commit-tree: %w\noutput: %s", err, commitOut)
	}
	commit := strings.TrimSpace(commitOut)
	if commit == "" {
		return "", fmt.Errorf("snapshot: commit-tree returned empty SHA")
	}
	if out, err := runGit(wtPath, "update-ref", ref, commit); err != nil {
		return "", fmt.Errorf("snapshot: git update-ref %s: %w\noutput: %s", ref, err, out)
	}
	return commit, nil
}

// worktreeStateClean reports whether the worktree at wtPath has no
// tracked changes from HEAD and no untracked files. Used as the fast
// path in snapshotWorktree to skip the `git add -A` index walk when
// nothing happened in the worktree. Implementation: one
// `git status --porcelain` invocation, parsed line-by-line. `diff-index
// --quiet HEAD --` would suffice for tracked changes but misses
// untracked files, which a snapshot does want to capture.
func worktreeStateClean(wtPath string) (clean, hasUntracked bool, err error) {
	out, err := runGit(wtPath, "status", "--porcelain")
	if err != nil {
		return false, false, err
	}
	out = strings.TrimSpace(out)
	if out == "" {
		return true, false, nil
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "??") {
			hasUntracked = true
		}
	}
	return false, hasUntracked, nil
}

// runGit invokes `git -C <wtPath> <args...>` and returns its combined
// stdout+stderr plus the error. Reuses gitCmd from worktree.go so
// LC_ALL=C + process-group detach apply (otherwise locale-dependent
// stderr would break the error-substring matching downstream, and a
// `watchexec -r` SIGTERM could interrupt an in-flight update-ref).
func runGit(wtPath string, args ...string) (string, error) {
	out, err := gitCmd(append([]string{"-C", wtPath}, args...)...).CombinedOutput()
	return string(out), err
}

// Ref builders live in pkg/store so the Fork API can share them
// without importing pkg/runtime. Aliased here so the engine's call
// sites read naturally.
var (
	nodeSnapshotRef = store.NodeSnapshotRef
	turnSnapshotRef = store.TurnSnapshotRef
)

// listIterionRefs enumerates every ref under `refs/iterion/runs/<runID>/`,
// returning their full ref names. Empty slice (no error) when the
// namespace is empty for this run.
func listIterionRefs(wtPath, runID string) ([]string, error) {
	out, err := runGit(wtPath, "for-each-ref", "--format=%(refname)", "refs/iterion/runs/"+runID+"/")
	if err != nil {
		return nil, fmt.Errorf("list iterion refs: %w\noutput: %s", err, out)
	}
	var refs []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			refs = append(refs, line)
		}
	}
	return refs, nil
}

// deleteIterionRefs removes every ref under `refs/iterion/runs/<runID>/`.
// Used by the per-run GC pass run by the runview service janitor (Phase
// 5). Safe to call on a run that never wrote any snapshots — git
// silently no-ops on unknown refs.
func deleteIterionRefs(wtPath, runID string) error {
	refs, err := listIterionRefs(wtPath, runID)
	if err != nil {
		return err
	}
	for _, r := range refs {
		if out, err := runGit(wtPath, "update-ref", "-d", r); err != nil {
			return fmt.Errorf("delete %s: %w\noutput: %s", r, err, out)
		}
	}
	return nil
}
