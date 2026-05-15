package conductor

import (
	"context"
	"time"

	"github.com/SocialGouv/iterion/pkg/conductor/tracker"
)

// state is the in-memory bookkeeping the conductor's actor goroutine
// owns. Outside callers must reach it through typed commands posted on
// Conductor.cmds — never read these maps directly.
//
// "Claimed" is not a separate field — the union (running ∪ retries)
// is the live claim set. Dispatch checks both maps before picking up
// a candidate.
type state struct {
	running      map[string]*runningEntry
	retries      map[string]*retryEntry
	slotsByState map[string]int // running count per workflow state
}

func newState() *state {
	return &state{
		running:      map[string]*runningEntry{},
		retries:      map[string]*retryEntry{},
		slotsByState: map[string]int{},
	}
}

// isClaimed reports whether the actor is currently treating issueID
// as "ours" — either in flight or queued for retry.
func (s *state) isClaimed(issueID string) bool {
	if _, ok := s.running[issueID]; ok {
		return true
	}
	_, ok := s.retries[issueID]
	return ok
}

// runningEntry tracks one in-flight dispatch. It outlives the actor's
// view of the goroutine: the goroutine writes LastEventAt via cmdEvent.
type runningEntry struct {
	IssueID       string
	Identifier    string
	RunID         string
	WorkflowState string
	WorkspacePath string
	StartedAt     time.Time
	LastEventAt   time.Time
	LastEventName string
	Attempt       int
	Cancel        context.CancelFunc

	// issueSnapshot is the tracker.Issue snapshot used to render
	// dispatch.vars. Kept so the conductor can render a fresh prompt
	// on retry without re-fetching from the tracker.
	issueSnapshot tracker.Issue
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
