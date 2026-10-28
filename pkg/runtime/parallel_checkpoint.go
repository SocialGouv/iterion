package runtime

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

// fallbackParallelSaveWindow is the coalescing window for routine fan-out
// boundary checkpoints (#561). Branch nodes run for seconds to minutes, so a
// window of this order merges the boundaries of siblings finishing together
// (the wait_all burst) into one durable write while keeping the replay
// window a crash can lose bounded to the same order. Zero disables
// coalescing entirely.
const fallbackParallelSaveWindow = 250 * time.Millisecond

// defaultParallelSaveWindow resolves the operator's window at engine
// construction: ITERION_PARALLEL_SAVE_WINDOW (a Go duration; "0" disables
// coalescing outright) overrides the fallback when an operator chasing a
// coalescing-related replay wants every boundary written inline without a
// rebuild. An unparsable value keeps the fallback rather than guessing.
func defaultParallelSaveWindow() time.Duration {
	if v := os.Getenv("ITERION_PARALLEL_SAVE_WINDOW"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d >= 0 {
			return d
		}
	}
	return fallbackParallelSaveWindow
}

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

// loopPreviousOutputView is the composed {{loop.<name>.previous_output}}
// map resolvers and prompt templates read. Inside a branch it unions the
// enclosing trunk snapshots with any branch-local loop snapshots; C244
// makes the two maps name-disjoint.
func loopPreviousOutputView(rs *runState) map[string]map[string]any {
	if rs == nil {
		return nil
	}
	if !rs.branchLocal {
		return rs.loopPreviousOutput
	}
	combined := copyOutputs(rs.enclosingLoopPreviousOutput)
	for name, out := range rs.loopPreviousOutput {
		inner := make(map[string]any, len(out))
		for k, v := range out {
			inner[k] = deepCopyValue(v)
		}
		combined[name] = inner
	}
	return combined
}

func enclosingLoopPreviousOutputFrom(parent *runState) map[string]map[string]any {
	if parent == nil {
		return nil
	}
	combined := copyOutputs(parent.enclosingLoopPreviousOutput)
	for name, out := range parent.loopPreviousOutput {
		inner := make(map[string]any, len(out))
		for k, v := range out {
			inner[k] = deepCopyValue(v)
		}
		combined[name] = inner
	}
	return combined
}

func branchLedgerKey(runID, branchID string, rs *runState) string {
	return fmt.Sprintf("%s#%s#%s", runID, branchID, branchCounterPath(runStateIterationCounters(rs)))
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
		e.startParallelSaver(rs, rs.parallel)
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
	e.startParallelSaver(rs, rs.parallel)
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
	// saveWindow coalesces routine boundary checkpoints (#561): a boundary
	// marks the invocation dirty and kicks the saver instead of writing
	// inline, and siblings finishing together share one durable write.
	// Immediate writes (completion, pause, spend, abandoned resume) bypass
	// the window in both modes — they own durability contracts of their own.
	// dirtyGen/writtenGen are mutated only under mu. saveWindow and the
	// three channels are set once in startParallelSaver before any branch
	// goroutine exists and read without mu afterwards (happens-before via
	// that hand-off) — re-arming them later would race. Every durable
	// write still serializes under saveMu, so an older snapshot can never
	// win last.
	saveWindow time.Duration
	saveKick   chan struct{}
	saveStop   chan struct{}
	saveDone   chan struct{}
	dirtyGen   int
	writtenGen int
	flush      func() // writes the current snapshot; holds saveMu (set with the saver)
	// base is captured by the trunk before goroutines start. Branches clone
	// this immutable snapshot instead of a live runState so they never race
	// the trunk's unsynchronized maps.
	base *runState
	// resumeBarrier lets the answered branch reach a durable terminal cursor
	// before incomplete siblings resume. Completed dependencies may pass it.
	resumeBarrier chan struct{}
	resumePending string
}

// retire marks the invocation finished and stops its coalescing saver. A
// routine boundary still pending at this point flushes first, so the
// collector never returns with a requested cursor undurable; the immediate
// writes (completion, pause, spend, abandoned resume) already covered their
// own. The saver stops before the flush: it may be parked inside a write
// holding saveMu, and retire must never wait on a lock its own stop signal
// is supposed to release.
func (p *parallelExecutionState) retire() {
	if p == nil {
		return
	}
	p.mu.Lock()
	already := p.retired
	p.retired = true
	stop, done := p.saveStop, p.saveDone
	p.mu.Unlock()
	if stop != nil && !already {
		close(stop)
		<-done
	}
	p.saveMu.Lock()
	defer p.saveMu.Unlock()
	p.flushIfDirty()
}

