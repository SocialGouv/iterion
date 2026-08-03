// Package workspacetrack versions a run's workspace: it captures what the
// files look like at a point in execution and can put them back.
//
// It exists because the run state iterion already persists — checkpoint
// outputs, artifacts — is not where a node's work actually lives. A docs
// bot writes dozens of files and returns a summary; a coding bot writes
// source and returns a verdict. Replaying such a node without restoring
// its files means replaying it on top of its own previous production,
// which is not the same experiment.
//
// # Why not git
//
// Per-node git snapshots already exist, but they only serve runs that
// declare `worktree: auto`. The DEFAULT is to run in place, and there the
// workspace IS the operator's live checkout: capturing it with git would
// mean `git add -A` on their repository, staging their own uncommitted
// work as a side effect of running a bot. That is not a gap to fill
// later, it is a reason git cannot be the mechanism there.
//
// So the capture is iterion-owned and lives beside the run's other
// durable state, under the store rather than inside the repository:
//
//	<store>/runs/<run>/workspace/
//	  objects/<aa>/<sha256 remainder>   file contents, deduped by hash
//	  snapshots/<id>.json               manifest: parent + path→hash entries
//	  labels.json                       label → snapshot id
//	  index.json                        stat cache (path,size,mtime) → hash
//
// Snapshots carry a Parent, so a run's captures form a chain — the
// node-by-node history of the workspace, readable without git.
//
// # Cost
//
// The expensive part of any such system is not storing blobs, it is
// knowing what changed without re-reading everything. Git solves it with
// its index; this package keeps the equivalent stat cache, so only files
// whose (size, mtime) moved are re-hashed. The first capture of a run
// pays for a full hash of the workspace; later ones pay a directory walk
// plus the changed files.
package workspacetrack

import (
	"errors"
	"fmt"
	"time"
)

// Boundary phases. A node is bracketed by the state it started from and
// the state it left behind.
const (
	// PhasePre labels the workspace as a node is about to execute. This
	// is what a rewind restores.
	PhasePre = "pre"
	// PhasePost labels the workspace a node left behind.
	PhasePost = "post"
)

// Label builds the boundary label for a node execution.
//
// It lives here, not at either call site, because the engine WRITES these
// labels and the rewind READS them from another package. When the format
// was duplicated, appending a suffix on the producing side left every
// test in runtime, runview, workspacetrack and e2e green — the drift
// surfaced only as a "no snapshot recorded" skip at rewind time, which
// reads like a missing feature rather than a bug.
func Label(phase, nodeID string, loopIter int) string {
	return fmt.Sprintf("%s:%s:%d", phase, nodeID, loopIter)
}

// ErrSnapshotNotFound is returned when a snapshot id or label is unknown.
var ErrSnapshotNotFound = errors.New("workspacetrack: snapshot not found")

// Entry is one file in a snapshot.
type Entry struct {
	Path string `json:"path"` // workspace-relative, slash-separated
	Hash string `json:"hash"` // sha256 of the contents, hex
	Mode uint32 `json:"mode"` // permission bits only
	Size int64  `json:"size"`
}

// Snapshot is one captured workspace state.
type Snapshot struct {
	ID     string `json:"id"`
	Parent string `json:"parent,omitempty"`
	// Label is the human-meaningful name this capture was made under
	// (e.g. "pre:implement:0"). Labels are also resolvable independently,
	// so one snapshot can carry several.
	Label     string    `json:"label,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	Entries   []Entry   `json:"entries"`
	// Skipped lists paths deliberately not captured (oversized files).
	// Recorded so a restore can report what it cannot put back, rather
	// than leaving the operator to assume full coverage.
	Skipped []string `json:"skipped,omitempty"`
}

// TotalBytes is the summed size of the captured files.
func (s *Snapshot) TotalBytes() int64 {
	var n int64
	for _, e := range s.Entries {
		n += e.Size
	}
	return n
}

// RestoreReport describes what a restore actually did, so the caller can
// surface it instead of asserting success.
type RestoreReport struct {
	Written   int      `json:"written"`
	Deleted   int      `json:"deleted"`
	Unchanged int      `json:"unchanged"`
	Skipped   []string `json:"skipped,omitempty"`
}

// Tracker captures and restores workspace states for a run.
//
// Implementations must be safe for sequential use by one run; the engine
// only captures on its main execution path (fan-out branches never
// snapshot, since workspace safety admits a single mutating branch).
type Tracker interface {
	// Capture records the workspace as it currently stands, chained onto
	// the run's current head, and returns the resulting snapshot.
	Capture(runID, workspaceDir, label string) (*Snapshot, error)
	// Alias points a label at an already-captured snapshot. Used for the
	// "state before node N" marker, which is by construction identical to
	// the state after node N-1 — nothing touches the workspace in
	// between, so re-walking it would be pure waste.
	Alias(runID, label, snapshotID string) error
	// Resolve returns the snapshot id a label points at.
	Resolve(runID, label string) (string, bool)
	// Head returns the run's most recent snapshot id ("" when none).
	Head(runID string) string
	// Load reads a snapshot by id.
	Load(runID, snapshotID string) (*Snapshot, error)
	// Restore puts the workspace back to a snapshot: files whose contents
	// differ are rewritten, files absent from the snapshot are removed,
	// and ignored paths are left alone.
	//
	// protected paths are neither rewritten nor removed. The caller uses
	// this for files a restore must not touch even though they live in
	// the workspace — above all the workflow source itself, since a
	// rewind is launched precisely to test an edit to it.
	Restore(runID, workspaceDir, snapshotID string, protected ...string) (*RestoreReport, error)
	// Forget releases the in-memory stat cache for a run. The cache is a
	// few MiB per run on a real repository and a Tracker outlives every
	// run in a studio process, so a long-lived one must be told when a run
	// is over. Everything it held is re-derivable from index.json.
	Forget(runID string)
}
