package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
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
// there is no drag-and-drop. See docs/native-tracker.md + ADR-074.
const (
	// Three fixed lanes. "opened" folds backlog + ready staging (a per-card
	// `ready` badge distinguishes prepared-but-not-ready from launch-eligible);
	// "closed" folds done + failed (per-card success/failed). IDs are the wire
	// contract (filters, tests). Formerly wire id "todo" — renamed to pair with
	// "closed".
	pipelineColumnOpened     = "opened"
	pipelineColumnInProgress = "in_progress"
	pipelineColumnClosed     = "closed"

	// pipelineColumnTodo is the legacy wire id for the opened lane. Kept as an
	// alias constant so any stray reference fails to compile if missed; do not
	// emit this on the wire.
	pipelineColumnTodo = pipelineColumnOpened

	pipelineTreeMaxDepth = 20
	pipelineTreeMaxCards = 500
	// Keep the card label compact even when a bot input or ticket title is a
	// full brief. The complete value remains available in EntryInput / Body.
	pipelineTitleMaxRunes = 80

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

// PipelineBoardColumn is one of the three fixed lanes. Unlike the previous
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
// slot frees. Closed holds every finished pipeline, success or failure —
// a per-card success/failed outcome distinguishes them (surfaced as the
// Closed lane's filter). In progress holds running / awaiting-review runs.
func pipelineColumns() []PipelineBoardColumn {
	return []PipelineBoardColumn{
		{ID: pipelineColumnOpened, Title: "Opened", Kind: "opened"},
		{ID: pipelineColumnInProgress, Title: "In progress", Kind: "in_progress"},
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
	// UpdatedAt is when this exact pending turn joined the operator queue.
	// Review gates reuse their interaction ID across dialogue turns, so the
	// timestamp also versions the form on the Studio side.
	UpdatedAt time.Time `json:"updated_at"`
	// Instructions is the human node's rendered `instructions:` markdown,
	// giving the operator the author's context next to the answer form.
	Instructions string `json:"instructions,omitempty"`
	// ReviewBrief is the validated, runtime-stamped AI checklist for this exact
	// human turn. It is kept separate from Instructions so clients can present
	// concise decision points without discarding the full authored context.
	ReviewBrief *store.HumanReviewBrief `json:"review_brief,omitempty"`
	// Media contains validated references to passive media, document, or data
	// attachments on this exact human turn. Payload bytes remain behind the
	// authenticated per-run artifact-files endpoint.
	Media []store.ReviewMediaRef `json:"media,omitempty"`
	// Review is present for a guided `interaction: review` pause. It carries
	// the dialogue and reserved-action configuration needed to render the same
	// approve/merge/request-changes flow as the run console.
	Review *store.ReviewGateState `json:"review,omitempty"`
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
	// Failed is true when the card sits in the FAILED lane because its run
	// failed / was cancelled. The UI shows the Error as the reason and
	// offers a Retry (move back to Ready) on ticket-backed cards.
	Failed bool `json:"failed,omitempty"`
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
	// Blockers are hard deps: issue IDs that must reach StateDone before
	// this ticket can launch. Cycles are rejected at create.
	Blockers []string `json:"blockers,omitempty"`
	// ParentID is the planner ticket that spawned this one (provenance).
	// Also accepted via bot_args.spawned_from.
	ParentID string `json:"parent_id,omitempty"`
	Start    bool   `json:"start,omitempty"`
	// Upsert, when true and bot_args.input_path is set, updates an existing
	// ticket with the same (bot, input_path) instead of creating a duplicate.
	// Does not reset state when the match is already in_progress / done /
	// awaiting_input (or has an active run stamp).
	Upsert bool `json:"upsert,omitempty"`
}

func (s *Server) registerPipelineBoardRoutes() {
	s.mux.Handle("GET /api/v1/pipeline-board", s.requireAuth(http.HandlerFunc(s.handlePipelineBoard)))
	s.mux.Handle("POST /api/v1/pipeline-board/tasks", s.requireAuth(http.HandlerFunc(s.handlePipelineBoardTaskCreate)))
	s.mux.Handle("POST /api/v1/pipeline-board/tasks/{id}/ready", s.requireAuth(http.HandlerFunc(s.handlePipelineBoardTaskReady)))
	s.mux.Handle("PATCH /api/v1/pipeline-board/tasks/{id}", s.requireAuth(http.HandlerFunc(s.handlePipelineBoardTaskUpdate)))
	s.mux.Handle("DELETE /api/v1/pipeline-board/tasks/{id}", s.requireAuth(http.HandlerFunc(s.handlePipelineBoardTaskDelete)))
	s.mux.Handle("POST /api/v1/pipeline-board/tasks/{id}/reset", s.requireAuth(http.HandlerFunc(s.handlePipelineBoardTaskReset)))
	s.mux.Handle("POST /api/v1/pipeline-board/tasks/{id}/launch", s.requireAuth(http.HandlerFunc(s.handlePipelineBoardTaskLaunch)))
	// Bulk + graph (multi-pipeline production ops).
	s.mux.Handle("POST /api/v1/pipeline-board/bulk/ready", s.requireAuth(http.HandlerFunc(s.handlePipelineBoardBulkReady)))
	s.mux.Handle("POST /api/v1/pipeline-board/bulk/delete", s.requireAuth(http.HandlerFunc(s.handlePipelineBoardBulkDelete)))
	s.mux.Handle("POST /api/v1/pipeline-board/bulk/recompute-deps", s.requireAuth(http.HandlerFunc(s.handlePipelineBoardRecomputeDeps)))
	s.mux.Handle("GET /api/v1/pipeline-board/tasks/{id}/dependency-graph", s.requireAuth(http.HandlerFunc(s.handlePipelineBoardDependencyGraph)))
	// Also under /api/v1/native for CLI/MCP consumers that already use that prefix.
	s.mux.Handle("GET /api/v1/native/issues/{id}/dependency-graph", s.requireAuth(http.HandlerFunc(s.handlePipelineBoardDependencyGraph)))
	// Input thumbnails: a ticket's bot_args may reference images living in the
	// studio workdir (e.g. a character-reference list) — this endpoint lets the
	// card sidebar actually SHOW them instead of printing bare paths.
	s.mux.Handle("GET /api/v1/pipeline-board/workspace-images/{path...}", s.requireAuth(http.HandlerFunc(s.handlePipelineBoardWorkspaceImage)))
}

// workspaceImageExts is the strict allowlist of the input-thumbnail endpoint:
// it exists solely to preview images referenced by ticket inputs, so anything
// that is not an image stays out of scope on purpose (no generic file reads).
var workspaceImageExts = map[string]string{
	".png":  "image/png",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".webp": "image/webp",
	".gif":  "image/gif",
}

// handlePipelineBoardWorkspaceImage serves one image file from the studio
// workdir for the card sidebar's input thumbnails. Containment relies on
// safePath — the same audited symlink-aware traversal boundary as the file
// editor — and the extension allowlist keeps the endpoint image-only.
func (s *Server) handlePipelineBoardWorkspaceImage(w http.ResponseWriter, r *http.Request) {
	relPath := strings.TrimSpace(r.PathValue("path"))
	if relPath == "" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "workspace image: missing path")
		return
	}
	contentType, ok := workspaceImageExts[strings.ToLower(filepath.Ext(relPath))]
	if !ok {
		s.httpErrorFor(w, r, http.StatusNotFound, "workspace image: unsupported extension: %s", filepath.Ext(relPath))
		return
	}
	absPath, err := s.safePath(relPath)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusBadRequest, "workspace image: invalid path: %v", err)
		return
	}
	info, err := os.Stat(absPath)
	if err != nil || !info.Mode().IsRegular() {
		s.httpErrorFor(w, r, http.StatusNotFound, "workspace image: not found: %s", relPath)
		return
	}
	file, err := os.Open(absPath)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusNotFound, "workspace image: not found: %s", relPath)
		return
	}
	defer file.Close()
	w.Header().Set("Content-Type", contentType)
	// Reference images are regenerated IN PLACE under the same filename
	// (portrait refresh): revalidate on each load so the sidebar never shows
	// a stale face after a refresh, while still allowing conditional requests.
	w.Header().Set("Cache-Control", "no-cache")
	http.ServeContent(w, r, filepath.Base(absPath), info.ModTime(), file)
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
	req.Title = compactPipelineTitle(req.Title)
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
	blockers := native.NormalizeBlockers(req.Blockers)
	botArgs := cloneStringMap(req.BotArgs)
	blockerPolicy := native.BlockerPolicy{RequireLabels: native.RequireBlockerLabels(botArgs)}

	// Upsert: planners re-run without duplicating tickets for the same request file.
	if req.Upsert {
		if bot, inputPath, ok := native.UpsertKey(entry.Name, botArgs); ok {
			existing, ferr := native.FindByBotInputPath(boardStore, bot, inputPath)
			if ferr != nil {
				s.httpErrorFor(w, r, http.StatusInternalServerError, "pipeline board task: upsert lookup: %v", ferr)
				return
			}
			if existing != nil {
				issue, uerr := s.upsertPipelineTask(boardStore, board, existing, req, entry.Name, botArgs, blockers)
				if uerr != nil {
					if strings.Contains(uerr.Error(), "cycle") {
						s.httpErrorFor(w, r, http.StatusBadRequest, "pipeline board task: %v", uerr)
						return
					}
					s.httpErrorFor(w, r, http.StatusInternalServerError, "pipeline board task: upsert: %v", uerr)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				s.reflectAllowedOrigin(w, r)
				// 200 = updated existing; create stays 201.
				_ = json.NewEncoder(w).Encode(issue)
				return
			}
		}
	}

	if err := native.ValidateBlockers(boardStore, "", blockers); err != nil {
		s.httpErrorFor(w, r, http.StatusBadRequest, "pipeline board task: %v", err)
		return
	}
	state := board.States[0].Name
	if req.Start {
		// Prefer StateReady when present; fall back to first eligible.
		state = native.StateReady
		if board.StateByName(state) == nil {
			state = ""
			for _, candidate := range board.States {
				if candidate.Eligible && !candidate.Terminal {
					state = candidate.Name
					break
				}
			}
		}
		if state == "" {
			s.httpErrorFor(w, r, http.StatusConflict, "pipeline board task: native board has no dispatch-eligible state")
			return
		}
		// D1: Ready only when hard deps are satisfied; otherwise park in
		// waiting_deps (or refuse if that state is absent on a custom board).
		ok, open := native.BlockersSatisfiedPolicy(boardStore, blockers, blockerPolicy)
		if !ok {
			if board.StateByName(native.StateWaitingDeps) != nil {
				state = native.StateWaitingDeps
			} else {
				s.httpErrorFor(w, r, http.StatusConflict,
					"pipeline board task: cannot start with open blockers %s (board has no waiting_deps state)",
					formatBlockerIDs(open))
				return
			}
		}
	} else if len(blockers) > 0 {
		// Planner publish: if deps known open and waiting_deps exists, park there
		// so the card is visible as waiting (not a silent draft).
		ok, _ := native.BlockersSatisfiedPolicy(boardStore, blockers, blockerPolicy)
		if !ok && board.StateByName(native.StateWaitingDeps) != nil {
			state = native.StateWaitingDeps
		}
	}
	parentID := strings.TrimSpace(req.ParentID)
	if parentID == "" && botArgs != nil {
		parentID = strings.TrimSpace(botArgs[native.BotArgSpawnedFrom])
	}
	if parentID != "" {
		if botArgs == nil {
			botArgs = map[string]string{}
		}
		botArgs[native.BotArgSpawnedFrom] = parentID
	}
	issue, err := boardStore.Create(native.Issue{
		Title:    uniquePipelineTitle(boardStore, req.Title),
		Body:     strings.TrimSpace(req.Body),
		State:    state,
		Labels:   append([]string(nil), req.Labels...),
		Priority: req.Priority,
		ParentID: parentID,
		Bot:      entry.Name,
		BotArgs:  botArgs,
		Blockers: blockers,
	})
	if err != nil {
		// Cycle validation inside Create surfaces as 400.
		if strings.Contains(err.Error(), "cycle") {
			s.httpErrorFor(w, r, http.StatusBadRequest, "pipeline board task: %v", err)
			return
		}
		s.httpErrorFor(w, r, http.StatusInternalServerError, "pipeline board task: create: %v", err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	s.reflectAllowedOrigin(w, r)
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(issue)
}

// upsertPipelineTask patches title/body/labels/priority/bot_args/blockers on an
// existing ticket. Does not move out of in_progress / done / awaiting_input;
// optional Start only stages ready/waiting_deps when the ticket is still
// pre-execution (backlog/inbox/waiting_deps/ready).
func (s *Server) upsertPipelineTask(
	boardStore native.BoardStore,
	board *native.Board,
	existing *native.Issue,
	req pipelineBoardTaskRequest,
	botName string,
	botArgs map[string]string,
	blockers []string,
) (*native.Issue, error) {
	if err := native.ValidateBlockers(boardStore, existing.ID, blockers); err != nil {
		return nil, err
	}
	title := strings.TrimSpace(req.Title)
	body := strings.TrimSpace(req.Body)
	patch := native.Patch{
		Title:    &title,
		Body:     &body,
		Priority: &req.Priority,
		Bot:      &botName,
		BotArgs:  &botArgs,
		Blockers: &blockers,
	}
	if req.Labels != nil {
		labels := append([]string(nil), req.Labels...)
		patch.Labels = &labels
	}
	iss, err := boardStore.Update(existing.ID, patch)
	if err != nil {
		return nil, err
	}
	// State: only nudge pre-run tickets. Never yank a live/finished run.
	if isPipelineTerminalOrActive(iss.State) {
		return iss, nil
	}
	policy := native.BlockerPolicy{RequireLabels: native.RequireBlockerLabels(botArgs)}
	ok, _ := native.BlockersSatisfiedPolicy(boardStore, blockers, policy)
	var target string
	if req.Start {
		if ok {
			target = native.StateReady
		} else if board != nil && board.StateByName(native.StateWaitingDeps) != nil {
			target = native.StateWaitingDeps
		}
	} else if !ok && board != nil && board.StateByName(native.StateWaitingDeps) != nil {
		// Keep / move to waiting_deps when deps still open.
		target = native.StateWaitingDeps
	}
	if target != "" && target != iss.State && board != nil && board.StateByName(target) != nil {
		return boardStore.SetState(iss.ID, target)
	}
	return iss, nil
}

func isPipelineTerminalOrActive(state string) bool {
	switch state {
	case native.StateInProgress, native.StateAwaitingInput, native.StateReview,
		native.StateDone, native.StateBlocked:
		return true
	default:
		return false
	}
}

// uniquePipelineTitle keeps board card titles distinct: if `desired` is
// already a ticket's title, it prefixes the smallest free "#N - " (N≥2)
// ("Episode" → "#2 - Episode" → "#3 - Episode"). The counter is a PREFIX so
// it stays visible even when a long title is truncated. Best-effort — a list
// error just returns the desired title unchanged.
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
		candidate := compactPipelineTitle(fmt.Sprintf("#%d - %s", n, desired))
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
// (a Backlog ticket, or a failed one before retry). Only non-nil fields are
// applied; the studio form sends the full state it wants to persist.
type pipelineBoardUpdateRequest struct {
	Title    *string            `json:"title,omitempty"`
	Body     *string            `json:"body,omitempty"`
	Labels   *[]string          `json:"labels,omitempty"`
	Priority *int               `json:"priority,omitempty"`
	Bot      *string            `json:"bot,omitempty"`
	BotArgs  *map[string]string `json:"bot_args,omitempty"`
	// Blockers, when non-nil, replaces the hard-dep list wholesale (empty
	// slice clears). Cycles are rejected.
	Blockers *[]string `json:"blockers,omitempty"`
}

// handlePipelineBoardTaskUpdate edits a ticket's fields (title, input,
// bot, …) so Backlog tickets stay editable while the operator prepares them.
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
		title := compactPipelineTitle(*req.Title)
		if title == "" {
			s.httpErrorFor(w, r, http.StatusBadRequest, "pipeline board update: title cannot be empty")
			return
		}
		patch.Title = &title
	}
	if req.Bot != nil {
		bot := strings.TrimSpace(*req.Bot)
		// An empty bot would silently unbind the ticket — it then never
		// launches and folds into Backlog with no UX to recover. Creation
		// already rejects a missing bot; the patch must too (unbinding, if
		// ever wanted, should be an explicit operation, not a blank field).
		if bot == "" {
			s.httpErrorFor(w, r, http.StatusBadRequest, "pipeline board update: bot cannot be empty")
			return
		}
		if _, found, ferr := s.findBot(bot); ferr != nil {
			s.httpErrorFor(w, r, http.StatusInternalServerError, "pipeline board update: discover bot: %v", ferr)
			return
		} else if !found {
			s.httpErrorFor(w, r, http.StatusNotFound, "pipeline board update: bot %q not found", bot)
			return
		}
		patch.Bot = &bot
	}
	if req.Blockers != nil {
		normalized := native.NormalizeBlockers(*req.Blockers)
		if err := native.ValidateBlockers(boardStore, id, normalized); err != nil {
			s.httpErrorFor(w, r, http.StatusBadRequest, "pipeline board update: %v", err)
			return
		}
		patch.Blockers = &normalized
	}
	issue, err := boardStore.Update(id, patch)
	if err != nil {
		if strings.Contains(err.Error(), "cycle") {
			s.httpErrorFor(w, r, http.StatusBadRequest, "pipeline board update: %v", err)
			return
		}
		s.httpErrorFor(w, r, http.StatusInternalServerError, "pipeline board update: %v", err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	s.reflectAllowedOrigin(w, r)
	_ = json.NewEncoder(w).Encode(issue)
}

type pipelineBoardReadyRequest struct {
	// Ready true stages the ticket Backlog→Ready (StateReady, eligible for
	// the launch loop); false unstages it Ready→Backlog (StateInbox).
	Ready bool `json:"ready"`
}

// handlePipelineBoardTaskReady flags a native ticket ready (or back to
// backlog) — the backend of the board's “→ Ready” / “→ Backlog” buttons. A
// ready ticket is launched by the admission loop when a concurrency slot
// frees. D1: Ready is only accepted when hard blockers are all StateDone;
// otherwise the ticket is parked in waiting_deps (or 409 when that state
// is absent on a custom board).
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
	// Unstage → backlog (prefer StateBacklog; StateInbox is the historical
	// unstage target for boards that still use it as the first column).
	target := native.StateBacklog
	if board := boardStore.Board(); board != nil && board.StateByName(target) == nil {
		target = native.StateInbox
	}
	if req.Ready {
		iss, gerr := boardStore.Get(id)
		if gerr != nil {
			s.httpErrorFor(w, r, http.StatusNotFound, "pipeline board ready: %v", gerr)
			return
		}
		ok, open := native.BlockersSatisfiedForIssue(boardStore, iss)
		if !ok {
			if board := boardStore.Board(); board != nil && board.StateByName(native.StateWaitingDeps) != nil {
				target = native.StateWaitingDeps
			} else {
				s.writeJSONError(w, r, http.StatusConflict, map[string]any{
					"error":         "open_blockers",
					"message":       fmt.Sprintf("cannot mark ready: open blockers %s", formatBlockerIDs(open)),
					"open_blockers": open,
				})
				return
			}
		} else {
			target = native.StateReady
		}
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

// formatBlockerIDs is a short human list for error messages.
func formatBlockerIDs(open []native.BlockerInfo) string {
	if len(open) == 0 {
		return "[]"
	}
	parts := make([]string, 0, len(open))
	for _, b := range open {
		if b.Title != "" {
			parts = append(parts, fmt.Sprintf("%s (%s)", b.ID, b.Title))
		} else {
			parts = append(parts, b.ID)
		}
	}
	return strings.Join(parts, ", ")
}

// writeJSONError writes a structured JSON error body (used when the client
// needs machine fields like open_blockers). Falls back to plain text when
// encoding fails.
func (s *Server) writeJSONError(w http.ResponseWriter, r *http.Request, code int, body map[string]any) {
	s.reflectAllowedOrigin(w, r)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}

// ---------------------------------------------------------------------------
// Projection
// ---------------------------------------------------------------------------

type pipelineProjectionBuilder struct {
	ctx            context.Context
	rs             store.RunStore
	boardStore     native.BoardStore
	allIssues      []*native.Issue
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
		boardStore:     boardStore,
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
	builder.allIssues = allIssues
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
			builder.addTaskCard(issue, nil)
			continue
		}
		// Restaged for relaunch: ticket is back in Opened (ready / inbox /
		// waiting_deps / …) while LastRunID still points at a terminal
		// failed/cancelled attempt. Project the TICKET card — not the old
		// Closed run — so Ready stays visible and the admission queue can
		// be understood. Without this, Retry/reset leaves the card stuck
		// behind its cancelled run in Closed (episode "invisible but not
		// lost"). History remains on Attempts.
		if pipelineIssueRestagedForRelaunch(issue, root) {
			builder.addTaskCard(issue, root)
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

	// Planner provenance only — a parent is an ordinary card and lands in
	// Closed as soon as its own run finishes, regardless of its children.
	// (There used to be a campaign hold pinning executed parents to Opened
	// until every child closed; it made the parent's own lane lie about its
	// run. The relation now shows purely as data: the children counter on
	// the parent, the parent name on each child.)
	builder.enrichParentChildLinks()

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

// pipelineIssueRestagedForRelaunch reports that the operator (or reset /
// Retry) put the ticket back into a pre-launch staging state while the
// current attempt is a terminal failure/cancel. The board must show an
// Opened task card, not bury the ticket under the old Closed run card.
// Finished-success is excluded — admission will not relaunch it.
func pipelineIssueRestagedForRelaunch(issue *native.Issue, root *store.Run) bool {
	if issue == nil || root == nil {
		return false
	}
	if !pipelineRunFailed(root.Status) {
		return false
	}
	switch issue.State {
	case native.StateReady, native.StateInbox, native.StateWaitingDeps,
		native.StateBacklog:
		return true
	default:
		// in_progress / awaiting_input / done / blocked / review → keep the
		// run card (live or closed outcome of that attempt).
		return false
	}
}

// addTaskCard emits a card for a native task pinned to a bot that has no
// *active* run (never launched, or restaged after a failed/cancelled
// attempt). Every not-yet-running ticket sits in Opened; its `ready` flag
// (StateReady) drives the studio's Ready badge + filter — the launch loop
// starts exactly the ready ones when a slot frees, the rest are still being
// prepared. A terminal-state ticket with no run lands in Closed.
//
// prior is the terminal previous attempt when restaged; it enriches title /
// entry_input / attempts without moving the card to Closed.
func (b *pipelineProjectionBuilder) addTaskCard(issue *native.Issue, prior *store.Run) {
	if issue == nil || len(b.cards) >= pipelineTreeMaxCards {
		b.cardLimitReached = len(b.cards) >= pipelineTreeMaxCards
		return
	}
	_, terminal := b.terminalStates[issue.State]
	// "Ready" is the specific StateReady the operator stages a ticket into;
	// the launch loop starts exactly those. Other non-terminal states
	// (inbox/waiting_deps/…) are tickets being prepared — same Todo lane,
	// no Ready badge (waiting_deps surfaces via open_blocker_count + reason).
	ready := issue.State == native.StateReady
	column := pipelineColumnTodo
	if terminal {
		column = pipelineColumnClosed
	}
	entry := stringMapToAny(issue.BotArgs)
	if len(entry) == 0 && prior != nil {
		entry = cloneAnyMap(prior.Inputs)
	}
	card := PipelineBoardCard{
		ID:         "task:" + issue.ID,
		Kind:       "task",
		ColumnID:   column,
		Title:      pipelineDisplayTitle(issue, prior),
		Body:       issue.Body,
		IssueID:    issue.ID,
		IssueState: issue.State,
		Ready:      ready,
		Labels:     append([]string(nil), issue.Labels...),
		Priority:   issue.Priority,
		BotID:      issue.Bot,
		Role:       pipelineIssueRole(issue),
		EntryInput: entry,
		CreatedAt:  issue.CreatedAt,
		UpdatedAt:  issue.UpdatedAt,
		Attempts:   b.attemptsForIssue(issue, prior),
	}
	b.attachDeps(&card, issue)
	b.cards = append(b.cards, card)
}

// attachDeps fills the hard-dependency projection fields on a card from
// its native issue. No-op when the card has no issue provenance.
func (b *pipelineProjectionBuilder) attachDeps(card *PipelineBoardCard, issue *native.Issue) {
	if card == nil || issue == nil || b.boardStore == nil {
		return
	}
	blockers := native.ResolveBlockersForIssue(b.boardStore, issue)
	card.Blockers = blockers
	open := 0
	for _, bl := range blockers {
		if !bl.Satisfied {
			open++
		}
	}
	card.OpenBlockerCount = open
	card.Blocking = native.ReverseBlockers(b.allIssues, issue.ID)
	// launch_blocked_reason is useful even for non-ready tickets (waiting_deps
	// / open blockers) so the UI can show a badge without re-deriving the rule.
	if reason := native.LaunchBlockedReason(b.boardStore, issue); reason != "" {
		// For non-ready tickets that simply aren't staged yet, suppress the
		// generic "not_ready" noise — only surface actionable gates.
		if reason == "not_ready" && issue.State != native.StateReady {
			if open > 0 {
				// Prefer blocker_labels when any open entry is label-gated.
				for _, bl := range blockers {
					if len(bl.MissingLabels) > 0 {
						card.LaunchBlockedReason = "blocker_labels"
						return
					}
				}
				card.LaunchBlockedReason = "open_blockers"
			} else if issue.State == native.StateWaitingDeps {
				card.LaunchBlockedReason = "waiting_deps"
			}
		} else {
			card.LaunchBlockedReason = reason
		}
	}
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

	card := PipelineBoardCard{
		ID:                "run:" + root.ID,
		Kind:              "run",
		ColumnID:          pipelineColumnForRoot(root, reviews),
		Title:             pipelineDisplayTitle(issue, root),
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
		card.Role = pipelineIssueRole(issue)
		card.Attempts = b.attemptsForIssue(issue, root)
		if card.EntryInput == nil {
			card.EntryInput = stringMapToAny(issue.BotArgs)
		}
		b.attachDeps(&card, issue)
	}
	b.cards = append(b.cards, card)
}

// pipelineIssueParentID returns the planner provenance pointer for an issue.
func pipelineIssueParentID(iss *native.Issue) string {
	if iss == nil {
		return ""
	}
	if id := strings.TrimSpace(iss.ParentID); id != "" {
		return id
	}
	if iss.BotArgs != nil {
		return strings.TrimSpace(iss.BotArgs[native.BotArgSpawnedFrom])
	}
	return ""
}

// pipelineIssueRole returns bot_args.role when set.
func pipelineIssueRole(iss *native.Issue) string {
	if iss == nil || iss.BotArgs == nil {
		return ""
	}
	return strings.TrimSpace(iss.BotArgs[native.BotArgRole])
}

// issueIsBoardClosed reports whether a ticket is "closed" on the pipeline
// board: it has a terminal root run, or (no run) sits in a terminal issue
// state. Draft/ready/in-progress tickets without a terminal run stay open.
func (b *pipelineProjectionBuilder) issueIsBoardClosed(iss *native.Issue) bool {
	if iss == nil {
		return true
	}
	if root := b.currentRunForIssue(iss); root != nil {
		return root.Status.IsTerminal()
	}
	_, term := b.terminalStates[iss.State]
	return term
}

// enrichParentChildLinks fills ParentIssueID / Children / ChildrenSummary /
// Role on every issue-backed card from the native issue graph.
func (b *pipelineProjectionBuilder) enrichParentChildLinks() {
	if len(b.allIssues) == 0 || len(b.cards) == 0 {
		return
	}
	issueByID := make(map[string]*native.Issue, len(b.allIssues))
	childrenByParent := map[string][]*native.Issue{}
	for _, iss := range b.allIssues {
		if iss == nil {
			continue
		}
		issueByID[iss.ID] = iss
		if pid := pipelineIssueParentID(iss); pid != "" {
			childrenByParent[pid] = append(childrenByParent[pid], iss)
		}
	}
	cardByIssue := make(map[string]int, len(b.cards))
	for i := range b.cards {
		if id := b.cards[i].IssueID; id != "" {
			cardByIssue[id] = i
		}
	}
	for i := range b.cards {
		c := &b.cards[i]
		if c.IssueID == "" {
			continue
		}
		iss := issueByID[c.IssueID]
		if iss == nil {
			continue
		}
		pid := pipelineIssueParentID(iss)
		c.ParentIssueID = pid
		if pid != "" {
			if p := issueByID[pid]; p != nil {
				c.ParentTitle = p.Title
			}
		}
		kids := childrenByParent[c.IssueID]
		if len(kids) == 0 {
			if c.Role == "" && pid != "" {
				c.Role = "producer"
			}
			continue
		}
		if c.Role == "" {
			c.Role = "planner"
		}
		// Stable order: priority desc, then created asc (same as board).
		sort.SliceStable(kids, func(a, b int) bool {
			if kids[a].Priority != kids[b].Priority {
				return kids[a].Priority > kids[b].Priority
			}
			return kids[a].CreatedAt.Before(kids[b].CreatedAt)
		})
		summary := &PipelineBoardChildrenSummary{Total: len(kids)}
		refs := make([]PipelineBoardChildRef, 0, len(kids))
		for _, k := range kids {
			ref := PipelineBoardChildRef{
				IssueID: k.ID,
				Title:   k.Title,
				State:   k.State,
				BotID:   k.Bot,
			}
			if j, ok := cardByIssue[k.ID]; ok {
				child := b.cards[j]
				ref.CardID = child.ID
				switch {
				case child.ColumnID == pipelineColumnInProgress:
					summary.InProgress++
				case child.ColumnID == pipelineColumnClosed && child.Failed:
					summary.Failed++
				case child.ColumnID == pipelineColumnClosed:
					summary.Done++
				case child.Ready:
					summary.Ready++
				default:
					summary.Open++
				}
			} else {
				// Child not on board (no bot) — count from issue state.
				switch k.State {
				case native.StateDone:
					summary.Done++
				case native.StateInProgress, native.StateAwaitingInput, native.StateReview:
					summary.InProgress++
				case native.StateBlocked:
					summary.Failed++
				case native.StateReady:
					summary.Ready++
				default:
					summary.Open++
				}
			}
			refs = append(refs, ref)
		}
		c.Children = refs
		c.ChildrenSummary = summary
	}
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
				UpdatedAt:     b.pendingReviewUpdatedAt(run),
				Instructions:  run.Checkpoint.InteractionInstructions,
				ReviewBrief:   cloneHumanReviewBrief(run.Checkpoint.InteractionReviewBrief),
				Media:         append([]store.ReviewMediaRef(nil), run.Checkpoint.InteractionMedia...),
				Review:        cloneReviewGateState(run.Checkpoint.InteractionReview),
				Depth:         depth,
			})
		}
		for _, child := range b.children[run.ID] {
			walk(child, depth+1)
		}
	}
	walk(root, 0)
	// FIFO by the time each pending turn was requested. In particular, a
	// review gate that resumes and re-pauses is a new turn and belongs at the
	// back of the queue, even though its run keeps the same place in the tree.
	sort.SliceStable(reviews, func(i, j int) bool {
		a, c := reviews[i], reviews[j]
		if !a.UpdatedAt.Equal(c.UpdatedAt) {
			return a.UpdatedAt.Before(c.UpdatedAt)
		}
		if a.RunID != c.RunID {
			return a.RunID < c.RunID
		}
		if a.NodeID != c.NodeID {
			return a.NodeID < c.NodeID
		}
		return a.InteractionID < c.InteractionID
	})
	return
}

