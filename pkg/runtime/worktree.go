// Package runtime — git worktree helpers for `worktree: auto` workflows.
//
// When a workflow declares `worktree: auto`, the engine creates a fresh
// git worktree at run start (under <store-dir>/worktrees/<run-id>) so the
// run executes in an isolated checkout. This decouples the run's mutations
// from the user's main working tree — WIP stays invisible, the run's
// commits land via the shared .git, and a failed run leaves the worktree
// in place for inspection.
//
// On a successful run, finalizeWorktree promotes any commits the run
// produced onto a persistent branch (default `iterion/run/<friendly>`)
// and best-effort fast-forwards the user's checked-out branch, then
// removes the worktree directory. Without that promotion the commits
// are reachable only via reflog and are eligible for GC.
package runtime

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	gitlib "github.com/SocialGouv/iterion/pkg/git"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// gitCmdTimeout bounds every git invocation made through gitCmd. Without
// it, a wedged git process (stale index.lock, a hung smudge/clean filter)
// blocks the engine goroutine running worktree setup/finalize forever —
// the run would never surface a timeout error, just hang.
const gitCmdTimeout = 60 * time.Second

// gitCmd wraps exec.Command("git", args...) with LC_ALL=C / LANG=C so
// callers can branch on stderr substrings ("already exists",
// "exists on disk, but not in") without those substrings being
// silently localized into the user's locale (fr_FR, etc).
//
// Children are detached into their own process group (Unix) so a
// SIGTERM delivered to the studio's PGID — typical when `watchexec -r`
// rebuilds the dev-mode backend during an in-flight squash merge —
// doesn't propagate and kill `git commit` mid-write with the
// "signal: terminated" failure mode observed in run_1778021294883.
//
// Returns the CancelFunc alongside the command; callers must `defer
// cancel()` immediately (releases the timeout timer once the command
// completes — the process itself is killed by the context on timeout
// regardless of whether cancel is ever called).
func gitCmd(args ...string) (*exec.Cmd, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(context.Background(), gitCmdTimeout)
	cmd := exec.CommandContext(ctx, "git", args...)
	// Strip what would override the repository this command names for itself.
	cmd.Env = append(gitlib.SanitizeEnv(os.Environ()), "LC_ALL=C", "LANG=C")
	detachGitProcessGroup(cmd)
	return cmd, cancel
}

// worktreeContext is the state captured at setupWorktree time and
// consumed by finalizeWorktree to decide whether the run actually
// produced new commits and whether a fast-forward of the user's branch
// is safe.
type worktreeContext struct {
	repoRoot       string // absolute path to the main repo (where .git lives)
	anchorDir      string // checkout the run was launched from and describes; equals repoRoot unless launched from a linked worktree
	wtPath         string // absolute path to the per-run worktree
	gitDir         string // absolute host path of the worktree's git-private dir (<repoRoot>/.git/worktrees/<basename(wtPath)>); the worktree's `.git` pointer file points here
	originalBranch string // current branch on the main worktree at run start ("" if detached)
	originalTip    string // SHA of HEAD at run start (worktree initial state)
}

// anchor is the checkout the run describes — where the operator's branch is
// checked out and where a fast-forward or squash has to land. Falls back to
// repoRoot for contexts built before anchorDir existed (tests, and the
// single-checkout case where the two are the same directory anyway).
func (wc worktreeContext) anchor() string {
	if wc.anchorDir != "" {
		return wc.anchorDir
	}
	return wc.repoRoot
}

