package runview

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/workspacetrack"
)

// FileRevertResult reports what the workspace half of a rewind did.
type FileRevertResult struct {
	Reverted     bool   `json:"reverted"`
	Ref          string `json:"ref,omitempty"`
	RevertCommit string `json:"revert_commit,omitempty"`
	BackupRef    string `json:"backup_ref,omitempty"`
	SkipReason   string `json:"skip_reason,omitempty"`
	// Scope is the restore breadth actually applied ("produced" | "full").
	// Empty for the git/worktree path, which has only one breadth.
	Scope string `json:"scope,omitempty"`
	// ScopeCount is how many workspace paths the scope admitted — the
	// blast radius, stated as a number before any of it is listed.
	ScopeCount int `json:"scope_count,omitempty"`
	// Overwritten names in-scope paths whose content on disk matched
	// NEITHER the state being restored NOR the run's last recorded
	// boundary: work that arrived after the run stopped recording, and
	// that the restore therefore took. Almost always the operator's own.
	// Capped; OverwrittenCount is exact.
	Overwritten      []string `json:"overwritten,omitempty"`
	OverwrittenCount int      `json:"overwritten_count,omitempty"`
	// LeftInPlace names paths that changed since the run's last recorded
	// boundary and were NOT restored, because no execution of this run is
	// recorded as having touched them. Some are the operator's edits;
	// some may be the partial output of a node that died before its
	// boundary was written. iterion cannot tell those apart — which is
	// exactly why it reports them instead of guessing. Capped;
	// LeftInPlaceCount is exact.
	LeftInPlace      []string `json:"left_in_place,omitempty"`
	LeftInPlaceCount int      `json:"left_in_place_count,omitempty"`
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
func (s *Service) revertWorkspace(run *store.Run, wf *ir.Workflow, cp *store.Checkpoint, pivot, sourcePath string, scope RestoreScope) (*FileRevertResult, error) {
	if run.WorkDir == "" {
		return &FileRevertResult{SkipReason: "run has no recorded workspace"}, nil
	}
	if !run.Worktree {
		// No isolated worktree: iterion's own versioning is the only
		// mechanism that applies (git would stage the operator's work).
		//
		// This is also the shape where the workspace is the operator's
		// LIVE CHECKOUT, so the default breadth is `produced` — the
		// restore stays inside what the run is recorded to have changed.
		return s.revertViaTracker(run, wf, cp, pivot, sourcePath, scope.orDefault(RestoreScopeProduced))
	}
	if scope == RestoreScopeProduced {
		// Refused, never silently widened. git is the mechanism for a
		// worktree run and it has exactly one breadth — `read-tree --reset`
		// plus `clean -fd` is the whole tree — so honouring this request is
		// not possible here, and quietly substituting the MAXIMAL blast
		// radius for an explicitly-requested minimal one is the single
		// behaviour this feature must never have.
		//
		// Doing nothing is the safe answer: the operator asked for narrow,
		// and a worktree can still hold work they care about (a post-mortem
		// shell writes into it).
		return &FileRevertResult{Scope: string(scope), SkipReason: fmt.Sprintf(
			"--restore-scope produced is not available for a run with an isolated worktree (%q): git reverts the whole tree or none of it — "+
				"rerun with --restore-scope full to revert it, or none to leave it", pivot)}, nil
	}
	ref, skip := findPreNodeRef(run, wf, cp, pivot)
	if skip != "" {
		// Fall back to iterion's own versioning: a worktree run may still
		// have been captured by the tracker (e.g. the git ref was never
		// written because the boundary predates the marker). Its lookup is
		// independent, so it can succeed where git's refs are missing —
		// and it carries the same staleness guard, so the fallback cannot
		// smuggle in the older-iteration revert the git path just refused.
		//
		// The DEFAULT breadth here is `full`, not `produced`: this run has
		// an isolated worktree, so the workspace is iterion's own and
		// holds no operator work to protect — the reason scoping exists
		// does not apply, while the reason a full revert exists (replaying
		// a node must not meet its own production) still does. An explicit
		// --restore-scope is still honoured.
		//
		// The fallback is not exotic: a worktree run resumed from a human
		// pause writes tracker labels and no git refs, so it lands here
		// every time.
		res, terr := s.revertViaTracker(run, wf, cp, pivot, sourcePath, scope.orDefault(RestoreScopeFull))
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
		return &FileRevertResult{Scope: string(RestoreScopeFull), SkipReason: skip}, nil
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
	res := &FileRevertResult{Reverted: true, Ref: ref, BackupRef: backupRef, Scope: string(RestoreScopeFull)}
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
					"(rewind with --restore-scope none to skip the workspace deliberately)",
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
func (s *Service) revertViaTracker(run *store.Run, wf *ir.Workflow, cp *store.Checkpoint, pivot, sourcePath string, scope RestoreScope) (*FileRevertResult, error) {
	if s.workspaceTracker == nil {
		return &FileRevertResult{SkipReason: "workspace versioning is not enabled on this store"}, nil
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
				return &FileRevertResult{SkipReason: fmt.Sprintf(
					"this run's workspace exceeded the %d-file cap, so nothing was versioned for it — "+
						"narrow the workspace with .iterionignore and relaunch; this run cannot be recovered",
					workspacetrack.DefaultMaxFiles)}, nil
			}
			return &FileRevertResult{SkipReason: "this run captured no workspace snapshots at all — it was launched on a path that does not enable workspace versioning, or versioning was off"}, nil
		}
		return &FileRevertResult{SkipReason: fmt.Sprintf(
			"the run has workspace snapshots but none recorded before %q — that node kind does not mark a pre-execution boundary", pivot)}, nil
	}
	// Resolve the run's own most recent boundary BEFORE banking. The bank
	// is labelled `rewind-backup:…`, which IsBoundaryLabel excludes, so
	// order is not load-bearing for correctness — but reading the run's
	// history before writing to it keeps the two apart at a glance.
	boundaryID, boundary := s.lastRecordedBoundary(run.ID)

	// Bank the current state first, so the restore destroys nothing: the
	// pre-rewind workspace stays a resolvable snapshot.
	//
	// Sequenced, like the git path's backup refs: a fixed key would let a
	// second rewind to the same pivot re-point the label and leave the
	// first bank on the chain with no name to reach it by — and under a
	// scoped restore this bank is the ONLY record of the files the
	// `overwritten` report names.
	backupLabel := fmt.Sprintf("rewind-backup:%s:%d", pivot, nextTrackerBackupSeq(s.workspaceTracker, run.ID, pivot))
	backup, err := s.workspaceTracker.Capture(run.ID, run.WorkDir, backupLabel)
	if err != nil {
		return nil, fmt.Errorf("bank the current workspace before reverting: %w", err)
	}

	res := &FileRevertResult{Ref: snapshotID, BackupRef: backup.ID, Scope: string(scope)}

	// `produced` narrows the restore to what the run recorded; anything
	// else forces every versioned path back to the snapshot, which is
	// what a worktree — iterion's own tree — should get, and what an
	// operator can still ask for explicitly.
	var only, parked []string
	scoped := scope == RestoreScopeProduced
	if scoped {
		produced, parkedChanges, ok := s.recordedChanges(run.ID, snapshotID, boundaryID)
		parked = parkedChanges
		if !ok {
			// The chain could not be walked from the boundary back to the
			// target. Refusing beats guessing: a wrong scope either leaves
			// the node's production in place (a replay on top of itself)
			// or reverts files with no evidence the run touched them.
			res.SkipReason = fmt.Sprintf(
				"could not resolve what this run changed after %q started (its snapshot chain does not reach that boundary) — "+
					"rewind with --restore-scope full to put the whole versioned workspace back, or --restore-scope none to leave it alone", pivot)
			return res, nil
		}
		only = produced
		res.ScopeCount = len(produced)
		if len(produced) == 0 {
			// NOT a success with a zero report. The workspace was not
			// touched, and saying "reverted" here would hand the operator
			// a line describing a restore that did not happen — then let
			// them resume onto whatever is actually on disk.
			res.SkipReason = fmt.Sprintf(
				"no execution of this run is recorded as having changed any file after %q started, so nothing was restored", pivot)
			// An EMPTY set, not nil: nil means "everything was in scope"
			// (the full restore), and passing it here would report every
			// path that moved since the boundary as overwritten by a
			// restore that never ran.
			s.describeUnrestored(run.ID, res, boundary, backup, map[string]bool{}, nil, parked)
			return res, nil
		}
	}

	// The workflow source is protected: a rewind exists to test an edit to
	// it, so restoring the workspace must not revert that edit — the
	// following `resume --force` would then recompile the OLD workflow and
	// silently test nothing. Bites only when the .bot lives inside the
	// workspace, which is the self-hosted dogfood shape.
	var report *workspacetrack.RestoreReport
	if scoped {
		report, err = s.workspaceTracker.RestoreOnly(run.ID, run.WorkDir, snapshotID, only, sourcePath, run.FilePath)
	} else {
		report, err = s.workspaceTracker.Restore(run.ID, run.WorkDir, snapshotID, sourcePath, run.FilePath)
	}
	if err != nil {
		return nil, fmt.Errorf("restore workspace to %s: %w", snapshotID, err)
	}
	res.Reverted = true
	res.Restored = report
	if len(report.Skipped) > 0 {
		res.CoverageGap = fmt.Sprintf(
			"%d path(s) were never captured (too large, or unreadable at capture time) and were left as-is",
			len(report.Skipped))
	}
	var inScope map[string]bool // nil = full restore: everything was in scope
	if scoped {
		inScope = make(map[string]bool, len(only))
		for _, p := range only {
			inScope[p] = true
		}
	}
	// A path the restore REFUSED to touch was not taken from anyone, and
	// naming it in the overwritten warning is worse than saying nothing:
	// the protected `.bot` heads that list on every self-hosted rewind,
	// steering the operator toward a --restore-snapshot that would undo
	// the whole rewind to "recover" a file that never moved.
	untouched := map[string]bool{sourcePath: true, run.FilePath: true}
	for _, p := range relativeToWorkspace(run.WorkDir, sourcePath, run.FilePath) {
		untouched[p] = true
	}
	for _, p := range report.Skipped {
		untouched[p] = true
	}
	s.describeUnrestored(run.ID, res, boundary, backup, inScope, untouched, parked)
	return res, nil
}

// relativeToWorkspace maps absolute protected paths onto the
// workspace-relative form the snapshots speak, dropping anything outside
// the workspace (which a restore could not have touched either).
func relativeToWorkspace(workspaceDir string, paths ...string) []string {
	var out []string
	for _, p := range paths {
		if p == "" {
			continue
		}
		abs, err := filepath.Abs(p)
		if err != nil {
			continue
		}
		rel, err := filepath.Rel(workspaceDir, abs)
		if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
			continue
		}
		out = append(out, filepath.ToSlash(rel))
	}
	return out
}

