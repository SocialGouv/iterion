package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/audit"
	"github.com/SocialGouv/iterion/pkg/forge"
	"github.com/SocialGouv/iterion/pkg/identity"
	"github.com/SocialGouv/iterion/pkg/webhooks"
)

// Team deletion and the forge tombstone. Deprovision keeps the disabled
// webhook config of a retired integration as an admission fence, and no
// route removes a provisioned config: counted as residue, it made every
// team that ever had a forge integration undeletable. The delete sets
// apart the tombstones of retired, cleaned-up integrations, and removes
// them with their rows; every other hook still refuses the delete.

// The whole lifecycle through the public handlers: a live integration
// refuses the delete and names its hook, deprovision then disconnect leave
// only the tombstone, and the delete then succeeds and takes the tombstone
// and its retired row with it.
func TestTeamDelete_afterDeprovisionAndDisconnect(t *testing.T) {
	gl := newMockGitLab()
	srv := gl.server()
	defer srv.Close()
	s := newOrgCredsTestServer(t)
	wireForgeTestStores(t, s)
	s.auditStore = audit.NewMemoryStore()
	ctx := superAdminCtx()

	w := httptest.NewRecorder()
	s.handleConnectForge(w, forgeReq(ctx, "POST", "/api/teams/t1/forge/connections",
		`{"provider":"gitlab","mode":"pat","forge_base_url":"`+srv.URL+`","pat":"glpat-token"}`, "t1"))
	if w.Code != http.StatusOK {
		t.Fatalf("connect: code=%d body=%s", w.Code, w.Body.String())
	}
	var connResp forgeConnectResp
	if err := json.Unmarshal(w.Body.Bytes(), &connResp); err != nil {
		t.Fatal(err)
	}
	connID := connResp.Connection.ID

	w = httptest.NewRecorder()
	s.handleEnableForgeRepoBots(w, forgeReq(ctx, "POST", "/api/teams/t1/forge/repo-bots",
		`{"connection_id":"`+connID+`","repo":"group/api","bot_ids":["review-pr"]}`, "t1"))
	if w.Code != http.StatusOK {
		t.Fatalf("enable: code=%d body=%s", w.Code, w.Body.String())
	}
	var res forge.ProvisionResult
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}

	w = httptest.NewRecorder()
	s.handleDeleteTeam(w, teamReq(ctx, "DELETE", "/api/teams/t1", "", "t1", ""))
	if w.Code != http.StatusConflict {
		t.Fatalf("delete a team with a live integration: code=%d body=%s, want 409", w.Code, w.Body.String())
	}
	for _, want := range []string{"repo integration", "inbound webhook"} {
		if !strings.Contains(w.Body.String(), want) {
			t.Errorf("the refusal of a live integration does not name %q: %s", want, w.Body.String())
		}
	}

	w = httptest.NewRecorder()
	dis := forgeReq(ctx, "DELETE", "/api/teams/t1/forge/repo-bots/"+res.IntegrationID, "", "t1")
	dis.SetPathValue("integration_id", res.IntegrationID)
	s.handleDisableForgeRepoBots(w, dis)
	if w.Code != http.StatusNoContent {
		t.Fatalf("disable: code=%d body=%s", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	disc := forgeReq(ctx, "DELETE", "/api/teams/t1/forge/connections/"+connID, "", "t1")
	disc.SetPathValue("conn_id", connID)
	s.handleDeleteForgeConnection(w, disc)
	if w.Code != http.StatusNoContent {
		t.Fatalf("disconnect: code=%d body=%s", w.Code, w.Body.String())
	}
	// The fixture is the one the ticket names: the tombstone is there.
	if cfg, err := s.webhookConfigs.Get(context.Background(), res.WebhookID); err != nil || cfg.Enabled || cfg.ProvisionIntegrationID != res.IntegrationID {
		t.Fatalf("deprovision left no bound tombstone: cfg=%+v err=%v", cfg, err)
	}

	w = httptest.NewRecorder()
	s.handleDeleteTeam(w, teamReq(ctx, "DELETE", "/api/teams/t1", "", "t1", ""))
	if w.Code != http.StatusNoContent {
		t.Fatalf("delete a team left with a retired tombstone: code=%d body=%s, want 204", w.Code, w.Body.String())
	}
	if _, err := s.authStore().GetTeam(context.Background(), "t1"); err == nil {
		t.Error("the team survived a 204 delete")
	}
	if _, err := s.authStore().GetMembership(context.Background(), "teamadmin", "t1"); err == nil {
		t.Error("a membership survived its team")
	}
	if _, err := s.webhookConfigs.Get(context.Background(), res.WebhookID); !errors.Is(err, webhooks.ErrNotFound) {
		t.Errorf("the tombstone survived the team delete: err=%v, want webhooks.ErrNotFound", err)
	}
	if _, err := s.forgeIntegrations.GetProvision(context.Background(), res.IntegrationID); !errors.Is(err, forge.ErrIntegrationNotFound) {
		t.Errorf("the retired integration row survived the team delete: err=%v, want forge.ErrIntegrationNotFound", err)
	}
	evs := waitForTenantAudit(t, s.auditStore, "o1", "team.deleted")
	if got := fmt.Sprint(evs[0].Meta["purged_webhook_tombstones"]); got != "1" {
		t.Errorf("team.deleted audit purged_webhook_tombstones = %s, want 1", got)
	}
}

// failingProvisionReads answers every lifecycle read with an error that is
// not a not-found: the integration store is down.
type failingProvisionReads struct {
	*forge.MemoryRepoIntegrationStore
}

func (failingProvisionReads) GetProvision(context.Context, string) (forge.RepoIntegration, error) {
	return forge.RepoIntegration{}, errors.New("integration store down")
}

// failingIntegrationDeletes refuses every integration delete.
type failingIntegrationDeletes struct {
	*forge.MemoryRepoIntegrationStore
}

func (failingIntegrationDeletes) Delete(context.Context, string) error {
	return errors.New("integration store down")
}

// tombstoneFixture seeds team t2 with ONE webhook config as its only
// residue, bound to an integration row when row is non-nil.
func tombstoneFixture(t *testing.T, cfg webhooks.Config, row *forge.RepoIntegration) *Server {
	t.Helper()
	s := newOrgCredsTestServer(t)
	wireForgeTestStores(t, s)
	if row != nil {
		if err := s.forgeIntegrations.Create(withTenantCtx(row.TenantID), *row); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.webhookConfigs.Create(withTenantCtx("t2"), cfg); err != nil {
		t.Fatal(err)
	}
	return s
}

func retiredRow(tenant string) *forge.RepoIntegration {
	return &forge.RepoIntegration{
		ID: "ri1", TenantID: tenant, ConnectionID: "c1", RepoFullName: "group/api",
		Retired: true, RetireCleanupComplete: true, CreatedAt: time.Now(),
	}
}

func boundHook(enabled bool) webhooks.Config {
	return webhooks.Config{
		ID: "wh1", TenantID: "t2", Name: "hook", Enabled: enabled,
		ProvisionIntegrationID: "ri1", ProvisionedBy: "forge:c1",
	}
}

// Every guard of the tombstone classification, one scenario each, the hook
// being the team's only residue: each refusal is that guard's alone.
func TestTeamDelete_hookStillRefuses(t *testing.T) {
	cases := []struct {
		name string
		cfg  webhooks.Config
		row  *forge.RepoIntegration
		wrap func(*Server)
		want string
	}{
		{
			name: "enabled config of a retired, cleaned-up integration",
			cfg:  boundHook(true),
			row:  retiredRow("t2"),
			want: "1 inbound webhook(s)",
		},
		{
			name: "standalone disabled hook",
			cfg:  webhooks.Config{ID: "wh1", TenantID: "t2", Name: "hook"},
			want: "1 inbound webhook(s)",
		},
		{
			name: "integration of another team",
			cfg:  boundHook(false),
			row:  retiredRow("t9"),
			want: "1 inbound webhook(s)",
		},
		{
			name: "integration not retired",
			cfg:  boundHook(false),
			// Pending with no webhook yet: hidden from ListByTenant, so the
			// hook stays the only residue and the Retired guard alone judges.
			row: &forge.RepoIntegration{
				ID: "ri1", TenantID: "t2", ConnectionID: "c1", RepoFullName: "group/api",
				ProvisionPending: true, RetireCleanupComplete: true, CreatedAt: time.Now(),
			},
			want: "1 inbound webhook(s)",
		},
		{
			name: "retirement cleanup not complete",
			cfg:  boundHook(false),
			row: &forge.RepoIntegration{
				ID: "ri1", TenantID: "t2", ConnectionID: "c1", RepoFullName: "group/api",
				Retired: true, CreatedAt: time.Now(),
			},
			want: "1 inbound webhook(s)",
		},
		{
			name: "frozen enabled config, retirement cleanup not complete",
			cfg:  func() webhooks.Config { c := boundHook(true); c.ProvisionFrozen = true; return c }(),
			row: &forge.RepoIntegration{
				ID: "ri1", TenantID: "t2", ConnectionID: "c1", RepoFullName: "group/api",
				Retired: true, CreatedAt: time.Now(),
			},
			want: "1 inbound webhook(s)",
		},
		{
			name: "integration store unreadable",
			cfg:  boundHook(false),
			row:  retiredRow("t2"),
			wrap: func(s *Server) {
				s.forgeIntegrations = failingProvisionReads{s.forgeIntegrations.(*forge.MemoryRepoIntegrationStore)}
			},
			want: "webhooks (unreadable: integration of webhook wh1: integration store down)",
		},
		{
			name: "no integration store",
			cfg:  boundHook(false),
			wrap: func(s *Server) { s.forgeIntegrations = nil },
			want: "1 inbound webhook(s)",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := tombstoneFixture(t, tc.cfg, tc.row)
			if tc.wrap != nil {
				tc.wrap(s)
			}
			w := httptest.NewRecorder()
			s.handleDeleteTeam(w, teamReq(superAdminCtx(), "DELETE", "/api/teams/t2", "", "t2", ""))
			if w.Code != http.StatusConflict {
				t.Fatalf("delete: code=%d body=%s, want 409", w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), tc.want) {
				t.Errorf("refusal = %s, want it to name %q", w.Body.String(), tc.want)
			}
			if _, err := s.authStore().GetTeam(context.Background(), "t2"); err != nil {
				t.Fatalf("the team was deleted despite the refusal: %v", err)
			}
			if _, err := s.webhookConfigs.Get(context.Background(), "wh1"); err != nil {
				t.Errorf("a refused delete removed the hook: %v", err)
			}
		})
	}
}

// The control of the table above: the same fixture with every guard
// satisfied deletes, and so does a hook bound to a row that is already gone
// (the state an interrupted purge leaves, which the intake answers 410).
func TestTeamDelete_retiredTombstoneDeletes(t *testing.T) {
	for _, tc := range []struct {
		name string
		row  *forge.RepoIntegration
	}{
		{name: "retired, cleaned-up integration", row: retiredRow("t2")},
		{name: "binding to a missing integration", row: nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := tombstoneFixture(t, boundHook(false), tc.row)
			w := httptest.NewRecorder()
			s.handleDeleteTeam(w, teamReq(superAdminCtx(), "DELETE", "/api/teams/t2", "", "t2", ""))
			if w.Code != http.StatusNoContent {
				t.Fatalf("delete: code=%d body=%s, want 204", w.Code, w.Body.String())
			}
			if _, err := s.webhookConfigs.Get(context.Background(), "wh1"); !errors.Is(err, webhooks.ErrNotFound) {
				t.Errorf("the tombstone survived: err=%v", err)
			}
			if _, err := s.forgeIntegrations.GetProvision(context.Background(), "ri1"); !errors.Is(err, forge.ErrIntegrationNotFound) {
				t.Errorf("the retired row survived: err=%v", err)
			}
		})
	}
}

