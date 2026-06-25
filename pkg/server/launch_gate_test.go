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
func (erroringCounter) Usage(context.Context, string, time.Time) (orgusage.MonthlyUsage, error) {
	return orgusage.MonthlyUsage{}, context.DeadlineExceeded
}

func seedGateTeam(t *testing.T, s *Server, team identity.Team) context.Context {
	t.Helper()
	if team.Name == "" {
		team.Name = team.ID
	}
	if team.Slug == "" {
		team.Slug = team.ID
	}
	team.CreatedAt = time.Now()
	if _, err := s.authStore().CreateTeam(context.Background(), team); err != nil {
		t.Fatalf("seed team: %v", err)
	}
	return auth.WithIdentity(context.Background(), auth.Identity{UserID: "u1", TeamID: team.ID})
}

func TestGateLaunch_TeamlessDenied(t *testing.T) {
	// A signed-in cloud user with no team (the GitHub submitter tier) is
	// denied — they have no workspace to launch into.
	s := newOrgTestServer(t)
	ctx := auth.WithIdentity(context.Background(), auth.Identity{UserID: "submitter", TeamID: ""})
	d := s.gateLaunch(ctx)
	if d == nil || d.status != 403 || d.reason != denyNoWorkspace {
		t.Fatalf("denial = %+v, want 403 %s", d, denyNoWorkspace)
	}
}

func TestGateLaunch_SuperAdminTeamlessBypasses(t *testing.T) {
	// A super-admin (also teamless) bypasses the gate entirely.
	s := newOrgTestServer(t)
	ctx := auth.WithIdentity(context.Background(), auth.Identity{UserID: "root", IsSuperAdmin: true})
	if d := s.gateLaunch(ctx); d != nil {
		t.Fatalf("super-admin denied: %+v", d)
	}
}

func TestGateLaunch_Suspend(t *testing.T) {
	s := newOrgTestServer(t)
	ctx := seedGateTeam(t, s, identity.Team{ID: "t1", Status: identity.TeamStatusSuspended})
	d := s.gateLaunch(ctx)
	if d == nil || d.status != 403 || d.reason != denyOrgSuspended {
		t.Fatalf("denial = %+v, want 403 %s", d, denyOrgSuspended)
	}
}

