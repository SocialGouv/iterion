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
	"os"
	"strconv"
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
	Name         string `json:"name,omitempty"`
	WorkflowName string `json:"workflow_name"`
	WorkflowHash string `json:"workflow_hash,omitempty"`
	FilePath     string `json:"file_path,omitempty"`
	// BundleName mirrors the .botz manifest's `name` field captured
	// at launch (e.g. "feature-dev"). The studio RunHeader pairs it
	// with WorkflowName so the operator can distinguish bundles whose
	// internal workflow name was customised. Empty for plain .bot
	// runs.
	BundleName string `json:"bundle_name,omitempty"`
	// BundleDisplayName mirrors the manifest's `display_name` (e.g.
	// "Nexie"). When set, the studio RunHeader leads the bot chip
	// with this persona name + a ✨ icon so dispatcher-spawned runs
	// belonging to a named bot read at a glance.
	BundleDisplayName string          `json:"bundle_display_name,omitempty"`
	Status            store.RunStatus `json:"status"`
	Inputs            map[string]any  `json:"inputs,omitempty"`
	// PermissionMode is the workflow-declared tool-permission gate mode
	// ("off"|"ask"|"deny"); empty when the gate is off/unset. The studio
	// badges ask/deny. See docs/permissions.md.
	PermissionMode string `json:"permission_mode,omitempty"`
	// ModelOverrides are the launch-time per-node/-group model/backend
	// pins captured on the run, surfaced so the studio Overview can show
	// what the run was launched with. Empty when none were set.
	ModelOverrides []store.RunModelOverride `json:"model_overrides,omitempty"`
	// NodesServed is the last (backend, model) that served each LLM
	// node, copied from run.json so a snapshot is self-describing
	// without replaying events. Distinct from BackendsUsed, which
	// aggregates unique pairs from node_finished events. Empty for
	// legacy runs and tool/compute-only workflows.
	NodesServed map[string]store.NodeServed `json:"nodes_served,omitempty"`
	// Budget is the effective budget cap set captured at launch (after
	// overrides + cloud ceiling clamp), surfaced so the studio Overview
	// draws budget meters with a denominator. Nil when the workflow
	// declared no budget: block. See store.RunBudget.
	Budget     *store.RunBudget  `json:"budget,omitempty"`
	CreatedAt  time.Time         `json:"created_at"`
	UpdatedAt  time.Time         `json:"updated_at"`
	FinishedAt *time.Time        `json:"finished_at,omitempty"`
	Error      string            `json:"error,omitempty"`
	Checkpoint *store.Checkpoint `json:"checkpoint,omitempty"`
	// WorkDir is the absolute filesystem path the run executed in
	// (per-run worktree when Worktree is true, otherwise inherited cwd).
	// Empty for runs created before this field was persisted; the studio
	// hides the modified-files panel in that case.
	WorkDir string `json:"work_dir,omitempty"`
	// ProjectPath is the stable forge slug ("group/project") the run
	// targets — the cloud run's repo identity (WorkDir is a runner-pod
	// path there). Empty for local and repo-less runs.
	ProjectPath string `json:"project_path,omitempty"`
	// Worktree is true when WorkDir was created by `worktree: auto`.
	Worktree bool `json:"worktree,omitempty"`
	// WorktreeAvailable is true when WorkDir still exists on THIS server's
	// filesystem — i.e. the inline file-editor + uncommitted/live diff
	// surfaces can be served without a 409. It is false for a cloud run
	// (whose worktree lives on the runner pod) and for a finalized/gc'd
	// local run (whose worktree was torn down). The studio gates the
	// Monaco file-editor affordances on it so the operator never clicks an
	// Edit button that then 409s. Mirrors the /files/content endpoint's own
	// gate (resolveRunWorktreePath).
	WorktreeAvailable bool `json:"worktree_available"`
	// Worktree finalization summary (only populated for `worktree:
	// auto` runs that reached a clean exit). The studio uses these to
	// surface the persistent branch and FF status in the run header.
	FinalCommit      string              `json:"final_commit,omitempty"`
	FinalBranch      string              `json:"final_branch,omitempty"`
	FinalBranchError string              `json:"final_branch_error,omitempty"`
	MergedInto       string              `json:"merged_into,omitempty"`
	MergedCommit     string              `json:"merged_commit,omitempty"`
	MergeStrategy    store.MergeStrategy `json:"merge_strategy,omitempty"`
	MergeStatus      store.MergeStatus   `json:"merge_status,omitempty"`
	AutoMerge        bool                `json:"auto_merge,omitempty"`
	// LocAdded / LocDeleted aggregate the three-dot numstat of the
	// run's commits against its fork point (merge-base of the merge
	// target and FinalCommit), computed server-side with a cache.
	// POINTERS on purpose: nil renders "—" (refs unresolvable — branch
	// deleted, commit GC'd), a zero literal renders "0" (diff resolved
	// and empty). Absent for runs without a FinalCommit.
	LocAdded   *int `json:"loc_added,omitempty"`
	LocDeleted *int `json:"loc_deleted,omitempty"`
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
	// The studio's QueuedBanner uses it to render the "3rd in queue"
	// copy. See cloud-ready plan §F (T-03, T-15, T-31).
	QueuePosition int `json:"queue_position,omitempty"`
	// Source records who originated the run (dispatcher → issue back-
	// reference today; CLI / studio launches leave it nil). The studio
	// RunHeader reads it to render a link back to the kanban ticket
	// that triggered the dispatch.
	Source *store.RunSource `json:"source,omitempty"`
	// Run-tree shard tuple (T4b, refs #125): the child←parent edge plus
	// the shard coordinates mirrored from the queue message. ParentRunID
	// points UP at the run that spawned this shard/child; ShardIndex /
	// ShardCount / ShardLabel describe this run's slot in its parent's
	// fan-out. All empty for a top-level (non-sharded) run. The studio
	// projects these to render a run's shard/child subtree.
	ParentRunID string `json:"parent_run_id,omitempty"`
	// ParentNodeID is the IR node id of the subbot node in the parent
	// workflow that spawned this child run; empty for root runs and
	// non-subbot children. See store.Run.ParentNodeID.
	ParentNodeID string `json:"parent_node_id,omitempty"`
	ShardIndex   int    `json:"shard_index,omitempty"`
	ShardCount   int    `json:"shard_count,omitempty"`
	ShardLabel   string `json:"shard_label,omitempty"`
	// WatchedIssueIDs is the server-authoritative set of native-kanban
	// issue IDs this run subscribed to (MVP3b). The studio's whats-next
	// WatchPanel reads it as the primary watch-list source, falling back
	// to its event-derived list for legacy runs that predate the field.
	WatchedIssueIDs []string `json:"watched_issue_ids,omitempty"`
	// BackendsUsed summarizes the distinct (backend, model) pairs the
	// run's LLM/delegate nodes actually executed against, derived by
	// folding node_finished events (each stamps _backend / _model on its
	// output). Auto-detected backends report their RESOLVED value, not
	// "auto". Nil for runs with no LLM nodes (tool/compute-only) so the
	// studio RunHeader renders no chip. See the studio BackendsUsed row.
	BackendsUsed []BackendUsage `json:"backends_used,omitempty"`
	// FallbacksUsed lists the nodes that were served by a fallback route
	// rather than their first choice, with the route that served them.
	// Empty on every clean run.
	//
	// It exists because a degraded run is otherwise indistinguishable
	// from a clean one after the fact: a weaker model still returns a
	// well-formed answer, and BackendsUsed alone cannot say whether a
	// second backend appeared because the bot asked for it or because
	// the first one ran out. See ADR-087.
	FallbacksUsed []FallbackUsage `json:"fallbacks_used,omitempty"`
	// Loops is the run-level "real loops" indicator: one entry per
	// declared named loop (e.g. "review_loop"), reporting the SEMANTIC
	// iteration counter (matching the runtime's `node#N` log label and
	// review_loop.iteration), NOT the count of node executions — a
	// resume re-runs a mid-loop iteration, so the physical execution
	// count drifts above the true loop counter. Current is the max
	// iteration observed across the loop's node_started events
	// (iteration_path); Max is the declared bound (0 = unbounded /
	// expression cap / unknown). Absent for runs with no named loops.
	Loops map[string]RunLoopProgress `json:"loops,omitempty"`
	// Deployment is the run's delivery outcome — the live URL, the image
	// actually running, and the traceability verdict — folded from any
	// node whose structured output carries the deployment-report keys
	// (see recordDeployment). Nil for the overwhelming majority of runs,
	// which deploy nothing; the studio then renders no deployment row.
	Deployment *DeploymentReport `json:"deployment,omitempty"`
}

