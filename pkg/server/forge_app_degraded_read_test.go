package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/forge"
)

// errStoreDown is what a Mongo blip looks like from the store's caller.
var errStoreDown = errors.New("mongo: connection refused")

// degradedOAuthAppStore is a real store whose READS can be switched off — the
// only way to exercise the difference between "this tenant has no App" and
// "I could not find out".
type degradedOAuthAppStore struct {
	forge.OAuthAppStore
	down *atomic.Bool
}

func (d degradedOAuthAppStore) Get(ctx context.Context, id string) (forge.ForgeOAuthApp, error) {
	if d.down.Load() {
		return forge.ForgeOAuthApp{}, errStoreDown
	}
	return d.OAuthAppStore.Get(ctx, id)
}

func (d degradedOAuthAppStore) GetByInstance(ctx context.Context, tenantID string, p forge.Provider, baseURL string) (forge.ForgeOAuthApp, error) {
	if d.down.Load() {
		return forge.ForgeOAuthApp{}, errStoreDown
	}
	return d.OAuthAppStore.GetByInstance(ctx, tenantID, p, baseURL)
}

func (d degradedOAuthAppStore) ListByInstance(ctx context.Context, tenantID string, p forge.Provider, baseURL string) ([]forge.ForgeOAuthApp, error) {
	if d.down.Load() {
		return nil, errStoreDown
	}
	return d.OAuthAppStore.ListByInstance(ctx, tenantID, p, baseURL)
}

// newDegradableAppServer returns a server whose OAuth-app store can be pushed
// over, plus the switch.
func newDegradableAppServer(t *testing.T) (*Server, *atomic.Bool) {
	t.Helper()
	s := newForgeTestServer(t)
	down := &atomic.Bool{}
	s.forgeOAuthApps = degradedOAuthAppStore{OAuthAppStore: forge.NewMemoryOAuthAppStore(), down: down}
	return s, down
}

// #969 — "no App configured" and "the store could not answer" are different
// facts. Collapsing them into one `ok bool` makes a Mongo blip indistinguishable
// from a deliberate absence, and every caller then acts on the wrong one.
func TestGitHubAppConfig_DegradedReadIsNotAbsence(t *testing.T) {
	s, down := newDegradableAppServer(t)
	t0 := time.Unix(1700000000, 0).UTC()
	app := storeApp(t, s, "app-1", "t1", "SocialGouv", "111", "PEM-1", t0)
	ctx := context.Background()
	conn := forge.Connection{ID: "c1", TenantID: "t1", Kind: forge.KindGitHubApp, OAuthAppID: app.ID}

	// Sanity: healthy store resolves.
	if _, _, err := s.githubAppConfigForConnection(ctx, conn); err != nil {
		t.Fatalf("healthy read: %v", err)
	}

	down.Store(true)
	_, _, err := s.githubAppConfigForConnection(ctx, conn)
	if err == nil {
		t.Fatal("a store that cannot answer must not report a resolved App")
	}
	if errors.Is(err, errNoGitHubApp) {
		t.Error("a degraded read reported as errNoGitHubApp — a blip is being sold as a definite absence")
	}
	if !errors.Is(err, errStoreDown) {
		t.Errorf("the store's own error must survive for the log: %v", err)
	}
}

// A connection that genuinely names no App, on a healthy store, is a DEFINITE
// negative — the sentinel says so, and callers may act on it.
func TestGitHubAppConfig_GenuineAbsenceIsDefinite(t *testing.T) {
	s, _ := newDegradableAppServer(t)
	ctx := context.Background()
	conn := forge.Connection{ID: "c1", TenantID: "t-empty", Kind: forge.KindGitHubApp}

	_, _, err := s.githubAppConfigForConnection(ctx, conn)
	if !errors.Is(err, errNoGitHubApp) {
		t.Fatalf("a tenant with no App on a healthy store must answer errNoGitHubApp, got %v", err)
	}
}

// The dangerous half: when the tenant's OWN app cannot be read, falling
// through to the SHARED platform App swaps the signing identity behind the
// operator's back. The platform key can mint for ANY installation, so this is
// not a graceful degradation.
func TestGitHubAppConfigForTenant_DegradedReadDoesNotServeThePlatformApp(t *testing.T) {
	s, down := newDegradableAppServer(t)
	t0 := time.Unix(1700000000, 0).UTC()
	storeApp(t, s, "app-1", "t1", "SocialGouv", "111", "PEM-1", t0)
	// A configured platform App is exactly the tempting fallback.
	s.forgeGitHubApp = ForgeGitHubAppConfig{AppID: 42, PrivateKey: testAppKeyPEM(t), AppSlug: "iterion-forge-x"}
	ctx := context.Background()

	down.Store(true)
	cfg, shared, err := s.githubAppConfigForTenant(ctx, "t1")
	if err == nil {
		t.Fatalf("a degraded tenant read resolved to %+v (shared=%v) — the tenant's own App was simply not consulted", cfg, shared)
	}
	if shared {
		t.Error("the SHARED platform App was served for a tenant whose own App merely could not be read")
	}
	if errors.Is(err, errNoGitHubApp) {
		t.Error("a degraded read must not be reported as a definite absence")
	}
}

// The user-visible half of the same defect: a blip answered 502 Bad Gateway —
// blaming the forge for a failure that never opened a socket. forgeAdminFor is
// network-free in BOTH branches (the App client is constructed and cached, the
// token branch unseals), so 502 there is structurally wrong.
func TestForgeAdminFor_LocalFailureAnswers500Not502(t *testing.T) {
	s, down := newDegradableAppServer(t)
	t0 := time.Unix(1700000000, 0).UTC()
	app := storeApp(t, s, "app-1", "t1", "SocialGouv", "111", "PEM-1", t0)
	s.forgeConnections = forge.NewMemoryConnectionStore()
	conn := forge.Connection{
		ID: "conn1", TenantID: "t1", Provider: forge.ProviderGitHub,
		Kind: forge.KindGitHubApp, OAuthAppID: app.ID, InstallationID: 42,
	}
	if err := s.forgeConnections.Create(context.Background(), conn); err != nil {
		t.Fatal(err)
	}

	down.Store(true)
	w := httptest.NewRecorder()
	_, _, ok := s.connAdminFor(w, context.Background(), "t1", "conn1")
	if ok {
		t.Fatal("a degraded read must not yield an admin client")
	}
	if w.Code == http.StatusBadGateway {
		t.Error("answered 502 Bad Gateway — nothing reached the forge; forgeAdminFor is network-free")
	}
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 (iterion could not build its own client)", w.Code)
	}
}
