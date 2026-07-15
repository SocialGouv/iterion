package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The pipeline board is a SINGLE global execution projection: one card per
// ROOT pipeline (a run with no parent), with every descendant folded into
// its root card as aggregate progress + a flat list of pending human
// reviews. It is a read model over the runtime + native tracker, not a
// second mutable store — cards are positioned by persisted run state, so
// there is no drag-and-drop. See docs/native-tracker.md + ADR-073.
const (
	pipelineColumnDraft      = "draft"
	pipelineColumnTodo       = "todo"
	pipelineColumnInProgress = "in_progress"
	pipelineColumnDone       = "done"

	pipelineTreeMaxDepth = 20
	pipelineTreeMaxCards = 500

	// pipelineFinalAnswerField mirrors notify.DefaultAnswerField — the
	// artifact-data key a finished run's "output" is read from. Duplicated
	// (not imported) to keep pkg/server off pkg/notify for one constant.
	pipelineFinalAnswerField = "final_answer"
	// pipelineOutputMaxLen bounds the DONE card's output string so a
	// verbose artifact can't bloat the board payload.
	pipelineOutputMaxLen = 1200
	// pipelineArtifactProbeCap bounds how many nodes the output fallback
	// probes for the latest artifact on a finished run.
	pipelineArtifactProbeCap = 24
)

// PipelineBoardColumn is one of the four fixed lanes. Unlike the previous
// per-bot board there are no derived interaction columns — human reviews
// live inside the IN_PROGRESS card that blocks on them.
type PipelineBoardColumn struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Kind  string `json:"kind"`
}

// pipelineColumns is the fixed, client-order column set. Draft holds
// not-yet-ready tickets AND tickets whose last run failed (with a Failed
// flag); the operator drags a ticket Draft→Todo to mark it ready, then the
// local launch loop starts it when a concurrency slot frees.
func pipelineColumns() []PipelineBoardColumn {
	return []PipelineBoardColumn{
		{ID: pipelineColumnDraft, Title: "Draft", Kind: "draft"},
		{ID: pipelineColumnTodo, Title: "Todo", Kind: "todo"},
		{ID: pipelineColumnInProgress, Title: "In progress", Kind: "in_progress"},
		{ID: pipelineColumnDone, Title: "Done", Kind: "done"},
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

	// Run identity (empty for a not-yet-launched task card).
	RunID        string          `json:"run_id,omitempty"`
	WorkflowName string          `json:"workflow_name,omitempty"`
	BotID        string          `json:"bot_id,omitempty"`
	Status       store.RunStatus `json:"status,omitempty"`
	Error        string          `json:"error,omitempty"`
	// Failed is true when the card sits in Draft because its run failed /
	// was cancelled (as opposed to a not-yet-ready draft ticket). The UI
	// renders a "failed" badge so the operator can fix and re-drag to Todo.
	Failed bool `json:"failed,omitempty"`
	// Ready reflects whether a task-backed card's ticket is in a
	// launch-eligible (ready) state — used by the UI to place run-less
	// tasks in Todo vs Draft and to drive drag targets.
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

type pipelineBoardTaskRequest struct {
	// Bot is the bot the created task runs as. Required — the board is
	// global, so the bot comes from the request body (not a URL path).
	Bot      string            `json:"bot"`
	Title    string            `json:"title"`
	Body     string            `json:"body,omitempty"`
	Labels   []string          `json:"labels,omitempty"`
	Priority int               `json:"priority,omitempty"`
	BotArgs  map[string]string `json:"bot_args,omitempty"`
	Start    bool              `json:"start,omitempty"`
}

func (s *Server) registerPipelineBoardRoutes() {
	s.mux.Handle("GET /api/v1/pipeline-board", s.requireAuth(http.HandlerFunc(s.handlePipelineBoard)))
	s.mux.Handle("POST /api/v1/pipeline-board/tasks", s.requireAuth(http.HandlerFunc(s.handlePipelineBoardTaskCreate)))
	s.mux.Handle("POST /api/v1/pipeline-board/tasks/{id}/ready", s.requireAuth(http.HandlerFunc(s.handlePipelineBoardTaskReady)))
	s.mux.Handle("PATCH /api/v1/pipeline-board/tasks/{id}", s.requireAuth(http.HandlerFunc(s.handlePipelineBoardTaskUpdate)))
}

func (s *Server) resolvePipelineBoardStore(r *http.Request) (native.BoardStore, error) {
	// Cloud requests must never fall back to a process-wide local store: the
	// board is selected from the authenticated active team on every request.
	if strings.EqualFold(s.cfg.Mode, "cloud") {
		if s.cfg.CloudBoardFor == nil {
			return nil, nil
		}
		return s.cloudBoardResolve(r)
	}
	if s.cfg.NativeTrackerStore != nil {
		return s.cfg.NativeTrackerStore, nil
	}
	if s.cfg.CloudBoardFor != nil {
		return s.cloudBoardResolve(r)
	}
	return nil, nil
}

func (s *Server) handlePipelineBoard(w http.ResponseWriter, r *http.Request) {
	boardStore, err := s.resolvePipelineBoardStore(r)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "pipeline board: resolve store: %v", err)
		return
	}
	if boardStore == nil {
		s.httpErrorFor(w, r, http.StatusNotFound, "pipeline board: native tracker is not available")
		return
	}
	s.stateMu.RLock()
	runs := s.runs
	s.stateMu.RUnlock()
	projection, err := s.buildPipelineBoard(r.Context(), boardStore, runs)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "pipeline board: %v", err)
		return
	}
	s.writeJSONFor(w, r, projection)
}

