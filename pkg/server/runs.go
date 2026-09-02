package server

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/SocialGouv/iterion/pkg/store"
)

// registerRunRoutes wires the /api/runs surface onto the server's
// mux. Called from routes() after the studio endpoints so the run
// console is opt-in: a server constructed without a store dir
// (s.runs == nil) silently skips registration and behaves exactly
// like the studio-only build.
func (s *Server) registerRunRoutes() {
	if s.runs == nil {
		return
	}
	s.mux.HandleFunc("GET /api/runs", s.handleListRuns)
	s.mux.HandleFunc("GET /api/runs/global-active", s.handleListGlobalActiveRuns)
	s.mux.HandleFunc("POST /api/runs", s.handleLaunchRun)
	s.mux.HandleFunc("POST /api/runs/preview-cost", s.handlePreviewCost)
	s.mux.HandleFunc("POST /api/runs/uploads", s.handleUploadAttachment)
	s.mux.HandleFunc("GET /api/runs/{id}/attachments/{name}", s.handleServeAttachment)
	s.mux.HandleFunc("GET /api/runs/{id}/attachments/{name}/url", s.handlePresignAttachment)
	s.mux.HandleFunc("GET /api/server/info", s.handleServerInfo)
	s.mux.HandleFunc("GET /api/runs/{id}", s.handleGetRun)
	s.mux.HandleFunc("GET /api/runs/{id}/children", s.handleListRunChildren)
	s.mux.HandleFunc("GET /api/runs/{id}/events", s.handleGetRunEvents)
	s.mux.HandleFunc("GET /api/runs/{id}/workflow", s.handleGetRunWorkflow)
	s.mux.HandleFunc("GET /api/runs/{id}/artifacts", s.handleListAllArtifacts)
	s.mux.HandleFunc("GET /api/runs/{id}/artifacts/{node}", s.handleListArtifacts)
	s.mux.HandleFunc("GET /api/runs/{id}/artifacts/{node}/{version}", s.handleGetArtifact)
	s.mux.HandleFunc("GET /api/runs/{id}/tools/{toolUseID}/{kind}", s.handleGetToolBlob)
	s.mux.HandleFunc("GET /api/runs/{id}/plans", s.handleListPlans)
	s.mux.HandleFunc("GET /api/runs/{id}/notes", s.handleListNotes)
	s.mux.HandleFunc("POST /api/runs/{id}/notes", s.handleAddNote)
	s.mux.HandleFunc("GET /api/runs/{id}/tags", s.handleGetRunTags)
	s.mux.HandleFunc("PUT /api/runs/{id}/tags", s.handleSetRunTags)
	s.mux.HandleFunc("GET /api/runs/{id}/artifact-files", s.handleListArtifactFiles)
	s.mux.HandleFunc("GET /api/runs/{id}/artifact-files/{path...}", s.handleGetArtifactFile)
	s.mux.HandleFunc("GET /api/runs/{id}/files", s.handleListRunFiles)
	s.mux.HandleFunc("GET /api/runs/{id}/files/touched", s.handleListRunTouchedFiles)
	s.mux.HandleFunc("GET /api/runs/{id}/files/diff", s.handleGetRunFileDiff)
	// Per-node file changes — "an iterion node is like a commit".
	s.mux.HandleFunc("GET /api/runs/{id}/nodes/{node}/changes", s.handleGetRunNodeChanges)
	s.mux.HandleFunc("GET /api/runs/{id}/nodes/{node}/diff", s.handleGetRunNodeFileDiff)
	s.mux.HandleFunc("GET /api/runs/{id}/route-decisions", s.handleListRouteDecisions)
	s.mux.HandleFunc("GET /api/runs/{id}/review/scope", s.handleGetRunReviewScope)
	s.mux.HandleFunc("GET /api/runs/{id}/review/diff", s.handleGetRunReviewDiff)
	// Workspace-relative file stream for the review panel's media players
	// (audio / video / image). Path-validated; never trusts raw query paths.
	s.mux.HandleFunc("GET /api/runs/{id}/workspace-files/{path...}", s.handleGetRunWorkspaceFile)
	s.mux.HandleFunc("GET /api/runs/{id}/files/content", s.handleGetRunFileContent)
	s.mux.HandleFunc("PUT /api/runs/{id}/files/content", s.handleSaveRunFileContent)
	s.mux.HandleFunc("GET /api/runs/{id}/commits", s.handleListRunCommits)
	s.mux.HandleFunc("GET /api/runs/{id}/commits/{sha}", s.handleGetRunCommit)
	s.mux.HandleFunc("GET /api/runs/{id}/commits/{sha}/diff", s.handleGetRunCommitFileDiff)
	s.mux.HandleFunc("POST /api/runs/{id}/cancel", s.handleCancelRun)
	s.mux.HandleFunc("POST /api/runs/{id}/pause", s.handlePauseRun)
	s.mux.HandleFunc("POST /api/runs/{id}/bump-loop", s.handleBumpLoop)
	s.mux.HandleFunc("POST /api/runs/{id}/raise-budget", s.handleRaiseBudget)
	s.mux.HandleFunc("POST /api/runs/{id}/answer-human", s.handleAnswerHuman)
	s.mux.HandleFunc("GET /api/runs/{id}/interactions/pending", s.handleListPendingInteractions)
	s.mux.HandleFunc("POST /api/runs/{id}/interactions/{iid}/answer", s.handleAnswerInteraction)
	s.mux.HandleFunc("POST /api/runs/{id}/fork", s.handleForkRun)
	s.mux.HandleFunc("POST /api/runs/{id}/rewind", s.handleRewindRun)
	s.mux.HandleFunc("GET /api/runs/{id}/skills", s.handleListRunSkills)
	s.mux.HandleFunc("GET /api/runs/{id}/session-board", s.handleGetSessionBoard)
	s.mux.HandleFunc("GET /api/runs/{id}/queue-messages", s.handleListQueuedMessages)
	s.mux.HandleFunc("POST /api/runs/{id}/queue-message", s.handleQueueMessage)
	s.mux.HandleFunc("DELETE /api/runs/{id}/queue-message/{msgID}", s.handleCancelQueuedMessage)
	s.mux.HandleFunc("POST /api/runs/{id}/watch/{issueID}", s.handleAddWatch)
	s.mux.HandleFunc("DELETE /api/runs/{id}/watch/{issueID}", s.handleRemoveWatch)
	s.mux.HandleFunc("POST /api/runs/{id}/resume", s.handleResumeRun)
	s.mux.HandleFunc("POST /api/runs/{id}/merge", s.handleMergeRun)
	s.mux.HandleFunc("POST /api/runs/{id}/commit-and-finalize", s.handleCommitAndFinalize)
	s.mux.HandleFunc("GET /api/runs/{id}/merge/conflicts", s.handleGetMergeConflicts)
	s.mux.HandleFunc("POST /api/runs/{id}/merge/conflicts/resolve", s.handleResolveMergeConflict)
	s.mux.HandleFunc("POST /api/runs/{id}/merge/conflicts/resolve-with-agent", s.handleResolveConflictWithAgent)
	s.mux.HandleFunc("POST /api/runs/{id}/merge/conflicts/finalize", s.handleFinalizeMergeConflict)
	s.mux.HandleFunc("POST /api/runs/{id}/merge/conflicts/abort", s.handleAbortMergeConflict)
	s.mux.HandleFunc("POST /api/runs/{id}/rename", s.handleRenameRun)
	s.mux.HandleFunc("DELETE /api/runs/{id}", s.handleDeleteRun)
	s.mux.HandleFunc("GET /api/ws/runs/{id}", s.handleRunWebSocket)
	s.mux.HandleFunc("GET /api/ws/runs/{id}/shell", s.handleRunShell)
	s.mux.HandleFunc("GET /api/runs/{id}/preview", s.handlePreviewProxy)
	s.mux.HandleFunc("GET /api/runs/{id}/browser/cdp", s.handleBrowserCDP)
	s.mux.HandleFunc("POST /api/runs/{id}/browser/attach", s.handleBrowserAttach)
}

