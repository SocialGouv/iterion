package server

import (
	"context"
	"time"

	"github.com/SocialGouv/iterion/pkg/alert"
	"github.com/SocialGouv/iterion/pkg/cloudsched"
	"github.com/SocialGouv/iterion/pkg/deeplink"
)

const (
	// scheduleOutcomeWatchInterval paces the schedule-outcome watch. A
	// stalled schedule is a matter of hours (a daily digest that posts
	// nothing), so ten minutes is prompt without loading the store.
	scheduleOutcomeWatchInterval = 10 * time.Minute
	// scheduleStallGrace is how far in the past a schedule's next_fire_at
	// may lie before the slot counts as unclaimed. It covers a scheduler
	// restart and a slow tick; scheduleStallGraceFor widens it for an
	// operator-lengthened tick interval.
	scheduleStallGrace = 2 * time.Hour
)

// scheduleStallGraceFor is the overdue grace for a scheduler ticking every
// tick: never shorter than three ticks, so ITERION_SCHEDULER_INTERVAL set
// above the default grace does not read every schedule as overdue.
func scheduleStallGraceFor(tick time.Duration) time.Duration {
	return max(scheduleStallGrace, 3*tick)
}

// runScheduleOutcomeWatch alarms the platform operator when a cloud schedule
// stops producing successful runs. The schedule row carries its own health
// (next_fire_at, last_error, last_run_status) but nobody reads it unless
// they look; the run-outcome alerts only see runs, so a tick refused before
// any run existed, or a scheduler that stopped claiming slots, reaches no
// one. Every replica sweeps; the ops dispatcher's first-writer-wins claim
// on the episode key delivers each episode once across replicas.
func (s *Server) runScheduleOutcomeWatch(ctx context.Context) {
	grace := scheduleStallGraceFor(schedulerTickInterval())
	s.logger.Info("server: schedule-outcome watch active (every %s, overdue grace %s)", scheduleOutcomeWatchInterval, grace)
	t := time.NewTicker(scheduleOutcomeWatchInterval)
	defer t.Stop()
	for {
		s.scheduleOutcomeSweep(ctx, time.Now().UTC(), grace)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// scheduleOutcomeSweep assesses every enabled schedule once and offers each
// stalled one to the ops dispatcher under a per-episode claim key. A failed
// delivery is released by NotifyOperator, so the next sweep offers it again.
func (s *Server) scheduleOutcomeSweep(ctx context.Context, now time.Time, grace time.Duration) {
	rows, err := s.cfg.ScheduledBots.ListEnabled(ctx)
	if err != nil {
		s.warnf("server: schedule-outcome watch: list schedules: %v", err)
		return
	}
	for _, sb := range rows {
		if ctx.Err() != nil {
			return
		}
		stall := cloudsched.AssessScheduleStall(sb, now, grace)
		if stall == nil {
			continue
		}
		a := alert.Alert{
			Kind:    alert.KindScheduleStalled,
			RunName: sb.BotID,
			Reason:  stall.Reason,
		}
		if stall.Cause == cloudsched.StallRunFailed {
			a.RunID = sb.LastRunID
			a.FailureCode = sb.LastRunErrorCode
			if s.cfg.PublicURL != "" {
				a.Link = deeplink.Run(s.cfg.PublicURL, sb.LastRunID)
			}
		}
		if err := s.opsAlerts.NotifyOperator(ctx, a, scheduleStallEpisodeKey(sb.ID, stall.Episode)); err != nil {
			s.warnf("server: schedule-outcome watch: schedule %s stalled (%s), alert not delivered — retried next sweep: %v", sb.ID, stall.Cause, err)
		}
	}
}

// scheduleStallEpisodeKey is the ops claim key of one stall episode of one
// schedule.
func scheduleStallEpisodeKey(scheduleID, episode string) string {
	return "sched-stall:" + scheduleID + ":" + episode
}