// A purge that fails answers 500 before the first revoke: the team, its
// grants and the tombstone are all still there for the retry.
func TestTeamDelete_aFailedPurgeLeavesTheTeamUntouched(t *testing.T) {
	s := tombstoneFixture(t, boundHook(false), retiredRow("t2"))
	if err := s.authStore().UpsertMembership(context.Background(), identity.Membership{UserID: "orgadmin", TeamID: "t2", Role: identity.RoleAdmin}); err != nil {
		t.Fatal(err)
	}
	s.forgeIntegrations = failingIntegrationDeletes{s.forgeIntegrations.(*forge.MemoryRepoIntegrationStore)}

	w := httptest.NewRecorder()
	s.handleDeleteTeam(w, teamReq(superAdminCtx(), "DELETE", "/api/teams/t2", "", "t2", ""))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("delete: code=%d body=%s, want 500", w.Code, w.Body.String())
	}
	if _, err := s.authStore().GetTeam(context.Background(), "t2"); err != nil {
		t.Errorf("the team was deleted despite the failed purge: %v", err)
	}
	if _, err := s.authStore().GetMembership(context.Background(), "orgadmin", "t2"); err != nil {
		t.Errorf("a grant was revoked despite the failed purge: %v", err)
	}
	if _, err := s.webhookConfigs.Get(context.Background(), "wh1"); err != nil {
		t.Errorf("the tombstone went although its row could not: %v", err)
	}
}

