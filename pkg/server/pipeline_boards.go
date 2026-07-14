package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/SocialGouv/iterion/pkg/botregistry"
	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
)

const (
	pipelineColumnTodo       = "todo"
	pipelineColumnRunning    = "running"
	pipelineColumnOtherInput = "interaction:other"
	pipelineColumnAttention  = "attention"
	pipelineColumnDone       = "done"

	pipelineTreeMaxDepth = 20
	pipelineTreeMaxCards = 500
)

// PipelineBoardIdentity is the stable identity of the derived board exposed
// by the Studio. V1 deliberately keys it by the bot registry entry instead of
// creating a second mutable board store: the native board remains the
// dispatcher's backlog, while this surface is a runtime projection.
type PipelineBoardIdentity struct {
	ID          string `json:"id"`
	BotID       string `json:"bot_id"`
	DisplayName string `json:"display_name"`
	Icon        string `json:"icon,omitempty"`
	Description string `json:"description,omitempty"`
	Enabled     bool   `json:"enabled"`
}

// PipelineBoardColumn is one derived lane. Interaction columns carry the
// workflow/node pair that selects paused runs; lifecycle columns leave those
// fields empty.
type PipelineBoardColumn struct {
	ID              string `json:"id"`
	Title           string `json:"title"`
	Kind            string `json:"kind"`
	WorkflowName    string `json:"workflow_name,omitempty"`
	NodeID          string `json:"node_id,omitempty"`
	InteractionMode string `json:"interaction_mode,omitempty"`
}

// PipelineBoardAttempt is one dispatcher attempt associated with a native
// task. Status is enriched from the run store when that run still exists.
type PipelineBoardAttempt struct {
	RunID  string          `json:"run_id"`
	Status store.RunStatus `json:"status,omitempty"`
	At     *time.Time      `json:"at,omitempty"`
}

// PipelineBoardCard is intentionally flat: it is the read model the Studio
// polls, not a mirror of either native.Issue or store.Run. A task without a run
// has Kind=task; once launched the root and every descendant are separate run
// cards linked by ParentRunID/Depth.
type PipelineBoardCard struct {
	ID            string                 `json:"id"`
	Kind          string                 `json:"kind"`
	ColumnID      string                 `json:"column_id"`
	Title         string                 `json:"title"`
	Body          string                 `json:"body,omitempty"`
	IssueID       string                 `json:"issue_id,omitempty"`
	IssueState    string                 `json:"issue_state,omitempty"`
	Labels        []string               `json:"labels,omitempty"`
	Priority      int                    `json:"priority,omitempty"`
	RunID         string                 `json:"run_id,omitempty"`
	RootRunID     string                 `json:"root_run_id,omitempty"`
	ParentRunID   string                 `json:"parent_run_id,omitempty"`
	Depth         int                    `json:"depth"`
	WorkflowName  string                 `json:"workflow_name,omitempty"`
	BotID         string                 `json:"bot_id,omitempty"`
	Status        store.RunStatus        `json:"status,omitempty"`
	Error         string                 `json:"error,omitempty"`
	NodeID        string                 `json:"node_id,omitempty"`
	InteractionID string                 `json:"interaction_id,omitempty"`
	Questions     map[string]any         `json:"questions,omitempty"`
	CreatedAt     time.Time              `json:"created_at"`
	UpdatedAt     time.Time              `json:"updated_at"`
	Attempts      []PipelineBoardAttempt `json:"attempts,omitempty"`
	ChildrenCount int                    `json:"children_count,omitempty"`
}

// PipelineBoardResponse is the aggregate read model for one bot-bound board.
type PipelineBoardResponse struct {
	Board         PipelineBoardIdentity `json:"board"`
	Columns       []PipelineBoardColumn `json:"columns"`
	Cards         []PipelineBoardCard   `json:"cards"`
	GeneratedAt   time.Time             `json:"generated_at"`
	TopologyError string                `json:"topology_error,omitempty"`
}

