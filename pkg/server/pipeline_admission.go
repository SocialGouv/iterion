package server

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/SocialGouv/iterion/pkg/dispatcher"
	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
)

// pipelineAdmissionInterval is the backstop cadence of the ready-ticket
// launch loop. It is not latency-critical (a couple of seconds to pick up a
// freshly-readied ticket is fine), so it stays lazy.
const pipelineAdmissionInterval = 2 * time.Second

// pipelineAdmissionEnabled reports whether the studio should run its
// built-in launch loop for ready pipeline tickets: a local native board +
// a run service. The studio always wires an *idle* dispatcher Manager for
// the /dispatcher dashboard, so we do NOT gate on Dispatcher==nil; instead
// admitReadyPipelines backs off while that dispatcher is actively running
// (only then would two launchers race the same board).
func (s *Server) pipelineAdmissionEnabled() bool {
	return s.cfg.Mode != "cloud" &&
		s.cfg.NativeTrackerStore != nil &&
		s.runs != nil
}

// dispatcherActivelyLaunching reports whether an external/SPA-started
// dispatcher is currently polling the board — in which case the admission
// loop must stand down so the two don't double-launch the same ticket.
func (s *Server) dispatcherActivelyLaunching() bool {
	return s.cfg.Dispatcher != nil &&
		s.cfg.Dispatcher.Status().State == dispatcher.ManagerStateRunning
}

// runPipelineAdmissionLoop is the studio's minimal, pipeline-cap-scoped
// dispatcher: it launches tickets the operator marked ready (staged into
// Ready) whenever a concurrency slot is free. Without it, "ready" tickets
// would sit forever unless the operator ran a full `iterion dispatch`.
// Stops when the server shuts down.
func (s *Server) runPipelineAdmissionLoop() {
	s.admitReadyPipelines() // drain any boot-time backlog immediately
	t := time.NewTicker(pipelineAdmissionInterval)
	defer t.Stop()
	for {
		select {
		case <-s.shutdown:
			return
		case <-t.C:
			s.admitReadyPipelines()
		}
	}
}

// admitReadyPipelines launches ready tickets (StateReady, a bot, hard
// blockers all done, no active run) oldest-first while concurrency slots
// remain free. The concurrency gate in runview.Service.Launch is the
// authority — this loop only decides WHICH ready ticket to submit next and
// stops offering more once the cap is reached. Hard-dep admission shares
// native.CanLaunch with the dispatcher adapter so a ticket with open
// blockers can never launch from /pipelines while being skipped by
// iterion dispatch.
func (s *Server) admitReadyPipelines() {
	board := s.cfg.NativeTrackerStore
	if board == nil {
		return
	}
	// Stand down while an operator-started dispatcher owns the board.
	if s.dispatcherActivelyLaunching() {
		return
	}
	s.stateMu.RLock()
	runs := s.runs
	s.stateMu.RUnlock()
	if runs == nil {
		return
	}

	issues, err := board.List(native.ListFilter{})
	if err != nil {
		s.logger.Warn("pipeline admission: list tickets: %v", err)
		return
	}
	ready := make([]*native.Issue, 0)
	for _, iss := range issues {
		if iss == nil {
			continue
		}
		// Board-side gate: bot + StateReady + blockers all StateDone.
		if !native.CanLaunch(board, iss) {
			continue
		}
		if !pipelineTicketLaunchable(context.Background(), runs.RunStore(), iss) {
			continue
		}
		ready = append(ready, iss)
	}
	sortReadyTickets(ready)

	for _, iss := range ready {
		st := runs.PipelineConcurrency()
		if st.Enabled && st.Active >= st.Max {
			return // no free slot; remaining ready tickets wait for the next tick
		}
		s.launchReadyTicket(runs, board, iss)
	}
}

// sortReadyTickets orders the launch candidates the admission loop submits:
// highest Priority first (the operator's ranking dial, same field /board
// sorts on), oldest CreatedAt as the tie-break so equal-priority tickets
// launch first-come-first-served. This IS the "which ticket goes next from
// Ready" policy — keep it aligned with the projection's card ordering.
func sortReadyTickets(ready []*native.Issue) {
	sort.SliceStable(ready, func(i, j int) bool {
		if ready[i].Priority != ready[j].Priority {
			return ready[i].Priority > ready[j].Priority
		}
		return ready[i].CreatedAt.Before(ready[j].CreatedAt)
	})
}

