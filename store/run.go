package store

import "time"

// ---------------------------------------------------------------------------
// RunStatus — lifecycle state of a run
// ---------------------------------------------------------------------------

// RunStatus represents the current state of a run.
type RunStatus string

const (
	RunStatusRunning            RunStatus = "running"
	RunStatusPausedWaitingHuman RunStatus = "paused_waiting_human"
	RunStatusFinished           RunStatus = "finished"
	RunStatusFailed             RunStatus = "failed"
)

// ---------------------------------------------------------------------------
// Run — top-level run metadata persisted in run.json
// ---------------------------------------------------------------------------

// Run is the top-level metadata for a single workflow invocation.
type Run struct {
	ID           string                 `json:"id"`
	WorkflowName string                 `json:"workflow_name"`
	Status       RunStatus              `json:"status"`
	Inputs       map[string]interface{} `json:"inputs,omitempty"`
	CreatedAt    time.Time              `json:"created_at"`
	UpdatedAt    time.Time              `json:"updated_at"`
	FinishedAt   *time.Time             `json:"finished_at,omitempty"`
	Error        string                 `json:"error,omitempty"`
}

// ---------------------------------------------------------------------------
// Artifact — structured output of a node
// ---------------------------------------------------------------------------

// Artifact is a versioned output persisted under artifacts/<node>/<version>.json.
type Artifact struct {
	RunID    string                 `json:"run_id"`
	NodeID   string                 `json:"node_id"`
	Version  int                    `json:"version"`
	Data     map[string]interface{} `json:"data"`
	WrittenAt time.Time             `json:"written_at"`
}

// ---------------------------------------------------------------------------
// Interaction — human input/output exchange
// ---------------------------------------------------------------------------

// Interaction records a human pause/resume exchange.
type Interaction struct {
	ID          string                 `json:"id"`
	RunID       string                 `json:"run_id"`
	NodeID      string                 `json:"node_id"`
	RequestedAt time.Time              `json:"requested_at"`
	AnsweredAt  *time.Time             `json:"answered_at,omitempty"`
	Questions   map[string]interface{} `json:"questions,omitempty"`
	Answers     map[string]interface{} `json:"answers,omitempty"`
}
