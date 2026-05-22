// Package runtime — conflict handling for the deferred squash merge.
//
// When `git merge --squash` hits a content conflict, the conventional
// "merge fails → reset the worktree" rollback (see trySquashMerge)
// throws away the conflict markers the operator would need to resolve
// them by hand. The conflict-resolver feature flips that: on a conflict
// we leave the worktree in the conflicted state (UU files in the index,
// markers in the worktree files) and surface structured ConflictFile
// records so the studio can render a per-file, per-hunk resolver UI.
//
// This file defines the data shapes, the parser, and the three
// operations the resolver UI needs (stage a resolved file, finalize
// the squash commit, abort the merge).
package runtime

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// ConflictHunk is a single `<<<<<<< … ======= … >>>>>>>` region inside
// a conflicted file. Line numbers are 1-indexed and refer to positions
// in the current on-disk content of the file (markers included).
type ConflictHunk struct {
	// StartLine is the line carrying the `<<<<<<<` marker.
	StartLine int `json:"start_line"`
	// EndLine is the line carrying the matching `>>>>>>>` marker.
	EndLine int `json:"end_line"`
	// OursLabel is the text following `<<<<<<< ` on StartLine (usually
	// "HEAD" or a branch name). Useful for the UI's "Take ours" label.
	OursLabel string `json:"ours_label,omitempty"`
	// TheirsLabel is the text following `>>>>>>> ` on EndLine.
	TheirsLabel string `json:"theirs_label,omitempty"`
	// OursLines is the lines between `<<<<<<<` and the first divider
	// (`|||||||` in diff3 mode, `=======` otherwise). May be empty.
	OursLines []string `json:"ours_lines"`
	// BaseLines is the lines between `|||||||` and `=======` (diff3
	// merge.conflictStyle only). Nil/empty for the default 2-way style.
	BaseLines []string `json:"base_lines,omitempty"`
	// TheirsLines is the lines between `=======` and `>>>>>>>`.
	TheirsLines []string `json:"theirs_lines"`
	// ContextBefore is up to 3 lines immediately preceding StartLine
	// (file order), useful for the LLM resolver prompt.
	ContextBefore []string `json:"context_before,omitempty"`
	// ContextAfter is up to 3 lines immediately following EndLine.
	ContextAfter []string `json:"context_after,omitempty"`
}

// ConflictFile is one path in the worktree that git left in a
// conflicted state. Content is the full on-disk content including
// markers — the studio renders it directly in the editor.
type ConflictFile struct {
	// Path is repo-root-relative (forward slashes).
	Path string `json:"path"`
	// Content is the current worktree content of the file, markers
	// included. Empty when ReadErr is non-empty.
	Content string `json:"content"`
	// Hunks is the list of conflict regions parsed from Content. May
	// be empty when the index has unmerged stages but the worktree
	// file no longer contains markers (rare: operator started a
	// manual resolve and saved without `git add`).
	Hunks []ConflictHunk `json:"hunks"`
	// ReadErr surfaces a file-read error so the UI can show a row
	// per conflicted path even when one is unreadable.
	ReadErr string `json:"read_err,omitempty"`
}

// ConflictDetail is the parsed conflict state of a repo. Empty Files
// means there's nothing to resolve.
type ConflictDetail struct {
	// Files lists each conflicted path, in lexical order so the UI is
	// stable across refreshes.
	Files []ConflictFile `json:"files"`
}

// MergeConflictError wraps the original `git merge --squash` output
// and the list of conflicted paths. Returned by trySquashMerge when
// the failure is a content conflict (callers should NOT roll back the
// worktree). Other merge failures use the plain fmt.Errorf path.
type MergeConflictError struct {
	// Files is the repo-root-relative paths git reported as unmerged.
	Files []string
	// Output is the original combined output from `git merge --squash`,
	// preserved for logs even when the UI uses the parsed Files list.
	Output string
}

func (e *MergeConflictError) Error() string {
	if len(e.Files) == 1 {
		return fmt.Sprintf("merge conflict in %s", e.Files[0])
	}
	return fmt.Sprintf("merge conflict in %d files", len(e.Files))
}

