// Package runview exposes a service-layer view of iterion runs for
// programmatic consumers — the HTTP server and the future "run console"
// UI. It contains the canonical Launch / Resume / Cancel / Snapshot
// implementations that the CLI also delegates to, along with a pure
// reducer that derives a per-execution snapshot from the persisted
// event stream.
package runview

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

// ExecStatus is the lifecycle state of a single execution (one branch ×
// one loop iteration of an IR node).
type ExecStatus string

const (
	ExecStatusRunning  ExecStatus = "running"
	ExecStatusFinished ExecStatus = "finished"
	ExecStatusFailed   ExecStatus = "failed"
	ExecStatusPaused   ExecStatus = "paused_waiting_human"
	ExecStatusSkipped  ExecStatus = "skipped"
)

// MainBranch is the synthetic branch name used when an event carries no
// explicit branch_id (single-threaded execution before any fan-out).
const MainBranch = "main"

// ExecutionState is one rendered row in the dynamic execution graph: a
// concrete invocation of an IR node within a specific branch and loop
// iteration. The same IR node may appear N times across branches and
// loop iterations — each gets its own ExecutionState with a distinct
// ExecutionID.
type ExecutionState struct {
	ExecutionID         string     `json:"execution_id"`
	IRNodeID            string     `json:"ir_node_id"`
	BranchID            string     `json:"branch_id"`
	LoopIteration       int        `json:"loop_iteration"`
	Status              ExecStatus `json:"status"`
	Kind                string     `json:"kind,omitempty"` // node kind (Agent / Judge / Router / ...)
	StartedAt           *time.Time `json:"started_at,omitempty"`
	FinishedAt          *time.Time `json:"finished_at,omitempty"`
	LastArtifactVersion *int       `json:"last_artifact_version,omitempty"`
	CurrentEventSeq     int64      `json:"current_event_seq"`
	Error               string     `json:"error,omitempty"`
	// FirstSeq / LastSeq mark the persisted event range that produced
	// this execution row, allowing clients to scrub directly to the
	// segment of events.jsonl describing this execution.
	FirstSeq int64 `json:"first_seq"`
	LastSeq  int64 `json:"last_seq"`
}

// RunHeader is the run-level metadata embedded in a snapshot.
type RunHeader struct {
	ID string `json:"id"`
	// Name is the deterministic, human-friendly label for the run.
	// Empty for legacy runs persisted before this field existed.
	Name         string                 `json:"name,omitempty"`
	WorkflowName string                 `json:"workflow_name"`
	WorkflowHash string                 `json:"workflow_hash,omitempty"`
	FilePath     string                 `json:"file_path,omitempty"`
	Status       store.RunStatus        `json:"status"`
	Inputs       map[string]interface{} `json:"inputs,omitempty"`
	CreatedAt    time.Time              `json:"created_at"`
	UpdatedAt    time.Time              `json:"updated_at"`
	FinishedAt   *time.Time             `json:"finished_at,omitempty"`
	Error        string                 `json:"error,omitempty"`
	Checkpoint   *store.Checkpoint      `json:"checkpoint,omitempty"`
	// WorkDir is the absolute filesystem path the run executed in
	// (per-run worktree when Worktree is true, otherwise inherited cwd).
	// Empty for runs created before this field was persisted; the editor
	// hides the modified-files panel in that case.
	WorkDir string `json:"work_dir,omitempty"`
	// Worktree is true when WorkDir was created by `worktree: auto`.
	Worktree bool `json:"worktree,omitempty"`
	// Worktree finalization summary (only populated for `worktree:
	// auto` runs that reached a clean exit). The editor uses these to
	// surface the persistent branch and FF status in the run header.
	FinalCommit   string              `json:"final_commit,omitempty"`
	FinalBranch   string              `json:"final_branch,omitempty"`
	MergedInto    string              `json:"merged_into,omitempty"`
	MergedCommit  string              `json:"merged_commit,omitempty"`
	MergeStrategy store.MergeStrategy `json:"merge_strategy,omitempty"`
	MergeStatus   store.MergeStatus   `json:"merge_status,omitempty"`
	AutoMerge     bool                `json:"auto_merge,omitempty"`
	// ActiveDurationMs is the wall-clock the run actually consumed —
	// the sum of run_started/resumed → paused/failed/cancelled/
	// interrupted/finished windows derived from events. Excludes time
	// the run sat paused waiting for human input or sat failed_resumable
	// between a crash and a resume.
	ActiveDurationMs int64 `json:"active_duration_ms"`
	// CurrentRunStart anchors the currently-accruing window. Non-nil
	// while the run is actively executing; nil once it pauses, fails,
	// is cancelled, is interrupted, or finishes. The frontend adds
	// (now - CurrentRunStart) to ActiveDurationMs to drive the live
	// timer and freezes the value once this clears.
	CurrentRunStart *time.Time `json:"current_run_start,omitempty"`
	// QueuePosition is the 1-based position of a queued cloud run on
	// the NATS queue. Populated only while Status == "queued"; absent
	// otherwise. Computed server-side (Mongo aggregation in T-17/T-31).
	// The editor's QueuedBanner uses it to render the "3rd in queue"
	// copy. See cloud-ready plan §F (T-03, T-15, T-31).
	QueuePosition int `json:"queue_position,omitempty"`
}

