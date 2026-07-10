package server

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/identity"
	"github.com/SocialGouv/iterion/pkg/orgusage"
	"github.com/SocialGouv/iterion/pkg/store"
)

// fakeActiveStore satisfies the activeRunCounter capability the
// concurrency gate type-asserts on cfg.Store. The embedded nil
// RunStore is never touched by the gate.
type fakeActiveStore struct {
	store.RunStore
	active int
}

func (f fakeActiveStore) CountActiveRunsByTenant(context.Context, string) (int, error) {
	return f.active, nil
}

// erroringCounter forces the fail-open paths.
type erroringCounter struct{}

func (erroringCounter) AllowRun(context.Context, string, time.Time, int, int64) (orgusage.DenyReason, error) {
	return orgusage.DenyNone, context.DeadlineExceeded
}
func (erroringCounter) AddSpend(context.Context, string, time.Time, float64, int64, int64) error {
	return context.DeadlineExceeded
}
func (erroringCounter) ReleaseRun(context.Context, string, time.Time) error {
	return context.DeadlineExceeded
}
func (erroringCounter) Usage(context.Context, string, time.Time) (orgusage.MonthlyUsage, error) {
	return orgusage.MonthlyUsage{}, context.DeadlineExceeded
}

// gateSpec describes one org+team to seed for a launch-gate test. The
// monthly run/cost budget + org suspend live on the Org; concurrency,
// launch-rate and the team suspend live on the Team. For test
// simplicity the org id equals the team id (distinct collections, no
// clash) so the org-keyed usage assertions can reference the same id.
type gateSpec struct {
	id                string
	orgRunQuota       int
	orgCostCapUSD     float64
	orgStatus         identity.TeamStatus
	teamStatus        identity.TeamStatus
	maxConcurrentRuns int
	launchRatePerMin  int
}

func seedGate(t *testing.T, s *Server, spec gateSpec) context.Context {
	t.Helper()
	now := time.Now()
	org := identity.Org{
		ID:                spec.id,
		Name:              spec.id,
		Slug:              spec.id,
		Status:            spec.orgStatus,
		MonthlyRunQuota:   spec.orgRunQuota,
		MonthlyCostCapUSD: spec.orgCostCapUSD,
		CreatedAt:         now,
	}
	if _, err := s.authStore().CreateOrg(context.Background(), org); err != nil {
		t.Fatalf("seed org: %v", err)
	}
	team := identity.Team{
		ID:                spec.id,
		OrgID:             org.ID,
		Name:              spec.id,
		Slug:              spec.id,
		Status:            spec.teamStatus,
		MaxConcurrentRuns: spec.maxConcurrentRuns,
		LaunchRatePerMin:  spec.launchRatePerMin,
		CreatedAt:         now,
	}
	if _, err := s.authStore().CreateTeam(context.Background(), team); err != nil {
		t.Fatalf("seed team: %v", err)
	}
	return auth.WithIdentity(context.Background(), auth.Identity{UserID: "u1", TeamID: team.ID, OrgID: org.ID})
}

func TestGateLaunch_TeamlessDenied(t *testing.T) {
	// A signed-in cloud user with no team (the GitHub submitter tier) is
	// denied — they have no workspace to launch into.
	s := newOrgTestServer(t)
	ctx := auth.WithIdentity(context.Background(), auth.Identity{UserID: "submitter", TeamID: ""})
	_, d := s.gateLaunch(ctx)
	if d == nil || d.status != 403 || d.reason != denyNoWorkspace {
		t.Fatalf("denial = %+v, want 403 %s", d, denyNoWorkspace)
	}
}

func TestGateLaunch_SuperAdminTeamlessBypasses(t *testing.T) {
	// A super-admin (also teamless) bypasses the gate entirely.
	s := newOrgTestServer(t)
	ctx := auth.WithIdentity(context.Background(), auth.Identity{UserID: "root", IsSuperAdmin: true})
	if _, d := s.gateLaunch(ctx); d != nil {
		t.Fatalf("super-admin denied: %+v", d)
	}
}