// describeUnrestored fills the two operator-facing sets: what the restore
// took that the run had not recorded, and what it left behind.
//
// Both are derived from ONE comparison — the run's last recorded boundary
// against the state on disk at rewind time — because that is the only
// place where "the run put this here" and "someone else did" diverge.
// Neither can be derived from the restore's own written/deleted lists: a
// path can be rewritten with content identical to what was there, and a
// path can be left alone for four different reasons.
//
// The honest part is that iterion cannot attribute the difference. A node
// that died before its boundary was written and an operator editing in
// another terminal produce the same evidence. So the sets are reported,
// not acted on.
func (s *Service) describeUnrestored(runID string, res *FileRevertResult, boundary, backup *workspacetrack.Snapshot, inScope, untouched map[string]bool, parked []string) {
	seen := map[string]bool{}
	report := func(overwritten bool, path string) {
		if seen[path] {
			return
		}
		seen[path] = true
		if overwritten {
			res.OverwrittenCount++
			res.Overwritten = appendCappedPath(res.Overwritten, path)
			return
		}
		res.LeftInPlaceCount++
		res.LeftInPlace = appendCappedPath(res.LeftInPlace, path)
	}
	// Paths that moved while the run was PARKED are excluded from the
	// scope by construction, so they belong in the left-in-place set even
	// when the boundary comparison below cannot see them (a later node
	// re-recorded them at the same content).
	for _, p := range parked {
		report(false, p)
	}
	if boundary == nil || backup == nil || boundary.ID == backup.ID {
		return
	}
	target, terr := s.workspaceTracker.Load(runID, res.Ref)
	if terr != nil {
		target = nil
	}
	stillDiffers := map[string]bool{}
	if target != nil {
		for _, c := range workspacetrack.StatusBetween(target, backup) {
			stillDiffers[c.Path] = true
		}
	}
	for _, c := range workspacetrack.StatusBetween(boundary, backup) {
		switch {
		case untouched[c.Path]:
			// The restore declined this path (protected, or a coverage
			// gap it holds no copy of). Nothing was taken; it was left.
			report(false, c.Path)
		case inScope == nil || inScope[c.Path]:
			// In the blast radius. It only counts as taken if the disk
			// also differed from what was restored — otherwise the file
			// already held the target content and nothing moved.
			if target != nil && !stillDiffers[c.Path] {
				continue
			}
			report(true, c.Path)
		default:
			report(false, c.Path)
		}
	}
}

