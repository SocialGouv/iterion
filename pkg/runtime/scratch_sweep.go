package runtime

import (
	"time"

	"github.com/SocialGouv/iterion/pkg/memory"
)

// sweepScratchOnExit reclaims the workspace's out-of-tree scratch entries
// that nothing has written to within the retention window. Best-effort:
// a run that produced real work must never fail because a cleanup did.
//
// WHY AGE, AND NOT "this run's files"
//
// ${PROJECT_SCRATCH_DIR} is per-WORKSPACE, deliberately shared between
// runs: a subbot child executes in its own container and writes into its
// PARENT's scratch, which is how a fan-in reads what the children
// produced. So a run cannot claim any entry as its own and delete it on
// the way out — doing that at the end of a child would delete the fan-in
// its parent is still waiting for, and doing it at the end of a parent
// would race a sibling launched a second earlier.
//
// What can be said without owning anything: a directory nothing has
// written to in days belongs to no run that is still working, because a
// run that is still working writes as it goes. That also leaves resume
// intact for any run interrupted recently — scratch survives the
// container precisely so a crashed run picks its working state back up.
//
// The retention is the operator's: ITERION_SCRATCH_RETENTION widens it,
// narrows it, or turns the sweep off entirely.
func (e *Engine) sweepScratchOnExit(runID string) {
	retention, err := memory.ScratchRetentionFromEnv(memory.DefaultScratchRetention)
	if err != nil {
		if e.logger != nil {
			e.logger.Warn("runtime: scratch sweep: %v", err)
		}
		return
	}
	if retention <= 0 {
		return // explicitly disabled
	}

	base := e.repoRoot
	if base == "" {
		base = e.workDir
	}
	root := memory.WorkspaceScratchDir(base)
	if root == "" {
		return
	}

	swept, freed, err := memory.SweepScratch(root, retention, time.Now(), false)
	if err != nil && e.logger != nil {
		e.logger.Warn("runtime: scratch sweep: %v", err)
	}
	if len(swept) > 0 && e.logger != nil {
		e.logger.Info("runtime: scratch sweep: reclaimed %d dir(s), %.1f MiB untouched for %s (run %s)",
			len(swept), float64(freed)/(1024*1024), retention, runID)
	}
}
