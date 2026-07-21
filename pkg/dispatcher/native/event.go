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
	EvtIssueComment  EventType = "issue_comment_added"
	// EvtIssueBlockersUpdated is emitted when an issue's blockers list changes
	// (create-with-blockers, Update patch). Payload: {blockers: []string}.
	EvtIssueBlockersUpdated EventType = "issue_blockers_updated"
	// EvtIssueUnblocked is emitted when a waiting_deps ticket is auto-
	// promoted because its last hard blocker reached StateDone. Payload:
	// {from, to, closed_blocker}.
	EvtIssueUnblocked EventType = "issue_unblocked"
	EvtBoardUpdated   EventType = "board_updated"
	// Label-vocabulary management events, emitted once per touched
	// issue. The payload carries `{from, to}` for rename/merge and
	// `{label}` for delete.
	EvtLabelRename EventType = "label_rename"
	EvtLabelMerge  EventType = "label_merge"
	EvtLabelDelete EventType = "label_delete"
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
