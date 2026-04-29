// Package store implements the file-backed persistence layer for iterion runs.
// It manages the lifecycle of runs, their events, artifacts, and human
// interactions using a local filesystem layout:
//
//	runs/<run_id>/run.json
//	runs/<run_id>/events.jsonl
//	runs/<run_id>/artifacts/<node>/<version>.json
//	runs/<run_id>/interactions/<interaction_id>.json
package store

import "time"

// ---------------------------------------------------------------------------
// Event — timestamped fact emitted by the runtime
// ---------------------------------------------------------------------------

// EventType enumerates the minimum events to persist per the V4 plan.
type EventType string

const (
	EventRunStarted           EventType = "run_started"
	EventBranchStarted        EventType = "branch_started"
	EventBranchFinished       EventType = "branch_finished"
	EventNodeStarted          EventType = "node_started"
	EventLLMRequest           EventType = "llm_request"
	EventLLMPrompt            EventType = "llm_prompt"
	EventLLMRetry             EventType = "llm_retry"
	EventNodeRecovery         EventType = "node_recovery"
	EventLLMStepFinished      EventType = "llm_step_finished"
	EventToolCalled           EventType = "tool_called"
	EventToolError            EventType = "tool_error"
	EventArtifactWritten      EventType = "artifact_written"
	EventHumanInputRequested  EventType = "human_input_requested"
	EventRunPaused            EventType = "run_paused"
	EventHumanAnswersRecorded EventType = "human_answers_recorded"
	EventRunResumed           EventType = "run_resumed"
	EventJoinReady            EventType = "join_ready"
	EventNodeFinished         EventType = "node_finished"
	EventEdgeSelected         EventType = "edge_selected"
	EventBudgetWarning        EventType = "budget_warning"
	EventBudgetExceeded       EventType = "budget_exceeded"
	EventRunFinished          EventType = "run_finished"
	EventRunFailed            EventType = "run_failed"
	EventRunCancelled         EventType = "run_cancelled"
	EventDelegateStarted      EventType = "delegate_started"
	EventDelegateFinished     EventType = "delegate_finished"
	EventDelegateError        EventType = "delegate_error"
	EventDelegateRetry        EventType = "delegate_retry"
)

// Event is a single timestamped fact persisted in events.jsonl.
// The Data field carries event-specific payload; its concrete shape
// depends on Type.
type Event struct {
	Seq       int64                  `json:"seq"`       // monotonic sequence within the run
	Timestamp time.Time              `json:"timestamp"` // wall-clock time
	Type      EventType              `json:"type"`
	RunID     string                 `json:"run_id"`
	BranchID  string                 `json:"branch_id,omitempty"`
	NodeID    string                 `json:"node_id,omitempty"`
	Data      map[string]interface{} `json:"data,omitempty"`
}
