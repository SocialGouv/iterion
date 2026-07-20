package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/SocialGouv/iterion/internal/httpx"
	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
)

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
	// External links the task to a forge repo (same shape as the native
	External *native.ExternalRef `json:"external,omitempty"`
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
		Bot:      entry.Name,
		BotArgs:  cloneStringMap(req.BotArgs),
		External: req.External,
	})
	if err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "pipeline board task: create: %v", err)
		return
	}
	s.reflectAllowedOrigin(w, r)
	httpx.WriteJSON(w, http.StatusCreated, issue)
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
		candidate := fmt.Sprintf("#%d - %s", n, desired)
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
	Blockers *[]string           `json:"blockers,omitempty"`
	External *native.ExternalRef `json:"external,omitempty"`
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
		External: req.External,
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
	s.writeJSONFor(w, r, issue)
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
	s.writeJSONFor(w, r, issue)
}
