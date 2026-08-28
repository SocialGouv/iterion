package runtime

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/SocialGouv/iterion/pkg/store"
)

func branchCounterPath(counters map[string]int) string {
	if len(counters) == 0 {
		return "root"
	}
	keys := make([]string, 0, len(counters))
	for key := range counters {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", key, counters[key]))
	}
	return strings.Join(parts, ";")
}

func (e *Engine) parallelInvocationKey(routerNodeID string, counters map[string]int) string {
	return routerNodeID + "@" + branchCounterPath(counters)
}

func (e *Engine) ensureParallelInvocation(rs *runState, routerNodeID string, starts map[string]string) *parallelExecutionState {
	ids := make([]string, 0, len(starts))
	for id := range starts {
		ids = append(ids, id)
	}
	// matches is order-independent; sorting simply makes debugger output and
	// future callers stable.
	sort.Strings(ids)
	key := e.parallelInvocationKey(routerNodeID, rs.loopCounters)
	if rs.parallel != nil && rs.parallel.matches(routerNodeID, key, ids) {
		rs.parallel.captureBase(rs)
		return rs.parallel
	}
	rs.parallel = newParallelInvocation(routerNodeID, key, starts, rs.artifactVersions)
	rs.parallel.captureBase(rs)
	cp := buildCheckpoint(rs, routerNodeID)
	if err := e.store.SaveCheckpoint(rs.ctx, rs.runID, cp); err != nil && e.logger != nil {
		e.logger.Error("failed to initialize parallel checkpoint for %q: %v", routerNodeID, err)
	}
	return rs.parallel
}

// parallelExecutionState is the synchronized in-memory owner of the durable
// fan-out checkpoint. Branches never mutate a store.BranchCheckpoint obtained
// from it; updates and snapshots deep-copy the JSON-shaped state.
type parallelExecutionState struct {
	mu     sync.Mutex
	saveMu sync.Mutex // orders durable writes so an older snapshot cannot win last
	cp     *store.ParallelCheckpoint
	// base is captured by the trunk before goroutines start. Copying a live
	// runState inside a branch would race its atomic branchLedgerSeq with a
	// sibling's Add; branches clone this immutable snapshot instead.
	base *runState
	// resumeBarrier lets the answered branch reach a durable terminal cursor
	// before incomplete siblings resume. Completed dependencies may pass it.
	resumeBarrier chan struct{}
	resumePending string
}

func (p *parallelExecutionState) captureBase(rs *runState) {
	if p == nil || rs == nil || p.base != nil {
		return
	}
	p.base = cloneRunStateForBranch(rs)
	p.base.parallel = nil
	p.mu.Lock()
	if p.cp != nil && p.cp.PendingBranchID != "" {
		if branch := p.cp.Branches[p.cp.PendingBranchID]; branch != nil && len(branch.ResumeAnswers) > 0 {
			p.resumeBarrier = make(chan struct{})
			p.resumePending = p.cp.PendingBranchID
		}
	}
	p.mu.Unlock()
}

// cloneRunStateForBranch copies the branch-readable trunk state without a
// struct assignment. runState contains atomic.Uint64 (and therefore noCopy),
// while the maps below are immutable for the lifetime of a fan-out by the
// runState concurrency contract. Branch-private mutable maps are replaced by
// newBranchRunState immediately after this clone.
func cloneRunStateForBranch(parent *runState) *runState {
	if parent == nil {
		return &runState{}
	}
	local := &runState{
		ctx:                   parent.ctx,
		runID:                 parent.runID,
		runInputs:             parent.runInputs,
		vars:                  parent.vars,
		outputs:               parent.outputs,
		artifacts:             parent.artifacts,
		loopCounters:          parent.loopCounters,
		loopOverrides:         parent.loopOverrides,
		permissionGrants:      parent.permissionGrants,
		loopPreviousOutput:    parent.loopPreviousOutput,
		loopCurrentOutput:     parent.loopCurrentOutput,
		loopProgressSig:       parent.loopProgressSig,
		loopStaleness:         parent.loopStaleness,
		loopBudgetMarks:       parent.loopBudgetMarks,
		roundRobinCounters:    parent.roundRobinCounters,
		selectedIncoming:      parent.selectedIncoming,
		parallel:              parent.parallel,
		events:                parent.events,
		artifactVersions:      parent.artifactVersions,
		nodeSessions:          parent.nodeSessions,
		pauseSessionRef:       parent.pauseSessionRef,
		lastGraceNode:         parent.lastGraceNode,
		lastGraceDim:          parent.lastGraceDim,
		gateAnchors:           parent.gateAnchors,
		lastWorkspaceSnapshot: parent.lastWorkspaceSnapshot,
		lastSnapshotCommit:    parent.lastSnapshotCommit,
		preMarked:             parent.preMarked,
		stopCaptured:          parent.stopCaptured,
		resumed:               parent.resumed,
		budget:                parent.budget,
		resourceSemaphores:    parent.resourceSemaphores,
		costUSDTotal:          parent.costUSDTotal,
		nodeAttempts:          parent.nodeAttempts,
		attachments:           parent.attachments,
		resumeBackend:         parent.resumeBackend,
		isWorktree:            parent.isWorktree,
	}
	local.branchLedgerSeq.Store(parent.branchLedgerSeq.Load())
	return local
}

