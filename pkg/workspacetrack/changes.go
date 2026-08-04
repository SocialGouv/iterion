package workspacetrack

import (
	"bytes"
	"fmt"
	"os"
	"sort"
)

// ChangeStatus mirrors git's name-status letters so a caller can render
// the two backends — git refs for a worktree run, this tracker for an
// in-place one — through one code path.
type ChangeStatus string

const (
	ChangeAdded    ChangeStatus = "A"
	ChangeModified ChangeStatus = "M"
	ChangeDeleted  ChangeStatus = "D"
)

// Change is one file's difference between two snapshots.
type Change struct {
	Path   string       `json:"path"`
	Status ChangeStatus `json:"status"`
	// OldHash/NewHash are empty on the side where the file is absent.
	OldHash string `json:"old_hash,omitempty"`
	NewHash string `json:"new_hash,omitempty"`
	OldSize int64  `json:"old_size,omitempty"`
	NewSize int64  `json:"new_size,omitempty"`
	// Binary marks content that must not be rendered as text. Decided by
	// sniffing the stored object, since Entry carries no such flag.
	Binary bool `json:"binary,omitempty"`
	// Uncaptured marks a path one of the two snapshots deliberately did
	// not store (oversized). Its diff is unavailable, and saying so is
	// the point: a reviewer must not read its absence as "unchanged".
	Uncaptured bool `json:"uncaptured,omitempty"`
}

// Changes reports what differs between two snapshots of the same run.
//
// This is the comparison half the tracker was missing: it could capture
// and restore, but not answer "what did this node do", which is what a
// per-node file view needs. Both manifests are sorted by path at capture
// time, so this is a merge join — no map, no allocation per entry beyond
// the result.
//
// A path present in both with the same hash is not a change and is
// omitted; identical content is exactly what the content-addressed store
// makes cheap to detect.
func (n *Native) Changes(runID, fromID, toID string) ([]Change, error) {
	from, err := n.Load(runID, fromID)
	if err != nil {
		return nil, fmt.Errorf("workspacetrack: changes: load %s: %w", fromID, err)
	}
	to, err := n.Load(runID, toID)
	if err != nil {
		return nil, fmt.Errorf("workspacetrack: changes: load %s: %w", toID, err)
	}

	fromSkipped, toSkipped := map[string]bool{}, map[string]bool{}
	for _, p := range from.Skipped {
		fromSkipped[p] = true
	}
	for _, p := range to.Skipped {
		toSkipped[p] = true
	}
	fromHas, toHas := map[string]bool{}, map[string]bool{}
	for _, e := range from.Entries {
		fromHas[e.Path] = true
	}
	for _, e := range to.Entries {
		toHas[e.Path] = true
	}

	var out []Change
	i, j := 0, 0
	for i < len(from.Entries) || j < len(to.Entries) {
		switch {
		case j >= len(to.Entries) || (i < len(from.Entries) && from.Entries[i].Path < to.Entries[j].Path):
			e := from.Entries[i]
			out = append(out, Change{Path: e.Path, Status: ChangeDeleted, OldHash: e.Hash, OldSize: e.Size})
			i++
		case i >= len(from.Entries) || from.Entries[i].Path > to.Entries[j].Path:
			e := to.Entries[j]
			out = append(out, Change{Path: e.Path, Status: ChangeAdded, NewHash: e.Hash, NewSize: e.Size})
			j++
		default:
			a, b := from.Entries[i], to.Entries[j]
			if a.Hash != b.Hash {
				out = append(out, Change{
					Path: a.Path, Status: ChangeModified,
					OldHash: a.Hash, NewHash: b.Hash,
					OldSize: a.Size, NewSize: b.Size,
				})
			}
			i++
			j++
		}
	}

	// A path either snapshot skipped has no stored content on that side,
	// so its diff cannot be produced. Flag it rather than let the caller
	// present a missing diff as an empty one.
	for k := range out {
		if fromSkipped[out[k].Path] || toSkipped[out[k].Path] {
			out[k].Uncaptured = true
			continue
		}
		// Path heuristic when building the LIST, byte-level detection only
		// on the per-file diff — the convention StatusBetween already
		// follows. Sniffing here opened every changed object and read 8 KB
		// of each just to render a list the operator has not asked a diff
		// from yet: O(N) opens for a node that ran `go mod vendor`.
		out[k].Binary = looksBinary(out[k].Path)
	}

	// An oversized path is in NEITHER Entries list, so the merge join
	// above never saw it. Left out, a file the node created would be
	// indistinguishable from one it never touched — the exact silence
	// this type exists to avoid. We cannot say what changed inside it,
	// only that it is there and uncovered.
	seen := map[string]bool{}
	for _, c := range out {
		seen[c.Path] = true
	}
	for _, p := range union(fromSkipped, toSkipped) {
		if seen[p] {
			continue
		}
		// Skipped on BOTH sides is a property of the FILE, not of this
		// node: an oversized asset or a permission-denied path is skipped
		// identically in every snapshot of the run. Emitting a change for
		// it made every node's Files tab claim "1 file was too large to
		// version", and — because the panel gates its "this node changed
		// no files" message on the uncaptured list being empty too — a
		// node that genuinely touched nothing never got the honest answer.
		// StatusBetween made the same call deliberately; the two
		// comparison functions must not disagree.
		if fromSkipped[p] && toSkipped[p] {
			continue
		}
		st := ChangeModified // present both sides, content unknown
		switch {
		case !fromSkipped[p] && !fromHas[p]:
			st = ChangeAdded
		case !toSkipped[p] && !toHas[p]:
			st = ChangeDeleted
		}
		out = append(out, Change{Path: p, Status: st, Uncaptured: true})
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Path < out[b].Path })
	return out, nil
}

// union lists every key of two sets, sorted.
func union(a, b map[string]bool) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range []map[string]bool{a, b} {
		for k := range m {
			if !seen[k] {
				seen[k] = true
				out = append(out, k)
			}
		}
	}
	sort.Strings(out)
	return out
}

// binarySniffBytes is how much of a file is examined for a NUL byte.
// git uses the same idea on the first 8000 bytes of a blob; matching it
// keeps the two backends reporting the same verdict for the same file.
const binarySniffBytes = 8000

// isBinary reports whether stored content should be treated as binary.
// Best-effort: unreadable content is not binary, since a wrong "binary"
// hides a diff the operator could have read.
func (n *Native) isBinary(hash string) bool {
	if !validHash(hash) {
		return false
	}
	f, err := os.Open(n.objectPath(hash))
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	buf := make([]byte, binarySniffBytes)
	read, err := f.Read(buf)
	if read <= 0 || (err != nil && read == 0) {
		return false
	}
	return bytes.IndexByte(buf[:read], 0) >= 0
}
