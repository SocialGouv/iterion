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
	if out, err := gitCommitMessage(r.WorkDir, message); err != nil {
		return fmt.Errorf("runtime: commit-uncommitted: git commit: %w (output: %s)", err, strings.TrimSpace(out))
	}
	if logger != nil {
		logger.Info("runtime: committed uncommitted workdir for run %s", r.ID)
	}

	return RecoverFinalize(ctx, st, r, logger)
}

// workdirIsClean returns true when `git status --porcelain` reports nothing
// once iterion's OWN scaffolding is set aside. See runOutputPaths for why that
// exclusion exists and what it deliberately does not cover.
func workdirIsClean(workdir string) (bool, error) {
	out, err := runGit(workdir, "status", "--porcelain")
	if err != nil {
		return false, fmt.Errorf("git status: %w (output: %s)", err, strings.TrimSpace(out))
	}
	return len(runOutputPaths(out)) == 0, nil
}

// scaffoldPrefix is where mirrorBundleSkills lays the bot's skills inside the
// run worktree. What lives there is written BY iterion, at run start, from the
// bundle — it is not something the run produced.
const scaffoldPrefix = ".claude/"

// runOutputPaths returns the porcelain entries that stand for work the RUN
// produced, dropping the scaffolding iterion mirrored in itself.
//
// Counting that mirror as run output has a cost that is not cosmetic. Finalize
// reads a dirty tree as "the bot left work uncommitted" and banks it as a wip
// commit — and a wip-banked HEAD is NEVER merged, by design. So a lot whose
// gate CONVERGED does not land, and the cause is iterion's own bundle
// scaffolding sitting untracked beside it. Measured on a run that converged
// and whose entire wip bank was 638 lines of mirrored skill files.
//
// `.git/info/exclude` is not the place to fix it: git reads that file from the
// COMMON dir, so a linked worktree cannot carry its own, and writing there
// would silently start ignoring `.claude/` in the operator's checkout too.
//
// A repository that TRACKS files under `.claude/` keeps what matters: those are
// its own committed files, and the run modifying them is real output — but a
// modification iterion itself made by mirroring is exactly what must not be
// mistaken for one. The exclusion is scoped to this decision and nothing else:
// it never removes a file, and never touches what the run committed.
func runOutputPaths(porcelain string) []string {
	var out []string
	for _, line := range strings.Split(porcelain, "\n") {
		if len(line) < 4 {
			continue
		}
		// Porcelain v1 is `XY<space><path>`, and a rename reads
		// `<old> -> <new>`. The destination is what exists on disk, so it
		// is the one that decides.
		path := line[3:]
		if i := strings.Index(path, " -> "); i >= 0 {
			path = path[i+4:]
		}
		// Non-ASCII paths come back quoted under core.quotePath. Only the
		// quoting is stripped; the C-style escapes inside are left as git
		// wrote them, since nothing here needs to open the file.
		path = strings.TrimSpace(path)
		if len(path) >= 2 && strings.HasPrefix(path, "\"") && strings.HasSuffix(path, "\"") {
			path = path[1 : len(path)-1]
		}
		if path == "" || strings.HasPrefix(path, scaffoldPrefix) {
			continue
		}
		out = append(out, path)
	}
	return out
}

func runGitInDir(workdir string, args ...string) error {
	out, err := runGit(workdir, args...)
	if err != nil {
		return fmt.Errorf("%w (output: %s)", err, strings.TrimSpace(out))
	}
	return nil
}
