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

// gitCmd wraps exec.Command("git", args...) with LC_ALL=C / LANG=C so
// callers can branch on stderr substrings ("already exists",
// "exists on disk, but not in") without those substrings being
// silently localized into the user's locale (fr_FR, etc).
func gitCmd(args ...string) *exec.Cmd {
	cmd := exec.Command("git", args...)
	cmd.Env = append(os.Environ(), "LC_ALL=C", "LANG=C")
	return cmd
}

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
	if out, brErr := gitCmd("-C", repoRoot, "symbolic-ref", "--quiet", "--short", "HEAD").Output(); brErr == nil {
		originalBranch = strings.TrimSpace(string(out))
	}
	originalTip := ""
	if out, tipErr := gitCmd("-C", repoRoot, "rev-parse", "HEAD").Output(); tipErr == nil {
		originalTip = strings.TrimSpace(string(out))
	}

	// `git worktree add <path> HEAD` creates the worktree at the current
	// HEAD commit. Any working-tree state (staged, unstaged, untracked)
	// in the main checkout is intentionally NOT copied — that is the whole
	// point of isolation.
	cmd := gitCmd("-C", repoRoot, "worktree", "add", wtPath, "HEAD")
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
		out, rmErr := gitCmd("-C", repoRoot, "worktree", "remove", "--force", wtPath).CombinedOutput()
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
// the engine can persist it to run.json and the editor can surface it.
type finalizeResult struct {
	// FinalCommit is the SHA the worktree's HEAD pointed to at end of
	// run. Empty when the run produced no commits (HEAD unchanged).
	FinalCommit string
	// FinalBranch is the persistent branch created on FinalCommit.
	// Empty when no commits were produced (no branch needed).
	FinalBranch string
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

	// 5. Decide the merge target branch.
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

// createBranchSafely creates a branch at sha; on collision, retries with
// a suffix (-1, -2, …) up to 16 times before giving up. Returns the
// final branch name actually created, or "" on total failure.
func createBranchSafely(repoRoot, name, sha string, logger *iterlog.Logger) (bool, string) {
	candidates := []string{name}
	for i := 1; i <= 16; i++ {
		candidates = append(candidates, fmt.Sprintf("%s-%d", name, i))
	}
	for _, candidate := range candidates {
		out, err := gitCmd("-C", repoRoot, "branch", candidate, sha).CombinedOutput()
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
	if err := gitCmd("-C", repoRoot, "merge-base", "--is-ancestor", "refs/heads/"+target, finalSHA).Run(); err != nil {
		return fmt.Errorf("non-fast-forward (%q has commits not in run output)", target)
	}

	out, err := gitCmd("-C", repoRoot, "merge", "--ff-only", branchToMerge).CombinedOutput()
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
	if out, err := gitCmd("-C", repoRoot, "symbolic-ref", "--quiet", "--short", "HEAD").Output(); err == nil {
		currentBranch = strings.TrimSpace(string(out))
	}
	if originalBranch != "" && currentBranch != originalBranch {
		return fmt.Errorf("checked-out branch changed from %q to %q since start", originalBranch, currentBranch)
	}
	if target != currentBranch {
		return fmt.Errorf("%s of %q skipped: only the currently-checked-out branch (%q) is supported", opName, target, currentBranch)
	}
	if out, err := gitCmd("-C", repoRoot, "status", "--porcelain").Output(); err == nil {
		if len(strings.TrimSpace(string(out))) > 0 {
			return fmt.Errorf("main working tree has uncommitted changes")
		}
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
func trySquashMerge(repoRoot, target, branchToMerge, originalBranch, message string, logger *iterlog.Logger) (string, error) {
	if err := guardMergeTarget(repoRoot, target, originalBranch, "squash"); err != nil {
		return "", err
	}

	// Step 1: stage the squashed diff via `git merge --squash`. This
	// updates the index + working tree to match branchToMerge but does
	// NOT create a commit on its own; we follow up with `git commit`.
	if out, err := gitCmd("-C", repoRoot, "merge", "--squash", branchToMerge).CombinedOutput(); err != nil {
		// Best-effort cleanup so we don't leave a half-merged index
		// behind. `git merge --abort` works on a half-applied non-FF
		// merge; for `--squash` we use `git reset --merge` which
		// restores index + working tree in the same shape.
		_ = gitCmd("-C", repoRoot, "reset", "--merge").Run()
		return "", fmt.Errorf("git merge --squash failed: %v\noutput: %s", err, string(out))
	}

	// Step 2: commit the squashed index. --no-edit prevents the editor
	// from being invoked when MERGE_MSG was populated by --squash; -m
	// supplies our aggregated message regardless.
	if out, err := gitCmd("-C", repoRoot, "commit", "-m", message).CombinedOutput(); err != nil {
		// `git commit` exits non-zero with "nothing to commit" if the
		// squash diff was empty (e.g. branch already merged). Treat
		// that as a soft success: nothing changed, target stays put.
		// Anything else is a real failure — reset the index.
		if strings.Contains(string(out), "nothing to commit") || strings.Contains(string(out), "no changes added to commit") {
			return readHEAD(repoRoot), nil
		}
		_ = gitCmd("-C", repoRoot, "reset", "--merge").Run()
		return "", fmt.Errorf("git commit (squash) failed: %v\noutput: %s", err, string(out))
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

// buildSquashMessage assembles the commit message for a squash merge:
// title is the run/workflow label, body lists each squashed commit as
// `- <shortSHA> <subject>` so the per-iteration history remains visible
// even though the commits themselves are collapsed.
func buildSquashMessage(repoRoot, base, head, runName string) string {
	title := strings.TrimSpace(runName)
	if title == "" {
		title = "iterion run"
	}

	var body strings.Builder
	body.WriteString(title)
	body.WriteString("\n\n")

	out, err := gitCmd("-C", repoRoot, "log", "--reverse", "--pretty=format:%h %s", base+".."+head).Output()
	if err != nil {
		body.WriteString("(squashed commits unavailable)\n")
		return body.String()
	}
	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		body.WriteString("- ")
		body.WriteString(line)
		body.WriteString("\n")
	}
	return body.String()
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
	if out, err := gitCmd("-C", req.RepoRoot, "symbolic-ref", "--quiet", "--short", "HEAD").Output(); err == nil {
		currentBranch = strings.TrimSpace(string(out))
	}
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
	out, err := gitCmd("-C", wtPath, "rev-parse", "HEAD").Output()
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