type pipelineBoardListResponse struct {
	Boards []PipelineBoardIdentity `json:"boards"`
}

type pipelineBoardTaskRequest struct {
	Title    string            `json:"title"`
	Body     string            `json:"body,omitempty"`
	Labels   []string          `json:"labels,omitempty"`
	Priority int               `json:"priority,omitempty"`
	BotArgs  map[string]string `json:"bot_args,omitempty"`
	Start    bool              `json:"start,omitempty"`
}

func (s *Server) registerPipelineBoardRoutes() {
	s.mux.Handle("GET /api/v1/pipeline-boards", s.requireAuth(http.HandlerFunc(s.handlePipelineBoardsList)))
	s.mux.Handle("GET /api/v1/pipeline-boards/{bot}", s.requireAuth(http.HandlerFunc(s.handlePipelineBoardGet)))
	s.mux.Handle("POST /api/v1/pipeline-boards/{bot}/tasks", s.requireAuth(http.HandlerFunc(s.handlePipelineBoardTaskCreate)))
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

func pipelineBoardIdentity(entry botregistry.EntryWithSchema) PipelineBoardIdentity {
	display := strings.TrimSpace(entry.DisplayName)
	if display == "" {
		display = humanizePipelineName(entry.Name)
	}
	return PipelineBoardIdentity{
		ID:          "bot:" + entry.Name,
		BotID:       entry.Name,
		DisplayName: display,
		Icon:        entry.Icon,
		Description: entry.Description,
		Enabled:     entry.Enabled,
	}
}

func (s *Server) handlePipelineBoardsList(w http.ResponseWriter, r *http.Request) {
	boardStore, err := s.resolvePipelineBoardStore(r)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "pipeline boards: resolve store: %v", err)
		return
	}
	if boardStore == nil {
		s.httpErrorFor(w, r, http.StatusNotFound, "pipeline boards: native tracker is not available")
		return
	}
	entries, err := botregistry.ListWithSchema(s.botListOptions())
	if err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "pipeline boards: discover bots: %v", err)
		return
	}
	boards := make([]PipelineBoardIdentity, 0, len(entries))
	for _, entry := range entries {
		boards = append(boards, pipelineBoardIdentity(entry))
	}
	s.writeJSONFor(w, r, pipelineBoardListResponse{Boards: boards})
}

func (s *Server) handlePipelineBoardGet(w http.ResponseWriter, r *http.Request) {
	boardStore, entry, ok := s.resolvePipelineBoardRequest(w, r)
	if !ok {
		return
	}

	s.stateMu.RLock()
	runs := s.runs
	s.stateMu.RUnlock()
	projection, err := s.buildPipelineBoard(r.Context(), boardStore, runs, entry)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "pipeline board %q: %v", entry.Name, err)
		return
	}
	s.writeJSONFor(w, r, projection)
}

func (s *Server) handlePipelineBoardTaskCreate(w http.ResponseWriter, r *http.Request) {
	if !s.requireSafeOrigin(w, r) {
		return
	}
	boardStore, entry, ok := s.resolvePipelineBoardRequest(w, r)
	if !ok {
		return
	}
	var req pipelineBoardTaskRequest
	if err := readJSON(r, &req); err != nil {
		s.httpErrorFor(w, r, http.StatusBadRequest, "pipeline board task: invalid request: %v", err)
		return
	}
	req.Title = strings.TrimSpace(req.Title)
	if req.Title == "" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "pipeline board task: title is required")
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
		Title:    req.Title,
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