// pipelineTicketLaunchable reports whether a ready ticket has no run that is
// active or already finished — i.e. it is fresh, or its last run failed and
// the operator retried it to Ready.
func pipelineTicketLaunchable(ctx context.Context, rs store.RunStore, iss *native.Issue) bool {
	if rs == nil {
		return true
	}
	if iss.LastRunID == "" {
		return true
	}
	r, err := rs.LoadRun(ctx, iss.LastRunID)
	if err != nil || r == nil {
		return true // last run vanished; a fresh launch is safe
	}
	switch r.Status {
	case store.RunStatusRunning,
		store.RunStatusPausedWaitingHuman,
		store.RunStatusPausedOperator,
		store.RunStatusQueued,
		store.RunStatusFinished:
		return false
	default:
		// failed / failed_resumable / cancelled → retry-able.
		return true
	}
}

// warnAdmissionSkipOnce logs a ready ticket whose bot the current catalog
// can't launch (unknown or disabled), deduped per (ticket, bot) so the 2s
// admission tick doesn't repeat it. A later tick with a DIFFERENT bot (the
// ticket was re-stamped) or a resolvable one re-arms the warning.
func (s *Server) warnAdmissionSkipOnce(ticketID, bot string, found bool) {
	s.admissionSkipMu.Lock()
	defer s.admissionSkipMu.Unlock()
	if s.admissionSkipWarned == nil {
		s.admissionSkipWarned = make(map[string]string)
	}
	if s.admissionSkipWarned[ticketID] == bot {
		return
	}
	s.admissionSkipWarned[ticketID] = bot
	reason := "is disabled"
	if !found {
		reason = "is not in the current workspace's catalog"
	}
	s.logger.Warn("pipeline admission: ticket %s stays in Ready — bot %q %s", ticketID, bot, reason)
}

// launchReadyTicket moves a ready ticket out of StateReady (so a slow launch
// can't be double-picked by the next tick), launches its bot through the
// concurrency gate, and stamps the resulting run onto the ticket so the
// projection folds them into one card. A launch failure reverts the ticket
// to Backlog.
func (s *Server) launchReadyTicket(runs *runview.Service, board native.BoardStore, iss *native.Issue) {
	if _, err := s.launchTicketNow(runs, board, iss); err != nil {
		// launchTicketNow already logged the specifics; the loop is
		// best-effort and simply retries on the next tick.
		return
	}
}

// launchTicketNow claims a ticket and launches its bot, returning the run
// id. It is the shared body of the admission loop (which ignores the error
// and retries next tick) and of the operator's explicit "launch now" drag,
// which needs the failure reported back over HTTP. The bot-not-in-catalog
// case is a *skip*, not a failure, for the loop — hence the dedicated
// error the caller can distinguish.
func (s *Server) launchTicketNow(runs *runview.Service, board native.BoardStore, iss *native.Issue) (string, error) {
	entry, found, err := s.findBot(iss.Bot)
	if err != nil {
		s.logger.Warn("pipeline admission: resolve bot %q: %v", iss.Bot, err)
		return "", fmt.Errorf("resolve bot %q: %w", iss.Bot, err)
	}
	if !found || !entry.Enabled {
		// Unknown/disabled bot: leave the ticket in Ready, surfaced as-is.
		// Say so (once per ticket+bot, not every tick) — after a studio
		// project switch the boot-scoped board can reference bots the
		// current workspace's catalog no longer resolves, and a silent
		// skip reads as a stuck pipeline.
		s.warnAdmissionSkipOnce(iss.ID, iss.Bot, found)
		return "", fmt.Errorf("bot %q is not in this workspace's catalog (or is disabled)", iss.Bot)
	}
	// Leave StateReady BEFORE launching so the next tick won't re-pick this
	// ticket while Launch is in flight. StateInProgress is not StateReady, so
	// admitReadyPipelines skips it; the run's status then drives the column.
	if _, err := board.SetState(iss.ID, native.StateInProgress); err != nil {
		s.logger.Warn("pipeline admission: claim ticket %s: %v", iss.ID, err)
		return "", fmt.Errorf("claim ticket: %w", err)
	}
	res, err := runs.Launch(context.Background(), runview.LaunchSpec{
		FilePath: entry.MainFile(),
		BotID:    entry.Name,
		Vars:     iss.BotArgs,
	})
	if err != nil {
		s.logger.Warn("pipeline admission: launch ticket %s: %v", iss.ID, err)
		// Return it to Backlog so it isn't stuck claimed with no run.
		if _, rErr := board.SetState(iss.ID, native.StateInbox); rErr != nil {
			s.logger.Warn("pipeline admission: revert ticket %s: %v", iss.ID, rErr)
		}
		return "", fmt.Errorf("launch: %w", err)
	}
	if err := board.SetLastRun(iss.ID, res.RunID, ""); err != nil {
		s.logger.Warn("pipeline admission: link run %s to ticket %s: %v", res.RunID, iss.ID, err)
	}
	s.logger.Info("pipeline admission: started ticket %s as run %s (bot %s)", iss.ID, res.RunID, entry.Name)
	return res.RunID, nil
}
