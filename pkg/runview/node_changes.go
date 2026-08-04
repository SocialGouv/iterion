package runview

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"

	gitlib "github.com/SocialGouv/iterion/pkg/git"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/workspacetrack"
)

// NodeChangeSet is what one node execution did to the workspace — the
// "node as a commit" view.
//
// It lives on the Service rather than in the HTTP layer because only the
// Service can reach BOTH backends: the git boundary refs of a worktree
// run, and the workspacetrack labels of an in-place one. The tracker is
// an unexported field here, and re-constructing one in the server would
// quietly bypass the operator's own off switch.
type NodeChangeSet struct {
	RunID     string `json:"run_id"`
	NodeID    string `json:"node_id"`
	Iteration int    `json:"iteration"`
	// Source names the backend that answered: "git" or "workspace".
	Source string `json:"source,omitempty"`
	// Available is false when no boundary pair could be resolved. Reason
	// then says why in the operator's terms — an empty list and "we
	// cannot tell" are different answers and must not look alike.
	Available bool             `json:"available"`
	Reason    string           `json:"reason,omitempty"`
	Files     []NodeFileChange `json:"files"`
	// Uncaptured lists paths a boundary deliberately did not store
	// (oversized). Their content is unavailable; saying so is the point.
	Uncaptured []string `json:"uncaptured,omitempty"`
}

// NodeFileChange is one file the node touched.
type NodeFileChange struct {
	Path    string `json:"path"`
	Status  string `json:"status"`
	Added   int    `json:"added,omitempty"`
	Deleted int    `json:"deleted,omitempty"`
	Binary  bool   `json:"binary,omitempty"`
}

// NodeChanges resolves what a node execution changed.
//
// iteration < 0 means "the latest one recorded", probed downwards: loop
// counters record where the run STOPPED, which is routinely past the last
// iteration this particular node ran, and composing a ref from them
// directly yields one that does not exist.
func (s *Service) NodeChanges(ctx context.Context, runID, nodeID string, iteration int) (*NodeChangeSet, error) {
	if nodeID == "" {
		return nil, fmt.Errorf("runview: node changes: node id is required")
	}
	run, err := s.LoadRunCtx(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("load run: %w", err)
	}
	out := &NodeChangeSet{RunID: runID, NodeID: nodeID, Iteration: iteration, Files: []NodeFileChange{}}

	// Git first when the run has a worktree: the refs are cheap, and
	// DiffBetween already computes line counts and binary flags.
	if repo := nodeChangesRepo(run); repo != "" {
		if set, ok := s.nodeChangesFromGit(repo, run.ID, nodeID, iteration); ok {
			return set, nil
		}
	}
	if s.workspaceTracker != nil {
		// Read-only surface over an ARBITRARY run, including ones this
		// process never executed — so the stat cache this resolve warms
		// must not be pinned for the life of the studio.
		defer s.forgetTrackerCacheIfNotLive(run.ID)
		if set, ok := s.nodeChangesFromTracker(run.ID, nodeID, iteration); ok {
			return set, nil
		}
	}

	// Nothing resolved. Say which of the several causes it is — they lead
	// to different actions, and conflating them sends people looking for
	// the wrong problem.
	out.Reason = s.explainMissingBoundary(run, nodeID)
	return out, nil
}

// nodeChangesRepo returns a directory from which the run's refs are
// reachable.
//
// Not simply run.WorkDir: a finalized worktree run has had its worktree
// removed, yet `refs/iterion/runs/<run>/…` was written into the
// repository's COMMON ref store — linked worktrees share refs, and
// nothing deletes them. So a finished run's node diffs remain readable
// from the repo root long after its worktree is gone.
func nodeChangesRepo(run *store.Run) string {
	for _, dir := range []string{run.WorkDir, run.RepoRoot} {
		if dir == "" {
			continue
		}
		if info, err := os.Stat(dir); err == nil && info.IsDir() {
			return dir
		}
	}
	return ""
}