// unmergedPaths returns the repo-root-relative paths git considers
// unmerged (any stage 1/2/3 entry from `git ls-files -u`). Returns nil
// + nil when the merge cleanly completed (no conflicts). Returns nil +
// err on a git failure — callers should treat that as "couldn't tell"
// rather than "no conflicts".
func unmergedPaths(repoRoot string) ([]string, error) {
	out, err := gitCmd("-C", repoRoot, "ls-files", "--unmerged", "-z").Output()
	if err != nil {
		return nil, fmt.Errorf("git ls-files --unmerged: %w", err)
	}
	if len(out) == 0 {
		return nil, nil
	}
	// `ls-files -u -z` emits one NUL-terminated record per stage entry
	// (so a 3-stage conflict produces 3 lines). Format:
	//     <mode> <sha1> <stage>\t<path>\0
	// We de-dup paths because the resolver acts at file granularity.
	seen := map[string]struct{}{}
	var paths []string
	for _, rec := range strings.Split(strings.TrimRight(string(out), "\x00"), "\x00") {
		if rec == "" {
			continue
		}
		tab := strings.IndexByte(rec, '\t')
		if tab < 0 || tab+1 >= len(rec) {
			continue
		}
		path := rec[tab+1:]
		if _, dup := seen[path]; dup {
			continue
		}
		seen[path] = struct{}{}
		paths = append(paths, path)
	}
	return paths, nil
}

// ParseConflicts inspects the worktree at repoRoot and returns the
// current conflict state. The Files list is empty when nothing is
// conflicted — callers should treat that as "no conflicts to resolve"
// rather than an error.
func ParseConflicts(repoRoot string) (ConflictDetail, error) {
	if repoRoot == "" {
		return ConflictDetail{}, fmt.Errorf("repo root required")
	}
	paths, err := unmergedPaths(repoRoot)
	if err != nil {
		return ConflictDetail{}, err
	}
	files := make([]ConflictFile, 0, len(paths))
	for _, p := range paths {
		cf := ConflictFile{Path: p}
		// Read via filepath.Join so OS path separators are correct on
		// Windows; we still emit forward-slash paths on the wire.
		full := filepath.Join(repoRoot, filepath.FromSlash(p))
		raw, readErr := os.ReadFile(full)
		if readErr != nil {
			cf.ReadErr = readErr.Error()
			files = append(files, cf)
			continue
		}
		cf.Content = string(raw)
		cf.Hunks = parseConflictHunks(cf.Content)
		files = append(files, cf)
	}
	return ConflictDetail{Files: files}, nil
}

// parseConflictHunks scans content for `<<<<<<<` / `|||||||` / `=======`
// / `>>>>>>>` markers and returns one ConflictHunk per matched region.
// Robust to:
//   - 2-way (`<<<<<<<`, `=======`, `>>>>>>>`) and diff3
//     (`<<<<<<<`, `|||||||`, `=======`, `>>>>>>>`) conflict styles.
//   - Markers with optional trailing label (`<<<<<<< HEAD`, etc).
//   - Stray markers (e.g. one `<<<<<<<` without a matching closer) are
//     skipped — the caller surfaces the partial state via Files but
//     the hunk list stops at the last complete region.
func parseConflictHunks(content string) []ConflictHunk {
	var hunks []ConflictHunk
	lines := splitLinesPreserve(content)
	state := 0 // 0=outside, 1=in-ours, 2=in-base (diff3), 3=in-theirs
	var cur ConflictHunk
	for i, ln := range lines {
		switch {
		case state == 0 && strings.HasPrefix(ln, "<<<<<<<"):
			cur = ConflictHunk{
				StartLine: i + 1,
				OursLabel: strings.TrimSpace(strings.TrimPrefix(ln, "<<<<<<<")),
			}
			state = 1
		case state == 1 && strings.HasPrefix(ln, "|||||||"):
			state = 2
		case (state == 1 || state == 2) && strings.HasPrefix(ln, "======="):
			state = 3
		case state == 3 && strings.HasPrefix(ln, ">>>>>>>"):
			cur.EndLine = i + 1
			cur.TheirsLabel = strings.TrimSpace(strings.TrimPrefix(ln, ">>>>>>>"))
			cur.ContextBefore = takeContext(lines, cur.StartLine-1, -3)
			cur.ContextAfter = takeContext(lines, cur.EndLine-1, +3)
			hunks = append(hunks, cur)
			cur = ConflictHunk{}
			state = 0
		case state == 1:
			cur.OursLines = append(cur.OursLines, ln)
		case state == 2:
			cur.BaseLines = append(cur.BaseLines, ln)
		case state == 3:
			cur.TheirsLines = append(cur.TheirsLines, ln)
		}
	}
	return hunks
}

