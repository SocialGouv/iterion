package runview

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/workspacetrack"
)

// fileRevertResult reports what the workspace half of a rewind did.
type fileRevertResult struct {
	Reverted     bool   `json:"reverted"`
	Ref          string `json:"ref,omitempty"`
	RevertCommit string `json:"revert_commit,omitempty"`
	BackupRef    string `json:"backup_ref,omitempty"`
	SkipReason   string `json:"skip_reason,omitempty"`
	// Restored carries the tracker's per-file accounting (written /
	// deleted / unchanged / skipped) when the restore went through
	// iterion's own versioning rather than git.
	Restored *workspacetrack.RestoreReport `json:"restored,omitempty"`
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
func (s *Service) revertWorkspace(run *store.Run, wf *ir.Workflow, cp *store.Checkpoint, pivot, sourcePath string) (*fileRevertResult, error) {
	if run.WorkDir == "" {
		return &fileRevertResult{SkipReason: "run has no recorded workspace"}, nil
	}
	if !run.Worktree {
		// No isolated worktree: iterion's own versioning is the only
		// mechanism that applies (git would stage the operator's work).
		return s.revertViaTracker(run, wf, cp, pivot, sourcePath)
	}
	ref, ok := findPreNodeRef(run, wf, cp, pivot)
	if !ok {
		// Fall back to iterion's own versioning: a worktree run may still
		// have been captured by the tracker (e.g. the git ref was never
		// written because the boundary predates the marker).
		if res, terr := s.revertViaTracker(run, wf, cp, pivot, sourcePath); terr == nil && res.Reverted {
			return res, nil
		}
		return &fileRevertResult{SkipReason: fmt.Sprintf(
			"no pre-execution snapshot recorded for %q (run predates the pre-boundary marker, or the node ran outside the worktree)", pivot)}, nil
	}

	// Bank the current state — including uncommitted and untracked work —
	// before touching anything, so the revert destroys nothing.
	backupRef := store.RewindBackupRef(run.ID, pivot, nextRewindBackupSeq(run.WorkDir, run.ID, pivot))
	if _, err := runtime.SnapshotWorktree(run.WorkDir, backupRef); err != nil {
		return nil, fmt.Errorf("bank the current workspace before reverting: %w", err)
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
func findPreNodeRef(run *store.Run, wf *ir.Workflow, cp *store.Checkpoint, pivot string) (string, bool) {
	for iter := loopIterationOf(wf, pivot, cp.LoopCounters); iter >= 0; iter-- {
		ref := store.NodePreSnapshotRef(run.ID, pivot, iter)
		if _, err := runtime.RunGitIn(run.WorkDir, "rev-parse", "--verify", "--quiet", ref+"^{commit}"); err == nil {
			return ref, true
		}
	}
	return "", false
}

// loopIterationOf mirrors the engine's currentLoopIteration (unexported
// in pkg/runtime): the max counter across every loop whose body contains
// the node.
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

// revertViaTracker restores the workspace through iterion's own
// versioning — the path that covers a run with no isolated worktree,
// which is the default shape and the majority of the catalog.
func (s *Service) revertViaTracker(run *store.Run, wf *ir.Workflow, cp *store.Checkpoint, pivot, sourcePath string) (*fileRevertResult, error) {
	if s.workspaceTracker == nil {
		return &fileRevertResult{SkipReason: "workspace versioning is not enabled on this store"}, nil
	}
	snapshotID, ok := findPreNodeSnapshot(s.workspaceTracker, run, wf, cp, pivot)
	if !ok {
		// Distinguish the three real causes: the operator can act on the
		// first two, and conflating them sent people hunting for a
		// non-existent "old run" problem.
		if s.workspaceTracker.Head(run.ID) == "" {
			return &fileRevertResult{SkipReason: "this run captured no workspace snapshots at all — it was launched on a path that does not enable workspace versioning, or versioning was off"}, nil
		}
		return &fileRevertResult{SkipReason: fmt.Sprintf(
			"the run has workspace snapshots but none recorded before %q — that node kind does not mark a pre-execution boundary", pivot)}, nil
	}
	// Bank the current state first, so the restore destroys nothing: the
	// pre-rewind workspace stays a resolvable snapshot.
	backup, err := s.workspaceTracker.Capture(run.ID, run.WorkDir, "rewind-backup:"+pivot)
	if err != nil {
		return nil, fmt.Errorf("bank the current workspace before reverting: %w", err)
	}
	// The workflow source is protected: a rewind exists to test an edit to
	// it, so restoring the workspace must not revert that edit — the
	// following `resume --force` would then recompile the OLD workflow and
	// silently test nothing. Bites only when the .bot lives inside the
	// workspace, which is the self-hosted dogfood shape.
	report, err := s.workspaceTracker.Restore(run.ID, run.WorkDir, snapshotID, sourcePath, run.FilePath)
	if err != nil {
		return nil, fmt.Errorf("restore workspace to %s: %w", snapshotID, err)
	}
	res := &fileRevertResult{
		Reverted:  true,
		Ref:       snapshotID,
		BackupRef: backup.ID,
		Restored:  report,
	}
	if len(report.Skipped) > 0 {
		res.SkipReason = fmt.Sprintf("%d path(s) were too large to version and were left as-is", len(report.Skipped))
	}
	return res, nil
}

// findPreNodeSnapshot resolves the pivot's pre-execution capture,
// probing downwards from the checkpoint's loop iteration: the counters
// record where the run STOPPED, which can be past the last iteration the
// pivot actually ran.
func findPreNodeSnapshot(tr workspacetrack.Tracker, run *store.Run, wf *ir.Workflow, cp *store.Checkpoint, pivot string) (string, bool) {
	for iter := loopIterationOf(wf, pivot, cp.LoopCounters); iter >= 0; iter-- {
		if id, ok := tr.Resolve(run.ID, workspacetrack.Label(workspacetrack.PhasePre, pivot, iter)); ok {
			return id, true
		}
	}
	return "", false
}

// RestoreWorkspaceSnapshot puts a run's workspace back to a snapshot the
// tracker holds — the consuming half of the backup a rewind banks before
// it restores.
//
// Without it the safety net was theoretical: `revertViaTracker` captured
// the pre-rewind workspace and printed the id, but nothing could act on
// it, so an operator whose own edits were swept up by a restore had the
// bytes on disk and no way to reach them.
func (s *Service) RestoreWorkspaceSnapshot(ctx context.Context, runID, snapshotID string) (*workspacetrack.RestoreReport, error) {
	if s.workspaceTracker == nil {
		return nil, fmt.Errorf("runview: workspace versioning is not enabled on this store")
	}
	run, err := s.store.LoadRun(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("load run: %w", err)
	}
	if run.WorkDir == "" {
		return nil, fmt.Errorf("runview: run %s has no recorded workspace", runID)
	}
	if isRewindableStatus(run.Status) || run.Status == store.RunStatusFinished || run.Status == store.RunStatusFailed {
		return s.workspaceTracker.Restore(run.ID, run.WorkDir, snapshotID)
	}
	return nil, fmt.Errorf("%w: %s — stop the run before restoring its workspace", ErrRewindNotRewindable, run.Status)
}

// ListWorkspaceSnapshots walks a run's capture chain, newest first, so an
// operator can see what states are recoverable.
func (s *Service) ListWorkspaceSnapshots(runID string) ([]*workspacetrack.Snapshot, error) {
	if s.workspaceTracker == nil {
		return nil, fmt.Errorf("runview: workspace versioning is not enabled on this store")
	}
	var out []*workspacetrack.Snapshot
	for id := s.workspaceTracker.Head(runID); id != ""; {
		snap, err := s.workspaceTracker.Load(runID, id)
		if err != nil {
			break
		}
		out = append(out, snap)
		id = snap.Parent
	}
	return out, nil
}
