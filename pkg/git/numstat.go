package git

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
)

// NumStat is one entry of `git diff --numstat -z` output. Binary files
// surface with Binary=true and Added=Deleted=-1 (git emits "-/-" for
// them); the FilesPanel renders this as "(binary)".
type NumStat struct {
	Path    string
	OldPath string // populated for renames/copies; new path is in Path
	Added   int    // -1 when Binary
	Deleted int    // -1 when Binary
	Binary  bool
}

// untrackedReadCap bounds CountUntrackedLines to avoid stalling the
// editor on a multi-GB log file accidentally left in the worktree.
// Anything larger is reported as binary so the UI displays "(binary)".
const untrackedReadCap = 5 << 20 // 5 MiB

// applyNumStats writes line counts from stats into files by path
// match. Used by Status (live) and StatusBetween (historical) to merge
// numstat output into the porcelain/diff name-status output. Untracked
// rows have no numstat entry and are left untouched (they're enriched
// separately via CountUntrackedLines).
func applyNumStats(files []FileStatus, stats []NumStat) {
	byPath := make(map[string]NumStat, len(stats))
	for _, s := range stats {
		byPath[s.Path] = s
	}
	for i := range files {
		f := &files[i]
		if ns, ok := byPath[f.Path]; ok {
			f.Added, f.Deleted, f.Binary = ns.Added, ns.Deleted, ns.Binary
		}
	}
}

// NumStatHEAD returns the per-file added/deleted line counts for the
// working tree against HEAD, mirroring the same NUL-separated rename
// shape used by NumStatBetween. Untracked files do not appear here —
// callers merge them in via CountUntrackedLines.
func NumStatHEAD(dir string) ([]NumStat, error) {
	if !isGitDir(dir) {
		return nil, ErrNotGitRepo
	}
	out, err := run(dir, "diff", "--numstat", "-z", "HEAD")
	if err != nil {
		return nil, err
	}
	return parseNumStatZ(out)
}

// NumStatBetween mirrors StatusBetween: line counts for files that
// changed between two commit-ishes inside repoRoot.
func NumStatBetween(repoRoot, baseRef, finalRef string) ([]NumStat, error) {
	if !isGitDir(repoRoot) {
		return nil, ErrNotGitRepo
	}
	out, err := run(repoRoot, "diff", "--numstat", "-z", baseRef, finalRef)
	if err != nil {
		return nil, err
	}
	return parseNumStatZ(out)
}

// parseNumStatZ walks `git diff --numstat -z` output. The format per
// entry is one of:
//
//	added\tdeleted\tpath\0                       (M, A, D, T)
//	added\tdeleted\t\0oldpath\0newpath\0         (R, C — empty path slot
//	                                              followed by old, new)
//
// Binary files set added/deleted to literal "-" characters; we map that
// to (-1, -1, Binary=true) so the wire and the UI can render "(binary)"
// instead of nonsensical zeros.
func parseNumStatZ(out []byte) ([]NumStat, error) {
	parts := bytes.Split(out, []byte{0})
	if len(parts) > 0 && len(parts[len(parts)-1]) == 0 {
		parts = parts[:len(parts)-1]
	}
	var entries []NumStat
	for i := 0; i < len(parts); i++ {
		head := parts[i]
		if len(head) == 0 {
			continue
		}
		fields := bytes.SplitN(head, []byte{'\t'}, 3)
		if len(fields) != 3 {
			return nil, fmt.Errorf("git: malformed numstat entry %q", string(head))
		}
		ns := NumStat{}
		if !parseNumStatCount(fields[0], fields[1], &ns) {
			return nil, fmt.Errorf("git: malformed numstat counts %q\\t%q", string(fields[0]), string(fields[1]))
		}
		if len(fields[2]) == 0 {
			// Rename/copy: next two NUL tokens are old, new.
			if i+2 >= len(parts) {
				return nil, fmt.Errorf("git: numstat rename entry missing paths after %q", string(head))
			}
			ns.OldPath = string(parts[i+1])
			ns.Path = string(parts[i+2])
			i += 2
		} else {
			ns.Path = string(fields[2])
		}
		entries = append(entries, ns)
	}
	return entries, nil
}

// parseNumStatCount fills ns.Added/Deleted/Binary from the two count
// fields. Returns false only on truly malformed input — "-" + "-" is a
// valid binary marker, not an error.
func parseNumStatCount(addedField, deletedField []byte, ns *NumStat) bool {
	dashAdded := bytes.Equal(addedField, []byte{'-'})
	dashDeleted := bytes.Equal(deletedField, []byte{'-'})
	if dashAdded || dashDeleted {
		ns.Binary = true
		ns.Added = -1
		ns.Deleted = -1
		return true
	}
	added, err := strconv.Atoi(string(addedField))
	if err != nil {
		return false
	}
	deleted, err := strconv.Atoi(string(deletedField))
	if err != nil {
		return false
	}
	ns.Added = added
	ns.Deleted = deleted
	return true
}

// CountUntrackedLines reads the file at dir/relPath and returns the
// line count for use as an "Added" value on a `??` status entry. When
// the file appears binary (NUL byte in the first 8 KiB) or larger than
// untrackedReadCap, returns (-1, true) so the caller can mark the row
// as binary in the UI. Read errors degrade silently to (0, false) —
// the panel keeps working with a zero count rather than 5xx-ing on a
// transiently-locked or vanished file.
func CountUntrackedLines(dir, relPath string) (added int, binary bool) {
	abs := filepath.Join(dir, filepath.FromSlash(relPath))
	info, err := os.Stat(abs)
	if err != nil {
		return 0, false
	}
	if info.Size() > untrackedReadCap {
		return -1, true
	}
	f, err := os.Open(abs)
	if err != nil {
		return 0, false
	}
	defer f.Close()

	const sniff = 8 * 1024
	buf := make([]byte, sniff)
	n, _ := io.ReadFull(f, buf)
	if n > 0 && bytes.IndexByte(buf[:n], 0) >= 0 {
		return -1, true
	}
	count := bytes.Count(buf[:n], []byte{'\n'})

	// Drain the rest streaming, only counting newlines so we don't
	// hold the whole file in memory.
	chunk := make([]byte, 32*1024)
	for {
		m, readErr := f.Read(chunk)
		if m > 0 {
			count += bytes.Count(chunk[:m], []byte{'\n'})
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return 0, false
		}
	}
	return count, false
}