// pendingReviewUpdatedAt returns the enqueue time of the current pending
// interaction. Interaction.RequestedAt is precise for guided review gates:
// every new companion turn rewrites the stable interaction ID with a fresh
// request timestamp. Legacy/missing interactions fall back to the run stamp.
func (b *pipelineProjectionBuilder) pendingReviewUpdatedAt(run *store.Run) time.Time {
	if run == nil {
		return time.Time{}
	}
	if b.rs != nil && run.Checkpoint != nil && run.Checkpoint.InteractionID != "" {
		interaction, err := b.rs.LoadInteraction(b.ctx, run.ID, run.Checkpoint.InteractionID)
		if err == nil && interaction != nil && !interaction.RequestedAt.IsZero() {
			return interaction.RequestedAt
		}
	}
	if !run.UpdatedAt.IsZero() {
		return run.UpdatedAt
	}
	return run.CreatedAt
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
		// Waiting for a local concurrency slot — not yet executing, so it
		// stays in Todo (the studio badges it Ready — it is cleared to run).
		return pipelineColumnTodo
	case store.RunStatusFinished:
		return pipelineColumnClosed
	case store.RunStatusRunning, store.RunStatusPausedWaitingHuman, store.RunStatusPausedOperator:
		// An operator soft-pause is a RESUMABLE mid-flight state (the run
		// console offers Resume), not a failure — it stays In progress with
		// its "paused" status chip rather than landing in Closed with a
		// Retry-from-zero affordance.
		return pipelineColumnInProgress
	case store.RunStatusFailed, store.RunStatusFailedResumable, store.RunStatusCancelled:
		// A failed/cancelled run lands in the CLOSED lane (with its error as
		// the reason, flagged failed) until the operator retries it to Todo.
		return pipelineColumnClosed
	default:
		return pipelineColumnInProgress
	}
}