// RunSnapshot is the structured view returned by GET /api/runs/{id} and
// pushed to WS subscribers on connect. It bundles a RunHeader (slowly-
// changing run-level metadata) with the dynamic ExecutionState rows
// derived by folding the run's events.
type RunSnapshot struct {
	Run        RunHeader        `json:"run"`
	Executions []ExecutionState `json:"executions"`
	LastSeq    int64            `json:"last_seq"`
}

// SnapshotBuilder is a stateful incremental reducer: feed it events in
// sequence order via Apply, and read out the current RunSnapshot via
// Snapshot. The same builder is used for cold reads (replay every event
// from disk) and for live subscribers (replay history then accept new
// events as they arrive).
//
// The reducer is deterministic: BuildSnapshot(run, events) always
// produces the same output for the same input, which lets the frontend
// derive the same per-seq snapshots locally to power the time-travel
// scrubber.
// NoEventsSeq is the sentinel value of RunSnapshot.LastSeq when no
// events have been applied yet. Distinguishing "empty stream" from
// "one event at seq 0" matters for WS catch-up dedup: we must not
// drop the first live event after subscribing to a fresh run.
const NoEventsSeq int64 = -1

type SnapshotBuilder struct {
	header    RunHeader
	execs     map[string]*ExecutionState
	order     []string                  // execution_id in first-seen order; defines snapshot.Executions order
	nodeCount map[string]map[string]int // branch_id → ir_node_id → next iteration index
	lastSeq   int64
}

// NewSnapshotBuilder seeds a builder from the persisted Run metadata.
// Pass run=nil for an empty initial snapshot (e.g. when the WS catch-up
// races run.json creation).
func NewSnapshotBuilder(run *store.Run) *SnapshotBuilder {
	b := &SnapshotBuilder{
		execs:     make(map[string]*ExecutionState),
		nodeCount: make(map[string]map[string]int),
		lastSeq:   NoEventsSeq,
	}
	if run != nil {
		b.header = headerFromRun(run)
	}
	return b
}

// SetRun refreshes the run-level header. Call this when a fresh
// run.json was just persisted (e.g. on terminal events). The
// event-derived timer fields (ActiveDurationMs, CurrentRunStart) are
// preserved across the refresh — run.json carries CreatedAt/FinishedAt
// but not the per-window accumulation, so we keep what events have
// already taught us.
func (b *SnapshotBuilder) SetRun(run *store.Run) {
	if run == nil {
		return
	}
	prevDuration := b.header.ActiveDurationMs
	prevAnchor := b.header.CurrentRunStart
	hadEventDerivedTimer := b.lastSeq != NoEventsSeq
	b.header = headerFromRun(run)
	if hadEventDerivedTimer {
		b.header.ActiveDurationMs = prevDuration
		b.header.CurrentRunStart = prevAnchor
	}
}