func (s *Server) handlePipelineBoardTaskCreate(w http.ResponseWriter, r *http.Request) {
	if !s.requireSafeOrigin(w, r) {
		return
	}
	boardStore, err := s.resolvePipelineBoardStore(r)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "pipeline board: resolve store: %v", err)
		return
	}
	if boardStore == nil {
		s.httpErrorFor(w, r, http.StatusNotFound, "pipeline board: native tracker is not available")
		return
	}
	var req pipelineBoardTaskRequest
	if err := readJSON(r, &req); err != nil {
		s.httpErrorFor(w, r, http.StatusBadRequest, "pipeline board task: invalid request: %v", err)
		return
	}
	req.Title = strings.TrimSpace(req.Title)
	req.Bot = strings.TrimSpace(req.Bot)
	if req.Title == "" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "pipeline board task: title is required")
		return
	}
	if req.Bot == "" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "pipeline board task: bot is required")
		return
	}
	entry, found, err := s.findBot(req.Bot)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "pipeline board task: discover bot: %v", err)
		return
	}
	if !found {
		s.httpErrorFor(w, r, http.StatusNotFound, "pipeline board task: bot %q not found", req.Bot)
		return
	}
	if req.Start && !entry.Enabled {
		s.httpErrorFor(w, r, http.StatusConflict, "pipeline board task: bot %q is disabled", entry.Name)
		return
	}
	board := boardStore.Board()
	if board == nil || len(board.States) == 0 {
		s.httpErrorFor(w, r, http.StatusConflict, "pipeline board task: native board has no states")
		return
	}
	state := board.States[0].Name
	if req.Start {
		state = ""
		for _, candidate := range board.States {
			if candidate.Eligible && !candidate.Terminal {
				state = candidate.Name
				break
			}
		}
		if state == "" {
			s.httpErrorFor(w, r, http.StatusConflict, "pipeline board task: native board has no dispatch-eligible state")
			return
		}
	}
	issue, err := boardStore.Create(native.Issue{
		Title:    uniquePipelineTitle(boardStore, req.Title),
		Body:     strings.TrimSpace(req.Body),
		State:    state,
		Labels:   append([]string(nil), req.Labels...),
		Priority: req.Priority,
		Bot:      entry.Name,
		BotArgs:  cloneStringMap(req.BotArgs),
	})
	if err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "pipeline board task: create: %v", err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	s.reflectAllowedOrigin(w, r)
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(issue)
}

// uniquePipelineTitle keeps board card titles distinct: if `desired` is
// already a ticket's title, it appends the smallest " N" (N≥2) that is free
// ("Episode" → "Episode 2" → "Episode 3"). Best-effort — a list error just
// returns the desired title unchanged.
func uniquePipelineTitle(boardStore native.BoardStore, desired string) string {
	existing, err := boardStore.List(native.ListFilter{})
	if err != nil {
		return desired
	}
	taken := make(map[string]struct{}, len(existing))
	for _, iss := range existing {
		if iss != nil {
			taken[iss.Title] = struct{}{}
		}
	}
	if _, clash := taken[desired]; !clash {
		return desired
	}
	for n := 2; n < 100000; n++ {
		candidate := fmt.Sprintf("%s %d", desired, n)
		if _, clash := taken[candidate]; !clash {
			return candidate
		}
	}
	return desired
}

func cloneStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