// setupWorktree creates a fresh git worktree at
// <storeRoot>/worktrees/<runID>, checked out at HEAD of the repository
// containing repoHint (typically the engine's workDir before override).
// On success returns the worktreeContext, a cleanup closure
// (`git worktree remove --force <path>`), and nil error.
func setupWorktree(storeRoot, runID, repoHint string, logger *iterlog.Logger) (worktreeContext, func(), error) {
	repoRoot, err := findGitRoot(repoHint)
	if err != nil {
		return worktreeContext{}, nil, fmt.Errorf("locate git repo: %w", err)
	}

	// Always resolve the worktree path to an absolute one. Tool nodes set
	// cmd.Dir to the worktree path AND substitute it into shell commands
	// like `git -C <path> ...`; if the path is relative, those two layers
	// stack the resolution (Go exec.Command resolves Dir against the parent
	// cwd, then sh resolves the substituted relative path against Dir),
	// landing in a phantom <wt>/<wt> location that doesn't exist.
	wtPath, err := filepath.Abs(filepath.Join(storeRoot, "worktrees", runID))
	if err != nil {
		return worktreeContext{}, nil, fmt.Errorf("resolve worktree absolute path: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(wtPath), 0o755); err != nil {
		return worktreeContext{}, nil, fmt.Errorf("create worktrees directory: %w", err)
	}

	// The run anchors on the checkout the OPERATOR launched from, which is
	// not always the main one: repoRoot is deliberately resolved up to the
	// main repository (that is where `.git` lives and where worktrees are
	// registered), but a linked worktree has its own HEAD and its own branch.
	//
	// Reading HEAD from repoRoot instead makes `iterion run` in a linked
	// worktree silently execute against the MAIN branch's tree — a run that
	// reports success while describing a tree the operator never asked about.
	// Measured: launched from a worktree pinned to an older commit, the bot
	// read the plan of `main`, found every step already done, and finished
	// "successfully" in 90 seconds.
	anchorDir := gitlib.FindRepoRoot(repoHint)
	if anchorDir == "" {
		anchorDir = repoRoot
	}

	// Capture the anchor checkout's branch + tip BEFORE creating the new
	// worktree so we have a baseline to compare against in finalize.
	// `symbolic-ref --quiet HEAD` returns "" + non-zero on detached HEAD —
	// that's intentional: we treat detached as "no branch to FF".
	originalBranch := ""
	brCmd, brCancel := gitCmd("-C", anchorDir, "symbolic-ref", "--quiet", "--short", "HEAD")
	if out, brErr := brCmd.Output(); brErr == nil {
		originalBranch = strings.TrimSpace(string(out))
	}
	brCancel()
	originalTip := ""
	tipCmd, tipCancel := gitCmd("-C", anchorDir, "rev-parse", "HEAD")
	if out, tipErr := tipCmd.Output(); tipErr == nil {
		originalTip = strings.TrimSpace(string(out))
	}
	tipCancel()

	// The COMMIT is passed, never the word `HEAD`: `git worktree add` runs
	// from repoRoot, where `HEAD` means the main checkout's HEAD and not the
	// anchor's. Any working-tree state (staged, unstaged, untracked) is
	// intentionally NOT copied — that is the whole point of isolation.
	startPoint := originalTip
	if startPoint == "" {
		startPoint = "HEAD"
	}
	cmd, cancel := gitCmd("-C", repoRoot, "worktree", "add", wtPath, startPoint)
	out, addErr := cmd.CombinedOutput()
	cancel()
	if addErr != nil {
		return worktreeContext{}, nil, fmt.Errorf("git worktree add %s at %s: %w\noutput: %s", wtPath, startPoint, addErr, string(out))
	}

	if logger != nil {
		if samePath(anchorDir, repoRoot) {
			logger.Info("runtime: worktree created at %s (base: %s HEAD %s on %s)",
				wtPath, repoRoot, shortSHA(originalTip), branchOrDetached(originalBranch))
		} else {
			// Say which checkout the run describes when it is not the main
			// one — the operator's cwd is the answer, and it used to be
			// invisible precisely when it mattered.
			logger.Info("runtime: worktree created at %s (base: %s HEAD %s on %s — linked worktree of %s)",
				wtPath, anchorDir, shortSHA(originalTip), branchOrDetached(originalBranch), repoRoot)
		}
	}

	cleanup := func() {
		// `--force` overrides protections; we accept the risk because the
		// engine owns the worktree's lifecycle. Best-effort: errors are
		// logged but do not fail the run. When no logger is configured
		// the failure goes to stderr so it isn't completely silent —
		// otherwise the worktree directory leaks on disk with no trace.
		rmCmd, rmCancel := gitCmd("-C", repoRoot, "worktree", "remove", "--force", wtPath)
		out, rmErr := rmCmd.CombinedOutput()
		rmCancel()
		if rmErr != nil {
			if logger != nil {
				logger.Warn("runtime: git worktree remove %s failed: %v\noutput: %s", wtPath, rmErr, string(out))
			} else {
				fmt.Fprintf(os.Stderr, "runtime: git worktree remove %s failed: %v\noutput: %s\n", wtPath, rmErr, string(out))
			}
		}
	}
	return worktreeContext{
		repoRoot:       repoRoot,
		anchorDir:      anchorDir,
		wtPath:         wtPath,
		gitDir:         resolveWorktreeGitDir(repoRoot, wtPath),
		originalBranch: originalBranch,
		originalTip:    originalTip,
	}, cleanup, nil
}

// resolveWorktreeGitDir returns the absolute host path of the worktree's
// git-private directory by reading the worktree's `.git` pointer file
// (a one-line `gitdir: <abs-path>`). Falls back to the conventional
// `<repoRoot>/.git/worktrees/<basename>` layout when the pointer file
// is missing or unreadable so behaviour stays stable for callers that
// pre-compute the path before the worktree exists.
//
// Reading the pointer is necessary because the conventional fallback
// is wrong when worktrees are nested: the dispatcher pre-creates a
// worktree under `.iterion/dispatcher/workspaces/<issue>/`, then the
// engine runs `git worktree add` from there to create a second
// worktree at `<storeRoot>/worktrees/<runID>/`. The conventional
// fallback would point at the dispatcher worktree's local `.git`
// (a file, not a directory), and the sandbox mount wiring would
// silently skip — leaving in-container git unable to resolve the
// pointer. The pointer file always names the real repo's gitdir.
//
// Returns "" when either input is empty so the caller (sandbox mount
// wiring) silently skips the bind on non-worktree runs.
func resolveWorktreeGitDir(repoRoot, wtPath string) string {
	if repoRoot == "" || wtPath == "" {
		return ""
	}
	// Read the worktree's `.git` pointer file (one-line `gitdir: <abs>`).
	// This is the only reliable way to find the real gitdir when worktrees
	// are nested (dispatcher pre-creates a worktree, engine adds another).
	pointerPath := filepath.Join(wtPath, ".git")
	if pointer, err := os.ReadFile(pointerPath); err == nil {
		line := strings.TrimSpace(string(pointer))
		if rest, ok := strings.CutPrefix(line, "gitdir:"); ok {
			if abs, absErr := filepath.Abs(strings.TrimSpace(rest)); absErr == nil {
				return abs
			}
		}
	}
	return filepath.Join(repoRoot, ".git", "worktrees", filepath.Base(wtPath))
}

// checkWorktreeLinkage verifies that a linked-worktree workspace still
// resolves to a live gitdir: workDir's `.git` pointer file must name an
// existing directory. When it doesn't, every git command in the workspace
// fails ("fatal: not a git repository: <gitdir>") and nodes read the
// broken workspace as "no repo" — a wrong answer, not a visible failure —
// so callers must refuse to execute there.
//
// Only the definitively-severed shape returns an error. A missing
// workspace, a `.git` directory (main checkout), or an unreadable/
// non-pointer `.git` all return nil so their existing failure modes stay
// unchanged.
func checkWorktreeLinkage(workDir string) error {
	if workDir == "" {
		return nil
	}
	pointer, err := os.ReadFile(filepath.Join(workDir, ".git"))
	if err != nil {
		return nil
	}
	rest, ok := strings.CutPrefix(strings.TrimSpace(string(pointer)), "gitdir:")
	if !ok {
		return nil
	}
	gitDir := strings.TrimSpace(rest)
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(workDir, gitDir)
	}
	if _, statErr := os.Stat(gitDir); statErr == nil {
		return nil
	}
	return fmt.Errorf("workspace %s is a git worktree whose gitdir %s no longer exists — the repository that registered it is gone (on a cloud runner the per-run clone is recycled between queue deliveries), so git cannot work there; re-launch the run", workDir, gitDir)
}

