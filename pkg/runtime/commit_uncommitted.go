package runtime

import (
	"context"
	"fmt"
	"strings"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/treenoise"
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

	porcelain, err := runGit(r.WorkDir, "status", "--porcelain")
	if err != nil {
		return fmt.Errorf("runtime: commit-uncommitted: probe workdir: %w (output: %s)", err, strings.TrimSpace(porcelain))
	}
	// The probe agrees with THIS gesture's staging (verdict 5, R138690): a
	// worktree whose only dirt is a tracked-and-modified devbox.lock is a
	// lock-only bump the merge-destined commit carries — refusing it here
	// left the studio's salvage action no path to bank it. Only the mirror
	// is set aside; the wip bank keeps the fuller IsNoise probe.
	if len(commitWorkPaths(porcelain)) == 0 {
		if strings.TrimSpace(porcelain) != "" {
			return fmt.Errorf("runtime: commit-uncommitted: workdir %q is dirty with tree noise only — nothing of the run's to commit (see git status)", r.WorkDir)
		}
		return fmt.Errorf("runtime: commit-uncommitted: workdir %q has no changes to commit", r.WorkDir)
	}

	if err := runGitInDir(r.WorkDir, commitStageArgs()...); err != nil {
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

// stageWorkArgs stages the whole tree EXCEPT the canonical tree noise
// (pkg/treenoise): the `.claude/` mirror and a drifted devbox.lock are not
// the pass's work, and the WIP BANK's staging gesture agrees with the
// cleanliness probe — never merged, the lock is derivable from devbox.json.
// The operator-initiated commit-and-finalize deliberately disagrees: its
// commit is merge-destined, so it stages a tracked-and-modified lock (see
// commitStageArgs).
func stageWorkArgs() []string {
	args := []string{"add", "-A", "--", ":/"}
	return append(args, treenoise.Pathspecs()...)
}

// commitStageArgs stages the tree for the OPERATOR-initiated commit-and-
// finalize: the `.claude/` mirror stays excluded (iterion wrote it, the run
// did not — deliverables under it are staged by name), but a
// tracked-and-modified devbox.lock is STAGED here, not dropped: this
// commit is merge-destined, and a dependency bot's lock bump is half its
// deliverable — dropping it would merge devbox.json without its
// resolution and destroy the bump with the worktree (verdict 3, R5478b3).
// The wip bank keeps the fuller exclusion: it is never merged, and the
// lock is derivable from devbox.json.
func commitStageArgs() []string {
	args := []string{"add", "-A", "--", ":/"}
	return append(args, treenoise.MirrorPathspec())
}

// commitWorkPaths returns the porcelain entries the OPERATOR-initiated
// commit-and-finalize would stage: everything except the engine's own
// mirror — a tracked-and-modified devbox.lock IS the dependency work half
// the merge-destined commit carries (verdict 3, R5478b3). The probe must
// agree with this gesture, not with the wip bank's (verdict 5, R138690).
func commitWorkPaths(porcelain string) []string {
	var out []string
	for _, path := range porcelainPaths(porcelain) {
		if !treenoise.IsMirror(path) {
			out = append(out, path)
		}
	}
	return out
}

// porcelainPaths normalizes `git status --porcelain` output into the paths
// it reports: rename arrows cut to the destination, outer quotes stripped.
func porcelainPaths(porcelain string) []string {
	var out []string
	for _, line := range strings.Split(porcelain, "\n") {
		if len(line) < 4 {
			continue
		}
		path := line[3:]
		if i := strings.Index(path, " -> "); i >= 0 {
			path = path[i+4:]
		}
		path = strings.TrimSpace(path)
		if len(path) >= 2 && strings.HasPrefix(path, "\"") && strings.HasSuffix(path, "\"") {
			path = path[1 : len(path)-1]
		}
		if path != "" {
			out = append(out, path)
		}
	}
	return out
}

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
	for _, path := range porcelainPaths(porcelain) {
		if !treenoise.IsNoise(path) {
			out = append(out, path)
		}
	}
	return out
}

// noisePaths returns the porcelain paths the noise list sets aside — the
// complement of runOutputPaths. The wip bank's warn line names them, so a
// banked commit never silently swallows what it excluded (verdict 8).
func noisePaths(porcelain string) []string {
	work := map[string]bool{}
	for _, p := range runOutputPaths(porcelain) {
		work[p] = true
	}
	var out []string
	for _, p := range porcelainPaths(porcelain) {
		if !work[p] {
			out = append(out, p)
		}
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
