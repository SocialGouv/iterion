package runtime

import (
	"context"
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

func branchIterationCounters(rs *runState) map[string]int {
	if rs == nil {
		return nil
	}
	combined := cloneMap(rs.enclosingLoopCounters)
	if combined == nil && len(rs.loopCounters) > 0 {
		combined = make(map[string]int, len(rs.loopCounters))
	}
	for name, count := range rs.loopCounters {
		combined[name] = count
	}
	return combined
}

func runStateIterationCounters(rs *runState) map[string]int {
	if rs != nil && rs.branchLocal {
		return branchIterationCounters(rs)
	}
	if rs == nil {
		return nil
	}
	return rs.loopCounters
}

func (e *Engine) parallelInvocationKey(routerNodeID string, counters map[string]int) string {
	return routerNodeID + "@" + branchCounterPath(counters)
}

func (e *Engine) ensureParallelInvocation(rs *runState, routerNodeID string, starts map[string]string) (*parallelExecutionState, error) {
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
		return rs.parallel, nil
	}
	if rs.parallel != nil {
		persisted := rs.parallel.snapshot()
		if persisted != nil {
			if persisted.RouterNodeID == routerNodeID && persisted.InvocationKey == key {
				return nil, fmt.Errorf("runtime: parallel invocation %q branch set changed across restart; rewind the router before resuming", routerNodeID)
			}
			return nil, fmt.Errorf("runtime: persisted parallel invocation %q (%s) does not match resumed invocation %q (%s); rewind the router before resuming",
				persisted.RouterNodeID, persisted.InvocationKey, routerNodeID, key)
		}
	}
	rs.parallel = newParallelInvocation(routerNodeID, key, starts, rs.artifactVersions)
	rs.parallel.captureBase(rs)
	cp := buildCheckpoint(rs, routerNodeID)
	if err := e.store.SaveCheckpoint(rs.ctx, rs.runID, cp); err != nil && e.logger != nil {
		e.logger.Error("failed to initialize parallel checkpoint for %q: %v", routerNodeID, err)
	}
	return rs.parallel, nil
}

// parallelExecutionState is the synchronized in-memory owner of the durable
// fan-out checkpoint. Branches never mutate a store.BranchCheckpoint obtained
// from it; updates and snapshots deep-copy the JSON-shaped state.
type parallelExecutionState struct {
	mu      sync.Mutex
	saveMu  sync.Mutex // orders durable writes so an older snapshot cannot win last
	cp      *store.ParallelCheckpoint
	retired bool // collector returned; late abandoned branches may no longer persist
	// base is captured by the trunk before goroutines start. Copying a live
	// runState inside a branch would race its atomic branchLedgerSeq with a
	// sibling's Add; branches clone this immutable snapshot instead.
	base *runState
	// resumeBarrier lets the answered branch reach a durable terminal cursor
	// before incomplete siblings resume. Completed dependencies may pass it.
	resumeBarrier chan struct{}
	resumePending string
}

func (p *parallelExecutionState) retire() {
	if p == nil {
		return
	}
	p.saveMu.Lock()
	defer p.saveMu.Unlock()
	p.mu.Lock()
	p.retired = true
	p.mu.Unlock()
}

