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
	if _, _, err := runGit(wtPath, "update-index", "--refresh"); err != nil {
		// --refresh exits non-zero when files need updating — that's
		// the signal to fall through to the full snapshot path; we
		// don't propagate the error.
		_ = err
	}
	clean, untracked, dirtyErr := worktreeStateClean(wtPath)
	if dirtyErr == nil && clean && !untracked {
		return "", nil
	}
	// Stage EVERY tracked + untracked change — tree noise included, on
	// purpose (#1464): a turn snapshot exists to replay the worktree AS IT
	// WAS, and a rewind that came back without the mirror or the drifted
	// lock would not be a rewind. The gates that exclude tree noise judge
	// the PASS's work; the snapshot preserves the world. Reverted at the
	// end so the engine's index stays as it was before the snapshot pass.
	if out, err := runGitCombined(wtPath, "add", "-A"); err != nil {
		return "", fmt.Errorf("snapshot: git add -A: %w\noutput: %s", err, out)
	}
	defer func() { _, _ = runGitCombined(wtPath, "reset", "--mixed", "HEAD") }()
	treeOut, treeStderr, err := runGit(wtPath, "write-tree")
	if err != nil {
		return "", fmt.Errorf("snapshot: git write-tree: %w\nstderr: %s", err, treeStderr)
	}
	tree := strings.TrimSpace(treeOut)
	if tree == "" {
		return "", fmt.Errorf("snapshot: write-tree returned empty SHA")
	}
	parentOut, parentStderr, err := runGit(wtPath, "rev-parse", "HEAD")
	if err != nil {
		return "", fmt.Errorf("snapshot: git rev-parse HEAD: %w\nstderr: %s", err, parentStderr)
	}
	parent := strings.TrimSpace(parentOut)
	// Belt-and-braces: if the worktree was dirty only because of mode
	// changes that fold away into the same tree SHA, skip the commit.
	if headTreeOut, _, headErr := runGit(wtPath, "rev-parse", "HEAD^{tree}"); headErr == nil && strings.TrimSpace(headTreeOut) == tree {
		return "", nil
	}
	commitOut, commitStderr, err := runGit(wtPath, "commit-tree", tree, "-p", parent, "-m", "iterion turn snapshot "+ref)
	if err != nil {
		return "", fmt.Errorf("snapshot: git commit-tree: %w\nstderr: %s", err, commitStderr)
	}
	commit := strings.TrimSpace(commitOut)
	if commit == "" {
		return "", fmt.Errorf("snapshot: commit-tree returned empty SHA")
	}
	if out, err := runGitCombined(wtPath, "update-ref", ref, commit); err != nil {
		return "", fmt.Errorf("snapshot: git update-ref %s: %w\noutput: %s", ref, err, out)
	}
	return commit, nil
}

// SnapshotWorktree is the exported form of snapshotWorktree for callers
// outside the engine needing the exact same capture semantics. The
// in-place rewind uses it to bank a worktree's uncommitted state before
// reverting, so the revert loses nothing. Exported rather than
// reimplemented because the plumbing is subtle (index restoration, the
// two distinct no-op cases) and a divergent copy would rot.
func SnapshotWorktree(wtPath, ref string) (string, error) {
	return snapshotWorktree(wtPath, ref)
}

// RunGitIn runs a git command in a worktree on behalf of an out-of-engine
// caller (the rewind's revert), reusing gitCmd's LC_ALL=C + process-group
// detach so a `watchexec -r` SIGTERM cannot interrupt a ref update.
//
// It returns the COMBINED stdout+stderr, which is the stream a diagnostic
// wants and the wrong one for a parse: a caller reading a value out of the
// answer reads git's warnings with it (runGit's doc states the mechanism).
// One caller parses anyway — pkg/runview's nextRewindBackupSeq counts
// rewind backups out of a for-each-ref listing — and that package carries
// the remaining sweep of this class (#2781).
func RunGitIn(wtPath string, args ...string) (string, error) {
	return runGitCombined(wtPath, args...)
}

// worktreeStateClean reports whether the worktree at wtPath has no
// tracked changes from HEAD and no untracked files. Used as the fast
// path in snapshotWorktree to skip the `git add -A` index walk when
// nothing happened in the worktree. Implementation: one
// `git status --porcelain` invocation, parsed line-by-line. `diff-index
// --quiet HEAD --` would suffice for tracked changes but misses
// untracked files, which a snapshot does want to capture.
func worktreeStateClean(wtPath string) (clean, hasUntracked bool, err error) {
	out, stderr, err := runGit(wtPath, "status", "--porcelain")
	if err != nil {
		return false, false, fmt.Errorf("git status --porcelain: %w\nstderr: %s", err, stderr)
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

// runGit invokes `git -C <wtPath> <args...>` and returns its STDOUT alone,
// git's stderr beside it, and the error.
//
// The callers that read the bytes PARSE them — porcelain records split on
// NUL, a refname list, a SHA or a path under TrimSpace — and git writes
// warnings, hints and advice on stderr whenever the environment gives it a
// reason to: a config file it cannot read, a broken ref under the prefix it
// was asked to list, an ambiguous refname. Spliced into the answer, such a
// line is a modified path in a clean worktree or a gate number nobody
// wrote, and the caller renders its verdict on the machine instead of on
// the repository. The stderr return is what the caller's message quotes, so
// git's own reason stays readable once it no longer reaches the parser.
//
// runGitCombined is the form for a caller whose only use of the bytes is a
// diagnostic. Both reuse gitCmd from worktree.go so LC_ALL=C +
// process-group detach apply (git's English diagnostics are what the
// substring matchers downstream read, and a `watchexec -r` SIGTERM could
// otherwise interrupt an in-flight update-ref).
func runGit(wtPath string, args ...string) (stdout string, stderr string, err error) {
	cmd, cancel := gitCmd(append([]string{"-C", wtPath}, args...)...)
	defer cancel()
	var errOut strings.Builder
	cmd.Stderr = &errOut
	out, err := cmd.Output()
	return string(out), strings.TrimSpace(errOut.String()), err
}

// runGitCombined invokes `git -C <wtPath> <args...>` and returns its
// combined stdout+stderr plus the error, for a caller whose only use of the
// bytes is a message — `git add`, `git reset`, `git update-ref`, `git
// worktree add`: there the whole stream IS the diagnostic and nothing
// parses it. A caller that reads a value out of the answer uses runGit.
func runGitCombined(wtPath string, args ...string) (string, error) {
	cmd, cancel := gitCmd(append([]string{"-C", wtPath}, args...)...)
	defer cancel()
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// nodeSnapshotRef builds ref names in pkg/store so the Fork API can
// share them without importing pkg/runtime. Aliased here so the
// engine's call sites read naturally.
var nodeSnapshotRef = store.NodeSnapshotRef
