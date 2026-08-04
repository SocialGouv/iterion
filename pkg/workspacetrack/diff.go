package workspacetrack

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

// FileChange is one path that differs between two snapshots. Status uses
// the same single-letter codes as git porcelain (A/M/D) so the review
// panel can render either backend through one UI path.
type FileChange struct {
	Path   string `json:"path"`
	Status string `json:"status"` // "A" | "M" | "D"
	// Binary is a best-effort guess from the path extension when no
	// content is loaded; the per-file diff path re-detects from bytes.
	Binary bool `json:"binary,omitempty"`
	// Size is the head-side size for A/M, base-side size for D.
	Size int64 `json:"size,omitempty"`
}

// DiffFile is the before/after payload for one path between two snapshots.
// Mirrors the studio's DiffPayload shape without importing pkg/git.
type DiffFile struct {
	Path      string
	Before    *string
	After     *string
	Binary    bool
	Oversized bool
}

// diffPayloadCap bounds the bytes loaded for either side of a per-file
// diff (matches pkg/git's untrackedReadCap so both backends behave alike).
const diffPayloadCap int64 = 5 << 20 // 5 MiB

// StatusBetween returns every path whose content hash differs between
// base and head. base may be nil (first capture of a run = all files
// appear as additions).
func StatusBetween(base, head *Snapshot) []FileChange {
	if head == nil {
		return nil
	}
	baseMap := map[string]Entry{}
	if base != nil {
		for _, e := range base.Entries {
			baseMap[e.Path] = e
		}
	}
	headMap := make(map[string]Entry, len(head.Entries))
	for _, e := range head.Entries {
		headMap[e.Path] = e
	}

	var out []FileChange
	for path, h := range headMap {
		b, ok := baseMap[path]
		switch {
		case !ok:
			out = append(out, FileChange{
				Path: path, Status: "A", Binary: looksBinary(path), Size: h.Size,
			})
		case b.Hash != h.Hash:
			out = append(out, FileChange{
				Path: path, Status: "M", Binary: looksBinary(path), Size: h.Size,
			})
		}
	}
	for path, b := range baseMap {
		if _, ok := headMap[path]; !ok {
			out = append(out, FileChange{
				Path: path, Status: "D", Binary: looksBinary(path), Size: b.Size,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// DiffBetween loads the before/after contents of relPath from the object
// store, using the entry hashes recorded in base and head. Either side
// is nil when the path is absent from that snapshot.
func DiffBetween(tr Tracker, base, head *Snapshot, relPath string) (DiffFile, error) {
	out := DiffFile{Path: relPath}
	var baseHash, headHash string
	var baseSize, headSize int64
	if base != nil {
		for _, e := range base.Entries {
			if e.Path == relPath {
				baseHash, baseSize = e.Hash, e.Size
				break
			}
		}
	}
	if head != nil {
		for _, e := range head.Entries {
			if e.Path == relPath {
				headHash, headSize = e.Hash, e.Size
				break
			}
		}
	}
	if baseHash == "" && headHash == "" {
		return out, fmt.Errorf("workspacetrack: path %q not in either snapshot", relPath)
	}
	if (baseSize > 0 && baseSize > diffPayloadCap) || (headSize > 0 && headSize > diffPayloadCap) {
		out.Oversized = true
		return out, nil
	}
	if baseHash != "" {
		b, err := tr.Object(baseHash)
		if err != nil {
			return out, err
		}
		if isBinaryContent(b) {
			out.Binary = true
			return out, nil
		}
		s := string(b)
		out.Before = &s
	}
	if headHash != "" {
		b, err := tr.Object(headHash)
		if err != nil {
			return out, err
		}
		if isBinaryContent(b) {
			out.Binary = true
			out.Before = nil
			out.After = nil
			return out, nil
		}
		s := string(b)
		out.After = &s
	}
	return out, nil
}

// Root walks the Parent chain from Head to the earliest snapshot of a
// run — the base of the first review gate when no earlier gate label
// exists.
func Root(tr Tracker, runID string) (string, bool) {
	id := tr.Head(runID)
	if id == "" {
		return "", false
	}
	for {
		snap, err := tr.Load(runID, id)
		if err != nil {
			return "", false
		}
		if snap.Parent == "" {
			return snap.ID, true
		}
		id = snap.Parent
	}
}

// ListGates returns the gate sequence numbers recorded for a run,
// ascending.
func ListGates(tr Tracker, runID string) []int {
	var seqs []int
	for label := range tr.Labels(runID) {
		if n, ok := ParseGateLabel(label); ok {
			seqs = append(seqs, n)
		}
	}
	sort.Ints(seqs)
	return seqs
}

// NextGateSeq returns the next free gate number for a run.
func NextGateSeq(tr Tracker, runID string) int {
	gates := ListGates(tr, runID)
	if len(gates) == 0 {
		return 0
	}
	return gates[len(gates)-1] + 1
}

// looksBinary is a cheap path-only heuristic for the status list (no
// content load). The per-file diff re-detects from bytes.
func looksBinary(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".png", ".jpg", ".jpeg", ".gif", ".webp", ".ico", ".bmp",
		".mp3", ".wav", ".ogg", ".flac", ".m4a",
		".mp4", ".webm", ".mov", ".avi", ".mkv",
		".pdf", ".zip", ".gz", ".tgz", ".bz2", ".xz",
		".woff", ".woff2", ".ttf", ".otf", ".eot",
		".bin", ".exe", ".dll", ".so", ".dylib",
		".pyc", ".whl", ".jar":
		return true
	default:
		return false
	}
}

func isBinaryContent(b []byte) bool {
	// Same NUL-byte rule git's diff path uses: any NUL in the first 8 KiB
	// (or whole buffer if shorter) marks the payload binary.
	n := len(b)
	if n > 8192 {
		n = 8192
	}
	for i := 0; i < n; i++ {
		if b[i] == 0 {
			return true
		}
	}
	return false
}