func (p *parallelExecutionState) isRetired() bool {
	if p == nil {
		return true
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.retired
}

func (p *parallelExecutionState) captureBase(rs *runState) {
	if p == nil || rs == nil || p.base != nil {
		return
	}
	p.base = cloneRunStateForBranch(rs)
	p.base.parallel = nil
	p.mu.Lock()
	if p.cp != nil && p.cp.PendingBranchID != "" {
		if branch := p.cp.Branches[p.cp.PendingBranchID]; branch != nil && branch.ResumeAnswered {
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
		enclosingLoopCounters: cloneMap(parent.enclosingLoopCounters),
		roundRobinCounters:    cloneMap(parent.roundRobinCounters),
		selectedIncoming:      parent.selectedIncoming,
		parallel:              parent.parallel,
		events:                parent.events,
		artifactVersions:      parent.artifactVersions,
		nodeSessions:          cloneNodeSessions(parent.nodeSessions),
		pauseSessionRef:       parent.pauseSessionRef,
		lastGraceNode:         parent.lastGraceNode,
		lastGraceDim:          parent.lastGraceDim,
		gateAnchors:           cloneMap(parent.gateAnchors),
		lastWorkspaceSnapshot: parent.lastWorkspaceSnapshot,
		lastSnapshotCommit:    parent.lastSnapshotCommit,
		preMarked:             cloneMap(parent.preMarked),
		stopCaptured:          cloneMap(parent.stopCaptured),
		resumed:               parent.resumed,
		budget:                parent.budget,
		resourceSemaphores:    parent.resourceSemaphores,
		costUSDTotal:          parent.costUSDTotal,
		nodeAttempts:          cloneNodeAttempts(parent.nodeAttempts),
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

// releaseResumeWaiters closes the resume barrier once the answered branch has
// exited by any path. checkpointResumedBranch already closed it on the happy
// path (this call is then a no-op); every other exit — a panic, an artifact
// or interaction write failure, an answer that satisfies no outgoing edge, a
// C245 guard on an edited source — would otherwise leave the siblings parked
// in waitResumeTurn forever, because best_effort never cancels them and
// collectBranches only arms its grace timer after a cancellation. The
// launchers call it AFTER deciding on sibling cancellation, so a parked
// sibling observes ctx.Done() rather than starting work that is about to be
// cancelled.
func (p *parallelExecutionState) releaseResumeWaiters(branchID string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	if p.resumePending != branchID || p.resumeBarrier == nil {
		p.mu.Unlock()
		return
	}
	barrier := p.resumeBarrier
	p.resumeBarrier = nil
	p.mu.Unlock()
	close(barrier)
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
	if p.retired {
		return false
	}
	p.cp.Branches[branch.BranchID] = cloneBranchCheckpoint(branch)
	return true
}

// abandonResume gives a gate back after the answered branch exited without
// reaching its successor cursor. The pending identity is cleared and the
// branch parks at the gate WITHOUT its consumed answers, so the next resume
// asks the question again (the schema-invalid re-pause precedent) instead of
// replaying a payload that already failed once — and a sibling under
// best_effort can elect its own gate instead of losing the election to a
// branch nobody is waiting on (beginPause would return false, and that branch
// would report ErrRunPaused with no interaction and no PauseRun behind it).
// The in-memory barrier is deliberately left to releaseResumeWaiters, which
// the launcher calls after the cancellation decision.
func (p *parallelExecutionState) abandonResume(branch *store.BranchCheckpoint) bool {
	if p == nil || branch == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.retired || p.cp.PendingBranchID != branch.BranchID {
		return false
	}
	branch.ResumeAnswers = nil
	branch.ResumeAnswered = false
	p.cp.Branches[branch.BranchID] = cloneBranchCheckpoint(branch)
	p.cp.PendingBranchID = ""
	p.cp.PendingNodeID = ""
	p.cp.PendingInteractionID = ""
	p.cp.PendingInteractionQuestions = nil
	return true
}

// updateResumedBranch commits the answered branch at its successor and clears
// the pending gate in the same parallel snapshot. Keeping those mutations
// together prevents a restart from observing an advanced cursor with a stale
// PendingBranchID after a crash between two checkpoint writes.
func (p *parallelExecutionState) updateResumedBranch(branch *store.BranchCheckpoint) (bool, chan struct{}) {
	if p == nil || branch == nil {
		return false, nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.retired {
		return false, nil
	}
	p.cp.Branches[branch.BranchID] = cloneBranchCheckpoint(branch)
	if p.resumePending != branch.BranchID {
		return true, nil
	}
	p.cp.PendingBranchID = ""
	p.cp.PendingNodeID = ""
	p.cp.PendingInteractionID = ""
	p.cp.PendingInteractionQuestions = nil
	barrier := p.resumeBarrier
	p.resumeBarrier = nil
	p.resumePending = ""
	return true, barrier
}

// beginPause elects at most one pending branch interaction for a fan-out.
func (p *parallelExecutionState) beginPause(branchID, nodeID string, branch *store.BranchCheckpoint) bool {
	if p == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.retired {
		return false
	}
	if p.cp.PendingBranchID != "" {
		if p.cp.PendingBranchID != branchID || p.cp.PendingNodeID != nodeID {
			return false
		}
		// Re-prompting the elected gate (for example after schema-invalid
		// answers) replaces its durable cursor and clears ResumeAnswers.
		p.cp.Branches[branchID] = cloneBranchCheckpoint(branch)
		return true
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
	branch.ResumeAnswered = true
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
	if p.cp.ArtifactAllocations == nil {
		p.cp.ArtifactAllocations = make(map[string]int)
	}
	if p.cp.NextArtifactVersion == nil {
		p.cp.NextArtifactVersion = make(map[string]int)
	}
	p.cp.ArtifactAllocations[executionKey] = next
	p.cp.NextArtifactVersion[nodeID] = next + 1
	return next
}

func cloneNodeAttempts(src map[string]map[ErrorCode]int) map[string]map[ErrorCode]int {
	if src == nil {
		return nil
	}
	dst := make(map[string]map[ErrorCode]int, len(src))
	for nodeID, attempts := range src {
		dst[nodeID] = cloneMap(attempts)
	}
	return dst
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
		ResumeAnswered:     src.ResumeAnswered,
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