// Apply folds a single event into the running snapshot. Events MUST be
// applied in non-decreasing seq order; out-of-order events are ignored
// (the reducer is monotonic — re-applying a stale event would not
// produce a deterministic state).
func (b *SnapshotBuilder) Apply(evt *store.Event) {
	if evt == nil {
		return
	}
	if b.lastSeq != NoEventsSeq && evt.Seq <= b.lastSeq {
		return
	}
	b.lastSeq = evt.Seq

	branch := evt.BranchID
	if branch == "" {
		branch = MainBranch
	}

	switch evt.Type {
	case store.EventNodeStarted:
		b.handleNodeStarted(evt, branch)
	case store.EventNodeFinished:
		b.handleNodeFinished(evt, branch)
	case store.EventArtifactWritten:
		b.handleArtifactWritten(evt, branch)
	case store.EventRunFailed:
		b.handleRunFailed(evt, branch)
	case store.EventHumanInputRequested:
		b.handleHumanInputRequested(evt, branch)
	case store.EventRunPaused:
		b.handleRunPaused(evt)
	case store.EventRunResumed:
		b.handleRunResumed(evt)
	case store.EventRunStarted:
		b.anchorActive(evt.Timestamp)
	case store.EventRunFinished:
		b.accumulateActive(evt.Timestamp)
	case store.EventRunCancelled:
		b.accumulateActive(evt.Timestamp)
	case store.EventRunInterrupted:
		// Server drain — freeze the timer like a pause. The matching
		// resume re-anchors. Without this case the event would fall
		// through to the default branch and erroneously keep the
		// run accruing across a drain → restart gap.
		b.accumulateActive(evt.Timestamp)
	default:
		// Node-scoped informational events (LLM prompts/requests/steps,
		// retries/compactions, tool calls/errors, human answers, budget
		// warnings, recovery/delegate events, etc.) still belong to the
		// currently running execution. Advancing the exec's event window here
		// lets live inspectors read trace/tools/events before the node later
		// finishes, writes an artifact, or pauses.
		b.touchCurrentExec(evt, branch)
	}
}

// Snapshot returns the current snapshot. Callers receive a fresh value
// (the slice is copied); the underlying ExecutionState pointers are
// shared but treated as immutable from the caller's side.
func (b *SnapshotBuilder) Snapshot() *RunSnapshot {
	execs := make([]ExecutionState, 0, len(b.order))
	for _, id := range b.order {
		if e := b.execs[id]; e != nil {
			execs = append(execs, *e)
		}
	}
	return &RunSnapshot{
		Run:        b.header,
		Executions: execs,
		LastSeq:    b.lastSeq,
	}
}

// LastSeq exposes the highest seq applied so far so live subscribers
// can resume cleanly via WS subscribe{from_seq}.
func (b *SnapshotBuilder) LastSeq() int64 { return b.lastSeq }

// ---------------------------------------------------------------------------
// Per-event handlers
// ---------------------------------------------------------------------------

func (b *SnapshotBuilder) touchCurrentExec(evt *store.Event, branch string) {
	if evt.NodeID == "" {
		return
	}
	exec := b.currentExec(branch, evt.NodeID)
	if exec == nil {
		return
	}
	exec.CurrentEventSeq = evt.Seq
	exec.LastSeq = evt.Seq
}

func (b *SnapshotBuilder) handleNodeStarted(evt *store.Event, branch string) {
	if evt.NodeID == "" {
		return
	}
	iter := b.allocIteration(branch, evt.NodeID)
	id := MakeExecutionID(branch, evt.NodeID, iter)
	ts := evt.Timestamp
	exec := &ExecutionState{
		ExecutionID:     id,
		IRNodeID:        evt.NodeID,
		BranchID:        branch,
		LoopIteration:   iter,
		Status:          ExecStatusRunning,
		StartedAt:       &ts,
		CurrentEventSeq: evt.Seq,
		FirstSeq:        evt.Seq,
		LastSeq:         evt.Seq,
	}
	if kind, ok := evt.Data["kind"].(string); ok {
		exec.Kind = kind
	}
	b.execs[id] = exec
	b.order = append(b.order, id)
}

func (b *SnapshotBuilder) handleNodeFinished(evt *store.Event, branch string) {
	exec := b.currentExec(branch, evt.NodeID)
	if exec == nil {
		return
	}
	ts := evt.Timestamp
	exec.FinishedAt = &ts
	exec.CurrentEventSeq = evt.Seq
	exec.LastSeq = evt.Seq
	if exec.Status == ExecStatusRunning {
		exec.Status = ExecStatusFinished
	}
}

