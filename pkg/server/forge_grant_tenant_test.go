package server

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/audit"
	"github.com/SocialGouv/iterion/pkg/forge"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/webhooks"
)

// waitForTenantAudit polls a tenant's audit trail for an action — the inserts
// are detached (goSafe), so they land shortly after the refusal.
func waitForTenantAudit(t *testing.T, st audit.Store, tenantID, action string) []audit.Event {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		evs, err := st.ListByTenant(context.Background(), tenantID, audit.Page{})
		if err != nil {
			t.Fatalf("list %s audit: %v", tenantID, err)
		}
		var hits []audit.Event
		for _, e := range evs {
			if e.Action == action {
				hits = append(hits, e)
			}
		}
		if len(hits) > 0 {
			return hits
		}
		if time.Now().After(deadline) {
			t.Fatalf("no %s audit row on tenant %q (got %d rows)", action, tenantID, len(evs))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A publish grant is minted for ONE tenant, at that tenant's own launch. The
// token is a launch VAR, and injectForgePublishVars honours a caller-pinned
// one — so a run of tenant B can end up carrying a grant minted for tenant A,
// and every reader that acts on a grant would then speak through tenant A's
// connection, under tenant A's forge identity: a comment on tenant A's pull
// request, a REQUIRED commit status on it, and — on the auto-fix lane — a
// code-pushing bot launched into tenant A on tenant A's money.
//
// The rule is one comparison at every reader that holds a run: the grant must
// belong to the run's own tenant.
func TestForgeGrant_ForeignTenantRefusedByTheGateReaders(t *testing.T) {
	// gateReconcileFixture mints the grant for team1 on conn1/team1; the run
	// below claims to belong to someone else.
	const foreign = "team-b"

	build := func(t *testing.T, runTenant string) (*Server, string, *listingGateClient, *stubCommenter) {
		t.Helper()
		gc := &listingGateClient{fakeGateClient: fakeGateClient{headSHA: "deadbeef"}}
		s, runID := gateReconcileFixture(t, gatingInputs(), gc)
		s.auditStore = audit.NewMemoryStore()
		c := &stubCommenter{}
		s.forgeIssueCommenterFor = func(context.Context, forge.Connection) (forgeIssueCommenter, error) { return c, nil }
		run, err := s.cfg.Store.LoadRun(context.Background(), runID)
		if err != nil {
			t.Fatal(err)
		}
		run.TenantID = runTenant
		run.Status = store.RunStatusFailedResumable
		run.FailureCode = store.FailureDLQParked
		at := time.Now().UTC().Add(time.Hour)
		run.RetryState = &store.RunRetryState{RetryAfter: &at, Reason: "usage_window"}
		run.Error = "rate_limited: You've hit your weekly limit"
		if err := s.cfg.Store.SaveRun(context.Background(), run); err != nil {
			t.Fatal(err)
		}
		return s, runID, gc, c
	}
	loadRun := func(t *testing.T, s *Server, runID string) *store.Run {
		t.Helper()
		run, err := s.cfg.Store.LoadRun(context.Background(), runID)
		if err != nil {
			t.Fatal(err)
		}
		return run
	}

	t.Run("the DLQ notice posts nothing", func(t *testing.T) {
		s, runID, _, c := build(t, foreign)
		s.noticeGateDLQParked(context.Background(), loadRun(t, s, runID))
		if len(c.bodies) != 0 {
			t.Fatalf("commented on another tenant's pull request under its own identity: %v", c.bodies)
		}
		if rows := waitForTenantAudit(t, s.auditStore, foreign, auditActionGrantTenantMismatch); len(rows) != 1 {
			t.Fatalf("want exactly one audit row, got %d", len(rows))
		}
	})

	t.Run("the pause notice posts nothing", func(t *testing.T) {
		s, runID, _, c := build(t, foreign)
		s.noticeGatePausedForRetry(context.Background(), loadRun(t, s, runID))
		if len(c.bodies) != 0 {
			t.Fatalf("commented on another tenant's pull request under its own identity: %v", c.bodies)
		}
		waitForTenantAudit(t, s.auditStore, foreign, auditActionGrantTenantMismatch)
	})

	t.Run("the reconciler writes no commit status", func(t *testing.T) {
		s, runID, gc, _ := build(t, foreign)
		run := loadRun(t, s, runID)
		run.FailureCode, run.RetryState = "", nil
		run.Status = store.RunStatusFailed
		if err := s.cfg.Store.SaveRun(context.Background(), run); err != nil {
			t.Fatal(err)
		}
		if err := s.reconcileGateForRunID(context.Background(), runID, gateTriggerEvent); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		if gc.setCalls != 0 {
			t.Fatalf("wrote %d commit statuses on another tenant's required check (last: %q %q)",
				gc.setCalls, gc.last.State, gc.last.Description)
		}
		waitForTenantAudit(t, s.auditStore, foreign, auditActionGrantTenantMismatch)
	})

	// The audit row names the two tenants and the surface, and NEVER the
	// token — an audit trail that leaks the credential is a second incident.
	t.Run("the audit row names the tenants, never the token", func(t *testing.T) {
		s, runID, _, _ := build(t, foreign)
		s.noticeGateDLQParked(context.Background(), loadRun(t, s, runID))
		ev := waitForTenantAudit(t, s.auditStore, foreign, auditActionGrantTenantMismatch)[0]
		if ev.TenantID != foreign || ev.TargetID != runID {
			t.Fatalf("row = tenant %q target %q, want the RUN's tenant and id", ev.TenantID, ev.TargetID)
		}
		if ev.Meta["grant_tenant"] != "team1" {
			t.Fatalf("meta = %v, want the grant's tenant named", ev.Meta)
		}
		for k, v := range ev.Meta {
			if s, ok := v.(string); ok && s == "tok-gate" {
				t.Fatalf("the audit row carries the token in %q", k)
			}
		}
	})

	// The run's OWN grant still works — the rule refuses a foreign grant, not
	// every grant.
	t.Run("a grant of the run's own tenant still posts", func(t *testing.T) {
		s, runID, _, c := build(t, "team1")
		s.noticeGateDLQParked(context.Background(), loadRun(t, s, runID))
		if len(c.bodies) != 1 {
			t.Fatalf("the run's own grant must still post, got %d comments", len(c.bodies))
		}
	})

	// A filesystem store never stamps a tenant on a run (only the Mongo twin
	// does), so a self-hosted single-tenant deployment states none — and a
	// deployment with one tenant has no second tenant to protect. Refusing
	// there would silence the gate on every such install.
	t.Run("a run that states no tenant is not refused", func(t *testing.T) {
		s, runID, _, c := build(t, "")
		s.noticeGateDLQParked(context.Background(), loadRun(t, s, runID))
		if len(c.bodies) != 1 {
			t.Fatalf("a tenant-less run must keep working, got %d comments", len(c.bodies))
		}
	})
}

// The auto-fix lane has the widest blast radius of the three readers: it
// launches a code-pushing bot INTO the grant's team, on that team's budget and
// against that team's repository. A run of another tenant carrying the grant
// must not reach it.
func TestForgeGrant_ForeignTenantRefusedByTheAutofixLane(t *testing.T) {
	const (
		team   = "t1"
		repo   = "acme/widgets"
		prURL  = "https://github.com/acme/widgets/pull/7"
		head   = "cafe1234cafe1234cafe1234cafe1234cafe1234"
		gateNm = "iterion/review"
	)
	build := func(t *testing.T, runTenant string) (*Server, string, *int) {
		t.Helper()
		s := newWebhookTestServer(t)
		s.cfg.WorkDir = writeConsumerBotFixture(t, "fixer-bot", "prior_review")
		s.auditStore = audit.NewMemoryStore()
		rs, err := store.New(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		s.cfg.Store = rs

		conns := forge.NewMemoryConnectionStore()
		if err := conns.Create(context.Background(), forge.Connection{
			ID: "c1", TenantID: team, Provider: forge.ProviderGitHub,
		}); err != nil {
			t.Fatal(err)
		}
		s.forgeConnections = conns
		s.forgePublishTokens = NewForgePublishTokenRegistry()
		registerPublishToken(t, s, "run-token", ForgePublishGrant{TeamID: team, ConnectionID: "c1", Repo: repo})

		ints := forge.NewMemoryRepoIntegrationStore()
		if err := ints.Create(context.Background(), forge.RepoIntegration{
			ID: "i1", TenantID: team, ConnectionID: "c1", RepoFullName: repo,
			BotIDs: []string{"fixer-bot"}, WebhookID: "w1", AutoFixOnGateFailure: true,
			LaunchVars: map[string]string{gateContextVar: gateNm},
		}); err != nil {
			t.Fatal(err)
		}
		s.forgeIntegrations = ints
		if err := s.webhookConfigs.Create(context.Background(), webhooks.Config{
			ID: "w1", TenantID: team, BotIDs: []string{"fixer-bot"},
		}); err != nil {
			t.Fatal(err)
		}
		s.forgeGateClientFor = func(context.Context, forge.Connection) (forgeGateClient, error) {
			return stubGateClient{head: head, state: forge.CommitStateFailure, ctxName: gateNm}, nil
		}
		launched := 0
		s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
			launched++
			return "run-fixer", nil
		}

		id, err := store.GenerateRunID()
		if err != nil {
			t.Fatal(err)
		}
		run, err := rs.CreateRun(context.Background(), id, "reviewer-bot", map[string]any{
			"pr_url": prURL, "gate_context": gateNm, "head_sha": head,
			forgePublishVarToken: "run-token",
		})
		if err != nil {
			t.Fatal(err)
		}
		run.BotID = "reviewer-bot"
		run.Status = store.RunStatusFinished
		run.TenantID = runTenant
		if err := rs.SaveRun(context.Background(), run); err != nil {
			t.Fatal(err)
		}
		return s, run.ID, &launched
	}

	t.Run("a foreign grant launches nothing", func(t *testing.T) {
		s, runID, launched := build(t, "team-b")
		s.autofixOffer(context.Background(), runID)
		if *launched != 0 {
			t.Fatalf("launched %d code-pushing runs into another tenant", *launched)
		}
		rows := waitForTenantAudit(t, s.auditStore, "team-b", auditActionGrantTenantMismatch)
		if len(rows) != 1 {
			t.Fatalf("want exactly one audit row, got %d", len(rows))
		}
		if got := rows[0].Meta["surface"]; got != "gate auto-fix" {
			t.Fatalf("meta surface = %v, want the lane named", got)
		}
	})

	t.Run("the run's own tenant still launches", func(t *testing.T) {
		s, runID, launched := build(t, team)
		s.autofixOffer(context.Background(), runID)
		if *launched != 1 {
			t.Fatalf("the lane must still fire for the run's own tenant, launched %d", *launched)
		}
	})
}

// The earliest door: a launch may pin its own forge_publish_token, and
// injectForgePublishVars honours the pin rather than overwriting it. A pin
// that resolves to ANOTHER tenant's grant is refused there, before the run
// exists — the run would otherwise carry a credential the publish endpoint
// authenticates on its own (that endpoint holds no run, so the token IS the
// authority there and it cannot tell).
func TestInjectForgePublishVars_RefusesAForeignPinnedGrant(t *testing.T) {
	newServer := func(t *testing.T) *Server {
		t.Helper()
		s, _ := newForgePublishTestServer(t)
		s.cfg.PublicURL = "https://iterion.test"
		registerPublishToken(t, s, "tok-team1", ForgePublishGrant{
			TeamID: "team1", ConnectionID: "conn1", Repo: "o/r", Bot: "review-pr",
		})
		return s
	}
	prVars := func(token string) map[string]string {
		return map[string]string{"pr_url": "https://github.com/o/r/pull/42", forgePublishVarToken: token}
	}

	t.Run("another tenant's grant is refused, nothing launches", func(t *testing.T) {
		s := newServer(t)
		_, err := s.injectForgePublishVars(context.Background(), "team-b", "", "review-pr", prVars("tok-team1"), nil)
		if err == nil {
			t.Fatal("a pinned grant of another tenant was accepted — the run would publish as that tenant")
		}
		if !errors.Is(err, errForgePublishGrantTenant) {
			t.Fatalf("err = %v, want the typed refusal so the HTTP lane answers 422", err)
		}
		if strings.Contains(err.Error(), "tok-team1") {
			t.Fatalf("the refusal leaks the token: %v", err)
		}
	})

	t.Run("the launch door propagates it", func(t *testing.T) {
		s := newServer(t)
		_, err := s.applyPRLaunchContext(context.Background(), "team-b", "", "review-pr", prVars("tok-team1"), nil)
		if !errors.Is(err, errForgePublishGrantTenant) {
			t.Fatalf("applyPRLaunchContext err = %v, want the typed refusal", err)
		}
	})

	t.Run("a run pinning its OWN tenant's grant keeps it", func(t *testing.T) {
		s := newServer(t)
		out, err := s.injectForgePublishVars(context.Background(), "team1", "", "review-pr", prVars("tok-team1"), nil)
		if err != nil {
			t.Fatalf("a team's own grant must be honoured: %v", err)
		}
		if out[forgePublishVarToken] != "tok-team1" {
			t.Fatalf("the pin was overwritten: %q", out[forgePublishVarToken])
		}
	})

	// An UNRESOLVABLE pin is not a tenant crossing: the in-memory registry is
	// emptied by a restart, so refusing it would turn a stale token into a
	// failed launch instead of a run that merely cannot publish. It stays
	// honoured, and the publish endpoint answers 401.
	t.Run("an unknown token is not refused", func(t *testing.T) {
		s := newServer(t)
		out, err := s.injectForgePublishVars(context.Background(), "team-b", "", "review-pr", prVars("tok-gone"), nil)
		if err != nil {
			t.Fatalf("an unresolvable pin must not fail the launch: %v", err)
		}
		if out[forgePublishVarToken] != "tok-gone" {
			t.Fatalf("the pin was overwritten: %q", out[forgePublishVarToken])
		}
	})
}
