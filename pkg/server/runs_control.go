package server

import (
	"errors"
	"net/http"
	"strings"

	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
)

type cancelRunResponse struct {
	RunID  string `json:"run_id"`
	Status string `json:"status"`
}

func (s *Server) handleCancelRun(w http.ResponseWriter, r *http.Request) {
	if !s.requireSafeOrigin(w, r) {
		return
	}
	if s.rejectCrossStoreWrite(w, r) {
		return
	}
	id := r.PathValue("id")
	if id == "" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "missing run id")
		return
	}
	// Tenant scoping: confirm the caller can see this run before any
	// cancel mutation. In cloud mode Cancel descends into the publisher
	// with a non-request context, so without this gate the mongo tenant
	// filter never runs and a cross-tenant run could be cancelled by id.
	// (Also rejects a malformed id via the store's path-component check.)
	if _, err := s.runs.LoadRunCtx(r.Context(), id); err != nil {
		s.httpErrorFor(w, r, http.StatusNotFound, "run not found: %v", err)
		return
	}
	// Log cancel intent with source attribution. Mystery context-canceled
	// failures during a run mid-flight typically trace back to either this
	// HTTP endpoint or the WS `cancel` envelope (handleCancel in runs_ws.go);
	// emitting a line per call site lets us tell the two apart without
	// instrumenting the runtime itself.
	if s.logger != nil {
		s.logger.Info("server: cancel run %q via HTTP from %s", id, r.RemoteAddr)
	}
	if err := s.runs.Cancel(id); err != nil {
		// If the run is not currently active in this process, the
		// operator's "cancel" intent depends on the persisted status:
		//   - dispatcher-spawned + running: the runview Manager only
		//     tracks manual studio launches, so cancel falls through
		//     here. The dispatcher owns its own cancel funcs keyed by
		//     runID — try that path before giving up.
		//   - already terminal (finished / failed / cancelled / merged):
		//     idempotent — return current state, no-op.
		//   - paused_waiting_human / failed_resumable: the operator
		//     wants to abandon the partial work. Flip the persisted
		//     status to cancelled, emit run_cancelled, and finalize the
		//     worktree so the studio's merge UI can act on whatever
		//     commits the run produced before it stalled.
		if errors.Is(err, runview.ErrRunNotActive) {
			if s.cfg.Dispatcher != nil && s.cfg.Dispatcher.CancelRun(id) {
				w.WriteHeader(http.StatusAccepted)
				s.writeJSONFor(w, r, cancelRunResponse{RunID: id, Status: "cancelling"})
				return
			}
			r2, loadErr := s.runs.LoadRunCtx(r.Context(), id)
			if loadErr != nil {
				s.httpErrorFor(w, r, http.StatusNotFound, "run not active and not on disk: %v", loadErr)
				return
			}
			if cancelled, cancelErr := s.runs.CancelInactiveCtx(r.Context(), id); cancelErr == nil && cancelled {
				w.WriteHeader(http.StatusAccepted)
				s.writeJSONFor(w, r, cancelRunResponse{RunID: id, Status: string(store.RunStatusCancelled)})
				return
			} else if cancelErr != nil {
				s.logger.Warn("server: cancel inactive run %s: %v", id, cancelErr)
			}
			w.WriteHeader(http.StatusAccepted)
			s.writeJSONFor(w, r, cancelRunResponse{RunID: id, Status: string(r2.Status)})
			return
		}
		s.httpErrorFor(w, r, http.StatusInternalServerError, "cancel: %v", err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
	s.writeJSONFor(w, r, cancelRunResponse{RunID: id, Status: "cancelling"})
}

// watchResponse is the body of the watch endpoints — the run's full
// subscription set after the mutation, so the studio can replace its
// local view without re-fetching the snapshot.
type watchResponse struct {
	RunID           string   `json:"run_id"`
	WatchedIssueIDs []string `json:"watched_issue_ids"`
}

// handleAddWatch subscribes a run to a native-kanban issue (MVP3b) so
// the watch coordinator forwards that issue's future board transitions
// to the run as queued messages.
func (s *Server) handleAddWatch(w http.ResponseWriter, r *http.Request) {
	s.mutateWatch(w, r, true)
}

// handleRemoveWatch unsubscribes a run from a native-kanban issue.
func (s *Server) handleRemoveWatch(w http.ResponseWriter, r *http.Request) {
	s.mutateWatch(w, r, false)
}

func (s *Server) mutateWatch(w http.ResponseWriter, r *http.Request, add bool) {
	if !s.requireSafeOrigin(w, r) {
		return
	}
	if s.rejectCrossStoreWrite(w, r) {
		return
	}
	id := r.PathValue("id")
	issueID := r.PathValue("issueID")
	if id == "" || issueID == "" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "missing run id or issue id")
		return
	}
	rs := s.runs.RunStore()
	var (
		watched []string
		err     error
	)
	if add {
		watched, err = rs.AddWatchedIssues(r.Context(), id, []string{issueID})
	} else {
		watched, err = rs.RemoveWatchedIssues(r.Context(), id, []string{issueID})
	}
	if err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "watch: %v", err)
		return
	}
	if watched == nil {
		watched = []string{}
	}
	s.writeJSONFor(w, r, watchResponse{RunID: id, WatchedIssueIDs: watched})
}

// pauseRunResponse is the body of POST /api/runs/{id}/pause. Mirrors
// cancelRunResponse — both expose a coarse client-friendly status
// snapshot the studio uses to update the RunHeader optimistically
// before the WS event arrives.
type pauseRunResponse struct {
	RunID  string `json:"run_id"`
	Status string `json:"status"`
}

