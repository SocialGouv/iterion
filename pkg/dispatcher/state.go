package dispatcher

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/SocialGouv/iterion/pkg/dispatcher/tracker"
)

// state is the in-memory bookkeeping the dispatcher's actor goroutine
// owns. Outside callers must reach it through typed commands posted on
// Dispatcher.cmds — never read these maps directly.
//
// "Claimed" is not a separate field — the union (running ∪ retries)
// is the live claim set. Dispatch checks both maps before picking up
// a candidate.
type state struct {
	running      map[string]*runningEntry
	retries      map[string]*retryEntry
	slotsByState map[string]int // running count per workflow state
	// tombstones holds issueIDs whose slot was reaped by
	// refreshRunningStates while the worker goroutine was still draining.
	// dispatch treats a tombstoned id as claimed so we don't land a
	// sibling dispatch on the same workspace; cmdRunFinished removes
	// the entry once the worker has actually exited.
	tombstones map[string]struct{}
	// lastTrackerErr is the most recent failure from tracker.ListCandidates,
	// surfaced in the Snapshot so the dashboard can show "GitHub token
	// expired" / "Forgejo unreachable" rather than going silent. Cleared
	// after the next successful poll. The actor goroutine is the single
	// writer so no mutex is needed.
	lastTrackerErr   string
	lastTrackerErrAt time.Time
	// costCap is the latest daily spend-cap status, recomputed each tick
	// when a cap is configured (nil when disabled). Surfaced in the
	// Snapshot so the studio dispatcher view can render the cap banner.
	// Actor-goroutine-owned; no mutex needed.
	costCap *CostCapView
	// discoveryInFlight is true while a candidate-discovery goroutine
	// (launchDiscovery → tracker.ListCandidates) is running off the actor
	// and has not yet posted its cmdCandidates result back. tick() sets it
	// before spawning the goroutine and cmdCandidates.apply clears it, so a
	// second poll tick while one is in flight skips starting another (no
	// overlapping discovery). Both writers run on the actor goroutine, so a
	// plain bool is race-free — no atomic needed. See ADR-028 Step 2.
	discoveryInFlight bool
	// dispatchSkips records eligible issues the actor refused to claim
	// because their explicit `bot` couldn't be resolved or has no
	// dispatch route. Keyed by issueID. Without this, an honest-fail
	// skip lived only in a dispatcher log line the studio operator never
	// sees, so a misconfigured ticket sat idle in the eligible lane with
	// no UI signal. Rebuilt each candidate scan (pruned to live
	// candidates in tick); an entry is cleared the moment its issue
	// successfully claims. Actor-goroutine-owned; no mutex needed.
	dispatchSkips map[string]DispatchSkipView
}

func newState() *state {
	return &state{
		running:       map[string]*runningEntry{},
		retries:       map[string]*retryEntry{},
		slotsByState:  map[string]int{},
		tombstones:    map[string]struct{}{},
		dispatchSkips: map[string]DispatchSkipView{},
	}
}

// isClaimed reports whether the actor is currently treating issueID
// as "ours" — either in flight or queued for retry (but not yet fired:
// once the retry timer fires we want the next tick to pick the issue
// up so the dispatch can carry the accumulated Attempt count), or
// tombstoned (slot reaped but worker still draining).
func (s *state) isClaimed(issueID string) bool {
	if _, ok := s.running[issueID]; ok {
		return true
	}
	if _, ok := s.tombstones[issueID]; ok {
		return true
	}
	r, ok := s.retries[issueID]
	if !ok {
		return false
	}
	return !r.Fired
}

