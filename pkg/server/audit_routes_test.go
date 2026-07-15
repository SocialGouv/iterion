package server

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/audit"
	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/identity"
)

// TestAuditEndToEnd exercises the write helper + the two list routes:
// a mutation handler call lands one row; an org admin reads tenant
// scope; platform scope stays super-admin only.
func TestAuditEndToEnd(t *testing.T) {
	s := newOrgTestServer(t)
	s.auditStore = audit.NewMemoryStore()
	seedTeam(t, s, "t1", "acme")
	if _, err := s.authStore().CreateUser(context.Background(), identity.User{ID: "u1", Email: "u1@x", Status: identity.UserStatusActive}); err != nil {
		t.Fatal(err)
	}
	if err := s.authStore().UpsertMembership(context.Background(), identity.Membership{UserID: "u1", TeamID: "t1", Role: identity.RoleAdmin}); err != nil {
		t.Fatal(err)
	}

	// Write via the helper exactly as a mutation handler would.
	adminCtx := auth.WithIdentity(context.Background(), auth.Identity{UserID: "u1", TeamID: "t1", Role: identity.RoleAdmin})
	r := orgReq(adminCtx, "POST", "/api/teams/t1/webhooks", "", "t1")
	s.auditTenant(r, "t1", "webhook.created", "webhook", "wh-1", map[string]any{"name": "qa"})
	s.auditPlatform(r, "t1", "org.status_changed", "org", "t1", nil)

	// Writes are detached; poll briefly for both rows to land.
	deadline := time.Now().Add(10 * time.Second)
	for {
		tenantEvs, _ := s.auditStore.ListByTenant(context.Background(), "t1", audit.Page{})
		platEvs, _ := s.auditStore.ListPlatform(context.Background(), audit.Page{})
		if (len(tenantEvs) >= 1 && len(platEvs) >= 1) || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	t.Run("org admin reads tenant scope", func(t *testing.T) {
		w := httptest.NewRecorder()
		s.handleTeamAudit(w, orgReq(adminCtx, "GET", "/api/teams/t1/audit", "", "t1"))
		if w.Code != 200 {
			t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
		}
		var resp auditListResponse
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		if len(resp.Events) != 1 || resp.Events[0].Action != "webhook.created" {
			t.Fatalf("events = %+v, want the single tenant-scoped row", resp.Events)
		}
		if resp.Events[0].ActorID != "u1" || resp.Events[0].ActorKind != "user" {
			t.Fatalf("actor = %s/%s, want u1/user", resp.Events[0].ActorID, resp.Events[0].ActorKind)
		}
	})

	t.Run("member without admin is denied", func(t *testing.T) {
		if err := s.authStore().UpsertMembership(context.Background(), identity.Membership{UserID: "u2", TeamID: "t1", Role: identity.RoleMember}); err != nil {
			t.Fatal(err)
		}
		memberCtx := auth.WithIdentity(context.Background(), auth.Identity{UserID: "u2", TeamID: "t1", Role: identity.RoleMember})
		w := httptest.NewRecorder()
		s.handleTeamAudit(w, orgReq(memberCtx, "GET", "/api/teams/t1/audit", "", "t1"))
		if w.Code != 403 {
			t.Fatalf("status = %d, want 403 for non-admin member", w.Code)
		}
	})

	t.Run("platform scope via admin route", func(t *testing.T) {
		w := httptest.NewRecorder()
		s.handleAdminAudit(w, orgReq(superAdminCtx(), "GET", "/api/admin/audit", "", ""))
		if w.Code != 200 {
			t.Fatalf("status = %d", w.Code)
		}
		var resp auditListResponse
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		if len(resp.Events) != 1 || resp.Events[0].Action != "org.status_changed" {
			t.Fatalf("platform events = %+v", resp.Events)
		}
	})
}
