package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/identity"
	"github.com/SocialGouv/iterion/pkg/pat"
)

func newPATTestServer(t *testing.T) (*Server, context.Context) {
	t.Helper()
	s := newOrgTestServer(t)
	s.pats = pat.NewMemoryStore()
	seedTeam(t, s, "t1", "acme")
	if _, err := s.authStore().CreateUser(context.Background(), identity.User{
		ID: "u1", Email: "u1@x", Status: identity.UserStatusActive, DefaultTeamID: "t1",
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.authStore().UpsertMembership(context.Background(), identity.Membership{
		UserID: "u1", TeamID: "t1", Role: identity.RoleMember,
	}); err != nil {
		t.Fatal(err)
	}
	ctx := auth.WithIdentity(context.Background(), auth.Identity{UserID: "u1", Email: "u1@x", TeamID: "t1", Role: identity.RoleMember})
	return s, ctx
}

func createPAT(t *testing.T, s *Server, ctx context.Context, body string) (pat.Token, string) {
	t.Helper()
	w := httptest.NewRecorder()
	s.handleCreatePAT(w, orgReq(ctx, "POST", "/api/me/tokens", body, ""))
	if w.Code != http.StatusCreated {
		t.Fatalf("create status = %d body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		PAT   pat.Token `json:"pat"`
		Token string    `json:"token"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	return resp.PAT, resp.Token
}

func TestPATLifecycle(t *testing.T) {
	s, ctx := newPATTestServer(t)
	created, plaintext := createPAT(t, s, ctx, `{"name":"ci"}`)
	if plaintext == "" || created.TokenLast4 == "" {
		t.Fatalf("create returned no plaintext/last4: %+v / %q", created, plaintext)
	}

	t.Run("bearer authenticates as the user", func(t *testing.T) {
		id, err := s.identityFromPAT(context.Background(), plaintext)
		if err != nil {
			t.Fatalf("identityFromPAT: %v", err)
		}
		if id.UserID != "u1" || id.TeamID != "t1" || id.Role != identity.RoleMember {
			t.Fatalf("identity = %+v", id)
		}
	})

	t.Run("list never returns plaintext", func(t *testing.T) {
		w := httptest.NewRecorder()
		s.handleListPATs(w, orgReq(ctx, "GET", "/api/me/tokens", "", ""))
		if w.Code != 200 {
			t.Fatalf("list status = %d", w.Code)
		}
		if strings.Contains(w.Body.String(), plaintext) {
			t.Fatal("list leaked plaintext")
		}
	})

	t.Run("revoke kills auth", func(t *testing.T) {
		w := httptest.NewRecorder()
		r := orgReq(ctx, "DELETE", "/api/me/tokens/"+created.ID, "", "")
		r.SetPathValue("token_id", created.ID)
		s.handleRevokePAT(w, r)
		if w.Code != http.StatusNoContent {
			t.Fatalf("revoke status = %d", w.Code)
		}
		if _, err := s.identityFromPAT(context.Background(), plaintext); err == nil {
			t.Fatal("revoked PAT still authenticates")
		}
	})
}

func TestPATExpiryAndPins(t *testing.T) {
	s, ctx := newPATTestServer(t)

	t.Run("expired token rejected", func(t *testing.T) {
		created, plaintext := createPAT(t, s, ctx, `{"name":"short","expires_in_days":1}`)
		// Force-expire by rewriting the stored row.
		tok, _ := s.pats.Get(context.Background(), created.ID)
		past := time.Now().Add(-time.Hour)
		tok.ExpiresAt = &past
		_ = s.pats.Create(context.Background(), tok) // memory store upserts by ID
		if _, err := s.identityFromPAT(context.Background(), plaintext); err == nil {
			t.Fatal("expired PAT still authenticates")
		}
	})

	t.Run("platform max TTL clamps", func(t *testing.T) {
		s.cfg.PATMaxTTL = 24 * time.Hour
		created, _ := createPAT(t, s, ctx, `{"name":"clamped"}`)
		if created.ExpiresAt == nil || time.Until(*created.ExpiresAt) > 25*time.Hour {
			t.Fatalf("ExpiresAt = %v, want clamped to ~24h", created.ExpiresAt)
		}
		s.cfg.PATMaxTTL = 0
	})

	t.Run("team pin requires membership", func(t *testing.T) {
		w := httptest.NewRecorder()
		s.handleCreatePAT(w, orgReq(ctx, "POST", "/api/me/tokens", `{"name":"pinned","team_id":"ghost"}`, ""))
		if w.Code != http.StatusForbidden {
			t.Fatalf("pin to non-member team status = %d, want 403", w.Code)
		}
	})

	t.Run("team pin states the team's org", func(t *testing.T) {
		if _, err := s.authStore().CreateTeam(context.Background(), identity.Team{
			ID: "t2", Name: "t2", Slug: "t2-slug", OrgID: "org-9", CreatedAt: time.Now(),
		}); err != nil {
			t.Fatal(err)
		}
		if err := s.authStore().UpsertMembership(context.Background(), identity.Membership{
			UserID: "u1", TeamID: "t2", Role: identity.RoleMember,
		}); err != nil {
			t.Fatal(err)
		}
		w := httptest.NewRecorder()
		s.handleCreatePAT(w, orgReq(ctx, "POST", "/api/me/tokens", `{"name":"org-stated","team_id":"t2"}`, ""))
		if w.Code != http.StatusCreated {
			t.Fatalf("create status = %d body=%s", w.Code, w.Body.String())
		}
		var resp struct {
			OrgID string `json:"org_id"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		// The server owns the team→org fact; clients (teams switch) align
		// their org scope on this instead of re-deriving it client-side.
		if resp.OrgID != "org-9" {
			t.Fatalf("org_id = %q, want org-9", resp.OrgID)
		}
	})

	t.Run("membership removal kills the PAT", func(t *testing.T) {
		_, plaintext := createPAT(t, s, ctx, `{"name":"member-bound"}`)
		if err := s.authStore().DeleteMembership(context.Background(), "u1", "t1"); err != nil {
			t.Fatal(err)
		}
		if _, err := s.identityFromPAT(context.Background(), plaintext); err == nil {
			t.Fatal("PAT survives membership removal")
		}
	})
}