// runningEntry tracks one in-flight dispatch. It outlives the actor's
// view of the goroutine. The actor owns the struct fields; the only
// off-actor liveness signal is the separate atomic heartbeat pointer
// below, which is intentionally shared with DispatchSpec.OnEvent.
type runningEntry struct {
	IssueID    string
	Identifier string
	RunID      string
	// WorkspaceGeneration is stable across retries of one logical dispatch,
	// even when a non-resumable engine failure requires a fresh RunID. A
	// reopened/completed ticket gets a new generation and therefore a new
	// absolute path.
	WorkspaceGeneration string
	WorkflowState       string
	WorkspacePath       string
	StartedAt           time.Time
	LastEventAt         time.Time
	LastEventName       string
	Attempt             int
	Cancel              context.CancelFunc

	// CancelIssuedAt is non-zero once reconcileStalled has called
	// Cancel(); subsequent ticks suppress the cancel + warn re-spam
	// while the worker drains (F-CD-12). The actor goroutine is the
	// single writer so no mutex is needed.
	CancelIssuedAt time.Time

	// setupPending is true between claim-time allocation (dispatch, on the
	// actor) and the off-actor setup worker reporting the in-progress
	// transition + workspace back via cmdDispatchSetupDone. It marks the
	// explicit claimed→running intermediate state (ADR-028 Step 4): the
	// transition's UpdateState runs off the actor, so for a brief window the
	// tracker may already read RunningState while the entry has not yet
	// recorded TransitionedFromState. The actor's reapers (refreshRunningStates,
	// reconcileStalled) skip setupPending entries so that in-flight transition
	// is not misread as an external move, and a run that has not started yet is
	// not stall-reaped on its claim-time watermark. Actor-owned; no mutex.
	setupPending bool

	// TransitionedFromState is the issue's tracker state at Claim time
	// IFF the dispatcher then successfully moved it to
	// cfg.Agent.RunningState. Empty when no transition occurred
	// (running_state disabled, transition rejected, or the issue was
	// already in RunningState at Claim time). The cancel and shutdown
	// paths read this to revert the transition; the clean-finish path
	// intentionally leaves the issue in RunningState.
	TransitionedFromState string

	// lastEventAtomicNano is updated synchronously by the OnEvent
	// callback (which runs in the runtime engine's goroutine). It
	// exists so reconcileStalled doesn't depend on the actor having
	// drained queued cmdEvent messages — the 2026-05-21 dogfood showed
	// runs being false-positive-stalled at the exact 10min mark when
	// tool events were still firing every few seconds. The actor-owned
	// LastEventAt above is still maintained via cmdEvent.apply for
	// observability (LastEventName, snapshot rendering), but stall
	// detection now reads this atomic.
	lastEventAtomicNano atomic.Int64

	// issueSnapshot is the tracker.Issue snapshot used to render
	// dispatch.vars. Kept so the dispatcher can render a fresh prompt
	// on retry without re-fetching from the tracker.
	issueSnapshot tracker.Issue
}

// touchEvent records that an event was observed for this entry. Safe
// to call from any goroutine — backs the atomic heartbeat used by
// reconcileStalled.
func (r *runningEntry) touchEvent(t time.Time) {
	r.lastEventAtomicNano.Store(t.UnixNano())
}

// lastEventTime returns the freshest heartbeat seen by any goroutine
// (synchronously-updated atomic OR the actor's actor-applied
// LastEventAt). Returns the max of both to avoid a race where the
// atomic was set after the actor read but before reconcileStalled.
func (r *runningEntry) lastEventTime() time.Time {
	atomicNano := r.lastEventAtomicNano.Load()
	if atomicNano == 0 {
		return r.LastEventAt
	}
	atomicT := time.Unix(0, atomicNano)
	if atomicT.After(r.LastEventAt) {
		return atomicT
	}
	return r.LastEventAt
}

// retryEntry tracks one pending retry. Used both for the timer
// bookkeeping (Timer + DueAt) and to render the dashboard row.
type retryEntry struct {
	IssueID    string
	Identifier string
	Attempt    int
	DueAt      time.Time
	LastError  string
	Timer      *time.Timer
	// Fired is true once the backoff timer has expired and the entry
	// is ready for re-dispatch. We keep the entry around (with the
	// running Attempt count) instead of deleting it on timer fire so
	// the next dispatch can pick up the correct attempt number — the
	// old code deleted on fire and re-derived attempt as 0 on the
	// next tick.
	Fired bool
	// PrevRunID is the runID of the previous attempt that produced
	// this retry. When the prior run terminated in a resumable state
	// (`failed_resumable`, `cancelled`, `paused_operator`), the
	// dispatcher resumes from that run's checkpoint instead of
	// minting a fresh runID — the engine's resume machinery picks
	// up at the failing node rather than re-executing every upstream
	// node. Empty when the retry is intentionally a clean restart
	// (e.g. the prior run was `failed` without a checkpoint, or the
	// retry policy explicitly disabled resume).
	PrevRunID string
	// WorkspaceGeneration carries the logical workspace lease across a fresh
	// (non-resume) retry so incremental files survive without ever reusing the
	// path after a completed logical dispatch.
	WorkspaceGeneration string
}