// finalizeOptions controls the post-run worktree promotion. All fields
// are optional; sensible defaults apply when empty.
type finalizeOptions struct {
	// runName is the deterministic friendly label (e.g. "neon-glitch-foxhowl-a3f2")
	// used when branchName is empty. Falls back to runID if also empty.
	runName string
	runID   string
	// branchName, when non-empty, overrides the default
	// `iterion/run/<runName>` storage branch. Useful for landing each
	// run on a stable name (e.g. `feat/auto-fixes`).
	branchName string
	// mergeInto controls the best-effort merge target:
	//   ""        → merge into the originalBranch (default)
	//   "none"    → skip the merge entirely
	//   "current" → same as default; explicit form
	//   <branch>  → merge this named branch instead of originalBranch
	mergeInto string
	// mergeStrategy selects how the run's commits are applied to the
	// merge target. "squash" collapses them into one commit; "merge"
	// fast-forwards (preserves history). Empty defaults to "squash".
	mergeStrategy string
	// autoMerge gates whether the merge runs synchronously at the end
	// of the run. When false, finalize stops after creating the storage
	// branch and reports MergeStatus="pending" so the UI can drive a
	// deferred merge.
	autoMerge bool
}

// finalizeResult captures what the post-run promotion actually did so
// the engine can persist it to run.json and the studio can surface it.
type finalizeResult struct {
	// FinalCommit is the SHA the worktree's HEAD pointed to at end of
	// run. Empty when the run produced no commits (HEAD unchanged).
	FinalCommit string
	// FinalBranch is the persistent branch created on FinalCommit.
	// Empty when no commits were produced (no branch needed).
	FinalBranch string
	// FinalBranchError is set when FinalCommit was produced but no
	// persistent branch could be created on it. The studio renders
	// this so the operator can recover the commits manually before
	// reflog GC.
	FinalBranchError string
	// MergedInto is the branch the engine merged into. Empty when the
	// merge was skipped (autoMerge=false), opted out, or failed.
	MergedInto string
	// MergedCommit is the SHA on the target branch after the merge.
	// Equals FinalCommit for the "merge" (FF) strategy; differs for
	// "squash" (a fresh commit). Empty when no merge happened.
	MergedCommit string
	// MergeStatus mirrors store.MergeStatus values:
	//   "pending"  — branch created, merge deferred to UI
	//   "merged"   — merge succeeded
	//   "skipped"  — explicit opt-out (mergeInto=none) or no commits
	//   "failed"   — merge attempted but failed (logged, run still ok)
	MergeStatus string
	// WipBanked is true when finalize found UNCOMMITTED changes in the
	// worktree and auto-committed them (a "wip" bank commit) so the
	// storage branch preserves them. A wip-banked HEAD is never merged
	// into the operator's branch — MergeStatus is forced to "skipped".
	WipBanked bool
	// PreserveWorktree is true when finalize could NOT secure the
	// worktree's uncommitted changes (the wip bank commit failed).
	// The caller must skip the worktree cleanup so the operator can
	// recover the files by hand — removing it would silently destroy
	// finished work.
	PreserveWorktree bool
}