// pipelineBoardUpdateRequest edits a ticket the operator is still preparing
// (a Draft, or a failed ticket before retry). Only non-nil fields are
// applied; the studio form sends the full state it wants to persist.
type pipelineBoardUpdateRequest struct {
	Title    *string            `json:"title,omitempty"`
	Body     *string            `json:"body,omitempty"`
	Labels   *[]string          `json:"labels,omitempty"`
	Priority *int               `json:"priority,omitempty"`
	Bot      *string            `json:"bot,omitempty"`
	BotArgs  *map[string]string `json:"bot_args,omitempty"`
}

// handlePipelineBoardTaskUpdate edits a ticket's fields (title, input,
// bot, …) so Draft tickets stay editable while the operator prepares them.
// It maps onto the native tracker's Update; the frontend only exposes it on
// task-backed cards that aren't executing.
func (s *Server) handlePipelineBoardTaskUpdate(w http.ResponseWriter, r *http.Request) {
	if !s.requireSafeOrigin(w, r) {
		return
	}
	boardStore, err := s.resolvePipelineBoardStore(r)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "pipeline board: resolve store: %v", err)
		return
	}
	if boardStore == nil {
		s.httpErrorFor(w, r, http.StatusNotFound, "pipeline board: native tracker is not available")
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "pipeline board update: missing task id")
		return
	}
	var req pipelineBoardUpdateRequest
	if err := readJSON(r, &req); err != nil {
		s.httpErrorFor(w, r, http.StatusBadRequest, "pipeline board update: invalid request: %v", err)
		return
	}
	patch := native.Patch{
		Body:     req.Body,
		Labels:   req.Labels,
		Priority: req.Priority,
		BotArgs:  req.BotArgs,
	}
	if req.Title != nil {
		title := strings.TrimSpace(*req.Title)
		if title == "" {
			s.httpErrorFor(w, r, http.StatusBadRequest, "pipeline board update: title cannot be empty")
			return
		}
		patch.Title = &title
	}
	if req.Bot != nil {
		bot := strings.TrimSpace(*req.Bot)
		if bot != "" {
			if _, found, ferr := s.findBot(bot); ferr != nil {
				s.httpErrorFor(w, r, http.StatusInternalServerError, "pipeline board update: discover bot: %v", ferr)
				return
			} else if !found {
				s.httpErrorFor(w, r, http.StatusNotFound, "pipeline board update: bot %q not found", bot)
				return
			}
		}
		patch.Bot = &bot
	}
	issue, err := boardStore.Update(id, patch)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "pipeline board update: %v", err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	s.reflectAllowedOrigin(w, r)
	_ = json.NewEncoder(w).Encode(issue)
}

type pipelineBoardReadyRequest struct {
	// Ready true drags the ticket Draft→Todo (StateReady, eligible for the
	// launch loop); false drags it Todo→Draft (StateInbox).
	Ready bool `json:"ready"`
}

// handlePipelineBoardTaskReady flags a native ticket ready (or back to
// draft) — the backend of the board's Draft↔Todo drag. A ready ticket is
// launched by the admission loop when a concurrency slot frees.
func (s *Server) handlePipelineBoardTaskReady(w http.ResponseWriter, r *http.Request) {
	if !s.requireSafeOrigin(w, r) {
		return
	}
	boardStore, err := s.resolvePipelineBoardStore(r)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "pipeline board: resolve store: %v", err)
		return
	}
	if boardStore == nil {
		s.httpErrorFor(w, r, http.StatusNotFound, "pipeline board: native tracker is not available")
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "pipeline board ready: missing task id")
		return
	}
	var req pipelineBoardReadyRequest
	if err := readJSON(r, &req); err != nil {
		s.httpErrorFor(w, r, http.StatusBadRequest, "pipeline board ready: invalid request: %v", err)
		return
	}
	target := native.StateInbox
	if req.Ready {
		target = native.StateReady
	}
	issue, err := boardStore.SetState(id, target)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "pipeline board ready: set state: %v", err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	s.reflectAllowedOrigin(w, r)
	_ = json.NewEncoder(w).Encode(issue)
}

// ---------------------------------------------------------------------------
// Projection
// ---------------------------------------------------------------------------

type pipelineProjectionBuilder struct {
	ctx            context.Context
	rs             store.RunStore
	runs           map[string]*store.Run
	children       map[string][]*store.Run
	terminalStates map[string]struct{}
	includedRuns   map[string]struct{}
	issueOwnedRuns map[string]struct{}
	nodeCountCache map[string]int
	queuePositions map[string]int
	cards          []PipelineBoardCard

	cardLimitReached  bool
	depthLimitReached bool
	cycleDetected     bool
}

