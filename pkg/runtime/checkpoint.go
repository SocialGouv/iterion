package runtime

import (
	"math"
	"reflect"
	"sort"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

// buildCheckpoint creates a Checkpoint from the current runState.
func buildCheckpoint(rs *runState, nodeID string) *store.Checkpoint {
	cp := buildCheckpointWithoutParallel(rs, nodeID)
	if rs.parallel != nil {
		cp.Parallel = rs.parallel.snapshot()
	}
	return cp
}

// buildCheckpointWithoutParallel builds the trunk portion of a checkpoint.
// Parallel callers already hold one authoritative snapshot and attach it
// themselves; keeping that path separate avoids deep-copying every branch a
// second time only to overwrite the copy immediately.
func buildCheckpointWithoutParallel(rs *runState, nodeID string) *store.Checkpoint {
	tokens, cost, iterations, elapsed, unpricedTokens, unpricedNodes := rs.budget.Snapshot()
	artifactValues, artifactOwners, artifactRevisions := snapshotArtifactState(rs.artifacts, rs.artifactOwners, rs.artifactRevisions, rs.outputs)
	cp := &store.Checkpoint{
		NodeID:                 nodeID,
		Outputs:                rs.outputs,
		LoopCounters:           rs.loopCounters,
		RoundRobinCounters:     rs.roundRobinCounters,
		LoopPreviousOutput:     rs.loopPreviousOutput,
		LoopCurrentOutput:      rs.loopCurrentOutput,
		LoopBudgetMarks:        snapshotLoopBudgetMarks(rs),
		LoopBudgetMarksV:       loopBudgetMarksVersion,
		ArtifactVersions:       rs.artifactVersions,
		Artifacts:              artifactValues,
		ArtifactOwners:         artifactOwners,
		ArtifactsKnown:         true,
		ArtifactRevisions:      artifactRevisions,
		ArtifactRevisionsKnown: true,
		SelectedIncoming:       cloneIncoming(rs.selectedIncoming),
		SettledIncoming:        cloneIncoming(rs.settledIncoming),
		Vars:                   rs.vars,
		NodeAttempts:           serializeNodeAttempts(rs.nodeAttempts),
		// Persist run-scoped accounting so resume continues from consumed
		// budget/spend instead of a fresh allowance (see Checkpoint docs).
		BudgetTokensUsed:       tokens,
		BudgetCostUSD:          cost,
		BudgetIterationsUsed:   iterations,
		BudgetElapsedNS:        elapsed.Nanoseconds(),
		BudgetUnpricedTokens:   unpricedTokens,
		BudgetUnpricedNodes:    unpricedNodes,
		CostUSDTotal:           rs.costUSDTotal,
		FiredEvents:            rs.events.snapshot(),
		NodeSessions:           cloneNodeSessions(rs.nodeSessions),
		BackendSessionStateRef: rs.pauseSessionRef,
	}
	return cp
}

// snapshotArtifactState persists the logical catalog without duplicating the
// common case where a published artifact is byte-for-byte the producer's
// checkpoint output. ArtifactOwners is the catalog in that sparse shape;
// Artifacts stores only values that cannot be reconstructed from either the
// owner's output or a verified immutable revision. Ownerless and unverified
// values remain inline.
func snapshotArtifactState(artifacts map[string]map[string]any, owners map[string]string, revisions map[string]store.ArtifactRevisionRef, outputs map[string]map[string]any) (map[string]map[string]any, map[string]string, map[string]store.ArtifactRevisionRef) {
	values := make(map[string]map[string]any)
	snapshotOwners := make(map[string]string, len(artifacts))
	snapshotRevisions := cloneMap(revisions)
	if snapshotRevisions == nil {
		snapshotRevisions = make(map[string]store.ArtifactRevisionRef)
	}
	// Preserve owner-only entries when compacting an already sparse checkpoint.
	for name, owner := range owners {
		if owner != "" {
			snapshotOwners[name] = owner
		}
	}
	for name := range artifacts {
		owner := snapshotOwners[name]
		if owner == "" {
			if revision, ok := revisions[name]; ok && revision.NodeID != "" {
				owner = revision.NodeID
				snapshotOwners[name] = owner
			}
		}
		if output, present := outputs[owner]; owner != "" && present && ArtifactValuesEqual(artifacts[name], output) {
			if revision, exact := snapshotRevisions[name]; exact {
				revision.ValueFromRevision = false
				snapshotRevisions[name] = revision
			}
			continue
		}
		if revision, exact := snapshotRevisions[name]; exact && revision.NodeID != "" && !revision.Unverified {
			revision.ValueFromRevision = true
			snapshotRevisions[name] = revision
			continue
		}
		if revision, exact := snapshotRevisions[name]; exact {
			revision.ValueFromRevision = false
			snapshotRevisions[name] = revision
		}
		values[name] = deepCopyAnyMap(artifacts[name])
	}
	return values, snapshotOwners, snapshotRevisions
}

// ArtifactValuesEqual compares JSON-shaped values across the filesystem and
// Mongo decode contracts. BSON turns integral interface values into int64,
// while artifact JSON turns them into float64; reflect.DeepEqual would treat
// those equivalent numbers as different and defeat checkpoint compaction.
func ArtifactValuesEqual(left, right any) bool {
	switch l := left.(type) {
	case map[string]any:
		r, ok := right.(map[string]any)
		if !ok || len(l) != len(r) {
			return false
		}
		for key, value := range l {
			other, present := r[key]
			if !present || !ArtifactValuesEqual(value, other) {
				return false
			}
		}
		return true
	case []any:
		r, ok := right.([]any)
		if !ok || len(l) != len(r) {
			return false
		}
		for i := range l {
			if !ArtifactValuesEqual(l[i], r[i]) {
				return false
			}
		}
		return true
	}
	if equal, numeric := equivalentJSONNumbers(left, right); numeric {
		return equal
	}
	return reflect.DeepEqual(left, right)
}

func equivalentJSONNumbers(left, right any) (equal, numeric bool) {
	lv := reflect.ValueOf(left)
	rv := reflect.ValueOf(right)
	if !lv.IsValid() || !rv.IsValid() || !isNumericKind(lv.Kind()) || !isNumericKind(rv.Kind()) {
		return false, false
	}
	switch {
	case isSignedKind(lv.Kind()):
		return signedNumberEquals(lv.Int(), rv), true
	case isUnsignedKind(lv.Kind()):
		return unsignedNumberEquals(lv.Uint(), rv), true
	default:
		return floatNumberEquals(lv.Float(), rv), true
	}
}

func isNumericKind(kind reflect.Kind) bool {
	return isSignedKind(kind) || isUnsignedKind(kind) || kind == reflect.Float32 || kind == reflect.Float64
}

func isSignedKind(kind reflect.Kind) bool {
	return kind >= reflect.Int && kind <= reflect.Int64
}

func isUnsignedKind(kind reflect.Kind) bool {
	return kind >= reflect.Uint && kind <= reflect.Uintptr
}

func signedNumberEquals(left int64, right reflect.Value) bool {
	if isSignedKind(right.Kind()) {
		return left == right.Int()
	}
	if isUnsignedKind(right.Kind()) {
		return left >= 0 && uint64(left) == right.Uint()
	}
	return integralFloatEqualsSigned(right.Float(), left)
}

func unsignedNumberEquals(left uint64, right reflect.Value) bool {
	if isSignedKind(right.Kind()) {
		return right.Int() >= 0 && left == uint64(right.Int())
	}
	if isUnsignedKind(right.Kind()) {
		return left == right.Uint()
	}
	return integralFloatEqualsUnsigned(right.Float(), left)
}

func floatNumberEquals(left float64, right reflect.Value) bool {
	if isSignedKind(right.Kind()) {
		return integralFloatEqualsSigned(left, right.Int())
	}
	if isUnsignedKind(right.Kind()) {
		return integralFloatEqualsUnsigned(left, right.Uint())
	}
	return left == right.Float()
}

func integralFloatEqualsSigned(value float64, integer int64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && math.Trunc(value) == value &&
		value >= float64(math.MinInt64) && value < -float64(math.MinInt64) && int64(value) == integer
}

func integralFloatEqualsUnsigned(value float64, integer uint64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && math.Trunc(value) == value &&
		value >= 0 && value < 2*float64(uint64(1)<<63) && uint64(value) == integer
}

// hydrateArtifactState expands the sparse checkpoint representation for the
// runtime. Only names explicitly catalogued in ArtifactOwners are rebuilt;
// an exact revision without a logical value is not enough to substitute the
// producer's newest output for an older immutable alias.
func hydrateArtifactState(artifacts map[string]map[string]any, owners map[string]string, revisions map[string]store.ArtifactRevisionRef, outputs map[string]map[string]any) {
	for name, owner := range owners {
		if _, present := artifacts[name]; present {
			continue
		}
		if revision, exact := revisions[name]; exact && revision.ValueFromRevision {
			continue
		}
		if output, present := outputs[owner]; present {
			artifacts[name] = deepCopyAnyMap(output)
		}
	}
}

// CompactCheckpointArtifactValues applies the canonical sparse logical
// snapshot to the trunk of checkpoints assembled by runview (fork/rewind)
// rather than the engine's normal checkpoint builder. Branch values stay
// expanded while V1 readers remain supported: older runners cannot hydrate a
// sparse BranchCheckpoint from ArtifactOwners.
func CompactCheckpointArtifactValues(cp *store.Checkpoint) {
	if cp == nil {
		return
	}
	cp.Artifacts, cp.ArtifactOwners, cp.ArtifactRevisions = snapshotArtifactState(
		cp.Artifacts, cp.ArtifactOwners, cp.ArtifactRevisions, cp.Outputs,
	)
}

// inferArtifactOwners recovers ownership for transitional checkpoints that
// persisted logical artifact values before they persisted ArtifactOwners.
// Only a unique value match is accepted: identical outputs from multiple
// nodes do not provide enough evidence to assign an owner safely.
func inferArtifactOwners(artifacts, outputs map[string]map[string]any, owners map[string]string) {
	nodeIDs := make([]string, 0, len(outputs))
	for nodeID := range outputs {
		nodeIDs = append(nodeIDs, nodeID)
	}
	sort.Strings(nodeIDs)
	for name, artifact := range artifacts {
		if owners[name] != "" {
			continue
		}
		match := ""
		matches := 0
		for _, nodeID := range nodeIDs {
			if !ArtifactValuesEqual(artifact, outputs[nodeID]) {
				continue
			}
			matches++
			if matches > 1 {
				break
			}
			match = nodeID
		}
		if matches == 1 {
			owners[name] = match
		}
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
	// Keep the run-state clock in step with the budget's, so the two
	// sources `{{run.elapsed_seconds}}` can read never disagree. Only the
	// budgeted shape persists an elapsed at all; without a `budget:` block
	// the checkpoint carries 0 and the resumed run measures its own attempt.
	if cp.BudgetElapsedNS > 0 {
		rs.startedAt = time.Now().Add(-time.Duration(cp.BudgetElapsedNS))
	}
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
//
// It rehydrates the settled floor in the same breath, because the two are
// only useful together: a run parked ON a convergence node whose fan-out
// produced nothing restarts from that very node without replaying the
// fan-out, so a floor left behind here would go missing on the resume
// after surviving the first attempt.
func restoreSelectedIncoming(rs *runState, cp *store.Checkpoint) {
	if cp == nil {
		return
	}
	if len(cp.SelectedIncoming) > 0 {
		rs.selectedIncoming = cloneIncoming(cp.SelectedIncoming)
	}
	if len(cp.SettledIncoming) > 0 {
		rs.settledIncoming = cloneIncoming(cp.SettledIncoming)
	}
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