// A refused delete purges nothing: the tombstone and its retired row survive
// a rank refusal (403) and a residue refusal (409), one scenario per
// refusal, since the purge runs only past both.
func TestTeamDelete_aRefusedDeleteKeepsTheTombstone(t *testing.T) {
	for _, tc := range []struct {
		name string
		seed func(*testing.T, *Server)
		ctx  context.Context
		want int
	}{
		{
			name: "rank refusal",
			seed: func(t *testing.T, s *Server) {
				seedAccount(t, s, "boss", "boss@example.com")
				seedOrgRow(t, s, "boss", "o1", identity.OrgRoleOwner)
				seedTeamGrant(t, s, "boss", "t2", identity.RoleOwner)
			},
			ctx:  orgAdminCtx(),
			want: http.StatusForbidden,
		},
		{
			name: "residue refusal",
			seed: func(t *testing.T, s *Server) {
				live := webhooks.Config{ID: "wh2", TenantID: "t2", Name: "live", Enabled: true}
				if err := s.webhookConfigs.Create(withTenantCtx("t2"), live); err != nil {
					t.Fatal(err)
				}
			},
			ctx:  superAdminCtx(),
			want: http.StatusConflict,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := tombstoneFixture(t, boundHook(false), retiredRow("t2"))
			tc.seed(t, s)
			w := httptest.NewRecorder()
			s.handleDeleteTeam(w, teamReq(tc.ctx, "DELETE", "/api/teams/t2", "", "t2", ""))
			if w.Code != tc.want {
				t.Fatalf("delete: code=%d body=%s, want %d", w.Code, w.Body.String(), tc.want)
			}
			if _, err := s.authStore().GetTeam(context.Background(), "t2"); err != nil {
				t.Fatalf("the team was deleted despite the refusal: %v", err)
			}
			if _, err := s.webhookConfigs.Get(context.Background(), "wh1"); err != nil {
				t.Errorf("a refused delete purged the tombstone: %v", err)
			}
			if _, err := s.forgeIntegrations.GetProvision(context.Background(), "ri1"); err != nil {
				t.Errorf("a refused delete purged the retired row: %v", err)
			}
		})
	}
}