func (s *Server) buildPipelineBoard(ctx context.Context, boardStore native.BoardStore, runs *runview.Service) (PipelineBoardResponse, error) {
	response := PipelineBoardResponse{
		Columns:     pipelineColumns(),
		GeneratedAt: time.Now().UTC(),
	}
	if runs != nil {
		response.Concurrency = runs.PipelineConcurrency()
	}
	builder := &pipelineProjectionBuilder{
		ctx:            ctx,
		runs:           map[string]*store.Run{},
		children:       map[string][]*store.Run{},
		terminalStates: map[string]struct{}{},
		includedRuns:   map[string]struct{}{},
		issueOwnedRuns: map[string]struct{}{},
		nodeCountCache: map[string]int{},
		queuePositions: map[string]int{},
	}
	if runs != nil {
		builder.rs = runs.RunStore()
	}
	if board := boardStore.Board(); board != nil {
		for _, state := range board.States {
			if state.Terminal {
				builder.terminalStates[state.Name] = struct{}{}
			}
		}
	}

	allIssues, err := boardStore.List(native.ListFilter{})
	if err != nil {
		return PipelineBoardResponse{}, fmt.Errorf("list native tasks: %w", err)
	}
	// Global board: keep every issue that names a bot (any bot). Issues
	// with no bot belong to the shared backlog (/board), not to pipelines.
	issues := make([]*native.Issue, 0, len(allIssues))
	for _, issue := range allIssues {
		if issue != nil && strings.TrimSpace(issue.Bot) != "" {
			issues = append(issues, issue)
		}
	}

	if runs != nil {
		records, listErr := runs.ListRunRecordsCtx(ctx, runview.ListFilter{})
		if listErr != nil {
			return PipelineBoardResponse{}, fmt.Errorf("list runs: %w", listErr)
		}
		for _, run := range records {
			builder.runs[run.ID] = run
		}
		builder.indexChildren()
	}
	builder.indexIssueOwnedRuns(issues)
	builder.indexQueuePositions()

	// Issue-backed roots first (stable: priority desc, then created asc).
	sort.SliceStable(issues, func(i, j int) bool {
		if issues[i].Priority != issues[j].Priority {
			return issues[i].Priority > issues[j].Priority
		}
		return issues[i].CreatedAt.Before(issues[j].CreatedAt)
	})
	for _, issue := range issues {
		root := builder.currentRunForIssue(issue)
		if root == nil {
			builder.addTaskCard(issue)
			continue
		}
		builder.addRootCard(root, issue)
	}

	// Standalone roots: manual/API/scheduled/queued runs with no native
	// issue. Only top-level runs (parent absent or dangling) become cards;
	// a child belongs to the root that spawned it, even when the child runs
	// a different bot.
	standalone := make([]*store.Run, 0)
	for _, run := range builder.runs {
		if _, owned := builder.issueOwnedRuns[run.ID]; owned {
			continue
		}
		parentID := pipelineParentRunID(run)
		if parentID != "" && builder.runs[parentID] != nil {
			continue
		}
		standalone = append(standalone, run)
	}
	sort.SliceStable(standalone, func(i, j int) bool {
		if standalone[i].CreatedAt.Equal(standalone[j].CreatedAt) {
			return standalone[i].ID < standalone[j].ID
		}
		return standalone[i].CreatedAt.After(standalone[j].CreatedAt)
	})
	for _, run := range standalone {
		builder.addRootCard(run, nil)
	}

	response.Cards = builder.cards
	if response.Cards == nil {
		response.Cards = []PipelineBoardCard{}
	}
	if builder.cardLimitReached {
		appendPipelineTopologyError(&response, fmt.Sprintf("board truncated after %d cards", pipelineTreeMaxCards))
	}
	if builder.depthLimitReached {
		appendPipelineTopologyError(&response, fmt.Sprintf("run tree truncated after depth %d", pipelineTreeMaxDepth))
	}
	if builder.cycleDetected {
		appendPipelineTopologyError(&response, "run tree contains a parent/child cycle")
	}
	return response, nil
}

func appendPipelineTopologyError(response *PipelineBoardResponse, message string) {
	if response.TopologyError == "" {
		response.TopologyError = message
		return
	}
	response.TopologyError += "; " + message
}