// pipelineRunFailed reports whether a run status marks a card failed (as
// opposed to a successfully-finished one — both share the Closed lane).
func pipelineRunFailed(status store.RunStatus) bool {
	switch status {
	case store.RunStatusFailed, store.RunStatusFailedResumable, store.RunStatusCancelled:
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

func cloneReviewGateState(in *store.ReviewGateState) *store.ReviewGateState {
	if in == nil {
		return nil
	}
	out := *in
	out.Verdict = cloneAnyMap(in.Verdict)
	if len(in.Turns) > 0 {
		out.Turns = make([]store.InteractionTurn, len(in.Turns))
		copy(out.Turns, in.Turns)
		for i := range out.Turns {
			out.Turns[i].Verdict = cloneAnyMap(in.Turns[i].Verdict)
			out.Turns[i].Media = append([]store.ReviewMediaRef(nil), in.Turns[i].Media...)
		}
	}
	return &out
}

func cloneHumanReviewBrief(in *store.HumanReviewBrief) *store.HumanReviewBrief {
	if in == nil {
		return nil
	}
	return &store.HumanReviewBrief{
		Version: in.Version,
		Source:  in.Source,
		Points:  append([]string(nil), in.Points...),
	}
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

// compactPipelineTitle turns any selected label into a bounded, single-line
// card title. Inputs can legitimately contain entire Markdown briefs; those
// belong in EntryInput, not in the board title or its JSON payload.
func compactPipelineTitle(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			s = line
			break
		}
	}
	s = strings.Join(strings.Fields(s), " ")
	runes := []rune(s)
	if len(runes) <= pipelineTitleMaxRunes {
		return s
	}
	return strings.TrimSpace(string(runes[:pipelineTitleMaxRunes-1])) + "…"
}

// pipelineDisplayTitle picks the label shown on a pipeline card.
//
// Priority:
//  1. Content-derived title from bot inputs / bot_args (explains WHAT the
//     pipeline is producing — e.g. "Boudicca · ÉP 1/5 — Le Fouet et le Serment")
//  2. Native ticket title (operator-authored)
//  3. Bundle display name / humanized workflow name
//  4. Run codename (GenerateRunName) — last resort; unique but opaque
//
// Run codenames are intentionally NOT preferred: they are branch/ID helpers,
// not content labels. See titleFromContentInputs for the input key heuristics.
func pipelineDisplayTitle(issue *native.Issue, root *store.Run) string {
	var inputs map[string]any
	if root != nil && len(root.Inputs) > 0 {
		inputs = root.Inputs
	} else if issue != nil && len(issue.BotArgs) > 0 {
		inputs = stringMapToAny(issue.BotArgs)
	}
	if t := titleFromContentInputs(inputs); t != "" {
		return compactPipelineTitle(t)
	}
	if issue != nil {
		if t := strings.TrimSpace(issue.Title); t != "" {
			return compactPipelineTitle(t)
		}
	}
	if root != nil {
		if t := strings.TrimSpace(root.BundleDisplayName); t != "" {
			return compactPipelineTitle(t)
		}
		if t := humanizePipelineName(root.WorkflowName); t != "" && t != "Pipeline" {
			return compactPipelineTitle(t)
		}
		if t := strings.TrimSpace(root.Name); t != "" {
			return compactPipelineTitle(t)
		}
	}
	return "Pipeline"
}

// titleFromContentInputs builds a human content label from common bot input
// keys. Prefer structured subject + episode framing (shorts / series bots)
// over free-form prose. Returns "" when inputs have nothing usable so callers
// can fall through to ticket title / run name.
//
// Recognised patterns (first match wins on each slot):
//
//	subject:  character | requested_character | subject | family | family_name |
//	          asset_name | collection | series
//	episode:  episode_no (+ episode_total) | ep / episode
//	title:    episode_title | title | topic | feature | name (when not a path)
//
// Example: character=Boudicca, episode_no=1, episode_total=5,
// episode_title="Le Fouet et le Serment"
// → "Boudicca · ÉP 1/5 — Le Fouet et le Serment"
func titleFromContentInputs(inputs map[string]any) string {
	if len(inputs) == 0 {
		return ""
	}
	get := func(keys ...string) string {
		for _, k := range keys {
			v, ok := inputs[k]
			if !ok || v == nil {
				continue
			}
			s := strings.TrimSpace(fmt.Sprint(v))
			if s == "" || s == "<nil>" {
				continue
			}
			return s
		}
		return ""
	}

	subject := get(
		"character", "requested_character", "subject",
		"family", "family_id", "family_name", "asset_name", "collection", "series",
	)
	// Planners often only carry a path — use the file stem as subject
	// (e.g. assets/.../boudicca.json → boudicca).
	if subject == "" {
		if p := get("input_path", "catalog_path", "output_dir"); p != "" {
			base := filepath.Base(strings.TrimRight(p, "/\\"))
			base = strings.TrimSuffix(base, filepath.Ext(base))
			base = strings.TrimSpace(base)
			if base != "" && !looksLikeMachineToken(base) {
				subject = humanizePipelineName(base)
			}
		}
	}
	epNo := get("episode_no", "ep", "episode")
	// episode_index is often 0-based; only use it when episode_no is absent,
	// and prefer the 1-based display the operator expects (index+1 when numeric).
	if epNo == "" {
		if idx := get("episode_index"); idx != "" {
			epNo = episodeIndexAsOneBased(idx)
		}
	}
	epTotal := get("episode_total", "episodes", "episode_count")
	epTitle := get("episode_title", "title", "topic", "feature")
	// "name" is common but often a machine id / path — only use when it looks
	// human (no slash, not a bare uuid-ish token) and we still have nothing.
	if epTitle == "" {
		if n := get("name"); n != "" && !looksLikeMachineToken(n) {
			epTitle = n
		}
	}

	// Full shorts-style frame: Subject · ÉP n/N — Title
	if epNo != "" {
		epLabel := "ÉP " + epNo
		if epTotal != "" {
			epLabel = fmt.Sprintf("ÉP %s/%s", epNo, epTotal)
		}
		switch {
		case subject != "" && epTitle != "":
			return fmt.Sprintf("%s · %s — %s", subject, epLabel, epTitle)
		case subject != "":
			return fmt.Sprintf("%s · %s", subject, epLabel)
		case epTitle != "":
			return fmt.Sprintf("%s — %s", epLabel, epTitle)
		default:
			return epLabel
		}
	}

	if subject != "" && epTitle != "" && subject != epTitle {
		return fmt.Sprintf("%s — %s", subject, epTitle)
	}
	if epTitle != "" {
		return epTitle
	}
	if subject != "" {
		return subject
	}

	// Last-resort content: a short prose field, truncated so it can't blow the card.
	for _, k := range []string{"hook", "angle", "summary", "place", "period"} {
		if s := get(k); s != "" {
			return pipelineTruncate(s, 80)
		}
	}
	return ""
}

// episodeIndexAsOneBased turns a 0-based episode_index into a 1-based display
// number when the value is a plain integer; non-numeric values pass through.
func episodeIndexAsOneBased(idx string) string {
	var n int
	if _, err := fmt.Sscanf(idx, "%d", &n); err == nil {
		// Heuristic: indices start at 0 in many bots; if the value is already
		// ≥1 leave it (some bots store 1-based in episode_index).
		if n == 0 {
			return "1"
		}
		// For n>=1 we can't know base — keep as-is (callers prefer episode_no).
		return fmt.Sprintf("%d", n)
	}
	return idx
}

// looksLikeMachineToken rejects values that are paths, UUIDs, or codenames
// from being used as a human title fragment.
func looksLikeMachineToken(s string) bool {
	if strings.ContainsAny(s, "/\\") {
		return true
	}
	if strings.Count(s, "-") >= 3 && !strings.Contains(s, " ") {
		// e.g. run codenames orbital-plunge-borealroar-707f
		return true
	}
	// Hex-ish uuid fragments
	if len(s) >= 32 {
		hexish := true
		for _, r := range s {
			if r == '-' {
				continue
			}
			if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
				hexish = false
				break
			}
		}
		if hexish {
			return true
		}
	}
	return false
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