func TestGateLaunch_MonthlyRunQuota(t *testing.T) {
	s := newOrgTestServer(t)
	s.orgUsage = orgusage.NewMemoryCounter()
	ctx := seedGateTeam(t, s, identity.Team{ID: "t1", MonthlyRunQuota: 2})
	for i := 0; i < 2; i++ {
		if d := s.gateLaunch(ctx); d != nil {
			t.Fatalf("launch #%d denied: %+v", i, d)
		}
	}
	d := s.gateLaunch(ctx)
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
	ctx := seedGateTeam(t, s, identity.Team{ID: "t1"})
	if d := s.gateLaunch(ctx); d != nil {
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
	ctx := seedGateTeam(t, s, identity.Team{ID: "t1", MonthlyCostCapUSD: 5})
	if d := s.gateLaunch(ctx); d != nil {
		t.Fatalf("under-cap launch denied: %+v", d)
	}
	if err := counter.AddSpend(context.Background(), "t1", time.Now().UTC(), 6.0, 0, 0); err != nil {
		t.Fatal(err)
	}
	d := s.gateLaunch(ctx)
	if d == nil || d.status != 402 || d.reason != denyMonthlyCostCap {
		t.Fatalf("denial = %+v, want 402 %s", d, denyMonthlyCostCap)
	}
}

func TestGateLaunch_ConcurrencyCap(t *testing.T) {
	s := newOrgTestServer(t)
	s.cfg.Store = fakeActiveStore{active: 3}
	ctx := seedGateTeam(t, s, identity.Team{ID: "t1", MaxConcurrentRuns: 3})
	d := s.gateLaunch(ctx)
	if d == nil || d.status != 429 || d.reason != denyConcurrencyCap {
		t.Fatalf("denial = %+v, want 429 %s", d, denyConcurrencyCap)
	}
	if d.retryAfter <= 0 {
		t.Fatalf("retryAfter = %v, want > 0", d.retryAfter)
	}
	// Under the cap → allowed.
	s.cfg.Store = fakeActiveStore{active: 2}
	if d := s.gateLaunch(ctx); d != nil {
		t.Fatalf("under-cap launch denied: %+v", d)
	}
}

func TestGateLaunch_RateLimit(t *testing.T) {
	s := newOrgTestServer(t)
	s.authLimiter = newAuthRateLimiter()
	ctx := seedGateTeam(t, s, identity.Team{ID: "t1", LaunchRatePerMin: 1})
	if d := s.gateLaunch(ctx); d != nil {
		t.Fatalf("first launch denied: %+v", d)
	}
	d := s.gateLaunch(ctx)
	if d == nil || d.status != 429 || d.reason != denyLaunchRateLimited {
		t.Fatalf("denial = %+v, want 429 %s", d, denyLaunchRateLimited)
	}
}

func TestGateLaunch_PlatformDefaults(t *testing.T) {
	s := newOrgTestServer(t)
	s.orgUsage = orgusage.NewMemoryCounter()
	s.orgDefaults = OrgLimitDefaults{MonthlyRunQuota: 1}
	ctx := seedGateTeam(t, s, identity.Team{ID: "t1"}) // no per-org override
	if d := s.gateLaunch(ctx); d != nil {
		t.Fatalf("first launch denied: %+v", d)
	}
	d := s.gateLaunch(ctx)
	if d == nil || d.reason != denyMonthlyRunQuota {
		t.Fatalf("denial = %+v, want %s from the platform default", d, denyMonthlyRunQuota)
	}
	// A per-org override beats the platform default.
	s2 := newOrgTestServer(t)
	s2.orgUsage = orgusage.NewMemoryCounter()
	s2.orgDefaults = OrgLimitDefaults{MonthlyRunQuota: 1}
	ctx2 := seedGateTeam(t, s2, identity.Team{ID: "t2", MonthlyRunQuota: 3})
	for i := 0; i < 3; i++ {
		if d := s2.gateLaunch(ctx2); d != nil {
			t.Fatalf("override launch #%d denied: %+v", i, d)
		}
	}
	if d := s2.gateLaunch(ctx2); d == nil {
		t.Fatal("4th launch allowed past the per-org override of 3")
	}
}

func TestGateLaunch_Bypasses(t *testing.T) {
	s := newOrgTestServer(t)
	s.orgUsage = orgusage.NewMemoryCounter()
	seedGateTeam(t, s, identity.Team{ID: "t1", Status: identity.TeamStatusSuspended, MonthlyRunQuota: 0})

	// Super-admin bypasses everything.
	super := auth.WithIdentity(context.Background(), auth.Identity{UserID: "root", TeamID: "t1", IsSuperAdmin: true})
	if d := s.gateLaunch(super); d != nil {
		t.Fatalf("super-admin denied: %+v", d)
	}
	// A teamless non-admin is now DENIED (no workspace) — see
	// TestGateLaunch_TeamlessDenied. Local mode (no auth store) still bypasses
	// via the st == nil branch.
	// Missing team fails open.
	ghost := auth.WithIdentity(context.Background(), auth.Identity{UserID: "u1", TeamID: "ghost"})
	if d := s.gateLaunch(ghost); d != nil {
		t.Fatalf("ghost team denied: %+v", d)
	}
}

func TestGateLaunch_FailOpenOnCounterError(t *testing.T) {
	s := newOrgTestServer(t)
	s.orgUsage = erroringCounter{}
	ctx := seedGateTeam(t, s, identity.Team{ID: "t1", MonthlyRunQuota: 1, MonthlyCostCapUSD: 1})
	if d := s.gateLaunch(ctx); d != nil {
		t.Fatalf("counter error must fail open, got %+v", d)
	}
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