// resolveCrossStore inspects the `?store=` query parameter and, when
// it's a permitted iterion store path under $HOME/.iterion/, returns a
// fresh read-only RunStore rooted there. Used by the read-only run
// endpoints so the desktop banner can deep-link into a run living in a
// different store (typically the global ~/.iterion/runs/ slot, or a
// per-project store not currently attached) without spawning a
// dedicated daemon.
//
// Returns (nil, "", nil) when ?store= is absent → callers fall through
// to the daemon's primary s.runs Service.
//
// Security: the path MUST resolve under $HOME/.iterion/ after symlink
// resolution; anything else is rejected with a clear error so a
// malicious ?store=/etc/.. can't read arbitrary host paths.
func (s *Server) resolveCrossStore(r *http.Request) (store.RunStore, string, error) {
	raw := r.URL.Query().Get("store")
	if raw == "" {
		return nil, "", nil
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return nil, "", fmt.Errorf("cross-store: $HOME not resolvable")
	}
	abs, err := filepath.Abs(raw)
	if err != nil {
		return nil, "", fmt.Errorf("cross-store: invalid path: %w", err)
	}
	// Symlink-safe containment check.
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, "", fmt.Errorf("cross-store: resolve %s: %w", abs, err)
	}
	allowedRoot, err := filepath.EvalSymlinks(filepath.Join(home, ".iterion"))
	if err != nil {
		return nil, "", fmt.Errorf("cross-store: resolve allowed root: %w", err)
	}
	if resolved != allowedRoot && !strings.HasPrefix(resolved, allowedRoot+string(filepath.Separator)) {
		return nil, "", fmt.Errorf("cross-store: %q is outside $HOME/.iterion/ — refused", raw)
	}
	rs, err := store.New(resolved)
	if err != nil {
		return nil, "", fmt.Errorf("cross-store: open %s: %w", resolved, err)
	}
	return rs, resolved, nil
}

// rejectCrossStoreWrite returns true (and writes 409 cross_store_readonly)
// when the request carries ?store= — symmetric to the WS handlers'
// rejection of cancel/answer on cross-store connections. Callers must
// `return` immediately when this returns true. The path itself isn't
// re-validated here (resolveCrossStore covers that on the read paths);
// any write attempt with ?store= set is refused, on the principle that
// only the owning daemon may mutate a run.
func (s *Server) rejectCrossStoreWrite(w http.ResponseWriter, r *http.Request) bool {
	if r.URL.Query().Get("store") == "" {
		return false
	}
	s.httpErrorFor(w, r, http.StatusConflict,
		"cross_store_readonly: this operation is not available for cross-store runs — open the owning daemon")
	return true
}