func TestGateLaunch_Suspend(t *testing.T) {
	s := newOrgTestServer(t)
	ctx := seedGate(t, s, gateSpec{id: "t1", teamStatus: identity.TeamStatusSuspended})
	_, d := s.gateLaunch(ctx)
	if d == nil || d.status != 403 || d.reason != denyOrgSuspended {
		t.Fatalf("denial = %+v, want 403 %s", d, denyOrgSuspended)
	}
}

func TestGateLaunch_MonthlyRunQuota(t *testing.T) {
	s := newOrgTestServer(t)
	s.orgUsage = orgusage.NewMemoryCounter()
	ctx := seedGate(t, s, gateSpec{id: "t1", orgRunQuota: 2})
	for i := 0; i < 2; i++ {
		if _, d := s.gateLaunch(ctx); d != nil {
			t.Fatalf("launch #%d denied: %+v", i, d)
		}
	}
	_, d := s.gateLaunch(ctx)
	if d == nil || d.status != 402 || d.reason != denyMonthlyRunQuota {
		t.Fatalf("denial = %+v, want 402 %s", d, denyMonthlyRunQuota)
	}
	if d.resetAt.IsZero() || !d.resetAt.After(time.Now()) {
		t.Fatalf("resetAt = %v, want a future month boundary", d.resetAt)
	}
}

func TestGateLaunch_MetersWithoutQuota(t *testing.T) {
	s := newOrgTestServer(t)
	counter := orgusage.NewMemoryCounter()
	s.orgUsage = counter
	ctx := seedGate(t, s, gateSpec{id: "t1"})
	if _, d := s.gateLaunch(ctx); d != nil {
		t.Fatalf("unlimited launch denied: %+v", d)
	}
	u, _ := counter.Usage(context.Background(), "t1", time.Now().UTC())
	if u.Runs != 1 {
		t.Fatalf("Runs = %d, want 1 (metering must happen without a cap)", u.Runs)
	}
}

func TestGateLaunch_CostCap(t *testing.T) {
	s := newOrgTestServer(t)
	counter := orgusage.NewMemoryCounter()
	s.orgUsage = counter
	ctx := seedGate(t, s, gateSpec{id: "t1", orgCostCapUSD: 5})
	if _, d := s.gateLaunch(ctx); d != nil {
		t.Fatalf("under-cap launch denied: %+v", d)
	}
	if err := counter.AddSpend(context.Background(), "t1", time.Now().UTC(), 6.0, 0, 0); err != nil {
		t.Fatal(err)
	}
	_, d := s.gateLaunch(ctx)
	if d == nil || d.status != 402 || d.reason != denyMonthlyCostCap {
		t.Fatalf("denial = %+v, want 402 %s", d, denyMonthlyCostCap)
	}
}

func TestGateLaunch_ConcurrencyCap(t *testing.T) {
	s := newOrgTestServer(t)
	s.cfg.Store = fakeActiveStore{active: 3}
	ctx := seedGate(t, s, gateSpec{id: "t1", maxConcurrentRuns: 3})
	_, d := s.gateLaunch(ctx)
	if d == nil || d.status != 429 || d.reason != denyConcurrencyCap {
		t.Fatalf("denial = %+v, want 429 %s", d, denyConcurrencyCap)
	}
	if d.retryAfter <= 0 {
		t.Fatalf("retryAfter = %v, want > 0", d.retryAfter)
	}
	// Under the cap → allowed.
	s.cfg.Store = fakeActiveStore{active: 2}
	if _, d := s.gateLaunch(ctx); d != nil {
		t.Fatalf("under-cap launch denied: %+v", d)
	}
}

func TestGateLaunch_RateLimit(t *testing.T) {
	s := newOrgTestServer(t)
	s.authLimiter = newAuthRateLimiter()
	ctx := seedGate(t, s, gateSpec{id: "t1", launchRatePerMin: 1})
	if _, d := s.gateLaunch(ctx); d != nil {
		t.Fatalf("first launch denied: %+v", d)
	}
	_, d := s.gateLaunch(ctx)
	if d == nil || d.status != 429 || d.reason != denyLaunchRateLimited {
		t.Fatalf("denial = %+v, want 429 %s", d, denyLaunchRateLimited)
	}
}

