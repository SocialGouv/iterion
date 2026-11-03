package server

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"time"

	"github.com/google/uuid"

	"github.com/SocialGouv/iterion/pkg/audit"
	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
)

// auditActionRunLaunched is the audit token one run launch writes, whatever
// surface launched it: one tenant-scoped row per run, keyed by run id.
const auditActionRunLaunched = "run.launched"

// auditActionRunForked is the audit token a fork writes: a fork is a new
// run that never crosses launchRun, so it carries its own line, keyed by
// the child run id and naming its parent in meta.
const auditActionRunForked = "run.forked"

// pipelineAdmissionActor names the local pipeline-admission loop on the
// audit line of a launch it drives: the loop launches with no principal on
// its context (a local board has no tenancy), so nothing else can name it.
const pipelineAdmissionActor = "pipeline-admission"

// launchOrigin is what the audit line of a launch records about where the
// launch came from, beyond the principal the launch context carries.
type launchOrigin struct {
	// r is the inbound request the launch serves, when one exists. Its
	// principal attributes the line when the launch context carries none
	// (a request lane that detaches its launch from the request), and its
	// client address and user-agent ride the line. Nil for a daemon.
	r *http.Request
	// system names the subsystem launching when neither the launch context
	// nor r carries a principal (the local pipeline-admission loop).
	system string
	// ip is the client address when the request cannot say it: a webhook
	// delivery replayed from a parked row carries the address the delivery
	// arrived from, and the replay's request is synthetic.
	ip string
}

// requestLaunch attributes a launch to the inbound request it serves.
func requestLaunch(r *http.Request) launchOrigin { return launchOrigin{r: r} }

// systemLaunch attributes a launch to a daemon of this process. The name is
// the fallback actor; a launch context stamped with the daemon's own owner
// (board dispatcher, scheduler, trigger spine) attributes by that owner.
func systemLaunch(name string) launchOrigin { return launchOrigin{system: name} }

// webhookLaunch attributes a launch to an inbound webhook delivery: r is the
// delivery's request (nil, or synthetic, on a replay lane) and srcIP the
// address the intake resolved for it.
func webhookLaunch(r *http.Request, srcIP string) launchOrigin {
	return launchOrigin{r: r, ip: srcIP}
}

// clientIP is the address the audit line records: the explicit one when
// the lane resolved it, else the request's, else nothing. The intake's
// resolved address wins over a re-derivation from r, which may be the
// synthetic request of a replay (no peer address at all).
func (o launchOrigin) clientIP(s *Server) string {
	if o.ip != "" {
		return o.ip
	}
	if o.r != nil {
		return s.clientIP(o.r)
	}
	return ""
}

func (o launchOrigin) userAgent() string {
	if o.r != nil {
		return o.r.UserAgent()
	}
	return ""
}

// launchRun is the ONE site of this package that asks the run service to
// launch: every launch surface — the HTTP handler, the inbound webhook
// tail, the cloud schedule tick, the board dispatcher, the trigger spine,
// the pipeline-admission loop — crosses it, so a launch that started work,
// or may have, has its `run.launched` audit line whatever launched it (the
// one other way a run comes to exist, a fork, writes `run.forked` from
// auditRunForked). TestLaunchAuditSweep keeps the class closed: a direct
// runs.Launch elsewhere is the one launch nobody audited.
//
// The error contract is the run service's own, untouched: the error is
// returned as is. The line follows the fact the meters read off that error,
// RunMayHaveStarted: a launch that errors after its run was persisted, or
// after a publish that may have landed, is a run the runner may execute, so
// it writes its line with meta.outcome=may_have_started and the run id the
// error names. A launch refused before anything could execute writes none.
func (s *Server) launchRun(ctx context.Context, from launchOrigin, spec runview.LaunchSpec) (*runview.LaunchResult, error) {
	return s.launchRunWith(ctx, s.runs, from, spec)
}

// launchRunWith is launchRun on an explicit run service — the pipeline
// lanes receive theirs as a parameter (the server's own in production, a
// probe service under test) and launch through it.
func (s *Server) launchRunWith(ctx context.Context, runs *runview.Service, from launchOrigin, spec runview.LaunchSpec) (*runview.LaunchResult, error) {
	if runs == nil {
		return nil, errors.New("launch: no run service wired")
	}
	res, err := runs.Launch(ctx, spec)
	if err != nil {
		if runview.RunMayHaveStarted(err) {
			s.auditRunLaunched(ctx, from, spec, launchErrorRunID(err, spec), launchOutcomeMayHaveStarted)
		}
		return nil, err
	}
	s.auditRunLaunched(ctx, from, spec, res.RunID, "")
	return res, nil
}

// launchOutcomeMayHaveStarted is the meta.outcome of the line a failed
// launch writes when the error says its run may still execute.
const launchOutcomeMayHaveStarted = "may_have_started"

