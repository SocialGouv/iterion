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
	"github.com/SocialGouv/iterion/pkg/identity"
	"github.com/SocialGouv/iterion/pkg/orgusage"
)

// OrgLimitDefaults are the platform-wide launch limits applied when a
// team doesn't carry its own override (Team field == 0). Zero means
// "no limit" — the safe default for existing deployments.
type OrgLimitDefaults struct {
	MonthlyRunQuota   int
	MonthlyCostCapUSD float64
	MaxConcurrentRuns int
	LaunchRatePerMin  int
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
)

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
// launch admission. nil (or an admission with no counter) means
// nothing was metered — fail-open, super-admin, local mode — and
// rollback is a no-op. Callers that abandon an admitted launch
// without creating any run (e.g. the loser of two concurrent
// duplicate webhook deliveries) call rollback so the monthly run
// counter stays true.
type launchAdmission struct {
	counter  orgusage.Counter
	usageKey string
	when     time.Time
}

// launchSubject names what a launch is, for the gates that reserve capacity
// for a workload or cap a repository. Both fields are best-effort: a surface
// that genuinely does not know (a plain .bot upload names no bot; a local run
// targets no repository) passes the zero value and is judged as ordinary,
// uncapped work — which is what it is.
type launchSubject struct {
	// BotID is the workload a reservation can name.
	BotID string
	// Repo is the forge slug (store.Run.ProjectPath), the same identity
	// pkg/credusage meters against — never a second one derived here.
	Repo string
}

func (a *launchAdmission) rollback(logger interface{ Warn(string, ...any) }) {
	if a == nil || a.counter == nil || a.usageKey == "" {
		return
	}
	// Detached ctx, same rationale as AllowRun's deny-path rollback: the
	// abandoning request may already be cancelled.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), 5*time.Second)
	defer cancel()
	if err := a.counter.ReleaseRun(ctx, a.usageKey, a.when); err != nil && logger != nil {
		logger.Warn("launch gate: admission rollback for %s: %v (monthly run counter over-counts by one)", a.usageKey, err)
	}
}

// gateLaunch is the shared run-launch admission gate: suspend → per-repo
// quota → concurrency → launch rate → monthly cost cap → monthly run quota
// (the last one is also the metering increment). Called by
// handleLaunchRun, handleResumeRun, the inbound webhook handlers, the retry
// sweeper and the board dispatcher (processBoardCard) — every cloud launch
// surface (the table in docs/quotas-and-limits.md).
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
	st := s.authStore()
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
			// no metering, which is invisible from the outside otherwise.
			if s.logger != nil {
				s.logger.Warn("launch gate: team %s unreadable (%v) — launching UNGATED: suspend unchecked, run unmetered", id.TeamID, err)
			}
			return nil, nil
		}
	}
	// The team's parent org owns the monthly budget + the top-level
	// suspend. Either level being suspended blocks the launch.
	org := s.orgForTeam(ctx, st, id, t)
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
	if d := s.gateLaunchRate(t); d != nil {
		return nil, d
	}
	return s.gateMonthlyCaps(ctx, org, t, floor, subj, now)
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
func (s *Server) gateRepoQuota(ctx context.Context, floor budgetfloor.Policy, subj launchSubject, now time.Time) *launchDenial {
	// Trimmed HERE because the meter is: the runner writes its RepoID as
	// strings.TrimSpace(run.ProjectPath), so an untrimmed slug would query a
	// ledger key nothing ever wrote and the quota would read as unused —
	// inert, and silently, which is the worst shape for a ceiling.
	repo := strings.TrimSpace(subj.Repo)
	if s.credUsage == nil || repo == "" {
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
	if held := floor.OtherReservedSlots(subj.BotID); held > 0 {
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
	counter, ok := s.cfg.Store.(activeRunCounter)
	if !ok {
		return nil
	}
	active, err := counter.CountActiveRunsByTenant(ctx, t.ID)
	if err != nil {
		if s.logger != nil {
			s.logger.Warn("launch gate: active-run count for %s: %v (fail-open)", t.ID, err)
		}
		return nil
	}
	if active >= maxActive {
		return &launchDenial{
			status:     http.StatusTooManyRequests,
			reason:     denyConcurrencyCap,
			detail:     fmt.Sprintf("org has %d active runs (cap %d) — retry when one finishes", active, maxActive),
			retryAfter: 30 * time.Second,
		}
	}
	return nil
}

func (s *Server) gateLaunchRate(t identity.Team) *launchDenial {
	perMin := orValue(t.LaunchRatePerMin, s.orgDefaults.LaunchRatePerMin)
	if perMin <= 0 || s.authLimiter == nil {
		return nil
	}
	bucket := authBucketCfg{rate: float64(perMin) / 60.0, burst: float64(perMin)}
	if ok, retry := s.authLimiter.allow("orglaunch:"+t.ID, bucket); !ok {
		return &launchDenial{
			status:     http.StatusTooManyRequests,
			reason:     denyLaunchRateLimited,
			detail:     fmt.Sprintf("org launch rate cap (%d/min) exceeded", perMin),
			retryAfter: retry,
		}
	}
	return nil
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
func (s *Server) gateMonthlyCaps(ctx context.Context, org identity.Org, t identity.Team, floor budgetfloor.Policy, subj launchSubject, now time.Time) (*launchAdmission, *launchDenial) {
	if s.orgUsage == nil {
		return nil, nil
	}
	usageKey := org.ID
	if usageKey == "" {
		usageKey = t.ID
	}
	maxRuns := orValue(org.MonthlyRunQuota, s.orgDefaults.MonthlyRunQuota)
	capUSD := orValue(org.MonthlyCostCapUSD, s.orgDefaults.MonthlyCostCapUSD)
	// Dollars held for OTHER workloads come off this launch's ceiling. Only
	// when a cap exists: a reservation holds work back, it never creates the
	// cost cap it subtracts from — the same guard the window and slot axes
	// carry, and the one that keeps an uncapped deployment uncapped.
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
		}
	}
	deny, err := s.orgUsage.AllowRun(ctx, usageKey, now, maxRuns, orgusage.CostToMillis(capUSD))
	if err != nil {
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
			detail:  fmt.Sprintf("monthly LLM cost cap ($%.2f) reached", capUSD),
			resetAt: nextMonthStart(now),
		}
	}
	return &launchAdmission{counter: s.orgUsage, usageKey: usageKey, when: now}, nil
}

// nextMonthStart is when monthly quotas reset (first instant of the
// next UTC month).
func nextMonthStart(now time.Time) time.Time {
	u := now.UTC()
	return time.Date(u.Year(), u.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, 1, 0)
}

// writeLaunchDenial renders a denial: `error` carries the stable
// reason token (machine contract), `detail` the human message, plus
// Retry-After / reset_at when applicable.
func (s *Server) writeLaunchDenial(w http.ResponseWriter, r *http.Request, d *launchDenial) {
	if s.cfg.Metrics != nil {
		s.cfg.Metrics.LaunchDeniedTotal.WithLabelValues(d.reason).Inc()
	}
	if d.retryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(d.retryAfter.Seconds())+1))
	}
	body := map[string]string{"error": d.reason, "detail": d.detail}
	if !d.resetAt.IsZero() {
		body["reset_at"] = d.resetAt.Format(time.RFC3339)
	}
	s.reflectAllowedOrigin(w, r)
	httpx.WriteJSON(w, d.status, body)
}
