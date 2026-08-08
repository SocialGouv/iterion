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
//	<store>/workspace-objects/<aa>/<rest>   file contents, deduped by hash
//	<store>/runs/<run>/workspace/
//	  snapshots/<id>.json                  manifest: parent + path→hash entries
//	  index.json                           stat cache + labels + head
//
// The object pool is store-GLOBAL (content is content; a per-run pool
// re-stores the whole workspace for every run), which is why pruning a
// run sweeps against the manifests that remain rather than deleting its
// objects outright.
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
	"strconv"
	"strings"
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
	// PhaseFail labels the workspace a node left behind when its execution
	// did NOT complete — a failure, an interruption, an operator cancel.
	//
	// It is deliberately not PhasePost: two consumers read `post:<node>:<n>`
	// as "the node COMPLETED iteration n" — the rewind's staleness guard
	// (runview.findPreNodeSnapshot) and the review panel's per-node
	// attribution — and a failed node completed nothing. But the files it
	// wrote before dying are real, and without a boundary recording them
	// nothing downstream can tell them apart from the operator's own work:
	// `pre:<node>` is an Alias that does not advance the chain head, so a
	// run that stops inside a node ends with its newest boundary being the
	// state that node STARTED from.
	PhaseFail = "fail"
	// PhasePause labels the workspace a node left behind when it PARKED —
	// a human gate, an ask_user question, a recovery pause.
	//
	// Same reason as PhaseFail, and a separate phase because a pause is
	// not a failure: the node is expected to resume and finish. It also
	// opens the one interval in which NOTHING of the run is executing,
	// which is the only window where a change to the workspace
	// demonstrably did not come from this run — so a scoped restore can
	// exclude it rather than claim authorship of the operator's editor.
	PhasePause = "pause"
)

// PauseLabelPrefix is the prefix of a pause boundary's label. Exported
// for the one consumer that must treat the interval a pause OPENS
// differently from an execution interval (runview's scope computation).
const PauseLabelPrefix = PhasePause + ":"

// BoundaryPhases are the phases the ENGINE writes as it executes. A label
// carrying one of these marks a state the run itself produced, as opposed
// to the banks a rewind takes on the operator's behalf
// ("rewind-backup:…", "pre-restore:…").
//
// Exported because the discriminator has to be shared: runview resolves
// "the run's own most recent boundary" from it, and a hand-written prefix
// test there would drift from what the engine writes here — and match
// `pre-restore:` by accident, since it also begins with "pre".
var BoundaryPhases = []string{PhasePre, PhasePost, PhaseFail, PhasePause}

// IsBoundaryLabel reports whether a label was written by the engine at a
// node or gate boundary, rather than by a rewind banking state.
func IsBoundaryLabel(label string) bool {
	for _, p := range BoundaryPhases {
		if strings.HasPrefix(label, p+":") {
			return true
		}
	}
	_, ok := ParseGateLabel(label)
	return ok
}

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

// GateLabel names the workspace state at a human gate. Parallel to
// store.ReviewGateRef for the git/worktree path: what a reviewer
// approves is everything changed since the previous gate, and numbering
// makes "the previous one" a lookup rather than event archaeology.
//
//	gate:0 ─── work ─── gate:1
//	  ▲ base of the first range     ▲ second reviewer's range base
func GateLabel(seq int) string {
	return fmt.Sprintf("gate:%d", seq)
}

// ParseGateLabel extracts the sequence number from a GateLabel. ok is
// false when the string is not a gate label.
func ParseGateLabel(label string) (seq int, ok bool) {
	const prefix = "gate:"
	if !strings.HasPrefix(label, prefix) {
		return 0, false
	}
	n, err := strconv.Atoi(label[len(prefix):])
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

// ErrSnapshotNotFound is returned when a snapshot id or label is unknown.
var ErrSnapshotNotFound = errors.New("workspacetrack: snapshot not found")

// ErrWorkspaceTooLarge is returned when a workspace exceeds MaxFiles.
//
// It latches for the run: the first overflow records the verdict in the
// run's index and every later Capture returns this immediately instead of
// re-walking. Without the latch the abort happened from inside the walk,
// BEFORE the stat cache was saved, so the next boundary started from an
// empty cache and re-read and re-hashed up to MaxFiles files again — a
// full-content read of 50,000 files per node boundary, producing zero
// snapshots, surfaced only as a warn.
var ErrWorkspaceTooLarge = errors.New("workspacetrack: workspace is too large to version")

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
	// WrittenPaths / DeletedPaths name what the restore actually changed
	// on disk, capped at ReportPathCap entries each (the counts above stay
	// exact).
	//
	// They are the only honest basis for a "here is what I just took from
	// you" report. Deriving that warning from a snapshot diff instead
	// misses every path whose on-disk content differed from BOTH sides of
	// the range — above all an operator edit made while the run was
	// paused, which the next node boundary captures as if the run had
	// produced it.
	WrittenPaths []string `json:"written_paths,omitempty"`
	DeletedPaths []string `json:"deleted_paths,omitempty"`
}

// ReportPathCap bounds the path lists a RestoreReport carries. The struct
// is returned verbatim by the HTTP rewind endpoint and printed by the
// CLI, so an uncapped list is a full workspace listing on both surfaces.
const ReportPathCap = 100

func appendCapped(dst []string, p string) []string {
	if len(dst) >= ReportPathCap {
		return dst
	}
	return append(dst, p)
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
	// RestoreOnly is Restore narrowed to a set of workspace-relative
	// paths: everything outside `only` is neither read, rewritten nor
	// removed, and never appears in the report.
	//
	// This is what makes a restore safe on a workspace iterion does not
	// own. The default run shape has no isolated worktree, so the
	// workspace IS the operator's live checkout, and a full-tree restore
	// there reverts every file that moved since the snapshot — including
	// files no node of the run ever wrote, and including the edit that
	// motivated the rewind. The caller supplies the paths the run is
	// RECORDED to have changed; this keeps the blast radius there.
	//
	// `only` is taken literally, empty included: an empty set restores
	// nothing. It deliberately does NOT overload nil to mean "everything"
	// — a caller that computed an empty scope and one that computed no
	// scope must not silently get opposite blast radii. Ask Restore for
	// the whole snapshot.
	RestoreOnly(runID, workspaceDir, snapshotID string, only []string, protected ...string) (*RestoreReport, error)
	// Forget releases the in-memory stat cache for a run. The cache is a
	// few MiB per run on a real repository and a Tracker outlives every
	// run in a studio process, so a long-lived one must be told when a run
	// is over. Everything it held is re-derivable from index.json.
	Forget(runID string)
	// Changes reports what differs between two snapshots of a run — the
	// comparison half, without which the tracker can restore a node's
	// work but not show it.
	Changes(runID, fromID, toID string) ([]Change, error)
	// Labels returns a copy of the run's label → snapshot-id map (gate:N,
	// pre:/post: node boundaries, …). Used by the review-scope panel to
	// list gate anchors and attribute files to nodes without re-walking
	// the snapshot chain.
	Labels(runID string) map[string]string
	// Object returns the stored contents of a content-addressed hash.
	// Missing content returns an error wrapping ErrSnapshotNotFound.
	Object(hash string) ([]byte, error)
}
