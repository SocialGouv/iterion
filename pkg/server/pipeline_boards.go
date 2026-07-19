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
	pipelineColumnBacklog    = "backlog"
	pipelineColumnTodo       = "todo"
	pipelineColumnInProgress = "in_progress"
	pipelineColumnDone       = "done"
	pipelineColumnFailed     = "failed"

	pipelineTreeMaxDepth = 20
	pipelineTreeMaxCards = 500

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
