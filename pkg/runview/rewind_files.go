package runview

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
)

// fileRevertResult reports what the workspace half of a rewind did.
type fileRevertResult struct {
	Reverted     bool   `json:"reverted"`
	Ref          string `json:"ref,omitempty"`
	RevertCommit string `json:"revert_commit,omitempty"`
	BackupRef    string `json:"backup_ref,omitempty"`
	SkipReason   string `json:"skip_reason,omitempty"`
}

// revertWorkspace restores the files a run's workspace held just BEFORE
// the pivot executed, so a replayed node meets its prior conditions
// rather than its own previous production.
//
// This matters most for the nodes whose real product is NOT their output
// map: a docs or code bot writes dozens of files and returns a summary.
// Rewinding only the checkpoint would leave the half-written tree in
// place and the replayed node would build on top of itself.
//
// Revert, not reset: the prior tree is committed ON TOP of HEAD, so the
// run's committed history is preserved and readable, and the uncommitted
// state at the instant of the rewind is banked under a backup ref first.
// Nothing the run ever had becomes unreachable.
//
// Non-fatal by design when there is nothing to revert TO (a run with no
// worktree, or a node with no recorded pre-boundary): the engine-state
// rewind is still worth having, and the caller surfaces SkipReason so the
// gap is loud rather than silent. A git command that FAILS is fatal —
// "cannot" and "broke" are different answers.
func revertWorkspace(run *store.Run, wf *ir.Workflow, cp *store.Checkpoint, pivot string) (*fileRevertResult, error) {
	if !run.Worktree || run.WorkDir == "" {
		return &fileRevertResult{SkipReason: "run has no isolated worktree — its workspace is the live tree, which iterion does not snapshot"}, nil
	}
	ref, skip := findPreNodeRef(run, wf, cp, pivot)
	if skip != "" {
		return &fileRevertResult{SkipReason: skip}, nil
	}

	// Bank the current state — including uncommitted and untracked work —
	// before touching anything, so the revert destroys nothing.
	backupRef := store.RewindBackupRef(run.ID, pivot, nextRewindBackupSeq(run.WorkDir, run.ID, pivot))
	banked, err := runtime.SnapshotWorktree(run.WorkDir, backupRef)
	if err != nil {
		return nil, fmt.Errorf("bank the current workspace before reverting: %w", err)
	}
	if banked == "" {
		// The tree already matched HEAD, so SnapshotWorktree wrote no ref.
		// Reporting the name anyway hands the operator a recovery hint
		// that resolves to nothing — and leaves nextRewindBackupSeq
		// handing out the same number again.
		backupRef = ""
	}

	// Index + worktree := the pre-node tree. HEAD is untouched, so the
	// commit below lands as a normal child of the current history.
	if out, err := runtime.RunGitIn(run.WorkDir, "read-tree", "-u", "--reset", ref); err != nil {
		return nil, fmt.Errorf("git read-tree %s: %w\noutput: %s", ref, err, out)
	}
	// Remove files created after the snapshot. No -x, so gitignored build
	// output survives — `git add -A` honours .gitignore, so ignored paths
	// were never in the snapshot to restore in the first place.
	if out, err := runtime.RunGitIn(run.WorkDir, "clean", "-fd"); err != nil {
		return nil, fmt.Errorf("git clean -fd: %w\noutput: %s", err, out)
	}

	tree, err := gitOut(run.WorkDir, "write-tree")
	if err != nil {
		return nil, err
	}
	headTree, err := gitOut(run.WorkDir, "rev-parse", "HEAD^{tree}")
	if err != nil {
		return nil, err
	}
	res := &fileRevertResult{Reverted: true, Ref: ref, BackupRef: backupRef}
	if tree == headTree {
		// The committed history already matches the pre-node state; the
		// worktree is restored and there is nothing to record.
		return res, nil
	}
	head, err := gitOut(run.WorkDir, "rev-parse", "HEAD")
	if err != nil {
		return nil, err
	}
	commit, err := gitOut(run.WorkDir, "commit-tree", tree, "-p", head,
		"-m", fmt.Sprintf("iterion: rewind to %q (run %s)", pivot, run.ID))
	if err != nil {
		return nil, err
	}
	if out, err := runtime.RunGitIn(run.WorkDir, "update-ref", "HEAD", commit); err != nil {
		return nil, fmt.Errorf("git update-ref HEAD %s: %w\noutput: %s", commit, err, out)
	}
	res.RevertCommit = commit
	return res, nil
}

