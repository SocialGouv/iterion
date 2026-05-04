package git

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// StatusBetween lists files that differ between two commits inside repoRoot,
// returning entries shaped like the porcelain Status() output so the editor
// can render them through the same FilesPanel code path.
//
// Used by the modified-files panel after a worktree-using run has finalized
// and its worktree directory has been torn down: the run's commits are still
// reachable on the persistent branch in repoRoot's shared .git, so we can
// recover the diff without the worktree.
//
// baseRef is the commit-ish the run started from (`Run.BaseCommit`); finalRef
// is the run's terminal commit (`Run.FinalCommit`). Both must resolve in
// repoRoot — callers should pass full SHAs for stability across branch moves.
func StatusBetween(repoRoot, baseRef, finalRef string) ([]FileStatus, error) {
	if !isGitDir(repoRoot) {
		return nil, ErrNotGitRepo
	}
	out, err := run(repoRoot, "diff", "--name-status", "-z", baseRef, finalRef)
	if err != nil {
		return nil, err
	}
	return parseDiffNameStatusZ(out)
}

// DiffBetween returns the Before (baseRef) and After (finalRef) blob contents
// for relPath in repoRoot. Either side is nil when the path does not exist
// at that ref (added on After-only, deleted on Before-only). Binary detection
// mirrors Diff(): a NUL byte on either side blanks both contents and sets
// Binary = true so Monaco does not try to render raw bytes.
func DiffBetween(repoRoot, baseRef, finalRef, relPath string) (DiffPayload, error) {
	if !isGitDir(repoRoot) {
		return DiffPayload{}, ErrNotGitRepo
	}
	payload := DiffPayload{Path: relPath}

	beforeOut, beforeErr := showAt(repoRoot, baseRef, relPath)
	switch beforeErr {
	case nil:
		s := string(beforeOut)
		payload.Before = &s
	case errNotInHead:
		// nil Before — path didn't exist at baseRef
	default:
		return DiffPayload{}, beforeErr
	}

	afterOut, afterErr := showAt(repoRoot, finalRef, relPath)
	switch afterErr {
	case nil:
		s := string(afterOut)
		payload.After = &s
	case errNotInHead:
		// nil After — path was deleted at finalRef
	default:
		return DiffPayload{}, afterErr
	}

	if (payload.Before != nil && bytes.IndexByte([]byte(*payload.Before), 0) >= 0) ||
		(payload.After != nil && bytes.IndexByte([]byte(*payload.After), 0) >= 0) {
		payload.Binary = true
		payload.Before = nil
		payload.After = nil
	}

	return payload, nil
}

// FindRepoRoot walks parent directories from startDir until it finds a `.git`
// entry (regular .git directory or a worktree's gitfile pointer), returning
// "" when no parent in the chain qualifies.
//
// Used by the editor server to recover a run's main repo root when the run
// record predates the persisted RepoRoot field — we walk up from the run's
// work_dir (`<repo>/.iterion/worktrees/<id>`) past `.iterion` to the repo
// itself. Falls back gracefully on legacy or migrated runs whose work_dir
// no longer resolves locally (returns "" so callers can try the server CWD).
func FindRepoRoot(startDir string) string {
	if startDir == "" {
		return ""
	}
	current, err := filepath.Abs(startDir)
	if err != nil {
		return ""
	}
	for {
		if _, statErr := os.Stat(filepath.Join(current, ".git")); statErr == nil {
			return current
		}
		parent := filepath.Dir(current)
		if parent == current {
			return ""
		}
		current = parent
	}
}

// MergeBase returns the merge base of refA and refB in repoRoot, or "" + nil
// when the two refs share no common ancestor. Wrapping the failure as a
// nil-string lets callers treat "no baseline available" as a soft outcome
// (skip the historical diff) rather than a hard error.
func MergeBase(repoRoot, refA, refB string) string {
	if !isGitDir(repoRoot) {
		return ""
	}
	out, err := run(repoRoot, "merge-base", refA, refB)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// showAt runs `git show <ref>:<relPath>` and disambiguates "missing at ref"
// (mapped to errNotInHead so callers can render it as a nil side) from
// genuine failures. Used directly by DiffBetween and via showHead for the
// HEAD-only fast path.
func showAt(dir, ref, relPath string) ([]byte, error) {
	cmd := exec.Command("git", "show", ref+":"+relPath)
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err == nil {
		return out, nil
	}
	msg := stderr.String()
	if bytes.Contains([]byte(msg), []byte("does not exist")) ||
		bytes.Contains([]byte(msg), []byte("exists on disk, but not in")) ||
		bytes.Contains([]byte(msg), []byte("unknown revision")) ||
		bytes.Contains([]byte(msg), []byte("bad revision")) {
		return nil, errNotInHead
	}
	return nil, err
}

// parseDiffNameStatusZ walks NUL-separated `git diff --name-status -z` output.
// Format mirrors porcelain status with a leading status letter:
//
//	X NUL path NUL                    (M, A, D, T)
//	X NUL oldpath NUL newpath NUL     (R, C — score suffix on X dropped)
func parseDiffNameStatusZ(out []byte) ([]FileStatus, error) {
	var files []FileStatus
	parts := bytes.Split(out, []byte{0})
	if len(parts) > 0 && len(parts[len(parts)-1]) == 0 {
		parts = parts[:len(parts)-1]
	}
	for i := 0; i < len(parts); i++ {
		entry := parts[i]
		if len(entry) == 0 {
			continue
		}
		// Status code is the first byte; rename/copy entries carry a
		// similarity score (R100, C75 …) we strip down to the letter.
		statusByte := entry[0]
		switch statusByte {
		case 'R', 'C':
			// next two parts are oldpath, newpath
			if i+2 >= len(parts) {
				return nil, fmt.Errorf("git: rename diff entry missing paths: %q", string(entry))
			}
			old := string(parts[i+1])
			newp := string(parts[i+2])
			files = append(files, FileStatus{Path: newp, Status: string(statusByte), OldPath: old})
			i += 2
		default:
			if i+1 >= len(parts) {
				return nil, fmt.Errorf("git: diff entry missing path: %q", string(entry))
			}
			path := string(parts[i+1])
			files = append(files, FileStatus{Path: path, Status: string(statusByte)})
			i++
		}
	}
	return files, nil
}
