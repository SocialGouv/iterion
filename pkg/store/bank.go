package store

// BankState describes delivery of the run's commits independently of the
// workflow outcome. Empty means the final fields do not establish an outcome
// yet (including a workless run); it is not a bank failure.
type BankState string

const (
	BankStateReady  BankState = "banked"
	BankStateFailed BankState = "bank_failed"
)

// BankState projects existing durable fields, so older runs and both stores
// have the same answer without backfilling a second authority.
func (r *Run) BankState() BankState {
	if r == nil {
		return ""
	}
	if r.FinalBranchError != "" {
		return BankStateFailed
	}
	if r.FinalBranch != "" && r.FinalCommit != "" {
		return BankStateReady
	}
	return ""
}