func (s *Service) nodeChangesFromGit(repo, runID, nodeID string, iteration int) (*NodeChangeSet, bool) {
	for iter := nodeChangesProbeStart(iteration); iter >= nodeChangesProbeStop(iteration); iter-- {
		pre := store.NodePreSnapshotRef(runID, nodeID, iter)
		post := store.NodeSnapshotRef(runID, nodeID, iter)
		if !gitRefExists(repo, pre) || !gitRefExists(repo, post) {
			continue
		}
		files, err := gitlib.StatusBetween(repo, pre, post)
		if err != nil {
			continue
		}
		set := &NodeChangeSet{
			RunID: runID, NodeID: nodeID, Iteration: iter,
			Source: "git", Available: true, Files: []NodeFileChange{},
		}
		for _, f := range files {
			set.Files = append(set.Files, NodeFileChange{
				Path: f.Path, Status: f.Status,
				Added: f.Added, Deleted: f.Deleted, Binary: f.Binary,
			})
		}
		return set, true
	}
	return nil, false
}

func (s *Service) nodeChangesFromTracker(runID, nodeID string, iteration int) (*NodeChangeSet, bool) {
	for iter := nodeChangesProbeStart(iteration); iter >= nodeChangesProbeStop(iteration); iter-- {
		preID, okPre := s.workspaceTracker.Resolve(runID, workspacetrack.Label(workspacetrack.PhasePre, nodeID, iter))
		postID, okPost := s.workspaceTracker.Resolve(runID, workspacetrack.Label(workspacetrack.PhasePost, nodeID, iter))
		if !okPre || !okPost {
			continue
		}
		changes, err := s.workspaceTracker.Changes(runID, preID, postID)
		if err != nil {
			continue
		}
		set := &NodeChangeSet{
			RunID: runID, NodeID: nodeID, Iteration: iter,
			Source: "workspace", Available: true, Files: []NodeFileChange{},
		}
		for _, c := range changes {
			if c.Uncaptured {
				set.Uncaptured = append(set.Uncaptured, c.Path)
				continue
			}
			set.Files = append(set.Files, NodeFileChange{
				Path: c.Path, Status: string(c.Status), Binary: c.Binary,
			})
		}
		sort.Strings(set.Uncaptured)
		return set, true
	}
	return nil, false
}

// nodeChangesProbeStart bounds the downward probe. The cap is generous
// enough for any real loop and keeps a malformed request from walking
// thousands of nonexistent refs.
func nodeChangesProbeStart(iteration int) int {
	if iteration >= 0 {
		return iteration
	}
	return 64
}

// nodeChangesProbeStop is where the downward probe must give up.
//
// Walking down is the semantics of iteration < 0 ("show me the latest one
// recorded") and ONLY that. For an explicit iteration it is actively
// wrong: the studio always sends one, so a loop node currently executing
// its iteration 2 — which has an opening boundary but no closing one —
// silently resolved iteration 1 and was reported Available with iteration
// 1's file list. That made explainMissingBoundary's "this node has not
// finished yet" unreachable for any looped node, and left a
// plausible-but-wrong diff on screen with only a small caption to
// contradict it. Precisely the class this feature exists to prevent.
func nodeChangesProbeStop(iteration int) int {
	if iteration >= 0 {
		return iteration
	}
	return 0
}

// explainMissingBoundary names the actual cause, because the fixes
// differ: a subbot never records one, a running node has not yet, and an
// older run never did.
func (s *Service) explainMissingBoundary(run *store.Run, nodeID string) string {
	if kind := s.nodeKind(run, nodeID); kind != "" && !nodeKindRecordsBoundary(kind) {
		return fmt.Sprintf(
			"a %s node records no completion boundary, so its file changes cannot be attributed to it — they appear in the run's overall changes instead",
			kind)
	}
	if run.Status == store.RunStatusRunning {
		return "this node has not finished yet, so its closing boundary does not exist — it appears once the node completes"
	}
	if !run.Worktree && s.workspaceTracker == nil {
		return "workspace versioning is not enabled on this store"
	}
	return "no boundary was recorded for this node — the run may predate workspace versioning, or it ran off the main execution path (a fan-out branch records none)"
}

// nodeKindRecordsBoundary reports whether a node kind is bracketed.
//
// Only the main executor path writes a closing boundary: everything
// dispatched specially (subbot, compute, routers, emit/wait) marks its
// opening boundary and never its close, so Resolve(post:…) is false for
// them by construction. A panel that renders that as "no changes" lies
// about the node kind most likely to have rewritten the tree.
func nodeKindRecordsBoundary(kind string) bool {
	switch strings.ToLower(kind) {
	case "subbot", "compute", "router", "emit", "wait", "await_answers":
		return false
	}
	return true
}