// appendCappedPath bounds a reported path list. The struct is returned
// verbatim by the HTTP rewind endpoint, so an uncapped list is a full
// workspace listing on the wire; the paired count stays exact.
func appendCappedPath(dst []string, p string) []string {
	if len(dst) >= workspacetrack.ReportPathCap {
		return dst
	}
	return append(dst, p)
}

// lastRecordedBoundary resolves the newest snapshot this RUN produced, as
// opposed to the banks a rewind takes on the operator's behalf.
//
// It is the "after" side of the scope range, and it must come from the
// run's own history rather than from the disk: the whole point is to
// separate what the run changed from what changed around it.
func (s *Service) lastRecordedBoundary(runID string) (string, *workspacetrack.Snapshot) {
	boundaries := boundarySnapshotIDs(s.workspaceTracker, runID)
	seen := map[string]bool{}
	for id := s.workspaceTracker.Head(runID); id != "" && !seen[id]; {
		seen[id] = true
		snap, err := s.workspaceTracker.Load(runID, id)
		if err != nil {
			return "", nil
		}
		if boundaries[id] {
			return id, snap
		}
		id = snap.Parent
	}
	return "", nil
}

// boundarySnapshotIDs indexes the snapshot ids the engine labelled at a
// node or gate boundary.
func boundarySnapshotIDs(tr workspacetrack.Tracker, runID string) map[string]bool {
	out := map[string]bool{}
	for label, id := range tr.Labels(runID) {
		if workspacetrack.IsBoundaryLabel(label) {
			out[id] = true
		}
	}
	return out
}

