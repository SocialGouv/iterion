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
// and best-effort fast-forwards the user's checked-out branch, then atomically
// retires the worktree. A clean, process-quiescent recovery copy is removed
// through non-forced Git cleanup; late activity remains quarantined with a
// manifest. Without promotion, commits are reachable only via reflog and are
// eligible for GC.
package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	gitlib "github.com/SocialGouv/iterion/pkg/git"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// gitCmdTimeout bounds every git invocation made through gitCmd. Without
// it, a wedged git process (stale index.lock, a hung smudge/clean filter)
// blocks the engine goroutine running worktree setup/finalize forever —
// the run would never surface a timeout error, just hang.
const (
	gitCmdTimeout            = 60 * time.Second
	worktreeQuiescenceWindow = 25 * time.Millisecond
)

var (
	cleanupGuardNonce    atomic.Uint64
	cleanupRecoveryNonce atomic.Uint64
	// cleanupProcessReferences is a package seam so deterministic cleanup
	// tests can isolate Git/permission behaviour from unrelated host
	// processes. Production never reassigns it.
	cleanupProcessReferences = worktreeProcessReferences
)

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
	cmd.Env = append(os.Environ(), "LC_ALL=C", "LANG=C")
	detachGitProcessGroup(cmd)
	return cmd, cancel
}

// worktreeContext is the state captured at setupWorktree time and
// consumed by finalizeWorktree to decide whether the run actually
// produced new commits and whether a fast-forward of the user's branch
// is safe.
type worktreeContext struct {
	repoRoot       string    // absolute path to the main repo (where .git lives)
	wtPath         string    // absolute path to the per-run worktree
	gitDir         string    // absolute host path of the worktree's git-private dir (<repoRoot>/.git/worktrees/<basename(wtPath)>); the worktree's `.git` pointer file points here
	originalBranch string    // current branch on the main worktree at run start ("" if detached)
	originalTip    string    // SHA of HEAD at run start (worktree initial state)
	authoritySince time.Time // trusted lower bound captured before worktree creation
}

// WorktreeCleanupResult identifies a recovery copy retained while a finalized
// worktree is retired. The zero value means the atomic quarantine was clean,
// process-quiescent, and removed through non-forced Git cleanup. A non-empty
// result is always recoverable: late activity or inconclusive quiescence is
// never fed to a recursive delete.
type WorktreeCleanupResult struct {
	RecoveryPath    string
	RecoveryMarker  string
	LateWrite       string
	RetentionReason string
	authoritySince  time.Time
}

// worktreeCleanup retires a finalized worktree after re-validating the exact
// HEAD, storage ref, tracked/untracked state, and ignored-path disposal policy.
type worktreeCleanup func(expectedHEAD, finalBranch string) (WorktreeCleanupResult, error)

// setupWorktree creates a fresh git worktree at
// <storeRoot>/worktrees/<runID>, checked out at HEAD of the repository
// containing repoHint (typically the engine's workDir before override).
// On success returns the worktreeContext, a race-safe retirement closure, and
// nil error.
func setupWorktree(
	storeRoot, runID, repoHint string,
	authoritySince time.Time,
	logger *iterlog.Logger,
) (worktreeContext, worktreeCleanup, error) {
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

	// Capture the main worktree's branch + tip BEFORE creating the new
	// worktree so we have a baseline to compare against in finalize.
	// `symbolic-ref --quiet HEAD` returns "" + non-zero on detached HEAD —
	// that's intentional: we treat detached as "no branch to FF".
	originalBranch := ""
	brCmd, brCancel := gitCmd("-C", repoRoot, "symbolic-ref", "--quiet", "--short", "HEAD")
	if out, brErr := brCmd.Output(); brErr == nil {
		originalBranch = strings.TrimSpace(string(out))
	}
	brCancel()
	originalTip := ""
	tipCmd, tipCancel := gitCmd("-C", repoRoot, "rev-parse", "HEAD")
	if out, tipErr := tipCmd.Output(); tipErr == nil {
		originalTip = strings.TrimSpace(string(out))
	}
	tipCancel()

	// `git worktree add <path> HEAD` creates the worktree at the current
	// HEAD commit. Any working-tree state (staged, unstaged, untracked)
	// in the main checkout is intentionally NOT copied — that is the whole
	// point of isolation.
	now := time.Now().UTC()
	if authoritySince.IsZero() {
		authoritySince = now
	} else if authoritySince.After(now) {
		return worktreeContext{}, nil, fmt.Errorf(
			"worktree process authority time %s is in the future",
			authoritySince.Format(time.RFC3339Nano),
		)
	}
	cmd, cancel := gitCmd("-C", repoRoot, "worktree", "add", wtPath, "HEAD")
	out, addErr := cmd.CombinedOutput()
	cancel()
	if addErr != nil {
		return worktreeContext{}, nil, fmt.Errorf("git worktree add %s: %w\noutput: %s", wtPath, addErr, string(out))
	}

	if logger != nil {
		logger.Info("runtime: worktree created at %s (base: %s HEAD %s on %s)",
			wtPath, repoRoot, shortSHA(originalTip), branchOrDetached(originalBranch))
	}

	cleanup := newWorktreeCleanup(runID, repoRoot, wtPath, authoritySince)
	return worktreeContext{
		repoRoot:       repoRoot,
		wtPath:         wtPath,
		gitDir:         resolveWorktreeGitDir(repoRoot, wtPath),
		originalBranch: originalBranch,
		originalTip:    originalTip,
		authoritySince: authoritySince,
	}, cleanup, nil
}

func newWorktreeCleanup(runID, repoRoot, wtPath string, authoritySince time.Time) worktreeCleanup {
	return func(expectedHEAD, finalBranch string) (WorktreeCleanupResult, error) {
		return cleanupRecoveredWorktreeForRun(
			runID,
			repoRoot,
			wtPath,
			finalBranch,
			expectedHEAD,
			authoritySince,
			nil,
		)
	}
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
	// PreserveWorktree is true when finalize could NOT prove that every
	// non-disposable worktree change is durably reachable (the cleanliness
	// probe / wip bank / storage-branch creation failed). The caller must skip
	// cleanup so the operator can recover files or commits by hand. Ignored
	// dependency/cache paths are governed separately by
	// isDisposableIgnoredPath.
	PreserveWorktree bool
}