// finalizeWorktree promotes the worktree's HEAD onto a persistent
// branch and best-effort fast-forwards the requested merge target.
// Always best-effort: any failure is logged but does not fail the run.
func finalizeWorktree(wc worktreeContext, opts finalizeOptions, logger *iterlog.Logger) finalizeResult {
	res := finalizeResult{}

	// 0. Safety invariant: the worktree MUST be a dedicated tree, never
	// the operator's live checkout. If it collapsed to the repo root — a
	// phantom-worktree run whose WorkDir resolved to repoRoot (seen via
	// RecoverFinalize on a run persisted with WorkDir==RepoRoot) — then
	// the wip-bank `git commit` below runs IN the main checkout and lands
	// on the operator's CURRENT branch, silently committing unrelated
	// uncommitted work. The "never merge a wip-banked HEAD" guard in
	// step 5 can't save this: the commit is already on their branch.
	// Refuse outright — no commit, preserve as-is, warn.
	if samePath(wc.wtPath, wc.repoRoot) {
		if logger != nil {
			logger.Warn("runtime: finalize: worktree path %s is the repo root — refusing to bank/promote (would commit on the operator's branch); preserving, recover any run output by hand", wc.wtPath)
		}
		res.PreserveWorktree = true
		return res
	}

	// 1. Read the worktree's current HEAD.
	finalSHA := readHEAD(wc.wtPath)
	if finalSHA == "" {
		// Couldn't read HEAD — log and bail. The cleanup runs anyway.
		if logger != nil {
			logger.Warn("runtime: finalize: cannot read worktree HEAD at %s — skipping promotion", wc.wtPath)
		}
		return res
	}

	// 1b. Bank uncommitted work before it can be destroyed. A bot that
	// exits through a non-commit edge (a gated commit node not reached,
	// a "hold for later" outcome, an agent that edited but never
	// committed) leaves finished work as a dirty working tree; the
	// cleanup that follows finalize is `git worktree remove --force`,
	// which would silently destroy it — the exact stranded-work failure
	// mode observed across the pre-ADR-055 bot-session corpus. Commit it
	// as an explicit wip bank so the storage branch preserves it; the
	// operator reviews it there (it is NEVER merged into their branch —
	// see step 5).
	if clean, cleanErr := workdirIsClean(wc.wtPath); cleanErr != nil {
		if logger != nil {
			logger.Warn("runtime: finalize: cannot probe worktree cleanliness: %v — proceeding without wip bank", cleanErr)
		}
	} else if !clean {
		msg := "wip(iterion): auto-banked uncommitted run output"
		if opts.runName != "" {
			msg += " (" + opts.runName + ")"
		}
		if err := runGitInDir(wc.wtPath, "add", "-A"); err != nil {
			if logger != nil {
				logger.Warn("runtime: finalize: wip bank `git add -A` failed: %v — preserving worktree at %s", err, wc.wtPath)
			}
			res.PreserveWorktree = true
		} else if err := runGitInDir(wc.wtPath, "commit", "-m", msg); err != nil {
			if logger != nil {
				logger.Warn("runtime: finalize: wip bank commit failed: %v — preserving worktree at %s", err, wc.wtPath)
			}
			res.PreserveWorktree = true
		} else {
			res.WipBanked = true
			if banked := readHEAD(wc.wtPath); banked != "" {
				finalSHA = banked
			}
			if logger != nil {
				logger.Warn("runtime: finalize: worktree had UNCOMMITTED changes — banked as wip commit %s (review it on the storage branch; it will not be merged)", shortSHA(finalSHA))
			}
		}
	}

	// 2. No commits produced → nothing to promote.
	if finalSHA == wc.originalTip {
		if logger != nil {
			logger.Info("runtime: finalize: no commits produced (HEAD %s unchanged)", shortSHA(finalSHA))
		}
		return res
	}
	res.FinalCommit = finalSHA

	// 3. Decide the storage branch name.
	branchName := opts.branchName
	if branchName == "" {
		label := opts.runName
		if label == "" {
			label = opts.runID
		}
		branchName = "iterion/run/" + label
	}
	// Defense-in-depth: user-supplied branchName is already rejected at
	// Launch / CLI entry, but a malformed default (e.g. runID containing
	// unexpected chars in a future refactor) would otherwise reach `git
	// branch` as a positional that could be parsed as a flag. Validate
	// here too and skip branch creation rather than risk an injection.
	if err := gitlib.ValidateBranchName(branchName); err != nil {
		if logger != nil {
			logger.Warn("runtime: finalize: refusing to create branch %q: %v — recover with: git branch <name> %s", branchName, err, finalSHA)
		}
		res.FinalBranchError = fmt.Sprintf("invalid branch name %q: %v (recover with: git branch <name> %s)", branchName, err, finalSHA)
		return res
	}

	// 4. Create the storage branch. If the name already exists, fall
	// back to a suffixed variant so we never overwrite a user-managed
	// branch. The branch is the GC guard — it must always succeed in
	// some form, otherwise the commits are lost on cleanup.
	created, finalName := createBranchSafely(wc.repoRoot, branchName, finalSHA, logger)
	if !created {
		// Even the suffixed fallback failed — surface the SHA so the
		// user can recover via reflog before GC.
		if logger != nil {
			logger.Warn("runtime: finalize: could not create branch for %s — recover with: git branch <name> %s",
				shortSHA(finalSHA), finalSHA)
		}
		res.FinalBranchError = fmt.Sprintf("git branch failed for %q (and suffixed variants) — recover with: git branch <name> %s", branchName, finalSHA)
		return res
	}
	res.FinalBranch = finalName
	if logger != nil {
		logger.Info("runtime: finalize: created branch %s → %s", finalName, shortSHA(finalSHA))
	}

	// 5. Decide the merge target branch. A wip-banked HEAD is never a
	// merge candidate: the bank commit is unreviewed, un-verified output
	// the run itself chose not to commit — fast-forwarding or squashing
	// it into the operator's branch would land it silently. Storage
	// branch only.
	if res.WipBanked {
		res.MergeStatus = "skipped"
		if logger != nil {
			logger.Info("runtime: finalize: merge skipped (wip-banked HEAD); review branch %s", finalName)
		}
		return res
	}
	target := resolveMergeTarget(opts.mergeInto, wc.originalBranch)
	if target == "" {
		// Explicit opt-out, or no candidate (detached HEAD at start with
		// no override). Branch alone is the result.
		res.MergeStatus = "skipped"
		if logger != nil {
			logger.Info("runtime: finalize: skipping merge (target empty); inspect %s and `git merge` when ready",
				finalName)
		}
		return res
	}

	// 6. autoMerge gate: when false, leave the merge for a UI-driven
	// action. Storage branch alone is the result with merge_status=pending.
	if !opts.autoMerge {
		res.MergeStatus = "pending"
		if logger != nil {
			logger.Info("runtime: finalize: auto_merge disabled; merge of %s into %s pending UI confirmation",
				finalName, target)
		}
		return res
	}

	// 7. Dispatch by strategy. Default empty → squash.
	strategy := strings.ToLower(strings.TrimSpace(opts.mergeStrategy))
	if strategy == "" {
		strategy = "squash"
	}

	switch strategy {
	case "merge":
		if mergeErr := tryFastForward(wc.anchor(), target, finalName, finalSHA, wc.originalBranch, logger); mergeErr != nil {
			res.MergeStatus = "failed"
			if logger != nil {
				logger.Warn("runtime: finalize: fast-forward of %s skipped: %v — `git merge %s` to bring it in",
					target, mergeErr, finalName)
			}
			return res
		}
		res.MergedInto = target
		res.MergedCommit = finalSHA
		res.MergeStatus = "merged"
		if logger != nil {
			logger.Info("runtime: finalize: fast-forwarded %s → %s", target, shortSHA(finalSHA))
		}
		return res

	case "squash":
		message := buildSquashMessage(wc.anchor(), wc.originalTip, finalSHA, opts.runName)
		merged, mergeErr := trySquashMerge(wc.anchor(), target, finalName, wc.originalBranch, message, logger)
		if mergeErr != nil {
			res.MergeStatus = "failed"
			if logger != nil {
				logger.Warn("runtime: finalize: squash of %s into %s failed: %v — branch %s preserved",
					finalName, target, mergeErr, finalName)
			}
			return res
		}
		res.MergedInto = target
		res.MergedCommit = merged
		res.MergeStatus = "merged"
		if logger != nil {
			logger.Info("runtime: finalize: squashed %s into %s as %s", finalName, target, shortSHA(merged))
		}
		return res

	default:
		res.MergeStatus = "failed"
		if logger != nil {
			logger.Warn("runtime: finalize: unknown merge_strategy %q; storage branch %s preserved",
				strategy, finalName)
		}
		return res
	}
}

// resolveMergeTarget converts the launch-param merge_into value into a
// concrete branch name (or "" to skip).
func resolveMergeTarget(mergeInto, originalBranch string) string {
	switch strings.ToLower(strings.TrimSpace(mergeInto)) {
	case "none":
		return ""
	case "", "current":
		return originalBranch
	default:
		return mergeInto
	}
}

