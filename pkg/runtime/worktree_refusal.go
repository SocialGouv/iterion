package runtime

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

// reclaimEarlyRefusalWorktree removes only a terminal run's untouched
// checkout. Compute/fail nodes may have recorded decisions and checkpoints,
// but no executor, child, human gate or parallel branch may have started.
// Keep the baseline ref and an explicit recovery marker so an operator can
// still rewind the refusal without borrowing today's repository HEAD.
func (e *Engine) reclaimEarlyRefusalWorktree(ctx context.Context, runID string, wc *worktreeContext, cleanup func()) bool {
	if cleanup == nil || wc.originalTip == "" || e.store.Root() == "" {
		return false
	}
	lock, err := lockPristineWorktree(e.store, runID)
	if err != nil {
		return false
	}
	defer func() { _ = lock.Unlock() }()
	run, err := e.store.LoadRun(ctx, runID)
	if err != nil || run.Status != store.RunStatusFailed || !run.Worktree || !samePath(run.WorkDir, wc.wtPath) || !ownedPristineWorktreePath(e.store, run) {
		return false
	}
	if run.BaseCommit != wc.originalTip || !samePath(run.RepoRoot, wc.repoRoot) {
		return false
	}
	// Rewind compiles the recorded source before claiming the run. Keep a
	// checkout that is itself the only recorded location of that source.
	for _, source := range []string{run.FilePath, run.BundlePath} {
		if source != "" {
			if rel, err := filepath.Rel(wc.wtPath, source); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				return false
			}
		}
	}
	events, err := e.store.LoadEvents(ctx, runID)
	if err != nil {
		return false
	}
	for _, ev := range events {
		if ev.Type != store.EventNodeStarted {
			continue
		}
		if ev.BranchID != "" {
			return false
		}
		kind, _ := ev.Data["kind"].(string)
		switch e.workflow.Nodes[ev.NodeID].(type) {
		case *ir.ComputeNode:
			if kind != "compute" {
				return false // a forced source edit cannot rewrite the history
			}
		case *ir.FailNode:
			if kind != "" && kind != "fail" {
				return false
			}
		default:
			return false
		}
	}
	if readHEAD(wc.wtPath) != wc.originalTip {
		return false
	}
	// Include ignored files: a clean ordinary git status says nothing about
	// an ignored crash log. Only the engine's own skill scaffold is omitted,
	// using the same rule as successful worktree finalization — and the
	// FULL noise list is right here, unlike the merge-destined commit path:
	// a release restores the baseline by design, so a drifted devbox.lock
	// (which re-derives from devbox.json on the next devbox run) is not work
	// this run is keeping.
	out, err := runGit(wc.wtPath, "status", "--porcelain", "--ignored=matching", "--untracked-files=all")
	if err != nil || len(runOutputPaths(out)) != 0 {
		return false
	}
	if _, err := runGit(wc.wtPath, "update-ref", pristineWorktreeRef(runID), wc.originalTip); err != nil {
		return false
	}
	// Persist BEFORE removal: a crash may leave a marked checkout in place,
	// which restoration accepts; it must never leave an unmarked missing one.
	run.WorktreeReclaimed = true
	if err := e.store.SaveRun(ctx, run); err != nil {
		return false
	}
	cleanup()
	if _, err := os.Lstat(wc.wtPath); !os.IsNotExist(err) {
		return false // cleanup logs its own failure; retain the recovery marker
	}
	if e.logger != nil {
		e.logger.Info("runtime: pristine worktree released after an early terminal refusal; rewind restores baseline %.9s: %s", wc.originalTip, wc.wtPath)
	}
	return true
}

func pristineWorktreeRef(runID string) string {
	return "refs/iterion/runs/" + runID + "/pristine-worktree"
}

func lockPristineWorktree(st store.RunStore, runID string) (store.RunLock, error) {
	// Separate from LockRun, which a host may already hold for the whole
	// execution. This short lock serializes deletion and reconstruction even
	// when a rewind observes the terminal event before Run's defers finish.
	return store.AcquireFileLock(filepath.Join(st.Root(), "runs", runID, ".worktree-lifecycle.lock"), "worktree lifecycle "+runID)
}

func ownedPristineWorktreePath(st store.RunStore, run *store.Run) bool {
	return st.Root() != "" && run.WorkDir != "" && run.RepoRoot != "" && run.BaseCommit != "" &&
		samePath(run.WorkDir, filepath.Join(st.Root(), "worktrees", run.ID)) &&
		!samePath(run.WorkDir, run.RepoRoot)
}

// RestoreReclaimedWorktree reconstructs an explicitly reclaimed pristine
// checkout for an operator rewind or resume. It never heals an unmarked lost
// directory or a broken .git pointer. The caller owns the run's mutation lease.
func RestoreReclaimedWorktree(ctx context.Context, st store.RunStore, run *store.Run) error {
	if !run.WorktreeReclaimed {
		return nil
	}
	if !run.Worktree || !ownedPristineWorktreePath(st, run) {
		return fmt.Errorf("runtime: refused to restore reclaimed worktree outside its recorded store-owned path")
	}
	lock, err := lockPristineWorktree(st, run.ID)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Unlock() }()
	info, err := os.Lstat(run.WorkDir)
	if os.IsNotExist(err) {
		base, refErr := runGit(run.RepoRoot, "rev-parse", "--verify", pristineWorktreeRef(run.ID)+"^{commit}")
		if refErr != nil || strings.TrimSpace(base) != run.BaseCommit {
			return fmt.Errorf("runtime: reclaimed worktree baseline is missing or differs from recorded commit %s", run.BaseCommit)
		}
		if _, addErr := runGit(run.RepoRoot, "worktree", "add", "--detach", run.WorkDir, run.BaseCommit); addErr != nil {
			return fmt.Errorf("runtime: restore reclaimed worktree: %w", addErr)
		}
	} else if err != nil || !info.IsDir() {
		return fmt.Errorf("runtime: reclaimed worktree path is not an accessible directory: %s", run.WorkDir)
	}
	if err := checkWorktreeLinkage(run.WorkDir); err != nil {
		return err
	}
	root, err := findGitRoot(run.WorkDir)
	if err != nil || !samePath(root, run.RepoRoot) || readHEAD(run.WorkDir) != run.BaseCommit {
		return fmt.Errorf("runtime: reclaimed worktree no longer matches its recorded repository and baseline")
	}
	// Clear before any execution: a later accidental deletion must not be
	// mistaken for this deliberate reclamation and lose newly produced work.
	run.WorktreeReclaimed = false
	if err := st.SaveRun(ctx, run); err != nil {
		return fmt.Errorf("runtime: clear reclaimed worktree marker: %w", err)
	}
	return nil
}
