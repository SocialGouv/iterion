package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
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
func poolRoutesServer(t *testing.T) (*Server, *secrets.MemoryApiKeyStore) {
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
	s.credPoolPledges = credpool.NewMemoryPledgeStore()
	poolRoutesAPIKeys := secrets.NewMemoryApiKeyStore()
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
	// The lender's own anthropic key, seeded so entry-put verification
	// passes and the re-homing test has a stored row to point at.
	own := secrets.ApiKey{
		ID: "key-jo-anthropic", TenantID: "team1", ScopeTeamID: "team1", ScopeUserID: "jo",
		Provider: secrets.ProviderAnthropic, Name: "jo's anthropic",
	}
	sealed, serr := secrets.SealAPIKey(sealer, own.ID, []byte("sk-jo-anthropic-0"))
	if serr != nil {
		t.Fatalf("seal jo's key: %v", serr)
	}
	own.SealedSecret = sealed
	if err := poolRoutesAPIKeys.Create(context.Background(), own); err != nil {
		t.Fatalf("seed jo's key: %v", err)
	}
	s.credPool = credpool.NewBroker(credpool.BrokerConfig{
		Pools:   pools,
		Pledges: s.credPoolPledges,
		Leases:  credpool.NewMemoryLeaseStore(),
		Ledger:  credpool.NewMemoryLedger(),
		OAuth:   secrets.NewMemoryOAuthStore(),
		APIKeys: poolRoutesAPIKeys,
		Sealer:  sealer,
		Logger:  iterlog.New(iterlog.LevelError, nil),
	})
	return s, poolRoutesAPIKeys
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
	if strings.HasSuffix(r.URL.Path, "/entries") && r.Method == http.MethodPut {
		r.SetPathValue("id", strings.SplitN(strings.TrimPrefix(r.URL.Path, "/api/pools/"), "/", 2)[0])
		s.handlePutPoolEntry(w, r)
		return
	}
	switch {
	case strings.HasSuffix(r.URL.Path, "/pools") && r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/me/"):
		s.handleCreateMyPool(w, r)
	case strings.HasSuffix(r.URL.Path, "/pool-attachment") && r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/orgs/"):
		s.handlePutOrgPoolAttachment(w, r)
	case strings.HasSuffix(r.URL.Path, "/pool-attachment") && r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/orgs/"):
		s.handleGetOrgPoolAttachment(w, r)
	case strings.HasSuffix(r.URL.Path, "/pool-attachment") && r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/teams/"):
		s.handlePutTeamPoolAttachment(w, r)
	case strings.HasSuffix(r.URL.Path, "/reset") && r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/api/pools/"):
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/pools/"), "/")
		r.SetPathValue("id", parts[0])
		pledge, _ := url.PathUnescape(parts[2])
		r.SetPathValue("pledge", pledge)
		s.handleResetPoolEntry(w, r)
	default:
		w.WriteHeader(http.StatusTeapot)
	}
}

