package runtime

import (
	"context"
	"fmt"
	"github.com/SocialGouv/iterion/pkg/store"
)

// This carrier is scoped to one prepared queued birth without accounting.
// It carries no output-correction protocol and is masked at every Run entry.
type preparedQueuedStart struct {
	runID    string
	original store.RunExecutionExpectation
	grant    *store.RunExecutionAuthority
}
type preparedQueuedStartKey struct{}

func (e *Engine) withPreparedQueuedStart(ctx context.Context, p *store.PreparedRunInvocation) context.Context {
	var start *preparedQueuedStart
	if p != nil && !e.correctionProtocolCapable() {
		if binding, expected, ok := p.OriginalBirth(); ok && expected.Status == store.RunStatusQueued {
			start = &preparedQueuedStart{runID: binding.RunID, original: expected}
		}
	}
	return context.WithValue(ctx, preparedQueuedStartKey{}, start)
}
func (e *Engine) acquirePreparedQueuedStart(ctx context.Context, observed *store.Run, start *preparedQueuedStart) (*store.Run, error) {
	expected := start.original
	if observed == nil || observed.ID != start.runID || observed.ExecutionEpoch != expected.Epoch || observed.Status != expected.Status || observed.CASVersion < expected.Version {
		return nil, ErrAdmissionStopped
	}
	// Only metadata version follows the observed row. QueuedAt is the original
	// birth marker, encoded by the same FS/Mongo storage semantics in the CAS.
	expected.Version = observed.CASVersion
	capability := store.AsRunExecutionStore(e.store)
	if capability == nil {
		return nil, fmt.Errorf("prepared queued start: store lacks RunExecutionStore")
	}
	authority, ok, err := capability.AcquireRunExecutionIf(ctx, start.runID, expected, store.ExecutionStart)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("%w: prepared queued acquisition conflicted", store.ErrRunConflict)
	}
	if authority.RunID != start.runID || authority.Epoch == "" {
		return nil, fmt.Errorf("prepared queued start: invalid acquisition response")
	}
	// Retain only the response of this successful CAS, before any cancellable read.
	copy := authority
	start.grant = &copy
	_, fresh, err := store.LoadRunWithValueFormat(ctx, e.store, start.runID)
	if err != nil {
		return nil, err
	}
	if !authority.Matches(fresh) {
		return nil, ErrAdmissionStopped
	}
	return fresh, nil
}
