package server

import (
	"net/http"
	"strings"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
)

// The pipeline board is a SINGLE global execution projection: one card per
// ROOT pipeline (a run with no parent), with every descendant folded into
// its root card as aggregate progress + a flat list of pending human
// reviews. It is a read model over the runtime + native tracker, not a
// second mutable store — cards are positioned by persisted run state, so
// there is no drag-and-drop. See docs/native-tracker.md + ADR-074.
const (
	// Four fixed lanes. "opened" folds backlog + ready staging (a per-card
	// `ready` badge distinguishes prepared-but-not-ready from launch-eligible);
	// "closed" folds done + cancelled (per-card success/failed). IDs are the
	// wire contract (filters, tests).
	//
	// "needs_attention" holds runs that DIED mid-flight and want a human. A
	// root card is in that lane iff its run status is failed or
	// failed_resumable, its tree has no pending human review, and — when the
	// card is ticket-backed — its ticket is in a non-terminal state. It is
	// deliberately narrower than the old failed bucket: a run the operator
	// CANCELLED is a decision, not an anomaly, and stays in Closed.
	//
	// The lane is load-bearing, not cosmetic: a card sitting in it RESERVES a
	// concurrency slot (see pipeline_reservations.go) so nothing else takes
	// the place it needs to restart into. That is why membership and
	// reservation are decided by one function, pipelineLaneForRoot — a card
	// that reserves without rendering here would be an invisible held slot.
	pipelineColumnOpened         = "opened"
	pipelineColumnInProgress     = "in_progress"
	pipelineColumnNeedsAttention = "needs_attention"
	pipelineColumnClosed         = "closed"

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

func (s *Server) registerPipelineBoardRoutes() {
	s.mux.Handle("GET /api/v1/pipeline-board", s.requireAuth(http.HandlerFunc(s.handlePipelineBoard)))
	s.mux.Handle("POST /api/v1/pipeline-board/tasks", s.requireAuth(http.HandlerFunc(s.handlePipelineBoardTaskCreate)))
	s.mux.Handle("POST /api/v1/pipeline-board/tasks/{id}/ready", s.requireAuth(http.HandlerFunc(s.handlePipelineBoardTaskReady)))
	s.mux.Handle("PATCH /api/v1/pipeline-board/tasks/{id}", s.requireAuth(http.HandlerFunc(s.handlePipelineBoardTaskUpdate)))
	// Ticket lifecycle beyond the ready toggle.
	s.mux.Handle("DELETE /api/v1/pipeline-board/tasks/{id}", s.requireAuth(http.HandlerFunc(s.handlePipelineBoardTaskDelete)))
	s.mux.Handle("POST /api/v1/pipeline-board/tasks/{id}/reset", s.requireAuth(http.HandlerFunc(s.handlePipelineBoardTaskReset)))
	// Close: end a pipeline for good (cancel the tree, file the ticket). The
	// run-scoped variant serves standalone cards that have no ticket.
	s.mux.Handle("POST /api/v1/pipeline-board/tasks/{id}/close", s.requireAuth(http.HandlerFunc(s.handlePipelineBoardTaskClose)))
	s.mux.Handle("POST /api/v1/pipeline-board/runs/{id}/close", s.requireAuth(http.HandlerFunc(s.handlePipelineBoardRunClose)))
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