// createBranchSafely creates a branch at sha; on collision, retries with
// a suffix (-1, -2, …) up to 16 times before giving up. Returns the
// final branch name actually created, or "" on total failure.
func createBranchSafely(repoRoot, name, sha string, logger *iterlog.Logger) (bool, string) {
	candidates := []string{name}
	for i := 1; i <= 16; i++ {
		candidates = append(candidates, fmt.Sprintf("%s-%d", name, i))
	}
	for _, candidate := range candidates {
		// `--` separates options from positional arguments so a
		// candidate that begins with `-` (or any future ValidateBranchName
		// regression) can never be parsed as a flag by git.
		brCmd, brCancel := gitCmd("-C", repoRoot, "branch", "--", candidate, sha)
		out, err := brCmd.CombinedOutput()
		brCancel()
		if err == nil {
			return true, candidate
		}
		// Branch-already-exists is the only error we silently retry on;
		// other errors (bad SHA, permissions) are terminal.
		if !strings.Contains(string(out), "already exists") {
			if logger != nil {
				logger.Warn("runtime: finalize: git branch %s failed: %v\noutput: %s", candidate, err, string(out))
			}
			return false, ""
		}
	}
	return false, ""
}

// tryFastForward enforces the safety guards and runs `git merge --ff-only`.
// Returns nil on success; a descriptive error explaining why the FF was
// skipped otherwise (callers log the reason).
func tryFastForward(repoRoot, target, branchToMerge, finalSHA, originalBranch string, logger *iterlog.Logger) error {
	if err := guardMergeTarget(repoRoot, target, originalBranch, "FF"); err != nil {
		return err
	}

	// FF must actually be possible (target is ancestor of finalSHA).
	ancCmd, ancCancel := gitCmd("-C", repoRoot, "merge-base", "--is-ancestor", "refs/heads/"+target, finalSHA)
	ancErr := ancCmd.Run()
	ancCancel()
	if ancErr != nil {
		return fmt.Errorf("non-fast-forward (%q has commits not in run output)", target)
	}

	ffCmd, ffCancel := gitCmd("-C", repoRoot, "merge", "--ff-only", branchToMerge)
	out, err := ffCmd.CombinedOutput()
	ffCancel()
	if err != nil {
		return fmt.Errorf("git merge --ff-only failed: %v\noutput: %s", err, string(out))
	}
	return nil
}

// guardMergeTarget enforces the prerequisites shared by every strategy
// that touches the user's working tree: the originalBranch invariant
// must hold, the target must equal the currently-checked-out branch,
// and the working tree must be clean. opName is interpolated into error
// messages so callers can distinguish FF / squash failures in logs.
func guardMergeTarget(repoRoot, target, originalBranch, opName string) error {
	currentBranch := ""
	brCmd, brCancel := gitCmd("-C", repoRoot, "symbolic-ref", "--quiet", "--short", "HEAD")
	if out, err := brCmd.Output(); err == nil {
		currentBranch = strings.TrimSpace(string(out))
	}
	brCancel()
	if originalBranch != "" && currentBranch != originalBranch {
		return fmt.Errorf("checked-out branch changed from %q to %q since start", originalBranch, currentBranch)
	}
	if target != currentBranch {
		return fmt.Errorf("%s of %q skipped: only the currently-checked-out branch (%q) is supported", opName, target, currentBranch)
	}
	// A failure of `git status --porcelain` itself must not be
	// interpreted as "tree is clean" — that lets a transient git error
	// (file lock, repo corruption, missing index) bypass the safety
	// check and merge over potentially-dirty state. Treat any error as
	// dirty / unknown.
	stCmd, stCancel := gitCmd("-C", repoRoot, "status", "--porcelain")
	out, err := stCmd.Output()
	stCancel()
	if err != nil {
		return fmt.Errorf("git status check failed before %s: %w", opName, err)
	}
	if len(strings.TrimSpace(string(out))) > 0 {
		return fmt.Errorf("main working tree has uncommitted changes")
	}
	return nil
}

// trySquashMerge applies the storage branch's commits onto target as a
// single squash commit. Returns the new commit SHA on the target branch
// and nil on success, or "" + a descriptive error explaining why the
// merge was skipped (callers log the reason).
//
// Guards mirror tryFastForward — see guardMergeTarget. Squash is
// allowed even when target is not an ancestor of the source branch
// (that's the whole point), so the FF-ancestry check is omitted.
//
// Conflict handling: when `git merge --squash` fails because of
// content conflicts (`UU` paths in the index), the worktree is left
// in the conflicted state and a *MergeConflictError is returned so
// callers can drive the in-studio resolver UI. Other failures clean
// up via `git reset --merge` as before.
func trySquashMerge(repoRoot, target, branchToMerge, originalBranch, message string, logger *iterlog.Logger) (string, error) {
	if err := guardMergeTarget(repoRoot, target, originalBranch, "squash"); err != nil {
		return "", err
	}

	// Capture the pre-merge HEAD so the "nothing to commit" soft-success
	// path returns the genuine base SHA. `git merge --squash` does not
	// move HEAD, so a post-failure readHEAD happens to give the same
	// answer today — but if any future cleanup ever does move HEAD on
	// the failure path, the return value would silently lie. Pin it now.
	baseHead := readHEAD(repoRoot)

	// Step 1: stage the squashed diff via `git merge --squash`. This
	// updates the index + working tree to match branchToMerge but does
	// NOT create a commit on its own; we follow up with `git commit`.
	sqCmd, sqCancel := gitCmd("-C", repoRoot, "merge", "--squash", branchToMerge)
	out, sqErr := sqCmd.CombinedOutput()
	sqCancel()
	if sqErr != nil {
		// Distinguish "merge conflict" (UU paths in index) from any
		// other failure mode. Conflicts must NOT be rolled back —
		// the operator (or the conflict-resolver UI) needs the
		// markers in the worktree to drive resolution. Genuine
		// errors (corrupt index, bad ref, etc) still get the reset.
		if conflicts, lsErr := unmergedPaths(repoRoot); lsErr == nil && len(conflicts) > 0 {
			return "", &MergeConflictError{Files: conflicts, Output: string(out)}
		}
		rsCmd, rsCancel := gitCmd("-C", repoRoot, "reset", "--merge")
		_ = rsCmd.Run()
		rsCancel()
		return "", fmt.Errorf("git merge --squash failed: %v\noutput: %s", sqErr, string(out))
	}

	// Step 2: commit the squashed index. --no-edit prevents the studio
	// from being invoked when MERGE_MSG was populated by --squash; -m
	// supplies our aggregated message regardless. `-m` consumes the
	// very next argv element as the value, so a leading "-" in the
	// message is fine here — exec.Command bypasses the shell.
	coCmd, coCancel := gitCmd("-C", repoRoot, "commit", "-m", message)
	out, coErr := coCmd.CombinedOutput()
	coCancel()
	if coErr != nil {
		// `git commit` exits non-zero with "nothing to commit" if the
		// squash diff was empty (e.g. branch already merged). Treat
		// that as a soft success: nothing changed, target stays put.
		// Anything else is a real failure — reset the index.
		if strings.Contains(string(out), "nothing to commit") || strings.Contains(string(out), "no changes added to commit") {
			return baseHead, nil
		}
		rsCmd, rsCancel := gitCmd("-C", repoRoot, "reset", "--merge")
		_ = rsCmd.Run()
		rsCancel()
		return "", fmt.Errorf("git commit (squash) failed: %v\noutput: %s", coErr, string(out))
	}

	// Read the new HEAD SHA — that's the squash commit on target.
	newHead := readHEAD(repoRoot)
	if newHead == "" {
		return "", fmt.Errorf("squash succeeded but cannot read new HEAD")
	}
	return newHead, nil
}

