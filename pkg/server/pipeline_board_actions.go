package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
)

// Ticket-level actions on the pipeline board that go beyond the ready-state
// toggle: deleting a Backlog ticket and resetting an in-progress one. Both
// are guarded so they can never break an in-flight run they don't own —
// delete refuses while a run is active, and reset only restages the ticket
// after every run in its tree has been cancelled through the same seams the
// run console uses.

// issueRunRoots returns the ROOT runs owned by a ticket: the dispatcher's
// LastRunID pointer, the attempt history refs, and any run whose Source
// names the issue (the in-flight edge the projection also honours).
// Descendants are reached from these via issueTreeRuns.
func issueRunRoots(issue *native.Issue, runs map[string]*store.Run) []*store.Run {
	if issue == nil {
		return nil
	}
	seen := map[string]struct{}{}
	roots := make([]*store.Run, 0, len(issue.Runs)+1)
	appendRoot := func(run *store.Run) {
		if run == nil {
			return
		}
		if _, ok := seen[run.ID]; ok {
			return
		}
		seen[run.ID] = struct{}{}
		roots = append(roots, run)
	}
	appendRoot(runs[issue.LastRunID])
	for _, ref := range issue.Runs {
		appendRoot(runs[ref.RunID])
	}
	for _, run := range runs {
		if run.Source != nil && run.Source.IssueID == issue.ID {
			appendRoot(run)
		}
	}
	return roots
}

// issueTreeRuns expands the ticket's root runs into the full run tree
// (roots first, then descendants breadth-first), reusing the projection's
// parent edge (ParentRunID with the ForkedFrom compatibility fallback) and
// its depth guard so a pathological parent/child cycle cannot hang the
// handler.
func issueTreeRuns(issue *native.Issue, runs map[string]*store.Run) []*store.Run {
	children := map[string][]*store.Run{}
	for _, run := range runs {
		if parentID := pipelineParentRunID(run); parentID != "" && parentID != run.ID {
			children[parentID] = append(children[parentID], run)
		}
	}
	visited := map[string]struct{}{}
	tree := make([]*store.Run, 0)
	var walk func(run *store.Run, depth int)
	walk = func(run *store.Run, depth int) {
		if run == nil || depth > pipelineTreeMaxDepth {
			return
		}
		if _, seen := visited[run.ID]; seen {
			return
		}
		visited[run.ID] = struct{}{}
		tree = append(tree, run)
		for _, child := range children[run.ID] {
			walk(child, depth+1)
		}
	}
	for _, root := range issueRunRoots(issue, runs) {
		walk(root, 0)
	}
	return tree
}

// loadRunIndex snapshots the run store into the id→run map the tree
// helpers consume. A nil service yields an empty index (delete/reset then
// operate on the ticket alone).
func loadRunIndex(ctx context.Context, runs *runview.Service) (map[string]*store.Run, error) {
	index := map[string]*store.Run{}
	if runs == nil {
		return index, nil
	}
	records, err := runs.ListRunRecordsCtx(ctx, runview.ListFilter{})
	if err != nil {
		return nil, err
	}
	for _, run := range records {
		index[run.ID] = run
	}
	return index, nil
}

// handlePipelineBoardTaskDelete removes a ticket the operator no longer
// wants — the backend of the Backlog card's Delete button. It only deletes
// the native ISSUE, never a run: past attempts stay in the run store (they
// resurface as standalone Done/Failed cards, same as /board deletes). While
// any run in the ticket's tree is still active (running / paused / queued)
// the delete is refused with 409 — cancel or reset first — so a live
// pipeline can never be silently detached from its ticket.
func (s *Server) handlePipelineBoardTaskDelete(w http.ResponseWriter, r *http.Request) {
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
		s.httpErrorFor(w, r, http.StatusBadRequest, "pipeline board delete: missing task id")
		return
	}
	issue, err := boardStore.Get(id)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusNotFound, "pipeline board delete: %v", err)
		return
	}
	s.stateMu.RLock()
	runs := s.runs
	s.stateMu.RUnlock()
	runIndex, err := loadRunIndex(r.Context(), runs)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "pipeline board delete: list runs: %v", err)
		return
	}
	for _, run := range issueTreeRuns(issue, runIndex) {
		if !run.Status.IsTerminal() {
			s.httpErrorFor(w, r, http.StatusConflict,
				"pipeline board delete: run %s is still %s — cancel or reset the ticket first", run.ID, run.Status)
			return
		}
	}
	if err := boardStore.Delete(id); err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "pipeline board delete: %v", err)
		return
	}
	s.reflectAllowedOrigin(w, r)
	w.WriteHeader(http.StatusNoContent)
}

