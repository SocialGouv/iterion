package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/SocialGouv/iterion/pkg/auth"
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

// firstLiveTreeRun returns the first run in a ticket's tree that has not
// reached a terminal status, or nil once the whole tree has settled. It is
// the single "is anything still going under this card?" answer shared by
// Delete (which refuses to detach a live pipeline from its ticket) and
// Reset's `fresh` option (which refuses to drop the pointer that holds one).
// One helper rather than a repeated loop: the two guards must agree on what
// "still going" means, and `failed_resumable` — terminal, and precisely the
// status a fresh restart exists to abandon — is where a divergence would
// hurt.
func firstLiveTreeRun(issue *native.Issue, runs map[string]*store.Run) *store.Run {
	for _, run := range issueTreeRuns(issue, runs) {
		if !run.Status.IsTerminal() {
			return run
		}
	}
	return nil
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
	if live := firstLiveTreeRun(issue, runIndex); live != nil {
		s.httpErrorFor(w, r, http.StatusConflict,
			"pipeline board delete: run %s is still %s — cancel or reset the ticket first", live.ID, live.Status)
		return
	}
	if err := boardStore.Delete(id); err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "pipeline board delete: %v", err)
		return
	}
	s.reflectAllowedOrigin(w, r)
	w.WriteHeader(http.StatusNoContent)
}

// pipelineBoardResetRequest is the reset action's OPTIONAL body.
type pipelineBoardResetRequest struct {
	// Fresh additionally drops the ticket's last-run pointer, so the next
	// launch cannot resume the run this reset discards. Without it the
	// pointer survives and "what happens next" depends on who picks the
	// ticket up — the studio admission loop mints a fresh run,
	// `iterion dispatch` resolves the pointer and resumes from its
	// checkpoint (resolveRunID → LastRunForIssue → resumableRunID).
	Fresh bool `json:"fresh"`
}

