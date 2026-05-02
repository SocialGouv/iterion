// Package git is a minimal wrapper around the `git` CLI for the editor's
// modified-files panel. It exposes Status (porcelain → typed entries),
// Diff (HEAD ↔ working-tree contents for one path), and a path validator.
//
// All operations shell out to `git`; `dir` must be an absolute path inside
// a git repository (or worktree). Errors that mean "this directory is not
// a git repository" are flattened to ErrNotGitRepo so callers can render
// a friendly message.
package git

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// ErrNotGitRepo is returned by Status/Diff when the target directory is
// not inside a git working tree (no .git, or `git` reports "not a git
// repository"). Callers in the HTTP layer translate it to a 200 with
// `available: false, reason: "not_git_repo"` so the editor can render a
// neutral empty-state instead of a red error.
var ErrNotGitRepo = errors.New("git: not a git repository")

// FileStatus is a single entry in the porcelain output, distilled to one
// effective change per path. The on-disk reality (worktree) wins over the
// index when both columns disagree — the editor cares about "what would I
// see if I opened the file right now" more than the staging state.
type FileStatus struct {
	Path    string `json:"path"`
	Status  string `json:"status"`             // "M" | "A" | "D" | "R" | "??"
	OldPath string `json:"old_path,omitempty"` // populated only when Status == "R"
}

// DiffPayload carries the two sides of a file diff for the Monaco
// DiffEditor. Both fields are nil (omitted in JSON) when the file does
// not exist on that side: `Before == nil` for untracked/added files,
// `After == nil` for deleted files. Binary files set Binary = true and
// leave Before/After nil — the editor swaps in a "binary file not shown"
// message instead of feeding non-text into Monaco.
// Status is intentionally absent: the caller already has it from the
// prior /files listing and feeds it back as UI metadata. Recomputing it
// here would force a second `git status` scan on every diff click.
type DiffPayload struct {
	Path   string  `json:"path"`
	Before *string `json:"before"`
	After  *string `json:"after"`
	Binary bool    `json:"binary"`
}

// run executes `git args...` with cwd=dir and returns combined stdout.
// Stderr is captured separately to surface git's own diagnostics in
// wrapped errors. Any error mentioning "not a git repository" is mapped
// to ErrNotGitRepo so the caller can branch on it cleanly.
func run(dir string, args ...string) ([]byte, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := stderr.String()
		if strings.Contains(msg, "not a git repository") {
			return nil, ErrNotGitRepo
		}
		return nil, fmt.Errorf("git %s: %w (stderr: %s)", strings.Join(args, " "), err, strings.TrimSpace(msg))
	}
	return out, nil
}

// isGitDir is a fast pre-check so callers can return ErrNotGitRepo
// without spawning a process. It accepts both regular checkouts (.git
// is a directory) and linked worktrees (.git is a file pointing to the
// real gitdir under the parent repo's worktrees/).
func isGitDir(dir string) bool {
	info, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil && (info.IsDir() || info.Mode().IsRegular())
}
