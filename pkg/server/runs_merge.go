package server

import (
	"net/http"

	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
)

// mergeRunRequest is the body of POST /api/runs/{id}/merge. Fields map
// directly onto runview.MergeRequest; the indirection keeps the wire
// shape decoupled from the internal type so we can add UI-only fields
// (e.g. dryRun preview) without leaking them through the service.
type mergeRunRequest struct {
	MergeStrategy string `json:"merge_strategy,omitempty"`
	MergeInto     string `json:"merge_into,omitempty"`
	CommitMessage string `json:"commit_message,omitempty"`
}

// mergeRunResponse echoes the persisted state after a successful merge,
// so the studio can update its local snapshot without an extra GET.
type mergeRunResponse struct {
	RunID         string              `json:"run_id"`
	MergedCommit  string              `json:"merged_commit"`
	MergedInto    string              `json:"merged_into"`
	MergeStrategy store.MergeStrategy `json:"merge_strategy"`
	MergeStatus   store.MergeStatus   `json:"merge_status"`
}

// handleMergeRun applies a UI-driven squash/merge against the run's
// persisted storage branch. Preconditions are validated by the service
// (FinalCommit/FinalBranch present, not already merged) and surface as
// 4xx; merge guards (target == current branch, clean working tree, FF
// ancestry) surface as 409 — the storage branch is preserved in either
// case so the user can retry after fixing the underlying repo state.
func (s *Server) handleMergeRun(w http.ResponseWriter, r *http.Request) {
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
	var req mergeRunRequest
	if err := readJSON(r, &req); err != nil {
		s.httpErrorFor(w, r, http.StatusBadRequest, "invalid request: %v", err)
		return
	}

	res, err := s.runs.PerformMergeCtx(r.Context(), id, runview.MergeRequest{
		Strategy:      store.MergeStrategy(req.MergeStrategy),
		MergeInto:     req.MergeInto,
		CommitMessage: req.CommitMessage,
	})
	if err != nil {
		// The service's error messages are descriptive enough to surface
		// directly; the studio renders them as a toast. 409 conveys
		// "preconditions not met / guard rejected" — the storage branch
		// still exists, so the failure is recoverable.
		s.httpErrorFor(w, r, http.StatusConflict, "merge: %v", err)
		return
	}
	s.writeJSONFor(w, r, mergeRunResponse{
		RunID:         id,
		MergedCommit:  res.MergedCommit,
		MergedInto:    res.MergedInto,
		MergeStrategy: res.MergeStrategy,
		MergeStatus:   res.MergeStatus,
	})
}

// ---------------------------------------------------------------------------
// Conflict-resolution endpoints
// ---------------------------------------------------------------------------

// handleGetMergeConflicts returns the structured conflict state for a
// run whose squash merge hit content conflicts. 404 when the run
// isn't conflicted (the studio shouldn't be calling here unless
// merge_status === "conflicted").
func (s *Server) handleGetMergeConflicts(w http.ResponseWriter, r *http.Request) {
	if !s.requireSafeOrigin(w, r) {
		return
	}
	id := r.PathValue("id")
	if id == "" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "missing run id")
		return
	}
	res, err := s.runs.GetMergeConflicts(r.Context(), id)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusNotFound, "conflicts: %v", err)
		return
	}
	s.writeJSONFor(w, r, res)
}

// resolveMergeConflictRequest carries one file's resolved content. The
// path identifies which conflicted file to resolve; the service-side
// validation rejects paths outside the current conflict set so this
// endpoint cannot be used to overwrite arbitrary files.
type resolveMergeConflictRequest struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// handleResolveMergeConflict accepts a resolved file payload and
// stages it via `git add`. 200 with a fresh conflict snapshot on
// success so the studio can update its accordion without a separate
// GET.
func (s *Server) handleResolveMergeConflict(w http.ResponseWriter, r *http.Request) {
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
	var req resolveMergeConflictRequest
	if err := readJSON(r, &req); err != nil {
		s.httpErrorFor(w, r, http.StatusBadRequest, "invalid request: %v", err)
		return
	}
	if req.Path == "" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "path required")
		return
	}
	if err := s.runs.ResolveMergeConflictFile(r.Context(), id, req.Path, req.Content); err != nil {
		s.httpErrorFor(w, r, http.StatusConflict, "resolve: %v", err)
		return
	}
	// Return the fresh state so the UI can reflect the now-staged file
	// without polling.
	res, err := s.runs.GetMergeConflicts(r.Context(), id)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "refresh: %v", err)
		return
	}
	s.writeJSONFor(w, r, res)
}

// resolveConflictWithAgentRequest is the body of the "resolve all
// with agent" endpoint. Empty body invokes the default resolver bot;
// callers can pin a specific model for the resolution.
type resolveConflictWithAgentRequest struct {
	// Model overrides the resolver bot's default. Empty uses the
	// bot's pinned model. Format: "<provider>/<model>" (claw spec).
	Model string `json:"model,omitempty"`
}

// handleResolveConflictWithAgent runs the LLM resolver against every
// conflicted file and returns the refreshed snapshot. 500 when no
// LLM credential is reachable — the studio surfaces the error
// verbatim so the operator knows to sign in.
func (s *Server) handleResolveConflictWithAgent(w http.ResponseWriter, r *http.Request) {
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
	var req resolveConflictWithAgentRequest
	_ = readJSON(r, &req) // body is optional
	res, err := s.runs.ResolveAllConflictsWithAgent(r.Context(), id, req.Model)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "agent resolve: %v", err)
		return
	}
	s.writeJSONFor(w, r, res)
}

// finalizeMergeConflictRequest accepts an optional message override
// that, when empty, falls back to the message captured on the run at
// conflict-time.
type finalizeMergeConflictRequest struct {
	Message string `json:"message,omitempty"`
}

// handleFinalizeMergeConflict commits the resolved squash merge.
// Returns the same shape as the conflict-free /merge endpoint so the
// studio can reuse its post-merge update path.
func (s *Server) handleFinalizeMergeConflict(w http.ResponseWriter, r *http.Request) {
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
	var req finalizeMergeConflictRequest
	_ = readJSON(r, &req)
	res, err := s.runs.FinalizeMergeAfterConflict(r.Context(), id, req.Message)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusConflict, "finalize: %v", err)
		return
	}
	s.writeJSONFor(w, r, mergeRunResponse{
		RunID:         id,
		MergedCommit:  res.MergedCommit,
		MergedInto:    res.MergedInto,
		MergeStrategy: res.MergeStrategy,
		MergeStatus:   res.MergeStatus,
	})
}

// handleAbortMergeConflict resets the worktree, clearing the
// conflict state. The run's merge_status flips back to "failed" so
// the operator can retry via the normal merge flow.
func (s *Server) handleAbortMergeConflict(w http.ResponseWriter, r *http.Request) {
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
	if err := s.runs.AbortMergeConflict(r.Context(), id); err != nil {
		s.httpErrorFor(w, r, http.StatusConflict, "abort: %v", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