// finalizeWorktree promotes the worktree's HEAD onto a persistent
// branch and best-effort fast-forwards the requested merge target.
// Promotion failures do not fail the run, but cleanup is fail-closed:
// PreserveWorktree prevents deletion until output is durably reachable.
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
		// Without a readable HEAD we cannot prove that cleanup is safe: the
		// worktree may contain commits or uncommitted output that no durable
		// ref protects. Fail closed and leave it in place.
		if logger != nil {
			logger.Warn("runtime: finalize: cannot read worktree HEAD at %s — preserving worktree", wc.wtPath)
		}
		res.PreserveWorktree = true
		return res
	}

	// 1b. Bank uncommitted work before it can be destroyed. A bot that
	// exits through a non-commit edge (a gated commit node not reached,
	// a "hold for later" outcome, an agent that edited but never
	// committed) leaves finished work as a dirty working tree; the
	// cleanup that follows finalize removes the worktree,
	// which would silently destroy it — the exact stranded-work failure
	// mode observed across the pre-ADR-055 bot-session corpus. Commit it
	// as an explicit wip bank so the storage branch preserves it; the
	// operator reviews it there (it is NEVER merged into their branch —
	// see step 5).
	if clean, cleanErr := workdirIsClean(wc.wtPath); cleanErr != nil {
		if logger != nil {
			logger.Warn("runtime: finalize: cannot probe worktree cleanliness: %v — preserving worktree", cleanErr)
		}
		// "Unknown" must not be treated as "clean": a failed status probe
		// followed by removal can destroy output we never banked.
		res.PreserveWorktree = true
		return res
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
			return res
		} else if err := runGitInDir(wc.wtPath, "commit", "-m", msg); err != nil {
			if logger != nil {
				logger.Warn("runtime: finalize: wip bank commit failed: %v — preserving worktree at %s", err, wc.wtPath)
			}
			res.PreserveWorktree = true
			return res
		} else {
			res.WipBanked = true
			banked := readHEAD(wc.wtPath)
			if banked == "" {
				if logger != nil {
					logger.Warn("runtime: finalize: wip bank commit succeeded but its HEAD cannot be read — preserving worktree at %s", wc.wtPath)
				}
				res.PreserveWorktree = true
				return res
			}
			finalSHA = banked
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
		res.PreserveWorktree = true
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
		// FinalCommit without a durable ref is only reflog-reachable once the
		// worktree is removed. Keep the worktree as the GC guard until an
		// operator can create the branch manually or recovery succeeds later.
		res.PreserveWorktree = true
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
		if mergeErr := tryFastForward(wc.repoRoot, target, finalName, finalSHA, wc.originalBranch, logger); mergeErr != nil {
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
		message := buildSquashMessage(wc.repoRoot, wc.originalTip, finalSHA, opts.runName)
		merged, mergeErr := trySquashMerge(wc.repoRoot, target, finalName, wc.originalBranch, message, logger)
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

// readBranchCommit resolves one validated local branch to its commit.
func readBranchCommit(repoRoot, branch string) (string, error) {
	if err := gitlib.ValidateBranchName(branch); err != nil {
		return "", fmt.Errorf("invalid branch name %q: %w", branch, err)
	}
	refCmd, refCancel := gitCmd("-C", repoRoot, "rev-parse", "--verify", "refs/heads/"+branch+"^{commit}")
	refOut, refErr := refCmd.Output()
	refCancel()
	if refErr != nil {
		return "", fmt.Errorf("resolve branch %q: %w", branch, refErr)
	}
	head := strings.TrimSpace(string(refOut))
	if head == "" {
		return "", fmt.Errorf("branch %q resolved to an empty commit", branch)
	}
	return head, nil
}

// verifyCleanWorktreeHEAD proves that wtPath is clean, contains no ignored
// output outside the explicit disposable-cache policy, and still points at the
// exact commit protected by finalization metadata.
func verifyCleanWorktreeHEAD(wtPath, expectedHEAD string) error {
	if strings.TrimSpace(expectedHEAD) == "" {
		return fmt.Errorf("expected worktree HEAD is empty")
	}
	head := readHEAD(wtPath)
	if head == "" {
		return fmt.Errorf("cannot read worktree HEAD")
	}
	if head != expectedHEAD {
		return fmt.Errorf("worktree HEAD changed: got %s, expected %s", shortSHA(head), shortSHA(expectedHEAD))
	}
	clean, cleanErr := workdirIsClean(wtPath)
	if cleanErr != nil {
		return fmt.Errorf("verify worktree cleanliness: %w", cleanErr)
	}
	if !clean {
		return fmt.Errorf("worktree is dirty")
	}
	protectedIgnored, ignoredErr := protectedIgnoredWorktreeEntries(wtPath)
	if ignoredErr != nil {
		return fmt.Errorf("verify ignored worktree entries: %w", ignoredErr)
	}
	if len(protectedIgnored) > 0 {
		const maxReported = 5
		reported := protectedIgnored
		if len(reported) > maxReported {
			reported = reported[:maxReported]
		}
		suffix := ""
		if remaining := len(protectedIgnored) - len(reported); remaining > 0 {
			suffix = fmt.Sprintf(" (and %d more)", remaining)
		}
		return fmt.Errorf("worktree has ignored non-disposable output: %s%s",
			strings.Join(reported, ", "), suffix)
	}
	return nil
}

// VerifyLauncherWorktreeCleanup proves that a dispatcher/launcher-owned
// worktree can be removed without losing run output. It deliberately accepts
// only current, explicitly-owned run records:
//
//   - a delegated worktree is the run's own WorkDir and its finalized HEAD is
//     protected by FinalBranch (or still equals BaseCommit when no commit was
//     produced);
//   - a managed run executes in a separate runtime worktree, so the launcher's
//     outer worktree must still equal BaseCommit.
//
// In both cases the target must remain a registered, clean linked worktree and
// contain no ignored output outside the disposable-cache policy. Any missing
// or legacy metadata fails closed.
func VerifyLauncherWorktreeCleanup(r *store.Run, launcherPath string) error {
	proof, err := launcherWorktreeCleanupProofForRun(r, launcherPath)
	if err != nil {
		return err
	}
	registered, err := registeredWorktree(proof.repoRoot, proof.worktreePath)
	if err != nil {
		return err
	}
	if !registered {
		return fmt.Errorf("launcher worktree is not registered in repository: %s", proof.worktreePath)
	}
	if err := verifyCleanWorktreeHEAD(proof.worktreePath, proof.expectedHEAD); err != nil {
		return err
	}
	if proof.finalBranch != "" {
		branchHEAD, err := readBranchCommit(proof.repoRoot, proof.finalBranch)
		if err != nil {
			return fmt.Errorf("storage branch %q is not readable: %w", proof.finalBranch, err)
		}
		if branchHEAD != proof.expectedHEAD {
			return fmt.Errorf("storage branch %q moved: got %s, expected %s",
				proof.finalBranch, shortSHA(branchHEAD), shortSHA(proof.expectedHEAD))
		}
	}
	return nil
}

// CleanupLauncherWorktree retires a dispatcher/launcher-owned linked worktree
// using the runtime's race-safe cleanup path. The ownership and durability
// proof is recomputed here; cleanup then rechecks HEAD, storage branch, tracked
// state, ignored output, and live process references. It never uses --force.
func CleanupLauncherWorktree(r *store.Run, launcherPath string) (WorktreeCleanupResult, error) {
	if r == nil || r.WorktreeCreatedAt.IsZero() {
		return WorktreeCleanupResult{}, fmt.Errorf(
			"launcher cleanup requires a trusted worktree creation time",
		)
	}
	return CleanupLauncherWorktreeSince(r, launcherPath, r.WorktreeCreatedAt)
}

// CleanupLauncherWorktreeSince is CleanupLauncherWorktree with a trusted
// launcher-owned creation boundary. Dispatcher ownership markers live outside
// the checkout, so a descendant cannot advance this timestamp to evade the
// live-process census.
func CleanupLauncherWorktreeSince(
	r *store.Run,
	launcherPath string,
	authoritySince time.Time,
) (WorktreeCleanupResult, error) {
	if authoritySince.IsZero() {
		return WorktreeCleanupResult{}, fmt.Errorf("launcher cleanup authority time is empty")
	}
	proof, err := launcherWorktreeCleanupProofForRun(r, launcherPath)
	if err != nil {
		return WorktreeCleanupResult{}, err
	}
	return cleanupRecoveredWorktreeForRun(
		r.ID,
		proof.repoRoot,
		proof.worktreePath,
		proof.finalBranch,
		proof.expectedHEAD,
		authoritySince,
		nil,
	)
}

type launcherWorktreeCleanupProof struct {
	repoRoot     string
	worktreePath string
	expectedHEAD string
	finalBranch  string
}

func launcherWorktreeCleanupProofForRun(r *store.Run, launcherPath string) (launcherWorktreeCleanupProof, error) {
	if r == nil {
		return launcherWorktreeCleanupProof{}, fmt.Errorf("cannot verify launcher cleanup without run metadata")
	}
	if r.Status != store.RunStatusFinished {
		return launcherWorktreeCleanupProof{}, fmt.Errorf("run %q is %q, not finished", r.ID, r.Status)
	}
	if !r.Worktree {
		return launcherWorktreeCleanupProof{}, fmt.Errorf("run %q has no verified worktree authority", r.ID)
	}
	if strings.TrimSpace(launcherPath) == "" {
		return launcherWorktreeCleanupProof{}, fmt.Errorf("launcher worktree path is empty")
	}
	if info, err := os.Lstat(launcherPath); err != nil {
		return launcherWorktreeCleanupProof{}, fmt.Errorf("inspect launcher worktree %s: %w", launcherPath, err)
	} else if info.Mode()&os.ModeSymlink != 0 {
		return launcherWorktreeCleanupProof{}, fmt.Errorf("launcher worktree path is a symlink: %s", launcherPath)
	}
	worktreePath, err := canonicalExistingPath(launcherPath)
	if err != nil {
		return launcherWorktreeCleanupProof{}, fmt.Errorf("canonicalize launcher worktree path: %w", err)
	}
	repoRoot, err := canonicalExistingPath(r.RepoRoot)
	if err != nil {
		return launcherWorktreeCleanupProof{}, fmt.Errorf("canonicalize launcher repository root: %w", err)
	}
	if samePath(repoRoot, worktreePath) {
		return launcherWorktreeCleanupProof{}, fmt.Errorf("refusing to remove repository root as launcher worktree: %s", worktreePath)
	}

	proof := launcherWorktreeCleanupProof{
		repoRoot:     repoRoot,
		worktreePath: worktreePath,
	}
	switch r.WorktreeOwnership {
	case store.WorktreeOwnershipDelegated:
		if err := verifyFinalizationWorktreeOwnership(nil, r); err != nil {
			return launcherWorktreeCleanupProof{}, fmt.Errorf("verify delegated worktree ownership: %w", err)
		}
		persistedPath, err := canonicalExistingPath(r.WorkDir)
		if err != nil {
			return launcherWorktreeCleanupProof{}, fmt.Errorf("canonicalize delegated worktree path: %w", err)
		}
		if !samePath(persistedPath, worktreePath) {
			return launcherWorktreeCleanupProof{}, fmt.Errorf(
				"launcher path %s does not match delegated run worktree %s",
				worktreePath, persistedPath,
			)
		}
		if r.FinalCommit != "" {
			if r.FinalBranch == "" {
				return launcherWorktreeCleanupProof{}, fmt.Errorf(
					"run %q finalized commit %s without a durable storage branch",
					r.ID, shortSHA(r.FinalCommit),
				)
			}
			proof.expectedHEAD = r.FinalCommit
			proof.finalBranch = r.FinalBranch
		} else {
			if r.FinalBranch != "" {
				return launcherWorktreeCleanupProof{}, fmt.Errorf(
					"run %q has storage branch %q without a finalized commit",
					r.ID, r.FinalBranch,
				)
			}
			proof.expectedHEAD = r.BaseCommit
		}

	case store.WorktreeOwnershipManaged:
		// The runtime-owned inner worktree may already have been removed, so
		// its path cannot be canonicalized here. A lexical equality check is
		// still sufficient to refuse treating that inner path as the
		// launcher's independently-owned outer workspace.
		if samePath(r.WorkDir, launcherPath) {
			return launcherWorktreeCleanupProof{}, fmt.Errorf(
				"managed run worktree %s is not launcher-owned",
				launcherPath,
			)
		}
		proof.expectedHEAD = r.BaseCommit

	default:
		return launcherWorktreeCleanupProof{}, fmt.Errorf(
			"run %q has unsupported worktree ownership %q",
			r.ID, r.WorktreeOwnership,
		)
	}
	if strings.TrimSpace(proof.expectedHEAD) == "" {
		return launcherWorktreeCleanupProof{}, fmt.Errorf("run %q has no expected launcher HEAD", r.ID)
	}
	return proof, nil
}

// protectedIgnoredWorktreeEntries returns ignored paths that cleanup must not
// silently delete. Git's ordinary porcelain status intentionally hides ignored
// paths, but a generated .env, export, or ignored build artefact may be the only
// copy. Fail closed for every ignored path except a deliberately small set of
// conventional dependency and cache locations.
func protectedIgnoredWorktreeEntries(wtPath string) ([]string, error) {
	out, err := runGit(wtPath, "status", "--porcelain=v1", "-z", "--ignored=matching", "--untracked-files=all")
	if err != nil {
		return nil, fmt.Errorf("git status --ignored: %w (output: %s)", err, strings.TrimSpace(out))
	}
	var protected []string
	for _, record := range strings.Split(out, "\x00") {
		if !strings.HasPrefix(record, "!! ") {
			continue
		}
		path := strings.TrimPrefix(record, "!! ")
		if path == "" || isDisposableIgnoredPath(path) {
			continue
		}
		protected = append(protected, path)
	}
	return protected, nil
}

// isDisposableIgnoredPath is the explicit cleanup contract for ignored paths.
// It intentionally covers only reproducible dependency/cache material. Unknown
// ignored output is preserved with the worktree, even at the cost of a leak,
// because deletion would be irreversible.
func isDisposableIgnoredPath(path string) bool {
	clean := strings.Trim(filepath.ToSlash(path), "/")
	if clean == "" {
		return false
	}
	parts := strings.Split(clean, "/")
	for _, part := range parts {
		switch part {
		case "node_modules", "__pycache__":
			return true
		}
	}
	switch parts[0] {
	case ".cache", ".npm", ".pnpm-store", ".pytest_cache", ".mypy_cache",
		".ruff_cache", ".tox", ".nox", ".gradle", ".parcel-cache", ".turbo",
		".vite":
		return true
	}
	if len(parts) >= 2 && parts[0] == ".yarn" && parts[1] == "cache" {
		return true
	}
	base := parts[len(parts)-1]
	return base == ".DS_Store" || base == ".eslintcache" || strings.HasSuffix(base, ".pyc")
}

// createBranchSafely creates a branch at sha; on collision, it reuses an
// existing candidate only when that branch already protects the exact same
// commit (the crash-retry case), otherwise it retries with a suffix (-1, -2,
// …) up to 16 times. Returns the final branch name, or "" on total failure.
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
		// an exact same-SHA branch is an idempotent recovery success.
		if !strings.Contains(string(out), "already exists") {
			if logger != nil {
				logger.Warn("runtime: finalize: git branch %s failed: %v\noutput: %s", candidate, err, string(out))
			}
			return false, ""
		}
		if existing, resolveErr := readBranchCommit(repoRoot, candidate); resolveErr == nil && existing == sha {
			if logger != nil {
				logger.Info("runtime: finalize: reusing existing branch %s already at %s", candidate, shortSHA(sha))
			}
			return true, candidate
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

func registeredWorktree(repoRoot, wtPath string) (bool, error) {
	listCmd, listCancel := gitCmd("-C", repoRoot, "worktree", "list", "--porcelain", "-z")
	listOut, listErr := listCmd.CombinedOutput()
	listCancel()
	if listErr != nil {
		return false, fmt.Errorf("list registered worktrees: %w (output: %s)", listErr, strings.TrimSpace(string(listOut)))
	}
	for _, field := range strings.Split(string(listOut), "\x00") {
		const prefix = "worktree "
		if strings.HasPrefix(field, prefix) && samePath(strings.TrimPrefix(field, prefix), wtPath) {
			return true, nil
		}
	}
	return false, nil
}

// verifyRecoveredWorktreeOwnership binds persisted run metadata back to the
// only path Iterion creates for that run. Merely appearing in `git worktree
// list` is not enough: stale/corrupt metadata could otherwise point at a
// different, user-owned registered worktree and recovery would mutate it.
func verifyRecoveredWorktreeOwnership(st store.RunStore, r *store.Run) error {
	if st == nil || r == nil {
		return fmt.Errorf("cannot verify recovered worktree ownership without store and run")
	}
	runID := strings.TrimSpace(r.ID)
	if runID == "" || runID != r.ID || runID == "." || runID == ".." ||
		strings.Contains(runID, "/") || strings.Contains(runID, `\`) {
		return fmt.Errorf("invalid run ID for recovered worktree ownership: %q", r.ID)
	}
	storeRoot := strings.TrimSpace(st.Root())
	if storeRoot == "" {
		return fmt.Errorf("run store has no filesystem root for worktree ownership verification")
	}
	expected, err := filepath.Abs(filepath.Join(storeRoot, "worktrees", runID))
	if err != nil {
		return fmt.Errorf("resolve expected worktree path: %w", err)
	}
	actual, err := filepath.Abs(r.WorkDir)
	if err != nil {
		return fmt.Errorf("resolve persisted worktree path: %w", err)
	}
	if info, lstatErr := os.Lstat(expected); lstatErr != nil {
		return fmt.Errorf("inspect expected worktree path %s: %w", expected, lstatErr)
	} else if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("expected worktree path is a symlink: %s", expected)
	}
	expectedReal, err := filepath.EvalSymlinks(expected)
	if err != nil {
		return fmt.Errorf("canonicalize expected worktree path %s: %w", expected, err)
	}
	actualReal, err := filepath.EvalSymlinks(actual)
	if err != nil {
		return fmt.Errorf("canonicalize persisted worktree path %s: %w", actual, err)
	}
	if !samePath(expectedReal, actualReal) {
		return fmt.Errorf("run %s does not own recovered worktree %s (expected %s)", runID, actualReal, expectedReal)
	}
	return nil
}

// verifyFinalizationWorktreeOwnership selects the proof appropriate to the
// persisted authority source. Legacy records are accepted only through the
// stronger managed-path convention; an untyped arbitrary linked worktree never
// gains resume/recovery authority.
func verifyFinalizationWorktreeOwnership(st store.RunStore, r *store.Run) error {
	if r == nil {
		return fmt.Errorf("cannot verify nil worktree run")
	}
	switch r.WorktreeOwnership {
	case "", store.WorktreeOwnershipManaged:
		if err := verifyRecoveredWorktreeOwnership(st, r); err != nil {
			return err
		}
		if r.WorktreeGitDir != "" {
			if err := verifyPersistedWorktreeGitDir(r); err != nil {
				return err
			}
		}
		return nil
	case store.WorktreeOwnershipDelegated:
		if samePath(r.WorkDir, r.RepoRoot) {
			return fmt.Errorf("delegated worktree path is the repository root: %s", r.WorkDir)
		}
		if r.WorktreeGitDir == "" {
			return fmt.Errorf("delegated worktree has no persisted private Git directory")
		}
		if info, err := os.Lstat(r.WorkDir); err != nil {
			return fmt.Errorf("inspect delegated worktree %s: %w", r.WorkDir, err)
		} else if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("delegated worktree path is a symlink: %s", r.WorkDir)
		}
		registered, err := registeredWorktree(r.RepoRoot, r.WorkDir)
		if err != nil {
			return err
		}
		if !registered {
			return fmt.Errorf("delegated worktree is not registered in repository: %s", r.WorkDir)
		}
		return verifyPersistedWorktreeGitDir(r)
	default:
		return fmt.Errorf("unknown worktree ownership %q", r.WorktreeOwnership)
	}
}

func verifyPersistedWorktreeGitDir(r *store.Run) error {
	actual := resolveWorktreeGitDir(r.RepoRoot, r.WorkDir)
	actualCanonical, err := canonicalExistingPath(actual)
	if err != nil {
		return fmt.Errorf("canonicalize actual worktree Git directory: %w", err)
	}
	persistedCanonical, err := canonicalExistingPath(r.WorktreeGitDir)
	if err != nil {
		return fmt.Errorf("canonicalize persisted worktree Git directory: %w", err)
	}
	if !samePath(actualCanonical, persistedCanonical) {
		return fmt.Errorf("worktree private Git directory changed: got %s, expected %s", actualCanonical, persistedCanonical)
	}
	return nil
}

func canonicalExistingPath(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", fmt.Errorf("path is empty")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(absolute)
}

// cleanupRecoveredWorktree retires a terminal run's dedicated worktree only
// after re-proving every condition required by the durability policy. The
// compatibility wrapper intentionally discards the recovery-copy location;
// launcher cleanup and live engine cleanup use cleanupRecoveredWorktreeForRun
// so they can surface it to operators.
func cleanupRecoveredWorktree(
	repoRoot, wtPath, finalBranch, expectedHEAD string,
	authoritySince time.Time,
) error {
	_, err := cleanupRecoveredWorktreeForRun(
		"",
		repoRoot,
		wtPath,
		finalBranch,
		expectedHEAD,
		authoritySince,
		nil,
	)
	return err
}

// cleanupRecoveredWorktreeForRun retires a terminal run's dedicated worktree
// after re-proving every condition required by the durability policy:
//
//   - the path is still a registered worktree of repoRoot (never an arbitrary
//     directory from stale/corrupt run metadata);
//   - its HEAD is the expected commit;
//   - when a storage branch is expected, that branch still protects the same
//     commit;
//   - the worktree is clean and has no non-disposable ignored output
//     immediately before retirement.
//
// Cleanup deliberately does not use --force or recursively delete anything.
// Git lockfiles and a guard ref close commit/ref races; an atomic same-parent
// rename closes ordinary writer races by moving their cwd inode into a
// registered recovery worktree. Only a process-quiescent, still-clean recovery
// is removed; otherwise reconciliation preserves it (and any guard ref) for
// inspection.
func cleanupRecoveredWorktreeForRun(
	runID, repoRoot, wtPath, finalBranch, expectedHEAD string,
	authoritySince time.Time,
	hooks *worktreeCleanupTestHooks,
) (WorktreeCleanupResult, error) {
	if authoritySince.IsZero() {
		return WorktreeCleanupResult{}, fmt.Errorf(
			"refusing worktree cleanup without a trusted process authority time",
		)
	}
	if now := time.Now().UTC(); authoritySince.After(now) {
		return WorktreeCleanupResult{}, fmt.Errorf(
			"refusing worktree cleanup with future process authority time %s",
			authoritySince.Format(time.RFC3339Nano),
		)
	}
	if samePath(repoRoot, wtPath) {
		return WorktreeCleanupResult{}, fmt.Errorf("refusing to retire repository root as recovered worktree: %s", wtPath)
	}
	if _, err := os.Stat(wtPath); err != nil {
		if os.IsNotExist(err) {
			return WorktreeCleanupResult{}, nil // already retired; idempotent recovery
		}
		return WorktreeCleanupResult{}, fmt.Errorf("stat recovered worktree %s: %w", wtPath, err)
	}

	registered, registerErr := registeredWorktree(repoRoot, wtPath)
	if registerErr != nil {
		return WorktreeCleanupResult{}, registerErr
	}
	if !registered {
		return WorktreeCleanupResult{}, fmt.Errorf("refusing to retire unregistered recovered worktree path %s", wtPath)
	}
	return quarantineFinalizedWorktree(
		runID,
		repoRoot,
		wtPath,
		finalBranch,
		expectedHEAD,
		authoritySince,
		hooks,
	)
}

// worktreeCleanupTestHooks is an internal deterministic seam for adversarial
// race tests. Production always passes nil.
type worktreeCleanupTestHooks struct {
	afterFinalVerification func()
	afterRename            func(WorktreeCleanupResult)
	processReferences      func(root string, authoritySince time.Time) ([]int, error)
}

// quarantineFinalizedWorktree closes the check→remove races that even a plain
// non-forced `git worktree remove` still permits:
//
//   - Git happily removes a clean worktree after a concurrent empty commit,
//     even when that new HEAD has no durable ref.
//   - a storage branch can move after validation and before removal.
//   - Git ignores ignored paths during removal, so an ordinary writer can
//     create the only copy of an artefact after the last status check and have
//     it recursively deleted.
//
// A hidden guard ref protects expectedHEAD while Git lockfiles block commits in
// this worktree. We then re-check HEAD/cleanliness/ref under those locks and
// atomically rename the checkout to a unique same-parent recovery path. Open
// cwd/file descriptors keep referring to that renamed tree, so relative writes
// arriving after the check are preserved. `git worktree repair` makes the
// recovery path the registered location; nothing is recursively deleted.
//
// A writer using the old absolute path may recreate it after the rename. That
// path is also left untouched and reported as an error. Any uncertainty leaks a
// recovery worktree/ref instead of losing output.
func quarantineFinalizedWorktree(
	runID, repoRoot, wtPath, finalBranch, expectedHEAD string,
	authoritySince time.Time,
	hooks *worktreeCleanupTestHooks,
) (result WorktreeCleanupResult, retErr error) {
	if err := verifyCleanWorktreeHEAD(wtPath, expectedHEAD); err != nil {
		return result, err
	}

	guardRef := ""
	if finalBranch != "" {
		branchHead, branchErr := readBranchCommit(repoRoot, finalBranch)
		if branchErr != nil {
			return result, fmt.Errorf("storage branch %q is not readable: %w", finalBranch, branchErr)
		}
		if branchHead != expectedHEAD {
			return result, fmt.Errorf("storage branch %q moved: got %s, expected %s", finalBranch, shortSHA(branchHead), shortSHA(expectedHEAD))
		}
		var guardErr error
		guardRef, guardErr = createCleanupGuard(repoRoot, expectedHEAD)
		if guardErr != nil {
			return result, guardErr
		}
		// On a pre-removal failure, discard the guard only through a ref
		// transaction that simultaneously proves the storage branch still
		// protects expectedHEAD. If that proof fails, deliberately retain it.
		defer func() {
			if guardRef == "" {
				return
			}
			if releaseErr := releaseCleanupGuard(repoRoot, guardRef, finalBranch, expectedHEAD); releaseErr != nil {
				if retErr == nil {
					retErr = fmt.Errorf("cleanup guard retained at %s: %w", guardRef, releaseErr)
				} else {
					retErr = fmt.Errorf("%w (cleanup guard retained at %s: %v)", retErr, guardRef, releaseErr)
				}
				return
			}
			guardRef = ""
		}()
	}

	unlockGit, lockErr := acquireWorktreeCleanupLocks(repoRoot, wtPath)
	if lockErr != nil {
		return result, lockErr
	}
	locksHeld := true
	releaseGitLocks := func() {
		if locksHeld {
			unlockGit()
			locksHeld = false
		}
	}
	defer releaseGitLocks()

	// Re-check after acquiring the Git lockfiles: a commit/write that won the
	// race before us is now visible and makes cleanup fail closed.
	if err := verifyCleanWorktreeHEAD(wtPath, expectedHEAD); err != nil {
		return result, err
	}
	if finalBranch != "" {
		branchHead, branchErr := readBranchCommit(repoRoot, finalBranch)
		if branchErr != nil {
			return result, fmt.Errorf("storage branch %q is not readable: %w", finalBranch, branchErr)
		}
		if branchHead != expectedHEAD {
			return result, fmt.Errorf("storage branch %q moved: got %s, expected %s", finalBranch, shortSHA(branchHead), shortSHA(expectedHEAD))
		}
	}
	if hooks != nil && hooks.afterFinalVerification != nil {
		hooks.afterFinalVerification()
	}

	var reserveErr error
	result, reserveErr = reserveWorktreeRecovery(
		runID,
		repoRoot,
		wtPath,
		finalBranch,
		expectedHEAD,
		authoritySince,
	)
	if reserveErr != nil {
		return result, reserveErr
	}
	if renameErr := os.Rename(wtPath, result.RecoveryPath); renameErr != nil {
		_ = os.Remove(result.RecoveryMarker)
		return WorktreeCleanupResult{}, fmt.Errorf(
			"atomically quarantine finalized worktree %s at %s: %w",
			wtPath, result.RecoveryPath, renameErr,
		)
	}
	if hooks != nil && hooks.afterRename != nil {
		hooks.afterRename(result)
	}

	// os.Rename deliberately leaves the worktree's private `gitdir` backlink
	// pointing at the old path. Repair that single administrative pointer so
	// the recovery copy remains a first-class linked worktree. Unlike
	// `git worktree remove <old>`, repair cannot recursively delete a directory
	// that a late absolute-path writer recreated.
	repairCmd, repairCancel := gitCmd("-C", repoRoot, "worktree", "repair", result.RecoveryPath)
	repairOut, repairErr := repairCmd.CombinedOutput()
	repairCancel()
	if repairErr != nil {
		return result, fmt.Errorf(
			"worktree quarantined at %s but Git registration repair failed: %w (output: %s)",
			result.RecoveryPath, repairErr, strings.TrimSpace(string(repairOut)),
		)
	}
	registered, registerErr := registeredWorktree(repoRoot, result.RecoveryPath)
	if registerErr != nil {
		return result, fmt.Errorf("worktree quarantined at %s but registration cannot be verified: %w", result.RecoveryPath, registerErr)
	}
	if !registered {
		return result, fmt.Errorf("worktree quarantined at %s but is not registered there", result.RecoveryPath)
	}

	if guardRef != "" {
		// releaseCleanupGuard verifies the storage ref and deletes the guard in
		// one ref transaction. Clear guardRef only on success so the deferred
		// fallback retains/reports it on a moved branch.
		releaseGitLocks()
		if releaseErr := releaseCleanupGuard(repoRoot, guardRef, finalBranch, expectedHEAD); releaseErr != nil {
			return result, fmt.Errorf("worktree quarantined but cleanup guard retained at %s: %w", guardRef, releaseErr)
		}
		guardRef = ""
	}

	// Re-check the recovery copy after the rename. Any new output makes the
	// quarantine permanent; it is never fed to a recursive delete.
	if verifyErr := verifyCleanWorktreeHEAD(result.RecoveryPath, expectedHEAD); verifyErr != nil {
		result.LateWrite = verifyErr.Error()
	}
	if _, statErr := os.Lstat(wtPath); statErr == nil {
		return result, fmt.Errorf(
			"original worktree path %s was recreated during quarantine; both it and recovery copy %s were preserved",
			wtPath, result.RecoveryPath,
		)
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return result, fmt.Errorf(
			"cannot prove original worktree path %s stayed absent after quarantine at %s: %w",
			wtPath, result.RecoveryPath, statErr,
		)
	}

	// A dirty recovery copy is the evidence: keep it registered and return the
	// manifest to the operator. Otherwise prove that no same-user process has
	// a cwd or open descriptor inside it across a quiet window. Only then is
	// the quarantine eligible for an ordinary, non-forced Git removal.
	if result.LateWrite != "" {
		return result, nil
	}
	removed, retireErr := retireQuiescentRecovery(
		repoRoot,
		wtPath,
		expectedHEAD,
		&result,
		releaseGitLocks,
		hooks,
	)
	if retireErr != nil {
		return result, retireErr
	}
	if !removed {
		return result, nil
	}
	return WorktreeCleanupResult{}, nil
}

// retireQuiescentRecovery bounds normal cleanup without reopening the
// check→delete writer race. Once the original path has been renamed, a stale
// relative writer can reach the checkout only through a cwd/open descriptor;
// worktreeProcessReferences proves that no same-user process holds either
// across a short quiet window. A stale absolute-path writer can only recreate
// originalPath, which is independently checked and (for dispatcher workspaces)
// cannot collide with a later run because paths are generation-specific.
//
// Unsupported or inconclusive process inspection fails closed and retains the
// recovery worktree. The removal itself is Git's non-forced worktree removal,
// never os.RemoveAll.
func retireQuiescentRecovery(
	repoRoot, originalPath, expectedHEAD string,
	result *WorktreeCleanupResult,
	releaseGitLocks func(),
	hooks *worktreeCleanupTestHooks,
) (bool, error) {
	if result == nil || result.RecoveryPath == "" || result.RecoveryMarker == "" {
		return false, fmt.Errorf("cannot retire recovery worktree without its path and manifest")
	}
	recoveryInfo, err := os.Stat(result.RecoveryPath)
	if err != nil {
		return false, fmt.Errorf("stat recovery worktree before quiescence proof: %w", err)
	}
	originalMode := recoveryInfo.Mode().Perm()
	hardenedMode := originalMode &^ 0o022
	if hardenedMode != originalMode {
		if err := os.Chmod(result.RecoveryPath, hardenedMode); err != nil {
			result.RetentionReason = "recovery permissions could not be hardened"
			return false, fmt.Errorf(
				"worktree retained at %s because group/other write access could not be revoked: %w",
				result.RecoveryPath,
				err,
			)
		}
		defer func() {
			// Restore operator-visible permissions when the recovery is
			// retained. ENOENT is expected after successful Git removal.
			_ = os.Chmod(result.RecoveryPath, originalMode)
		}()
	}
	processReferences := cleanupProcessReferences
	if hooks != nil && hooks.processReferences != nil {
		processReferences = hooks.processReferences
	}
	checkReferences := func() (bool, error) {
		pids, err := processReferences(result.RecoveryPath, result.authoritySince)
		if err != nil {
			result.RetentionReason = "process quiescence could not be proved"
			return false, err
		}
		if len(pids) > 0 {
			result.RetentionReason = fmt.Sprintf(
				"live process references recovery worktree (pids: %s)",
				formatProcessIDs(pids),
			)
			return false, nil
		}
		return true, nil
	}

	quiet, err := checkReferences()
	if err != nil {
		return false, fmt.Errorf(
			"worktree retained at %s because process quiescence cannot be proved: %w",
			result.RecoveryPath,
			err,
		)
	}
	if !quiet {
		return false, nil
	}

	timer := time.NewTimer(worktreeQuiescenceWindow)
	<-timer.C

	if err := verifyCleanWorktreeHEAD(result.RecoveryPath, expectedHEAD); err != nil {
		result.LateWrite = err.Error()
		return false, nil
	}
	if _, err := os.Lstat(originalPath); err == nil {
		result.RetentionReason = "original path was recreated during quiescence check"
		return false, fmt.Errorf(
			"original worktree path %s was recreated while recovery copy %s was retained",
			originalPath,
			result.RecoveryPath,
		)
	} else if !errors.Is(err, os.ErrNotExist) {
		result.RetentionReason = "original path absence could not be proved"
		return false, fmt.Errorf(
			"cannot prove original worktree path %s stayed absent while recovery copy %s was retained: %w",
			originalPath,
			result.RecoveryPath,
			err,
		)
	}
	quiet, err = checkReferences()
	if err != nil {
		return false, fmt.Errorf(
			"worktree retained at %s because process quiescence cannot be re-proved: %w",
			result.RecoveryPath,
			err,
		)
	}
	if !quiet {
		return false, nil
	}

	// Git must acquire its own administrative locks for worktree removal.
	// Release our verification locks only after both quiescence snapshots and
	// the final cleanliness proof.
	releaseGitLocks()
	removeCmd, removeCancel := gitCmd("-C", repoRoot, "worktree", "remove", result.RecoveryPath)
	removeOut, removeErr := removeCmd.CombinedOutput()
	removeCancel()
	if removeErr != nil {
		result.RetentionReason = "non-forced Git removal failed"
		return false, fmt.Errorf(
			"remove quiescent recovery worktree %s: %w (output: %s)",
			result.RecoveryPath,
			removeErr,
			strings.TrimSpace(string(removeOut)),
		)
	}

	// Preserve the manifest if an absolute-path writer recreated the old
	// generation during Git removal. The recreated path is never deleted.
	if _, err := os.Lstat(originalPath); err == nil {
		result.RetentionReason = "original path was recreated during recovery removal"
		return false, fmt.Errorf(
			"original worktree path %s was recreated while quiescent recovery %s was removed; recreated output was preserved",
			originalPath,
			result.RecoveryPath,
		)
	} else if !errors.Is(err, os.ErrNotExist) {
		result.RetentionReason = "original path absence could not be proved after recovery removal"
		return false, fmt.Errorf(
			"cannot prove original worktree path %s stayed absent after removing recovery %s: %w",
			originalPath,
			result.RecoveryPath,
			err,
		)
	}
	if err := os.Remove(result.RecoveryMarker); err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf(
			"recovery worktree was removed but manifest %s could not be removed: %w",
			result.RecoveryMarker,
			err,
		)
	}
	return true, nil
}

func formatProcessIDs(pids []int) string {
	parts := make([]string, 0, len(pids))
	for _, pid := range pids {
		parts = append(parts, fmt.Sprintf("%d", pid))
	}
	return strings.Join(parts, ",")
}

type worktreeRecoveryManifest struct {
	FormatVersion  int       `json:"format_version"`
	RunID          string    `json:"run_id,omitempty"`
	RepositoryRoot string    `json:"repository_root"`
	OriginalPath   string    `json:"original_path"`
	RecoveryPath   string    `json:"recovery_path"`
	ExpectedHEAD   string    `json:"expected_head"`
	FinalBranch    string    `json:"final_branch,omitempty"`
	AuthoritySince time.Time `json:"authority_since"`
	QuarantinedAt  time.Time `json:"quarantined_at"`
}

// reserveWorktreeRecovery chooses a same-parent destination (required for an
// atomic directory rename) and durably writes a sidecar manifest before the
// move. A crash on either side of the rename therefore leaves a human-readable
// breadcrumb containing both possible locations.
func reserveWorktreeRecovery(
	runID, repoRoot, wtPath, finalBranch, expectedHEAD string,
	authoritySince time.Time,
) (WorktreeCleanupResult, error) {
	if authoritySince.IsZero() {
		return WorktreeCleanupResult{}, fmt.Errorf(
			"cannot reserve worktree recovery without trusted creation time",
		)
	}
	parent := filepath.Dir(wtPath)
	for attempts := 0; attempts < 16; attempts++ {
		now := time.Now().UTC()
		recoveryPath := filepath.Join(parent, fmt.Sprintf(
			".iterion-recovery-%d-%d-%d",
			os.Getpid(), now.UnixNano(), cleanupRecoveryNonce.Add(1),
		))
		if _, err := os.Lstat(recoveryPath); err == nil {
			continue
		} else if !errors.Is(err, os.ErrNotExist) {
			return WorktreeCleanupResult{}, fmt.Errorf("inspect worktree recovery path %s: %w", recoveryPath, err)
		}

		markerPath := recoveryPath + ".json"
		marker, err := os.OpenFile(markerPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return WorktreeCleanupResult{}, fmt.Errorf("reserve worktree recovery marker %s: %w", markerPath, err)
		}
		manifest := worktreeRecoveryManifest{
			FormatVersion:  1,
			RunID:          runID,
			RepositoryRoot: repoRoot,
			OriginalPath:   wtPath,
			RecoveryPath:   recoveryPath,
			ExpectedHEAD:   expectedHEAD,
			FinalBranch:    finalBranch,
			AuthoritySince: authoritySince,
			QuarantinedAt:  now,
		}
		writeErr := json.NewEncoder(marker).Encode(manifest)
		if writeErr == nil {
			writeErr = marker.Sync()
		}
		if closeErr := marker.Close(); writeErr == nil {
			writeErr = closeErr
		}
		if writeErr != nil {
			_ = os.Remove(markerPath)
			return WorktreeCleanupResult{}, fmt.Errorf("write worktree recovery marker %s: %w", markerPath, writeErr)
		}
		return WorktreeCleanupResult{
			RecoveryPath:   recoveryPath,
			RecoveryMarker: markerPath,
			authoritySince: authoritySince,
		}, nil
	}
	return WorktreeCleanupResult{}, fmt.Errorf("could not reserve a unique recovery path beside %s", wtPath)
}

func createCleanupGuard(repoRoot, expectedHEAD string) (string, error) {
	// The explicit all-zero old OID makes creation compare-and-set: even an
	// extremely unlikely name collision can never move another cleanup's
	// guard. The process-local nonce avoids relying solely on UnixNano, whose
	// resolution is coarse on some platforms.
	zeroOID := strings.Repeat("0", len(expectedHEAD))
	guardRef := fmt.Sprintf("refs/iterion/cleanup-guards/%d-%d-%d",
		os.Getpid(), time.Now().UnixNano(), cleanupGuardNonce.Add(1))
	cmd, cancel := gitCmd("-C", repoRoot, "update-ref", guardRef, expectedHEAD, zeroOID)
	out, err := cmd.CombinedOutput()
	cancel()
	if err != nil {
		return "", fmt.Errorf("create cleanup guard %s: %w (output: %s)",
			guardRef, err, strings.TrimSpace(string(out)))
	}
	return guardRef, nil
}

// releaseCleanupGuard atomically proves that finalBranch still protects
// expectedHEAD and deletes guardRef. If the branch moved, the transaction fails
// and the hidden guard remains as the durable recovery ref.
func releaseCleanupGuard(repoRoot, guardRef, finalBranch, expectedHEAD string) error {
	if err := gitlib.ValidateBranchName(finalBranch); err != nil {
		return fmt.Errorf("invalid storage branch %q: %w", finalBranch, err)
	}
	transaction := "verify refs/heads/" + finalBranch + " " + expectedHEAD + "\n" +
		"delete " + guardRef + " " + expectedHEAD + "\n"
	cmd, cancel := gitCmd("-C", repoRoot, "update-ref", "--stdin")
	cmd.Stdin = strings.NewReader(transaction)
	out, err := cmd.CombinedOutput()
	cancel()
	if err != nil {
		return fmt.Errorf("release cleanup guard: %w (output: %s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// acquireWorktreeCleanupLocks participates in Git's own lockfile protocol.
// index.lock blocks new staging/commit operations and HEAD.lock covers detached
// HEAD updates and symbolic-ref changes. We deliberately do not leave a lock in
// the common branch-ref namespace: an attached HEAD update remains durable via
// that branch, while a crash-created stale branch lock could disable the
// operator's branch indefinitely. Existing worktree-local locks make
// acquisition fail immediately, so another Git operation wins and cleanup
// preserves the worktree.
func acquireWorktreeCleanupLocks(repoRoot, wtPath string) (func(), error) {
	gitDir := resolveWorktreeGitDir(repoRoot, wtPath)
	if gitDir == "" {
		return nil, fmt.Errorf("cannot resolve worktree git directory for cleanup")
	}
	lockPaths := []string{
		filepath.Join(gitDir, "index.lock"),
		filepath.Join(gitDir, "HEAD.lock"),
	}

	created := make([]string, 0, len(lockPaths))
	unlock := func() {
		for i := len(created) - 1; i >= 0; i-- {
			_ = os.Remove(created[i])
		}
	}
	for _, lockPath := range lockPaths {
		f, err := os.OpenFile(lockPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			unlock()
			return nil, fmt.Errorf("acquire Git cleanup lock %s: %w", lockPath, err)
		}
		if closeErr := f.Close(); closeErr != nil {
			unlock()
			_ = os.Remove(lockPath)
			return nil, fmt.Errorf("close Git cleanup lock %s: %w", lockPath, closeErr)
		}
		created = append(created, lockPath)
	}
	return unlock, nil
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
// The recovery is idempotent. A run whose storage branch was already persisted
// only retries the safe cleanup step; an unfinalized run rebuilds a minimal
// worktreeContext and calls finalizeWorktree with autoMerge=false so the user
// retains UI control over the deferred merge.
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
	// `failed` is also recovered (F-RT-5): a hard failure may
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
	if _, err := os.Stat(r.WorkDir); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("stat recovered worktree %s: %w", r.WorkDir, err)
	}
	if ownershipErr := verifyFinalizationWorktreeOwnership(st, r); ownershipErr != nil {
		return fmt.Errorf("refusing recovered worktree mutation: %w", ownershipErr)
	}
	registered, registerErr := registeredWorktree(r.RepoRoot, r.WorkDir)
	if registerErr != nil {
		return registerErr
	}
	if !registered {
		return fmt.Errorf("refusing to finalize unregistered recovered worktree path %s", r.WorkDir)
	}
	// Only a successfully finished run is eligible for automatic cleanup.
	// Cancelled/failed runs retain their worktree for inspection even after a
	// storage branch has made committed output durable; ignored or otherwise
	// non-censused files may still be diagnostically important.
	cleanupEligible := r.Status == store.RunStatusFinished &&
		r.WorktreeOwnership != store.WorktreeOwnershipDelegated &&
		!r.WorktreeCreatedAt.IsZero()
	if r.Status == store.RunStatusFinished &&
		r.WorktreeOwnership != store.WorktreeOwnershipDelegated &&
		r.WorktreeCreatedAt.IsZero() &&
		logger != nil {
		logger.Warn(
			"runtime: RecoverFinalize: run %s predates trusted worktree creation metadata — finalizing but preserving %s",
			r.ID,
			r.WorkDir,
		)
	}
	cleanupRecovered := func(finalBranch, expectedHEAD string) error {
		result, err := cleanupRecoveredWorktreeForRun(
			r.ID,
			r.RepoRoot,
			r.WorkDir,
			finalBranch,
			expectedHEAD,
			r.WorktreeCreatedAt,
			nil,
		)
		if logger != nil && result.RecoveryPath != "" {
			logger.Warn(
				"runtime: recovered worktree retained at %s (manifest: %s, late_write=%q, reason=%q)",
				result.RecoveryPath,
				result.RecoveryMarker,
				result.LateWrite,
				result.RetentionReason,
			)
		}
		return err
	}
	// The metadata may have been saved just before a crash that prevented the
	// normal cleanup. Re-verify the durable ref + clean worktree and finish that
	// last step for successful runs only.
	if r.FinalBranch != "" {
		if !cleanupEligible {
			return nil
		}
		return cleanupRecovered(r.FinalBranch, r.FinalCommit)
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
		if res.PreserveWorktree {
			return nil
		}
		if !cleanupEligible {
			return nil
		}
		// A clean worktree whose HEAD still equals BaseCommit produced no work.
		// Remove it with the same conservative checks; future reconciliation is
		// then a silent no-op because the path no longer exists.
		return cleanupRecovered("", r.BaseCommit)
	}
	// An in-process auto-merge can succeed in Git and then lose only its
	// metadata SaveRun. Recovery intentionally never performs a NEW merge, but
	// it must reconcile a merge that is already visible on the currently
	// checked-out target instead of persisting the misleading "skipped" result
	// produced by mergeInto:"none" above.
	if target, mergedCommit, ok := detectRecoveredAutoMerge(r, res.FinalCommit); ok {
		res.MergedInto = target
		res.MergedCommit = mergedCommit
		res.MergeStatus = string(store.MergeStatusMerged)
		if logger != nil {
			logger.Info("runtime: recovered already-applied auto-merge for run %s into %s at %s",
				r.ID, target, shortSHA(mergedCommit))
		}
	}
	updated := *r
	updated.FinalCommit = res.FinalCommit
	updated.FinalBranch = res.FinalBranch
	updated.FinalBranchError = res.FinalBranchError
	updated.MergeStatus = store.MergeStatus(res.MergeStatus)
	if res.MergedInto != "" {
		updated.MergedInto = res.MergedInto
	}
	if res.MergedCommit != "" {
		updated.MergedCommit = res.MergedCommit
	}
	// Persist the durable recovery metadata before removing its worktree. If the
	// save fails, keep both the caller-visible run and the registered worktree
	// unchanged so retrying with the same pointer cannot bypass persistence.
	if err := st.SaveRun(ctx, &updated); err != nil {
		return err
	}
	*r = updated
	if logger != nil && res.FinalBranch != "" {
		logger.Info("runtime: recovered finalize for run %s → branch %s (commit %s, status %s)",
			r.ID, res.FinalBranch, shortSHA(res.FinalCommit), res.MergeStatus)
	}
	if res.PreserveWorktree || res.FinalBranch == "" || !cleanupEligible {
		return nil
	}
	return cleanupRecovered(res.FinalBranch, res.FinalCommit)
}

// detectRecoveredAutoMerge recognizes the narrow crash window where Git merge
// succeeded but the run metadata save did not. It never mutates Git.
//
// The original merge target is necessarily the currently checked-out branch:
// guardMergeTarget rejects any other target. For a history-preserving merge,
// FinalCommit being an ancestor is conclusive. For squash, the target's current
// tree must exactly equal the finalized tree and must have advanced from the
// recorded base. The latter avoids calling an opted-out empty-tree result
// "merged".
func detectRecoveredAutoMerge(r *store.Run, finalCommit string) (target, mergedCommit string, ok bool) {
	if r == nil || !r.AutoMerge || r.RepoRoot == "" || finalCommit == "" {
		return "", "", false
	}
	targetOut, targetCancel := gitCmd("-C", r.RepoRoot, "symbolic-ref", "--quiet", "--short", "HEAD")
	targetBytes, targetErr := targetOut.Output()
	targetCancel()
	target = strings.TrimSpace(string(targetBytes))
	if targetErr != nil || target == "" {
		return "", "", false
	}
	mergedCommit = readHEAD(r.RepoRoot)
	if mergedCommit == "" {
		return "", "", false
	}
	strategy := strings.ToLower(strings.TrimSpace(string(r.MergeStrategy)))
	if strategy == "" {
		strategy = "squash"
	}
	switch strategy {
	case "merge":
		ancestorCmd, ancestorCancel := gitCmd("-C", r.RepoRoot, "merge-base", "--is-ancestor", finalCommit, mergedCommit)
		ancestorErr := ancestorCmd.Run()
		ancestorCancel()
		return target, mergedCommit, ancestorErr == nil
	case "squash":
		if mergedCommit == r.BaseCommit {
			return "", "", false
		}
		finalTree := readGitObject(r.RepoRoot, finalCommit+"^{tree}")
		targetTree := readGitObject(r.RepoRoot, mergedCommit+"^{tree}")
		return target, mergedCommit, finalTree != "" && finalTree == targetTree
	default:
		return "", "", false
	}
}

func readGitObject(repoRoot, revision string) string {
	cmd, cancel := gitCmd("-C", repoRoot, "rev-parse", "--verify", revision)
	out, err := cmd.Output()
	cancel()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
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
