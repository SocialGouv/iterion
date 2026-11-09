package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/internal/httpx"
	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/budgetfloor"
	"github.com/SocialGouv/iterion/pkg/errtrack"
	"github.com/SocialGouv/iterion/pkg/identity"
	"github.com/SocialGouv/iterion/pkg/orgusage"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/trigger"
	"github.com/SocialGouv/iterion/pkg/webhooks"
)

// OrgLimitDefaults are the platform-wide launch limits applied when a
// team doesn't carry its own override (Team field == 0). Zero means
// "no limit" — the safe default for existing deployments, EXCEPT the
// fork-author bound pair, whose platform default is 20 runs / $20
// (ADR-162 Q4): that bound is the perimeter against an outside party,
// not operator convenience, so its unset value is a number, and the
// escape hatch is the explicit `unbounded`.
type OrgLimitDefaults struct {
	MonthlyRunQuota   int
	MonthlyCostCapUSD float64
	MaxConcurrentRuns int
	LaunchRatePerMin  int
	// ForkAuthorRunQuota / ForkAuthorCostCapUSD are the per-contributor
	// bound of the fork review lane (runs and dollars per month, metered
	// on orgusage.ForkAuthorSubject). The ladder: a NEGATIVE org field is
	// the explicit `unbounded` escape hatch; 0 inherits THIS default; and
	// this default itself resolves from
	// ITERION_ORG_DEFAULT_FORK_AUTHOR_{RUN_QUOTA,COST_CAP_USD}, which
	// accepts the literal `unbounded` as 0 (no cap). 0 after the ladder
	// means no cap — AllowRun's own convention.
	ForkAuthorRunQuota   int
	ForkAuthorCostCapUSD float64
}

// launchDenial is one launch-gate refusal: an HTTP status, a stable
// machine-readable reason token (the SPA and API clients switch on
// it), and a human detail. Quota denials carry the month-reset time;
// throttle denials carry a Retry-After hint.
type launchDenial struct {
	status     int
	reason     string
	detail     string
	retryAfter time.Duration
	resetAt    time.Time
}

// launchDeniedError is a gate denial as an error, for the launch surfaces
// that record a refusal on a ledger instead of answering an HTTP request
// (the board dispatcher, whose card keeps the rule that refused it).
// Reason is the stable denial token below; Detail the sentence the HTTP
// envelope carries; RetryAfter / ResetAt the hints it would have sent.
type launchDeniedError struct {
	Reason     string
	Detail     string
	RetryAfter time.Duration
	ResetAt    time.Time
}

func (e *launchDeniedError) Error() string {
	if e.Detail == "" {
		return "launch gate: " + e.Reason
	}
	return "launch gate: " + e.Reason + ": " + e.Detail
}

// Preserve the HTTP denial details while giving durable consumers a typed
// postponement, distinct from a launch failure with a bounded retry budget.
func (e *launchDeniedError) Unwrap() error {
	if e.Reason == denyMaintenanceFence {
		return trigger.ErrMaintenanceHold
	}
	return nil
}

// err converts a denial for a caller that reports through an error chain.
// Nil-safe: an allowed launch has no error.
func (d *launchDenial) err() error {
	if d == nil {
		return nil
	}
	return &launchDeniedError{Reason: d.reason, Detail: d.detail, RetryAfter: d.retryAfter, ResetAt: d.resetAt}
}

// Stable denial reason tokens (API contract — documented in
// docs/quotas-and-limits.md).
const (
	denyOrgSuspended      = "org_suspended"
	denyMonthlyRunQuota   = "monthly_run_quota_exceeded"
	denyMonthlyCostCap    = "monthly_cost_cap_exceeded"
	denyConcurrencyCap    = "concurrency_cap_exceeded"
	denyLaunchRateLimited = "launch_rate_limited"
	// denyRepoQuota is one REPOSITORY over its own ceiling inside the
	// shared budget — distinct from the tenant-wide caps above, because the
	// operator's next move is different: raise that repo's quota, not the
	// org's.
	denyRepoQuota = "repo_quota_exceeded"
	// denyNoWorkspace refuses a signed-in user who belongs to no team (the
	// GitHub "submitter" tier): they have no workspace to launch into.
	denyNoWorkspace = "no_workspace"
	// denyMemberRoleRequired refuses a principal who HAS a workspace but not
	// the rung a launch takes: a viewer (read-only) or the orthogonal
	// config_editor capability (rank 0, ADR-078).
	denyMemberRoleRequired = "member_role_required"
	// The fork review lane's per-contributor bound (ADR-162 decision 5,
	// amending ADR-033): an untrusted launch fails CLOSED on every arm
	// that admits a trusted one unmetered, because this subject is the
	// perimeter against an outside party — not operator policy, whose
	// degraded arms fail open. Each token is one row of the lane's
	// delivery-reason table (docs/webhooks.md).
	denyForkTeamUnreadable = "fork_team_unreadable"
	denyForkOrgUnreadable  = "fork_org_unreadable"
	// denyForkMeteringUnavailable: no counter to meter the bound on — a
	// local-mode server (no identity store) or one built without the
	// orgusage store — or an untrusted subject that names no fork author
	// to meter. Both are 503s the surface may retry, never silent passes.
	denyForkMeteringUnavailable = "fork_metering_unavailable"
	denyForkMeteringError       = "fork_metering_error"
	// The bound's own ceilings, the per-contributor twin of the org's
	// monthly caps: 402s that reset on the 1st.
	denyForkAuthorRuns = "fork_author_run_quota_exceeded"
	denyForkAuthorCost = "fork_author_cost_cap_exceeded"
)