// nodeKind reads a node's kind from the run's compiled workflow.
func (s *Service) nodeKind(run *store.Run, nodeID string) string {
	path := resolveWorkflowPath(run)
	if path == "" {
		return ""
	}
	wf, err := CompileWorkflow(path)
	if err != nil {
		return ""
	}
	node, ok := wf.Nodes[nodeID]
	if !ok {
		return ""
	}
	return node.NodeKind().String()
}

// NodeFileDiff returns one file's before/after within a node's boundary.
func (s *Service) NodeFileDiff(ctx context.Context, runID, nodeID string, iteration int, path string) (*gitlib.DiffPayload, error) {
	if err := gitlib.ValidateRelPath(path); err != nil {
		return nil, fmt.Errorf("invalid path: %w", err)
	}
	set, err := s.NodeChanges(ctx, runID, nodeID, iteration)
	if err != nil {
		return nil, err
	}
	if !set.Available {
		return nil, fmt.Errorf("no boundary for %q: %s", nodeID, set.Reason)
	}
	run, err := s.LoadRunCtx(ctx, runID)
	if err != nil {
		return nil, err
	}
	if set.Source == "git" {
		repo := nodeChangesRepo(run)
		payload, derr := gitlib.DiffBetween(repo,
			store.NodePreSnapshotRef(runID, nodeID, set.Iteration),
			store.NodeSnapshotRef(runID, nodeID, set.Iteration), path)
		if derr != nil {
			return nil, derr
		}
		return &payload, nil
	}
	return s.trackerFileDiff(runID, nodeID, set.Iteration, path)
}

// nodeDiffPayloadCap mirrors pkg/git's diffPayloadCap and
// workspacetrack's: one bound for every per-file diff surface.
const nodeDiffPayloadCap int64 = 5 << 20 // 5 MiB

// trackerFileDiff builds a diff payload from stored content.
func (s *Service) trackerFileDiff(runID, nodeID string, iteration int, path string) (*gitlib.DiffPayload, error) {
	// Same reason as NodeChanges: a read-only resolve on a run this
	// process does not hold must not pin its stat cache forever.
	defer s.forgetTrackerCacheIfNotLive(runID)
	preID, okPre := s.workspaceTracker.Resolve(runID, workspacetrack.Label(workspacetrack.PhasePre, nodeID, iteration))
	postID, okPost := s.workspaceTracker.Resolve(runID, workspacetrack.Label(workspacetrack.PhasePost, nodeID, iteration))
	if !okPre || !okPost {
		return nil, fmt.Errorf("no workspace boundary for %s@%d", nodeID, iteration)
	}
	changes, err := s.workspaceTracker.Changes(runID, preID, postID)
	if err != nil {
		return nil, err
	}
	for _, c := range changes {
		if c.Path != path {
			continue
		}
		payload := &gitlib.DiffPayload{Path: path, Binary: c.Binary}
		if c.Uncaptured {
			// No content was ever stored; rendering an empty diff would
			// present "too large to version" as "unchanged".
			payload.Oversized = true
			return payload, nil
		}
		if c.Binary {
			return payload, nil
		}
		// Bound both sides at the same 5 MiB the other two diff paths use
		// (pkg/git.DiffBetween and workspacetrack.DiffBetween). The tracker
		// captures anything up to MaxFileBytes — 32 MiB by default, more
		// with ITERION_WORKSPACE_MAX_FILE_MB — so an edit to a large
		// generated artefact otherwise allocated both sides plus their
		// JSON encoding in the server and shipped ~64 MiB to a browser
		// that then asked Monaco to render it. The same file opened
		// through the review-scope panel is refused; the two must agree.
		// Change already carries the sizes, so the guard costs nothing.
		if c.OldSize > nodeDiffPayloadCap || c.NewSize > nodeDiffPayloadCap {
			payload.Oversized = true
			return payload, nil
		}
		if c.OldHash != "" {
			b, rerr := s.workspaceTracker.Object(c.OldHash)
			if rerr != nil {
				return nil, rerr
			}
			before := string(b)
			payload.Before = &before
		}
		if c.NewHash != "" {
			b, rerr := s.workspaceTracker.Object(c.NewHash)
			if rerr != nil {
				return nil, rerr
			}
			after := string(b)
			payload.After = &after
		}
		return payload, nil
	}
	return nil, fmt.Errorf("%q is not among the changes of %s@%d", path, nodeID, iteration)
}

// gitRefExists reports whether a ref resolves to a commit.
func gitRefExists(repo, ref string) bool {
	_, err := runtime.RunGitIn(repo, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
	return err == nil
}