func TestGateLaunch_PlatformDefaults(t *testing.T) {
	s := newOrgTestServer(t)
	s.orgUsage = orgusage.NewMemoryCounter()
	s.orgDefaults = OrgLimitDefaults{MonthlyRunQuota: 1}
	ctx := seedGate(t, s, gateSpec{id: "t1"}) // no per-org override
	if _, d := s.gateLaunch(ctx); d != nil {
		t.Fatalf("first launch denied: %+v", d)
	}
	_, d := s.gateLaunch(ctx)
	if d == nil || d.reason != denyMonthlyRunQuota {
		t.Fatalf("denial = %+v, want %s from the platform default", d, denyMonthlyRunQuota)
	}
	// A per-org override beats the platform default.
	s2 := newOrgTestServer(t)
	s2.orgUsage = orgusage.NewMemoryCounter()
	s2.orgDefaults = OrgLimitDefaults{MonthlyRunQuota: 1}
	ctx2 := seedGate(t, s2, gateSpec{id: "t2", orgRunQuota: 3})
	for i := 0; i < 3; i++ {
		if _, d := s2.gateLaunch(ctx2); d != nil {
			t.Fatalf("override launch #%d denied: %+v", i, d)
		}
	}
	if _, d := s2.gateLaunch(ctx2); d == nil {
		t.Fatal("4th launch allowed past the per-org override of 3")
	}
}

func TestGateLaunch_Bypasses(t *testing.T) {
	s := newOrgTestServer(t)
	s.orgUsage = orgusage.NewMemoryCounter()
	seedGate(t, s, gateSpec{id: "t1", orgStatus: identity.TeamStatusSuspended})

	// Super-admin bypasses everything.
	super := auth.WithIdentity(context.Background(), auth.Identity{UserID: "root", TeamID: "t1", IsSuperAdmin: true})
	if _, d := s.gateLaunch(super); d != nil {
		t.Fatalf("super-admin denied: %+v", d)
	}
	// A teamless non-admin is now DENIED (no workspace) — see
	// TestGateLaunch_TeamlessDenied. Local mode (no auth store) still bypasses
	// via the st == nil branch.
	// Missing team fails open.
	ghost := auth.WithIdentity(context.Background(), auth.Identity{UserID: "u1", TeamID: "ghost"})
	if _, d := s.gateLaunch(ghost); d != nil {
		t.Fatalf("ghost team denied: %+v", d)
	}
}

func TestGateLaunch_FailOpenOnCounterError(t *testing.T) {
	s := newOrgTestServer(t)
	s.orgUsage = erroringCounter{}
	ctx := seedGate(t, s, gateSpec{id: "t1", orgRunQuota: 1, orgCostCapUSD: 1})
	if _, d := s.gateLaunch(ctx); d != nil {
		t.Fatalf("counter error must fail open, got %+v", d)
	}
}

// TestGateLaunch_OrgBudgetSumsAcrossTeams is the core ADR-048 claim: the
// monthly run quota is org-level, so two teams in the same org draw down
// one shared budget. With org quota = 1, team A's launch succeeds and
// team B's (same org) is denied.
func TestGateLaunch_OrgBudgetSumsAcrossTeams(t *testing.T) {
	s := newOrgTestServer(t)
	s.orgUsage = orgusage.NewMemoryCounter()
	ctx := context.Background()
	if _, err := s.authStore().CreateOrg(ctx, identity.Org{ID: "o1", Name: "o1", Slug: "o1", MonthlyRunQuota: 1}); err != nil {
		t.Fatal(err)
	}
	for _, tid := range []string{"ta", "tb"} {
		if _, err := s.authStore().CreateTeam(ctx, identity.Team{ID: tid, OrgID: "o1", Name: tid, Slug: tid}); err != nil {
			t.Fatal(err)
		}
	}
	ctxA := auth.WithIdentity(ctx, auth.Identity{UserID: "u1", TeamID: "ta", OrgID: "o1"})
	ctxB := auth.WithIdentity(ctx, auth.Identity{UserID: "u2", TeamID: "tb", OrgID: "o1"})

	if _, d := s.gateLaunch(ctxA); d != nil {
		t.Fatalf("team A first launch denied: %+v", d)
	}
	// Team B shares the org budget → denied.
	_, d := s.gateLaunch(ctxB)
	if d == nil || d.reason != denyMonthlyRunQuota {
		t.Fatalf("team B should hit the shared org quota, got %+v", d)
	}
}