// launchExemptPrincipal reports whether a principal launches with no human
// role for the member rung to judge, and is therefore exempt from it: an
// inbound webhook delivery — which authenticates itself with the config's
// bearer token or an HMAC body signature (middleware_webhook.go), and that
// IS its authorization — and the daemons of this process (board dispatcher,
// cloud schedule tick, trigger spine, retry sweeper), which launch work no
// request asked for.
//
// Deliberately NOT every synthetic kind: a config-share editor link
// (KindShare) authorizes editing one team's config and nothing else
// (ADR-078). No launch path carries one, and the rung is what keeps it that
// way — an exemption written as IsSynthetic() would hand that public token
// class a launch it was never granted.
func launchExemptPrincipal(id auth.Identity) bool {
	return id.Kind == auth.KindWebhook || id.IsMachine()
}

// launchRungCleared reports whether a principal clears the member rung a
// launch takes: exempt by kind, or at least the member role of the team. The
// ONE definition of the rung, read by gateLaunch below and by the board
// write guard (board_cloud_routes.go) — the board's Ready column queues
// launches the dispatcher performs under an exempt principal, so the two
// surfaces owe the same answer and must not drift.
//
// Fail-CLOSED: an empty Role is refused unless the principal is marked
// exempt, so a future construction site that forgets its kind loses its
// launches visibly instead of gaining an unjudged one.
func launchRungCleared(id auth.Identity) bool {
	return launchExemptPrincipal(id) || id.Role.AtLeast(identity.RoleMember)
}

// machineLaunchCtx stamps the machine principal a daemon of this process
// launches under: the tenant whose caps and counters the launch is metered
// against, the actor it is audited as, and KindInternal — the mark the
// member rung below exempts, because a daemon launch has no human standing
// behind it. The four daemon launchers (board dispatcher, cloud schedule
// tick, trigger spine, retry sweeper) all mint here. The other hand-built
// launch identities of this package — the merge-gate auto-fix and relaunch
// lanes, the debounced webhook replay — re-stamp the inbound delivery they
// continue as KindWebhook and clear the rung by that kind, not by the Role
// they also carry.
func machineLaunchCtx(ctx context.Context, tenantID, actor string) context.Context {
	return auth.WithIdentity(ctx, auth.Identity{TeamID: tenantID, UserID: actor, Kind: auth.KindInternal})
}

// activeRunCounter is the optional store capability the concurrency
// cap needs: how many of the org's runs are currently active
// (queued + running). The Mongo store implements it; the filesystem
// store doesn't — local mode is single-operator and has no per-org
// concurrency semantics.
type activeRunCounter interface {
	CountActiveRunsByTenant(ctx context.Context, tenantID string) (int, error)
}

// orValue returns the team override when set (> 0), else the platform
// default.
func orValue[T int | float64](team, def T) T {
	if team > 0 {
		return team
	}
	return def
}

// launchAdmission is the undo handle for a granted (and metered)
// launch admission. nil means nothing was metered and rollback is a no-op.
// An admission with no monthly counter can still own a rate token; rollback
// releases only what was metered. Callers that abandon an admitted launch
// without creating any run (e.g. the loser of two concurrent
// duplicate webhook deliveries) call rollback so the monthly run
// counter stays true.
type launchAdmission struct {
	// Replacement credit is provisional until cancellation frees capacity.
	// This rechecks only concurrency, preserving the maintenance decision
	// and the quota/rate tokens already acquired by this admission.
	confirmReplacement func(context.Context) *launchDenial
	counter            orgusage.Counter
	usageKey           orgusage.Subject
	when               time.Time
	// forkSubject is the per-contributor metering subject the fork lane's
	// second charge reserved (orgusage.ForkAuthorSubject, composed on the
	// metering id the org budget used). Empty when the launch was not an
	// untrusted one — the single value the launch tail stamps onto the
	// target, whose carriers (run document, queue message, the runner's
	// AddSpend) re-enter this SAME subject instead of re-deriving it.
	forkSubject orgusage.Subject
	// The per-minute launch bucket whose token this launch consumed
	// (#1726). rollback refunds it: a launch the caller refuses after the
	// gate — a malformed body, a lost duplicate race — must not leave the
	// org's per-minute budget empty for a run that never was, 429-ing
	// every legitimate launch behind the loop. rate is nil when no bucket
	// was consumed (no limit configured, or no limiter).
	rate    authRateLimiterBackend
	rateKey string
	rateCfg authBucketCfg
}

// launchSubject names what a launch is, for the gates that reserve capacity
// for a workload or cap a repository. Both fields are best-effort: a surface
// that genuinely does not know (a plain .bot upload names no bot; a local run
// targets no repository) passes the zero value and is judged as ordinary,
// uncapped work — which is what it is.
type launchSubject struct {
	// ReplacingRunIDs are selected by the webhook's tenant/subject/bot scope.
	// The gate verifies each is live and owned by this team before provisionally
	// crediting it. The caller must confirmReplacement after cancellation.
	ReplacingRunIDs []string
	// ExistingRun is set after reading the run or its durable retry row. It exempts
	// continuation from maintenance, never from the existing quota gates.
	ExistingRun bool
	// BotID is the workload a reservation can name.
	BotID string
	// Repo is the forge slug (store.Run.ProjectPath), the same identity
	// pkg/credusage meters against — never a second one derived here.
	Repo string
	// Trust says who wrote the code this launch runs (ADR-162 decision 5:
	// "launchSubject carries the trust as a required field"). The zero value
	// is the trusted default — every surface that cannot name a trust
	// launches none, because the only producer of untrusted work (the fork
	// review gesture) sets it. It is REQUIRED in the sense the gate owes
	// its answer to: an untrusted subject is metered TWICE (the org's
	// budget and the per-contributor bound) and refused by every arm that
	// would admit it unmetered — a caller that forgot the field keeps the
	// pre-fork behaviour exactly, which for an untrusted launch is a
	// refusal at the disjointness gate one step earlier.
	Trust store.RunTrust
	// ForkAuthor names the OUTSIDE author the fork review gesture admitted,
	// for the per-contributor bound. Nil on every trusted subject; REQUIRED
	// on an untrusted one — gateMonthlyCaps refuses an untrusted subject it
	// cannot meter (denyForkMeteringUnavailable), never fails open.
	ForkAuthor *webhooks.ForkAuthorRef
}