// pauseSnapshotIDs indexes the snapshots that OPEN a parked interval —
// the state the run was in when it stopped waiting for a human.
func pauseSnapshotIDs(tr workspacetrack.Tracker, runID string) map[string]bool {
	out := map[string]bool{}
	for label, id := range tr.Labels(runID) {
		if strings.HasPrefix(label, workspacetrack.PauseLabelPrefix) {
			out[id] = true
		}
	}
	return out
}

// recordedChanges returns every workspace path this run is RECORDED to
// have changed between the target snapshot and its last boundary — the
// blast radius a scoped restore is allowed to touch.
//
// It is a UNION over consecutive boundaries, not a diff of the two
// endpoints, and the difference is not academic: a path a node rewrote
// and a later node put back is identical at both ends of the range while
// having been, demonstrably, a file this run writes to. Endpoint-only
// membership would exclude it, and a third, uncaptured write — the one a
// dying node leaves behind — would then survive the rewind.
//
// Non-boundary snapshots inside the range (the banks of an earlier
// rewind) are stepped OVER rather than diffed: their content is whatever
// was on disk at that moment, operator edits included, and a bank is not
// evidence that the run wrote anything.
//
// ok is false when the chain does not reach the target, which is the one
// case where guessing is worse than refusing.
func (s *Service) recordedChanges(runID, targetID, boundaryID string) (scope []string, parked []string, ok bool) {
	if targetID == "" {
		return nil, nil, false
	}
	if boundaryID == targetID {
		// The target IS the run's most recent boundary: nothing was
		// recorded after the pivot started. An empty scope, honestly.
		return nil, nil, true
	}
	if boundaryID == "" {
		// NOT the same thing, and it must not report as it. The target is
		// itself a boundary and lies on the chain, so failing to find one
		// means the walk broke — an unreadable manifest, a truncated
		// chain. "I could not tell" and "there was nothing" lead the
		// operator to different next steps.
		return nil, nil, false
	}
	boundaries := boundarySnapshotIDs(s.workspaceTracker, runID)
	paused := pauseSnapshotIDs(s.workspaceTracker, runID)
	// Walk from the boundary back to the target, keeping the boundary
	// snapshots (newest first). Bounded by `seen`: a manifest is on-disk
	// JSON unmarshalled bare, so a self-referential Parent must not spin.
	var stack []*workspacetrack.Snapshot
	seen := map[string]bool{}
	reached := false
	for id := boundaryID; id != "" && !seen[id]; {
		seen[id] = true
		snap, err := s.workspaceTracker.Load(runID, id)
		if err != nil {
			return nil, nil, false
		}
		if id == targetID {
			stack = append(stack, snap)
			reached = true
			break
		}
		if boundaries[id] {
			stack = append(stack, snap)
		}
		id = snap.Parent
	}
	if !reached {
		return nil, nil, false
	}
	changed, parkedSet := map[string]bool{}, map[string]bool{}
	// stack is newest → oldest, so consecutive pairs walk the range
	// backwards; StatusBetween is symmetric in membership.
	for i := len(stack) - 1; i > 0; i-- {
		older, newer := stack[i], stack[i-1]
		// An interval OPENED by a pause is the one interval in which
		// nothing of the run was executing. Whatever moved inside it was
		// moved by someone else — an editor, a build, a second run — so
		// claiming it as this run's production is exactly the false
		// authorship a scoped restore exists to stop. Excluded from the
		// scope and reported instead.
		into := changed
		if paused[older.ID] {
			into = parkedSet
		}
		for _, c := range workspacetrack.StatusBetween(older, newer) {
			into[c.Path] = true
		}
		// StatusBetween compares content hashes, so a node that only ran
		// `chmod +x deploy.sh` moves nothing it can see — and the restore's
		// own chmod branch would then never be reached for that path.
		// Executable-bit flips are routine output of build and scaffold
		// nodes, so the mode is part of what the run changed.
		for _, p := range modeOnlyChanges(older, newer) {
			into[p] = true
		}
	}
	// A path the run demonstrably touched is the run's, even if it also
	// moved during a pause; the reverse would let one parked interval
	// launder a node's whole production out of the scope.
	for p := range changed {
		delete(parkedSet, p)
	}
	return sortedKeys(changed), sortedKeys(parkedSet), true
}