// BackendUsage is one distinct (backend, model) pair a run's LLM /
// delegate nodes executed against. NodeCount is the number of distinct
// IR nodes that resolved to this pair, so loop iterations and resume
// re-executions of the same node do not inflate it. Model is empty when
// the backend did not report an effective model.
type BackendUsage struct {
	Backend   string `json:"backend"`
	Model     string `json:"model,omitempty"`
	NodeCount int    `json:"node_count"`
}

// RunLoopProgress reports a named loop's semantic progress at the run
// level: the current iteration counter and its declared bound.
type RunLoopProgress struct {
	Current int `json:"current"`
	Max     int `json:"max,omitempty"`
}

// DeploymentReport is a run's delivery outcome, assembled from the
// deployment-report output contract (see recordDeployment). It is the
// run-level answer to "what did this ship, and where can I see it".
//
// Deployed/Healthy are the deploying node's own claims; Trace carries
// the INDEPENDENT verdict on whether that claim is traceable back to
// the repository. The two are kept apart on purpose: a URL that answers
// 200 while nothing was pushed and nothing is reproducible is not a
// delivery, and the studio must never render the first without the
// second.
type DeploymentReport struct {
	// NodeID is the IR node that reported the delivery, so the operator
	// can jump to its output. Empty only for hand-built values.
	NodeID string `json:"node_id,omitempty"`
	// Deployed is the reporting node's claim that the deploy applied.
	Deployed bool `json:"deployed"`
	// Healthy is its claim that the deployed URL answers healthily.
	Healthy bool `json:"healthy"`
	// URL is the public address a human opens. Empty on a failed or
	// blocked deploy — a truthful blocker, never a guessed URL.
	URL string `json:"url,omitempty"`
	// ImageRef is the exact image reference running, e.g.
	// "ghcr.io/owner/repo:<sha>". Empty when none was reported.
	ImageRef string `json:"image_ref,omitempty"`
	// Commit is the source commit the delivery is anchored to (the one
	// the image is expected to name). Empty when the reporter did not
	// state it — the studio then shows no commit rather than borrowing
	// the run's final_commit, which can be a LATER commit than the one
	// deployed.
	Commit string `json:"commit,omitempty"`
	// Notes is the reporter's own prose: what was published, or the
	// concrete blocking error.
	Notes string `json:"notes,omitempty"`
	// Trace is the traceability verdict. Nil when no node ran a
	// traceability gate at all — a distinct fact from a gate that ran
	// and could not establish the facts (Trace.Verifiable == false).
	Trace *DeploymentTrace `json:"trace,omitempty"`
}