func (a *launchAdmission) rollback(logger interface{ Warn(string, ...any) }) {
	if a == nil {
		return
	}
	if a.counter != nil && a.usageKey != "" {
		// Detached ctx, same rationale as AllowRun's deny-path rollback: the
		// abandoning request may already be cancelled.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), 5*time.Second)
		defer cancel()
		if err := a.counter.ReleaseRun(ctx, a.usageKey, a.when); err != nil && logger != nil {
			logger.Warn("launch gate: admission rollback for %s: %v (monthly run counter over-counts by one)", a.usageKey, err)
		}
		// The fork lane's SECOND counter releases with the first (decision
		// 5): the handle owns both reservations, and a caller that abandons
		// the launch hands back the author's unit with the org's — an
		// outside contributor's bound left holding a run that never was is
		// a denial-of-service against the person it exists to protect.
		if a.forkSubject != "" {
			if err := a.counter.ReleaseRun(ctx, a.forkSubject, a.when); err != nil && logger != nil {
				logger.Warn("launch gate: admission rollback for %s: %v (fork-author run counter over-counts by one)", a.forkSubject, err)
			}
		}
	}
	if a.rate != nil && a.rateKey != "" {
		a.rate.refund(a.rateKey, a.rateCfg)
	}
}

// gateLaunch is the shared run-launch admission gate: the member rung →
// suspend → per-repo quota → concurrency → launch rate → monthly cost
// cap → monthly run quota (the last one is also the metering
// increment). Called by
// handleLaunchRun, handleResumeRun, the WebSocket answer envelope,
// handleEmitTrigger, handlePipelineBoardTaskLaunch, the inbound webhook
// handlers, the retry sweeper, the cloud schedule tick, the trigger spine
// and the board dispatcher (processBoardCard) — every surface that launches
// a NEW workflow from a request, plus the daemons that launch on their own
// schedule (the table in docs/quotas-and-limits.md).
//
// NOT every path that reaches the engine: fork and rewind create a run from
// an EXISTING one (runs_control.go), and a resume driven by an existing
// run's own machinery (answer-human, the interaction answer, host-event, the
// assistant mission and watch coordinators) re-enters it. Those are
// authorized by the run-by-id ladder (ADR-103) and are not metered.
// On allow it returns the admission handle for the metered increment
// (nil when nothing was metered).
//
// Fail-open on a DEGRADED store read, mirroring the suspend check:
// quotas are an operator policy, not a hard security boundary — a
// transient Mongo blip must not wedge every launch. A team the store
// answers is GONE is not that case and is denied, since no later check
// can bound a run whose tenant does not exist. Super-admins bypass
// entirely.
// The run-quota increment is the one exception to fail-open being
// "free": when AllowRun errors the launch proceeds unmetered (logged).
//
// `subj` names WHAT is being launched, for the capacity reservations and the
// per-repository quotas (pkg/budgetfloor). It is an explicit parameter rather
// than a context value on purpose: a caller that forgot to pass it would make
// the protected workload read as ordinary, and the reservation would then
// REFUSE the very run it exists to protect. A compile error is the only
// version of that mistake anyone ever sees.
func (s *Server) gateLaunch(ctx context.Context, subj launchSubject) (*launchAdmission, *launchDenial) {
	id, _ := auth.FromContext(ctx)
	if d := s.gateMaintenance(ctx, subj); d != nil {
		return nil, d
	}
	st := s.authStore()
	// ADR-162 decision 5, the amendment to ADR-033: every arm below that
	// admits a launch UNMETERED refuses an untrusted subject instead. The
	// local-mode arm is the first — a local server has no orgusage counter
	// at all, so an untrusted launch there is unmeterable in principle,
	// and the super-admin bypass is the second: the fork-author bound is
	// the perimeter against an outside party, not operator policy, so no
	// principal stands above it. No surface can reach either arm with an
	// untrusted subject today (the in-process launch refuses one, decision
	// 10); the arms exist so the next surface inherits the closed posture.
	if !subj.Trust.Trusted() {
		if st == nil {
			return nil, s.forkClosedDenial(denyForkMeteringUnavailable,
				"this server runs in local mode and meters no launch — an untrusted launch cannot be admitted unmetered")
		}
		if id.IsSuperAdmin {
			return nil, s.forkClosedDenial(denyForkMeteringUnavailable,
				"the super-admin bypass admits a launch unmetered, which an untrusted subject never is — the per-contributor bound is the perimeter, not operator policy")
		}
	}
	// st == nil is local/filesystem mode (no auth, single operator); a
	// super-admin bypasses the gate entirely.
	if st == nil || id.IsSuperAdmin {
		return nil, nil
	}
	if id.TeamID == "" {
		// A signed-in cloud user with no team (the GitHub submitter tier) has
		// no workspace to launch into. Deny rather than fail-open, so the
		// teamless tier can't run unmetered work under the empty tenant.
		return nil, &launchDenial{
			status: http.StatusForbidden,
			reason: denyNoWorkspace,
			detail: "you are not a member of any workspace — ask an admin to add you to a team",
		}
	}
	// The rung. A launch spends the org's money and executes code in its
	// workspace, so it takes at least a MEMBER of the team: a viewer is
	// read-only and the orthogonal config_editor capability ranks 0
	// (ADR-078), so neither clears it. Enforced HERE rather than per
	// handler because this gate is the one thing every launch surface
	// crosses — the run-by-id choke point (run_scope.go) covers only the
	// routes that address an EXISTING run, and the surfaces that CREATE one
	// are outside its class. The board WRITE surfaces take the same rung one
	// layer earlier (boardWriteAuth, board_cloud_routes.go): their Ready
	// column QUEUES launches this gate then sees under a machine principal
	// it exempts, so a card refused there is the only moment a human role
	// can be read.
	//
	// Placed after the teamless arm, whose own refusal is more specific and
	// whose population is therefore never admitted past it, and BEFORE the
	// team read, whose degraded arm fails OPEN — behind it, a store blip
	// would have admitted the viewer this refuses. That ORDER has its own
	// witness (launch_role_gate_test.go), which a reordering reddens.
	if !launchRungCleared(id) {
		return nil, &launchDenial{
			status: http.StatusForbidden,
			reason: denyMemberRoleRequired,
			detail: "launching a run takes at least the member role in this workspace — ask a workspace admin to raise your role",
		}
	}
	// Reuse a Team the webhook middleware already loaded for its
	// suspend check (same document, same request) — one Mongo round
	// trip instead of two on the inbound-webhook hot path.
	t, ok := teamFromContext(ctx)
	if !ok || t.ID != id.TeamID {
		var err error
		t, err = st.GetTeam(ctx, id.TeamID)
		switch {
		case errors.Is(err, identity.ErrNotFound):
			// Not a blip — a definite answer. The token names a team that
			// no longer exists, so admitting it runs work under a tenant
			// nobody can suspend, bill or see. The teamless arm above
			// refuses that situation when the claim is empty; this is the
			// same situation arriving later. The webhook middleware draws
			// the same line one layer up (middleware_webhook.go).
			return nil, &launchDenial{
				status: http.StatusForbidden,
				reason: denyNoWorkspace,
				detail: "the workspace this session points at no longer exists — sign in again, or ask an admin to add you to a team",
			}
		case err != nil:
			// A degraded read keeps the fail-open of the doc comment, but
			// says so: what follows is a launch with no suspend check and
			// no metering, which is invisible from the outside otherwise —
			// EXCEPT for an untrusted subject, which decision 5 refuses
			// here: a team the gate cannot read is a team whose suspension
			// state and metering id are unknown, and an untrusted launch
			// admitted past this point would spend a budget nobody bounded.
			if !subj.Trust.Trusted() {
				return nil, s.forkClosedDenial(denyForkTeamUnreadable,
					fmt.Sprintf("the fork lane's budget holder (team %s) could not be read: %v — an untrusted launch is not admitted past an unreadable team", id.TeamID, err))
			}
			if s.logger != nil {
				s.logger.Warn("launch gate: team %s unreadable (%v) — launching UNGATED: suspend unchecked, run unmetered", id.TeamID, err)
			}
			return nil, nil
		}
	}
	// The team's parent org owns the monthly budget + the top-level
	// suspend. Either level being suspended blocks the launch.
	org := s.orgForTeam(ctx, st, id, t)
	// orgForTeam folds a FAILED org read into the zero Org (it cannot tell
	// the caller), and gateMonthlyCaps below would then fall back to the
	// team id as the metering key — silently metering the fork lane's
	// per-contributor bound on a different id than the org budget, and
	// skipping the org-level fork caps. An untrusted subject refuses here
	// instead: an org the gate cannot read is an org whose budget the gate
	// cannot bound. An orgless team (no OrgID on either side) is the
	// legitimate pre-backfill shape and meters on the team id as before.
	if !subj.Trust.Trusted() {
		orgID := id.OrgID
		if orgID == "" {
			orgID = t.OrgID
		}
		if orgID != "" && org.ID == "" {
			return nil, s.forkClosedDenial(denyForkOrgUnreadable,
				fmt.Sprintf("the fork lane's budget org %s could not be read — the per-contributor bound meters on the same id the org budget does, and an unreadable org is not one to guess", orgID))
		}
	}
	if !t.CanLaunch() {
		return nil, &launchDenial{
			status: http.StatusForbidden,
			reason: denyOrgSuspended,
			detail: "team cannot launch runs (suspended or read-only)",
		}
	}
	if org.ID != "" && !org.CanLaunch() {
		return nil, &launchDenial{
			status: http.StatusForbidden,
			reason: denyOrgSuspended,
			detail: "org cannot launch runs (suspended or read-only)",
		}
	}
	now := time.Now().UTC()
	floor := s.budgetFloorPolicy(ctx)
	if d := s.gateRepoQuota(ctx, floor, subj, now); d != nil {
		return nil, d
	}
	if d := s.gateConcurrency(ctx, t, floor, subj); d != nil {
		return nil, d
	}
	rateKey, rateCfg, d := s.gateLaunchRate(t)
	if d != nil {
		return nil, d
	}
	adm, d := s.gateMonthlyCaps(ctx, org, t, floor, subj, now)
	if d != nil && rateKey != "" {
		s.authLimiter.refund(rateKey, rateCfg)
	}
	if d == nil && adm == nil && rateKey != "" {
		adm = &launchAdmission{}
	}
	if adm != nil && rateKey != "" {
		adm.rate, adm.rateKey, adm.rateCfg = s.authLimiter, rateKey, rateCfg
	}
	if d == nil && len(subj.ReplacingRunIDs) > 0 {
		if adm == nil {
			adm = &launchAdmission{}
		}
		subj.ReplacingRunIDs = nil
		adm.confirmReplacement = func(ctx context.Context) *launchDenial {
			return s.gateConcurrency(ctx, t, floor, subj)
		}
	}
	return adm, d
}