// launchErrorRunID is the run a failed launch may have left behind: the one
// the error names, else the id the caller supplied, else unknown — the line
// is still written, the run id is then the one fact it lacks.
func launchErrorRunID(err error, spec runview.LaunchSpec) string {
	var persisted *runview.RunPersistedError
	if errors.As(err, &persisted) && persisted.RunID != "" {
		return persisted.RunID
	}
	return spec.RunID
}

// launchTriggered is the trigger spine's launch: the spine holds no *Server
// by design, so it is handed this bound closure instead of the run service.
func (s *Server) launchTriggered(ctx context.Context, spec runview.LaunchSpec) (*runview.LaunchResult, error) {
	return s.launchRun(ctx, systemLaunch(triggerSpineActor), spec)
}

// auditRunLaunched writes the tenant-scoped `run.launched` line of one
// launched run. The actor is the principal on the launch context (the
// signed-in user, the synthetic webhook principal, the daemon's own owner),
// else the request's, else the origin's subsystem name; the tenant is the
// one the run was scoped to. Meta names the bot and the trigger — never the
// launch vars, which may carry secret material. A non-empty outcome rides
// meta.outcome: the launch returned an error, yet its run may execute.
//
// The request's principal is a real lane, not a belt-and-braces: the
// operator's "launch now" on a pipeline card detaches its launch from the
// request (a Background context, so the run outlives the HTTP call), and
// that context names nobody — without the request the line would read as
// a system launch of no tenant, which no listing shows.
func (s *Server) auditRunLaunched(ctx context.Context, from launchOrigin, spec runview.LaunchSpec, runID, outcome string) {
	if s.auditStore == nil {
		return
	}
	id, ok := auth.FromContext(ctx)
	if !ok && from.r != nil {
		id, _ = auth.FromContext(from.r.Context())
	}
	tenantID, _ := store.TenantFromContext(ctx)
	if tenantID == "" {
		tenantID = id.TeamID
	}
	actorID := id.UserID
	if actorID == "" {
		actorID, _ = store.OwnerFromContext(ctx)
	}
	if actorID == "" {
		actorID = from.system
	}
	meta := launchAuditMeta(from, spec)
	if outcome != "" {
		if meta == nil {
			meta = map[string]any{}
		}
		meta["outcome"] = outcome
	}
	s.auditInsert(audit.Event{
		ID:        uuid.NewString(),
		Scope:     audit.ScopeTenant,
		TenantID:  tenantID,
		ActorID:   actorID,
		ActorKind: auditActorKind(id, actorID),
		Action:    auditActionRunLaunched,
		Target:    "run",
		TargetID:  runID,
		Meta:      meta,
		IP:        from.clientIP(s),
		UserAgent: from.userAgent(),
		CreatedAt: time.Now().UTC(),
	})
}

// auditRunForked writes the tenant-scoped `run.forked` line of a fork the
// request r served: the child run is the target, the parent rides meta.
// The actor, address and user-agent are the request's, like every other
// request-served audit line.
func (s *Server) auditRunForked(r *http.Request, childID, parentID string) {
	ident, _ := auth.FromContext(r.Context())
	s.auditTenant(r, ident.TeamID, auditActionRunForked, "run", childID, map[string]any{"parent_run_id": parentID})
}

// Trigger values of a request-served launch whose spec carries no
// provenance of its own: the launch form of the studio, or an API / CLI
// client — told apart as the login routes tell them apart.
const (
	launchTriggerStudio = "studio"
	launchTriggerAPI    = "api"
)

// launchAuditMeta is the small, non-secret description of a launch: the bot
// and workflow file, the repository, and the trigger — the run's own
// provenance when the spec carries one (RunSource.Kind: dispatcher |
// schedule | studio_chat | webhook), else the client of the request that
// served it (studio | api), else nothing (a daemon's line already names
// the daemon as actor). Delivery and schedule ids tie the line to the
// record that caused the run.
func launchAuditMeta(from launchOrigin, spec runview.LaunchSpec) map[string]any {
	meta := map[string]any{}
	if spec.BotID != "" {
		meta["bot"] = spec.BotID
	}
	if spec.FilePath != "" {
		meta["workflow"] = filepath.Base(spec.FilePath)
	}
	if spec.ProjectPath != "" {
		meta["repo"] = spec.ProjectPath
	}
	trigger := ""
	if src := spec.SourceRef; src != nil {
		trigger = src.Kind
		if src.DeliveryID != "" {
			meta["delivery_id"] = src.DeliveryID
		}
		if src.ScheduleID != "" {
			meta["schedule_id"] = src.ScheduleID
		}
	}
	if trigger == "" && from.r != nil {
		trigger = launchTriggerAPI
		if isBrowserClient(from.r) {
			trigger = launchTriggerStudio
		}
	}
	if trigger != "" {
		meta["trigger"] = trigger
	}
	if len(meta) == 0 {
		return nil
	}
	return meta
}