// modeOnlyChanges lists paths present on both sides with the same content
// and a different permission bit set.
func modeOnlyChanges(base, head *workspacetrack.Snapshot) []string {
	if base == nil || head == nil {
		return nil
	}
	baseMap := make(map[string]workspacetrack.Entry, len(base.Entries))
	for _, e := range base.Entries {
		baseMap[e.Path] = e
	}
	var out []string
	for _, h := range head.Entries {
		if b, ok := baseMap[h.Path]; ok && b.Hash == h.Hash && b.Mode != h.Mode {
			out = append(out, h.Path)
		}
	}
	return out
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// nextTrackerBackupSeq counts the banks already taken for this (run,
// pivot) so repeated rewinds each keep their own, mirroring the git
// path's nextRewindBackupSeq.
func nextTrackerBackupSeq(tr workspacetrack.Tracker, runID, pivot string) int {
	prefix := "rewind-backup:" + pivot + ":"
	max := -1
	for label := range tr.Labels(runID) {
		if !strings.HasPrefix(label, prefix) {
			continue
		}
		if n, err := strconv.Atoi(label[len(prefix):]); err == nil && n > max {
			max = n
		}
	}
	return max + 1
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
	case run.Status == store.RunStatusFinished:
		// Terminal and genuinely unclaimable: `finished` is in no
		// resumable/rewindable path, so nothing can race the restore.
		return s.restoreBanked(run, snapshotID)
	case run.Status == store.RunStatusFailed:
		// Terminal but REWINDABLE: a concurrent `rewind` + `resume` can
		// claim the run and start an engine while the restore rewrites
		// its workspace — so the old "nothing to race with" shortcut no
		// longer holds. A real claim is not an option either: the CAS
		// would flip the run to `cancelled` and wipe run.Error (the only
		// record of why it failed) for what is here a pure workspace
		// operation. Re-check at the last moment and refuse if the run
		// moved; the sliver that remains — the restore itself — is the
		// same exposure any external workspace mutation has, and the
		// operator's own race to make.
		fresh, lerr := s.store.LoadRun(ctx, runID)
		if lerr != nil {
			return nil, fmt.Errorf("reload run before restore: %w", lerr)
		}
		if fresh.Status != store.RunStatusFailed {
			return nil, fmt.Errorf("%w: status changed under us (%s → %s) — reload and retry",
				ErrRewindNotRewindable, run.Status, fresh.Status)
		}
		return s.restoreBanked(fresh, snapshotID)
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
	// Protect the workflow source, exactly as revertViaTracker does.
	// --restore-snapshot accepts any id the chain holds — including a
	// mid-run `pre:<node>:<iter>` boundary, which --list-snapshots prints
	// right alongside the banks — so without this an operator recovering
	// their workspace can have their edited .bot silently rewritten to
	// the version the run originally executed, and the next
	// `resume --force` then compiles the OLD workflow. Invisible: the CLI
	// reports "N written, N deleted" and nothing flags that the source
	// moved. Bites when the .bot lives inside the workspace, which is the
	// self-hosted shape the docs make the primary example.
	return s.workspaceTracker.Restore(run.ID, run.WorkDir, snapshotID, run.FilePath)
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
