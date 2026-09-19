package server

import (
	"context"
	"strings"

	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/trigger"
)

// scheduleOutcomeName is the eventbus subscriber (and NATS queue group)
// name that back-writes a run's terminal outcome onto its parent schedule
// record (#1426). One replica handles each event.
const scheduleOutcomeName = "schedule-outcome"

// startScheduleOutcome wires the schedule-outcome subscriber: on every
// run-terminal event, look up the run's Source.ScheduleID and stamp the
// (last_run_id, last_run_status, last_run_error, last_run_error_code)
// tuple onto the schedule record.
//
// Placed at the run-terminal chokepoint via the eventbus, per the ticket's
// arbitration: NEVER a poller. The event authority is trigger.BuildRunOutcome
// (fired by runview.emitRunOutcome in-process and runner.fireOutcomeEvent
// on the cloud runner), same chokepoint every other terminal consumer
// listens to (usernotify, alert.OpsDispatcher, gate reconcile).
//
// No-op unless BOTH ScheduledBots and the eventbus are wired: self-hosted
// mode has neither, and the ticker never fires there anyway.
func (s *Server) startScheduleOutcome() {
	if s.cfg.ScheduledBots == nil || s.runs == nil {
		return
	}
	bus := s.eventsBus()
	if bus == nil {
		return
	}
	cancel, err := bus.Subscribe(scheduleOutcomeName, trigger.Matcher{
		Sources: []trigger.Source{trigger.SourceRun},
		Kinds: []string{
			trigger.KindRunFinished,
			trigger.KindRunFailed,
			trigger.KindRunCancelled,
		},
	}, s.handleScheduleOutcomeEvent)
	if err != nil {
		s.logWarn("server: schedule-outcome subscribe failed: %v", err)
		return
	}
	s.scheduleOutcomeCancel = cancel
	if s.logger != nil {
		s.logger.Info("server: schedule-outcome subscriber attached")
	}
}

// handleScheduleOutcomeEvent is the eventbus.Handler for schedule-outcome.
// Keep it small and side-effect narrow — the reconciliation sweep is
// not this handler's business (the bus is lossy by design; a periodic
// re-scan of terminal runs would be the sibling of usernotify.NewSweeper
// if we ever add one, but the ticket doesn't ask for it and the schedule's
// health surface is a rendered read, not a decision, so a missed write
// resolves on the next firing).
func (s *Server) handleScheduleOutcomeEvent(ctx context.Context, ev trigger.Event) error {
	runID := strings.TrimSpace(ev.Subject.ID)
	if runID == "" {
		return nil
	}
	if s.runs == nil {
		return nil
	}
	rs := s.runs.RunStore()
	if rs == nil {
		return nil
	}
	// Load the run with the tenant filter OFF: the subscriber is a
	// platform-level consumer (queue-group "schedule-outcome" on cloud),
	// not scoped to any one tenant.
	run, err := rs.LoadRun(store.WithoutTenantFilter(ctx), runID)
	if err != nil || run == nil {
		// A load error is not the subscriber's problem — the run may have
		// been purged already. Silent: the schedule's LastFireAt still
		// carries the dispatch fact.
		return nil
	}
	// Non-scheduled runs (manual, webhook, board, chain) skip: no schedule
	// to back-write.
	if run.Source == nil || run.Source.ScheduleID == "" {
		return nil
	}
	// The write is targeted (MarkRunOutcome does an $set / $unset) and
	// idempotent under a replay of the same event, so a redelivery
	// (queue-group failover) is safe.
	errMsg, errCode := "", ""
	if run.Status != store.RunStatusFinished {
		errMsg = run.Error
		errCode = string(run.FailureCode)
	}
	// Scope the write to the schedule's tenant (the run's TenantID, which
	// was stamped by launchScheduledBot when it fired). The store's
	// tenant filter honours WithoutTenantFilter above; the Mongo path
	// filters by _id only, so this is defensive but correct.
	tctx := store.WithTenant(store.WithoutTenantFilter(ctx), run.TenantID)
	if err := s.cfg.ScheduledBots.MarkRunOutcome(
		tctx,
		run.Source.ScheduleID,
		runID,
		string(run.Status),
		errMsg,
		errCode,
		run.UpdatedAt,
	); err != nil {
		s.logWarn("server: schedule-outcome mark run %s for schedule %s: %v", runID, run.Source.ScheduleID, err)
	}
	return nil
}