func (s *Server) resolvePipelineBoardRequest(w http.ResponseWriter, r *http.Request) (native.BoardStore, botregistry.EntryWithSchema, bool) {
	boardStore, err := s.resolvePipelineBoardStore(r)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "pipeline boards: resolve store: %v", err)
		return nil, botregistry.EntryWithSchema{}, false
	}
	if boardStore == nil {
		s.httpErrorFor(w, r, http.StatusNotFound, "pipeline boards: native tracker is not available")
		return nil, botregistry.EntryWithSchema{}, false
	}
	name := strings.TrimSpace(r.PathValue("bot"))
	if name == "" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "pipeline boards: missing bot")
		return nil, botregistry.EntryWithSchema{}, false
	}
	entry, found, err := s.findBot(name)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "pipeline boards: discover bot: %v", err)
		return nil, botregistry.EntryWithSchema{}, false
	}
	if !found {
		s.httpErrorFor(w, r, http.StatusNotFound, "pipeline boards: bot %q not found", name)
		return nil, botregistry.EntryWithSchema{}, false
	}
	return boardStore, entry, true
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

type pipelineProjectionBuilder struct {
	bot               botregistry.EntryWithSchema
	columns           []PipelineBoardColumn
	columnIDs         map[string]struct{}
	dynamicColumns    map[string]PipelineBoardColumn
	runs              map[string]*store.Run
	children          map[string][]*store.Run
	terminalStates    map[string]struct{}
	includedRuns      map[string]struct{}
	issueOwnedRuns    map[string]struct{}
	cards             []PipelineBoardCard
	cardLimitReached  bool
	depthLimitReached bool
	cycleDetected     bool
}

