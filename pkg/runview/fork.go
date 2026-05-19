package runview

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/store"
)

// ForkSpec describes a fork request — a "create-an-alternative-future"
// operation that mints a new run id, copies the parent's persisted
// state up to (NodeID, TurnIndex), and parks the new run in a
// resumable state ready for the caller to launch via Resume.
type ForkSpec struct {
	// RunID is the parent run to fork from.
	RunID string
	// NodeID identifies the anchor node (i.e. the node the new run
	// will re-execute from). Required.
	NodeID string
	// TurnIndex selects the per-(node, iter) turn checkpoint to
	// rehydrate from. Negative means "latest available turn for this
	// node". For claude_code the index is always 0 (one TurnCheckpoint
	// per delegate call).
	TurnIndex int
	// RewindCode controls the worktree state of the child run:
	//   - false (default): the child's worktree starts at the parent's
	//     current HEAD (inherit current files).
	//   - true: the child's worktree is reset to the snapshot ref
	//     recorded at (NodeID, iter) — `git reset --hard` semantics
	//     against the per-node snapshot.
	// Phase 2 wires per-node refs; per-turn refs are a Phase 5 polish.
	RewindCode bool
	// ForkName, when non-empty, overrides the auto-generated child
	// name. Cosmetic — the child's id is always a fresh UUID.
	ForkName string
	// NewInputs, when non-nil, replaces the child run's Inputs map
	// (merged onto the parent's). Useful for "fork with a different
	// prompt vars" workflows from the studio's ForkDialog JSON editor.
	NewInputs map[string]interface{}
}

// ForkResult is the response shape returned to HTTP / CLI callers.
type ForkResult struct {
	NewRunID    string            `json:"new_run_id"`
	ParentRunID string            `json:"parent_run_id"`
	ForkAnchor  *store.ForkAnchor `json:"fork_anchor,omitempty"`
}

// Fork mints a new run id, copies the parent's persisted state up to
// (NodeID, TurnIndex), and writes a synthetic Checkpoint anchored
// there. The child run lands in the `cancelled` status so the
// existing Resume dispatch can pick it up via resumeFromFailure — the
// caller (CLI / HTTP) calls Resume separately when ready to actually
// execute the fork.
//
// Returns ForkResult on success. Errors short-circuit before any
// persistence so a failed Fork leaves the parent untouched.
func (s *Service) Fork(ctx context.Context, spec ForkSpec) (*ForkResult, error) {
	if spec.RunID == "" {
		return nil, errors.New("runview: fork: run_id is required")
	}
	if spec.NodeID == "" {
		return nil, errors.New("runview: fork: node_id is required")
	}
	parent, err := s.store.LoadRun(ctx, spec.RunID)
	if err != nil {
		return nil, fmt.Errorf("load parent run: %w", err)
	}
	turnStore := store.AsTurnStore(s.store)
	if turnStore == nil {
		return nil, fmt.Errorf("runview: fork: backend store does not support turn checkpoints (cloud stores not yet supported)")
	}
	var turn *store.TurnCheckpoint
	if spec.TurnIndex < 0 {
		turn, err = turnStore.LatestTurn(ctx, spec.RunID, spec.NodeID)
	} else {
		// Pick the highest LoopIter that has a turn at TurnIndex.
		// For the common single-iteration node, that's loop_iter=0.
		turn, err = turnStore.LoadTurn(ctx, spec.RunID, spec.NodeID, 0, spec.TurnIndex)
	}
	if err != nil {
		return nil, fmt.Errorf("load turn checkpoint: %w", err)
	}
	if turn == nil {
		return nil, fmt.Errorf("runview: fork: no turn checkpoint at node=%s turn=%d", spec.NodeID, spec.TurnIndex)
	}
	childID, err := store.GenerateRunID()
	if err != nil {
		return nil, fmt.Errorf("generate child run id: %w", err)
	}
	childInputs := map[string]interface{}{}
	for k, v := range parent.Inputs {
		childInputs[k] = v
	}
	for k, v := range spec.NewInputs {
		childInputs[k] = v
	}
	child, err := s.store.CreateRun(ctx, childID, parent.WorkflowName, childInputs)
	if err != nil {
		return nil, fmt.Errorf("create child run: %w", err)
	}
	// Mirror the parent's launch-time metadata so resume can pick up
	// the workflow source + bundle without re-supplying.
	child.FilePath = parent.FilePath
	child.WorkflowHash = parent.WorkflowHash
	child.Preset = parent.Preset
	child.BundleHash = parent.BundleHash
	child.BundlePath = parent.BundlePath
	child.LaunchEnv = parent.LaunchEnv
	child.IterionVersion = parent.IterionVersion
	child.TenantID = parent.TenantID
	child.OwnerID = parent.OwnerID
	if spec.ForkName != "" {
		child.Name = spec.ForkName
	} else {
		child.Name = fmt.Sprintf("%s · fork @ %s", parent.Name, spec.NodeID)
	}
	child.ForkedFrom = parent.ID
	child.ForkAnchor = &store.ForkAnchor{
		NodeID:     spec.NodeID,
		LoopIter:   turn.LoopIter,
		TurnIndex:  turn.TurnIndex,
		RewindCode: spec.RewindCode,
	}
	child.SourceHash = parent.WorkflowHash
	// Rewind code: build a child worktree at the snapshot ref or at
	// parent.HEAD. Best-effort — failure of the worktree-side step
	// fails the whole fork (the child is meaningless without a code
	// landing spot).
	if parent.Worktree {
		newWtPath, wterr := forkWorktree(parent, spec, turn, s.store.Root(), child.ID)
		if wterr != nil {
			return nil, fmt.Errorf("fork worktree: %w", wterr)
		}
		child.Worktree = true
		child.WorkDir = newWtPath
		child.RepoRoot = parent.RepoRoot
		child.BaseCommit = parent.BaseCommit
	} else {
		// Non-worktree parent: child inherits the parent's WorkDir
		// (typically the user's cwd). Rewind is meaningless; ignore
		// spec.RewindCode silently.
		child.WorkDir = parent.WorkDir
	}
	// Synthetic checkpoint anchoring the engine at NodeID. The engine's
	// resumeFromFailure path re-executes NodeID first, then walks
	// downstream.
	child.Checkpoint = &store.Checkpoint{
		NodeID:           spec.NodeID,
		Outputs:          copyOutputs(parent.Checkpoint),
		LoopCounters:     copyLoopCounters(parent.Checkpoint, spec.NodeID, turn.LoopIter),
		ArtifactVersions: copyArtifactVersions(parent.Checkpoint),
		Vars:             copyVars(parent.Checkpoint),
		BackendName:      turn.Backend,
		BackendSessionID: turn.SessionID,
	}
	// Claw rehydration: when the turn checkpoint has a MessagesRef
	// (i.e. the parent was running on the claw backend), load the
	// sibling messages.json blob and pin it to the checkpoint so the
	// child run's first delegate call replays the conversation.
	// claude_code skips this — the CLI owns its own session jsonl,
	// and BackendSessionID alone is the rehydration anchor.
	if turn.Backend == delegate.BackendClaw && turn.MessagesRef != "" {
		msgBytes, mErr := turnStore.LoadTurnMessages(ctx, parent.ID, spec.NodeID, turn.LoopIter, turn.TurnIndex)
		if mErr != nil {
			return nil, fmt.Errorf("load turn messages for claw rehydration: %w", mErr)
		}
		child.Checkpoint.BackendConversation = msgBytes
	}
	// Drop the forked node's existing output so re-execution starts
	// fresh (preserves topological ordering for downstream refs).
	delete(child.Checkpoint.Outputs, spec.NodeID)
	// Park the child as "cancelled" so the existing resumeFromFailure
	// dispatch picks it up unchanged. Caller posts /resume separately.
	child.Status = store.RunStatusCancelled
	now := time.Now().UTC()
	child.UpdatedAt = now
	if err := s.store.SaveRun(ctx, child); err != nil {
		return nil, fmt.Errorf("save child run: %w", err)
	}
	if err := s.store.SaveCheckpoint(ctx, child.ID, child.Checkpoint); err != nil {
		return nil, fmt.Errorf("save child checkpoint: %w", err)
	}
	return &ForkResult{
		NewRunID:    child.ID,
		ParentRunID: parent.ID,
		ForkAnchor:  child.ForkAnchor,
	}, nil
}