// An integration migrated off a legacy (hash-only) hook keeps the old config
// ENABLED and frozen, bound to the row; Deprovision disables only the row's
// current and candidate configs. The frozen config is a superseded epoch the
// intake answers 410, so once the row is retired and cleaned up the team
// deletes and takes both configs and the row with it.
func TestTeamDelete_afterLegacyHookMigration(t *testing.T) {
	gl := newMockGitLab()
	srv := gl.server()
	defer srv.Close()
	s := newOrgCredsTestServer(t)
	wireForgeTestStores(t, s)
	ctx := superAdminCtx()
	tctx := withTenantCtx("t1")

	w := httptest.NewRecorder()
	s.handleConnectForge(w, forgeReq(ctx, "POST", "/api/teams/t1/forge/connections",
		`{"provider":"gitlab","mode":"pat","forge_base_url":"`+srv.URL+`","pat":"glpat-token"}`, "t1"))
	if w.Code != http.StatusOK {
		t.Fatalf("connect: code=%d body=%s", w.Code, w.Body.String())
	}
	var connResp forgeConnectResp
	if err := json.Unmarshal(w.Body.Bytes(), &connResp); err != nil {
		t.Fatal(err)
	}
	connID := connResp.Connection.ID
	enable := func() forge.ProvisionResult {
		t.Helper()
		w := httptest.NewRecorder()
		s.handleEnableForgeRepoBots(w, forgeReq(ctx, "POST", "/api/teams/t1/forge/repo-bots",
			`{"connection_id":"`+connID+`","repo":"group/api","bot_ids":["review-pr"]}`, "t1"))
		if w.Code != http.StatusOK {
			t.Fatalf("enable: code=%d body=%s", w.Code, w.Body.String())
		}
		var res forge.ProvisionResult
		if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
			t.Fatal(err)
		}
		return res
	}
	first := enable()

	// The legacy shape, as pkg/forge TestProvisionLegacyMigratesWithoutChangingOldSecret
	// builds it: a hash-only row whose config is not bound to it.
	ri, err := s.forgeIntegrations.GetProvision(tctx, first.IntegrationID)
	if err != nil {
		t.Fatal(err)
	}
	ri.WebhookSecretSealed = nil
	ri.HookURL = ""
	if err := s.forgeIntegrations.Update(tctx, ri); err != nil {
		t.Fatal(err)
	}
	old, err := s.webhookConfigs.Get(tctx, first.WebhookID)
	if err != nil {
		t.Fatal(err)
	}
	old.ProvisionIntegrationID = ""
	if err := s.webhookConfigs.Update(tctx, old); err != nil {
		t.Fatal(err)
	}

	second := enable()
	if second.WebhookID == first.WebhookID {
		t.Fatalf("the re-enable did not migrate to a new webhook: %s", second.WebhookID)
	}

	w = httptest.NewRecorder()
	dis := forgeReq(ctx, "DELETE", "/api/teams/t1/forge/repo-bots/"+second.IntegrationID, "", "t1")
	dis.SetPathValue("integration_id", second.IntegrationID)
	s.handleDisableForgeRepoBots(w, dis)
	if w.Code != http.StatusNoContent {
		t.Fatalf("disable: code=%d body=%s", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	disc := forgeReq(ctx, "DELETE", "/api/teams/t1/forge/connections/"+connID, "", "t1")
	disc.SetPathValue("conn_id", connID)
	s.handleDeleteForgeConnection(w, disc)
	if w.Code != http.StatusNoContent {
		t.Fatalf("disconnect: code=%d body=%s", w.Code, w.Body.String())
	}
	// The fixture the finding names: the old config is still enabled and
	// frozen, and the intake refuses it as a retired epoch.
	frozen, err := s.webhookConfigs.Get(tctx, first.WebhookID)
	if err != nil || !frozen.Enabled || !frozen.ProvisionFrozen {
		t.Fatalf("the migration left no enabled frozen config: cfg=%+v err=%v", frozen, err)
	}
	rec := httptest.NewRecorder()
	if s.webhookLive(rec, httptest.NewRequest("POST", "/", nil), frozen) || rec.Code != http.StatusGone {
		t.Fatalf("the intake admits the frozen config: code=%d body=%s, want 410", rec.Code, rec.Body.String())
	}

	w = httptest.NewRecorder()
	s.handleDeleteTeam(w, teamReq(ctx, "DELETE", "/api/teams/t1", "", "t1", ""))
	if w.Code != http.StatusNoContent {
		t.Fatalf("delete a team left with a migrated, retired integration: code=%d body=%s, want 204", w.Code, w.Body.String())
	}
	if left, err := s.webhookConfigs.ListByTenant(tctx, "t1"); err != nil || len(left) != 0 {
		t.Errorf("webhook configs left after the delete: %d (err=%v), want 0", len(left), err)
	}
	if _, err := s.forgeIntegrations.GetProvision(tctx, first.IntegrationID); !errors.Is(err, forge.ErrIntegrationNotFound) {
		t.Errorf("the retired integration row survived the team delete: err=%v, want forge.ErrIntegrationNotFound", err)
	}
}
