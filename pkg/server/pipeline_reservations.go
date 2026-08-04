package server

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
)

// A needs-attention card holds one concurrency slot open so the operator's
// fix restarts into it rather than queueing behind whatever grabbed it.
// This file computes WHICH tickets hold one.
//
// The count is DERIVED — a pure function of (ticket state × run status) —
// never a persisted field or an in-memory ledger. That choice is what makes
// it correct across a restart for free: there is nothing to rebuild, and no
// way for a stored reservation to outlive the run that justified it. The
// enforcement point is elsewhere (pkg/runview/pipeline_queue.go), because
// the FIFO drain and five non-board launch paths would bypass a gate that
// only sat in the admission loop.
//
// The memo exists because the provider is consulted on every admission
// decision, every FIFO drain and every server_info read, and each call lists
// the run store. One second is short enough that no operator notices and
// long enough to collapse a poll storm; the launch teardown invalidates it
// explicitly at the one moment staleness would actually hand out a reserved
// slot (see Service.SetPipelineReservedProvider).
const pipelineReservedTTL = time.Second

type pipelineReservedMemo struct {
	mu       sync.Mutex
	at       time.Time
	set      map[string]struct{}
	computed bool
}

func (m *pipelineReservedMemo) invalidate() {
	m.mu.Lock()
	m.computed = false
	m.mu.Unlock()
}

// pipelineReservedSet reports the ticket ids currently holding a slot.
//
// The predicate mirrors pipelineLaneForRoot exactly — a card that reserves
// without rendering in the needs-attention lane would be an invisible held
// slot, which is the worst way this feature can fail. A ticket reserves iff:
//
//   - it names a bot (otherwise it is a plain backlog item, not a pipeline);
//   - its current run's status is failed or failed_resumable;
//   - its ticket state is NOT terminal (Close files the ticket, which is how
//     the reservation is released);
//   - nothing in its run tree is parked on a human review (the tree is alive
//     and already holds a real slot);
//   - the failure was not caused by iterion itself — a drain or the boot
//     orphan sweep flips every in-flight run at once, and reserving for those
//     would wedge the board on every restart, i.e. on every .go save under
//     `task studio:dev`.
func (s *Server) pipelineReservedSet(boardStore native.BoardStore, runs *runview.Service) map[string]struct{} {
	return s.pipelineReservedSetMemo(s.currentPipelineReservedMemo(), boardStore, runs)
}

// currentPipelineReservedMemo returns the memo bound to the CURRENT wiring.
// wirePipelineReservations allocates a fresh one per wiring so a project
// switch — which builds a new runview.Service while deliberately leaving the
// old one running — cannot have the two share a cache and serve each other's
// reservation set for the length of the TTL.
func (s *Server) currentPipelineReservedMemo() *pipelineReservedMemo {
	s.pipelineReservedMu.RLock()
	memo := s.pipelineReserved
	s.pipelineReservedMu.RUnlock()
	if memo == nil {
		return &pipelineReservedMemo{} // uncached; correct, just not memoized
	}
	return memo
}