// forkWorktree materialises the child run's worktree. With
// RewindCode=true the worktree is created at the per-node snapshot
// ref; otherwise it's created at the parent's current HEAD (inherit
// current files). A missing snapshot ref produces a clean
// `git worktree add` error — callers see it instead of a silent
// HEAD fallback.
func forkWorktree(parent *store.Run, spec ForkSpec, turn *store.TurnCheckpoint, storeRoot, childID string) (string, error) {
	if parent.RepoRoot == "" {
		return "", fmt.Errorf("parent run has no repo root recorded — cannot fork worktree")
	}
	newWtPath, err := filepath.Abs(filepath.Join(storeRoot, "worktrees", childID))
	if err != nil {
		return "", fmt.Errorf("resolve new worktree path: %w", err)
	}
	target := "HEAD"
	if spec.RewindCode {
		target = store.NodeSnapshotRef(parent.ID, spec.NodeID, turn.LoopIter)
	}
	out, err := exec.Command("git", "-C", parent.RepoRoot, "worktree", "add", newWtPath, target).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git worktree add %s %s: %w\noutput: %s", newWtPath, target, err, out)
	}
	return newWtPath, nil
}

func copyOutputs(cp *store.Checkpoint) map[string]map[string]interface{} {
	if cp == nil {
		return map[string]map[string]interface{}{}
	}
	out := make(map[string]map[string]interface{}, len(cp.Outputs))
	for k, v := range cp.Outputs {
		out[k] = maps.Clone(v)
	}
	return out
}

func copyLoopCounters(cp *store.Checkpoint, anchorNode string, anchorIter int) map[string]int {
	out := map[string]int{}
	if cp != nil {
		out = maps.Clone(cp.LoopCounters)
		if out == nil {
			out = map[string]int{}
		}
	}
	if anchorIter > 0 {
		out[anchorNode] = anchorIter
	}
	return out
}

func copyArtifactVersions(cp *store.Checkpoint) map[string]int {
	if cp == nil {
		return map[string]int{}
	}
	if out := maps.Clone(cp.ArtifactVersions); out != nil {
		return out
	}
	return map[string]int{}
}

func copyVars(cp *store.Checkpoint) map[string]interface{} {
	if cp == nil {
		return map[string]interface{}{}
	}
	if out := maps.Clone(cp.Vars); out != nil {
		return out
	}
	return map[string]interface{}{}
}