// markWritten records that the durable record carries every boundary
// requested so far; a coalescing saver then skips a write nothing new asked
// for. Callers hold saveMu and call it after a successful store write.
func (p *parallelExecutionState) markWritten() {
	p.mu.Lock()
	p.writtenGen = p.dirtyGen
	p.mu.Unlock()
}

// flushIfDirty writes the current snapshot when a routine boundary is
// pending. The saver calls it when its window closes and retire calls it
// before it returns; the caller holds saveMu.
func (p *parallelExecutionState) flushIfDirty() {
	p.mu.Lock()
	pending := p.dirtyGen > p.writtenGen
	flush := p.flush
	p.mu.Unlock()
	if pending && flush != nil {
		flush()
	}
}

func (p *parallelExecutionState) isRetired() bool {
	if p == nil {
		return true
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.retired
}

// startParallelSaver arms the coalescing saver for one invocation (#561).
// Both the fresh and the resumed path cross ensureParallelInvocation before
// any branch goroutine starts, so the set-once here never races a branch.
func (e *Engine) startParallelSaver(parent *runState, parallel *parallelExecutionState) {
	if parallel == nil || parent == nil || e.parallelSaveWindow <= 0 {
		return
	}
	parallel.mu.Lock()
	defer parallel.mu.Unlock()
	if parallel.saveKick != nil {
		return
	}
	parallel.saveWindow = e.parallelSaveWindow
	parallel.saveKick = make(chan struct{}, 1)
	parallel.saveStop = make(chan struct{})
	parallel.saveDone = make(chan struct{})
	parallel.flush = func() { e.persistParallelSnapshotLocked(parent, parallel) }
	go parallel.saverLoop(parent.ctx)
}

// saverLoop writes at most one durable record per window: a kick parks it
// until the window closes, and the write then carries whatever the
// invocation looks like at write time. Virtual-time tests drive the window
// through testing/synctest; retire stops the loop before its own flush.
//
// The flushes ride parent.ctx: after a cancellation a ctx-honouring store
// refuses them, so a cancelled run can lose the final window's boundaries —
// bounded by the window, within the same best-effort contract as every
// branch checkpoint. The window likewise bounds the spend gap: a branch's
// CostUSD cursor becomes durable within one window of its booking instead
// of at the boundary itself.
func (p *parallelExecutionState) saverLoop(ctx context.Context) {
	defer close(p.saveDone)
	p.mu.Lock()
	window := p.saveWindow
	p.mu.Unlock()
	for {
		select {
		case <-p.saveStop:
			return
		case <-ctx.Done():
			return
		case <-p.saveKick:
		}
		timer := time.NewTimer(window)
		select {
		case <-timer.C:
		case <-p.saveStop:
			timer.Stop()
			return
		case <-ctx.Done():
			timer.Stop()
			return
		}
		p.saveMu.Lock()
		p.flushIfDirty()
		p.saveMu.Unlock()
	}
}

// requestParallelSave records a routine branch boundary (#561). Under a
// save window the boundary marks the invocation dirty and kicks the saver —
// siblings finishing together share one durable write — without one it
// degrades to the synchronous write the pre-coalescing code made. Ordering
// is unchanged either way: every durable write serializes under saveMu and
// carries the state current at write time, so an older boundary can never
// overwrite a newer one.
func (e *Engine) requestParallelSave(parent *runState, parallel *parallelExecutionState, branchID, nodeID string) {
	if parallel == nil {
		return
	}
	parallel.mu.Lock()
	parallel.dirtyGen++
	window := parallel.saveWindow
	parallel.mu.Unlock()
	if window <= 0 {
		e.persistParallelCheckpointLocked(parent, parallel, branchID, nodeID)
		return
	}
	select {
	case parallel.saveKick <- struct{}{}:
	default: // a kick is already parked; the saver writes the newest state
	}
}

// persistParallelSnapshotLocked snapshots the invocation and writes the run
// record. The caller holds parallel.saveMu and the invocation has a pending
// boundary (flushIfDirty checked it); the saver and retire's flush both
// come through here.
func (e *Engine) persistParallelSnapshotLocked(parent *runState, parallel *parallelExecutionState) {
	ps := parallel.snapshot()
	if ps == nil {
		return
	}
	cp := buildCheckpointWithoutParallel(parent, ps.RouterNodeID)
	cp.Parallel = ps
	if ps.PendingInteractionID != "" {
		cp.InteractionID = ps.PendingInteractionID
		cp.InteractionQuestions = deepCopyAnyMap(ps.PendingInteractionQuestions)
	}
	if err := e.store.SaveCheckpoint(parent.ctx, parent.runID, cp); err != nil {
		if e.logger != nil {
			e.logger.Error("failed to save coalesced parallel checkpoint: %v", err)
		}
		return
	}
	parallel.markWritten()
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
// struct assignment. The maps below are immutable for the lifetime of a
// fan-out by the runState concurrency contract. Branch-private mutable maps
// are replaced by newBranchRunState immediately after this clone.
func cloneRunStateForBranch(parent *runState) *runState {
	if parent == nil {
		return &runState{}
	}
	local := &runState{
		ctx:                         parent.ctx,
		runID:                       parent.runID,
		runInputs:                   parent.runInputs,
		vars:                        parent.vars,
		outputs:                     parent.outputs,
		inheritedOutputs:            parent.inheritedOutputs,
		artifacts:                   parent.artifacts,
		artifactOwners:              parent.artifactOwners,
		artifactRevisions:           parent.artifactRevisions,
		loopCounters:                parent.loopCounters,
		loopOverrides:               parent.loopOverrides,
		permissionGrants:            parent.permissionGrants,
		loopPreviousOutput:          parent.loopPreviousOutput,
		loopCurrentOutput:           parent.loopCurrentOutput,
		loopProgressSig:             parent.loopProgressSig,
		loopStaleness:               parent.loopStaleness,
		loopBudgetMarks:             parent.loopBudgetMarks,
		correctionScope:             parent.correctionScope,
		enclosingLoopCounters:       cloneMap(parent.enclosingLoopCounters),
		enclosingLoopPreviousOutput: copyOutputs(parent.enclosingLoopPreviousOutput),
		roundRobinCounters:          cloneMap(parent.roundRobinCounters),
		selectedIncoming:            parent.selectedIncoming,
		parallel:                    parent.parallel,
		events:                      parent.events,
		artifactVersions:            parent.artifactVersions,
		nodeSessions:                cloneNodeSessions(parent.nodeSessions),
		sessionLedger:               parent.sessionLedger,
		pauseSessionRef:             parent.pauseSessionRef,
		lastGraceNode:               parent.lastGraceNode,
		lastGraceDim:                parent.lastGraceDim,
		gateAnchors:                 cloneMap(parent.gateAnchors),
		lastWorkspaceSnapshot:       parent.lastWorkspaceSnapshot,
		lastSnapshotCommit:          parent.lastSnapshotCommit,
		preMarked:                   cloneMap(parent.preMarked),
		stopCaptured:                cloneMap(parent.stopCaptured),
		resumed:                     parent.resumed,
		budget:                      parent.budget,
		// The branch is part of the SAME run: it inherits the run's clock,
		// not a fresh one. A zero startedAt here would make
		// `{{run.elapsed_seconds}}` inside a branch report the age of the
		// Unix epoch.
		startedAt:          parent.startedAt,
		resourceSemaphores: parent.resourceSemaphores,
		costUSDTotal:       parent.costUSDTotal,
		nodeAttempts:       cloneNodeAttempts(parent.nodeAttempts),
		attachments:        parent.attachments,
		resumeBackend:      parent.resumeBackend,
		isWorktree:         parent.isWorktree,
	}
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
			ArtifactOwners:     make(map[string]string),
			ArtifactVersions:   cloneMap(artifactVersions),
			LoopCounters:       make(map[string]int),
			LoopPreviousOutput: make(map[string]map[string]any),
			LoopCurrentOutput:  make(map[string]map[string]any),
			LoopBudgetMarks:    make(map[string]map[string]float64),
			SelectedIncoming:   make(map[string][]store.IncomingEdge),
			SettledIncoming:    make(map[string][]store.IncomingEdge),
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

// matchesInvocation reports whether the run is re-entering the same durable
// router invocation. Unlike matches it intentionally ignores the branch set:
// callers use it before re-planning branches only to suppress duplicate router
// lifecycle events on resume; ensureParallelInvocation still performs the
// strict branch-set validation afterward.
func (p *parallelExecutionState) matchesInvocation(routerNodeID, invocationKey string) bool {
	if p == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cp != nil && p.cp.RouterNodeID == routerNodeID && p.cp.InvocationKey == invocationKey
}

func (e *Engine) resumingParallelInvocation(rs *runState, routerNodeID string) bool {
	return rs != nil && rs.parallel != nil && rs.parallel.matchesInvocation(
		routerNodeID,
		e.parallelInvocationKey(routerNodeID, rs.loopCounters),
	)
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

// raiseBranchCostUSD moves ONE branch's durable cost cursor and nothing else.
//
// It exists for the accounting-only write (persistBranchSpend): the branch's
// in-memory cumulative is what the daily-cap ledger was told, and a resume
// re-seeds a branch from this field — so leaving it behind hands the resumed
// branch a cursor lower than the ledger's monotonic-max entry, and every
// contribution it makes afterwards is silently discarded until the recomputed
// cumulative overtakes the figure the failed attempt already wrote.
// Rebuilding the whole entry instead would persist linear cursor, output and
// loop progress that no branch boundary blessed.
//
// Monotonic for the same reason store.AddSpend is: a cursor may only rise.
func (p *parallelExecutionState) raiseBranchCostUSD(branchID string, costUSD float64) bool {
	if p == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.retired || p.cp == nil {
		return false
	}
	branch := p.cp.Branches[branchID]
	if branch == nil || costUSD <= branch.CostUSD {
		return false
	}
	branch.CostUSD = costUSD
	return true
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
	p.pruneArtifactAllocationsLocked(branch.BranchID)
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
	p.pruneArtifactAllocationsLocked(branch.BranchID)
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
	p.pruneArtifactAllocationsLocked(branch.BranchID)
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
func (p *parallelExecutionState) beginPause(branchID, nodeID string, branch *store.BranchCheckpoint) (elected, parked bool) {
	if p == nil {
		return false, false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.retired {
		return false, false
	}
	if p.cp.PendingBranchID != "" {
		if p.cp.PendingBranchID != branchID || p.cp.PendingNodeID != nodeID {
			// The sibling that owns the interaction remains pending, but this
			// branch has still reached a durable restart boundary. Parking its
			// gate cursor prevents the linear prefix from replaying on every
			// subsequent sibling resume.
			p.pruneArtifactAllocationsLocked(branchID)
			p.cp.Branches[branchID] = cloneBranchCheckpoint(branch)
			return false, true
		}
		// Re-prompting the elected gate (for example after schema-invalid
		// answers) replaces its durable cursor and clears ResumeAnswers.
		p.pruneArtifactAllocationsLocked(branchID)
		p.cp.Branches[branchID] = cloneBranchCheckpoint(branch)
		return true, true
	}
	p.cp.PendingBranchID = branchID
	p.cp.PendingNodeID = nodeID
	p.pruneArtifactAllocationsLocked(branchID)
	p.cp.Branches[branchID] = cloneBranchCheckpoint(branch)
	return true, true
}

// pruneArtifactAllocationsLocked drops retry-stability keys whose effects are
// already covered by a durable branch cursor. NextArtifactVersion remains the
// monotonic allocator; if the branch later publishes again it receives a fresh
// version, while old loop-iteration execution keys cannot grow the checkpoint
// for the lifetime of a wide fan-out. p.mu must be held by the caller.
func (p *parallelExecutionState) pruneArtifactAllocationsLocked(branchID string) {
	if p == nil || p.cp == nil || len(p.cp.ArtifactAllocations) == 0 {
		return
	}
	prefix := branchID + "/"
	for key := range p.cp.ArtifactAllocations {
		if strings.HasPrefix(key, prefix) {
			delete(p.cp.ArtifactAllocations, key)
		}
	}
}

// abortPause releases an election whose interaction could not be durably
// paused. A sibling may then elect its own gate, and no branch can mistake the
// failed attempt for a real paused run. It deliberately preserves the branch
// cursor written by beginPause so retry/recovery still starts at the gate.
func (p *parallelExecutionState) abortPause(branchID, nodeID string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cp.PendingBranchID != branchID || p.cp.PendingNodeID != nodeID {
		return
	}
	p.cp.PendingBranchID = ""
	p.cp.PendingNodeID = ""
	p.cp.PendingInteractionID = ""
	p.cp.PendingInteractionQuestions = nil
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
		ArtifactOwners:     cloneMap(src.ArtifactOwners),
		ArtifactVersions:   cloneMap(src.ArtifactVersions),
		ArtifactRevisions:  cloneMap(src.ArtifactRevisions),
		LoopCounters:       cloneMap(src.LoopCounters),
		LoopPreviousOutput: copyOutputs(src.LoopPreviousOutput),
		LoopCurrentOutput:  copyOutputs(src.LoopCurrentOutput),
		LoopBudgetMarks:    cloneNestedFloatMap(src.LoopBudgetMarks),
		SelectedIncoming:   cloneIncoming(src.SelectedIncoming),
		SettledIncoming:    cloneIncoming(src.SettledIncoming),
		JoinNodeID:         src.JoinNodeID,
		TerminalNodeID:     src.TerminalNodeID,
		Completed:          src.Completed,
		TerminatedAtDone:   src.TerminatedAtDone,
		CostUSD:            src.CostUSD,
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
