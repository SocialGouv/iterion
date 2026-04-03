// Package process manages Claude CLI subprocess lifecycle.
package process

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// FindCLI locates the claude binary. It checks:
// 1. Explicit path (if provided)
// 2. PATH lookup
// 3. ~/.claude/local/claude
func FindCLI(explicit string) (string, error) {
	if explicit != "" {
		if _, err := os.Stat(explicit); err == nil {
			return explicit, nil
		}
		return "", &CLINotFoundError{Searched: []string{explicit}}
	}

	// Check PATH
	if path, err := exec.LookPath("claude"); err == nil {
		return path, nil
	}

	// Check ~/.claude/local/
	home, err := os.UserHomeDir()
	if err == nil {
		local := filepath.Join(home, ".claude", "local", "claude")
		if _, err := os.Stat(local); err == nil {
			return local, nil
		}
	}

	return "", &CLINotFoundError{
		Searched: []string{"PATH", "~/.claude/local/claude"},
	}
}

// CLINotFoundError contains the paths that were searched.
type CLINotFoundError struct {
	Searched []string
}

func (e *CLINotFoundError) Error() string {
	return "claude: CLI not found (searched: " + joinPaths(e.Searched) + ")"
}

func joinPaths(paths []string) string {
	return strings.Join(paths, ", ")
}