func (b *SnapshotBuilder) handleArtifactWritten(evt *store.Event, branch string) {
	exec := b.currentExec(branch, evt.NodeID)
	if exec == nil {
		return
	}
	if v, ok := evt.Data["version"]; ok {
		switch n := v.(type) {
		case int:
			vv := n
			exec.LastArtifactVersion = &vv
		case int64:
			vv := int(n)
			exec.LastArtifactVersion = &vv
		case float64:
			vv := int(n)
			exec.LastArtifactVersion = &vv
		}
	}
	exec.CurrentEventSeq = evt.Seq
	exec.LastSeq = evt.Seq
}

func (b *SnapshotBuilder) handleRunFailed(evt *store.Event, branch string) {
	// Always close the active window — a failure terminates execution
	// regardless of which node id (if any) the event carries.
	b.accumulateActive(evt.Timestamp)
	if evt.NodeID == "" {
		return
	}
	exec := b.currentExec(branch, evt.NodeID)
	if exec == nil {
		return
	}
	ts := evt.Timestamp
	exec.Status = ExecStatusFailed
	if exec.FinishedAt == nil {
		exec.FinishedAt = &ts
	}
	if msg, ok := evt.Data["error"].(string); ok && msg != "" {
		exec.Error = msg
	}
	exec.CurrentEventSeq = evt.Seq
	exec.LastSeq = evt.Seq
}

func (b *SnapshotBuilder) handleHumanInputRequested(evt *store.Event, branch string) {
	exec := b.currentExec(branch, evt.NodeID)
	if exec == nil {
		return
	}
	exec.Status = ExecStatusPaused
	exec.CurrentEventSeq = evt.Seq
	exec.LastSeq = evt.Seq
}

func (b *SnapshotBuilder) handleRunPaused(evt *store.Event) {
	// Close the active timer window. The matching node was already
	// marked paused by handleHumanInputRequested; the run-level
	// status flips via SetRun on the next disk read.
	b.accumulateActive(evt.Timestamp)
}

func (b *SnapshotBuilder) handleRunResumed(evt *store.Event) {
	// Re-anchor the active timer window. Covers both resume-from-pause
	// and resume-from-failed_resumable — neither emits an explicit
	// run_started, so this is the only place the second window opens.
	b.anchorActive(evt.Timestamp)
	// Find the most-recent paused execution and re-mark it running.
	// In practice there is exactly one because resume can only target
	// the checkpoint node, but iterating is cheap and avoids relying
	// on event payload shape.
	for i := len(b.order) - 1; i >= 0; i-- {
		exec := b.execs[b.order[i]]
		if exec == nil {
			continue
		}
		if exec.Status == ExecStatusPaused {
			exec.Status = ExecStatusRunning
			exec.CurrentEventSeq = evt.Seq
			exec.LastSeq = evt.Seq
			return
		}
	}
}

// No-op when no window is open, so it's safe to call on every
// terminal/pause event without tracking prior state.
func (b *SnapshotBuilder) accumulateActive(at time.Time) {
	if b.header.CurrentRunStart == nil {
		return
	}
	if delta := at.Sub(*b.header.CurrentRunStart); delta > 0 {
		b.header.ActiveDurationMs += delta.Milliseconds()
	}
	b.header.CurrentRunStart = nil
}