// budgetFloorPolicy resolves the deployment's reservations, or the zero
// policy when the family is unwired or unwritten — which reserves and caps
// nothing, so every gate below behaves exactly as it did before.
func (s *Server) budgetFloorPolicy(ctx context.Context) budgetfloor.Policy {
	if s.budgetFloor == nil {
		return budgetfloor.Policy{}
	}
	if p := s.budgetFloor.Get(ctx); p != nil {
		return *p
	}
	return budgetfloor.Policy{}
}

// gateRepoQuota stops ONE repository from eating the shared budget. It is a
// ceiling, not a floor: the reservations above answer "what is held for this
// workload", this answers "how far may this repository go".
//
// Read off pkg/credusage's repository dimension, which is the meter the runs
// actually write — so the number this refuses on is one an operator can read
// back. The view that shows it is the PLATFORM one,
// `GET /api/admin/credentials/usage?repo=X` (`iterion remote admin` audience,
// the same as the policy itself): credusage.ListByRepo spans TENANTS by
// design, and this quota is deployment-wide, so it bounds what the repository
// consumed whoever ran it — including rows left under a previous team after a
// repo moved. The TEAM-scoped `usage --by-credential --repo X` narrows to one
// tenant (cred_usage_routes.go's oneTenant) and will therefore read LOWER than
// the number that refused the launch; that is the view to avoid reconciling
// this against.
//
// Fail-open on a degraded read, like every other quota here: a Mongo blip
// must not wedge a repository's launches.
//
// The super-admin bypass lives HERE rather than only in gateLaunch's prologue
// because this is also called on its own, by the one surface that learns its
// repository after the gate has already run (handleLaunchRun). Both callers
// must answer a super-admin the same way: a bypass that holds on five launch
// paths and not on the sixth is not a bypass, it is a surface-dependent
// refusal nobody can predict.
func (s *Server) gateRepoQuota(ctx context.Context, floor budgetfloor.Policy, subj launchSubject, now time.Time) *launchDenial {
	// Trimmed HERE because the meter is: the runner writes its RepoID as
	// strings.TrimSpace(run.ProjectPath), so an untrimmed slug would query a
	// ledger key nothing ever wrote and the quota would read as unused —
	// inert, and silently, which is the worst shape for a ceiling.
	repo := strings.TrimSpace(subj.Repo)
	if s.credUsage == nil || repo == "" {
		return nil
	}
	if id, _ := auth.FromContext(ctx); id.IsSuperAdmin {
		return nil
	}
	maxUSD, maxSpends := floor.RepoCap(repo)
	if maxUSD <= 0 && maxSpends <= 0 {
		return nil
	}
	rows, err := s.credUsage.ListByRepo(ctx, now, repo)
	if err != nil {
		if s.logger != nil {
			s.logger.Warn("launch gate: repo usage for %s: %v (fail-open)", repo, err)
		}
		return nil
	}
	var spent float64
	routeSpends := 0
	for _, r := range rows {
		// metered and estimated are added HERE and nowhere else: the quota is
		// a budget for the repository's consumption, and on a fleet mixing a
		// forfait with a metered key the two halves are both real usage even
		// though only one is an invoice. The REPORTING keeps them apart
		// (metered_usd / estimated_usd) precisely so this is the only place
		// the sum is taken, deliberately.
		spent += r.CostUSD
		// Not a run count: credusage increments this once per AddSpend, and
		// the runner calls AddSpend once per (credential, backend, model)
		// route an attempt charged. Named for what it counts everywhere it
		// is shown — see RepoQuota.RouteSpendsPerMonth.
		routeSpends += r.Runs
	}
	if maxUSD > 0 && spent >= maxUSD {
		return &launchDenial{
			status:  http.StatusPaymentRequired,
			reason:  denyRepoQuota,
			detail:  fmt.Sprintf("repository %s has used $%.2f of its $%.2f monthly quota", repo, spent, maxUSD),
			resetAt: nextMonthStart(now),
		}
	}
	if maxSpends > 0 && routeSpends >= maxSpends {
		return &launchDenial{
			status: http.StatusPaymentRequired,
			reason: denyRepoQuota,
			detail: fmt.Sprintf("repository %s has recorded %d of its %d monthly metered route-spends (one per credential+model route a run charges, so a two-model run counts twice)",
				repo, routeSpends, maxSpends),
			resetAt: nextMonthStart(now),
		}
	}
	return nil
}