// BuildSquashMessage is the public form of buildSquashMessage used by
// the HTTP merge handler when the client did not supply its own message.
// Identical semantics — see buildSquashMessage docs.
func BuildSquashMessage(repoRoot, base, head, runName string) string {
	return buildSquashMessage(repoRoot, base, head, runName)
}

// BuildSquashMessageFromCommits is the cached-input form for callers
// that already fetched the commit slice (e.g. the /api/runs/{id}/commits
// handler renders the same data and would otherwise re-shell `git log`
// every time the merge-message preview refreshes).
//
// Single-commit runs still need one `git log -1 --pretty=format:%B`
// to recover the body — gitlib.CommitInfo carries Subject only.
func BuildSquashMessageFromCommits(repoRoot, head, runName string, commits []gitlib.CommitInfo) string {
	if len(commits) == 1 {
		if full := readFullCommitMessage(repoRoot, head); full != "" {
			return full
		}
	}
	return assembleSquashMessage(commits, runName)
}

// RunDisplayName returns the human-friendly label for a run: its
// deterministic friendly name when set (e.g. "neon-glitch-foxhowl-a3f2"),
// else the workflow name. Callers that build squash titles or
// log lines share this fallback chain.
func RunDisplayName(run *store.Run) string {
	if run == nil {
		return ""
	}
	if run.Name != "" {
		return run.Name
	}
	return run.WorkflowName
}

// buildSquashMessage assembles the commit message for a squash merge.
// Wrapper for callers that don't already have the commit slice — the
// /commits handler does, so it routes through BuildSquashMessageFromCommits
// directly to skip the redundant `git log`.
//
// Single-commit runs reuse that commit's full message (subject + body)
// verbatim, so the squash on `main` carries the same conventional-
// commit description the workflow's `commit_changes` node authored —
// no information loss vs. a non-squash merge.
//
// Multi-commit runs use the first commit's subject as the title, then
// a `- <shortSHA> <subject>` body list to preserve the per-iteration
// audit trail in collapsed form.
//
// Falls back to runName then "iterion run" when no commits are
// readable in base..head (degenerate ranges, bad refs).
func buildSquashMessage(repoRoot, base, head, runName string) string {
	commits, _ := gitlib.Log(repoRoot, base, head)
	return BuildSquashMessageFromCommits(repoRoot, head, runName, commits)
}

// assembleSquashMessage formats the multi-commit case: title is the
// first subject, body is `- <shortSHA> <subject>` per commit. Single-
// commit-with-full-body is handled upstream by
// BuildSquashMessageFromCommits → readFullCommitMessage; this is the
// subject-only path used for multi-commit runs and the
// no-body-recoverable degraded single-commit case.
func assembleSquashMessage(commits []gitlib.CommitInfo, runName string) string {
	title := ""
	if len(commits) > 0 {
		title = commits[0].Subject
	}
	if title == "" {
		title = strings.TrimSpace(runName)
	}
	if title == "" {
		title = "iterion run"
	}

	var body strings.Builder
	body.WriteString(title)
	body.WriteString("\n")

	if len(commits) <= 1 {
		return body.String()
	}

	body.WriteString("\n")
	for _, c := range commits {
		body.WriteString("- ")
		body.WriteString(c.Short)
		body.WriteString(" ")
		body.WriteString(c.Subject)
		body.WriteString("\n")
	}
	return body.String()
}

// readFullCommitMessage returns the full commit message (subject + body)
// for ref, normalised to a single trailing newline so the output is
// safe to feed into `git commit -m`. Returns "" on any git failure;
// callers degrade to the subject-only fallback chain.
func readFullCommitMessage(repoRoot, ref string) string {
	cmd, cancel := gitCmd("-C", repoRoot, "log", "-1", "--pretty=format:%B", ref)
	out, err := cmd.Output()
	cancel()
	if err != nil {
		return ""
	}
	msg := strings.TrimRight(string(out), "\n")
	if msg == "" {
		return ""
	}
	return msg + "\n"
}