func (s *Server) handlePauseRun(w http.ResponseWriter, r *http.Request) {
	if !s.requireSafeOrigin(w, r) {
		return
	}
	if s.rejectCrossStoreWrite(w, r) {
		return
	}
	id := r.PathValue("id")
	if id == "" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "missing run id")
		return
	}
	// Tenant scoping: confirm the caller can see this run before the
	// pause mutation (see handleCancelRun for the cloud-mode rationale).
	if _, err := s.runs.LoadRunCtx(r.Context(), id); err != nil {
		s.httpErrorFor(w, r, http.StatusNotFound, "run not found: %v", err)
		return
	}
	if s.logger != nil {
		s.logger.Info("server: pause run %q via HTTP from %s", id, r.RemoteAddr)
	}
	if err := s.runs.Pause(id); err != nil {
		if errors.Is(err, runview.ErrRunNotActive) {
			// 409 is the right code for "operator pause is meaningless
			// right now" — either the run is terminal or it's running
			// in another process (cloud mode). The studio hides the
			// Pause button in both cases; this is a defensive guard
			// against double-clicks racing with status changes.
			s.httpErrorFor(w, r, http.StatusConflict, "run is not active in this process")
			return
		}
		s.httpErrorFor(w, r, http.StatusInternalServerError, "pause: %v", err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
	s.writeJSONFor(w, r, pauseRunResponse{RunID: id, Status: "pause_requested"})
}

// forkRunRequest is the body of POST /api/runs/{id}/fork. Mirrors
// runview.ForkSpec but kept as a separate type so the HTTP wire shape
// stays decoupled from the service struct (we can deprecate fields
// without breaking ForkSpec consumers).
type forkRunRequest struct {
	NodeID     string         `json:"node_id"`
	TurnIndex  int            `json:"turn_index,omitempty"`
	RewindCode bool           `json:"rewind_code,omitempty"`
	ForkName   string         `json:"fork_name,omitempty"`
	NewInputs  map[string]any `json:"new_inputs,omitempty"`
}

func (s *Server) handleForkRun(w http.ResponseWriter, r *http.Request) {
	if !s.requireSafeOrigin(w, r) {
		return
	}
	if s.rejectCrossStoreWrite(w, r) {
		return
	}
	id := r.PathValue("id")
	if id == "" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "missing run id")
		return
	}
	var req forkRunRequest
	if err := readJSON(r, &req); err != nil {
		s.httpErrorFor(w, r, http.StatusBadRequest, "decode fork request: %v", err)
		return
	}
	if req.NodeID == "" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "node_id is required")
		return
	}
	if s.logger != nil {
		s.logger.Info("server: fork run %q at node %q from %s", id, req.NodeID, r.RemoteAddr)
	}
	result, err := s.runs.Fork(r.Context(), runview.ForkSpec{
		RunID:      id,
		NodeID:     req.NodeID,
		TurnIndex:  req.TurnIndex,
		RewindCode: req.RewindCode,
		ForkName:   req.ForkName,
		NewInputs:  req.NewInputs,
	})
	if err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "fork: %v", err)
		return
	}
	w.WriteHeader(http.StatusCreated)
	s.writeJSONFor(w, r, result)
}

func (s *Server) handleListQueuedMessages(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "missing run id")
		return
	}
	msgs, err := s.runs.ListQueuedMessages(r.Context(), id)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "list queued messages: %v", err)
		return
	}
	if msgs == nil {
		msgs = []store.QueuedUserMessage{}
	}
	s.writeJSONFor(w, r, map[string]any{"messages": msgs})
}

type queueMessageRequest struct {
	Text string `json:"text"`
	// Skills is the optional list of bundle skill names the operator
	// attached to this message. Each referenced SKILL.md is mirrored
	// into the run's .claude/skills/ before the engine injects the
	// message into the agent's conversation. Sticky — the skill stays
	// loaded for the rest of the run.
	Skills []string `json:"skills,omitempty"`
}

func (s *Server) handleQueueMessage(w http.ResponseWriter, r *http.Request) {
	if !s.requireSafeOrigin(w, r) {
		return
	}
	if s.rejectCrossStoreWrite(w, r) {
		return
	}
	id := r.PathValue("id")
	if id == "" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "missing run id")
		return
	}
	var req queueMessageRequest
	if err := readJSON(r, &req); err != nil {
		s.httpErrorFor(w, r, http.StatusBadRequest, "invalid request: %v", err)
		return
	}
	if strings.TrimSpace(req.Text) == "" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "text is required")
		return
	}
	var qopts []runview.QueueMessageOption
	if len(req.Skills) > 0 {
		qopts = append(qopts, runview.WithMessageSkills(req.Skills))
	}
	msg, err := s.runs.QueueMessage(r.Context(), id, req.Text, qopts...)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "queue message: %v", err)
		return
	}
	w.WriteHeader(http.StatusCreated)
	s.writeJSONFor(w, r, msg)
}

func (s *Server) handleCancelQueuedMessage(w http.ResponseWriter, r *http.Request) {
	if !s.requireSafeOrigin(w, r) {
		return
	}
	if s.rejectCrossStoreWrite(w, r) {
		return
	}
	id := r.PathValue("id")
	msgID := r.PathValue("msgID")
	if id == "" || msgID == "" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "missing run id or message id")
		return
	}
	if err := s.runs.CancelQueuedMessage(r.Context(), id, msgID); err != nil {
		switch {
		case errors.Is(err, store.ErrQueuedMessageNotFound):
			s.httpErrorFor(w, r, http.StatusNotFound, "queued message not found")
		case errors.Is(err, store.ErrQueuedMessageStatusConflict):
			s.httpErrorFor(w, r, http.StatusConflict, "queued message already delivered or cancelled")
		default:
			s.httpErrorFor(w, r, http.StatusInternalServerError, "cancel queued message: %v", err)
		}
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