// orgForTeam resolves the parent org for the launch gate: the JWT's
// active OrgID when present (the common REST path — no extra read), else
// the team's OrgID. Returns the zero Org (ID=="") on any miss; the gate
// then falls back to team-keyed metering so a pre-backfill row still
// launches and meters.
func (s *Server) orgForTeam(ctx context.Context, st identity.Store, id auth.Identity, t identity.Team) identity.Org {
	orgID := id.OrgID
	if orgID == "" {
		orgID = t.OrgID
	}
	if orgID == "" {
		return identity.Org{}
	}
	o, err := st.GetOrg(ctx, orgID)
	if err != nil {
		return identity.Org{}
	}
	return o
}

func (s *Server) gateConcurrency(ctx context.Context, t identity.Team, floor budgetfloor.Policy, subj launchSubject) *launchDenial {
	maxActive := orValue(t.MaxConcurrentRuns, s.orgDefaults.MaxConcurrentRuns)
	if maxActive <= 0 {
		// No cap to hold slots inside of. A reservation must not create one:
		// same rule as the window axis — a floor may hold work back, it may
		// never invent a ceiling.
		return nil
	}
	// Resolved BEFORE the reservation is applied, and not where the count is
	// taken, because a store that cannot count leaves this cap UNENFORCED —
	// every launch below returns nil. Subtracting a reserve from a ceiling
	// nothing enforces would refuse work on a cap that does not exist, which
	// is the invented ceiling the arm above refuses by name. So: no counter,
	// no cap, and therefore nothing for a reservation to hold a slot inside
	// of.
	counter, ok := s.cfg.Store.(activeRunCounter)
	if !ok {
		return nil
	}
	// Slots held for OTHER workloads are not available to this one. The
	// reserved bot itself faces the plain team cap, so a reservation never
	// costs its holder a slot.
	//
	// The rule this implements is deliberately the CONSERVATIVE one, because
	// the count below is per TENANT and not per bot: unreserved work may not
	// push the tenant's TOTAL past `cap - held`. That guarantees the reserved
	// workload always finds its slots free, and it over-refuses in one case —
	// while the holder is spending its own reserve, an unreserved launch is
	// denied even though the fleet is under its cap (cap 3, reserve 2, the
	// reviewer running 2: the third slot stays unused). Erring that way keeps
	// the operator's concurrency cap inviolate; the other way overcommits it.
	// Counting the reserved bots' OWN active runs (and subtracting only the
	// unused part of each reserve) is the exact rule, and it needs a per-bot
	// active count the run store does not expose today.
	configured := maxActive
	held := floor.OtherReservedSlots(subj.BotID)
	if held > 0 {
		if held >= maxActive {
			// Every slot is held elsewhere. The `active >= maxActive` test
			// below would refuse this launch too (0 >= 0), but it would say
			// "retry when one finishes" about a wait that no finishing run
			// ever ends — the operator's move is the reservation, not time.
			return &launchDenial{
				status:     http.StatusTooManyRequests,
				reason:     denyConcurrencyCap,
				detail:     fmt.Sprintf("all %d concurrency slots are reserved for other workloads", maxActive),
				retryAfter: 30 * time.Second,
			}
		}
		maxActive -= held
	}
	active, err := counter.CountActiveRunsByTenant(ctx, t.ID)
	if err != nil {
		if s.logger != nil {
			s.logger.Warn("launch gate: active-run count for %s: %v (fail-open)", t.ID, err)
		}
		return nil
	}
	seen := map[string]bool{}
	for _, runID := range subj.ReplacingRunIDs {
		if seen[runID] {
			continue
		}
		seen[runID] = true
		run, err := s.cfg.Store.LoadRun(ctx, runID)
		if err == nil && run != nil && run.TenantID == t.ID && (run.Status == store.RunStatusRunning || run.Status == store.RunStatusQueued) && active > 0 {
			active--
		}
	}
	if active >= maxActive {
		return &launchDenial{
			status:     http.StatusTooManyRequests,
			reason:     denyConcurrencyCap,
			detail:     concurrencyDenialDetail(active, maxActive, configured, held),
			retryAfter: 30 * time.Second,
		}
	}
	return nil
}

