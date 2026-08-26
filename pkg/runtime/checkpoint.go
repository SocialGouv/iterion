package runtime

import (
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

// buildCheckpoint creates a Checkpoint from the current runState.
func buildCheckpoint(rs *runState, nodeID string) *store.Checkpoint {
	tokens, cost, iterations, elapsed, unpricedTokens, unpricedNodes := rs.budget.Snapshot()
	return &store.Checkpoint{
		NodeID:             nodeID,
		Outputs:            rs.outputs,
		LoopCounters:       rs.loopCounters,
		RoundRobinCounters: rs.roundRobinCounters,
		LoopPreviousOutput: rs.loopPreviousOutput,
		LoopCurrentOutput:  rs.loopCurrentOutput,
		LoopBudgetMarks:    snapshotLoopBudgetMarks(rs),
		ArtifactVersions:   rs.artifactVersions,
		SelectedIncoming:   cloneIncoming(rs.selectedIncoming),
		Vars:               rs.vars,
		NodeAttempts:       serializeNodeAttempts(rs.nodeAttempts),
		// Persist run-scoped accounting so resume continues from consumed
		// budget/spend instead of a fresh allowance (see Checkpoint docs).
		BudgetTokensUsed:       tokens,
		BudgetCostUSD:          cost,
		BudgetIterationsUsed:   iterations,
		BudgetElapsedNS:        elapsed.Nanoseconds(),
		BudgetUnpricedTokens:   unpricedTokens,
		BudgetUnpricedNodes:    unpricedNodes,
		CostUSDTotal:           rs.costUSDTotal,
		NodeSessions:           cloneNodeSessions(rs.nodeSessions),
		BackendSessionStateRef: rs.pauseSessionRef,
	}
}

// cloneMap returns a shallow copy of m (nil in → nil out).
func cloneMap[K comparable, V any](m map[K]V) map[K]V {
	if m == nil {
		return nil
	}
	dst := make(map[K]V, len(m))
	for k, v := range m {
		dst[k] = v
	}
	return dst
}

// restoreBudgetAccounting seeds a resumed run's SharedBudget consumption and
// cumulative cost from the checkpoint so the resume continues from what was
// already spent instead of a fresh allowance. No-op when cp is nil (a
// from-entry restart) or the checkpoint predates these fields (all zero).
func restoreBudgetAccounting(rs *runState, cp *store.Checkpoint) {
	if cp == nil {
		return
	}
	rs.budget.Restore(cp.BudgetTokensUsed, cp.BudgetCostUSD, cp.BudgetIterationsUsed, time.Duration(cp.BudgetElapsedNS), cp.BudgetUnpricedTokens, cp.BudgetUnpricedNodes)
	rs.costUSDTotal = cp.CostUSDTotal
	// Consumption is continuous across the pause, so the persisted loop
	// prices stay comparable to it and the first crossing after a resume
	// is measured like any other.
	restoreLoopBudgetMarks(rs, cp.LoopBudgetMarks)
}

// serializeNodeAttempts converts the runState's typed-key bucket into a
// JSON-friendly map[string]map[string]int. Returns nil when the source is
// empty so checkpoints stay compact.
func serializeNodeAttempts(src map[string]map[ErrorCode]int) map[string]map[string]int {
	if len(src) == 0 {
		return nil
	}
	out := make(map[string]map[string]int, len(src))
	for nodeID, bucket := range src {
		if len(bucket) == 0 {
			continue
		}
		inner := make(map[string]int, len(bucket))
		for code, n := range bucket {
			inner[string(code)] = n
		}
		out[nodeID] = inner
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// restoreLoopSnapshots rehydrates the loop-edge snapshot maps from a
// checkpoint into the runState. Without this, a paused/failed run that
// resumes mid-loop would lose the prior-iteration `previous_output`,
// causing {{loop.<name>.previous_output}} to read nil on the next
// iteration (silent data loss in expression-form `when` clauses and
// compute nodes that depend on it).
func restoreLoopSnapshots(rs *runState, cp *store.Checkpoint) {
	if cp.LoopPreviousOutput != nil {
		rs.loopPreviousOutput = cp.LoopPreviousOutput
	}
	if cp.LoopCurrentOutput != nil {
		rs.loopCurrentOutput = cp.LoopCurrentOutput
	}
}

// restoreSelectedIncoming rehydrates the per-node selected-incoming set
// so a resumed execution of cp.NodeID applies the same with-mappings it
// would have on the first attempt. A missing field (legacy checkpoint)
// leaves the empty map newRunState allocated: incomingFor then reports
// untracked and the resolver falls back to source-output presence.
func restoreSelectedIncoming(rs *runState, cp *store.Checkpoint) {
	if cp == nil || len(cp.SelectedIncoming) == 0 {
		return
	}
	rs.selectedIncoming = cloneIncoming(cp.SelectedIncoming)
}

// restoreNodeAttempts is the inverse of serializeNodeAttempts: it rebuilds
// the typed-key map used by the recovery dispatcher from a checkpoint.
func restoreNodeAttempts(src map[string]map[string]int) map[string]map[ErrorCode]int {
	if len(src) == 0 {
		return make(map[string]map[ErrorCode]int)
	}
	out := make(map[string]map[ErrorCode]int, len(src))
	for nodeID, bucket := range src {
		inner := make(map[ErrorCode]int, len(bucket))
		for code, n := range bucket {
			inner[ErrorCode(code)] = n
		}
		out[nodeID] = inner
	}
	return out
}