func (b *pipelineProjectionBuilder) indexChildren() {
	for _, run := range b.runs {
		parentID := pipelineParentRunID(run)
		if parentID != "" && parentID != run.ID {
			b.children[parentID] = append(b.children[parentID], run)
		}
	}
	for parentID := range b.children {
		sort.SliceStable(b.children[parentID], func(i, j int) bool {
			a, c := b.children[parentID][i], b.children[parentID][j]
			if a.CreatedAt.Equal(c.CreatedAt) {
				return a.ID < c.ID
			}
			return a.CreatedAt.Before(c.CreatedAt)
		})
	}
}

func (b *pipelineProjectionBuilder) indexIssueOwnedRuns(issues []*native.Issue) {
	issueIDs := make(map[string]struct{}, len(issues))
	for _, issue := range issues {
		if issue == nil {
			continue
		}
		issueIDs[issue.ID] = struct{}{}
		if issue.LastRunID != "" {
			b.issueOwnedRuns[issue.LastRunID] = struct{}{}
		}
		for _, ref := range issue.Runs {
			if ref.RunID != "" {
				b.issueOwnedRuns[ref.RunID] = struct{}{}
			}
		}
	}
	for _, run := range b.runs {
		if run.Source == nil || run.Source.IssueID == "" {
			continue
		}
		if _, owned := issueIDs[run.Source.IssueID]; owned {
			b.issueOwnedRuns[run.ID] = struct{}{}
		}
	}
}

// indexQueuePositions assigns each queued ROOT run a 1-based position by
// QueuedAt (fallback CreatedAt), decoupled from the in-memory scheduler so
// the number is stable across a restart.
func (b *pipelineProjectionBuilder) indexQueuePositions() {
	queued := make([]*store.Run, 0)
	for _, run := range b.runs {
		if run.Status == store.RunStatusQueued && pipelineParentRunID(run) == "" {
			queued = append(queued, run)
		}
	}
	sort.SliceStable(queued, func(i, j int) bool {
		ti, tj := queuedAt(queued[i]), queuedAt(queued[j])
		if ti.Equal(tj) {
			return queued[i].ID < queued[j].ID
		}
		return ti.Before(tj)
	})
	for i, run := range queued {
		b.queuePositions[run.ID] = i + 1
	}
}

func queuedAt(run *store.Run) time.Time {
	if run.QueuedAt != nil {
		return *run.QueuedAt
	}
	return run.CreatedAt
}

func pipelineParentRunID(run *store.Run) string {
	if run == nil {
		return ""
	}
	if run.ParentRunID != "" {
		return run.ParentRunID
	}
	// Forks created before ParentRunID was populated still carry the exact
	// structural source. Treat it as a read-only compatibility edge.
	return run.ForkedFrom
}

func (b *pipelineProjectionBuilder) currentRunForIssue(issue *native.Issue) *store.Run {
	if issue == nil {
		return nil
	}
	refTime := func(runID string) time.Time {
		for index := len(issue.Runs) - 1; index >= 0; index-- {
			if issue.Runs[index].RunID == runID {
				return issue.Runs[index].At
			}
		}
		return time.Time{}
	}

	// LastRunID is the dispatcher's canonical current-attempt pointer.
	current := b.runs[issue.LastRunID]
	baseline := refTime(issue.LastRunID)
	if current == nil {
		for index := len(issue.Runs) - 1; index >= 0; index-- {
			if candidate := b.runs[issue.Runs[index].RunID]; candidate != nil {
				current = candidate
				baseline = issue.Runs[index].At
				break
			}
		}
	}
	if current != nil && baseline.IsZero() {
		baseline = current.CreatedAt
	}

	// A dispatcher stamps LastRunID at the end of an attempt. During the
	// attempt the authoritative source edge already exists on the run, so
	// prefer a newer, non-terminal run sourced from this issue.
	sourceCandidates := make([]*store.Run, 0, 1)
	for _, run := range b.runs {
		if run.Source != nil && run.Source.IssueID == issue.ID && pipelineParentRunID(run) == "" {
			sourceCandidates = append(sourceCandidates, run)
		}
	}
	sort.SliceStable(sourceCandidates, func(i, j int) bool {
		if sourceCandidates[i].CreatedAt.Equal(sourceCandidates[j].CreatedAt) {
			return sourceCandidates[i].ID < sourceCandidates[j].ID
		}
		return sourceCandidates[i].CreatedAt.Before(sourceCandidates[j].CreatedAt)
	})
	if current == nil && len(sourceCandidates) > 0 {
		return sourceCandidates[len(sourceCandidates)-1]
	}
	for _, candidate := range sourceCandidates {
		if current == nil || candidate.ID == current.ID || candidate.Status.IsTerminal() || !candidate.CreatedAt.After(baseline) {
			continue
		}
		current = candidate
		baseline = candidate.CreatedAt
	}
	return current
}

