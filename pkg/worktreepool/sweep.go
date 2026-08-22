package worktreepool

import (
	"fmt"
	"os"
)

// SweepResult is what one pass took, spared, and could not take.
type SweepResult struct {
	Deleted             []Entry
	Spared              []Entry
	Failed              []Entry
	BytesReclaimed      int64
	RegistrationsPruned int
	// Errors are the individual failures. A failure never aborts the
	// pass: returning early would strand the deletions already made with
	// no report at all — the caller would read an error and have no way
	// to learn what is already gone.
	Errors []error
}

// Sweep deletes every entry a scan left as a candidate, in the order
// given, and reports what happened to each one.
//
// Entries arrive already classified. That classification is a photograph,
// and the pass can take tens of seconds to reach a given entry, so every
// verdict is re-derived immediately before the deletion — the whole
// verdict, not the dirty bit: a COMMIT leaves a clean tree, so asking
// only about the working tree waves through the one change that creates
// something to lose.
func Sweep(all []Entry, opts SweepOptions) SweepResult {
	result := SweepResult{Deleted: []Entry{}, Spared: []Entry{}}

	for i := range all {
		wt := &all[i]
		if wt.SkipReason != "" {
			result.Spared = append(result.Spared, *wt)
			continue
		}
		if !opts.Apply {
			result.BytesReclaimed += wt.Bytes
			result.Deleted = append(result.Deleted, *wt)
			continue
		}

		// Hold the run's lock across the whole deletion. Re-reading the
		// status is not enough on its own: the window it closes is not an
		// instant but the entire removal, which on a real worktree
		// (node_modules, target, .venv) runs for seconds. `iterion run`
		// and `iterion resume` hold this same lock for a run's lifetime,
		// so taking it is what actually makes "a live run keeps its
		// worktree" true rather than likely.
		lock, err := lockRun(wt.StoreDir, wt.RunID)
		if err != nil {
			wt.SkipReason = SkipRunActive
			wt.Error = err.Error()
			result.Spared = append(result.Spared, *wt)
			continue
		}

		if st, ok := reloadRunStatus(wt.StoreDir, wt.RunID); ok &&
			(ownsWorktree(st) || (isResumable(st) && !opts.IncludeResumable)) {
			wt.RunStatus = string(st)
			wt.SkipReason = SkipRunActive
			if !ownsWorktree(st) {
				wt.SkipReason = SkipResumable
			}
			result.Spared = append(result.Spared, *wt)
			releaseLock(lock)
			continue
		}

		// A concurrent sweep may already have taken it. Asked before the
		// re-derivation, because inspecting a path that is gone yields
		// "git could not tell" and would file it under `unlanded` — a
		// verdict about work that is not what happened.
		if _, err := os.Lstat(wt.Path); os.IsNotExist(err) {
			// Its bytes were reclaimed by whoever removed it, not held
			// back by us: reporting them would read as "still to gain".
			wt.SkipReason, wt.Bytes = SkipVanished, 0
			result.Spared = append(result.Spared, *wt)
			releaseLock(lock)
			continue
		}

		if reason, ok := stillEligible(wt, opts.admission(), opts.during); !ok {
			// git cannot answer for a path that is no longer there, and
			// its silence reads as `unlanded` — the alarm verdict, which
			// would send an operator hunting for work that was never
			// lost. Say what actually happened instead.
			if _, err := os.Lstat(wt.Path); os.IsNotExist(err) {
				reason, wt.Bytes = SkipVanished, 0
			}
			wt.SkipReason = reason
			result.Spared = append(result.Spared, *wt)
			releaseLock(lock)
			continue
		}

		opts.after(wt.Path)

		// stillEligible spends several git calls and a full walk, so ask
		// once more: os.RemoveAll succeeds on a path that is already gone
		// and both sweeps would claim the deletion and its bytes.
		if _, err := os.Lstat(wt.Path); os.IsNotExist(err) {
			wt.SkipReason, wt.Bytes = SkipVanished, 0
			result.Spared = append(result.Spared, *wt)
			releaseLock(lock)
			continue
		}

		if err := opts.remove()(wt.Path); err != nil {
			wt.Error = err.Error()
			result.Errors = append(result.Errors, fmt.Errorf("delete worktree %s: %w", wt.Path, err))
			result.Failed = append(result.Failed, *wt)
			releaseLock(lock)
			continue
		}
		wt.Deleted = true
		result.BytesReclaimed += wt.Bytes

		if wt.gitCommonDir != "" {
			if pruned, err := pruneWorktreeRegistration(wt.gitCommonDir, wt.resolvedPath); err != nil {
				wt.RegistrationError = err.Error()
				result.Errors = append(result.Errors, fmt.Errorf("prune registration for %s: %w", wt.Path, err))
			} else if pruned {
				result.RegistrationsPruned++
			}
		}
		releaseLock(lock)

		if opts.WithRuns && wt.RunID != "" {
			switch existed, err := deleteRunRecord(wt.StoreDir, wt.RunID); {
			case err != nil:
				wt.RunError = err.Error()
				result.Errors = append(result.Errors, fmt.Errorf("delete run %s: %w", wt.RunID, err))
			case existed:
				wt.RunDeleted = true
			}
		}
		result.Deleted = append(result.Deleted, *wt)
	}
	return result
}
