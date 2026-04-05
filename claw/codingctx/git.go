package codingctx

import (
	"fmt"
	"os/exec"
	"strings"
)

// GitStatus collects a brief git status string for workDir.
// Returns empty string if workDir is not a git repo or git is unavailable.
func GitStatus(workDir string) string {
	branch := runGit(workDir, "branch", "--show-current")
	if branch == "" {
		return ""
	}

	status := runGit(workDir, "status", "--porcelain")
	log := runGit(workDir, "log", "--oneline", "-n", "5")

	var sb strings.Builder
	fmt.Fprintf(&sb, "Current branch: %s\n", branch)

	if status != "" {
		lines := strings.Split(strings.TrimSpace(status), "\n")
		modified, untracked := 0, 0
		for _, l := range lines {
			if strings.HasPrefix(l, "??") {
				untracked++
			} else if strings.TrimSpace(l) != "" {
				modified++
			}
		}
		fmt.Fprintf(&sb, "Modified: %d, Untracked: %d\n", modified, untracked)
		if len(status) > 500 {
			status = status[:500] + "..."
		}
		fmt.Fprintf(&sb, "\nStatus:\n%s", status)
	} else {
		sb.WriteString("Working tree clean\n")
	}

	if log != "" {
		fmt.Fprintf(&sb, "\nRecent commits:\n%s", log)
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
