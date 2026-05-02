// Package runtime — git worktree helpers for `worktree: auto` workflows.
//
// When a workflow declares `worktree: auto`, the engine creates a fresh
// git worktree at run start (under <store-dir>/worktrees/<run-id>) so the
// run executes in an isolated checkout. This decouples the run's mutations
// from the user's main working tree — WIP stays invisible, the run's
// commits land via the shared .git, and a failed run leaves the worktree
// in place for inspection.
package runtime

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// setupWorktree creates a fresh git worktree at
// <storeRoot>/worktrees/<runID>, checked out at HEAD of the repository
// containing repoHint (typically the engine's workDir before override).
// On success returns the absolute worktree path, a cleanup closure
// (`git worktree remove --force <path>`), and nil error.
func setupWorktree(storeRoot, runID, repoHint string, logger *iterlog.Logger) (string, func(), error) {
	repoRoot, err := findGitRoot(repoHint)
	if err != nil {
		return "", nil, fmt.Errorf("locate git repo: %w", err)
	}

	// Always resolve the worktree path to an absolute one. Tool nodes set
	// cmd.Dir to the worktree path AND substitute it into shell commands
	// like `git -C <path> ...`; if the path is relative, those two layers
	// stack the resolution (Go exec.Command resolves Dir against the parent
	// cwd, then sh resolves the substituted relative path against Dir),
	// landing in a phantom <wt>/<wt> location that doesn't exist.
	wtPath, err := filepath.Abs(filepath.Join(storeRoot, "worktrees", runID))
	if err != nil {
		return "", nil, fmt.Errorf("resolve worktree absolute path: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(wtPath), 0o755); err != nil {
		return "", nil, fmt.Errorf("create worktrees directory: %w", err)
	}

	// `git worktree add <path> HEAD` creates the worktree at the current
	// HEAD commit. Any working-tree state (staged, unstaged, untracked)
	// in the main checkout is intentionally NOT copied — that is the whole
	// point of isolation.
	cmd := exec.Command("git", "-C", repoRoot, "worktree", "add", wtPath, "HEAD")
	if out, addErr := cmd.CombinedOutput(); addErr != nil {
		return "", nil, fmt.Errorf("git worktree add %s: %w\noutput: %s", wtPath, addErr, string(out))
	}

	if logger != nil {
		logger.Info("runtime: worktree created at %s (base: %s HEAD)", wtPath, repoRoot)
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
	return wtPath, cleanup, nil
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