// Snapshot is the read-only view the dashboard consumes. Built on
// demand from inside the actor so callers always see a consistent
// snapshot of running/retries/slots.
type Snapshot struct {
	Name             string    `json:"name"`
	Tracker          string    `json:"tracker"`
	GeneratedAt      time.Time `json:"generated_at"`
	PollingIntervalS float64   `json:"polling_interval_seconds"`
	StallTimeoutS    float64   `json:"stall_timeout_seconds"`
	// Paused is true when new dispatches are currently suspended via
	// Pause(); runs in flight are not affected.
	Paused  bool          `json:"paused"`
	Running []RunningView `json:"running"`
	Retries []RetryView   `json:"retries"`
	Slots   SlotsView     `json:"slots"`
	// LastTrackerError reports the most recent failure from the
	// tracker's ListCandidates call. Empty when the last poll
	// succeeded. The dashboard surfaces this as a banner so auth
	// failures (expired tokens, rotated credentials) don't silently
	// stall the dispatcher.
	LastTrackerError   string    `json:"last_tracker_error,omitempty"`
	LastTrackerErrorAt time.Time `json:"last_tracker_error_at,omitempty"`
	// CostCap reports the per-(store, UTC-day) spend-cap status when a
	// cap is configured; nil when disabled. The dashboard renders a
	// banner + spend/limit when Exceeded.
	CostCap *CostCapView `json:"cost_cap,omitempty"`
	// DispatchSkips lists eligible issues the dispatcher refused to claim
	// this scan because their explicit `bot` is unresolvable or has no
	// dispatch route. Empty in the common case. The board + dispatcher
	// dashboard surface these so a misconfigured `bot:` is visible and
	// actionable instead of silently never running.
	DispatchSkips []DispatchSkipView `json:"dispatch_skips,omitempty"`
}

// DispatchSkipView is one entry of the "won't dispatch" surface: an
// eligible issue the dispatcher refused to claim because its explicit
// bot can't be resolved to a workflow file, or resolves but has no
// dispatch route (not in assignee_workflows). The remedy is operator-
// side — fix the `bot:` value or add it to assignee_workflows — so
// surfacing the reason turns a silent stall into an actionable signal.
type DispatchSkipView struct {
	IssueID    string `json:"issue_id"`
	Identifier string `json:"identifier"`
	Bot        string `json:"bot,omitempty"`
	Reason     string `json:"reason"`
}

// CostCapView is the dashboard projection of the daily spend cap.
type CostCapView struct {
	Date           string  `json:"date"`
	SpentUSD       float64 `json:"spent_usd"`
	LimitUSD       float64 `json:"limit_usd"`
	Exceeded       bool    `json:"exceeded"`
	OverrideActive bool    `json:"override_active"`
}

// RunningView is one row of the dashboard's "running" table.
type RunningView struct {
	IssueID       string    `json:"issue_id"`
	Identifier    string    `json:"identifier"`
	RunID         string    `json:"run_id"`
	WorkflowState string    `json:"workflow_state"`
	WorkspacePath string    `json:"workspace_path,omitempty"`
	StartedAt     time.Time `json:"started_at"`
	LastEventAt   time.Time `json:"last_event_at"`
	LastEventName string    `json:"last_event_name,omitempty"`
	Attempt       int       `json:"attempt,omitempty"`
}

// RetryView is one row of the dashboard's "retries" table.
type RetryView struct {
	IssueID    string    `json:"issue_id"`
	Identifier string    `json:"identifier"`
	Attempt    int       `json:"attempt"`
	DueAt      time.Time `json:"due_at"`
	Error      string    `json:"error,omitempty"`
}

// SlotsView reports concurrency usage at the moment of capture.
type SlotsView struct {
	GlobalMax    int            `json:"global_max"`
	GlobalUsed   int            `json:"global_used"`
	PerStateMax  map[string]int `json:"per_state_max,omitempty"`
	PerStateUsed map[string]int `json:"per_state_used,omitempty"`
}
