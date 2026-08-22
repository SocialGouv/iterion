package worktreepool

import "time"

// ScanOptions decides which entries a scan admits and how it dates them.
// It carries no I/O of its own: a scan classifies, it never deletes.
type ScanOptions struct {
	// Level is how much the caller is willing to lose. It gates nothing
	// during classification — every entry is classified in full — it only
	// decides which verdicts are reported as candidates.
	Level Level
	// OlderThan spares anything touched more recently.
	OlderThan time.Duration
	// KeepLast is a floor on what survives, applied per store: an
	// operator who asks to keep the last 10 means 10 of THIS project's.
	KeepLast int
	// IncludeResumable gives up the ability to resume the runs whose
	// worktrees are swept. Off by default.
	IncludeResumable bool
	Now              func() time.Time // test seam; nil = time.Now

	// MeasureSpared walks the tree of entries the scan refuses, so a
	// report can say how much a higher level would free. It is what
	// `iterion clean` is for and pure cost for the pool bound, which
	// deletes or warns and never offers a next step by size.
	MeasureSpared bool
	// Admit overrides what a verdict has to be for a deletion to be
	// allowed. Nil means the Level ladder — see admission.go for why the
	// pool bound needs a different rule over the same facts.
	Admit admission
}

// now resolves the clock once, so a nil seam does not have to be checked
// at every call site.
func (o ScanOptions) now() func() time.Time {
	if o.Now != nil {
		return o.Now
	}
	return time.Now
}

// SweepOptions is a scan's admission rules plus what a deletion may take.
type SweepOptions struct {
	ScanOptions
	// Apply turns the pass from a report into deletions.
	Apply bool
	// WithRuns deletes the run record paired with each worktree taken.
	// The automatic pool bound never sets it: a run's record is the
	// operator's history and costs kilobytes, while its worktree is a
	// full checkout — only one of the two is worth reclaiming behind
	// their back.
	WithRuns bool

	// Remove is the seam the sweep deletes through; nil uses
	// RemoveAllForce. A removal that fails for a reason a single uid
	// cannot arrange — another owner's files, a busy mount — is exactly
	// the case the continuation contract exists for, so tests substitute
	// it rather than approximate it.
	Remove func(string) error
	// DuringEligibility and AfterEligibility fire in the two halves of
	// the window between the re-derivation and the removal — the window
	// in which a concurrent sweep can take the directory. Two different
	// checks cover those halves, and each needs a place a test can stand
	// to prove it is load-bearing. Both are nil in production.
	DuringEligibility func(string)
	AfterEligibility  func(string)
}

// remove resolves the deletion seam.
func (o SweepOptions) remove() func(string) error {
	if o.Remove != nil {
		return o.Remove
	}
	return RemoveAllForce
}

func (o SweepOptions) during(path string) {
	if o.DuringEligibility != nil {
		o.DuringEligibility(path)
	}
}

func (o SweepOptions) after(path string) {
	if o.AfterEligibility != nil {
		o.AfterEligibility(path)
	}
}