// handlePipelineBoardTaskReset restarts an in-progress ticket from zero —
// the backend of the In-progress card's Reset button. It cancels every
// still-active run in the ticket's tree through the SAME cascade the run
// console's Cancel uses (in-process cancel → dispatcher cancel → inactive
// status flip), and only then restages the ticket to Ready so the admission
// loop relaunches it fresh once the cancelled runs settle
// (pipelineTicketLaunchable holds the launch until the old run is
// terminal). If any run cannot be cancelled from this process the reset is
// refused with 409 and the ticket is left untouched.
func (s *Server) handlePipelineBoardTaskReset(w http.ResponseWriter, r *http.Request) {
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
		s.httpErrorFor(w, r, http.StatusBadRequest, "pipeline board reset: missing task id")
		return
	}
	issue, err := boardStore.Get(id)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusNotFound, "pipeline board reset: %v", err)
		return
	}
	if strings.TrimSpace(issue.Bot) == "" {
		s.httpErrorFor(w, r, http.StatusConflict, "pipeline board reset: ticket %s has no bot — not a pipeline ticket", id)
		return
	}
	s.stateMu.RLock()
	runs := s.runs
	s.stateMu.RUnlock()
	runIndex, err := loadRunIndex(r.Context(), runs)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "pipeline board reset: list runs: %v", err)
		return
	}
	// Roots before descendants (tree walk order): cancelling the root first
	// lets the engine's own context propagation stop in-process children,
	// and the sweep then flips any independently-parked child.
	cancelled := make([]string, 0, 1)
	for _, run := range issueTreeRuns(issue, runIndex) {
		if run.Status.IsTerminal() {
			continue
		}
		if !s.cancelPipelineRun(r.Context(), runs, run.ID) {
			s.httpErrorFor(w, r, http.StatusConflict,
				"pipeline board reset: run %s could not be cancelled from this process — cancel it from its run console first", run.ID)
			return
		}
		cancelled = append(cancelled, run.ID)
		if s.logger != nil {
			s.logger.Info("pipeline board: reset ticket %s cancelled run %s (was %s)", id, run.ID, run.Status)
		}
	}
	// A cancel may only have been SIGNALLED (the engine unwinds at its next
	// safe boundary), and the admission loop's relaunch gate
	// (pipelineTicketLaunchable) only inspects LastRunID. If the still-dying
	// run is linked to the ticket some other way (e.g. a dispatcher attempt
	// that never stamped LastRunID), pin it as the current attempt BEFORE
	// restaging so the fresh launch waits for it to actually stop.
	if runs != nil {
		for _, runID := range cancelled {
			run, loadErr := runs.LoadRunCtx(r.Context(), runID)
			if loadErr != nil || run.Status.IsTerminal() {
				continue
			}
			if issue.LastRunID != run.ID {
				if slErr := boardStore.SetLastRun(id, run.ID, ""); slErr != nil && s.logger != nil {
					s.logger.Warn("pipeline board: reset ticket %s: pin dying run %s: %v", id, run.ID, slErr)
				}
			}
			break
		}
	}
	updated, err := boardStore.SetState(id, native.StateReady)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "pipeline board reset: restage: %v", err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	s.reflectAllowedOrigin(w, r)
	_ = json.NewEncoder(w).Encode(updated)
}

