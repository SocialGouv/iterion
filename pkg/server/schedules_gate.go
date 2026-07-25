package server

import (
	"context"
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
	// The provenance query is tenant-scoped the same way the launch is;
	// a nil store degrades to guard-only (Apply skips the overlap leg).
	var lister schedgate.ScheduleRunLister
	if s.cfg.Store != nil {
		lister = tenantRunLister{s: s.cfg.Store, tenantID: sb.TenantID, actor: "scheduler:" + sb.BotID}
	}
	out := schedgate.Apply(ctx, schedgate.GateInput{
		Policy:     sb.Policy(),
		Lister:     lister,
		ScheduleID: sb.ID,
		Record:     s.cloudTickRecord(sb, ""),
		GuardEnv: []string{
			"ITERION_SCHEDULE=" + sb.ID,
			"ITERION_SCHEDULE_BOT=" + sb.BotID,
			"ITERION_TENANT=" + sb.TenantID,
		},
		Logger: s.logger,
	})
	if !out.Proceed {
		return false, "", out.Record
	}
	// out.ReapRunIDs is intentionally dropped here: in cloud the NATS lease
	// is the liveness authority, so reaping a stranded run is owned by the
	// lease-aware queue sweeper/reaper (force-flipping a still-leased run
	// would fight it). schedgate's stale_after still drives relaunch; the
	// sweeper's lease TTL drives the eventual status flip.
	return true, out.GuardStdout, schedgate.TickRecord{}
}

// tenantRunLister stamps the tenant identity on the context of every
// store call, mirroring what the inline gate did with store.WithIdentity.
type tenantRunLister struct {
	s        store.RunStore
	tenantID string
	actor    string
}

func (t tenantRunLister) ListRunsBySchedule(ctx context.Context, scheduleID string) ([]string, error) {
	return t.s.ListRunsBySchedule(store.WithIdentity(ctx, t.tenantID, t.actor), scheduleID)
}

func (t tenantRunLister) LoadRun(ctx context.Context, runID string) (*store.Run, error) {
	return t.s.LoadRun(store.WithIdentity(ctx, t.tenantID, t.actor), runID)
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