// DeploymentTrace is the traceability verdict on a delivery: whether the
// running artifact can be traced back to reviewable source.
//
// Verifiable is the meta-fact and gates the other three: false means the
// gate could not establish them at all (git unreachable, gate miswired).
// That is NOT a failed delivery — reading it as one rejects deliveries
// that are in fact correct — and the studio renders it as its own
// "unverified" state, never as a failure.
type DeploymentTrace struct {
	NodeID string `json:"node_id,omitempty"`
	// Verifiable reports whether the gate could establish the facts.
	// When false, the three booleans below carry no information.
	Verifiable bool `json:"verifiable"`
	// Pushed: the deployed commits are reachable from a remote branch.
	Pushed bool `json:"pushed"`
	// ImageFromRepo: the running image is published under this repo's
	// own registry path (not a stock base image).
	ImageFromRepo bool `json:"image_from_repo"`
	// BuiltFromHead: the image reference names the pushed commit, which
	// is what ties the running artifact to reviewable source.
	BuiltFromHead bool `json:"built_from_head"`
	// Log is the gate's own explanation — the remedy on a failure, the
	// environment fault when unverifiable.
	Log string `json:"log,omitempty"`
}

// Traceable reports whether the delivery is fully traced back to the
// repository. It is only meaningful when Verifiable is true; callers
// must branch on Verifiable first.
func (t *DeploymentTrace) Traceable() bool {
	return t != nil && t.Verifiable && t.Pushed && t.ImageFromRepo && t.BuiltFromHead
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
	nodeCount map[string]map[string]int // branch_id → ir_node_id → next iteration index (LEGACY, for fallback int-iter path)
	// lastExecID maps (branch → nodeID → exec_id) to the most recent
	// node_started exec_id for that node. currentExec uses it to find
	// the in-flight execution for downstream events (node_finished,
	// artifact_written, run_failed, …) when the exec_id is derived from
	// `iteration_path` (a stable encoding of all containing loop
	// counters) rather than the scalar `iteration`. nodeCount alone
	// can't reconstruct a path-based id; this map is the bridge.
	lastExecID map[string]map[string]string
	lastSeq    int64
	// lastResumedSeq is the seq of the most recent EventRunResumed.
	// The monotonic guard in handleNodeStarted refuses to downgrade a
	// terminal exec back to running on a duplicate node_started, which
	// is the right behaviour for WS-history replay and runtime re-
	// emission within the same execution. It is the WRONG behaviour for
	// a genuine post-resume re-execution: when the runtime resumes from
	// a `failed_resumable` checkpoint, it re-runs the failed node with
	// the SAME (branch, node, iter) and therefore the same exec_id, and
	// the guard would otherwise lock the canvas on the pre-resume
	// terminal status forever (observed live across 10+ session-fix
	// attempts as "pipeline running but no node currently running").
	//
	// We disambiguate by seq: an existing terminal exec whose LastSeq
	// is BEFORE the last run_resumed is a pre-resume artefact, and a
	// node_started arriving AFTER the run_resumed is a fresh attempt
	// allowed to flip it back to running. The WS-replay scenario is
	// preserved — those duplicate node_starteds all have seq ≤
	// lastResumedSeq (no resume happened during the replay), so the
	// existing.LastSeq comparison fails and the guard still kicks in.
	lastResumedSeq int64
	// monotonicActive is set once any applied event carries a non-zero
	// Event.ActiveMs (the engine's monotonic SharedBudget elapsed). In
	// that mode the authoritative active-duration base is adopted
	// directly from ActiveMs and accumulateActive stops summing
	// wall-clock event windows (which over-counted OS-suspend time —
	// BUG A). Legacy runs (all ActiveMs == 0) keep the wall-clock path.
	monotonicActive bool
	// loopCurrent / loopBound back the run-level "real loops" indicator:
	// the max SEMANTIC iteration seen per named loop (from each
	// node_started's iteration_path — dedup-safe against resume
	// re-executions because it's a max, not a count) and the declared
	// bound (from run_started's `loops` payload). Combined into
	// RunHeader.Loops on Snapshot.
	loopCurrent map[string]int
	loopBound   map[string]int
	// backendUsage aggregates the distinct (backend, model) pairs the
	// run's LLM/delegate nodes executed against (from each node_finished's
	// stamped _backend / _model). Keyed by backend\x00model; backendOrder
	// preserves first-seen order for a deterministic BackendsUsed slice;
	// each agg tracks the set of distinct IR node ids so loops/resumes
	// don't inflate NodeCount.
	backendUsage map[string]*backendAgg
	backendOrder []string
	// fallbacksUsed accumulates the nodes a fallback route served, in
	// event order.
	fallbacksUsed []FallbackUsage
	// deployment / deployTrace hold the LAST reported delivery and the
	// LAST traceability verdict (see recordDeployment). Last-write-wins
	// because a redeploy loop re-reports both, and the final attempt is
	// the run's actual outcome. Kept out of b.header: SetRun rebuilds
	// the header from run.json, so event-derived state is re-attached in
	// Snapshot() like Loops and BackendsUsed.
	deployment  *DeploymentReport
	deployTrace *DeploymentTrace
	// deployTraceCommit is the commit the traceability gate resolved from
	// git. Held separately from deployment.Commit so the two groups can
	// arrive in either order; buildDeployment applies the precedence.
	deployTraceCommit string
}

// backendAgg accumulates one (backend, model) pair while folding events.
type backendAgg struct {
	backend string
	model   string
	nodes   map[string]bool
}