// DeferredMergeRequest is the input for a UI-driven merge action: the
// run is already finalized (worktree gone, storage branch created),
// and the user picked the strategy + target after seeing the commits.
//
// Differs from finalizeOptions in that there is no run-time context
// (no originalBranch invariant to check) — only the live state of the
// repo at the moment of the click is relevant.
type DeferredMergeRequest struct {
	// RepoRoot is the absolute path of the main repo to merge into.
	// The storage branch (BranchToMerge) must live inside it.
	RepoRoot string
	// Target is "current" / "" → currently-checked-out branch, or an
	// explicit branch name (which must equal the currently-checked-out
	// branch — see tryFastForward's guard rationale).
	Target string
	// BranchToMerge is the storage branch produced at finalization
	// (e.g. "iterion/run/<friendly>"). Must point at a commit reachable
	// from the run's FinalCommit.
	BranchToMerge string
	// FinalSHA is the SHA at the tip of BranchToMerge — passed in so
	// the FF guard can verify ancestry without re-resolving it.
	FinalSHA string
	// Strategy is "squash" (default) or "merge". Empty → "squash".
	Strategy string
	// Message is the squash commit message. Ignored for "merge"
	// strategy. Empty → caller-provided fallback applied below.
	Message string
}

// DeferredMergeResult reports what happened. MergedCommit is the SHA on
// the target branch after the merge (a fresh squash SHA or, for FF,
// equal to FinalSHA).
type DeferredMergeResult struct {
	MergedCommit string
	MergedInto   string
	Strategy     string
}

// PerformDeferredMerge executes a UI-driven merge against a finalized
// run's storage branch. Returns a populated result on success, or a
// descriptive error explaining which guard rejected the merge — the
// HTTP handler maps that error to a 4xx/5xx status without rescuing
// partial state on the repo (the storage branch is preserved either
// way).
func PerformDeferredMerge(req DeferredMergeRequest, logger *iterlog.Logger) (DeferredMergeResult, error) {
	if req.RepoRoot == "" {
		return DeferredMergeResult{}, fmt.Errorf("repo root required")
	}
	if req.BranchToMerge == "" {
		return DeferredMergeResult{}, fmt.Errorf("branch to merge required")
	}
	if req.FinalSHA == "" {
		return DeferredMergeResult{}, fmt.Errorf("final SHA required")
	}

	currentBranch := ""
	brCmd, brCancel := gitCmd("-C", req.RepoRoot, "symbolic-ref", "--quiet", "--short", "HEAD")
	if out, err := brCmd.Output(); err == nil {
		currentBranch = strings.TrimSpace(string(out))
	}
	brCancel()
	target := resolveMergeTarget(req.Target, currentBranch)
	if target == "" {
		return DeferredMergeResult{}, fmt.Errorf("merge target empty (detached HEAD?)")
	}

	strategy := strings.ToLower(strings.TrimSpace(req.Strategy))
	if strategy == "" {
		strategy = "squash"
	}

	switch strategy {
	case "merge":
		// Pass currentBranch as originalBranch so the FF still requires
		// the user to be on the merge target — same guard as in-engine.
		if err := tryFastForward(req.RepoRoot, target, req.BranchToMerge, req.FinalSHA, currentBranch, logger); err != nil {
			return DeferredMergeResult{}, err
		}
		return DeferredMergeResult{MergedCommit: req.FinalSHA, MergedInto: target, Strategy: strategy}, nil

	case "squash":
		message := req.Message
		if message == "" {
			message = "iterion run squash"
		}
		merged, err := trySquashMerge(req.RepoRoot, target, req.BranchToMerge, currentBranch, message, logger)
		if err != nil {
			return DeferredMergeResult{}, err
		}
		return DeferredMergeResult{MergedCommit: merged, MergedInto: target, Strategy: strategy}, nil

	default:
		return DeferredMergeResult{}, fmt.Errorf("unknown merge strategy %q", strategy)
	}
}