// cancelPipelineRun signals one run to stop, mirroring handleCancelRun's
// cascade: in-process cancel first, then the dispatcher's cancel funcs,
// then the inactive status flip for parked runs, accepting an
// already-terminal run as success. Returns false only when the run stays
// active and unreachable from this process (e.g. held by another one).
func (s *Server) cancelPipelineRun(ctx context.Context, runs *runview.Service, runID string) bool {
	if runs == nil {
		return false
	}
	if err := runs.Cancel(runID); err == nil {
		return true
	} else if !errors.Is(err, runview.ErrRunNotActive) {
		if s.logger != nil {
			s.logger.Warn("pipeline board: cancel run %s: %v", runID, err)
		}
		return false
	}
	if s.cfg.Dispatcher != nil && s.cfg.Dispatcher.CancelRun(runID) {
		return true
	}
	if cancelled, err := runs.CancelInactiveCtx(ctx, runID); err == nil && cancelled {
		return true
	} else if err != nil && s.logger != nil {
		s.logger.Warn("pipeline board: cancel inactive run %s: %v", runID, err)
	}
	run, err := runs.LoadRunCtx(ctx, runID)
	if err != nil {
		// The run vanished from the store — nothing left to protect.
		return true
	}
	return run.Status.IsTerminal()
}

// handlePipelineBoardTaskLaunch starts a ticket RIGHT NOW, bypassing the
// admission loop's priority ordering (POST .../tasks/{id}/launch). It backs
// the Opened → In progress drag: the operator overrides the queue, jumping
// this ticket ahead of higher-priority ones.
//
// What it bypasses: the ready-state staging and the priority/oldest-first
// ordering — i.e. exactly the ranking. What it does NOT bypass, because
// those are correctness rather than ranking:
//   - open hard dependencies (a ticket whose blockers aren't done would run
//     against work that doesn't exist yet),
//   - a run already active or finished on this ticket (that is what Reset
//     and Retry are for),
//   - the pipeline concurrency cap — over it, runview.Service.Launch parks
//     the run as queued instead of running, which would silently contradict
//     the drop the operator just made. We refuse with 409 and say why.
func (s *Server) handlePipelineBoardTaskLaunch(w http.ResponseWriter, r *http.Request) {
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
		s.httpErrorFor(w, r, http.StatusBadRequest, "pipeline board launch: missing task id")
		return
	}
	issue, err := boardStore.Get(id)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusNotFound, "pipeline board launch: %v", err)
		return
	}
	if strings.TrimSpace(issue.Bot) == "" {
		s.httpErrorFor(w, r, http.StatusConflict, "pipeline board launch: ticket %s has no bot — not a pipeline ticket", id)
		return
	}
	s.stateMu.RLock()
	runs := s.runs
	s.stateMu.RUnlock()
	if runs == nil {
		s.httpErrorFor(w, r, http.StatusConflict, "pipeline board launch: no run service on this server")
		return
	}
	if !pipelineTicketLaunchable(r.Context(), runs.RunStore(), issue) {
		s.httpErrorFor(w, r, http.StatusConflict,
			"pipeline board launch: ticket %s already has an active or finished run — use Reset or Retry", id)
		return
	}
	if ok, open := native.BlockersSatisfiedForIssue(boardStore, issue); !ok {
		s.httpErrorFor(w, r, http.StatusConflict,
			"pipeline board launch: ticket %s still waits on %d unfinished dependency — dependencies are correctness, not ranking", id, len(open))
		return
	}
	if st := runs.PipelineConcurrency(); st.Enabled && st.Active >= st.Max {
		s.httpErrorFor(w, r, http.StatusConflict,
			"pipeline board launch: all %d pipeline slots are busy — stop or pause a run first (launching anyway would only park this one as queued)", st.Max)
		return
	}
	runID, err := s.launchTicketNow(runs, boardStore, issue)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusConflict, "pipeline board launch: %v", err)
		return
	}
	if s.logger != nil {
		s.logger.Info("pipeline board: ticket %s launched on operator demand as run %s (queue bypassed)", id, runID)
	}
	writeJSONStatus(w, http.StatusAccepted, map[string]string{"task_id": id, "run_id": runID})
}