func (b *pipelineProjectionBuilder) attemptsForIssue(issue *native.Issue, current *store.Run) []PipelineBoardAttempt {
	if issue == nil {
		return nil
	}
	seen := map[string]struct{}{}
	attempts := make([]PipelineBoardAttempt, 0, len(issue.Runs)+1)
	appendAttempt := func(runID string, at time.Time) {
		if runID == "" {
			return
		}
		if _, ok := seen[runID]; ok {
			return
		}
		seen[runID] = struct{}{}
		attempt := PipelineBoardAttempt{RunID: runID}
		if run := b.runs[runID]; run != nil {
			attempt.Status = run.Status
			if at.IsZero() {
				at = run.CreatedAt
			}
		}
		if !at.IsZero() {
			atCopy := at
			attempt.At = &atCopy
		}
		attempts = append(attempts, attempt)
	}
	for _, ref := range issue.Runs {
		appendAttempt(ref.RunID, ref.At)
	}
	appendAttempt(issue.LastRunID, time.Time{})
	if current != nil {
		appendAttempt(current.ID, current.CreatedAt)
	}
	return attempts
}

// addTaskCard emits a card for a native task pinned to a bot that has no
// current run yet. A ticket in a launch-eligible (ready) state sits in
// Todo — the launch loop starts it when a slot frees; otherwise it is a
// Draft the operator prepares and drags to Todo when ready.
func (b *pipelineProjectionBuilder) addTaskCard(issue *native.Issue) {
	if issue == nil || len(b.cards) >= pipelineTreeMaxCards {
		b.cardLimitReached = len(b.cards) >= pipelineTreeMaxCards
		return
	}
	_, terminal := b.terminalStates[issue.State]
	// "Ready" is the specific StateReady the operator drags a ticket into;
	// the launch loop starts exactly those. Other non-terminal states
	// (inbox/backlog/…) are Drafts being prepared.
	ready := issue.State == native.StateReady
	column := pipelineColumnDraft
	switch {
	case terminal:
		column = pipelineColumnDone
	case ready:
		column = pipelineColumnTodo
	}
	b.cards = append(b.cards, PipelineBoardCard{
		ID:         "task:" + issue.ID,
		Kind:       "task",
		ColumnID:   column,
		Title:      issue.Title,
		Body:       issue.Body,
		IssueID:    issue.ID,
		IssueState: issue.State,
		Ready:      ready,
		Labels:     append([]string(nil), issue.Labels...),
		Priority:   issue.Priority,
		BotID:      issue.Bot,
		EntryInput: stringMapToAny(issue.BotArgs),
		CreatedAt:  issue.CreatedAt,
		UpdatedAt:  issue.UpdatedAt,
		Attempts:   b.attemptsForIssue(issue, nil),
	})
}

// addRootCard emits ONE card for a root run, folding its whole descendant
// tree into aggregate progress + pending reviews. issue is non-nil when
// the root is backed by a native task.
func (b *pipelineProjectionBuilder) addRootCard(root *store.Run, issue *native.Issue) {
	if root == nil {
		return
	}
	if _, already := b.includedRuns[root.ID]; already {
		return
	}
	if len(b.cards) >= pipelineTreeMaxCards {
		b.cardLimitReached = true
		return
	}
	b.includedRuns[root.ID] = struct{}{}

	rootExec, rootTotal := b.runProgress(root)
	treeExec, treeTotal, descCount, reviews := b.aggregateTree(root)

	title := strings.TrimSpace(root.Name)
	if title == "" {
		title = humanizePipelineName(root.WorkflowName)
	}
	if issue != nil {
		title = issue.Title
	}

	card := PipelineBoardCard{
		ID:                "run:" + root.ID,
		Kind:              "run",
		ColumnID:          pipelineColumnForRoot(root, reviews),
		Title:             title,
		RunID:             root.ID,
		WorkflowName:      root.WorkflowName,
		BotID:             pipelineRunBotID(root),
		Status:            root.Status,
		Error:             root.Error,
		Failed:            pipelineRunFailed(root.Status),
		ExecutedNodes:     rootExec,
		TotalNodes:        rootTotal,
		TreeExecutedNodes: treeExec,
		TreeTotalNodes:    treeTotal,
		DescendantCount:   descCount,
		PendingReviews:    reviews,
		CreatedAt:         root.CreatedAt,
		UpdatedAt:         root.UpdatedAt,
	}
	if root.Status == store.RunStatusQueued {
		card.QueuePosition = b.queuePositions[root.ID]
		card.EntryInput = cloneAnyMap(root.Inputs)
	} else if len(root.Inputs) > 0 {
		card.EntryInput = cloneAnyMap(root.Inputs)
	}
	if root.Status == store.RunStatusFinished {
		card.Output = pipelineTruncate(b.finalOutput(root), pipelineOutputMaxLen)
	}
	if issue != nil {
		card.IssueID = issue.ID
		card.IssueState = issue.State
		card.Body = issue.Body
		card.Labels = append([]string(nil), issue.Labels...)
		card.Priority = issue.Priority
		card.Attempts = b.attemptsForIssue(issue, root)
		if card.EntryInput == nil {
			card.EntryInput = stringMapToAny(issue.BotArgs)
		}
	}
	b.cards = append(b.cards, card)
}

