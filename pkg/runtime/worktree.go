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
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// worktreeContext is the state captured at setupWorktree time and
// consumed by finalizeWorktree to decide whether the run actually
// produced new commits and whether a fast-forward of the user's branch
// is safe.
type worktreeContext struct {
	repoRoot       string // absolute path to the main repo (where .git lives)
	wtPath         string // absolute path to the per-run worktree
	originalBranch string // current branch on the main worktree at run start ("" if detached)
	originalTip    string // SHA of HEAD at run start (worktree initial state)
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

	// Capture the main worktree's branch + tip BEFORE creating the new
	// worktree so we have a baseline to compare against in finalize.
	// `symbolic-ref --quiet HEAD` returns "" + non-zero on detached HEAD —
	// that's intentional: we treat detached as "no branch to FF".
	originalBranch := ""
	if out, brErr := exec.Command("git", "-C", repoRoot, "symbolic-ref", "--quiet", "--short", "HEAD").Output(); brErr == nil {
		originalBranch = strings.TrimSpace(string(out))
	}
	originalTip := ""
	if out, tipErr := exec.Command("git", "-C", repoRoot, "rev-parse", "HEAD").Output(); tipErr == nil {
		originalTip = strings.TrimSpace(string(out))
	}

	// `git worktree add <path> HEAD` creates the worktree at the current
	// HEAD commit. Any working-tree state (staged, unstaged, untracked)
	// in the main checkout is intentionally NOT copied — that is the whole
	// point of isolation.
	cmd := exec.Command("git", "-C", repoRoot, "worktree", "add", wtPath, "HEAD")
	if out, addErr := cmd.CombinedOutput(); addErr != nil {
		return worktreeContext{}, nil, fmt.Errorf("git worktree add %s: %w\noutput: %s", wtPath, addErr, string(out))
	}

	if logger != nil {
		logger.Info("runtime: worktree created at %s (base: %s HEAD %s on %s)",
			wtPath, repoRoot, shortSHA(originalTip), branchOrDetached(originalBranch))
	}

	cleanup := func() {
		// `--force` overrides protections; we accept the risk because the
		// engine owns the worktree's lifecycle. Best-effort: errors are
		// logged but do not fail the run.
		out, rmErr := exec.Command("git", "-C", repoRoot, "worktree", "remove", "--force", wtPath).CombinedOutput()
		if rmErr != nil && logger != nil {
			logger.Warn("runtime: git worktree remove %s failed: %v\noutput: %s", wtPath, rmErr, string(out))
		}
	}
	return worktreeContext{
		repoRoot:       repoRoot,
		wtPath:         wtPath,
		originalBranch: originalBranch,
		originalTip:    originalTip,
	}, cleanup, nil
}

// finalizeOptions controls the post-run worktree promotion. All fields
// are optional; sensible defaults apply when empty.
type finalizeOptions struct {
	// runName is the deterministic friendly label (e.g. "swift-cedar-a3f2")
	// used when branchName is empty. Falls back to runID if also empty.
	runName string
	runID   string
	// branchName, when non-empty, overrides the default
	// `iterion/run/<runName>` storage branch. Useful for landing each
	// run on a stable name (e.g. `feat/auto-fixes`).
	branchName string
	// mergeInto controls the best-effort FF target:
	//   ""        → fast-forward the originalBranch (default)
	//   "none"    → skip the FF entirely
	//   "current" → same as default; explicit form
	//   <branch>  → fast-forward this named branch instead of originalBranch
	mergeInto string
}

// finalizeResult captures what the post-run promotion actually did so
// the engine can persist it to run.json and the editor can surface it.
type finalizeResult struct {
	// FinalCommit is the SHA the worktree's HEAD pointed to at end of
	// run. Empty when the run produced no commits (HEAD unchanged).
	FinalCommit string
	// FinalBranch is the persistent branch created on FinalCommit.
	// Empty when no commits were produced (no branch needed).
	FinalBranch string
	// MergedInto is the branch the engine fast-forwarded to FinalCommit.
	// Empty when the FF was skipped, opted out, or failed.
	MergedInto string
}

