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
	// CoverageGap describes files the revert could NOT put back even
	// though it ran (paths the capture never stored). Distinct from
	// SkipReason, which means the workspace was not touched at all — the
	// CLI prints that one only when Reverted is false, so folding a
	// partial-coverage warning into it dropped the warning on the single
	// path where both are true.
	CoverageGap string `json:"coverage_gap,omitempty"`
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
	ref, skip := findPreNodeRef(run, wf, cp, pivot)
	if skip != "" {
		// Fall back to iterion's own versioning: a worktree run may still
		// have been captured by the tracker (e.g. the git ref was never
		// written because the boundary predates the marker). Its lookup is
		// independent, so it can succeed where git's refs are missing —
		// and it carries the same staleness guard, so the fallback cannot
		// smuggle in the older-iteration revert the git path just refused.
		res, terr := s.revertViaTracker(run, wf, cp, pivot, sourcePath)
		if terr != nil {
			// A tracker restore that FAILED is not "nothing happened": its
			// deletion pass runs to completion before the write-back, so
			// files may already be gone. Falling through to the git skip
			// reason would hand the operator text that literally says the
			// workspace was left as-is while it has in fact been mutated.
			// "Could not attempt" and "attempted and broke" are different
			// answers — the non-worktree path above already propagates.
			return nil, terr
		}
		if res.Reverted {
			return res, nil
		}
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

// revertViaTracker restores the workspace through iterion's own
// versioning — the path that covers a run with no isolated worktree,
// which is the default shape and the majority of the catalog.
func (s *Service) revertViaTracker(run *store.Run, wf *ir.Workflow, cp *store.Checkpoint, pivot, sourcePath string) (*fileRevertResult, error) {
	if s.workspaceTracker == nil {
		return &fileRevertResult{SkipReason: "workspace versioning is not enabled on this store"}, nil
	}
	snapshotID, ok := findPreNodeSnapshot(s.workspaceTracker, run, wf, cp, pivot)
	if !ok {
		// Distinguish the real causes: the operator can act on most of
		// them, and conflating them sent people hunting for a non-existent
		// "old run" problem.
		if s.workspaceTracker.Head(run.ID) == "" {
			// A workspace over the file cap produces no snapshots either,
			// but for an entirely different reason and with a different
			// fix (raise the cap, or narrow the workspace with
			// .iterionignore). Reporting it as "versioning was off" is the
			// wrong-problem hunt this branch set out to remove.
			if over, ok := s.workspaceTracker.(interface{ Overflowed(string) bool }); ok && over.Overflowed(run.ID) {
				// Deliberately no "raise the cap": MaxFiles has no env or
				// flag override, and the overflow latches in this run's
				// index — so neither half of the old advice could rescue
				// the run being rewound. Say what is actually true.
				return &fileRevertResult{SkipReason: fmt.Sprintf(
					"this run's workspace exceeded the %d-file cap, so nothing was versioned for it — "+
						"narrow the workspace with .iterionignore and relaunch; this run cannot be recovered",
					workspacetrack.DefaultMaxFiles)}, nil
			}
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
		res.CoverageGap = fmt.Sprintf(
			"%d path(s) were never captured (too large, or unreadable at capture time) and were left as-is",
			len(report.Skipped))
	}
	return res, nil
}

// findPreNodeSnapshot resolves the pivot's pre-execution capture,
// probing downwards from the checkpoint's loop iteration: the counters
// record where the run STOPPED, which can be past the last iteration the
// pivot actually ran.
//
// It carries the same staleness guard as findPreNodeRef, and must: this
// is the fallback the git path takes when its own refs are missing, so
// without it the fallback would quietly perform the older-iteration
// revert the git path had just refused. The closing label is the
// discriminator — post:<pivot>:<n> exists iff the pivot completed n, so
// one above the anchor means the anchor predates work the checkpoint
// still keeps.
func findPreNodeSnapshot(tr workspacetrack.Tracker, run *store.Run, wf *ir.Workflow, cp *store.Checkpoint, pivot string) (string, bool) {
	want := loopIterationOf(wf, pivot, cp.LoopCounters)
	id, found := "", -1
	for iter := want; iter >= 0; iter-- {
		if got, ok := tr.Resolve(run.ID, workspacetrack.Label(workspacetrack.PhasePre, pivot, iter)); ok {
			id, found = got, iter
			break
		}
	}
	if found < 0 {
		return "", false
	}
	for iter := want; iter > found; iter-- {
		if _, ok := tr.Resolve(run.ID, workspacetrack.Label(workspacetrack.PhasePost, pivot, iter)); ok {
			return "", false
		}
	}
	return id, true
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
	switch {
	case isRewindableStatus(run.Status):
		// CLAIM before touching the workspace, exactly as Rewind does and
		// for the same reason: reading the status and acting on it leaves
		// a window in which `iterion resume` claims the run, and the
		// restore then deletes and rewrites files under a live engine
		// mid-node. The CAS closes it — losing the race means the run is
		// already `running` and we refuse.
		//
		// Parking it at `cancelled` is not incidental either: resuming a
		// paused run whose workspace was rewritten underneath it is the
		// very hazard here, so the operator should have to resume
		// deliberately. `cancelled` preserves the checkpoint, so that
		// resume costs nothing.
		claimed, cerr := s.store.UpdateRunStatusIf(ctx, run.ID, store.RunStatusCancelled, "", rewindableStatuses)
		if cerr != nil {
			return nil, fmt.Errorf("claim run for restore: %w", cerr)
		}
		if !claimed {
			return nil, fmt.Errorf("%w: status changed under us — reload and retry", ErrRewindNotRewindable)
		}
		return s.restoreBanked(run, snapshotID)
	case run.Status == store.RunStatusFinished || run.Status == store.RunStatusFailed:
		// Terminal and non-resumable: no engine can claim it, so there is
		// nothing to race with.
		return s.restoreBanked(run, snapshotID)
	}
	return nil, fmt.Errorf("%w: %s — stop the run before restoring its workspace", ErrRewindNotRewindable, run.Status)
}

// restoreBanked banks the CURRENT workspace before restoring, so this
// operation is as recoverable as the rewind it undoes.
//
// Rewind captures before it reverts — that bank is the whole premise of
// the --list-snapshots / --restore-snapshot pair. The consuming half went
// straight to Restore, so the documented scenario ("I rewound, kept
// editing, then wanted my pre-rewind work back") destroyed those
// post-rewind edits irreversibly: the deletion pass removes every file
// absent from the target snapshot, and on an in-place run those include
// the operator's own writes. One capture makes the pair symmetric.
func (s *Service) restoreBanked(run *store.Run, snapshotID string) (*workspacetrack.RestoreReport, error) {
	if _, berr := s.workspaceTracker.Capture(run.ID, run.WorkDir, "pre-restore:"+snapshotID); berr != nil {
		return nil, fmt.Errorf("bank the current workspace before restoring: %w", berr)
	}
	return s.workspaceTracker.Restore(run.ID, run.WorkDir, snapshotID)
}

// ListWorkspaceSnapshots walks a run's capture chain, newest first, so an
// operator can see what states are recoverable.
func (s *Service) ListWorkspaceSnapshots(ctx context.Context, runID string) ([]*workspacetrack.Snapshot, error) {
	if s.workspaceTracker == nil {
		return nil, fmt.Errorf("runview: workspace versioning is not enabled on this store")
	}
	// Resolve the run first, as RestoreWorkspaceSnapshot does: the id
	// reaches the tracker's path builders directly, so an unvalidated one
	// would read a manifest outside the run's directory.
	if _, err := s.store.LoadRun(ctx, runID); err != nil {
		return nil, fmt.Errorf("load run: %w", err)
	}
	// `seen` bounds the walk. A manifest is on-disk JSON that Load
	// unmarshals bare — the same untrusted-data posture the restore path
	// takes for Entry.Path — so a Parent pointing at itself, or two
	// pointing at each other (a partially-written file, a hand-edited or
	// hand-copied store), would spin here appending until the process
	// OOMs. Reachable from the CLI with only a run id.
	var out []*workspacetrack.Snapshot
	seen := map[string]bool{}
	for id := s.workspaceTracker.Head(runID); id != "" && !seen[id]; {
		seen[id] = true
		snap, err := s.workspaceTracker.Load(runID, id)
		if err != nil {
			break
		}
		out = append(out, snap)
		id = snap.Parent
	}
	return out, nil
}