// aggregateTree walks root ∪ descendants, summing node-weighted progress,
// counting descendants, and collecting every pending human review. It
// reuses the depth / cycle guards; a finished subtree contributes without
// an event scan (see runProgress).
func (b *pipelineProjectionBuilder) aggregateTree(root *store.Run) (treeExec, treeTotal, descCount int, reviews []PipelineBoardPendingReview) {
	visited := map[string]struct{}{}
	var walk func(run *store.Run, depth int)
	walk = func(run *store.Run, depth int) {
		if run == nil {
			return
		}
		if _, seen := visited[run.ID]; seen {
			b.cycleDetected = true
			return
		}
		if depth > pipelineTreeMaxDepth {
			b.depthLimitReached = true
			return
		}
		visited[run.ID] = struct{}{}
		if depth > 0 {
			descCount++
			// A descendant is included in the root's card, so mark it so it
			// never also becomes a standalone root card.
			b.includedRuns[run.ID] = struct{}{}
		}
		exec, total := b.runProgress(run)
		treeExec += exec
		treeTotal += total
		if run.Status == store.RunStatusPausedWaitingHuman && run.Checkpoint != nil && run.Checkpoint.NodeID != "" {
			reviews = append(reviews, PipelineBoardPendingReview{
				RunID:         run.ID,
				WorkflowName:  run.WorkflowName,
				BotID:         pipelineRunBotID(run),
				NodeID:        run.Checkpoint.NodeID,
				InteractionID: run.Checkpoint.InteractionID,
				Questions:     cloneAnyMap(run.Checkpoint.InteractionQuestions),
				Depth:         depth,
			})
		}
		for _, child := range b.children[run.ID] {
			walk(child, depth+1)
		}
	}
	walk(root, 0)
	return
}

// runProgress returns (executed, total) node counts for one run. Finished
// runs clamp to 100% with no event scan; queued runs report 0/total; other
// statuses count distinct node_started events (clamped to total).
func (b *pipelineProjectionBuilder) runProgress(run *store.Run) (executed, total int) {
	total = b.totalNodes(run.FilePath)
	switch run.Status {
	case store.RunStatusFinished:
		return total, total
	case store.RunStatusQueued:
		return 0, total
	default:
		exec := b.executedNodes(run.ID)
		if total > 0 && exec > total {
			exec = total
		}
		return exec, total
	}
}

// executedNodes counts distinct nodes that started for a run (node_started
// fires once per loop iteration, so dedup on node id).
func (b *pipelineProjectionBuilder) executedNodes(runID string) int {
	if b.rs == nil {
		return 0
	}
	seen := map[string]struct{}{}
	_ = b.rs.ScanEvents(b.ctx, runID, func(e *store.Event) bool {
		if e.Type == store.EventNodeStarted && e.NodeID != "" {
			seen[e.NodeID] = struct{}{}
		}
		return true
	})
	return len(seen)
}

// totalNodes compiles the run's workflow (memoized by file path) and
// returns its node count; 0 when the file is absent or fails to compile.
func (b *pipelineProjectionBuilder) totalNodes(filePath string) int {
	if filePath == "" {
		return 0
	}
	if n, ok := b.nodeCountCache[filePath]; ok {
		return n
	}
	var (
		wf  *ir.Workflow
		err error
	)
	if bundle := runview.ResolveBundleFromFilePath(filePath); bundle != nil {
		wf, _, err = runview.CompileBundleWorkflow(filePath, bundle)
	} else {
		wf, _, err = runview.CompileWorkflowWithHash(filePath)
	}
	n := 0
	if err == nil && wf != nil {
		n = len(wf.Nodes)
	}
	b.nodeCountCache[filePath] = n
	return n
}

