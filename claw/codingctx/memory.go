package codingctx

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const maxMemoryBytes = 20 * 1024 // 20KB

// LoadMemoryFiles discovers and loads CLAUDE.md files, returning concatenated content.
// Searches: ~/.claude/CLAUDE.md, <workDir>/CLAUDE.md, <workDir>/.claude/CLAUDE.md
func LoadMemoryFiles(workDir string) string {
	homeDir, _ := os.UserHomeDir()

	candidates := []struct {
		label string
		path  string
	}{
		{"User global (~/.claude/CLAUDE.md)", filepath.Join(homeDir, ".claude", "CLAUDE.md")},
		{"Project root (CLAUDE.md)", filepath.Join(workDir, "CLAUDE.md")},
		{"Project config (.claude/CLAUDE.md)", filepath.Join(workDir, ".claude", "CLAUDE.md")},
	}

	var parts []string
	totalBytes := 0

	for _, c := range candidates {
		data, err := os.ReadFile(c.path)
		if err != nil {
			continue
		}
		text := string(data)
		remaining := maxMemoryBytes - totalBytes
		if remaining <= 0 {
			break
		}
		if len(text) > remaining {
			text = text[:remaining] + "\n... (truncated)"
		}
		parts = append(parts, fmt.Sprintf("## %s\n\n%s", c.label, text))
		totalBytes += len(text)
	}

	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, "\n\n---\n\n")
}

// MemoryFileMtimes returns a map of path -> mtime nanoseconds for all CLAUDE.md
// candidates that currently exist on disk.
func MemoryFileMtimes(workDir string) map[string]int64 {
	homeDir, _ := os.UserHomeDir()
	paths := []string{
		filepath.Join(homeDir, ".claude", "CLAUDE.md"),
		filepath.Join(workDir, "CLAUDE.md"),
		filepath.Join(workDir, ".claude", "CLAUDE.md"),
	}
	result := make(map[string]int64)
	for _, p := range paths {
		if info, err := os.Stat(p); err == nil {
			result[p] = info.ModTime().UnixNano()
		}
	}
	return result
}
