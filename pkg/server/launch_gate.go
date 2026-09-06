package server

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/SocialGouv/iterion/internal/httpx"
	"github.com/SocialGouv/iterion/pkg/auth"
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

// gateLaunch is the shared run-launch admission gate: suspend →
// concurrency → launch rate → monthly cost cap → monthly run quota
// (the last one is also the metering increment). Called by
// handleLaunchRun, handleResumeRun, the inbound webhook handlers, the retry
// sweeper and the board dispatcher (processBoardCard) — every cloud launch
// surface (the table in docs/quotas-and-limits.md).
// On allow it returns the admission handle for the metered increment
// (nil when nothing was metered).
//
// Fail-open on store errors, mirroring the suspend check: quotas are an
// operator policy, not a hard security boundary — a transient Mongo
// blip must not wedge every launch. Super-admins bypass entirely.
// The run-quota increment is the one exception to fail-open being
// "free": when AllowRun errors the launch proceeds unmetered (logged).
func (s *Server) gateLaunch(ctx context.Context) (*launchAdmission, *launchDenial) {
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
		if err != nil {
			return nil, nil // fail-open (see doc comment)
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
	if d := s.gateConcurrency(ctx, t); d != nil {
		return nil, d
	}
	if d := s.gateLaunchRate(t); d != nil {
		return nil, d
	}
	return s.gateMonthlyCaps(ctx, org, t, now)
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

func (s *Server) gateConcurrency(ctx context.Context, t identity.Team) *launchDenial {
	maxActive := orValue(t.MaxConcurrentRuns, s.orgDefaults.MaxConcurrentRuns)
	if maxActive <= 0 {
		return nil
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
func (s *Server) gateMonthlyCaps(ctx context.Context, org identity.Org, t identity.Team, now time.Time) (*launchAdmission, *launchDenial) {
	if s.orgUsage == nil {
		return nil, nil
	}
	usageKey := org.ID
	if usageKey == "" {
		usageKey = t.ID
	}
	maxRuns := orValue(org.MonthlyRunQuota, s.orgDefaults.MonthlyRunQuota)
	capUSD := orValue(org.MonthlyCostCapUSD, s.orgDefaults.MonthlyCostCapUSD)
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