// readPipelineResetRequest decodes the reset body, which is OPTIONAL: this
// endpoint shipped without one and clients still POST it empty. An absent or
// blank body is the historical (pointer-keeping) reset; a body that is
// present but malformed is an ERROR, never silently read as `fresh: false` —
// the flag decides whether a run pointer is discarded, so a typo'd request
// must not be answered with the safe-looking default.
func readPipelineResetRequest(r *http.Request) (pipelineBoardResetRequest, error) {
	var req pipelineBoardResetRequest
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return req, err
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return req, nil
	}
	return req, json.Unmarshal(raw, &req)
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
//
// `fresh: true` additionally clears the last-run pointer, which is what
// makes the restart deterministic: with the pointer gone both launch
// authorities mint a new run, instead of the studio minting one and a live
// dispatcher resuming the dead one. It is REFUSED while anything in the
// ticket's tree is still non-terminal, and refused BEFORE the cancel sweep
// so nothing is touched — see the guard below.
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
	req, err := readPipelineResetRequest(r)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusBadRequest, "pipeline board reset: invalid request: %v", err)
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
	// THE PIN'S WINDOW. The sweep below only SIGNALS a cancel, so a run can
	// still be unwinding when it returns — which is why reset pins that run
	// as the ticket's current attempt (see below) instead of letting the
	// relaunch start beside it. `fresh` deletes exactly that pointer, so
	// running it in that window would reopen the hole the pin closes.
	//
	// The guard therefore refuses rather than waits, and refuses HERE,
	// before the sweep: waiting would hold an HTTP request for however long
	// an agent takes to reach its next safe boundary and still have to
	// refuse on timeout, while refusing first leaves the ticket, the runs
	// and the pointer exactly as they were — a 409 the operator can act on
	// (stop or plain-reset it, then start over) rather than a half-done
	// reset. The consequence is that `fresh` only ever runs on a settled
	// tree, where the sweep and the pin are both no-ops.
	//
	// Same predicate as Delete's guard (firstLiveTreeRun) and the same
	// dead-vs-live discrimination as `iterion issue update
	// --clear-last-run` (refuseClearWhileRunAlive): `failed_resumable` and
	// `cancelled` are dead and clearable — they are the whole point —
	// while running/queued/paused hold the pointer. A server with no run
	// service reads an empty index and so admits the clear: it launches
	// nothing itself (the launch endpoint refuses outright), and both
	// siblings resolve that same unknowable the same way rather than
	// bricking the escape hatch.
	if req.Fresh {
		if live := firstLiveTreeRun(issue, runIndex); live != nil {
			s.httpErrorFor(w, r, http.StatusConflict,
				"pipeline board reset: ticket %s cannot start fresh while run %s is still %s — discarding its last-run pointer now would let a second run start beside it; stop or reset it first, then start over once it is terminal",
				id, live.ID, live.Status)
			return
		}
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
	// Drop the pointer BEFORE the restage, never after: restaging is what
	// makes the ticket eligible again, so a window where it is Ready and
	// still names a resumable run is a window in which a dispatcher resumes
	// the very run this call was told to discard.
	//
	// Its failure is RAISED, not logged: the operator asked for a fresh
	// start, and restaging with the pointer intact would hand them the
	// race they clicked to avoid. Nothing has been restaged yet, so the
	// 409 leaves a coherent card (its runs cancelled, its state unchanged).
	if req.Fresh {
		if err := boardStore.SetLastRun(id, "", ""); err != nil {
			s.httpErrorFor(w, r, http.StatusInternalServerError,
				"pipeline board reset: ticket %s: clear the last-run pointer: %v — the ticket was NOT restaged, so nothing can resume the discarded run", id, err)
			return
		}
		if s.logger != nil {
			s.logger.Info("pipeline board: reset ticket %s dropped its last-run pointer (was %s) — the next launch starts fresh", id, issue.LastRunID)
		}
	}
	// Reset is an OPERATOR gesture: a terminal ticket restaged is a
	// sanctioned reopen, not a machine resurrection.
	updated, err := native.SetStateOrReopen(boardStore, id, native.StateReady)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "pipeline board reset: restage: %v", err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	s.reflectAllowedOrigin(w, r)
	_ = json.NewEncoder(w).Encode(updated)
}

// pipelineRunRetired reports that a run needs no cancel sweep. It is
// deliberately NARROWER than store.RunStatus.IsTerminal(): failed_resumable
// IS terminal, but it is exactly the status CancelInactiveCtx exists to flip
// (the run is parked with a checkpoint and could still be resumed), so
// skipping it would leave a closed ticket shadowing a resumable run and —
// worse for us — leave that run reserving a concurrency slot forever.
func pipelineRunRetired(status store.RunStatus) bool {
	switch status {
	case store.RunStatusFinished, store.RunStatusFailed, store.RunStatusCancelled:
		return true
	default:
		return false
	}
}

// pipelineCloseTargetState picks the terminal state Close files a ticket
// into: ABANDONED (native.StateBlocked) whenever the board declares it,
// otherwise the board's first terminal state (the same rule as
// `iterion issue close`).
//
// It is never `done`, and that is a correctness decision rather than a
// wording one. native.BlockerSatisfied counts ONLY `done`, and SetState(done)
// fires PromoteUnblockedDependents — so closing a broken pipeline as done
// would release every ticket parked behind it into work whose input was
// never produced. A closed pipeline did not deliver; the tickets waiting on
// it must keep waiting, and the studio names them in the confirm dialog so
// the operator sees the consequence before it happens.
func pipelineCloseTargetState(board *native.Board) (string, bool) {
	if board == nil {
		return "", false
	}
	if st := board.StateByName(native.StateBlocked); st != nil && st.Terminal {
		return st.Name, true
	}
	for _, st := range board.States {
		// NEVER `done`, including in the fallback. native.DefaultBoard lists
		// done (index 7) BEFORE blocked (index 8), so a plain "first terminal
		// state" scan resolves to done the moment a board lacks `blocked` —
		// and UpgradeBoardSchema backfills inbox/waiting_deps/awaiting_input
		// but never blocked, while preserving operator-customised boards. That
		// would auto-promote every ticket parked behind a pipeline that never
		// delivered, which is precisely what this function claims to prevent
		// and the opposite of what the confirm dialog just promised.
		// Falling through to the 409 is the safe outcome.
		if st.Terminal && st.Name != native.StateDone {
			return st.Name, true
		}
	}
	return "", false
}