// readHEAD returns the SHA of HEAD in the given worktree, or "" on error.
func readHEAD(wtPath string) string {
	cmd, cancel := gitCmd("-C", wtPath, "rev-parse", "HEAD")
	out, err := cmd.Output()
	cancel()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func shortSHA(sha string) string {
	if len(sha) >= 7 {
		return sha[:7]
	}
	return sha
}

func branchOrDetached(branch string) string {
	if branch == "" {
		return "detached HEAD"
	}
	return branch
}

// RecoverFinalize re-runs the worktree promotion step for a run that
// reached a terminal status but never persisted its finalization metadata
// (FinalCommit / FinalBranch / MergeStatus). This happens when the daemon
// is killed between "Run finished" being written and finalizeWorktree
// completing — observed on 2026-05-14 when a dpkg post-install trigger
// SIGTERM'd the daemon ~50ms after run_1778749561103 hit `done`, leaving
// the worktree with 11 commits but the studio showing "no commits to
// merge" because run.json had no final_branch.
//
// The recovery is idempotent: it bails immediately if the run already
// has FinalBranch set or if it's not a worktree run. Reads the worktree
// HEAD via git rev-parse, rebuilds a minimal worktreeContext from the
// run's persisted RepoRoot/BaseCommit/WorkDir, and calls finalizeWorktree
// with autoMerge=false so the user retains UI control over the deferred
// merge (the same default as a non-recovery finalize).
//
// Designed to be invoked from the daemon's reconcileOrphans pass at
// startup so the gap between "daemon up" and "metadata recovered"
// stays sub-second.
func RecoverFinalize(ctx context.Context, st store.RunStore, r *store.Run, logger *iterlog.Logger) error {
	if r == nil {
		return fmt.Errorf("nil run")
	}
	if !r.Worktree || r.WorkDir == "" || r.RepoRoot == "" {
		return nil // not a worktree run — nothing to recover
	}
	// Phantom-worktree guard: a run whose WorkDir collapsed to the repo
	// root has no dedicated worktree to finalize — its WorkDir IS the
	// operator's live checkout. Recovering it would bank uncommitted
	// changes (possibly unrelated operator WIP) onto their current
	// branch. Skip. finalizeWorktree enforces the same invariant as a
	// backstop, but returning here avoids the misleading recovery logs.
	if samePath(r.WorkDir, r.RepoRoot) {
		if logger != nil {
			logger.Warn("runtime: RecoverFinalize: run %s WorkDir %s == repo root — not a dedicated worktree; skipping recovery so uncommitted work is not banked onto the operator's branch", r.ID, r.WorkDir)
		}
		return nil
	}
	if r.FinalBranch != "" || r.FinalCommit != "" {
		return nil // already finalized
	}
	// A worktree already removed (operator cleanup, manual
	// `git worktree remove`, store GC) leaves nothing to promote, and
	// that state is permanent — the reconcile loop scans every terminal
	// run on every tick, so reaching finalizeWorktree here re-logs the
	// same "cannot read worktree HEAD" warning every minute per deleted
	// worktree. Skip quietly; an EXISTING worktree with an unreadable
	// HEAD still warns from finalizeWorktree below.
	//
	// Gated on ENOENT only: a stat failure on an existing path (EACCES
	// on a parent, EIO/ESTALE on an unavailable mount) is recoverable —
	// the run's commits may still be promotable once the mount or the
	// permissions are fixed — so it falls through to finalizeWorktree's
	// warning instead of vanishing silently.
	if _, err := os.Stat(r.WorkDir); os.IsNotExist(err) {
		return nil
	}
	// Finalize any terminally-stopped run that left work in the
	// worktree. The happy path is `finished` (the original case that
	// motivated RecoverFinalize). `cancelled` also benefits: when the
	// operator stops a run with commits in flight, they commonly want
	// to merge whatever partial work the run produced — and the merge
	// UI requires FinalCommit+FinalBranch, so without recovery the
	// "Squash and merge" button fails with "no storage branch".
	//
	// `failed_resumable` is deliberately NOT recovered: those runs are
	// designed to be resumed (the engine left a checkpoint specifically
	// for that). Pre-creating a storage branch here would land at the
	// partial HEAD; when the operator resumes and finalize runs
	// normally on completion, createBranchSafely sees the existing
	// branch and falls back to a suffixed name, leaving the user with
	// two branches for one logical run. The operator's path on a
	// failed_resumable is either resume-to-finish (normal finalize
	// fires) OR cancel-then-merge (this RecoverFinalize fires on the
	// `cancelled` status).
	//
	// `failed` is now also recovered (F-RT-5): a hard failure may
	// still leave partial commits the operator wants to inspect, and
	// without a persistent branch they only survive in reflog. The
	// `finalSHA == originalTip` guard in finalizeWorktree means a
	// no-op when the failed run produced nothing. The operator can
	// still salvage hand-built work via the run's WorkDir + a manual
	// `git branch`, but the recovery path now matches the symmetry
	// expected by RunHeader's "view final branch" link.
	switch r.Status {
	case store.RunStatusFinished, store.RunStatusCancelled, store.RunStatusFailed:
		// recover
	default:
		return nil
	}
	wc := worktreeContext{
		repoRoot:    r.RepoRoot,
		wtPath:      r.WorkDir,
		gitDir:      resolveWorktreeGitDir(r.RepoRoot, r.WorkDir),
		originalTip: r.BaseCommit,
		// originalBranch left empty — recovery has no source for the
		// branch name the user was on at launch. finalizeWorktree's
		// merge target resolution treats empty originalBranch as a
		// reason to skip the merge (MergeStatus="skipped"), which is
		// the right default here: the run's main result is the
		// persistent branch; the user merges via the UI.
	}
	res := finalizeWorktree(wc, finalizeOptions{
		runName:   r.Name,
		runID:     r.ID,
		mergeInto: "none", // skip auto-merge on recovery path; user drives via UI
	}, logger)
	if res.FinalCommit == "" && res.FinalBranch == "" {
		return nil // worktree gone, HEAD == base, or branch creation failed
	}
	r.FinalCommit = res.FinalCommit
	r.FinalBranch = res.FinalBranch
	r.MergeStatus = store.MergeStatus(res.MergeStatus)
	if res.MergedInto != "" {
		r.MergedInto = res.MergedInto
	}
	if res.MergedCommit != "" {
		r.MergedCommit = res.MergedCommit
	}
	if logger != nil {
		logger.Info("runtime: recovered finalize for run %s → branch %s (commit %s, status %s)",
			r.ID, res.FinalBranch, shortSHA(res.FinalCommit), res.MergeStatus)
	}
	return st.SaveRun(ctx, r)
}

// workspaceIsGitRepo reports whether `dir` (or any parent up to /) is
// inside a git repository. Used by the engine's worktree precheck so a
// workflow that defaults to `worktree: auto` against a non-git workspace
// degrades gracefully to in-place execution instead of hard-failing with
// "not a git repository". Falls back to os.Getwd() when `dir` is empty
// so the engine's pre-Run normalization matches the cwd-based behaviour.
//
// This wraps gitlib.FindMainRepoRoot purely for self-documenting intent
// at the engine call site — the underlying helper already returns "" for
// non-git paths.
func workspaceIsGitRepo(dir string) bool {
	if dir == "" {
		var err error
		dir, err = os.Getwd()
		if err != nil {
			return false
		}
	}
	return gitlib.FindMainRepoRoot(dir) != ""
}

// workspaceHasCommits reports whether the repo containing dir has a
// resolvable HEAD. A freshly created (empty) repository — exactly what
// the create-repo launch journey clones — has an unborn HEAD, and
// `git worktree add … HEAD` cannot anchor on it.
func workspaceHasCommits(dir string) bool {
	cmd, cancel := gitCmd("-C", dir, "rev-parse", "--verify", "-q", "HEAD")
	defer cancel()
	return cmd.Run() == nil
}

// findGitRoot walks up parent directories from `dir` until it finds a `.git`
// entry, then resolves linked-worktree pointer files back to the main repo
// so a per-run worktree set up on top of an outer worktree (e.g. the
// dispatcher's pre-seeded workspace at
// `<repo>/.iterion/dispatcher/workspaces/<id>`) records the OPERATOR's
// main checkout as the run's repo root — not the intermediate worktree.
//
// Falls back to os.Getwd() when `dir` is empty. Returns an error only
// when no `.git` entry is found anywhere in the parent chain.
func findGitRoot(dir string) (string, error) {
	if dir == "" {
		var err error
		dir, err = os.Getwd()
		if err != nil {
			return "", fmt.Errorf("getwd: %w", err)
		}
	}
	main := gitlib.FindMainRepoRoot(dir)
	if main == "" {
		return "", fmt.Errorf("not a git repository (or any parent up to /): %s", dir)
	}
	return main, nil
}
