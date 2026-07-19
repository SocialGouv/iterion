package server

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
)

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
// Backlog ticket the operator prepares and stages to Todo when ready.
func (b *pipelineProjectionBuilder) addTaskCard(issue *native.Issue) {
	if issue == nil || len(b.cards) >= pipelineTreeMaxCards {
		b.cardLimitReached = len(b.cards) >= pipelineTreeMaxCards
		return
	}
	_, terminal := b.terminalStates[issue.State]
	// "Ready" is the specific StateReady the operator stages a ticket into;
	// the launch loop starts exactly those. Other non-terminal states
	// (inbox/…) are Backlog tickets being prepared.
	ready := issue.State == native.StateReady
	column := pipelineColumnBacklog
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
		External:   issue.External,
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
	treeExec, treeTotal, descCount, reviews, treeRunIDs := b.aggregateTree(root)

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
		TreeRunIDs:        treeRunIDs,
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
		card.External = issue.External
		card.Attempts = b.attemptsForIssue(issue, root)
		if card.EntryInput == nil {
			card.EntryInput = stringMapToAny(issue.BotArgs)
		}
	}
	// A run-only card (no backing issue) still has a repo identity when
	// the run targeted one — surface it so the repo scope covers direct
	// launches too.
	if card.External == nil && root.ProjectPath != "" {
		card.External = &native.ExternalRef{Repo: root.ProjectPath}
	}
	b.cards = append(b.cards, card)
}

// aggregateTree walks root ∪ descendants, summing node-weighted progress,
// counting descendants, collecting every pending human review, and gathering
// the flattened run-id list (root first, then descendants in walk order). It
// reuses the depth / cycle guards; a finished subtree contributes without
// an event scan (see runProgress).
func (b *pipelineProjectionBuilder) aggregateTree(root *store.Run) (treeExec, treeTotal, descCount int, reviews []PipelineBoardPendingReview, runIDs []string) {
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
		runIDs = append(runIDs, run.ID)
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