// handlePipelineBoardTaskClose ends a ticket-backed pipeline for good: every
// still-active run in its tree is cancelled through the same cascade as
// Reset, then the ticket is filed in a terminal state. Unlike Reset it never
// restages, so the admission loop will not relaunch it.
//
// This is the release valve for a needs-attention card's reserved
// concurrency slot — the ONLY exit besides a retry. Without it a failed
// pipeline the operator does not intend to fix would hold its slot forever
// and the board's throughput would silently drop.
func (s *Server) handlePipelineBoardTaskClose(w http.ResponseWriter, r *http.Request) {
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
		s.httpErrorFor(w, r, http.StatusBadRequest, "pipeline board close: missing task id")
		return
	}
	issue, err := boardStore.Get(id)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusNotFound, "pipeline board close: %v", err)
		return
	}
	board, err := boardStore.Board()
	if err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "pipeline board close: read board: %v", err)
		return
	}
	target, ok := pipelineCloseTargetState(board)
	if !ok {
		s.httpErrorFor(w, r, http.StatusConflict,
			"pipeline board close: board has no terminal state — declare one or move the ticket by hand")
		return
	}
	s.stateMu.RLock()
	runs := s.runs
	s.stateMu.RUnlock()

	cancelled, ok := s.cancelIssueTree(w, r, runs, issue, "close")
	if !ok {
		return
	}
	updated, err := boardStore.SetState(id, target)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "pipeline board close: file ticket: %v", err)
		return
	}
	// Close IS the acknowledgement a dispatcher give-up was missing, so drop
	// the stamp that put the card in Needs attention. Every other exit
	// invalidates it for free — a retry moves the ticket, a relaunch changes
	// the run — but Close files a ticket into the state the give-up already
	// wrote, so SetState above is a no-op there and only an explicit clear
	// ends it. Best-effort: the ticket is filed either way, and a surviving
	// stamp costs a card in the wrong lane, not correctness.
	if err := boardStore.SetGaveUp(id, nil); err != nil {
		if s.logger != nil {
			s.logger.Warn("pipeline board: close ticket %s: clear give-up stamp: %v", id, err)
		}
	} else if refreshed, getErr := boardStore.Get(id); getErr == nil {
		// `updated` is SetState's pre-clear snapshot; encoding it would hand
		// an API consumer a ticket that still carries the stamp this call
		// just dropped — a response contradicting its own write.
		updated = refreshed
	}
	// Re-sweep once against a FRESH view. launchTicketNow flips the ticket to
	// in_progress seconds before the run it started becomes discoverable, so
	// the admission loop can have started a run for this very ticket during
	// the cancel sweep above. Closing without this leaves that run alive and
	// orphaned under a card that now reads Closed.
	//
	// Its failures are REPORTED, not raised: the ticket is already filed and
	// the card already reads Closed, so a 409 here would tell the operator the
	// operation failed when it half-succeeded — and would discard the first
	// sweep's result. The unreachable runs travel in the 200 body instead, so
	// the reported outcome matches what was persisted.
	unreachable := make([]string, 0)
	if refreshed, getErr := boardStore.Get(id); getErr == nil {
		extra, stuck := s.cancelIssueTreeBestEffort(r, runs, refreshed)
		cancelled = append(cancelled, extra...)
		unreachable = append(unreachable, stuck...)
	}
	if s.logger != nil {
		s.logger.Info("pipeline board: closed ticket %s → %s (cancelled %d run(s), %d unreachable)",
			id, target, len(cancelled), len(unreachable))
	}
	w.Header().Set("Content-Type", "application/json")
	s.reflectAllowedOrigin(w, r)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"task":             updated,
		"state":            target,
		"cancelled_runs":   cancelled,
		"unreachable_runs": unreachable,
	})
}