// concurrencyDenialDetail names the ceiling that actually refused, and where
// it came from when a reservation lowered it.
//
// The unreserved wording is unchanged — it is quoted in
// docs/quotas-and-limits.md and read by operators who configured no floor. But
// once a reserve applies, printing the LOWERED number alone sends the operator
// hunting for a cap of 1 in a team configured for 3, and there is nowhere to
// find it: the missing slots are in a platform settings family, not on their
// team. A refusal has to name the setting whose change would lift it.
func concurrencyDenialDetail(active, effective, configured, held int) string {
	if held <= 0 {
		return fmt.Sprintf("org has %d active runs (cap %d) — retry when one finishes", active, effective)
	}
	return fmt.Sprintf("org has %d active runs (cap %d — %d of the team's %d slots are reserved for other workloads) — retry when one finishes",
		active, effective, held, configured)
}

// gateLaunchRate consumes one token from the org's per-minute launch
// bucket and returns the key + config it consumed, so the admission can
// refund them on a launch the caller refuses afterwards (#1726).
func (s *Server) gateLaunchRate(t identity.Team) (string, authBucketCfg, *launchDenial) {
	perMin := orValue(t.LaunchRatePerMin, s.orgDefaults.LaunchRatePerMin)
	if perMin <= 0 || s.authLimiter == nil {
		return "", authBucketCfg{}, nil
	}
	bucket := authBucketCfg{rate: float64(perMin) / 60.0, burst: float64(perMin)}
	key := "orglaunch:" + t.ID
	if ok, retry := s.authLimiter.allow(key, bucket); !ok {
		return "", authBucketCfg{}, &launchDenial{
			status:     http.StatusTooManyRequests,
			reason:     denyLaunchRateLimited,
			detail:     fmt.Sprintf("org launch rate cap (%d/min) exceeded", perMin),
			retryAfter: retry,
		}
	}
	return key, bucket, nil
}

// forkClosedDenial is the one constructor for the fork lane's fail-closed
// arms (ADR-162 decision 5): a 503 the surface may retry, never a silent
// pass and never a 4xx the forge would read as a verdict. The retry
// sweeper treats every token it names as transient (retryDenialIsTransient).
func (s *Server) forkClosedDenial(reason, detail string) *launchDenial {
	return &launchDenial{status: http.StatusServiceUnavailable, reason: reason, detail: detail}
}

// forkAuthorCaps resolves the per-contributor bound's two ceilings off the
// ladder (ADR-162 decision 5 + Q4): a NEGATIVE org field is the explicit
// `unbounded` escape hatch; 0 inherits the platform default; the platform
// default itself comes from the settings env (20 runs / $20 unless the
// operator set another value or the literal `unbounded`). Zero after the
// ladder means no cap — AllowRun's own convention, so `unbounded` and
// "no default configured" are the same state to the counter.
func (s *Server) forkAuthorCaps(org identity.Org) (maxRuns int, capUSD float64) {
	if org.ForkAuthorRunQuota < 0 {
		maxRuns = 0
	} else if org.ForkAuthorRunQuota > 0 {
		maxRuns = org.ForkAuthorRunQuota
	} else {
		maxRuns = s.orgDefaults.ForkAuthorRunQuota
	}
	if org.ForkAuthorCostCapUSD < 0 {
		capUSD = 0
	} else if org.ForkAuthorCostCapUSD > 0 {
		capUSD = org.ForkAuthorCostCapUSD
	} else {
		capUSD = s.orgDefaults.ForkAuthorCostCapUSD
	}
	return maxRuns, capUSD
}

