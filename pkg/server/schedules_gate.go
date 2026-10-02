package server

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/cloudsched"
	"github.com/SocialGouv/iterion/pkg/schedgate"
	"github.com/SocialGouv/iterion/pkg/store"
)

// envCloudScheduleGuards opts a cloud deployment into running schedule guards.
const envCloudScheduleGuards = "ITERION_CLOUD_SCHEDULE_GUARDS"

// cloudGuardRefusal says why a cloud schedule's guard may not run: a guard is
// a shell command, and a cloud server runs it in its own pod — beside the
// platform's service account, network and processes — on behalf of whoever
// may manage a team's schedules. A deployment whose teams are its operators
// opts in with ITERION_CLOUD_SCHEDULE_GUARDS=allow. Nil when the guard is
// empty, the server is not a cloud one, or the deployment opted in.
func (s *Server) cloudGuardRefusal(guard string) error {
	if strings.TrimSpace(guard) == "" || s.cfg.Mode != "cloud" ||
		strings.EqualFold(strings.TrimSpace(os.Getenv(envCloudScheduleGuards)), "allow") {
		return nil
	}
	return fmt.Errorf("a schedule guard is a shell command a cloud server would run in its own pod: refused unless the deployment sets %s=allow", envCloudScheduleGuards)
}

// cloudScheduleGate is the cloudsched.GateFunc: it runs the overlap
// policy + guard for a slot THIS replica already won (the CAS is the
// exactly-once authority; the gate only decides launch-vs-skip for the
// consumed slot). The guard executes in the server pod's working
// directory, and only where the deployment opted in (cloudGuardRefusal);
// a repo-local guard is a local-mode feature (the runner, not the server,
// holds the clone).
func (s *Server) cloudScheduleGate(ctx context.Context, sb cloudsched.ScheduledBot) (bool, string, schedgate.TickRecord) {
	if err := s.cloudGuardRefusal(sb.Guard); err != nil {
		rec := s.cloudTickRecord(sb, schedgate.TickGuardError)
		rec.Reason = err.Error()
		return false, "", rec
	}
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