// takeContext returns up to abs(n) lines around lines[idx] (excluding
// idx itself). Negative n grabs preceding lines (in original order),
// positive n grabs following lines.
func takeContext(lines []string, idx, n int) []string {
	if n == 0 {
		return nil
	}
	if n < 0 {
		start := idx + n
		if start < 0 {
			start = 0
		}
		if start >= idx {
			return nil
		}
		out := make([]string, idx-start)
		copy(out, lines[start:idx])
		return out
	}
	end := idx + 1 + n
	if end > len(lines) {
		end = len(lines)
	}
	if idx+1 >= end {
		return nil
	}
	out := make([]string, end-(idx+1))
	copy(out, lines[idx+1:end])
	return out
}

// splitLinesPreserve splits content into lines without using
// strings.Split (which loses the empty-trailing-newline distinction).
// We use bufio.Scanner with the default SplitLines, which strips the
// terminator — that's fine because the parser only inspects content
// per-line and doesn't reassemble the file.
func splitLinesPreserve(content string) []string {
	var lines []string
	sc := bufio.NewScanner(strings.NewReader(content))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	return lines
}

// StageResolvedFile writes content to <repoRoot>/<path> (overwriting
// the conflicted version) and runs `git add` to mark it resolved in
// the merge index. The path is resolved through filepath.Join so it
// must NOT contain `..` components — caller is expected to have
// validated that path equals one of ParseConflicts' returned Files.
func StageResolvedFile(repoRoot, path, content string) error {
	if repoRoot == "" {
		return fmt.Errorf("repo root required")
	}
	if path == "" {
		return fmt.Errorf("path required")
	}
	// Reject path traversal defensively — the HTTP layer already
	// gates on the path coming from a ParseConflicts result, but a
	// double check here keeps this function safe to call from any
	// future code path.
	if strings.Contains(path, "..") {
		return fmt.Errorf("path traversal in %q", path)
	}
	full := filepath.Join(repoRoot, filepath.FromSlash(path))
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	if out, err := gitCmd("-C", repoRoot, "add", "--", path).CombinedOutput(); err != nil {
		return fmt.Errorf("git add %s: %v\noutput: %s", path, err, string(out))
	}
	return nil
}

// FinalizeConflictMerge commits the squash merge with the given
// message. Caller must ensure every conflicted file has been resolved
// + staged first — otherwise git's own check fails the commit and the
// error is returned verbatim. Returns the new HEAD SHA on success.
func FinalizeConflictMerge(repoRoot, message string) (string, error) {
	if repoRoot == "" {
		return "", fmt.Errorf("repo root required")
	}
	if strings.TrimSpace(message) == "" {
		return "", fmt.Errorf("commit message required")
	}
	// Re-check the unmerged set. If anything is still conflicted git
	// refuses the commit, but we surface a friendlier error than
	// "error: Committing is not possible because you have unmerged
	// files" so the UI can highlight the remaining files.
	if remaining, err := unmergedPaths(repoRoot); err == nil && len(remaining) > 0 {
		return "", fmt.Errorf("still unmerged: %s", strings.Join(remaining, ", "))
	}
	out, err := gitCmd("-C", repoRoot, "commit", "-m", message).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git commit: %v\noutput: %s", err, string(out))
	}
	newHead := readHEAD(repoRoot)
	if newHead == "" {
		return "", fmt.Errorf("commit succeeded but cannot read new HEAD")
	}
	return newHead, nil
}

// AbortConflictMerge discards a partial squash merge by resetting the
// index + working tree to HEAD. Safe to call when no merge is in
// progress (no-op). Caller is responsible for guarding against
// operator-visible data loss (the UI must confirm before invoking).
func AbortConflictMerge(repoRoot string) error {
	if repoRoot == "" {
		return fmt.Errorf("repo root required")
	}
	if out, err := gitCmd("-C", repoRoot, "reset", "--merge").CombinedOutput(); err != nil {
		return fmt.Errorf("git reset --merge: %v\noutput: %s", err, string(out))
	}
	return nil
}

// GitSymbolicRef returns the short name of HEAD's symbolic ref (e.g.
// "main"). Returns "" + nil on detached HEAD; "" + err on a real git
// failure. Exported so the runview service can persist the merge
// target without re-implementing the symbolic-ref dance.
func GitSymbolicRef(repoRoot string) (string, error) {
	if repoRoot == "" {
		return "", fmt.Errorf("repo root required")
	}
	cmd := gitCmd("-C", repoRoot, "symbolic-ref", "--quiet", "--short", "HEAD")
	out, err := cmd.Output()
	if err != nil {
		// Exit code 1 with empty output means detached HEAD — not an
		// error worth surfacing. Anything else is a real git failure.
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			return "", nil
		}
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
