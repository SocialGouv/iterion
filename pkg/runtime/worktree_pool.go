package runtime

import (
	"context"
	"fmt"

	"github.com/SocialGouv/iterion/pkg/worktreepool"
)

// boundWorktreePool keeps `<store>/worktrees/` from growing without end.
//
// # WHY THIS RUNS HERE, ON THE PATH THAT CREATES ONE
//
// A worktree is removed on a clean exit and PRESERVED on a failure, for
// inspection — deliberately. Nothing else ever reclaims the preserved
// ones: `iterion runs prune` only touches runs/, and `iterion clean` is a
// command an operator has to know exists and remember to run. So a store
// whose runs fail grows by a full checkout of the repository per failure,
// with no ceiling and no signal. Measured on this repo: 355 MB each, of
// which 309 MB is the vendored tree every worktree faithfully copies. A
// studio left alone for forty minutes reached 32 of them and 12 GB, on a
// host where the store sat on a 16 GB tmpfs; the machine started killing
// processes, which is how anyone found out.
//
// The moment a worktree is created is the only one where acting is both
// cheap and timely: the pool is about to grow, the store is known, and the
// common case costs a single ReadDir. Startup would have been too early
// (the pool that filled the disk did not exist when the server booted)
// and a sweep on exit too late (the failures that fill it are exactly the
// runs that do not reach a clean exit).
//
// # WHAT IT WILL AND WILL NOT DO
//
// It reclaims what git proves recoverable, oldest first, and never more
// than the excess. It does NOT refuse to create the worktree: the run in
// front of us was asked for, and failing it over some other run's
// leftovers would be a limitation nobody chose. And it does not touch
// anything dirty — the uncommitted output "preserved for inspection"
// exists to keep. When it cannot get back under the ceiling it says so,
// naming the reasons and the command, because a bound that silently gives
// up is the state this replaced.
//
// Best-effort throughout: a run must never fail because a cleanup did.
func (e *Engine) boundWorktreePool(ctx context.Context, storeRoot string) {
	// Git reports absolute paths. Keep the same canonical root across the
	// classifier, operator messages and suggested cleanup command when the
	// documented `--store-dir .iterion` form is used.
	storeRoot = worktreepool.AbsPath(storeRoot)

	budget, err := worktreepool.ResolveBudget()
	if err != nil {
		// A malformed ceiling is the operator's, and it disables the
		// bound — so it is said out loud rather than defaulted over.
		if e.logger != nil {
			e.logger.Warn("runtime: worktree pool: %v — the pool is unbounded until this is fixed", err)
		}
		return
	}
	if budget <= 0 {
		return // explicitly disabled
	}

	boundCtx, cancel := context.WithTimeout(ctx, worktreepool.DefaultEnforcementTimeout)
	defer cancel()
	report, err := worktreepool.EnforceBudget(storeRoot, budget, worktreepool.SweepOptions{
		ScanOptions: worktreepool.ScanOptions{Context: boundCtx},
	})
	if err != nil && e.logger != nil {
		e.logger.Warn("runtime: worktree pool: %v", err)
	}
	if e.logger == nil {
		return
	}
	for _, sweepErr := range report.Errors {
		e.logger.Warn("runtime: worktree pool: %v", sweepErr)
	}
	if n := len(report.Reclaimed); n > 0 {
		e.logger.Info("runtime: worktree pool: reclaimed %d worktree(s), %s — %d parked and %d live left, budget %d (%s)",
			n, humanBytes(report.BytesReclaimed), report.After, report.Held, report.Budget, worktreepool.BudgetEnv)
	}
	if report.OverBudget() {
		// The one line that turns an invisible leak into something an
		// operator can act on. It carries the count, WHY the rest could
		// not be taken, and a command that would work on this pool —
		// because "you are over budget" on its own is the state this
		// replaced, only louder.
		e.logger.Warn("%s", formatWorktreePoolWarning(report, storeRoot))
	}
}

func formatWorktreePoolWarning(report worktreepool.BudgetReport, storeRoot string) string {
	msg := fmt.Sprintf("runtime: worktree pool: %d parked worktrees in %s exceed the budget of %d",
		report.After, worktreepool.PoolDir(storeRoot), report.Budget)
	if report.Held > 0 {
		msg += fmt.Sprintf(" (%d live worktrees excluded)", report.Held)
	}
	if report.Incomplete {
		msg += "; automatic classification stopped at its launch-time deadline"
	}
	if report.Limited {
		msg += "; automatic classification paused at its per-launch batch limit"
	}
	if summary := report.Summary(); summary != "" {
		msg += "; " + summary
	}
	if remedy := report.Remedy(storeRoot); remedy != "" {
		msg += fmt.Sprintf(". Review them with `%s` (add --apply to delete)", remedy)
	}
	return msg + fmt.Sprintf(". Raise or lift the budget with %s=<n> (`off` disables it).", worktreepool.BudgetEnv)
}

// humanBytes renders a size the way an operator reads one.
func humanBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
}
