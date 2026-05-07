package git

import (
	"bytes"
	"fmt"
)

// Status returns one entry per modified/untracked file in dir, derived from
// `git status --porcelain=v1 -z`. The NUL-terminated form is used so paths
// containing spaces, newlines, or non-ASCII bytes survive intact.
//
// Renames are reported with both the new path (Path) and the original path
// (OldPath); other entries leave OldPath empty.
//
// Added/Deleted line counts are merged from `git diff --numstat HEAD` for
// tracked changes and from a direct file scan for untracked entries
// (which numstat does not report). A numstat failure degrades silently —
// the panel keeps working with zeroed counts rather than 5xx-ing.
func Status(dir string) ([]FileStatus, error) {
	if !isGitDir(dir) {
		return nil, ErrNotGitRepo
	}

	// `git status` and `git diff --numstat HEAD` are independent — kick
	// numstat off in parallel so its 50–150ms doesn't serialize behind
	// the porcelain status call. A failure on the numstat side is
	// non-fatal; we degrade to zeroed counts for the UI rather than
	// 5xx-ing on what is purely an annotation.
	type nsResult struct {
		stats []NumStat
		err   error
	}
	nsCh := make(chan nsResult, 1)
	go func() {
		stats, err := NumStatHEAD(dir)
		nsCh <- nsResult{stats, err}
	}()

	out, err := run(dir, "status", "--porcelain=v1", "-z", "--untracked-files=all")
	if err != nil {
		<-nsCh
		return nil, err
	}
	files, err := parseStatusZ(out)
	if err != nil {
		<-nsCh
		return nil, err
	}

	if ns := <-nsCh; ns.err == nil {
		applyNumStats(files, ns.stats)
	}
	// Untracked entries don't appear in numstat; scan the file directly
	// for line counts and binary detection.
	for i := range files {
		f := &files[i]
		if f.Status != "??" {
			continue
		}
		added, binary := CountUntrackedLines(dir, f.Path)
		if binary {
			f.Added = -1
			f.Deleted = -1
			f.Binary = true
		} else {
			f.Added = added
		}
	}
	return files, nil
}

// parseStatusZ walks NUL-separated porcelain entries. The format is:
//
//	XY SP path NUL                 (most statuses)
//	XY SP newpath NUL oldpath NUL  (renames/copies — oldpath comes second)
//
// X is the index column, Y the worktree column. We collapse the two into
// one effective status per file, biased toward the worktree (what the
// user actually sees on disk).
func parseStatusZ(out []byte) ([]FileStatus, error) {
	var files []FileStatus
	parts := bytes.Split(out, []byte{0})
	// Last element after a trailing NUL is empty — drop it.
	if len(parts) > 0 && len(parts[len(parts)-1]) == 0 {
		parts = parts[:len(parts)-1]
	}
	for i := 0; i < len(parts); i++ {
		entry := parts[i]
		if len(entry) < 4 {
			return nil, fmt.Errorf("git: malformed status entry %q", string(entry))
		}
		x, y := entry[0], entry[1]
		// Byte 2 is a space separator before the path.
		path := string(entry[3:])
		fs := FileStatus{Path: path, Status: collapseStatus(x, y)}
		// For renames/copies the next NUL-separated entry is the old path.
		if x == 'R' || x == 'C' || y == 'R' || y == 'C' {
			if i+1 >= len(parts) {
				return nil, fmt.Errorf("git: rename entry missing source path: %q", string(entry))
			}
			i++
			fs.OldPath = string(parts[i])
		}
		files = append(files, fs)
	}
	return files, nil
}

// collapseStatus reduces the (index, worktree) pair to a single one-letter
// status. Worktree changes win when both columns are populated because
// that's what the user observes when opening the file. `?` in either
// column means "untracked" (porcelain emits it as ??).
func collapseStatus(x, y byte) string {
	if x == '?' || y == '?' {
		return "??"
	}
	// Worktree column is authoritative for "what the file looks like now".
	if y != ' ' {
		return string(y)
	}
	return string(x)
}