func (s *Server) buildPipelineBoard(ctx context.Context, boardStore native.BoardStore, runs *runview.Service, entry botregistry.EntryWithSchema) (PipelineBoardResponse, error) {
	response := PipelineBoardResponse{
		Board:       pipelineBoardIdentity(entry),
		GeneratedAt: time.Now().UTC(),
	}
	staticColumns, topologyErr := pipelineBoardTopology(entry)
	if topologyErr != nil {
		response.TopologyError = topologyErr.Error()
	}
	builder := &pipelineProjectionBuilder{
		bot:            entry,
		columns:        staticColumns,
		columnIDs:      make(map[string]struct{}, len(staticColumns)),
		dynamicColumns: map[string]PipelineBoardColumn{},
		runs:           map[string]*store.Run{},
		children:       map[string][]*store.Run{},
		terminalStates: map[string]struct{}{},
		includedRuns:   map[string]struct{}{},
		issueOwnedRuns: map[string]struct{}{},
	}
	if board := boardStore.Board(); board != nil {
		for _, state := range board.States {
			if state.Terminal {
				builder.terminalStates[state.Name] = struct{}{}
			}
		}
	}
	for _, column := range staticColumns {
		builder.columnIDs[column.ID] = struct{}{}
	}

	allIssues, err := boardStore.List(native.ListFilter{})
	if err != nil {
		return PipelineBoardResponse{}, fmt.Errorf("list native tasks: %w", err)
	}
	issues := make([]*native.Issue, 0, len(allIssues))
	wantedBot := botregistry.NormalizeName(entry.Name)
	for _, issue := range allIssues {
		if issue != nil && botregistry.NormalizeName(issue.Bot) == wantedBot {
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
		builder.addRunTree(root, issue, root.ID, 0, map[string]struct{}{})
	}

	// Manual/API/scheduled roots do not necessarily have a native issue. Only
	// top-level runs (or legacy runs whose parent no longer exists) become
	// standalone roots. A child belongs to the board of the root that spawned
	// it, even when the child itself executes the target bot.
	matching := make([]*store.Run, 0)
	for _, run := range builder.runs {
		if pipelineRunMatchesBot(run, entry) {
			matching = append(matching, run)
		}
	}
	sort.SliceStable(matching, func(i, j int) bool {
		if matching[i].CreatedAt.Equal(matching[j].CreatedAt) {
			return matching[i].ID < matching[j].ID
		}
		return matching[i].CreatedAt.After(matching[j].CreatedAt)
	})
	for _, run := range matching {
		if _, owned := builder.issueOwnedRuns[run.ID]; owned {
			continue
		}
		parentID := pipelineParentRunID(run)
		parent := builder.runs[parentID]
		if parentID != "" && parent != nil {
			continue
		}
		builder.addRunTree(run, nil, run.ID, 0, map[string]struct{}{})
	}

	response.Columns = builder.finalColumns()
	response.Cards = builder.cards
	if response.Columns == nil {
		response.Columns = []PipelineBoardColumn{}
	}
	if response.Cards == nil {
		response.Cards = []PipelineBoardCard{}
	}
	if builder.cardLimitReached {
		appendPipelineTopologyError(&response, fmt.Sprintf("run tree truncated after %d cards", pipelineTreeMaxCards))
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

	// LastRunID is the dispatcher's canonical current-attempt pointer. Run
	// creation time is not a substitute: imported/legacy histories can have
	// timestamps that do not reflect append order.
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
	// attempt the authoritative source edge already exists on the run, so use
	// it to avoid rendering the same work once as a stale task and once as a
	// standalone run.
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
		if candidate.ID == current.ID || candidate.Status.IsTerminal() || !candidate.CreatedAt.After(baseline) {
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

func (b *pipelineProjectionBuilder) addTaskCard(issue *native.Issue) {
	if issue == nil || len(b.cards) >= pipelineTreeMaxCards {
		b.cardLimitReached = len(b.cards) >= pipelineTreeMaxCards
		return
	}
	column := pipelineColumnTodo
	if _, terminal := b.terminalStates[issue.State]; terminal {
		column = pipelineColumnDone
	} else if issue.Claim != "" {
		column = pipelineColumnRunning
	}
	b.cards = append(b.cards, PipelineBoardCard{
		ID:         "task:" + issue.ID,
		Kind:       "task",
		ColumnID:   column,
		Title:      issue.Title,
		Body:       issue.Body,
		IssueID:    issue.ID,
		IssueState: issue.State,
		Labels:     append([]string(nil), issue.Labels...),
		Priority:   issue.Priority,
		BotID:      issue.Bot,
		Depth:      0,
		CreatedAt:  issue.CreatedAt,
		UpdatedAt:  issue.UpdatedAt,
		Attempts:   b.attemptsForIssue(issue, nil),
	})
}

func (b *pipelineProjectionBuilder) addRunTree(run *store.Run, issue *native.Issue, rootRunID string, depth int, path map[string]struct{}) {
	if run == nil {
		return
	}
	if len(b.cards) >= pipelineTreeMaxCards {
		b.cardLimitReached = true
		return
	}
	if depth > pipelineTreeMaxDepth {
		b.depthLimitReached = true
		return
	}
	if _, cycle := path[run.ID]; cycle {
		b.cycleDetected = true
		return
	}
	if _, already := b.includedRuns[run.ID]; already {
		return
	}
	nextPath := make(map[string]struct{}, len(path)+1)
	for id := range path {
		nextPath[id] = struct{}{}
	}
	nextPath[run.ID] = struct{}{}
	b.includedRuns[run.ID] = struct{}{}

	columnID, nodeID, interactionID, questions := b.columnForRun(run, depth)
	title := strings.TrimSpace(run.Name)
	if title == "" {
		title = humanizePipelineName(run.WorkflowName)
	}
	if issue != nil && depth == 0 {
		title = issue.Title
	}
	card := PipelineBoardCard{
		ID:            "run:" + run.ID,
		Kind:          "run",
		ColumnID:      columnID,
		Title:         title,
		RunID:         run.ID,
		RootRunID:     rootRunID,
		ParentRunID:   pipelineParentRunID(run),
		Depth:         depth,
		WorkflowName:  run.WorkflowName,
		BotID:         pipelineRunBotID(run),
		Status:        run.Status,
		Error:         run.Error,
		NodeID:        nodeID,
		InteractionID: interactionID,
		Questions:     questions,
		CreatedAt:     run.CreatedAt,
		UpdatedAt:     run.UpdatedAt,
		ChildrenCount: len(b.children[run.ID]),
	}
	if issue != nil {
		card.IssueID = issue.ID
		card.IssueState = issue.State
		if depth == 0 {
			card.Body = issue.Body
			card.Labels = append([]string(nil), issue.Labels...)
			card.Priority = issue.Priority
			card.Attempts = b.attemptsForIssue(issue, run)
		}
	}
	b.cards = append(b.cards, card)
	for _, child := range b.children[run.ID] {
		b.addRunTree(child, issue, rootRunID, depth+1, nextPath)
	}
}

func (b *pipelineProjectionBuilder) columnForRun(run *store.Run, depth int) (columnID, nodeID, interactionID string, questions map[string]any) {
	if run == nil {
		return pipelineColumnAttention, "", "", nil
	}
	switch run.Status {
	case store.RunStatusFinished:
		return pipelineColumnDone, "", "", nil
	case store.RunStatusQueued, store.RunStatusRunning:
		return pipelineColumnRunning, "", "", nil
	case store.RunStatusPausedWaitingHuman:
		if run.Checkpoint == nil || run.Checkpoint.NodeID == "" {
			return pipelineColumnOtherInput, "", "", nil
		}
		nodeID = run.Checkpoint.NodeID
		interactionID = run.Checkpoint.InteractionID
		questions = cloneAnyMap(run.Checkpoint.InteractionQuestions)
		columnID = pipelineInteractionColumnID(run.WorkflowName, nodeID)
		if _, exists := b.columnIDs[columnID]; !exists {
			if depth == 0 {
				return pipelineColumnOtherInput, nodeID, interactionID, questions
			}
			workflowName := run.WorkflowName
			title := humanizePipelineName(nodeID)
			if workflowName != "" && botregistry.NormalizeName(workflowName) != botregistry.NormalizeName(b.bot.Name) {
				title += " · " + humanizePipelineName(workflowName)
			}
			b.dynamicColumns[columnID] = PipelineBoardColumn{
				ID:           columnID,
				Title:        title,
				Kind:         "interaction",
				WorkflowName: workflowName,
				NodeID:       nodeID,
			}
		}
		return columnID, nodeID, interactionID, questions
	case store.RunStatusPausedOperator, store.RunStatusFailed, store.RunStatusFailedResumable, store.RunStatusCancelled:
		return pipelineColumnAttention, "", "", nil
	default:
		return pipelineColumnAttention, "", "", nil
	}
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

func (b *pipelineProjectionBuilder) finalColumns() []PipelineBoardColumn {
	dynamic := make([]PipelineBoardColumn, 0, len(b.dynamicColumns))
	for _, column := range b.dynamicColumns {
		dynamic = append(dynamic, column)
	}
	sort.Slice(dynamic, func(i, j int) bool {
		if dynamic[i].WorkflowName != dynamic[j].WorkflowName {
			return dynamic[i].WorkflowName < dynamic[j].WorkflowName
		}
		return dynamic[i].NodeID < dynamic[j].NodeID
	})
	// Static columns are laid out as todo, running, interactions,
	// other-input, attention, done. Insert runtime-discovered child columns
	// immediately before the generic fallback.
	out := make([]PipelineBoardColumn, 0, len(b.columns)+len(dynamic))
	for _, column := range b.columns {
		if column.ID == pipelineColumnOtherInput {
			out = append(out, dynamic...)
		}
		out = append(out, column)
	}
	return out
}

func pipelineBoardTopology(entry botregistry.EntryWithSchema) ([]PipelineBoardColumn, error) {
	columns := []PipelineBoardColumn{
		{ID: pipelineColumnTodo, Title: "Todo", Kind: "todo"},
		{ID: pipelineColumnRunning, Title: "Running", Kind: "running"},
	}
	path := entry.MainFile()
	var (
		workflow *ir.Workflow
		err      error
	)
	if bundle := runview.ResolveBundleFromFilePath(path); bundle != nil {
		workflow, _, err = runview.CompileBundleWorkflow(path, bundle)
	} else {
		workflow, _, err = runview.CompileWorkflowWithHash(path)
	}
	if err == nil {
		for _, nodeID := range pipelineWorkflowNodeOrder(workflow) {
			node := workflow.Nodes[nodeID]
			mode := ir.NodeInteraction(node)
			if !pipelineInteractionNeedsHuman(node, mode) {
				continue
			}
			columns = append(columns, PipelineBoardColumn{
				ID:              pipelineInteractionColumnID(workflow.Name, nodeID),
				Title:           humanizePipelineName(nodeID),
				Kind:            "interaction",
				WorkflowName:    workflow.Name,
				NodeID:          nodeID,
				InteractionMode: mode.String(),
			})
		}
	}
	columns = append(columns,
		PipelineBoardColumn{ID: pipelineColumnOtherInput, Title: "Other input", Kind: "interaction"},
		PipelineBoardColumn{ID: pipelineColumnAttention, Title: "Needs attention", Kind: "attention"},
		PipelineBoardColumn{ID: pipelineColumnDone, Title: "Done", Kind: "done"},
	)
	return columns, err
}

func pipelineInteractionNeedsHuman(node ir.Node, mode ir.InteractionMode) bool {
	if node == nil {
		return false
	}
	switch mode {
	case ir.InteractionHuman, ir.InteractionLLMOrHuman, ir.InteractionReview:
		return true
	case ir.InteractionNone:
		// A compiled HumanNode normally defaults to InteractionHuman. Keep
		// the kind fallback for old/hand-built IR values used by importers.
		return node.NodeKind() == ir.NodeHuman
	default:
		return false
	}
}

func pipelineWorkflowNodeOrder(workflow *ir.Workflow) []string {
	if workflow == nil {
		return nil
	}
	adjacent := make(map[string][]string, len(workflow.Nodes))
	for _, edge := range workflow.Edges {
		if edge == nil {
			continue
		}
		adjacent[edge.From] = append(adjacent[edge.From], edge.To)
	}
	seen := map[string]struct{}{}
	order := make([]string, 0, len(workflow.Nodes))
	queue := []string{workflow.Entry}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		if _, exists := workflow.Nodes[id]; exists {
			order = append(order, id)
		}
		queue = append(queue, adjacent[id]...)
	}
	remaining := make([]string, 0, len(workflow.Nodes)-len(order))
	for id := range workflow.Nodes {
		if _, ok := seen[id]; !ok {
			remaining = append(remaining, id)
		}
	}
	sort.Strings(remaining)
	return append(order, remaining...)
}

func pipelineInteractionColumnID(workflowName, nodeID string) string {
	encode := func(value string) string {
		return base64.RawURLEncoding.EncodeToString([]byte(value))
	}
	return "interaction:" + encode(workflowName) + ":" + encode(nodeID)
}

func pipelineRunMatchesBot(run *store.Run, entry botregistry.EntryWithSchema) bool {
	if run == nil {
		return false
	}
	want := botregistry.NormalizeName(entry.Name)
	candidates := []string{run.BotID, run.BundleName}
	if run.BundlePath != "" {
		candidates = append(candidates, strings.TrimSuffix(filepath.Base(strings.TrimRight(run.BundlePath, "/")), ".botz"))
	}
	for _, candidate := range candidates {
		if candidate != "" && botregistry.NormalizeName(candidate) == want {
			return true
		}
	}
	if run.FilePath != "" {
		runPath, runErr := filepath.Abs(run.FilePath)
		botPath, botErr := filepath.Abs(entry.MainFile())
		if runErr == nil && botErr == nil && filepath.Clean(runPath) == filepath.Clean(botPath) {
			return true
		}
	}
	return false
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

func humanizePipelineName(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "Pipeline"
	}
	var b strings.Builder
	previousSeparator := true
	for _, r := range value {
		if r == '_' || r == '-' || r == '.' || r == '/' || unicode.IsSpace(r) {
			if b.Len() > 0 && !previousSeparator {
				b.WriteByte(' ')
			}
			previousSeparator = true
			continue
		}
		if previousSeparator {
			b.WriteRune(unicode.ToUpper(r))
		} else {
			b.WriteRune(r)
		}
		previousSeparator = false
	}
	return strings.TrimSpace(b.String())
}
