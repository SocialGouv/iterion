package native

import "time"

// EventType enumerates the kinds of events the native tracker emits.
type EventType string

const (
	EvtIssueCreated  EventType = "issue_created"
	EvtIssueUpdated  EventType = "issue_updated"
	EvtIssueState    EventType = "issue_state_changed"
	EvtIssueDeleted  EventType = "issue_deleted"
	EvtIssueClaimed  EventType = "issue_claimed"
	EvtIssueReleased EventType = "issue_released"
	EvtIssueLastRun  EventType = "issue_last_run_updated"
	EvtBoardUpdated  EventType = "board_updated"
)

// Event is the audit-log record persisted to events.jsonl. Seq is a
// monotonic per-tracker counter; Timestamp is UTC.
type Event struct {
	Seq       int64          `json:"seq"`
	Timestamp time.Time      `json:"timestamp"`
	Type      EventType      `json:"type"`
	IssueID   string         `json:"issue_id,omitempty"`
	Payload   map[string]any `json:"payload,omitempty"`
}
