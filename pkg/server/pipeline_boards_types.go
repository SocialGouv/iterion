package server

import (
	"time"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
)

// PipelineBoardColumn is one of the five fixed lanes. Unlike the previous
// per-bot board there are no derived interaction columns — human reviews
// live inside the IN_PROGRESS card that blocks on them.
type PipelineBoardColumn struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Kind  string `json:"kind"`
}

// pipelineColumns is the fixed, client-order column set. Backlog holds
// not-yet-ready tickets; Failed holds pipelines whose run ended in failure
// (with the reason) until the operator retries them to Todo; the local
// launch loop starts ready tickets when a concurrency slot frees.
func pipelineColumns() []PipelineBoardColumn {
	return []PipelineBoardColumn{
		{ID: pipelineColumnBacklog, Title: "Backlog", Kind: "backlog"},
		{ID: pipelineColumnTodo, Title: "Todo", Kind: "todo"},
		{ID: pipelineColumnInProgress, Title: "In progress", Kind: "in_progress"},
		{ID: pipelineColumnDone, Title: "Done", Kind: "done"},
		{ID: pipelineColumnFailed, Title: "Failed", Kind: "failed"},
	}
}

// PipelineBoardPendingReview is one paused human interaction somewhere in
// a root's tree (the root itself or any descendant). The card presents
// these one at a time; each answer targets the exact run_id shown here.
type PipelineBoardPendingReview struct {
	RunID         string         `json:"run_id"`
	WorkflowName  string         `json:"workflow_name,omitempty"`
	BotID         string         `json:"bot_id,omitempty"`
	NodeID        string         `json:"node_id,omitempty"`
	InteractionID string         `json:"interaction_id,omitempty"`
	Questions     map[string]any `json:"questions,omitempty"`
	// Depth is 0 for the root's own pause, >0 for a descendant's.
	Depth int `json:"depth"`
}

// PipelineBoardAttempt is one dispatcher attempt associated with a native
// task-backed root. Status is enriched from the run store when the run
// still exists.
type PipelineBoardAttempt struct {
	RunID  string          `json:"run_id"`
	Status store.RunStatus `json:"status,omitempty"`
	At     *time.Time      `json:"at,omitempty"`
}

// PipelineBoardCard is the read model the studio polls: one per root
// pipeline (or per not-yet-launched native task). Descendants are NOT
// separate cards — their progress and pending reviews are folded here.
type PipelineBoardCard struct {
	ID       string `json:"id"`
	Kind     string `json:"kind"` // "run" | "task"
	ColumnID string `json:"column_id"`
	Title    string `json:"title"`
	Body     string `json:"body,omitempty"`

	// Native task provenance (present when the root is backed by a board issue).
	IssueID    string   `json:"issue_id,omitempty"`
	IssueState string   `json:"issue_state,omitempty"`
	Labels     []string `json:"labels,omitempty"`
	Priority   int      `json:"priority,omitempty"`
	// External is the backing issue's forge linkage — the card's repo
	// identity, powering the repo-first scope on /pipelines.
	External *native.ExternalRef `json:"external,omitempty"`

	// Run identity (empty for a not-yet-launched task card).
	RunID        string          `json:"run_id,omitempty"`
	WorkflowName string          `json:"workflow_name,omitempty"`
	BotID        string          `json:"bot_id,omitempty"`
	Status       store.RunStatus `json:"status,omitempty"`
	Error        string          `json:"error,omitempty"`
	// Failed is true when the card sits in the FAILED lane because its run
	// failed / was cancelled. The UI shows the Error as the reason and
	// offers a Retry (move back to Todo) on ticket-backed cards.
	Failed bool `json:"failed,omitempty"`
	// Ready reflects whether a task-backed card's ticket is in a
	// launch-eligible (ready) state — used by the UI to place run-less
	// tasks in Todo vs Backlog and to enable the move buttons.
	Ready bool `json:"ready,omitempty"`

	// TODO — the pipeline's entry input (launch vars / task bot-args).
	EntryInput map[string]any `json:"entry_input,omitempty"`
	// QueuePosition is the 1-based place in the local concurrency queue
	// (queued roots only); 0 otherwise.
	QueuePosition int `json:"queue_position,omitempty"`

	// IN_PROGRESS — node-progress for the root and the whole tree
	// (executed / total). Tree_* is node-weighted over root ∪ descendants.
	ExecutedNodes     int `json:"executed_nodes"`
	TotalNodes        int `json:"total_nodes"`
	TreeExecutedNodes int `json:"tree_executed_nodes"`
	TreeTotalNodes    int `json:"tree_total_nodes"`
	// DescendantCount is how many child runs the tree folded into this card.
	DescendantCount int `json:"descendant_count,omitempty"`
	// TreeRunIDs is the flattened run tree — the root first, then every
	// descendant in walk order. The studio fans out over these to aggregate
	// the whole pipeline's produced elements (a sub-bot's outputs surface on
	// the root's card, not just the root's own).
	TreeRunIDs []string `json:"tree_run_ids,omitempty"`
	// PendingReviews are the human gates the tree is currently blocked on
	// (root + descendants), presented one at a time by the card.
	PendingReviews []PipelineBoardPendingReview `json:"pending_reviews,omitempty"`

	// DONE — the pipeline's output (final_answer, else latest artifact).
	Output string `json:"output,omitempty"`

	Attempts  []PipelineBoardAttempt `json:"attempts,omitempty"`
	CreatedAt time.Time              `json:"created_at"`
	UpdatedAt time.Time              `json:"updated_at"`
}

// PipelineBoardResponse is the aggregate global read model.
type PipelineBoardResponse struct {
	Columns       []PipelineBoardColumn             `json:"columns"`
	Cards         []PipelineBoardCard               `json:"cards"`
	Concurrency   runview.PipelineConcurrencyStatus `json:"concurrency"`
	GeneratedAt   time.Time                         `json:"generated_at"`
	TopologyError string                            `json:"topology_error,omitempty"`
}