// gateMonthlyCaps charges the month's run counter and checks BOTH
// monthly caps (run quota + LLM cost cap) off the counter's single
// CAS round trip — the increment IS the metering, so this runs even
// with no caps configured.
//
// The budget is ORG-level: the counter is keyed by org.ID, so every
// team in the org charges the same monthly document and the caps sum
// across them automatically. The cap *values* come off the Org. When
// the org couldn't be resolved (pre-backfill row) we fall back to the
// team id as the metering key + platform defaults, so launches still
// meter.
//
// An UNTRUSTED subject is metered TWICE (ADR-162 decision 5): on the
// org's budget exactly as ADR-033 decides, and on
// orgusage.ForkAuthorSubject — the pull request author's own runs and
// dollars, beside the org's. Every fail-open arm below is closed for
// that subject: no counter, no author to meter, a metering error —
// each is a refusal, because this subject is the perimeter against an
// outside party, not operator policy.
func (s *Server) gateMonthlyCaps(ctx context.Context, org identity.Org, t identity.Team, floor budgetfloor.Policy, subj launchSubject, now time.Time) (*launchAdmission, *launchDenial) {
	if s.orgUsage == nil {
		if !subj.Trust.Trusted() {
			return nil, s.forkClosedDenial(denyForkMeteringUnavailable,
				"this server meters no launch (no usage counter wired) — an untrusted launch is refused rather than admitted unmetered")
		}
		return nil, nil
	}
	meteredID := org.ID
	if meteredID == "" {
		meteredID = t.ID
	}
	usageKey := orgusage.OrgSubject(meteredID)
	// The fork lane's second subject, composed HERE — the one place the
	// metering id is decided — so the subject charged, the one stamped on
	// the run document and the one the runner re-charges on its second
	// AddSpend are the same string by construction. A re-entry that holds
	// a stored subject (the retry sweeper reading a run document) carries
	// it ready-made and never re-derives the identity halves.
	var forkSubject orgusage.Subject
	if !subj.Trust.Trusted() {
		if subj.ForkAuthor == nil {
			return nil, s.forkClosedDenial(denyForkMeteringUnavailable,
				"the launch names no fork author to meter the per-contributor bound on — an untrusted launch is refused rather than admitted unmetered")
		}
		if subj.ForkAuthor.Subject != "" {
			forkSubject = orgusage.Subject(subj.ForkAuthor.Subject)
		} else if subj.ForkAuthor.Provider == "" || subj.ForkAuthor.AuthorID == "" {
			return nil, s.forkClosedDenial(denyForkMeteringUnavailable,
				"the fork author names no provider or no numeric forge id — the per-contributor bound is keyed on both (a login is renameable), and an untrusted launch is refused rather than metered on a guess")
		} else {
			forkSubject = orgusage.ForkAuthorSubject(meteredID, subj.ForkAuthor.Provider, subj.ForkAuthor.AuthorID)
		}
	}
	maxRuns := orValue(org.MonthlyRunQuota, s.orgDefaults.MonthlyRunQuota)
	capUSD := orValue(org.MonthlyCostCapUSD, s.orgDefaults.MonthlyCostCapUSD)
	// Dollars held for OTHER workloads come off this launch's ceiling. Only
	// when a cap exists: a reservation holds work back, it never creates the
	// cost cap it subtracts from — the same guard the window and slot axes
	// carry, and the one that keeps an uncapped deployment uncapped.
	configuredUSD, heldUSD := capUSD, 0.0
	if capUSD > 0 {
		if held := floor.OtherReservedUSD(subj.BotID); held > 0 {
			if held >= capUSD {
				// Nothing left for unreserved work — and that is a DENIAL,
				// not a zero ceiling: orgusage gates on `maxCostMillis > 0`
				// in both twins, so a cap lowered to 0 would stop enforcing
				// the cost cap for the rest of the month on exactly the
				// workloads the reserve holds back. Refused before AllowRun,
				// so the launch consumes no run slot either.
				return nil, &launchDenial{
					status:  http.StatusPaymentRequired,
					reason:  denyMonthlyCostCap,
					detail:  fmt.Sprintf("the monthly LLM cost cap ($%.2f) is entirely reserved for other workloads", capUSD),
					resetAt: nextMonthStart(now),
				}
			}
			capUSD -= held
			heldUSD = held
		}
	}
	deny, err := s.orgUsage.AllowRun(ctx, usageKey, now, maxRuns, orgusage.CostToMillis(capUSD))
	if err != nil {
		if !subj.Trust.Trusted() {
			return nil, s.forkClosedDenial(denyForkMeteringError,
				fmt.Sprintf("run metering for %s failed: %v — an untrusted launch is refused rather than admitted unmetered", usageKey, err))
		}
		if s.logger != nil {
			s.logger.Warn("launch gate: run metering for %s: %v (fail-open, launch unmetered)", usageKey, err)
		}
		return nil, nil
	}
	switch deny {
	case orgusage.DenyRuns:
		return nil, &launchDenial{
			status:  http.StatusPaymentRequired,
			reason:  denyMonthlyRunQuota,
			detail:  fmt.Sprintf("monthly run quota (%d) exhausted", maxRuns),
			resetAt: nextMonthStart(now),
		}
	case orgusage.DenyCost:
		return nil, &launchDenial{
			status:  http.StatusPaymentRequired,
			reason:  denyMonthlyCostCap,
			detail:  costCapDenialDetail(capUSD, configuredUSD, heldUSD),
			resetAt: nextMonthStart(now),
		}
	}
	adm := &launchAdmission{counter: s.orgUsage, usageKey: usageKey, when: now}
	if forkSubject != "" {
		// The SECOND charge: the author's own runs and dollars this month.
		// The org's increment above is already reserved by adm; an author
		// denial hands it back (ReleaseRun on the org key) before
		// returning, so a refused gesture costs the org nothing either.
		forkRuns, forkUSD := s.forkAuthorCaps(org)
		fdeny, ferr := s.orgUsage.AllowRun(ctx, forkSubject, now, forkRuns, orgusage.CostToMillis(forkUSD))
		if ferr != nil {
			releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), 5*time.Second)
			if rerr := s.orgUsage.ReleaseRun(releaseCtx, usageKey, now); rerr != nil && s.logger != nil {
				s.logger.Warn("launch gate: org counter rollback after a fork-author metering error: %v", rerr)
			}
			cancel()
			return nil, s.forkClosedDenial(denyForkMeteringError,
				fmt.Sprintf("fork-author metering for %s failed: %v — an untrusted launch is refused rather than admitted unmetered", forkSubject, ferr))
		}
		switch fdeny {
		case orgusage.DenyRuns:
			releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), 5*time.Second)
			if rerr := s.orgUsage.ReleaseRun(releaseCtx, usageKey, now); rerr != nil && s.logger != nil {
				s.logger.Warn("launch gate: org counter rollback after a fork-author run denial: %v", rerr)
			}
			cancel()
			return nil, &launchDenial{
				status:  http.StatusPaymentRequired,
				reason:  denyForkAuthorRuns,
				detail:  fmt.Sprintf("this contributor's monthly fork review quota (%d) is exhausted — a fresh gesture next month, or the org admin raises the bound", forkRuns),
				resetAt: nextMonthStart(now),
			}
		case orgusage.DenyCost:
			releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), 5*time.Second)
			if rerr := s.orgUsage.ReleaseRun(releaseCtx, usageKey, now); rerr != nil && s.logger != nil {
				s.logger.Warn("launch gate: org counter rollback after a fork-author cost denial: %v", rerr)
			}
			cancel()
			return nil, &launchDenial{
				status:  http.StatusPaymentRequired,
				reason:  denyForkAuthorCost,
				detail:  fmt.Sprintf("this contributor's monthly fork review spend ($%.2f) reached the bound — the reviews this author already ran consumed it", forkUSD),
				resetAt: nextMonthStart(now),
			}
		}
		adm.forkSubject = forkSubject
	}
	return adm, nil
}

