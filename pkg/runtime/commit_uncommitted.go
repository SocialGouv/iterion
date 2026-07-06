package runtime

import (
	"context"
	"fmt"
	"strings"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// CommitUncommittedAndFinalize stages every change in a run's worktree
// (`git add -A`), commits with the operator-supplied message, then
// re-runs the worktree finalization so FinalCommit / FinalBranch land
// on the run record. Existing /merge UX takes over from there.
//
// Use case: bots that finish a work session without committing (e.g.
// whole_improve_loop's reviewer/fixer pairs leave a dirty workdir
// without a prepare_commit step). The operator can salvage the work
// from the run page instead of having to commit by hand in the
// workspace directory.
//
// Idempotence:
//   - bails when the run isn't a worktree run (nothing to finalize).
//   - bails when FinalBranch is already set (the run was already
//     finalized; the operator should use /merge instead).
//   - bails when the workdir is clean (no diff to commit).
//
// Safety:
//   - the message is operator-supplied; the runtime does no further
//     transformation beyond passing it to `git commit -m`.
//   - `git add -A` honors the project's .gitignore — untracked
//     sandbox runtime artifacts that the project has ignored stay
//     out. Untracked files NOT in .gitignore (e.g. the bot's new
//     ADR) are committed; surface this in the studio so the
//     operator can adjust .gitignore beforehand if needed.
func CommitUncommittedAndFinalize(
	ctx context.Context,
	st store.RunStore,
	r *store.Run,
	message string,
	logger *iterlog.Logger,
) error {
	if r == nil {
		return fmt.Errorf("runtime: commit-uncommitted: nil run")
	}
	if !r.Worktree || r.WorkDir == "" {
		return fmt.Errorf("runtime: commit-uncommitted: run %q is not a worktree run", r.ID)
	}
	if r.FinalBranch != "" || r.FinalCommit != "" {
		return fmt.Errorf("runtime: commit-uncommitted: run %q is already finalized (FinalBranch=%q, FinalCommit=%q) — use /merge instead", r.ID, r.FinalBranch, r.FinalCommit)
	}
	if strings.TrimSpace(message) == "" {
		return fmt.Errorf("runtime: commit-uncommitted: commit message is required")
	}

	clean, err := workdirIsClean(r.WorkDir)
	if err != nil {
		return fmt.Errorf("runtime: commit-uncommitted: probe workdir: %w", err)
	}
	if clean {
		return fmt.Errorf("runtime: commit-uncommitted: workdir %q has no changes to commit", r.WorkDir)
	}

	if err := runGitInDir(r.WorkDir, "add", "-A"); err != nil {
		return fmt.Errorf("runtime: commit-uncommitted: git add: %w", err)
	}
	if err := runGitInDir(r.WorkDir, "commit", "-m", message); err != nil {
		return fmt.Errorf("runtime: commit-uncommitted: git commit: %w", err)
	}
	if logger != nil {
		logger.Info("runtime: committed uncommitted workdir for run %s", r.ID)
	}

	return RecoverFinalize(ctx, st, r, logger)
}

// workdirIsClean returns true when `git status --porcelain` produces
// no output — i.e. no modifications, no staged changes, no untracked
// files that aren't gitignored.
func workdirIsClean(workdir string) (bool, error) {
	out, err := runGit(workdir, "status", "--porcelain")
	if err != nil {
		return false, fmt.Errorf("git status: %w (output: %s)", err, strings.TrimSpace(out))
	}
	return strings.TrimSpace(out) == "", nil
}

func runGitInDir(workdir string, args ...string) error {
	out, err := runGit(workdir, args...)
	if err != nil {
		return fmt.Errorf("%w (output: %s)", err, strings.TrimSpace(out))
	}
	return nil
}