// finalOutput resolves a finished run's user-facing output: the
// final_answer artifact field (pinned node first, then any artifact node),
// falling back to a compact rendering of the latest-written artifact.
func (b *pipelineProjectionBuilder) finalOutput(run *store.Run) string {
	if b.rs == nil {
		return ""
	}
	if run.CallbackAnswerNode != "" {
		if s := b.answerField(run.ID, run.CallbackAnswerNode); s != "" {
			return s
		}
	}
	for nodeID := range run.ArtifactIndex {
		if s := b.answerField(run.ID, nodeID); s != "" {
			return s
		}
	}
	return b.latestArtifactSummary(run)
}

func (b *pipelineProjectionBuilder) answerField(runID, nodeID string) string {
	art, err := b.rs.LoadLatestArtifact(b.ctx, runID, nodeID)
	if err != nil || art == nil || art.Data == nil {
		return ""
	}
	if raw, ok := art.Data[pipelineFinalAnswerField]; ok {
		if s, ok := raw.(string); ok {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

// latestArtifactSummary probes each artifact-bearing node (bounded),
// picks the most-recently-written artifact, and returns a compact JSON of
// its data — the DONE fallback when no final_answer field exists.
func (b *pipelineProjectionBuilder) latestArtifactSummary(run *store.Run) string {
	var (
		best   *store.Artifact
		probed int
	)
	for nodeID := range run.ArtifactIndex {
		if probed >= pipelineArtifactProbeCap {
			break
		}
		probed++
		art, err := b.rs.LoadLatestArtifact(b.ctx, run.ID, nodeID)
		if err != nil || art == nil || len(art.Data) == 0 {
			continue
		}
		if best == nil || art.WrittenAt.After(best.WrittenAt) {
			best = art
		}
	}
	if best == nil {
		return ""
	}
	encoded, err := json.Marshal(best.Data)
	if err != nil {
		return ""
	}
	return string(encoded)
}

// pipelineColumnForRoot maps a root run to a lane. A tree blocked on a
// human review is IN_PROGRESS (the operator's turn) regardless of the
// root's own transient status.
func pipelineColumnForRoot(root *store.Run, reviews []PipelineBoardPendingReview) string {
	if len(reviews) > 0 {
		return pipelineColumnInProgress
	}
	switch root.Status {
	case store.RunStatusQueued:
		// Waiting for a local concurrency slot — not yet executing.
		return pipelineColumnTodo
	case store.RunStatusFinished:
		return pipelineColumnDone
	case store.RunStatusRunning, store.RunStatusPausedWaitingHuman:
		return pipelineColumnInProgress
	case store.RunStatusFailed, store.RunStatusFailedResumable, store.RunStatusCancelled, store.RunStatusPausedOperator:
		// A failed/cancelled run sends its ticket back to Draft (Failed
		// flag) so the operator can fix it and re-drag it to Todo.
		return pipelineColumnDraft
	default:
		return pipelineColumnInProgress
	}
}

// pipelineRunFailed reports whether a run status lands a card in Draft as a
// failure (as opposed to a not-yet-ready draft ticket).
func pipelineRunFailed(status store.RunStatus) bool {
	switch status {
	case store.RunStatusFailed, store.RunStatusFailedResumable, store.RunStatusCancelled, store.RunStatusPausedOperator:
		return true
	default:
		return false
	}
}

func pipelineRunBotID(run *store.Run) string {
	if run == nil {
		return ""
	}
	if run.BotID != "" {
		return run.BotID
	}
	if run.BundleName != "" {
		return run.BundleName
	}
	if run.BundlePath != "" {
		return strings.TrimSuffix(filepath.Base(strings.TrimRight(run.BundlePath, "/")), ".botz")
	}
	return ""
}

func cloneAnyMap(in map[string]any) map[string]any {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]any, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func stringMapToAny(in map[string]string) map[string]any {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]any, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func pipelineTruncate(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

func humanizePipelineName(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "Pipeline"
	}
	var b strings.Builder
	previousSeparator := true
	for _, r := range value {
		if r == '_' || r == '-' || r == '.' || r == '/' || r == ' ' {
			if b.Len() > 0 && !previousSeparator {
				b.WriteByte(' ')
			}
			previousSeparator = true
			continue
		}
		if previousSeparator {
			b.WriteRune(toUpperRune(r))
		} else {
			b.WriteRune(r)
		}
		previousSeparator = false
	}
	return strings.TrimSpace(b.String())
}

func toUpperRune(r rune) rune {
	if r >= 'a' && r <= 'z' {
		return r - 32
	}
	return r
}