// If a window is already open (rare race like a missing pause event),
// the prior interval is silently dropped — preferable to double-counting.
func (b *SnapshotBuilder) anchorActive(at time.Time) {
	ts := at
	b.header.CurrentRunStart = &ts
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// allocIteration returns the next loop-iteration index for (branch,
// nodeID) and increments the counter. The first appearance of a node
// in a branch is iteration 0; the second is 1; etc.
func (b *SnapshotBuilder) allocIteration(branch, nodeID string) int {
	if b.nodeCount[branch] == nil {
		b.nodeCount[branch] = make(map[string]int)
	}
	iter := b.nodeCount[branch][nodeID]
	b.nodeCount[branch][nodeID] = iter + 1
	return iter
}

// currentExec returns the most recently started execution of (branch,
// nodeID) — i.e. the highest iteration index. Subsequent events
// (node_finished, artifact_written, run_failed) are attributed there.
func (b *SnapshotBuilder) currentExec(branch, nodeID string) *ExecutionState {
	counts := b.nodeCount[branch]
	if counts == nil {
		return nil
	}
	iter := counts[nodeID] - 1
	if iter < 0 {
		return nil
	}
	id := MakeExecutionID(branch, nodeID, iter)
	return b.execs[id]
}

// MakeExecutionID composes a stable ID from (branch, node, iteration).
// The format is documented in the WS protocol; clients depend on it
// for tab/anchor URLs and for matching events to executions. Empty
// branch is normalised to MainBranch.
func MakeExecutionID(branch, nodeID string, iteration int) string {
	if branch == "" {
		branch = MainBranch
	}
	return fmt.Sprintf("exec:%s:%s:%d", branch, nodeID, iteration)
}

// ParseExecutionID is the inverse of MakeExecutionID. It returns the
// branch, node ID, and iteration. Returns an error if the input is not
// a well-formed exec ID.
func ParseExecutionID(id string) (branch, nodeID string, iteration int, err error) {
	const prefix = "exec:"
	if !strings.HasPrefix(id, prefix) {
		return "", "", 0, fmt.Errorf("runview: not an execution id: %q", id)
	}
	rest := id[len(prefix):]
	// branch and nodeID are arbitrary strings; only the trailing
	// iteration is numeric. Split from the right on the last colon.
	idx := strings.LastIndex(rest, ":")
	if idx < 0 {
		return "", "", 0, fmt.Errorf("runview: malformed execution id: %q", id)
	}
	iterStr := rest[idx+1:]
	left := rest[:idx]
	mid := strings.Index(left, ":")
	if mid < 0 {
		return "", "", 0, fmt.Errorf("runview: malformed execution id: %q", id)
	}
	branch = left[:mid]
	nodeID = left[mid+1:]
	if _, scanErr := fmt.Sscanf(iterStr, "%d", &iteration); scanErr != nil {
		return "", "", 0, fmt.Errorf("runview: malformed iteration in %q: %w", id, scanErr)
	}
	return branch, nodeID, iteration, nil
}

func headerFromRun(r *store.Run) RunHeader {
	h := RunHeader{
		ID:            r.ID,
		Name:          r.Name,
		WorkflowName:  r.WorkflowName,
		WorkflowHash:  r.WorkflowHash,
		FilePath:      r.FilePath,
		Status:        r.Status,
		Inputs:        r.Inputs,
		CreatedAt:     r.CreatedAt,
		UpdatedAt:     r.UpdatedAt,
		FinishedAt:    r.FinishedAt,
		Error:         r.Error,
		Checkpoint:    r.Checkpoint,
		WorkDir:       r.WorkDir,
		Worktree:      r.Worktree,
		FinalCommit:   r.FinalCommit,
		FinalBranch:   r.FinalBranch,
		MergedInto:    r.MergedInto,
		MergedCommit:  r.MergedCommit,
		MergeStrategy: r.MergeStrategy,
		MergeStatus:   r.MergeStatus,
		AutoMerge:     r.AutoMerge,
	}
	// Bootstrap fallback: when the run is already running but the WS
	// catch-up hasn't yet seen the run_started event, anchor on
	// CreatedAt so the live timer starts at 0 instead of staying frozen.
	// Apply() will overwrite this with the real run_started timestamp
	// once the event arrives.
	if r.Status == store.RunStatusRunning {
		ts := r.CreatedAt
		h.CurrentRunStart = &ts
	}
	return h
}

// BuildSnapshot is the cold-read convenience: load run.json + events
// from the store, then fold them into a RunSnapshot. Events are
// streamed via ScanEvents to keep memory bounded for long runs.
func BuildSnapshot(ctx context.Context, s store.RunStore, runID string) (*RunSnapshot, error) {
	run, err := s.LoadRun(ctx, runID)
	if err != nil {
		return nil, err
	}
	b := NewSnapshotBuilder(run)
	if err := s.ScanEvents(ctx, runID, func(evt *store.Event) bool {
		b.Apply(evt)
		return true
	}); err != nil {
		return nil, err
	}
	return b.Snapshot(), nil
}