func (s *Server) pipelineReservedSetMemo(
	memo *pipelineReservedMemo,
	boardStore native.BoardStore,
	runs *runview.Service,
) map[string]struct{} {
	if boardStore == nil || runs == nil || memo == nil {
		return nil
	}
	// Cloud resolves a board per request from the authenticated team, and its
	// Service is built with a publisher so there is no local queue to gate.
	// Bail explicitly rather than relying on that invariant holding at every
	// call site: a process-wide memo keyed on nothing would otherwise let one
	// team's request read another team's set.
	if strings.EqualFold(s.cfg.Mode, "cloud") {
		return nil
	}
	memo.mu.Lock()
	defer memo.mu.Unlock()
	if memo.computed && time.Since(memo.at) < pipelineReservedTTL {
		return memo.set
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	issues, err := boardStore.List(native.ListFilter{})
	if err != nil {
		// Fail OPEN: an unreadable board must not silently withhold slots.
		if s.logger != nil {
			s.logger.Warn("pipeline reservations: list tickets: %v", err)
		}
		return nil
	}
	runIndex, err := loadRunIndex(ctx, runs)
	if err != nil {
		if s.logger != nil {
			s.logger.Warn("pipeline reservations: list runs: %v", err)
		}
		return nil
	}

	terminal := map[string]struct{}{}
	if board := boardStore.Board(); board != nil {
		for _, st := range board.States {
			if st.Terminal {
				terminal[st.Name] = struct{}{}
			}
		}
	}

	// Reuse the projection's own current-attempt resolution so the gate and
	// the card can never disagree about WHICH run a ticket is showing.
	// currentRunForIssue reads nothing but the run index.
	resolver := &pipelineProjectionBuilder{runs: runIndex}

	set := map[string]struct{}{}
	for _, issue := range issues {
		root := resolver.currentRunForIssue(issue)
		if !pipelineTicketHoldsSlot(issue, root, terminal) {
			continue
		}
		if pipelineTreeAwaitingHuman(issue, runIndex) {
			continue
		}
		set[issue.ID] = struct{}{}
	}

	memo.set = set
	memo.at = time.Now()
	memo.computed = true
	return set
}

// pipelineTreeAwaitingHuman reports that some run in the ticket's tree is
// parked on a human gate, which pins the card to In progress regardless of
// the root's own status (pipelineLaneForRoot's reviews short-circuit).
//
// This is a STATUS proxy for the projection's full pending-review walk,
// which would mean scanning every run's events on a 2s cadence. It errs
// toward "alive": treating a live tree as non-reserving can only cost a
// slot that the board is free to use, whereas the opposite would hold a
// slot invisibly.
func pipelineTreeAwaitingHuman(issue *native.Issue, runIndex map[string]*store.Run) bool {
	for _, run := range issueTreeRuns(issue, runIndex) {
		if run.Status == store.RunStatusPausedWaitingHuman {
			return true
		}
	}
	return false
}

// wirePipelineReservations points the run service's concurrency gate at
// this server's board. Called at boot AND after a project switch — missing
// the second would leave the fresh Service unguarded while the previous
// board's tickets were still being counted.
func (s *Server) wirePipelineReservations(runs *runview.Service) {
	if runs == nil {
		return
	}
	if strings.EqualFold(s.cfg.Mode, "cloud") {
		// Cloud has no local queue (pipelineQueue is only built when the
		// server owns its runs) and resolves the board per request from the
		// authenticated team, so there is no single board to bind here.
		return
	}
	// NOTE: cfg.NativeTrackerStore is boot-scoped — swapWorkDir rebuilds the
	// run service but never re-resolves the board. That predates this feature
	// (the whole /pipelines projection already reads the boot board after a
	// project switch), and what matters here is that the gate and the
	// projection use the SAME (board, runs) pair, which they do: both take
	// this store and the current service. Fixing the underlying scoping is a
	// separate change to the studio's project model.
	boardStore := s.cfg.NativeTrackerStore
	if boardStore == nil {
		return
	}
	// A FRESH memo per wiring, captured by the closure. The previous Service
	// is deliberately not drained on a project switch, so its queue keeps
	// calling its own provider; giving each wiring its own cache is what stops
	// the two from serving each other's set within the TTL.
	memo := &pipelineReservedMemo{}
	s.pipelineReservedMu.Lock()
	s.pipelineReserved = memo
	s.pipelineReservedMu.Unlock()
	runs.SetPipelineReservedProvider(
		func() map[string]struct{} { return s.pipelineReservedSetMemo(memo, boardStore, runs) },
		memo.invalidate,
	)
}

// pipelineReservedForGate clamps a raw reserved count for an admission
// decision and drops the candidate's own reservation, which it is spending
// rather than competing with. PipelineConcurrencyStatus.Reserved carries the
// RAW count (what the operator reads), so every gate has to clamp for itself.
func pipelineReservedForGate(reserved, max int, holdsOwn bool) int {
	if reserved > max {
		reserved = max
	}
	if holdsOwn && reserved > 0 {
		reserved--
	}
	if reserved < 0 {
		return 0
	}
	return reserved
}
