package git

import (
	"fmt"
	"strings"
	"time"
)

// CommitInfo is one entry in the log between two refs. Fields mirror what
// the Commits tab needs to render a GitHub-PR-style row: short SHA, the
// subject line, the author display name, and an absolute timestamp the
// frontend formats relatively.
type CommitInfo struct {
	SHA     string    `json:"sha"`
	Short   string    `json:"short"`
	Subject string    `json:"subject"`
	Author  string    `json:"author"`
	Email   string    `json:"email,omitempty"`
	Date    time.Time `json:"date"`
}

// Log returns commits in (base, head] inside repo. base may be empty —
// in which case all commits reachable from head are returned. The caller
// is responsible for passing valid refs (full SHAs preferred for stability).
//
// Errors that mean "this directory is not a git repository" are flattened
// to ErrNotGitRepo so the HTTP layer can render a 200 + available:false
// response, matching the contract of Status / StatusBetween.
func Log(repo, base, head string) ([]CommitInfo, error) {
	if !isGitDir(repo) {
		return nil, ErrNotGitRepo
	}
	rangeArg := head
	if base != "" {
		rangeArg = base + ".." + head
	}
	// TABs separate fields (none of which can contain TAB); --pretty
	// emits one record per line with a trailing NUL so we can split
	// reliably on \n without the subject swallowing newlines.
	format := "%H%x09%h%x09%s%x09%an%x09%ae%x09%aI"
	out, err := run(repo, "log", "--reverse", "--pretty=format:"+format, rangeArg)
	if err != nil {
		return nil, err
	}
	return parseLog(out)
}

// RevParseHead resolves HEAD in repo to a full SHA. Returns ErrNotGitRepo
// when repo is not a git checkout. Used by the commits endpoint to surface
// the live worktree's tip alongside the commit list.
func RevParseHead(repo string) (string, error) {
	if !isGitDir(repo) {
		return "", ErrNotGitRepo
	}
	out, err := run(repo, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func parseLog(raw []byte) ([]CommitInfo, error) {
	if len(raw) == 0 {
		return []CommitInfo{}, nil
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	out := make([]CommitInfo, 0, len(lines))
	for _, line := range lines {
		if line == "" {
			continue
		}
		parts := strings.Split(line, "\t")
		if len(parts) != 6 {
			return nil, fmt.Errorf("git log: malformed entry %q", line)
		}
		ts, tErr := time.Parse(time.RFC3339, parts[5])
		if tErr != nil {
			return nil, fmt.Errorf("git log: parse date %q: %w", parts[5], tErr)
		}
		out = append(out, CommitInfo{
			SHA:     parts[0],
			Short:   parts[1],
			Subject: parts[2],
			Author:  parts[3],
			Email:   parts[4],
			Date:    ts,
		})
	}
	return out, nil
}
