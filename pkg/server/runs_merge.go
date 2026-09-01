package server

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/SocialGouv/iterion/pkg/forge"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/secrets"
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
	s.maybeTransitionMergedIssue(r.Context(), id, res.SourceIssueID)
	s.writeJSONFor(w, r, mergeRunResponse{
		RunID:         id,
		MergedCommit:  res.MergedCommit,
		MergedInto:    res.MergedInto,
		MergeStrategy: res.MergeStrategy,
		MergeStatus:   res.MergeStatus,
	})
}

// commitAndFinalizeRequest is the body of POST
// /api/runs/{id}/commit-and-finalize. Mirrors mergeRunRequest's
// shape — the operator supplies the commit message; everything
// else is derived from the run record.
type commitAndFinalizeRequest struct {
	CommitMessage string `json:"commit_message"`
}

// handleCommitAndFinalize stages and commits a run's uncommitted
// workdir changes, then promotes the new HEAD onto a persistent
// branch via the standard finalize path. After it returns the
// existing /merge endpoint takes over — the response shape carries
// enough state for the studio to pivot to its merge UX without an
// extra GET. Preconditions surface as 409 (the workdir is preserved
// in every failure mode so retries are safe).
func (s *Server) handleCommitAndFinalize(w http.ResponseWriter, r *http.Request) {
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
	var req commitAndFinalizeRequest
	if err := readJSON(r, &req); err != nil {
		s.httpErrorFor(w, r, http.StatusBadRequest, "invalid request: %v", err)
		return
	}
	res, err := s.runs.CommitAndFinalizeCtx(r.Context(), id, req.CommitMessage)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusConflict, "commit-and-finalize: %v", err)
		return
	}
	// When finalize auto-FF'd the commit straight onto the target branch
	// (MergeStatus="merged"), there's no follow-up /merge click — so this
	// is the only place that can close the source issue. The common path
	// (status flips to "pending") leaves the issue alone; /merge handles
	// it after the operator confirms.
	if res.MergeStatus == store.MergeStatusMerged {
		s.maybeTransitionMergedIssue(r.Context(), id, res.SourceIssueID)
	}
	s.writeJSONFor(w, r, res)
}

// forgeTokenForRun is the runview.ForgeTokenResolver for repo-targeted
// merges: it opens the managed forge secret the launch pinned on the run
// (SecretOverrides["forge_token"]), re-minting it first when it belongs to
// a github_app connection — those installation tokens live one hour, so a
// stored value is usually dead by merge time (same point-of-use re-mint
// the launch path does).
func (s *Server) forgeTokenForRun(ctx context.Context, r *store.Run) (string, error) {
	if r == nil || r.RepoURL == "" {
		return "", nil
	}
	secID := r.SecretOverrides["forge_token"]
	if secID == "" {
		return "", fmt.Errorf("run %s is repo-targeted but pins no forge_token secret — its connection could not be resolved at launch", r.ID)
	}
	o := s.forgeOrchestrator
	if o == nil || o.Secrets == nil || o.Sealer == nil {
		return "", fmt.Errorf("forge integrations are not wired on this server — cannot open the run's forge token")
	}
	tctx := store.WithTenant(ctx, r.TenantID)
	conns, err := o.Connections.ListByTenant(tctx, r.TenantID)
	if err != nil {
		// A listing failure would silently skip the re-mint and surface
		// later as an opaque push-auth error — fail here, with the cause.
		return "", fmt.Errorf("list forge connections for the merge of %s: %w", r.ID, err)
	}
	var conn *forge.Connection
	for i := range conns {
		if conns[i].ManagedSecretID == secID {
			conn = &conns[i]
			break
		}
	}
	if conn == nil {
		// The pinned secret can outlive its connection (forge re-linked
		// since launch) and the stored token may still be live, so this
		// is a visible degradation, not a refusal.
		s.logWarn("merge: no connection backs forge token secret %s (run %s) — using the stored token without re-mint", secID, r.ID)
	} else {
		// The token authenticates to the connection's forge host; refuse
		// to open it for a run whose RepoURL points elsewhere (a
		// mismatch means the run doc drifted from what launch resolved).
		base := conn.ForgeBaseURL
		if base == "" {
			base = forge.DefaultBaseURL(conn.Provider)
		}
		if !sameForgeHost(base, r.RepoURL) {
			return "", fmt.Errorf("run %s targets %s but its pinned forge connection authenticates to %s — refusing to open the token", r.ID, r.RepoURL, base)
		}
		// Best-effort: a mint failure keeps the stored token,
		// which may still be live within its hour.
		_, _ = o.EnsureManagedSecret(tctx, conn, "merge")
	}
	gs, err := o.Secrets.Get(tctx, secID)
	if err != nil {
		return "", fmt.Errorf("forge token secret %s: %w", secID, err)
	}
	raw, err := secrets.OpenGenericSecret(o.Sealer, gs.ID, gs.SealedSecret)
	if err != nil {
		return "", fmt.Errorf("unseal forge token %s: %w", secID, err)
	}
	return string(raw), nil
}

// maybeTransitionMergedIssue fires the dispatcher's MergedState
// transition for a run's source issue when the merge succeeds. Silent
// no-op when:
//   - no Dispatcher is wired (CLI/cloud variants without an actor),
//   - issueID is empty (run wasn't dispatcher-spawned),
//   - or the dispatcher's MergedState config is empty / "none".
//
// The caller supplies issueID from MergeResponse.SourceIssueID so this
// path never hits the store — the merge handler already loaded and
// persisted the run.
//
// Tracker errors are logged but never propagated — the merge response
// stays clean, and the operator can move the issue manually if the
// auto-transition didn't land.
func (s *Server) maybeTransitionMergedIssue(ctx context.Context, runID, issueID string) {
	if s.cfg.Dispatcher == nil || issueID == "" {
		return
	}
	if err := s.cfg.Dispatcher.TransitionMergedIssue(ctx, issueID); err != nil {
		s.logger.Warn("server: post-merge issue transition (run=%s, issue=%s): %v", runID, issueID, err)
	}
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
	s.maybeTransitionMergedIssue(r.Context(), id, res.SourceIssueID)
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

// sameForgeHost reports whether two URLs point at the same forge host
// (case-insensitive hostname comparison; ports and paths ignored — a
// connection's base URL is canonicalised to scheme+host at connect
// time).
func sameForgeHost(baseURL, repoURL string) bool {
	b, err := url.Parse(baseURL)
	if err != nil || b.Hostname() == "" {
		return false
	}
	r, err := url.Parse(repoURL)
	if err != nil || r.Hostname() == "" {
		return false
	}
	return strings.EqualFold(b.Hostname(), r.Hostname())
}