// costCapDenialDetail is concurrencyDenialDetail's twin on the dollar axis,
// and it exists for the same reason: an org capped at $100 that reads
// "monthly LLM cost cap ($70.00) reached" has no $70 anywhere in its
// settings, and the $30 that explains it is in a platform family the org
// admin may not even be able to see. Naming the reserve turns a figure that
// looks like a bug into one that points at the operator who can lift it.
func costCapDenialDetail(effective, configured, held float64) string {
	if held <= 0 {
		return fmt.Sprintf("monthly LLM cost cap ($%.2f) reached", effective)
	}
	return fmt.Sprintf("monthly LLM cost cap ($%.2f — $%.2f of the org's $%.2f is reserved for other workloads) reached",
		effective, held, configured)
}

// nextMonthStart is when monthly quotas reset (first instant of the
// next UTC month).
func nextMonthStart(now time.Time) time.Time {
	u := now.UTC()
	return time.Date(u.Year(), u.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, 1, 0)
}

// writeLaunchDenial renders a denial AND counts it. This is the renderer of
// the ADMISSION gate: every production caller of it answers a refusal
// gateLaunch returned, so its refusals are run launches denied — the series
// iterion_launch_denied_total publishes and the starter alert sums with no
// reason filter (charts/iterion/templates/prometheus-rule.yaml). A refusal
// that is NOT a refused launch renders through writeRungDenial, which counts
// nothing.
func (s *Server) writeLaunchDenial(w http.ResponseWriter, r *http.Request, d *launchDenial) {
	if s.cfg.Metrics != nil {
		s.cfg.Metrics.LaunchDeniedTotal.WithLabelValues(d.reason).Inc()
	}
	s.renderLaunchDenial(w, r, d)
}

// writeRungDenial renders the same envelope for the member rung taken at a
// MOUNT (mutatingRungAuth, board_cloud_routes.go): the caller was changing a
// board, a team's automation or its shared knowledge, not launching a run.
// Same token, status,
// Retry-After, origin reflection and zero-status floor — the renderer below
// holds all of it — and deliberately no counter: a refused board write is
// not a refused launch, and four of them must not read as four refused
// launches on the gate's own series.
func (s *Server) writeRungDenial(w http.ResponseWriter, r *http.Request, d *launchDenial) {
	s.renderLaunchDenial(w, r, d)
}

// renderLaunchDenial is the envelope both renderers above share: `error`
// carries the stable reason token (machine contract), `detail` the human
// message, plus Retry-After / reset_at when applicable.
func (s *Server) renderLaunchDenial(w http.ResponseWriter, r *http.Request, d *launchDenial) {
	if d.retryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(d.retryAfter.Seconds())+1))
	}
	body := map[string]string{"error": d.reason, "detail": d.detail}
	if !d.resetAt.IsZero() {
		body["reset_at"] = d.resetAt.Format(time.RFC3339)
	}
	s.reflectAllowedOrigin(w, r)
	// Floor the code. WriteHeader(0) PANICS in net/http, killing the request
	// goroutine — the caller sees a dropped connection, and a forge answers
	// repeated failures by disabling the hook. No denial constructed today
	// leaves status zero; that was equally true of the sibling arm in
	// writeSingleLaunchResult until a new status reached it.
	status := d.status
	if status == 0 {
		status = http.StatusInternalServerError
	}
	if status >= http.StatusInternalServerError {
		body["error_code"] = d.reason
		httpx.WriteErrorJSON(errtrack.HTTPErrorWriter(w, r, s.logger), status, body)
		return
	}
	httpx.WriteJSON(w, status, body)
}