// finalizeWorktree promotes the worktree's HEAD onto a persistent
// branch and best-effort fast-forwards the requested merge target.
// Always best-effort: any failure is logged but does not fail the run.
func finalizeWorktree(wc worktreeContext, opts finalizeOptions, logger *iterlog.Logger) finalizeResult {
	res := finalizeResult{}

	// 1. Read the worktree's current HEAD.
	finalSHA := readHEAD(wc.wtPath)
	if finalSHA == "" {
		// Couldn't read HEAD — log and bail. The cleanup runs anyway.
		if logger != nil {
			logger.Warn("runtime: finalize: cannot read worktree HEAD at %s — skipping promotion", wc.wtPath)
		}
		return res
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
		return res
	}
	res.FinalBranch = finalName
	if logger != nil {
		logger.Info("runtime: finalize: created branch %s → %s", finalName, shortSHA(finalSHA))
	}

	// 5. Decide the FF target branch.
	target := resolveMergeTarget(opts.mergeInto, wc.originalBranch)
	if target == "" {
		// Explicit opt-out, or no candidate (detached HEAD at start with
		// no override). Branch alone is the result.
		if logger != nil {
			logger.Info("runtime: finalize: skipping fast-forward (target empty); inspect %s and `git merge` when ready",
				finalName)
		}
		return res
	}

	// 6. Run the guards + the FF itself.
	if mergeErr := tryFastForward(wc.repoRoot, target, finalName, finalSHA, wc.originalBranch, logger); mergeErr != nil {
		if logger != nil {
			logger.Warn("runtime: finalize: fast-forward of %s skipped: %v — `git merge %s` to bring it in",
				target, mergeErr, finalName)
		}
		return res
	}
	res.MergedInto = target
	if logger != nil {
		logger.Info("runtime: finalize: fast-forwarded %s → %s", target, shortSHA(finalSHA))
	}
	return res
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
		out, err := exec.Command("git", "-C", repoRoot, "branch", candidate, sha).CombinedOutput()
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
	// Guard 1: the user's currently-checked-out branch must still be
	// originalBranch. If they switched mid-run, leave their state alone.
	currentBranch := ""
	if out, err := exec.Command("git", "-C", repoRoot, "symbolic-ref", "--quiet", "--short", "HEAD").Output(); err == nil {
		currentBranch = strings.TrimSpace(string(out))
	}
	if originalBranch != "" && currentBranch != originalBranch {
		return fmt.Errorf("checked-out branch changed from %q to %q since start", originalBranch, currentBranch)
	}

	// When the FF target is the currently-checked-out branch, we touch
	// the working tree → require it to be clean. When the target is a
	// different branch, we update its ref out-of-band via fetch-style
	// — but that path is more invasive than the user asked for, so we
	// skip non-current targets entirely with a clear error.
	if target != currentBranch {
		return fmt.Errorf("FF of %q skipped: only the currently-checked-out branch (%q) is supported", target, currentBranch)
	}

	// Guard 2: working tree must be clean.
	if out, err := exec.Command("git", "-C", repoRoot, "status", "--porcelain").Output(); err == nil {
		if len(strings.TrimSpace(string(out))) > 0 {
			return fmt.Errorf("main working tree has uncommitted changes")
		}
	}

	// Guard 3: FF must actually be possible (target is ancestor of finalSHA).
	if err := exec.Command("git", "-C", repoRoot, "merge-base", "--is-ancestor", "refs/heads/"+target, finalSHA).Run(); err != nil {
		return fmt.Errorf("non-fast-forward (%q has commits not in run output)", target)
	}

	// Run the merge.
	out, err := exec.Command("git", "-C", repoRoot, "merge", "--ff-only", branchToMerge).CombinedOutput()
	if err != nil {
		return fmt.Errorf("git merge --ff-only failed: %v\noutput: %s", err, string(out))
	}
	return nil
}

// readHEAD returns the SHA of HEAD in the given worktree, or "" on error.
func readHEAD(wtPath string) string {
	out, err := exec.Command("git", "-C", wtPath, "rev-parse", "HEAD").Output()
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

// findGitRoot walks up parent directories from `dir` until it finds a `.git`
// entry (file or directory). Falls back to os.Getwd() when `dir` is empty.
func findGitRoot(dir string) (string, error) {
	if dir == "" {
		var err error
		dir, err = os.Getwd()
		if err != nil {
			return "", fmt.Errorf("getwd: %w", err)
		}
	}
	current, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("abs path: %w", err)
	}
	for {
		if _, statErr := os.Stat(filepath.Join(current, ".git")); statErr == nil {
			return current, nil
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", fmt.Errorf("not a git repository (or any parent up to /): %s", dir)
		}
		current = parent
	}
}
