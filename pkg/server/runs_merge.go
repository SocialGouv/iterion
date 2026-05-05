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
// so the editor can update its local snapshot without an extra GET.
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

	res, err := s.runs.PerformMerge(id, runview.MergeRequest{
		Strategy:      store.MergeStrategy(req.MergeStrategy),
		MergeInto:     req.MergeInto,
		CommitMessage: req.CommitMessage,
	})
	if err != nil {
		// The service's error messages are descriptive enough to surface
		// directly; the editor renders them as a toast. 409 conveys
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
