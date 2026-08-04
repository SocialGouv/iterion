package server

import (
	"time"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
)

// PipelineBoardColumn is one of the four fixed lanes. Unlike the previous
// per-bot board there are no derived interaction columns — human reviews
// live inside the IN_PROGRESS card that blocks on them.
type PipelineBoardColumn struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Kind  string `json:"kind"`
}

// pipelineColumns is the fixed, client-order column set. Opened holds every
// not-yet-running ticket — a per-card `ready` flag marks the launch-eligible
// ones (the studio badges + filters them), the rest are still being
// prepared; the local launch loop starts ready tickets when a concurrency
// slot frees. In progress holds running / awaiting-review runs. Needs
// attention holds runs that died mid-flight and hold their slot until the
// operator retries, resumes or closes them. Closed holds every pipeline
// that reached an end: finished, or cancelled by the operator — a per-card
// success/failed outcome distinguishes them (surfaced as the Closed lane's
// filter).
func pipelineColumns() []PipelineBoardColumn {
	return []PipelineBoardColumn{
		{ID: pipelineColumnOpened, Title: "Opened", Kind: "opened"},
		{ID: pipelineColumnInProgress, Title: "In progress", Kind: "in_progress"},
		{ID: pipelineColumnNeedsAttention, Title: "Needs attention", Kind: "needs_attention"},
		{ID: pipelineColumnClosed, Title: "Closed", Kind: "closed"},
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
	// Instructions is the resolved `instructions:` prompt of the paused
	// human node — the author's operator-facing question. Questions above
	// carries the node's INBOUND data, which the schema-driven form does
	// not render (it renders the node's output fields), so for any bot
	// that puts its whole question in `instructions:` this string is the
	// only readable content on the card. Empty when the node declares no
	// instructions prompt.
	Instructions string `json:"instructions,omitempty"`
	// UpdatedAt is when this exact pending turn joined the operator queue.
	// Review gates reuse their interaction ID across dialogue turns, so the
	// timestamp also versions the form on the Studio side.
	UpdatedAt time.Time `json:"updated_at"`
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

// PipelineBoardChildRef is one spawned child ticket under a planner parent.
type PipelineBoardChildRef struct {
	IssueID string `json:"issue_id"`
	Title   string `json:"title,omitempty"`
	State   string `json:"state,omitempty"`
	BotID   string `json:"bot_id,omitempty"`
	// CardID is the pipeline card id (task:… or run:…) when the child is
	// projected on the board; empty if the child has no bot card yet.
	CardID string `json:"card_id,omitempty"`
}

// PipelineBoardChildrenSummary is the compact face for a plan group.
type PipelineBoardChildrenSummary struct {
	Total      int `json:"total"`
	Ready      int `json:"ready"`
	InProgress int `json:"in_progress"`
	Done       int `json:"done"`
	Failed     int `json:"failed"`
	// Open is opened-but-not-ready (drafts / waiting_deps).
	Open int `json:"open"`
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
	// provenance when the ticket was imported from a forge.
	External *native.ExternalRef `json:"external,omitempty"`

	// Hard-dependency graph (ticket roots only — not sub-bot runs).
	// Blockers are resolved server-side; OpenBlockerCount > 0 means the
	// launch loop will refuse even if Ready is true.
	Blockers            []native.BlockerInfo `json:"blockers,omitempty"`
	OpenBlockerCount    int                  `json:"open_blocker_count,omitempty"`
	LaunchBlockedReason string               `json:"launch_blocked_reason,omitempty"`
	// Blocking is the reverse index: tickets that list this one as a blocker.
	Blocking []native.BlockingInfo `json:"blocking,omitempty"`

	// Planner provenance (distinct from hard blockers). ParentIssueID is the
	// ticket that spawned this one; Children are reverse edges on the board.
	ParentIssueID string                  `json:"parent_issue_id,omitempty"`
	ParentTitle   string                  `json:"parent_title,omitempty"`
	Children      []PipelineBoardChildRef `json:"children,omitempty"`
	// ChildrenSummary aggregates child ticket statuses for the plan group face.
	ChildrenSummary *PipelineBoardChildrenSummary `json:"children_summary,omitempty"`
	// Role is planner|producer when known (bot_args.role or inferred).
	Role string `json:"role,omitempty"`

	// Run identity (empty for a not-yet-launched task card).
	RunID        string          `json:"run_id,omitempty"`
	WorkflowName string          `json:"workflow_name,omitempty"`
	BotID        string          `json:"bot_id,omitempty"`
	Status       store.RunStatus `json:"status,omitempty"`
	Error        string          `json:"error,omitempty"`
	// Failed is true when the card's run failed / was cancelled, as opposed
	// to finishing successfully. It spans two lanes: needs_attention (failed
	// mid-flight) and closed (cancelled by the operator). The UI shows the
	// Error as the reason and offers a Retry on ticket-backed cards.
	Failed bool `json:"failed,omitempty"`
	// ReservesSlot is true when this card is holding one of the local
	// pipeline concurrency slots open for its own restart — no process is
	// running, but nothing else may take its place until the operator
	// retries, resumes or closes it. Only ever set on needs_attention cards;
	// see pipelineLaneForRoot for the exact predicate.
	ReservesSlot bool `json:"reserves_slot,omitempty"`
	// Ready reflects whether a task-backed card's ticket is in a
	// launch-eligible (ready) state — used by the UI to place run-less
	// tasks in Ready vs Backlog and to enable the move buttons.
	Ready bool `json:"ready,omitempty"`

	// Ready lane — the pipeline's entry input (launch vars / task bot-args).
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

	// HiddenClosedCount / HiddenClosedBefore report the `?since=` filter's
	// effect: how many CLOSED cards (finished/failed/cancelled runs, or
	// terminal-state task cards) were pruned because they last changed before
	// the cutoff, and what the resolved cutoff was. Zero/absent when no filter
	// was applied. Surfaced so the pruning is never silent (the studio can
	// offer "show N older pipelines").
	HiddenClosedCount  int        `json:"hidden_closed_count,omitempty"`
	HiddenClosedBefore *time.Time `json:"hidden_closed_before,omitempty"`
}
