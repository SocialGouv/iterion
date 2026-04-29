package context

import (
	"fmt"
	"os/exec"
	"strings"
)

// GitCommitEntry represents a single git commit.
type GitCommitEntry struct {
	Hash    string // abbreviated commit hash
	Subject string // commit subject line
}

// GitWorktreeInfo holds extended git worktree information.
type GitWorktreeInfo struct {
	Branch          string           // current branch name (empty if detached HEAD)
	SHA             string           // current HEAD commit SHA
	IsDetached      bool             // true if HEAD is detached
	Modified        int              // number of modified/staged files
	Untracked       int              // number of untracked files
	Ahead           int              // commits ahead of upstream
	Behind          int              // commits behind upstream
	RecentCommits   []GitCommitEntry // most recent commits
	StagedFiles     []string         // files in the staging area
	StatusPorcelain string           // raw porcelain output, cached to avoid duplicate calls
}

// CollectWorktreeInfo gathers detailed git worktree information for workDir.
// Returns nil if workDir is not a git repo or git is unavailable.
func CollectWorktreeInfo(workDir string) *GitWorktreeInfo {
	// Gate: check if this is a git repo.
	check := runGit(workDir, "rev-parse", "--is-inside-work-tree")
	if check != "true" {
		return nil
	}

	info := &GitWorktreeInfo{}

	// Branch name.
	info.Branch = runGit(workDir, "rev-parse", "--abbrev-ref", "HEAD")
	if info.Branch == "HEAD" {
		info.IsDetached = true
		info.Branch = ""
	}

	// Current SHA.
	info.SHA = runGit(workDir, "rev-parse", "HEAD")

	// Status counts.
	status := runGit(workDir, "--no-optional-locks", "status", "--porcelain")
	info.StatusPorcelain = status
	if status != "" {
		for _, line := range strings.Split(status, "\n") {
			if strings.TrimSpace(line) == "" {
				continue
			}
			if strings.HasPrefix(line, "??") {
				info.Untracked++
			} else {
				info.Modified++
			}
		}
	}

	// Staged files.
	staged := runGit(workDir, "--no-optional-locks", "diff", "--cached", "--name-only")
	if staged != "" {
		info.StagedFiles = strings.Split(staged, "\n")
	}

	// Ahead/behind upstream.
	revList := runGit(workDir, "rev-list", "--left-right", "--count", "HEAD...@{upstream}")
	if revList != "" {
		parts := strings.Fields(revList)
		if len(parts) == 2 {
			fmt.Sscanf(parts[0], "%d", &info.Ahead)
			fmt.Sscanf(parts[1], "%d", &info.Behind)
		}
	}

	// Recent commits.
	info.RecentCommits = RecentCommits(workDir, 5)

	return info
}

// RecentCommits returns the most recent count commits from workDir.
func RecentCommits(workDir string, count int) []GitCommitEntry {
	log := runGit(workDir, "--no-optional-locks", "log", fmt.Sprintf("--format=%%h %%s"), fmt.Sprintf("-n"), fmt.Sprintf("%d", count), "--no-decorate")
	if log == "" {
		return nil
	}

	var entries []GitCommitEntry
	for _, line := range strings.Split(log, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, " ", 2)
		entry := GitCommitEntry{Hash: parts[0]}
		if len(parts) > 1 {
			entry.Subject = parts[1]
		}
		entries = append(entries, entry)
	}
	return entries
}

// GitStatus collects a brief git status string for workDir.
// Returns empty string if workDir is not a git repo or git is unavailable.
func GitStatus(workDir string) string {
	info := CollectWorktreeInfo(workDir)
	if info == nil {
		return ""
	}

	var sb strings.Builder

	branch := info.Branch
	if info.IsDetached {
		branch = "(detached HEAD)"
	}
	fmt.Fprintf(&sb, "Current branch: %s\n", branch)

	if info.Modified > 0 || info.Untracked > 0 {
		fmt.Fprintf(&sb, "Modified: %d, Untracked: %d\n", info.Modified, info.Untracked)

		status := info.StatusPorcelain
		if len(status) > 500 {
			status = status[:500] + "..."
		}
		fmt.Fprintf(&sb, "\nStatus:\n%s", status)
	} else {
		sb.WriteString("Working tree clean\n")
	}

	if len(info.RecentCommits) > 0 {
		sb.WriteString("\nRecent commits:\n")
		for _, c := range info.RecentCommits {
			fmt.Fprintf(&sb, "%s %s\n", c.Hash, c.Subject)
		}
	}

	return sb.String()
}

func runGit(workDir string, args ...string) string {
	cmd := exec.Command("git", args...)
	cmd.Dir = workDir
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