// cancelIssueTreeBestEffort is the post-commit counterpart of
// cancelIssueTree: it never writes an HTTP error, returning the runs it
// stopped and the ones it could not reach so the caller can report both
// alongside a state change that has already landed.
func (s *Server) cancelIssueTreeBestEffort(
	r *http.Request,
	runs *runview.Service,
	issue *native.Issue,
) (cancelled, unreachable []string) {
	runIndex, err := loadRunIndex(r.Context(), runs)
	if err != nil {
		if s.logger != nil {
			s.logger.Warn("pipeline board close: re-sweep list runs: %v", err)
		}
		return nil, nil
	}
	for _, run := range issueTreeRuns(issue, runIndex) {
		if pipelineRunRetired(run.Status) {
			continue
		}
		if s.cancelPipelineRun(r.Context(), runs, run.ID) {
			cancelled = append(cancelled, run.ID)
			continue
		}
		unreachable = append(unreachable, run.ID)
		if s.logger != nil {
			s.logger.Warn("pipeline board close: run %s survived the close of ticket %s — cancel it from its run console", run.ID, issue.ID)
		}
	}
	return cancelled, unreachable
}

// cancelIssueTree cancels every non-retired run in a ticket's tree, writing
// a 409 and returning ok=false on the first run this process cannot reach.
// Shared by Close (and shaped like Reset's sweep, which keeps its own copy
// because it additionally pins the dying run as the current attempt).
func (s *Server) cancelIssueTree(
	w http.ResponseWriter,
	r *http.Request,
	runs *runview.Service,
	issue *native.Issue,
	op string,
) ([]string, bool) {
	runIndex, err := loadRunIndex(r.Context(), runs)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "pipeline board %s: list runs: %v", op, err)
		return nil, false
	}
	cancelled := make([]string, 0, 1)
	for _, run := range issueTreeRuns(issue, runIndex) {
		if pipelineRunRetired(run.Status) {
			continue
		}
		if !s.cancelPipelineRun(r.Context(), runs, run.ID) {
			s.httpErrorFor(w, r, http.StatusConflict,
				"pipeline board %s: run %s could not be cancelled from this process — cancel it from its run console first", op, run.ID)
			return nil, false
		}
		cancelled = append(cancelled, run.ID)
	}
	return cancelled, true
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
	// Concurrency, counting slots RESERVED by pipelines that need a human —
	// minus this ticket's own reservation, if it holds one. Without the
	// self-exclusion a needs-attention card is refused by the slot it is
	// holding for its own restart, and since this endpoint has no ready-state
	// precondition it is a legal target for exactly that card: the deadlock
	// is reachable straight from the API.
	if st := runs.PipelineConcurrency(); st.Enabled {
		_, holdsOwn := s.pipelineReservedSet(boardStore, runs)[id]
		reserved := pipelineReservedForGate(st.Reserved, st.Max, holdsOwn)
		if st.Active+reserved >= st.Max {
			s.httpErrorFor(w, r, http.StatusConflict,
				"pipeline board launch: all %d pipeline slots are taken (%d running, %d held by pipelines needing attention) — retry, close or stop one first (launching anyway would only park this one as queued)",
				st.Max, st.Active, reserved)
			return
		}
	}
	// The bot resolves for the team the board itself was selected from
	// (cloudBoardResolve), so a team's fork — or a bot only it authored —
	// serves its own cards.
	caller, _ := auth.FromContext(r.Context())
	runID, err := s.launchTicketNow(r.Context(), caller.TeamID, runs, boardStore, issue)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusConflict, "pipeline board launch: %v", err)
		return
	}
	if s.logger != nil {
		s.logger.Info("pipeline board: ticket %s launched on operator demand as run %s (queue bypassed)", id, runID)
	}
	writeJSONStatus(w, http.StatusAccepted, map[string]string{"task_id": id, "run_id": runID})
}