// findPreNodeRef locates the pivot's pre-execution snapshot, probing
// downwards from the checkpoint's loop iteration: the counters record
// where the run STOPPED, which can be past the last iteration the pivot
// actually ran.
// A non-empty skipReason means "do not revert" — never a silent
// fallback to an older iteration.
//
// Probing DOWNWARDS is legitimate: the loop counters record where the run
// STOPPED, which can be past the last iteration the pivot itself ran, so
// the highest existing pre-marker at or below that is the right anchor.
// What is NOT legitimate is landing below an iteration the pivot actually
// executed. There the tree predates work whose outputs downstreamOf
// deliberately keeps (loop-mates are ancestors of the pivot), so the
// checkpoint would claim work whose files had been reverted away — and
// the old code reported that as Reverted=true with no warning.
//
// The closing ref is what tells the two apart: `nodes/<pivot>/<n>` exists
// iff the pivot completed iteration n. If one exists above the pre-marker
// we settled on, the anchor is stale and we refuse.
//
// Two ordinary paths reach that: a workflow whose Loop.Body is empty
// (loopIterationOf answered 0 for every node — now fixed by its
// edge-endpoint fallback), and a run resumed from a human pause, where
// the engine's markPreNodeBoundary is gated on rs.isWorktree, which that
// path never sets — so nothing executed after the resume has a marker.
//
// Restoring the wrong tree is worse than restoring none: a SkipReason is
// visible, a stale revert is not.
func findPreNodeRef(run *store.Run, wf *ir.Workflow, cp *store.Checkpoint, pivot string) (ref string, skipReason string) {
	want := loopIterationOf(wf, pivot, cp.LoopCounters)
	found := -1
	for iter := want; iter >= 0; iter-- {
		candidate := store.NodePreSnapshotRef(run.ID, pivot, iter)
		if _, err := runtime.RunGitIn(run.WorkDir, "rev-parse", "--verify", "--quiet", candidate+"^{commit}"); err == nil {
			ref, found = candidate, iter
			break
		}
	}
	if found < 0 {
		return "", fmt.Sprintf(
			"no pre-execution snapshot recorded for %q (run predates the pre-boundary marker, or the node ran outside the worktree)", pivot)
	}
	for iter := want; iter > found; iter-- {
		post := store.NodeSnapshotRef(run.ID, pivot, iter)
		if _, err := runtime.RunGitIn(run.WorkDir, "rev-parse", "--verify", "--quiet", post+"^{commit}"); err == nil {
			return "", fmt.Sprintf(
				"%q completed iteration %d but only iteration %d has a pre-execution snapshot — "+
					"reverting to it would discard work the checkpoint still claims, so the workspace was left as-is "+
					"(rewind with --keep-files to skip the workspace deliberately)",
				pivot, iter, found)
		}
	}
	return ref, ""
}

// loopIterationOf mirrors the engine's currentLoopIteration (unexported
// in pkg/runtime): the max counter across every loop whose body contains
// the node, falling back to loop-edge endpoints.
//
// The edge-endpoint half is not decoration. ir.Compile does not always
// populate Loop.Body (older IRs, hand-written fixtures), and without the
// fallback this answered 0 for every node in such a workflow — so the
// pre-snapshot probe never looked above iteration 0 and a rewind of a
// loop's third pass restored the tree from before its first.
func loopIterationOf(wf *ir.Workflow, nodeID string, counters map[string]int) int {
	iter := 0
	for name, loop := range wf.Loops {
		if loop == nil || !loop.Body[nodeID] {
			continue
		}
		if n := counters[name]; n > iter {
			iter = n
		}
	}
	for _, edge := range wf.Edges {
		if edge == nil || edge.LoopName == "" {
			continue
		}
		if edge.From != nodeID && edge.To != nodeID {
			continue
		}
		if n := counters[edge.LoopName]; n > iter {
			iter = n
		}
	}
	return iter
}

// nextRewindBackupSeq counts existing backups for this (run, node) so
// repeated rewinds each keep their own rather than overwriting.
func nextRewindBackupSeq(workDir, runID, nodeID string) int {
	prefix := store.RewindBackupRef(runID, nodeID, 0)
	prefix = strings.TrimSuffix(prefix, "0")
	out, err := runtime.RunGitIn(workDir, "for-each-ref", "--format=%(refname)", prefix)
	if err != nil {
		return 0
	}
	max := -1
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if n, cerr := strconv.Atoi(line[strings.LastIndex(line, "/")+1:]); cerr == nil && n > max {
			max = n
		}
	}
	return max + 1
}

func gitOut(workDir string, args ...string) (string, error) {
	out, err := runtime.RunGitIn(workDir, args...)
	if err != nil {
		return "", fmt.Errorf("git %s: %w\noutput: %s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(out), nil
}
