package server

import (
	"context"
	"fmt"
	"time"

	"github.com/SocialGouv/iterion/pkg/cloudsched"
	"github.com/SocialGouv/iterion/pkg/schedgate"
	"github.com/SocialGouv/iterion/pkg/store"
)

// cloudScheduleGate is the cloudsched.GateFunc: it runs the overlap
// policy + guard for a slot THIS replica already won (the CAS is the
// exactly-once authority; the gate only decides launch-vs-skip for the
// consumed slot). The guard executes in the server pod's working
// directory — cloud guards are for API-shaped checks (gh/curl); a
// repo-local guard is a local-mode feature (the runner, not the
// server, holds the clone).
func (s *Server) cloudScheduleGate(ctx context.Context, sb cloudsched.ScheduledBot) (bool, string, schedgate.TickRecord) {
	policy := sb.Policy()

	// Tenant-scope the provenance query the same way the launch does.
	tctx := store.WithIdentity(ctx, sb.TenantID, "scheduler:"+sb.BotID)
	if s.cfg.Store != nil {
		live := schedgate.LiveRunsForSchedule(tctx, s.cfg.Store, sb.ID, s.logger)
		if decision, blocking := schedgate.EvaluateOverlap(live, policy); decision == schedgate.DecisionSkipOverlap {
			rec := s.cloudTickRecord(sb, schedgate.TickSkippedOverlap)
			rec.BlockingRunID = blocking
			rec.Reason = fmt.Sprintf("blocked by live run %s (%d live, overlap=%s)", blocking, len(live), policy.Overlap)
			return false, "", rec
		}
	}

	if policy.Guard != "" {
		res := schedgate.RunGuard(ctx, schedgate.GuardSpec{
			Command: policy.Guard,
			Env: []string{
				"ITERION_SCHEDULE=" + sb.ID,
				"ITERION_SCHEDULE_BOT=" + sb.BotID,
				"ITERION_TENANT=" + sb.TenantID,
			},
			Timeout: policy.GuardTimeoutDuration(),
		})
		switch res.Kind {
		case schedgate.GuardBlocked:
			rec := s.cloudTickRecord(sb, schedgate.TickGuardBlocked)
			rec.Reason = fmt.Sprintf("guard exited %d — nothing to do", res.ExitCode)
			rec.ApplyGuard(res)
			return false, "", rec
		case schedgate.GuardError:
			rec := s.cloudTickRecord(sb, schedgate.TickGuardError)
			rec.Reason = "guard failed to execute"
			rec.ApplyGuard(res)
			return false, "", rec
		default:
			return true, res.Stdout, schedgate.TickRecord{}
		}
	}
	return true, "", schedgate.TickRecord{}
}

// cloudScheduleAudit lands every tick decision on the tenant audit
// trail (actions schedule.tick.*), readable via the existing
// /api/teams/{id}/audit?action= filter.
func (s *Server) cloudScheduleAudit(rec schedgate.TickRecord) {
	if rec.ScheduleID == "" {
		return
	}
	s.auditSystem(rec.TenantID, "scheduler:"+rec.BotID,
		"schedule.tick."+string(rec.Decision), "schedule", rec.ScheduleID, rec.ToAuditMeta())
}

func (s *Server) cloudTickRecord(sb cloudsched.ScheduledBot, decision schedgate.TickDecision) schedgate.TickRecord {
	rec := schedgate.NewTickRecord(schedgate.SurfaceCloud, sb.ID, time.Now().UTC(), decision)
	rec.ScheduleName = sb.BotID
	rec.BotID = sb.BotID
	rec.TenantID = sb.TenantID
	rec.Cron = sb.Cron
	return rec
}
