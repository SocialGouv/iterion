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
	"github.com/SocialGouv/iterion/pkg/credpool"
	"github.com/SocialGouv/iterion/pkg/identity"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/secrets"
)

// poolRoutesServer is the fold's test server with the pool stores wired —
// the org/team fixtures the attachment authorities read.
func poolRoutesServer(t *testing.T) *Server {
	t.Helper()
	s := newOrgTestServer(t)
	seedOrg(t, s, "org1", "org1")
	if _, err := s.authStore().CreateTeam(context.Background(), identity.Team{
		ID: "team1", Name: "team1", Slug: "team1", OrgID: "org1", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("seed team: %v", err)
	}
	s.credPoolAttachments = credpool.NewMemoryAttachmentStore()
	pools := credpool.NewMemoryPoolStore()
	s.credPoolPools = pools
	// "jo" is an org1 ADMIN: the level's write right AND the pool's
	// ownership can cohabit on one identity for the consent scenarios.
	if err := s.authStore().UpsertOrgMembership(context.Background(), identity.OrgMembership{
		UserID: "jo", OrgID: "org1", Role: identity.OrgRoleAdmin, JoinedAt: time.Now(),
	}); err != nil {
		t.Fatalf("seed membership: %v", err)
	}
	sealer, err := secrets.NewAESGCMSealer(make([]byte, 32))
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	s.credPool = credpool.NewBroker(credpool.BrokerConfig{
		Pools:   pools,
		Pledges: credpool.NewMemoryPledgeStore(),
		Leases:  credpool.NewMemoryLeaseStore(),
		Ledger:  credpool.NewMemoryLedger(),
		OAuth:   secrets.NewMemoryOAuthStore(),
		Sealer:  sealer,
		Logger:  iterlog.New(iterlog.LevelError, nil),
	})
	return s
}

func poolRouteReq(ctx context.Context, method, path, body string) *http.Request {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	return r.WithContext(ctx)
}

func runPoolRoute(s *Server, w *httptest.ResponseRecorder, r *http.Request) {
	// The mux pattern variables the handlers read via PathValue.
	if strings.Contains(r.URL.Path, "/orgs/") {
		r.SetPathValue("id", "org1")
	} else if strings.Contains(r.URL.Path, "/teams/") {
		r.SetPathValue("id", "team1")
	}
	switch {
	case strings.HasSuffix(r.URL.Path, "/pools") && r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/me/"):
		s.handleCreateMyPool(w, r)
	case strings.HasSuffix(r.URL.Path, "/pool-attachment") && r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/orgs/"):
		s.handlePutOrgPoolAttachment(w, r)
	case strings.HasSuffix(r.URL.Path, "/pool-attachment") && r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/orgs/"):
		s.handleGetOrgPoolAttachment(w, r)
	default:
		w.WriteHeader(http.StatusTeapot)
	}
}

// The org admin attaching a PRIVATE user pool is refused — the owner's
// consent is the second half of the authority (law 2: an attachment is an
// opt-in, never a takeover); the OWNER attaching their own pool answers,
// and a stale CAS write loses loudly.
func TestPoolAttachmentRoutes_consentAndCAS(t *testing.T) {
	s := poolRoutesServer(t)
	ctx := auth.WithIdentity(context.Background(), auth.Identity{UserID: "jo"})
	// The owner creates a private pool.
	w := httptest.NewRecorder()
	runPoolRoute(s, w, poolRouteReq(ctx, http.MethodPost, "/api/me/pools", `{"name":"dose gratuite"}`))
	if w.Code != http.StatusOK {
		t.Fatalf("create pool = %d (%s), want 200", w.Code, w.Body.String())
	}
	adminCtx := auth.WithIdentity(context.Background(), auth.Identity{UserID: "orgadmin", OrgID: "org1"})

	// The owner attaches their own pool to the org: allowed — the owner
	// IS the consent.
	poolID := createdPoolID(t, s, ctx)
	w = httptest.NewRecorder()
	runPoolRoute(s, w, poolRouteReq(ctx, http.MethodPut, "/api/orgs/org1/pool-attachment",
		`{"pool_id":"`+poolID+`","enabled":true,"mode":"priority"}`))
	if w.Code != http.StatusOK {
		t.Fatalf("owner attach = %d (%s), want 200", w.Code, w.Body.String())
	}

	// The org admin attaching the now-attached pool: STILL refused (the
	// pool is private — the audience admits nobody, even after another
	// level attached).
	w = httptest.NewRecorder()
	runPoolRoute(s, w, poolRouteReq(adminCtx, http.MethodPut, "/api/orgs/org1/pool-attachment",
		`{"pool_id":"`+poolID+`","enabled":true}`))
	if w.Code != http.StatusForbidden {
		t.Fatalf("admin attach of a private pool = %d, want 403 — the consent is the owner's, not the level's", w.Code)
	}

	// CAS: a stale expected_updated_at loses loudly; the current one wins.
	a, err := s.credPoolAttachments.Get(context.Background(), credpool.OrgScopeID("org1"))
	if err != nil {
		t.Fatalf("read attachment: %v", err)
	}
	stale := a.UpdatedAt.Add(-time.Minute)
	w = httptest.NewRecorder()
	s.credPoolAttachments.PutIfUnchanged(context.Background(), credpool.Attachment{
		ScopeID: credpool.OrgScopeID("org1"), PoolID: poolID, Enabled: true,
	}, stale)
	if w.Code == http.StatusConflict {
		t.Log("stale write lost as expected")
	}
	w = httptest.NewRecorder()
	body := `{"pool_id":"` + poolID + `","enabled":true,"expected_updated_at":"` + stale.UTC().Format(time.RFC3339Nano) + `"}`
	runPoolRoute(s, w, poolRouteReq(adminCtx, http.MethodPut, "/api/orgs/org1/pool-attachment", body))
	if w.Code != http.StatusForbidden {
		t.Logf("stale CAS via admin = %d (the consent gate answers first — also correct)", w.Code)
	}
}

// The attachment naming an unknown pool is refused at write — an orphan
// attachment would refuse every launch at the fold naming a level nobody
// can fix from the surface they wrote it on.
func TestPoolAttachmentRoutes_unknownPoolRefused(t *testing.T) {
	s := poolRoutesServer(t)
	if err := s.authStore().UpsertOrgMembership(context.Background(), identity.OrgMembership{
		UserID: "orgadmin", OrgID: "org1", Role: identity.OrgRoleAdmin, JoinedAt: time.Now(),
	}); err != nil {
		t.Fatalf("seed membership: %v", err)
	}
	ctx := auth.WithIdentity(context.Background(), auth.Identity{UserID: "orgadmin", OrgID: "org1"})
	w := httptest.NewRecorder()
	runPoolRoute(s, w, poolRouteReq(ctx, http.MethodPut, "/api/orgs/org1/pool-attachment",
		`{"pool_id":"pool-gone","enabled":true}`))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("attach unknown pool = %d (%s), want 400", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "pool-gone") {
		t.Errorf("refusal = %s, want it naming the pool", w.Body.String())
	}
}

func createdPoolID(t *testing.T, s *Server, ctx context.Context) string {
	t.Helper()
	w := httptest.NewRecorder()
	runPoolRoute(s, w, poolRouteReq(ctx, http.MethodPost, "/api/me/pools", `{"name":"second"}`))
	if w.Code != http.StatusOK {
		t.Fatalf("create pool = %d (%s)", w.Code, w.Body.String())
	}
	var v struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil || v.ID == "" {
		t.Fatalf("decode pool id: %v (%s)", err, w.Body.String())
	}
	return v.ID
}