// NewSnapshotBuilder seeds a builder from the persisted Run metadata.
// Pass run=nil for an empty initial snapshot (e.g. when the WS catch-up
// races run.json creation).
func NewSnapshotBuilder(run *store.Run) *SnapshotBuilder {
	b := &SnapshotBuilder{
		execs:          make(map[string]*ExecutionState),
		nodeCount:      make(map[string]map[string]int),
		lastExecID:     make(map[string]map[string]string),
		lastSeq:        NoEventsSeq,
		lastResumedSeq: NoEventsSeq,
		loopCurrent:    make(map[string]int),
		loopBound:      make(map[string]int),
		backendUsage:   make(map[string]*backendAgg),
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

	// Authoritative monotonic active-duration base (BUG A fix): when the
	// engine stamped Event.ActiveMs (SharedBudget CLOCK_MONOTONIC
	// elapsed, suspend-excluded), adopt it directly instead of summing
	// wall-clock event windows, which counted OS-suspend time as active.
	// Monotonic within a run and across resume, so max() is just a guard
	// against a cosmetic live tail that briefly ran ahead. Once seen,
	// the run is in monotonic mode (accumulateActive stops adding
	// wall-clock windows); legacy runs (all ActiveMs == 0) are unchanged.
	if evt.ActiveMs > 0 {
		b.monotonicActive = true
		if evt.ActiveMs > b.header.ActiveDurationMs {
			b.header.ActiveDurationMs = evt.ActiveMs
		}
	}

	branch := evt.BranchID
	if branch == "" {
		branch = MainBranch
	}

	switch evt.Type {
	case store.EventNodeStarted:
		b.handleNodeStarted(evt, branch)
	case store.EventNodeFinished:
		b.handleNodeFinished(evt, branch)
		b.recordBackendUsage(evt)
		b.recordDeployment(evt)
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
		b.recordLoopBounds(evt)
		b.anchorActive(evt.Timestamp)
	case store.EventRunFinished:
		b.accumulateActive(evt.Timestamp)
	case store.EventRunCancelled:
		b.handleRunCancelled(evt)
	case store.EventRunRewound:
		b.handleRunRewound(evt)
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
	header := b.header
	header.Loops = b.buildLoopProgress()
	header.BackendsUsed = b.buildBackendsUsed()
	header.FallbacksUsed = b.fallbacksUsed
	header.Deployment = b.buildDeployment()
	return &RunSnapshot{
		Run:        header,
		Executions: execs,
		LastSeq:    b.lastSeq,
	}
}

// buildLoopProgress assembles the run-level named-loop indicator from
// the observed per-loop max iteration (loopCurrent) and declared bounds
// (loopBound). Returns nil when no named loop has been observed, so
// runs without loops (and legacy runs) render nothing. The union of
// keys is taken so a bound with no observed iteration yet still shows
// (current 0), and vice-versa.
func (b *SnapshotBuilder) buildLoopProgress() map[string]RunLoopProgress {
	if len(b.loopCurrent) == 0 && len(b.loopBound) == 0 {
		return nil
	}
	out := make(map[string]RunLoopProgress, len(b.loopCurrent))
	for name, cur := range b.loopCurrent {
		out[name] = RunLoopProgress{Current: cur, Max: b.loopBound[name]}
	}
	for name, max := range b.loopBound {
		if _, seen := out[name]; !seen {
			out[name] = RunLoopProgress{Current: 0, Max: max}
		}
	}
	return out
}

// recordBackendUsage folds a node_finished event into the per-(backend,
// model) usage tally. The runtime stamps _backend (always, for delegate
// nodes) and _model (when the backend reports an effective model) onto
// the node's output map, which node_finished carries under data.output.
// Nodes with no _backend (tool/compute/router terminals) contribute
// nothing, so a run with only such nodes yields an empty BackendsUsed.
func (b *SnapshotBuilder) recordBackendUsage(evt *store.Event) {
	if evt.NodeID == "" || evt.Data == nil {
		return
	}
	output, ok := evt.Data["output"].(map[string]any)
	if !ok {
		return
	}
	backend, _ := output["_backend"].(string)
	if backend == "" {
		return
	}
	model, _ := output["_model"].(string)
	if used, _ := output["_fallback_used"].(bool); used {
		servedBy, _ := output["_served_by"].(string)
		// Dedupe by node: a declared loop re-emits node_finished per
		// iteration, and appending once per pass produces N identical
		// chips (and colliding React keys) in the run header (R546ddc).
		// backends_used already keys on (backend, model) + a node set;
		// keep the first-seen route per node so the header names the
		// first fall-through that served it.
		already := false
		for _, u := range b.fallbacksUsed {
			if u.NodeID == evt.NodeID {
				already = true
				break
			}
		}
		if !already {
			b.fallbacksUsed = append(b.fallbacksUsed, FallbackUsage{
				NodeID: evt.NodeID, ServedBy: servedBy, Backend: backend, Model: model,
			})
		}
	}
	key := backend + "\x00" + model
	agg := b.backendUsage[key]
	if agg == nil {
		agg = &backendAgg{backend: backend, model: model, nodes: make(map[string]bool)}
		b.backendUsage[key] = agg
		b.backendOrder = append(b.backendOrder, key)
	}
	agg.nodes[evt.NodeID] = true
}

// FallbackUsage names one node that a fallback route served.
type FallbackUsage struct {
	NodeID string `json:"node_id"`
	// ServedBy is the route's declared name (a `fallbacks:` entry name,
	// or "run-fallback" for the operator's launch-time route).
	ServedBy string `json:"served_by,omitempty"`
	// Backend / Model are what actually ran, not what was requested.
	Backend string `json:"backend,omitempty"`
	Model   string `json:"model,omitempty"`
}

// buildBackendsUsed materialises the aggregated (backend, model) pairs
// in first-seen order. Returns nil when no delegate node has finished so
// tool/compute-only runs render no backend chip.
func (b *SnapshotBuilder) buildBackendsUsed() []BackendUsage {
	if len(b.backendOrder) == 0 {
		return nil
	}
	out := make([]BackendUsage, 0, len(b.backendOrder))
	for _, key := range b.backendOrder {
		agg := b.backendUsage[key]
		out = append(out, BackendUsage{
			Backend:   agg.backend,
			Model:     agg.model,
			NodeCount: len(agg.nodes),
		})
	}
	return out
}

// ---------------------------------------------------------------------------
// Deployment report (delivery + traceability output contract)
// ---------------------------------------------------------------------------

// The deployment-report output contract: reserved output keys any bot
// can emit to declare a delivery, folded here into RunHeader.Deployment.
// The seam is the FIELD NAMES, never a bot or node name, so the studio
// stays bot-agnostic. Two groups, recognized independently so a bot may
// emit them from one node or split them across a deploying agent and a
// deterministic traceability gate (the app-dev shape):
//
//	delivery      keyed on deploymentURLKey — plus deployed / healthy /
//	              image_ref / commit / notes
//	traceability  keyed on traceVerifiableKey — plus pushed /
//	              image_from_repo / built_from_head / trace_log
//
// A node output lacking the group's key contributes nothing, so runs
// that deploy nothing (nearly all of them) carry no Deployment at all.
const (
	deploymentURLKey   = "deployed_url"
	traceVerifiableKey = "verifiable"
)

// recordDeployment folds a node_finished event into the run's delivery
// outcome. Last-write-wins per group: a redeploy loop re-reports both,
// and the final attempt is the run's actual outcome.
func (b *SnapshotBuilder) recordDeployment(evt *store.Event) {
	if evt.NodeID == "" || evt.Data == nil {
		return
	}
	output, ok := evt.Data["output"].(map[string]any)
	if !ok {
		return
	}
	if url, present := output[deploymentURLKey]; present {
		urlStr, _ := url.(string)
		b.deployment = &DeploymentReport{
			NodeID:   evt.NodeID,
			Deployed: outputBool(output, "deployed"),
			Healthy:  outputBool(output, "healthy"),
			URL:      strings.TrimSpace(urlStr),
			ImageRef: strings.TrimSpace(outputString(output, "image_ref")),
			Commit:   strings.TrimSpace(outputString(output, "commit")),
			Notes:    outputString(output, "notes"),
		}
	}
	// The traceability group needs its meta-fact AND at least one of the
	// facts it qualifies: `verifiable` alone is too common a field name
	// to treat as a traceability verdict on its own.
	if _, present := output[traceVerifiableKey]; present && hasAnyKey(output, "pushed", "image_from_repo", "built_from_head") {
		b.deployTrace = &DeploymentTrace{
			NodeID:        evt.NodeID,
			Verifiable:    outputBool(output, traceVerifiableKey),
			Pushed:        outputBool(output, "pushed"),
			ImageFromRepo: outputBool(output, "image_from_repo"),
			BuiltFromHead: outputBool(output, "built_from_head"),
			Log:           outputString(output, "trace_log"),
		}
		b.deployTraceCommit = strings.TrimSpace(outputString(output, "commit"))
	}
}

// buildDeployment materialises the run's delivery outcome, attaching the
// traceability verdict to the delivery. Returns nil when no node
// reported a delivery, so a run that deploys nothing renders no row.
//
// A traceability verdict with no delivery is dropped: the verdict exists
// to qualify a URL, and on its own it has nothing to qualify.
func (b *SnapshotBuilder) buildDeployment() *DeploymentReport {
	if b.deployment == nil {
		return nil
	}
	out := *b.deployment
	if b.deployTrace != nil {
		trace := *b.deployTrace
		out.Trace = &trace
	}
	// The gate resolves the commit from git itself, so it outranks the
	// deploying agent's own claim; the claim stands when no gate ran.
	if b.deployTraceCommit != "" {
		out.Commit = b.deployTraceCommit
	}
	return &out
}

// outputBool reads a bool field from a node output map, treating a
// missing or non-bool value as false.
func outputBool(output map[string]any, key string) bool {
	v, _ := output[key].(bool)
	return v
}

// outputString reads a string field from a node output map, treating a
// missing or non-string value as empty.
func outputString(output map[string]any, key string) string {
	v, _ := output[key].(string)
	return v
}

// hasAnyKey reports whether the map carries at least one of the keys.
func hasAnyKey(output map[string]any, keys ...string) bool {
	for _, k := range keys {
		if _, ok := output[k]; ok {
			return true
		}
	}
	return false
}

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
	// Run-level loop indicator: fold this node's iteration_path into the
	// per-named-loop max counter (the semantic loop iteration).
	b.recordLoopIteration(evt)
	iter := b.resolveIteration(branch, evt)
	// Exec-id disambiguation: when the runtime stamps `iteration_path`
	// (a stable encoding of EVERY containing loop's counter) we key on
	// it instead of the scalar `iteration`. Single-int iter collapsed
	// executions of a node living in NESTED loops onto the same exec
	// (observed live: validate_upgrade in fix_loop ⊂ package_loop ⊂
	// family_loop; max() returned the frozen family_loop counter so
	// every package's attempt landed on the SAME exec_id, the canvas
	// then locked on the first attempt's terminal status). The path
	// gives every (loop counters tuple) a strictly unique identity.
	// LoopIteration on the exec stays as the scalar iter for the
	// studio's pip strips + per-iteration filtering.
	id := makeExecutionIDFromEvent(branch, evt.NodeID, iter, evt.Data)
	ts := evt.Timestamp
	existing := b.execs[id]
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
	// Collision (retry of the same (node, iteration) after a recovery
	// loop, or runtime re-emission when the node is not on a loop edge
	// directly and currentLoopIteration returns the same value for
	// successive runs): preserve original started_at / first_seq / kind
	// so the timeline still anchors on the first attempt.
	//
	// Monotonic guard (mirror of studio T2.4 9bcccff): a duplicate
	// node_started must NOT downgrade an already-terminal execution
	// back to running. paused_waiting_human is monotonic too — only an
	// explicit run_resumed transitions it back to running, via
	// handleRunResumed.
	if existing != nil {
		exec.StartedAt = existing.StartedAt
		exec.FirstSeq = existing.FirstSeq
		if exec.Kind == "" {
			exec.Kind = existing.Kind
		}
		// Disambiguate "stale duplicate node_started for an already-
		// finished exec" (WS replay, runtime re-emission inside the
		// same execution attempt) from "genuine re-execution after a
		// run_resumed". For a post-resume re-run the runtime emits a
		// node_started with the SAME (branch, node, iter) and therefore
		// the same exec_id, and the existing exec — its last event
		// landed BEFORE the resume — should not lock the canvas on its
		// pre-resume terminal status. The seq comparison handles both
		// cases with one rule:
		//
		//   existing.LastSeq < lastResumedSeq  →  pre-resume artefact,
		//   the current node_started (seq > lastResumedSeq by construction
		//   since we only get here after applying it) is a fresh attempt
		//   that gets a fresh "running" status + a fresh StartedAt.
		//
		// Conversely, when no resume has happened (lastResumedSeq is
		// NoEventsSeq) or the existing exec's LastSeq is already past
		// the resume (it's the same execution stream as the duplicate),
		// the monotonic guard kicks in as before.
		preResumeArtefact := b.lastResumedSeq != NoEventsSeq && existing.LastSeq < b.lastResumedSeq
		if isTerminalExecStatus(existing.Status) && !preResumeArtefact {
			exec.Status = existing.Status
			exec.FinishedAt = existing.FinishedAt
			exec.Error = existing.Error
		}
		// On a post-resume re-execution we want a fresh started_at
		// (the user-visible "this node has been running for Xs" timer)
		// while still keeping FirstSeq anchored on the original event
		// for scrubbing / log-window calculations.
		if preResumeArtefact {
			ts := evt.Timestamp
			exec.StartedAt = &ts
			exec.FinishedAt = nil
			exec.Error = ""
		}
		b.execs[id] = exec
		b.rememberCurrentExec(branch, evt.NodeID, id)
		// Already in b.order — don't re-append, otherwise Snapshot()
		// would emit the same execution twice.
		return
	}
	b.execs[id] = exec
	b.order = append(b.order, id)
	b.rememberCurrentExec(branch, evt.NodeID, id)
}

// isTerminalExecStatus reports whether an exec status is monotonic —
// once reached, a stale or duplicate node_started must not downgrade
// it back to running. paused_waiting_human counts as terminal here
// because only run_resumed (explicit operator action) can transition
// it back, never a re-emitted node_started.
func isTerminalExecStatus(s ExecStatus) bool {
	switch s {
	case ExecStatusFinished, ExecStatusFailed, ExecStatusSkipped, ExecStatusPaused:
		return true
	}
	return false
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
	// Human nodes pass through Paused before they finish (the status
	// is flipped by handleHumanInputRequested, then a resume → answer
	// path emits node_finished). Allow the transition from Paused too,
	// otherwise the canvas stays stuck on the human node after answer.
	if exec.Status == ExecStatusRunning || exec.Status == ExecStatusPaused {
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
	errMsg, _ := evt.Data["error"].(string)
	if evt.NodeID != "" {
		exec := b.currentExec(branch, evt.NodeID)
		if exec != nil {
			ts := evt.Timestamp
			exec.Status = ExecStatusFailed
			if exec.FinishedAt == nil {
				exec.FinishedAt = &ts
			}
			if errMsg != "" {
				exec.Error = errMsg
			}
			exec.CurrentEventSeq = evt.Seq
			exec.LastSeq = evt.Seq
		}
	}
	// Any other execution still marked running when the run terminates
	// is, by definition, no longer running — the engine has stopped
	// driving it. Close them so the canvas spinner clears.
	b.closeInFlightExecs(ExecStatusFailed, evt.Timestamp, evt.Seq, errMsg)
}

// handleRunCancelled flips every still-running execution to "failed" with
// a cancelled-by-user marker. Without this, the canvas keeps spinning on
// whatever node was in flight when the operator hit cancel.
func (b *SnapshotBuilder) handleRunCancelled(evt *store.Event) {
	b.accumulateActive(evt.Timestamp)
	reason, _ := evt.Data["reason"].(string)
	if reason == "" {
		reason = "run cancelled"
	}
	b.closeInFlightExecs(ExecStatusFailed, evt.Timestamp, evt.Seq, reason)
}

// handleRunRewound erases the execution state of every node the rewind
// invalidated. Rewind (pkg/runview/rewind.go) deletes those nodes'
// outputs from the checkpoint, but the snapshot is folded from the
// append-only event log — the pre-rewind node_started / node_finished
// records are still in the stream, and without this the canvas keeps
// rendering the dropped nodes with their pre-rewind status, duration
// and error as if the rewind had not happened.
//
// Deleting (rather than downgrading to a neutral status) is what
// resets the node: with no execution left the canvas renders it as
// never-run, and the node_started the post-rewind resume emits
// recreates the exec cleanly — no dependence on the lastResumedSeq
// pre-resume-artefact rule, which exists to triage duplicate
// emissions, not to undo recorded state.
func (b *SnapshotBuilder) handleRunRewound(evt *store.Event) {
	b.accumulateActive(evt.Timestamp)
	dropped := rewoundNodeIDs(evt.Data["dropped_nodes"])
	if len(dropped) == 0 {
		return
	}
	removed := map[string]bool{}
	for id, exec := range b.execs {
		if exec != nil && dropped[exec.IRNodeID] {
			removed[id] = true
			delete(b.execs, id)
		}
	}
	if len(removed) == 0 {
		return
	}
	kept := b.order[:0]
	for _, id := range b.order {
		if !removed[id] {
			kept = append(kept, id)
		}
	}
	b.order = kept
	// Clear the (branch, node) → exec_id pointers and the legacy
	// iteration counters of the dropped nodes so the post-rewind
	// re-execution starts from a clean slate instead of attributing
	// downstream events to (or numbering iterations after) an
	// execution that no longer exists.
	for branch, perNode := range b.lastExecID {
		for nodeID := range perNode {
			if dropped[nodeID] {
				delete(perNode, nodeID)
			}
		}
		if len(perNode) == 0 {
			delete(b.lastExecID, branch)
		}
	}
	for branch, counts := range b.nodeCount {
		for nodeID := range counts {
			if dropped[nodeID] {
				delete(counts, nodeID)
			}
		}
		if len(counts) == 0 {
			delete(b.nodeCount, branch)
		}
	}
}

// rewoundNodeIDs normalises the run_rewound payload's dropped_nodes
// field into a lookup set. As appended by Rewind it is a []string;
// after a JSON round-trip through events.jsonl or Mongo it decodes
// as []any.
func rewoundNodeIDs(raw any) map[string]bool {
	out := map[string]bool{}
	switch v := raw.(type) {
	case []string:
		for _, id := range v {
			out[id] = true
		}
	case []any:
		for _, item := range v {
			if id, ok := item.(string); ok {
				out[id] = true
			}
		}
	}
	return out
}

// closeInFlightExecs terminates every execution still marked running
// (or paused) when a terminal run-level event arrives. Idempotent —
// already-closed executions are left untouched.
func (b *SnapshotBuilder) closeInFlightExecs(status ExecStatus, ts time.Time, seq int64, errMsg string) {
	for _, exec := range b.execs {
		if exec.Status != ExecStatusRunning && exec.Status != ExecStatusPaused {
			continue
		}
		exec.Status = status
		if exec.FinishedAt == nil {
			finished := ts
			exec.FinishedAt = &finished
		}
		if errMsg != "" && exec.Error == "" {
			exec.Error = errMsg
		}
		exec.CurrentEventSeq = seq
		exec.LastSeq = seq
	}
}

func (b *SnapshotBuilder) handleHumanInputRequested(evt *store.Event, branch string) {
	// An async question (ADR-081) never pauses the node — the run keeps
	// executing, so the exec status must not flip.
	if store.IsAsyncHumanInput(*evt) {
		return
	}
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
	// Stash the resume seq so subsequent handleNodeStarted calls can
	// distinguish "duplicate of an already-finished exec" (WS replay,
	// pre-resume artefact) from "fresh re-execution of the checkpoint
	// node" (resume-from-failed_resumable). See SnapshotBuilder
	// .lastResumedSeq for the rationale.
	b.lastResumedSeq = evt.Seq
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
	// In monotonic mode the authoritative base is already set from
	// Event.ActiveMs; only freeze the live tail (clear the anchor).
	// Adding the wall-clock window here would re-introduce the suspend
	// over-count BUG A fixes. Legacy runs (no ActiveMs) still sum it.
	if !b.monotonicActive {
		if delta := at.Sub(*b.header.CurrentRunStart); delta > 0 {
			b.header.ActiveDurationMs += delta.Milliseconds()
		}
	}
	b.header.CurrentRunStart = nil
}

// recordLoopBounds folds the run_started `loops` payload (name → max
// iterations) into loopBound. No-op when the event carries no loops
// (runs with no named loops, or legacy events pre-dating the field).
func (b *SnapshotBuilder) recordLoopBounds(evt *store.Event) {
	raw, ok := evt.Data["loops"]
	if !ok {
		return
	}
	m, ok := raw.(map[string]any)
	if !ok {
		return
	}
	for name, v := range m {
		b.loopBound[name] = toInt(v)
	}
}

// recordLoopIteration folds a node_started's iteration_path
// ("loop=count;loop2=count2") into loopCurrent, keeping the MAX counter
// seen per named loop. Using max (not a count of executions) is what
// makes the indicator report the semantic loop iteration (e.g. 48) and
// stay immune to resume re-executions that repeat the same iteration.
func (b *SnapshotBuilder) recordLoopIteration(evt *store.Event) {
	raw, ok := evt.Data["iteration_path"]
	if !ok {
		return
	}
	path, ok := raw.(string)
	if !ok || path == "" {
		return
	}
	for _, part := range strings.Split(path, ";") {
		eq := strings.IndexByte(part, '=')
		if eq <= 0 {
			continue
		}
		name := part[:eq]
		count, err := strconv.Atoi(part[eq+1:])
		if err != nil {
			continue
		}
		if count > b.loopCurrent[name] {
			b.loopCurrent[name] = count
		}
	}
}

// toInt coerces a JSON-decoded numeric (int / int64 / float64) to int;
// 0 for anything else. Event.Data round-trips through JSON so integer
// payloads surface as float64 on replay.
func toInt(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	}
	return 0
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

// resolveIteration honors the runtime-supplied iteration field on
// node_started when present (loop-counter semantics — does not bump
// on recovery retries). Falls back to the legacy auto-increment for
// pre-fix events. Keeps b.nodeCount[branch][nodeID] >= iter+1 so the
// fallback path stays monotonic.
func (b *SnapshotBuilder) resolveIteration(branch string, evt *store.Event) int {
	if raw, ok := evt.Data["iteration"]; ok {
		switch v := raw.(type) {
		case int:
			b.bumpNodeCount(branch, evt.NodeID, v)
			return v
		case int64:
			b.bumpNodeCount(branch, evt.NodeID, int(v))
			return int(v)
		case float64:
			b.bumpNodeCount(branch, evt.NodeID, int(v))
			return int(v)
		}
	}
	return b.allocIteration(branch, evt.NodeID)
}

// bumpNodeCount ensures the per-(branch, nodeID) counter is at least
// iter+1 so subsequent allocIteration calls (e.g. older events
// without iteration in the same run) don't collide retroactively.
func (b *SnapshotBuilder) bumpNodeCount(branch, nodeID string, iter int) {
	if b.nodeCount[branch] == nil {
		b.nodeCount[branch] = make(map[string]int)
	}
	if iter+1 > b.nodeCount[branch][nodeID] {
		b.nodeCount[branch][nodeID] = iter + 1
	}
}

// currentExec returns the most recently started execution of (branch,
// nodeID) — i.e. the latest node_started for that node. Subsequent
// events (node_finished, artifact_written, run_failed) are attributed
// there.
//
// Resolution order: (1) the lastExecID map populated by
// handleNodeStarted (path-aware, works for both legacy int-iter and
// iteration_path exec_ids); (2) the legacy nodeCount-based fallback
// for snapshots constructed before lastExecID existed (cold replays
// of old event streams, hand-built test fixtures).
func (b *SnapshotBuilder) currentExec(branch, nodeID string) *ExecutionState {
	if perBranch, ok := b.lastExecID[branch]; ok {
		if id, ok := perBranch[nodeID]; ok {
			if e := b.execs[id]; e != nil {
				return e
			}
		}
	}
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

// makeExecutionIDFromEvent prefers `iteration_path` (a stable string
// encoding of EVERY containing loop's counter — see runtime's
// currentLoopIterationPath) over the scalar `iteration` when building
// an exec_id. The path disambiguates executions of the same node
// across nested loops where a single int counter would collapse them
// (e.g., validate_upgrade in fix_loop ⊂ package_loop ⊂ family_loop:
// the scalar `iteration` was returning the frozen family_loop counter
// for every package's attempt, locking the canvas on the first
// attempt's terminal status). Events emitted by older runtime builds
// don't carry `iteration_path` — fall back to the legacy int form
// transparently so historical event streams still replay deterministically.
func makeExecutionIDFromEvent(branch, nodeID string, iteration int, data map[string]any) string {
	if branch == "" {
		branch = MainBranch
	}
	if data != nil {
		if p, ok := data["iteration_path"].(string); ok && p != "" {
			return fmt.Sprintf("exec:%s:%s:%s", branch, nodeID, p)
		}
	}
	return fmt.Sprintf("exec:%s:%s:%d", branch, nodeID, iteration)
}

// rememberCurrentExec stamps the latest exec_id seen for (branch,
// nodeID). currentExec consults the map for downstream events whose
// payload doesn't carry the iteration_path the node_started used.
func (b *SnapshotBuilder) rememberCurrentExec(branch, nodeID, execID string) {
	if b.lastExecID[branch] == nil {
		b.lastExecID[branch] = make(map[string]string)
	}
	b.lastExecID[branch][nodeID] = execID
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
		ID:                r.ID,
		Name:              r.Name,
		WorkflowName:      r.WorkflowName,
		WorkflowHash:      r.WorkflowHash,
		FilePath:          r.FilePath,
		BundleName:        r.BundleName,
		BundleDisplayName: r.BundleDisplayName,
		Status:            r.Status,
		Inputs:            r.Inputs,
		PermissionMode:    r.PermissionMode,
		ModelOverrides:    r.ModelOverrides,
		NodesServed:       r.NodesServed,
		Budget:            r.Budget,
		CreatedAt:         r.CreatedAt,
		UpdatedAt:         r.UpdatedAt,
		FinishedAt:        r.FinishedAt,
		Error:             r.Error,
		Checkpoint:        r.Checkpoint,
		WorkDir:           r.WorkDir,
		ProjectPath:       r.ProjectPath,
		Worktree:          r.Worktree,
		WorktreeAvailable: worktreeAvailable(r.WorkDir),
		FinalCommit:       r.FinalCommit,
		FinalBranch:       r.FinalBranch,
		FinalBranchError:  r.FinalBranchError,
		MergedInto:        r.MergedInto,
		MergedCommit:      r.MergedCommit,
		MergeStrategy:     r.MergeStrategy,
		MergeStatus:       r.MergeStatus,
		AutoMerge:         r.AutoMerge,
		Source:            r.Source,
		ParentRunID:       r.ParentRunID,
		ParentNodeID:      r.ParentNodeID,
		ShardIndex:        r.ShardIndex,
		ShardCount:        r.ShardCount,
		ShardLabel:        r.ShardLabel,
		WatchedIssueIDs:   r.WatchedIssueIDs,
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

// worktreeAvailable reports whether dir exists and is a directory on this
// server's filesystem. Empty dir (pre-feature runs, or runs with no
// recorded WorkDir) and absent/removed paths both yield false. This is the
// single signal the studio keys its inline file-editor affordances off of;
// it deliberately mirrors the /files/content endpoint's own gate so the
// UI's affordance and the endpoint's 409 stay in lockstep.
func worktreeAvailable(dir string) bool {
	if dir == "" {
		return false
	}
	info, err := os.Stat(dir)
	return err == nil && info.IsDir()
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