func (p *parallelExecutionState) branchBase(fallback *runState) *runState {
	if p != nil && p.base != nil {
		return p.base
	}
	return fallback
}

func (p *parallelExecutionState) waitResumeTurn(ctx context.Context, branchID string) error {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	barrier := p.resumeBarrier
	pending := p.resumePending
	completed := p.cp != nil && p.cp.Branches[branchID] != nil && p.cp.Branches[branchID].Completed
	p.mu.Unlock()
	if barrier == nil || branchID == pending || completed {
		return nil
	}
	select {
	case <-barrier:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (e *Engine) completeParallelResume(parent *runState, p *parallelExecutionState, branchID string, result *branchResult) {
	if p == nil || result == nil || errors.Is(result.err, ErrRunPaused) {
		return
	}
	p.saveMu.Lock()
	defer p.saveMu.Unlock()
	p.mu.Lock()
	if p.resumePending != branchID {
		p.mu.Unlock()
		return
	}
	p.cp.PendingBranchID = ""
	p.cp.PendingNodeID = ""
	p.cp.PendingInteractionID = ""
	p.cp.PendingInteractionQuestions = nil
	barrier := p.resumeBarrier
	p.resumeBarrier = nil
	p.resumePending = ""
	p.mu.Unlock()

	ps := p.snapshot()
	cp := buildCheckpoint(parent, ps.RouterNodeID)
	cp.Parallel = ps
	if err := e.store.SaveCheckpoint(parent.ctx, parent.runID, cp); err != nil && e.logger != nil {
		e.logger.Error("failed to complete parallel resume checkpoint for %s: %v", branchID, err)
	}
	if barrier != nil {
		close(barrier)
	}
}

func newParallelExecutionState(cp *store.ParallelCheckpoint) *parallelExecutionState {
	if cp == nil {
		return nil
	}
	return &parallelExecutionState{cp: cloneParallelCheckpoint(cp)}
}

func newParallelInvocation(routerNodeID, invocationKey string, starts map[string]string, artifactVersions map[string]int) *parallelExecutionState {
	cp := &store.ParallelCheckpoint{
		RouterNodeID:        routerNodeID,
		InvocationKey:       invocationKey,
		Branches:            make(map[string]*store.BranchCheckpoint, len(starts)),
		ArtifactAllocations: make(map[string]int),
		NextArtifactVersion: cloneMap(artifactVersions),
	}
	if cp.NextArtifactVersion == nil {
		cp.NextArtifactVersion = make(map[string]int)
	}
	for branchID, start := range starts {
		cp.Branches[branchID] = &store.BranchCheckpoint{
			BranchID:           branchID,
			StartNodeID:        start,
			CurrentNodeID:      start,
			Outputs:            make(map[string]map[string]any),
			Artifacts:          make(map[string]map[string]any),
			ArtifactVersions:   cloneMap(artifactVersions),
			LoopCounters:       make(map[string]int),
			LoopPreviousOutput: make(map[string]map[string]any),
			LoopCurrentOutput:  make(map[string]map[string]any),
			LoopBudgetMarks:    make(map[string]map[string]float64),
			SelectedIncoming:   make(map[string][]store.IncomingEdge),
		}
	}
	return &parallelExecutionState{cp: cp}
}

func (p *parallelExecutionState) matches(routerNodeID, invocationKey string, branchIDs []string) bool {
	if p == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cp == nil || p.cp.RouterNodeID != routerNodeID || p.cp.InvocationKey != invocationKey || len(p.cp.Branches) != len(branchIDs) {
		return false
	}
	for _, id := range branchIDs {
		if p.cp.Branches[id] == nil {
			return false
		}
	}
	return true
}

func (p *parallelExecutionState) snapshot() *store.ParallelCheckpoint {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return cloneParallelCheckpoint(p.cp)
}

func (p *parallelExecutionState) branch(branchID string) *store.BranchCheckpoint {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return cloneBranchCheckpoint(p.cp.Branches[branchID])
}

// updateBranch commits one branch cursor. Durable writes also copy the pending
// interaction metadata, so siblings may checkpoint cancellation-boundary
// progress without erasing the pause that owns the parent run.
func (p *parallelExecutionState) updateBranch(branch *store.BranchCheckpoint) bool {
	if p == nil || branch == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cp.Branches[branch.BranchID] = cloneBranchCheckpoint(branch)
	return true
}

// beginPause elects at most one pending branch interaction for a fan-out.
func (p *parallelExecutionState) beginPause(branchID, nodeID string, branch *store.BranchCheckpoint) bool {
	if p == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cp.PendingBranchID != "" {
		return p.cp.PendingBranchID == branchID && p.cp.PendingNodeID == nodeID
	}
	p.cp.PendingBranchID = branchID
	p.cp.PendingNodeID = nodeID
	p.cp.Branches[branchID] = cloneBranchCheckpoint(branch)
	return true
}

func (p *parallelExecutionState) setPendingInteraction(id string, questions map[string]any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cp.PendingInteractionID = id
	p.cp.PendingInteractionQuestions = deepCopyAnyMap(questions)
}

func (p *parallelExecutionState) setResumeAnswers(branchID string, answers map[string]any) error {
	if p == nil {
		return fmt.Errorf("parallel checkpoint is missing")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cp.PendingBranchID != branchID {
		return fmt.Errorf("parallel checkpoint waits on branch %q, not %q", p.cp.PendingBranchID, branchID)
	}
	branch := p.cp.Branches[branchID]
	if branch == nil {
		return fmt.Errorf("parallel checkpoint branch %q is missing", branchID)
	}
	branch.ResumeAnswers = deepCopyAnyMap(answers)
	// The interaction has been answered. Keeping the pending identity until
	// the human node advances prevents sibling progress from racing ahead.
	return nil
}

// artifactVersion returns the stable version allocated to one logical branch
// execution. Repeated calls after restart return the same version.
func (p *parallelExecutionState) artifactVersion(nodeID, executionKey string, floor int) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	if v, ok := p.cp.ArtifactAllocations[executionKey]; ok {
		return v
	}
	next := p.cp.NextArtifactVersion[nodeID]
	if next < floor {
		next = floor
	}
	p.cp.ArtifactAllocations[executionKey] = next
	p.cp.NextArtifactVersion[nodeID] = next + 1
	return next
}

func cloneParallelCheckpoint(src *store.ParallelCheckpoint) *store.ParallelCheckpoint {
	if src == nil {
		return nil
	}
	dst := &store.ParallelCheckpoint{
		RouterNodeID:                src.RouterNodeID,
		InvocationKey:               src.InvocationKey,
		PendingBranchID:             src.PendingBranchID,
		PendingNodeID:               src.PendingNodeID,
		PendingInteractionID:        src.PendingInteractionID,
		PendingInteractionQuestions: deepCopyAnyMap(src.PendingInteractionQuestions),
		Branches:                    make(map[string]*store.BranchCheckpoint, len(src.Branches)),
		ArtifactAllocations:         cloneMap(src.ArtifactAllocations),
		NextArtifactVersion:         cloneMap(src.NextArtifactVersion),
	}
	for id, branch := range src.Branches {
		dst.Branches[id] = cloneBranchCheckpoint(branch)
	}
	return dst
}

func cloneBranchCheckpoint(src *store.BranchCheckpoint) *store.BranchCheckpoint {
	if src == nil {
		return nil
	}
	return &store.BranchCheckpoint{
		BranchID:           src.BranchID,
		StartNodeID:        src.StartNodeID,
		CurrentNodeID:      src.CurrentNodeID,
		Outputs:            copyOutputs(src.Outputs),
		Artifacts:          copyOutputs(src.Artifacts),
		ArtifactVersions:   cloneMap(src.ArtifactVersions),
		LoopCounters:       cloneMap(src.LoopCounters),
		LoopPreviousOutput: copyOutputs(src.LoopPreviousOutput),
		LoopCurrentOutput:  copyOutputs(src.LoopCurrentOutput),
		LoopBudgetMarks:    cloneNestedFloatMap(src.LoopBudgetMarks),
		SelectedIncoming:   cloneIncoming(src.SelectedIncoming),
		JoinNodeID:         src.JoinNodeID,
		TerminalNodeID:     src.TerminalNodeID,
		Completed:          src.Completed,
		TerminatedAtDone:   src.TerminatedAtDone,
		ResumeAnswers:      deepCopyAnyMap(src.ResumeAnswers),
	}
}

func cloneNestedFloatMap(src map[string]map[string]float64) map[string]map[string]float64 {
	if src == nil {
		return nil
	}
	dst := make(map[string]map[string]float64, len(src))
	for key, values := range src {
		dst[key] = cloneMap(values)
	}
	return dst
}

func deepCopyAnyMap(src map[string]any) map[string]any {
	if src == nil {
		return nil
	}
	dst := make(map[string]any, len(src))
	for k, v := range src {
		dst[k] = deepCopyValue(v)
	}
	return dst
}
