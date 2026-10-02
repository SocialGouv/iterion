package server

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/identity"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/pat"
)

// outageStore fails exactly one call — GetTeam — and passes everything
// else through: an infrastructure outage must read as a 5xx, never as the
// client-fault 400 the unknown-team refusal carries.
type outageStore struct {
	identity.Store
	err error
}

func (f outageStore) GetTeam(ctx context.Context, id string) (identity.Team, error) {
	return identity.Team{}, f.err
}

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

	t.Run("team pin refuses a team that does not exist", func(t *testing.T) {
		// A super-admin passes the membership gate for ANY string — the
		// existence gate is what stops a slug-shaped pin minted 201 and
		// then unable to authenticate ("token team unavailable" forever).
		w := httptest.NewRecorder()
		s.handleCreatePAT(w, orgReq(superAdminCtx(), "POST", "/api/me/tokens", `{"name":"slug","team_id":"pic-graal"}`, ""))
		if w.Code != http.StatusBadRequest {
			t.Fatalf("mint for a non-existent team status = %d, want 400", w.Code)
		}
		if !strings.Contains(w.Body.String(), "unknown team") {
			t.Fatalf("the refusal names the unknown team: %s", w.Body.String())
		}
	})

	t.Run("a store outage is not read as an unknown team", func(t *testing.T) {
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			t.Fatalf("rand: %v", err)
		}
		signer, err := auth.NewJWTSigner(base64.RawStdEncoding.EncodeToString(key), 15*time.Minute)
		if err != nil {
			t.Fatalf("signer: %v", err)
		}
		svc, err := auth.NewService(auth.Config{
			Store:      outageStore{Store: identity.NewMemoryStore(), err: errors.New("store unavailable")},
			Sessions:   auth.NewMemorySessionStore(),
			Signer:     signer,
			SignupMode: auth.SignupOpen,
			RefreshTTL: time.Hour,
		})
		if err != nil {
			t.Fatalf("auth service: %v", err)
		}
		s2 := New(Config{}, iterlog.New(iterlog.LevelError, nil))
		s2.authSvc = svc
		w := httptest.NewRecorder()
		s2.handleCreatePAT(w, orgReq(superAdminCtx(), "POST", "/api/me/tokens", `{"name":"outage","team_id":"t1"}`, ""))
		if w.Code != http.StatusInternalServerError {
			t.Fatalf("a store outage must read 5xx, not the client-fault 400: status=%d body=%s", w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "check team") {
			t.Fatalf("the 500 carries the cause: %s", w.Body.String())
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
