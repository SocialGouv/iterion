package store

import (
	"context"
	"fmt"
)

// RunOutcomeExpectation is a mutation preimage, never execution authority.
type RunOutcomeExpectation struct {
	Version int64
	Epoch   string
}

func (e RunOutcomeExpectation) Validate() error {
	if e.Version < 0 || e.Epoch == "" {
		return fmt.Errorf("invalid outcome preimage")
	}
	return nil
}
func (e RunOutcomeExpectation) Matches(r *Run) bool {
	return r != nil && r.CASVersion == e.Version && r.ExecutionEpoch == e.Epoch
}

type RunOutcomeCASStore interface {
	UpdateRunOutcomeIf(context.Context, string, RunOutcomeExpectation, RunStatus, string, RunOutcomeMeta, []RunStatus) (bool, error)
}

func AsRunOutcomeCASStore(s RunStore) RunOutcomeCASStore { return capability[RunOutcomeCASStore](s) }
