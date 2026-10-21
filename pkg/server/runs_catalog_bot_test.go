package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/botsource"
	"github.com/SocialGouv/iterion/pkg/budgetfloor"
	"github.com/SocialGouv/iterion/pkg/identity"
	"github.com/SocialGouv/iterion/pkg/orgusage"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
)

// TestInferCatalogBotID pins the path→bot-id inference that lets a cloud
// launch/resume reference a catalog bundle without uploading its bytes. The
// security-relevant cases are the ones that must return "" so the strict
// cloud "source required" gate still fires for an arbitrary operator path.
func TestInferCatalogBotID(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		// Catalog-shaped paths → the bundle dir name.
		{"bots/whats-next/main.bot", "whats-next"},
		{"/opt/iterion/bots/whats-next/main.bot", "whats-next"},
		{"examples/foo/main.bot", "foo"},
		{"./bots/review-pr/main.bot", "review-pr"},
		{"whats-next/main.bot", "whats-next"},
		// Bare names / loose basenames → candidate id (confirmed later
		// against the real catalog by catalogBotSource).
		{"whats-next", "whats-next"},
		{"hello.bot", "hello"},
		// Arbitrary absolute/workspace paths with no bots|examples segment
		// → "" so cloud still demands inline source (the security gate).
		{"/home/operator/secret/plan.bot", ""},
		{"/etc/passwd", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := inferCatalogBotID(c.in); got != c.want {
			t.Errorf("inferCatalogBotID(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func newLaunchIdentityTestServer(t *testing.T, mode string, spec gateSpec) (*Server, *tierPublisher, context.Context) {
	t.Helper()
	pub := &tierPublisher{}
	s := newOrgTestServer(t)
	seedGate(t, s, spec)
	s.cfg.Mode = mode
	rs, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s.cfg.Store = fakeActiveStore{RunStore: rs, active: 1}
	s.runs = newTestRunviewService(t, "", runview.WithStore(rs), runview.WithLaunchPublisher(pub))
	s.orgUsage = orgusage.NewMemoryCounter()
	pub.onLaunch = func(runID string) { finishRunAs(t, rs, runID, store.RunStatusFinished) }
	ctx := auth.WithIdentity(context.Background(), auth.Identity{UserID: "u1", TeamID: "t1", OrgID: "t1", Role: identity.RoleMember})
	return s, pub, ctx
}

func launchIdentityRequest(t *testing.T, s *Server, ctx context.Context, fields map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/runs", bytes.NewReader(body)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.handleLaunchRun(rec, req)
	return rec
}

func TestHandleLaunchRun_InfersCatalogBotIDForAdmission(t *testing.T) {
	for _, tc := range []struct {
		name     string
		filePath string
	}{
		{name: "catalog path", filePath: "bots/probe/main.bot"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pub := &tierPublisher{}
			s := newOrgTestServer(t)
			seedGate(t, s, gateSpec{id: "t1", maxConcurrentRuns: 2})
			catalog := t.TempDir()
			if err := os.MkdirAll(filepath.Join(catalog, "probe"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(catalog, "probe", "main.bot"), []byte(boardGateProbeBot), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(catalog, "probe", "manifest.yaml"), []byte("name: probe\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			s.cfg.Bots.Paths = []string{catalog}
			s.cfg.Mode = "cloud"
			rs, err := store.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			s.cfg.Store = fakeActiveStore{RunStore: rs, active: 1}
			s.runs = newTestRunviewService(t, "", runview.WithStore(rs), runview.WithLaunchPublisher(pub))
			s.orgUsage = orgusage.NewMemoryCounter()
			pub.onLaunch = func(runID string) { finishRunAs(t, rs, runID, store.RunStatusFinished) }
			withFloor(t, s, budgetfloor.Policy{Reservations: []budgetfloor.Reservation{
				{BotID: "probe", Reserve: budgetfloor.Reserve{ConcurrentRuns: 1}},
			}})
			ctx := auth.WithIdentity(context.Background(), auth.Identity{UserID: "u1", TeamID: "t1", OrgID: "t1", Role: identity.RoleMember})
			body, err := json.Marshal(map[string]string{"file_path": tc.filePath})
			if err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest(http.MethodPost, "/api/runs", bytes.NewReader(body)).WithContext(ctx)
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			s.handleLaunchRun(rec, req)
			if rec.Code != http.StatusAccepted {
				t.Fatalf("status = %d, want 202 for the reserved bot named by file_path: %s", rec.Code, rec.Body.String())
			}
			if got := pub.only(t).BotID; got != "probe" {
				t.Errorf("LaunchSpec BotID = %q, want the canonical identity used at admission", got)
			}
		})
	}
}

func TestHandleLaunchRun_InlineSourceDoesNotInheritCatalogReservation(t *testing.T) {
	pub := &countingPublisher{}
	s, _ := newGatedBoardServer(t, gateSpec{id: "t1", orgCostCapUSD: 30}, pub)
	s.cfg.Mode = "cloud"
	s.orgUsage = orgusage.NewMemoryCounter()
	withFloor(t, s, budgetfloor.Policy{Reservations: []budgetfloor.Reservation{
		{BotID: "probe", Reserve: budgetfloor.Reserve{MonthlyUSD: 30}},
	}})
	ctx := auth.WithIdentity(context.Background(), auth.Identity{UserID: "u1", TeamID: "t1", OrgID: "t1", Role: identity.RoleMember})
	body, err := json.Marshal(map[string]string{"file_path": "bots/probe/main.bot", "source": boardGateProbeBot})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/runs", bytes.NewReader(body)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.handleLaunchRun(rec, req)
	if rec.Code != http.StatusPaymentRequired {
		t.Fatalf("status = %d, want 402: an inline source must not inherit the reservation named by its display path: %s", rec.Code, rec.Body.String())
	}
	if got := pub.count(); got != 0 {
		t.Errorf("publisher calls = %d, want 0 after the monthly refusal", got)
	}
}

func TestHandleLaunchRun_ExplicitBotIDPublishesTheResolvedName(t *testing.T) {
	for _, tc := range []struct {
		name     string
		reserved bool
	}{
		{name: "with an exact reservation", reserved: true},
		{name: "without a reservation"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, pub, ctx := newLaunchIdentityTestServer(t, "cloud", gateSpec{id: "t1", maxConcurrentRuns: 2})
			catalog := t.TempDir()
			if err := os.MkdirAll(filepath.Join(catalog, "feature-dev"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(catalog, "feature-dev", "main.bot"), []byte(boardGateProbeBot), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(catalog, "feature-dev", "manifest.yaml"), []byte("name: feature-dev\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			s.cfg.Bots.Paths = []string{catalog}
			if tc.reserved {
				withFloor(t, s, budgetfloor.Policy{Reservations: []budgetfloor.Reservation{
					{BotID: "feature_dev", Reserve: budgetfloor.Reserve{ConcurrentRuns: 1}},
				}})
			}
			rec := launchIdentityRequest(t, s, ctx, map[string]string{"bot_id": "feature_dev"})
			if rec.Code != http.StatusAccepted {
				t.Fatalf("status = %d, want 202 for the exact explicit bot_id: %s", rec.Code, rec.Body.String())
			}
			// The typed string is a label: the run publishes the resolution's
			// canonical answer, not the label (#2613).
			if got := pub.only(t).BotID; got != "feature-dev" {
				t.Errorf("LaunchSpec BotID = %q, want the resolved canonical name", got)
			}
		})
	}
}

func TestHandleLaunchRun_ExplicitBotIDBesideAnInlineSourceNamesNoBot(t *testing.T) {
	s, pub, ctx := newLaunchIdentityTestServer(t, "cloud", gateSpec{id: "t1"})
	rec := launchIdentityRequest(t, s, ctx, map[string]string{
		"bot_id": "feature_dev", "file_path": "inline.bot", "source": boardGateProbeBot,
	})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202: %s", rec.Code, rec.Body.String())
	}
	// A source launch names no bot: the gate refuses to judge inline work
	// under a named band, and nothing stamps an identity on the run.
	if got := pub.only(t).BotID; got != "" {
		t.Errorf("LaunchSpec BotID = %q, want no identity beside an inline source", got)
	}
}

func TestHandleLaunchRun_LocalFileDoesNotAcquireInferredBotID(t *testing.T) {
	s, pub, ctx := newLaunchIdentityTestServer(t, "", gateSpec{id: "t1"})
	s.cfg.WorkDir = t.TempDir()
	if err := os.WriteFile(filepath.Join(s.cfg.WorkDir, "plain.bot"), []byte(boardGateProbeBot), 0o600); err != nil {
		t.Fatal(err)
	}
	rec := launchIdentityRequest(t, s, ctx, map[string]string{"file_path": "plain.bot"})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202: %s", rec.Code, rec.Body.String())
	}
	if got := pub.only(t).BotID; got != "" {
		t.Errorf("LaunchSpec BotID = %q, want no identity for an unresolved local file", got)
	}
}

func newStoredLaunchIdentityTestServer(t *testing.T, reserved string) (*Server, *tierPublisher, context.Context) {
	t.Helper()
	s, pub, ctx := newLaunchIdentityTestServer(t, "cloud", gateSpec{id: "t1", maxConcurrentRuns: 2})
	s.botSources = botsource.NewMemoryStore()
	if _, err := s.botSources.Create(store.WithTenant(context.Background(), "t1"), botsource.BotSource{
		TenantID: "t1", Slug: "feature_dev",
		Files: map[string]string{botsource.MainBotFile: boardGateProbeBot},
	}); err != nil {
		t.Fatal(err)
	}
	if reserved != "" {
		withFloor(t, s, budgetfloor.Policy{Reservations: []budgetfloor.Reservation{
			{BotID: reserved, Reserve: budgetfloor.Reserve{ConcurrentRuns: 1}},
		}})
	}
	return s, pub, ctx
}

func TestHandleLaunchRun_InferredStoredSlugCannotSpendAnotherWorkloadReservation(t *testing.T) {
	s, pub, ctx := newStoredLaunchIdentityTestServer(t, "feature-dev")
	rec := launchIdentityRequest(t, s, ctx, map[string]string{"file_path": "bots/feature_dev/main.bot"})
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429: the feature-dev reservation belongs to another workload: %s", rec.Code, rec.Body.String())
	}
	if got := len(pub.specs); got != 0 {
		t.Errorf("publisher calls = %d, want 0 after the reservation refusal", got)
	}
}

func TestHandleLaunchRun_InferredStoredSlugKeepsBaseBehavior(t *testing.T) {
	t.Run("exact reservation keeps ordinary base admission", func(t *testing.T) {
		s, pub, ctx := newStoredLaunchIdentityTestServer(t, "feature_dev")
		rec := launchIdentityRequest(t, s, ctx, map[string]string{"file_path": "bots/feature_dev/main.bot"})
		if rec.Code != http.StatusTooManyRequests {
			t.Fatalf("status = %d, want 429: a stored bot without explicit bot_id keeps its base ordinary admission: %s", rec.Code, rec.Body.String())
		}
		if got := len(pub.specs); got != 0 {
			t.Errorf("publisher calls = %d, want 0 after the reservation refusal", got)
		}
	})

	t.Run("unresolved admission preserves the resolved identity", func(t *testing.T) {
		s, pub, ctx := newStoredLaunchIdentityTestServer(t, "")
		rec := launchIdentityRequest(t, s, ctx, map[string]string{"file_path": "bots/feature_dev/main.bot"})
		if rec.Code != http.StatusAccepted {
			t.Fatalf("status = %d, want 202: %s", rec.Code, rec.Body.String())
		}
		if got := pub.only(t).BotID; got != "feature_dev" {
			t.Errorf("LaunchSpec BotID = %q, want the resolved stored identity preserved", got)
		}
	})
}

type reconcileAccountingCounter struct {
	*orgusage.MemoryCounter
	allowCalls   int
	releaseCalls int
	failNext     bool
}

func (c *reconcileAccountingCounter) AllowRun(ctx context.Context, subject orgusage.Subject, when time.Time, maxRuns int, maxCostMillis int64) (orgusage.DenyReason, error) {
	c.allowCalls++
	if c.failNext {
		c.failNext = false
		return orgusage.DenyNone, context.DeadlineExceeded
	}
	return c.MemoryCounter.AllowRun(ctx, subject, when, maxRuns, maxCostMillis)
}

func (c *reconcileAccountingCounter) ReleaseRun(ctx context.Context, subject orgusage.Subject, when time.Time) error {
	c.releaseCalls++
	return c.MemoryCounter.ReleaseRun(ctx, subject, when)
}

type reconcileRateLimiter struct {
	*authRateLimiter
	allowCalls  int
	refundCalls int
}

func (r *reconcileRateLimiter) allow(key string, cfg authBucketCfg) (bool, time.Duration) {
	r.allowCalls++
	return r.authRateLimiter.allow(key, cfg)
}

func (r *reconcileRateLimiter) refund(key string, cfg authBucketCfg) {
	r.refundCalls++
	r.authRateLimiter.refund(key, cfg)
}

func newReconcileCanonicalAliasServer(t *testing.T, active int) (*Server, *tierPublisher, context.Context, *reconcileAccountingCounter, *reconcileRateLimiter) {
	t.Helper()
	s, pub, ctx := newStoredLaunchIdentityTestServer(t, "feature-dev")
	st := s.cfg.Store.(fakeActiveStore)
	st.active = active
	s.cfg.Store = st
	s.orgDefaults = OrgLimitDefaults{MonthlyRunQuota: 1, LaunchRatePerMin: 1}
	counter := &reconcileAccountingCounter{MemoryCounter: orgusage.NewMemoryCounter()}
	s.orgUsage = counter
	limiter := &reconcileRateLimiter{authRateLimiter: newAuthRateLimiter()}
	s.authLimiter = limiter
	return s, pub, ctx, counter, limiter
}

func assertReconcileAccounting(t *testing.T, counter *reconcileAccountingCounter, limiter *reconcileRateLimiter, wantAllow, wantRelease, wantRateAllow, wantRateRefund, wantRuns int) {
	t.Helper()
	if counter.allowCalls != wantAllow || counter.releaseCalls != wantRelease {
		t.Errorf("monthly calls = AllowRun %d / ReleaseRun %d, want %d / %d", counter.allowCalls, counter.releaseCalls, wantAllow, wantRelease)
	}
	if limiter.allowCalls != wantRateAllow || limiter.refundCalls != wantRateRefund {
		t.Errorf("rate calls = allow %d / refund %d, want %d / %d", limiter.allowCalls, limiter.refundCalls, wantRateAllow, wantRateRefund)
	}
	u, err := counter.Usage(context.Background(), orgusage.OrgSubject("t1"), time.Now().UTC())
	if err != nil {
		t.Fatalf("usage: %v", err)
	}
	if u.Runs != wantRuns {
		t.Errorf("monthly runs = %d, want %d", u.Runs, wantRuns)
	}
}

func TestHandleLaunchRun_ReconcilesCanonicalInferenceWithResolvedIdentity(t *testing.T) {
	t.Run("ordinary capacity exhausted refuses and refunds the inferred admission", func(t *testing.T) {
		s, pub, ctx, counter, limiter := newReconcileCanonicalAliasServer(t, 1)
		rec := launchIdentityRequest(t, s, ctx, map[string]string{"file_path": "bots/feature-dev/main.bot"})
		if rec.Code != http.StatusTooManyRequests {
			t.Fatalf("status = %d, want 429 from the resolved identity's ordinary admission: %s", rec.Code, rec.Body.String())
		}
		if got := len(pub.specs); got != 0 {
			t.Errorf("publisher calls = %d, want 0", got)
		}
		assertReconcileAccounting(t, counter, limiter, 1, 1, 1, 1, 0)
	})

	t.Run("ordinary capacity available launches once under the resolved identity", func(t *testing.T) {
		s, pub, ctx, counter, limiter := newReconcileCanonicalAliasServer(t, 0)
		rec := launchIdentityRequest(t, s, ctx, map[string]string{"file_path": "bots/feature-dev/main.bot"})
		if rec.Code != http.StatusAccepted {
			t.Fatalf("status = %d, want 202: %s", rec.Code, rec.Body.String())
		}
		if got := pub.only(t).BotID; got != "feature_dev" {
			t.Errorf("LaunchSpec BotID = %q, want the resolved identity", got)
		}
		assertReconcileAccounting(t, counter, limiter, 2, 1, 2, 1, 1)
	})

	t.Run("a later refusal refunds the replacement admission", func(t *testing.T) {
		s, pub, ctx, counter, limiter := newReconcileCanonicalAliasServer(t, 0)
		rec := launchIdentityRequest(t, s, ctx, map[string]string{"file_path": "bots/feature-dev/main.bot", "timeout": "invalid"})
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
		}
		if got := len(pub.specs); got != 0 {
			t.Errorf("publisher calls = %d, want 0", got)
		}
		assertReconcileAccounting(t, counter, limiter, 2, 2, 2, 2, 0)
	})

	t.Run("a fail-open first admission still refunds its rate token", func(t *testing.T) {
		s, pub, ctx, counter, limiter := newReconcileCanonicalAliasServer(t, 0)
		counter.failNext = true
		rec := launchIdentityRequest(t, s, ctx, map[string]string{"file_path": "bots/feature-dev/main.bot"})
		if rec.Code != http.StatusAccepted {
			t.Fatalf("status = %d, want 202: %s", rec.Code, rec.Body.String())
		}
		if got := pub.only(t).BotID; got != "feature_dev" {
			t.Errorf("LaunchSpec BotID = %q, want the resolved identity", got)
		}
		assertReconcileAccounting(t, counter, limiter, 2, 0, 2, 1, 1)
	})
}