// TestGateLaunch_AdmissionRollback exercises the concurrent-duplicate
// undo path: a granted admission's rollback releases exactly the one
// metered run so an abandoned launch doesn't consume quota.
func TestGateLaunch_AdmissionRollback(t *testing.T) {
	s := newOrgTestServer(t)
	counter := orgusage.NewMemoryCounter()
	s.orgUsage = counter
	ctx := seedGate(t, s, gateSpec{id: "t1"})
	adm, d := s.gateLaunch(ctx)
	if d != nil {
		t.Fatalf("launch denied: %+v", d)
	}
	if adm == nil {
		t.Fatal("granted metered launch returned a nil admission")
	}
	u, _ := counter.Usage(context.Background(), "t1", time.Now().UTC())
	if u.Runs != 1 {
		t.Fatalf("Runs = %d, want 1 after admission", u.Runs)
	}
	adm.rollback(nil)
	u, _ = counter.Usage(context.Background(), "t1", time.Now().UTC())
	if u.Runs != 0 {
		t.Fatalf("Runs = %d, want 0 after rollback", u.Runs)
	}
	// A nil admission (fail-open / bypass) rolls back as a no-op.
	var none *launchAdmission
	none.rollback(nil)
}

func TestWriteLaunchDenial_Shape(t *testing.T) {
	s := newOrgTestServer(t)
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/runs", nil)
	s.writeLaunchDenial(w, r, &launchDenial{
		status:     402,
		reason:     denyMonthlyRunQuota,
		detail:     "monthly run quota (5) exhausted",
		resetAt:    time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
		retryAfter: 30 * time.Second,
	})
	if w.Code != 402 {
		t.Fatalf("status = %d, want 402", w.Code)
	}
	if got := w.Header().Get("Retry-After"); got != "31" {
		t.Fatalf("Retry-After = %q, want 31", got)
	}
	body := w.Body.String()
	for _, want := range []string{denyMonthlyRunQuota, "2026-07-01T00:00:00Z", "detail"} {
		if !strings.Contains(body, want) {
			t.Fatalf("body %q missing %q", body, want)
		}
	}
}

// TestGateLaunch_CostCapAcrossTeams is the end-to-end regression for the
// multi-team cost-cap bug: spend recorded on the ORG key (as the runner
// now does via RunMessage.OrgID) must trip the cap for a launch from any
// team of that org.
func TestGateLaunch_CostCapAcrossTeams(t *testing.T) {
	s := newOrgTestServer(t)
	counter := orgusage.NewMemoryCounter()
	s.orgUsage = counter
	ctx := context.Background()
	if _, err := s.authStore().CreateOrg(ctx, identity.Org{ID: "o1", Name: "o1", Slug: "o1", MonthlyCostCapUSD: 5}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.authStore().CreateTeam(ctx, identity.Team{ID: "ta", OrgID: "o1", Name: "ta", Slug: "ta"}); err != nil {
		t.Fatal(err)
	}
	ctxA := auth.WithIdentity(ctx, auth.Identity{UserID: "u1", TeamID: "ta", OrgID: "o1"})
	if _, d := s.gateLaunch(ctxA); d != nil {
		t.Fatalf("under-cap launch denied: %+v", d)
	}
	// Runner-side spend lands on the ORG key (RunMessage.OrgID).
	if err := counter.AddSpend(ctx, "o1", time.Now().UTC(), 6.0, 0, 0); err != nil {
		t.Fatal(err)
	}
	_, d := s.gateLaunch(ctxA)
	if d == nil || d.reason != denyMonthlyCostCap {
		t.Fatalf("denial = %+v, want %s (org-keyed spend must trip the org cap)", d, denyMonthlyCostCap)
	}
}