// The org admin attaching a PRIVATE user pool is refused — the owner's
// consent is the second half of the authority (law 2: an attachment is an
// opt-in, never a takeover); the OWNER attaching their own pool answers,
// and a stale CAS write loses loudly.
func TestPoolAttachmentRoutes_consentAndCAS(t *testing.T) {
	s, _ := poolRoutesServer(t)
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
	s, _ := poolRoutesServer(t)
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

// Fix round (Revi 2026-11-09): the entry's LendScope is DERIVED from the
// pool's owner scope — a request-named scope (platform, a foreign
// orgtier) is ignored, or any pool manager could aim an entry at a
// foreign reserved scope and lend keys it was never granted.
func TestPoolEntryRequest_lendScopeIgnored(t *testing.T) {
	s, poolRoutesAPIKeys := poolRoutesServer(t)
	ctx := auth.WithIdentity(context.Background(), auth.Identity{UserID: "jo"})
	// jo creates a USER pool (private): its lender is personal — no
	// reserved scope. The request tries to aim the entry at the PLATFORM
	// sentinel's key rows.
	w := httptest.NewRecorder()
	runPoolRoute(s, w, poolRouteReq(ctx, http.MethodPost, "/api/me/pools", `{"name":"mine"}`))
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil || created.ID == "" {
		t.Fatalf("create pool: %v (%s)", err, w.Body.String())
	}
	sealer, err := secrets.NewAESGCMSealer(make([]byte, 32))
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	// jo's own personal key — VerifyEntry passes for it; the request then
	// LIES about the lend scope, naming the platform sentinel. The stored
	// row must carry the DERIVED scope (empty: personal), never the lie.
	own := secrets.ApiKey{
		ID: "key-jo-gw", TenantID: "team1", ScopeTeamID: "team1", ScopeUserID: "jo",
		Provider: secrets.ProviderOpenAICompatible, Name: "jo's key",
		BaseURL: "https://gw.example",
	}
	ownSealed, err := secrets.SealAPIKeyFor(sealer, own, []byte("sk-jo-gw-000"))
	if err != nil {
		t.Fatalf("seal jo's key: %v", err)
	}
	own.SealedSecret = ownSealed
	if err := poolRoutesAPIKeys.Create(context.Background(), own); err != nil {
		t.Fatalf("seed jo's key: %v", err)
	}

	// The entry names lend_scope "platform:" with jo's own key id (which
	// verifies): the stored row must carry the DERIVED scope (empty for a
	// user pool), never the request's.
	w = httptest.NewRecorder()
	body := `{"source":"api_key","ref":"openai_compatible","key_id":"key-jo-gw","lend_scope":"` + secrets.PlatformTenantID + `","model":"openai_compatible/x"}`
	runPoolRoute(s, w, poolRouteReq(ctx, http.MethodPut, "/api/pools/"+created.ID+"/entries", body))
	if w.Code != http.StatusOK {
		t.Fatalf("put entry = %d (%s), want 200", w.Code, w.Body.String())
	}
	pledges, err := s.credPoolPledges.ListByPool(context.Background(), created.ID)
	if err != nil || len(pledges) != 1 {
		t.Fatalf("pledges = (%d, %v), want the one stored entry", len(pledges), err)
	}
	if pledges[0].LendScope != "" {
		t.Errorf("stored LendScope = %q, want empty (derived: a user pool reads personal keys) — the request's %q must never reach a row", pledges[0].LendScope, secrets.PlatformTenantID)
	}
	if pledges[0].KeyID != "key-jo-gw" {
		t.Errorf("stored key id = %q, want the entry's own key", pledges[0].KeyID)
	}
}

// Fix round (Revi 2026-11-09): the schedule-lane pool block is validated
// AT RECORD-WRITE time — shape, the pool's existence and enablement, and
// the consent (an org pool only serves teams of its own org).
func TestValidateSchedulePoolBinding(t *testing.T) {
	s, _ := poolRoutesServer(t)
	ctx := context.Background()
	if err := s.credPoolPools.Upsert(ctx, credpool.Pool{ID: "pool-org", OrgID: "org1", Enabled: true}); err != nil {
		t.Fatalf("seed org pool: %v", err)
	}
	if err := s.credPoolPools.Upsert(ctx, credpool.Pool{ID: "pool-other", OrgID: "org9", Enabled: true}); err != nil {
		t.Fatalf("seed other pool: %v", err)
	}
	// A block naming no pool: refused.
	err := s.validatePoolBindingWrite(ctx, "team1", &credpool.Binding{PoolID: "", Enabled: true})
	if err == nil {
		t.Error("an empty pool block accepted")
	}
	// A block naming a vanished pool: refused, naming it.
	err = s.validatePoolBindingWrite(ctx, "team1", &credpool.Binding{PoolID: "pool-gone", Enabled: true})
	if err == nil || !strings.Contains(err.Error(), "pool-gone") {
		t.Errorf("vanished pool err = %v, want a refusal naming the pool", err)
	}
	// A block naming a DISABLED pool: refused.
	if err := s.credPoolPools.Upsert(ctx, credpool.Pool{ID: "pool-off", OrgID: "org1", Enabled: false}); err != nil {
		t.Fatalf("seed disabled pool: %v", err)
	}
	err = s.validatePoolBindingWrite(ctx, "team1", &credpool.Binding{PoolID: "pool-off", Enabled: true})
	if err == nil || !strings.Contains(err.Error(), "disabled") {
		t.Errorf("disabled pool err = %v, want a refusal naming the disablement", err)
	}
	// An ORG pool of ANOTHER org: refused — the consent is the org's own.
	err = s.validatePoolBindingWrite(ctx, "team1", &credpool.Binding{PoolID: "pool-other", Enabled: true})
	if err == nil || !strings.Contains(err.Error(), "org") {
		t.Errorf("foreign org pool err = %v, want a consent refusal", err)
	}
	// The team's own org pool: accepted.
	if err := s.validatePoolBindingWrite(ctx, "team1", &credpool.Binding{
		PoolID: "pool-org", Enabled: true, Strategy: credpool.Strategy{Mode: credpool.StrategyPriority},
	}); err != nil {
		t.Errorf("own org pool refused: %v", err)
	}
	// A garbage strategy inside the block: refused by shape validation.
	err = s.validatePoolBindingWrite(ctx, "team1", &credpool.Binding{
		PoolID: "pool-org", Enabled: true, Strategy: credpool.Strategy{Mode: "weighted"},
	})
	if err == nil {
		t.Error("a garbage strategy mode accepted")
	}
}

// Revi round 2 (Rac48bb + Ra08682): the platform pool's owner consents at
// the platform scope (super-admin), and a credential pledged to one pool
// cannot be silently re-homed to a second pool by the deterministic id.
func TestPoolAttachmentRoutes_platformPoolConsentsAtItsOwnScope(t *testing.T) {
	s, _ := poolRoutesServer(t)
	// A super-admin identity: isSuperAdmin rides the identity flag.
	adminCtx := auth.WithIdentity(context.Background(), auth.Identity{UserID: "root", IsSuperAdmin: true})
	// A platform pool created by the admin lane.
	w := httptest.NewRecorder()
	runPoolRoutePlatformCreate(s, w, poolRouteReq(adminCtx, http.MethodPost, "/api/admin/llm/pools", `{"name":"instance default"}`))
	if w.Code != http.StatusOK {
		t.Fatalf("platform create = %d (%s)", w.Code, w.Body.String())
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil || created.ID == "" {
		t.Fatalf("decode: %v", err)
	}
	// The SAME super-admin attaches it at the PLATFORM floor: allowed —
	// the write IS the consent.
	w = httptest.NewRecorder()
	s.handlePutPlatformPoolAttachment(w, poolRouteReq(adminCtx, http.MethodPut, "/api/admin/llm/pool-attachment",
		`{"pool_id":"`+created.ID+`","enabled":true}`))
	if w.Code != http.StatusOK {
		t.Fatalf("platform attach = %d (%s), want 200 — the instance floor is the platform pool's purpose", w.Code, w.Body.String())
	}
}

func runPoolRoutePlatformCreate(s *Server, w *httptest.ResponseRecorder, r *http.Request) {
	s.handleCreatePlatformPool(w, r)
}

// A credential already an entry of pool A cannot be re-homed to pool B by
// the deterministic pledge id — 409 naming the holding pool.
func TestPoolEntryRoutes_rehomingRefused(t *testing.T) {
	s, _ := poolRoutesServer(t)
	ctx := auth.WithIdentity(context.Background(), auth.Identity{UserID: "jo"})
	w := httptest.NewRecorder()
	runPoolRoute(s, w, poolRouteReq(ctx, http.MethodPost, "/api/me/pools", `{"name":"first"}`))
	var first struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &first)
	w = httptest.NewRecorder()
	runPoolRoute(s, w, poolRouteReq(ctx, http.MethodPost, "/api/me/pools", `{"name":"second"}`))
	var second struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &second)

	entry := `{"source":"api_key","ref":"anthropic","key_id":"key-jo-anthropic"}`
	w = httptest.NewRecorder()
	runPoolRouteEntryPut(s, w, ctx, first.ID, entry)
	if w.Code != http.StatusOK {
		t.Fatalf("first pool entry = %d (%s)", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	runPoolRouteEntryPut(s, w, ctx, second.ID, entry)
	if w.Code != http.StatusConflict {
		t.Fatalf("re-homing = %d (%s), want 409 naming the holding pool", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), first.ID) {
		t.Errorf("409 body = %s, want it naming the holding pool", w.Body.String())
	}
}

func runPoolRouteEntryPut(s *Server, w *httptest.ResponseRecorder, ctx context.Context, poolID, body string) {
	r := poolRouteReq(ctx, http.MethodPut, "/api/pools/"+poolID+"/entries", body)
	r.SetPathValue("id", poolID)
	s.handlePutPoolEntry(w, r)
}

// The manual reset lifts the entry's cooldown AND every reset_hold naming
// it (ADR-164, #3122): a run holding the entry re-ranks at its next resume.
// reset_hold is accepted at the attachment write now that the hold is real.
func TestPoolEntryReset_liftsTheCooldownAndTheHolds(t *testing.T) {
	s, _ := poolRoutesServer(t)
	ctx := auth.WithIdentity(context.Background(), auth.Identity{UserID: "jo"})
	w := httptest.NewRecorder()
	runPoolRoute(s, w, poolRouteReq(ctx, http.MethodPost, "/api/me/pools", `{"name":"held"}`))
	if w.Code != http.StatusOK {
		t.Fatalf("create pool = %d (%s)", w.Code, w.Body.String())
	}
	poolID := createdPoolID(t, s, ctx)

	// A held attachment writes (the dial was refused while its runtime half
	// was missing).
	w = httptest.NewRecorder()
	runPoolRoute(s, w, poolRouteReq(ctx, http.MethodPut, "/api/teams/team1/pool-attachment",
		`{"pool_id":"`+poolID+`","enabled":true,"mode":"priority","reset_hold":true}`))
	if w.Code != http.StatusOK {
		t.Fatalf("attach with reset_hold = %d (%s), want 200", w.Code, w.Body.String())
	}

	// The entry cools down and a run holds it.
	until := time.Now().Add(time.Hour)
	entry := credpool.Pledge{ID: "jo|api_key|anthropic", PoolID: poolID, UserID: "jo",
		Credential: credpool.Credential{Source: credpool.SourceAPIKey, Ref: "anthropic", KeyID: "key-jo-anthropic"},
		Enabled:    true, Health: credpool.HealthOK, CooldownUntil: &until}
	if err := s.credPoolPledges.Upsert(context.Background(), entry); err != nil {
		t.Fatal(err)
	}
	leases := credpool.NewMemoryLeaseStore()
	sealer, err := secrets.NewAESGCMSealer(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	s.credPool = credpool.NewBroker(credpool.BrokerConfig{
		Pools: s.credPoolPools, Pledges: s.credPoolPledges, Leases: leases, Ledger: credpool.NewMemoryLedger(),
		OAuth: secrets.NewMemoryOAuthStore(), APIKeys: secrets.NewMemoryApiKeyStore(), Sealer: sealer,
		Logger: iterlog.New(iterlog.LevelError, nil),
	})
	now := time.Now().UTC()
	if err := leases.Put(context.Background(), credpool.Lease{ID: "l1", RunID: "run-held", PledgeID: entry.ID, Credential: entry.Credential,
		Held: true, Closed: true, AcquiredAt: now, ExpiresAt: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}

	w = httptest.NewRecorder()
	runPoolRoute(s, w, poolRouteReq(ctx, http.MethodPost, "/api/pools/"+poolID+"/entries/"+url.PathEscape(entry.ID)+"/reset", `{}`))
	if w.Code != http.StatusOK {
		t.Fatalf("reset = %d (%s), want 200", w.Code, w.Body.String())
	}
	got, err := s.credPoolPledges.Get(context.Background(), entry.ID)
	if err != nil || got.CooldownUntil != nil {
		t.Fatalf("after reset the entry still cools: %+v (%v)", got.CooldownUntil, err)
	}
	if held, err := s.credPool.HeldPledge(context.Background(), "run-held", entry.Credential); err != nil || held != "" {
		t.Fatalf("after reset the run still holds %q (%v) — the reset must lift the holds naming the entry", held, err)
	}
}
