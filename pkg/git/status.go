package git

import (
	"fmt"
	"strings"
	"time"
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

	// Bound the wait on numstat: if `git diff --numstat` hangs (e.g. a
	// stuck index.lock), proceed with zeroed counts rather than blocking
	// the status call forever. nsCh is buffered(1) so the goroutine still
	// completes and never leaks.
	select {
	case ns := <-nsCh:
		if ns.err == nil {
			applyNumStats(files, ns.stats)
		}
	case <-time.After(3 * time.Second):
		// numstat is purely an annotation; degrade to zeroed counts.
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

// StatusRecord is one record of `git status --porcelain=v1 -z` output — the
// NUL-terminated form, where every path is printed raw: no C-quoting, no
// rename arrow, so a name holding " -> ", a newline or a non-ASCII byte
// survives intact and no consumer ever cuts a rename at the wrong " -> "
// (#1577).
type StatusRecord struct {
	// Status is the two-column XY code as printed (" M", "R ", "??", "!!").
	Status string
	// Path is the record's path — the destination for a rename/copy.
	Path string
	// Source is the rename/copy origin; empty for any other record.
	Source string
}

// ParseStatusPorcelainZ walks the NUL-separated records of
// `git status --porcelain=v1 -z`. The format is:
//
//	XY SP path NUL                 (most statuses)
//	XY SP newpath NUL oldpath NUL  (renames/copies — the destination first)
//
// This is the ONE reader of git's status porcelain: Status (above),
// pkg/runtime's tree probes and pkg/worktreepool's worktreeStatus all
// consume it, so two packages cannot drift again on what a rename's
// destination is — the class where one parser stepped over a quoted source
// and the other cut at the first " -> " (#1577).
func ParseStatusPorcelainZ(out string) ([]StatusRecord, error) {
	var records []StatusRecord
	parts := strings.Split(out, "\x00")
	// Last element after a trailing NUL is empty — drop it.
	if len(parts) > 0 && parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	for i := 0; i < len(parts); i++ {
		entry := parts[i]
		if len(entry) < 4 {
			return nil, fmt.Errorf("git: malformed status entry %q", entry)
		}
		rec := StatusRecord{Status: entry[:2], Path: entry[3:]}
		// For renames/copies the next NUL-separated record is the old path.
		if x, y := entry[0], entry[1]; x == 'R' || x == 'C' || y == 'R' || y == 'C' {
			if i+1 >= len(parts) {
				return nil, fmt.Errorf("git: rename entry missing source path: %q", entry)
			}
			i++
			rec.Source = parts[i]
		}
		records = append(records, rec)
	}
	return records, nil
}

// parseStatusZ adapts the shared record parser to Status's FileStatus rows,
// collapsing the (index, worktree) pair into one effective status per file,
// biased toward the worktree (what the user actually sees on disk).
func parseStatusZ(out []byte) ([]FileStatus, error) {
	records, err := ParseStatusPorcelainZ(string(out))
	if err != nil {
		return nil, err
	}
	files := make([]FileStatus, 0, len(records))
	for _, rec := range records {
		files = append(files, FileStatus{
			Path:    rec.Path,
			Status:  collapseStatus(rec.Status[0], rec.Status[1]),
			OldPath: rec.Source,
		})
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
