package forge

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// tenantScopedSecretStore mirrors the Mongo generic-secret store's contract:
// Get/Update require the tenant on the ctx (the memory store does not, which is
// why it never caught the refresh worker passing a tenant-less ctx). Seeding
// via Create is left unchecked so tests can populate it with a plain ctx.
type tenantScopedSecretStore struct {
	*secrets.MemoryGenericSecretStore
}

var errNoTenant = errors.New("test store: tenant required on ctx")

func (s tenantScopedSecretStore) Get(ctx context.Context, id string) (secrets.GenericSecret, error) {
	if t, ok := store.TenantFromContext(ctx); !ok || t == "" {
		return secrets.GenericSecret{}, errNoTenant
	}
	return s.MemoryGenericSecretStore.Get(ctx, id)
}

func (s tenantScopedSecretStore) Update(ctx context.Context, rec secrets.GenericSecret) error {
	if t, ok := store.TenantFromContext(ctx); !ok || t == "" {
		return errNoTenant
	}
	return s.MemoryGenericSecretStore.Update(ctx, rec)
}

// ListByTeam and Delete mirror the same Mongo contract
// (withGenericSecretTenantFilter refuses a tenant-less ctx). Without them a
// caller that forgets to scope its ctx passes here and fails in production
// — which is exactly how the security-read withdrawal shipped broken.
func (s tenantScopedSecretStore) ListByTeam(ctx context.Context, teamID, userID string) ([]secrets.GenericSecret, error) {
	if t, ok := store.TenantFromContext(ctx); !ok || t == "" {
		return nil, errNoTenant
	}
	return s.MemoryGenericSecretStore.ListByTeam(ctx, teamID, userID)
}

func (s tenantScopedSecretStore) Delete(ctx context.Context, id string) error {
	if t, ok := store.TenantFromContext(ctx); !ok || t == "" {
		return errNoTenant
	}
	return s.MemoryGenericSecretStore.Delete(ctx, id)
}

type fakeRefresher struct {
	newAccess string
	expiresAt time.Time
	err       error
}

func (f fakeRefresher) Refresh(context.Context, Connection, string) (RefreshedToken, error) {
	if f.err != nil {
		return RefreshedToken{}, f.err
	}
	return RefreshedToken{AccessToken: f.newAccess, ExpiresAt: f.expiresAt}, nil
}

func seedOAuthConn(t *testing.T, sealer secrets.Sealer, connStore ConnectionStore, secStore secrets.GenericSecretStore, expiresAt time.Time) (Connection, string) {
	t.Helper()
	ctx := context.Background()
	const connID = "conn-oauth"
	// managed secret holds the OLD access token.
	secID := secrets.NewGenericSecretID()
	oldSealed, err := secrets.SealGenericSecret(sealer, secID, []byte("old-access"))
	if err != nil {
		t.Fatal(err)
	}
	if err := secStore.Create(ctx, secrets.GenericSecret{ID: secID, TenantID: "t1", ScopeTeamID: "t1", Name: "forge_gitlab_x", SealedSecret: oldSealed}); err != nil {
		t.Fatal(err)
	}
	blob, err := sealConnectionSecret(sealer, connID, connectionSecret{AccessToken: "old-access", RefreshToken: "refresh-1", ExpiresAt: expiresAt})
	if err != nil {
		t.Fatal(err)
	}
	exp := expiresAt
	c := Connection{
		ID: connID, TenantID: "t1", Provider: ProviderGitLab, Kind: KindOAuthApp,
		Status: StatusActive, SealedPayload: blob, AccessTokenExpiresAt: &exp,
		ManagedSecretID: secID,
	}
	if err := connStore.Create(ctx, c); err != nil {
		t.Fatal(err)
	}
	return c, secID
}

func TestRefreshWorker_RotatesTokenAndSecret(t *testing.T) {
	sealer, _ := secrets.NewAESGCMSealer(make([]byte, 32))
	connStore := NewMemoryConnectionStore()
	secStore := secrets.NewMemoryGenericSecretStore()
	now := time.Unix(1700000000, 0).UTC()
	// token expires in 2 minutes → inside the 5m lead window.
	_, secID := seedOAuthConn(t, sealer, connStore, secStore, now.Add(2*time.Minute))

	newExpiry := now.Add(time.Hour)
	w := &RefreshWorker{
		Connections: connStore,
		Secrets:     secStore,
		Sealer:      sealer,
		Now:         func() time.Time { return now },
		RefresherFor: func(Connection) TokenRefresher {
			return fakeRefresher{newAccess: "new-access", expiresAt: newExpiry}
		},
	}
	n, err := w.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if n != 1 {
		t.Fatalf("refreshed = %d, want 1", n)
	}

	// connection blob now holds the new access token + bumped expiry.
	conn, _ := connStore.Get(context.Background(), "conn-oauth")
	sec, err := openConnectionSecret(sealer, conn.ID, conn.SealedPayload)
	if err != nil {
		t.Fatal(err)
	}
	if sec.AccessToken != "new-access" {
		t.Errorf("connection access token = %q, want new-access", sec.AccessToken)
	}
	if conn.AccessTokenExpiresAt == nil || !conn.AccessTokenExpiresAt.Equal(newExpiry) {
		t.Errorf("expiry not bumped: %v", conn.AccessTokenExpiresAt)
	}
	if conn.Status != StatusActive {
		t.Errorf("status = %q, want active", conn.Status)
	}

	// managed secret plaintext rewritten to the new token.
	gs, _ := secStore.Get(context.Background(), secID)
	pt, err := secrets.OpenGenericSecret(sealer, secID, gs.SealedSecret)
	if err != nil || string(pt) != "new-access" {
		t.Errorf("managed secret = %q (err %v), want new-access", string(pt), err)
	}
}

// TestRefreshWorker_RewritesManagedSecretWithTenantScopedStore pins the fix for
// the tenant-context bug: the Mongo managed-secret store requires the tenant on
// the ctx, but RunOnce iterates connections cross-tenant. If refreshOne doesn't
// scope the ctx to the connection's tenant, the token mint succeeds and the
// connection updates (keyed on _id) while the managed secret is NEVER rewritten
// — leaving bot runs reading a stale, expired token (HTTP 401 in prod). This
// uses a tenant-asserting store so the memory store's laxness can't hide it.
func TestRefreshWorker_RewritesManagedSecretWithTenantScopedStore(t *testing.T) {
	sealer, _ := secrets.NewAESGCMSealer(make([]byte, 32))
	connStore := NewMemoryConnectionStore()
	secStore := tenantScopedSecretStore{secrets.NewMemoryGenericSecretStore()}
	now := time.Unix(1700000000, 0).UTC()
	_, secID := seedOAuthConn(t, sealer, connStore, secStore, now.Add(2*time.Minute))

	newExpiry := now.Add(time.Hour)
	w := &RefreshWorker{
		Connections: connStore,
		Secrets:     secStore,
		Sealer:      sealer,
		Now:         func() time.Time { return now },
		RefresherFor: func(Connection) TokenRefresher {
			return fakeRefresher{newAccess: "fresh-token", expiresAt: newExpiry}
		},
	}
	if _, err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v (the refresh must scope the ctx to the connection tenant)", err)
	}
	gs, err := secStore.Get(store.WithTenant(context.Background(), "t1"), secID)
	if err != nil {
		t.Fatalf("get managed secret: %v", err)
	}
	pt, err := secrets.OpenGenericSecret(sealer, secID, gs.SealedSecret)
	if err != nil || string(pt) != "fresh-token" {
		t.Errorf("managed secret = %q (err %v), want fresh-token — refresh did not rewrite it", string(pt), err)
	}
}

func TestRefreshWorker_UnauthorizedMarksNeedsReauth(t *testing.T) {
	sealer, _ := secrets.NewAESGCMSealer(make([]byte, 32))
	connStore := NewMemoryConnectionStore()
	secStore := secrets.NewMemoryGenericSecretStore()
	now := time.Unix(1700000000, 0).UTC()
	_, secID := seedOAuthConn(t, sealer, connStore, secStore, now.Add(time.Minute))

	w := &RefreshWorker{
		Connections: connStore, Secrets: secStore, Sealer: sealer,
		Now:          func() time.Time { return now },
		RefresherFor: func(Connection) TokenRefresher { return fakeRefresher{err: ErrUnauthorized} },
	}
	if _, err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce should swallow per-conn revoke, got %v", err)
	}
	conn, _ := connStore.Get(context.Background(), "conn-oauth")
	if conn.Status != StatusNeedsReauth {
		t.Errorf("status = %q, want needs_reauth", conn.Status)
	}
	// managed secret untouched on a revoke.
	gs, _ := secStore.Get(context.Background(), secID)
	pt, _ := secrets.OpenGenericSecret(sealer, secID, gs.SealedSecret)
	if string(pt) != "old-access" {
		t.Errorf("managed secret should be untouched on revoke, got %q", string(pt))
	}
}

// countingRefresher records how many times Refresh is invoked so a test can
// assert the worker stops re-minting a terminally-failed connection.
type countingRefresher struct {
	calls *int
	err   error
}

func (c countingRefresher) Refresh(context.Context, Connection, string) (RefreshedToken, error) {
	*c.calls++
	return RefreshedToken{}, c.err
}

// A 422 "permissions not granted" is a PERMANENT config mismatch: the worker
// must mark the connection degraded (with an actionable reason) and STOP
// re-minting it every tick, rather than returning an error that re-logs each
// cycle. Mirrors the ErrUnauthorized → needs_reauth test.
func TestRefreshWorker_PermissionsNotGrantedMarksDegradedAndStops(t *testing.T) {
	sealer, _ := secrets.NewAESGCMSealer(make([]byte, 32))
	connStore := NewMemoryConnectionStore()
	secStore := secrets.NewMemoryGenericSecretStore()
	now := time.Unix(1700000000, 0).UTC()
	_, secID := seedOAuthConn(t, sealer, connStore, secStore, now.Add(time.Minute))

	calls := 0
	w := &RefreshWorker{
		Connections: connStore, Secrets: secStore, Sealer: sealer,
		Now: func() time.Time { return now },
		RefresherFor: func(Connection) TokenRefresher {
			return countingRefresher{calls: &calls, err: fmt.Errorf("mint: %w", ErrPermissionsNotGranted)}
		},
	}
	// First tick: mint 422s → mark degraded, swallowed (no worker error).
	if _, err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce should swallow the terminal degrade, got %v", err)
	}
	if calls != 1 {
		t.Fatalf("first tick Refresh calls = %d, want 1", calls)
	}
	conn, _ := connStore.Get(context.Background(), "conn-oauth")
	if conn.Status != StatusDegraded {
		t.Errorf("status = %q, want degraded", conn.Status)
	}
	if conn.StatusReason == "" {
		t.Error("StatusReason must record the actionable remediation once")
	}
	// managed secret untouched on a degrade (same as revoke).
	gs, _ := secStore.Get(context.Background(), secID)
	pt, _ := secrets.OpenGenericSecret(sealer, secID, gs.SealedSecret)
	if string(pt) != "old-access" {
		t.Errorf("managed secret should be untouched on degrade, got %q", string(pt))
	}
	// Second tick: the still-expired connection is re-scanned, but a degraded
	// connection must NOT be re-minted — no additional Refresh call, no re-log.
	if _, err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("second RunOnce: %v", err)
	}
	if calls != 1 {
		t.Errorf("degraded connection re-minted: Refresh calls = %d, want 1 (stop re-minting)", calls)
	}
}

func TestRefreshWorker_SkipsPAT(t *testing.T) {
	sealer, _ := secrets.NewAESGCMSealer(make([]byte, 32))
	connStore := NewMemoryConnectionStore()
	now := time.Unix(1700000000, 0).UTC()
	// A PAT connection with an expiry in-window must NOT be returned by the
	// expiry scan (PATs don't refresh).
	blob, _ := sealConnectionSecret(sealer, "conn-pat", connectionSecret{PATToken: "pat"})
	exp := now.Add(time.Minute)
	_ = connStore.Create(context.Background(), Connection{ID: "conn-pat", TenantID: "t1", Provider: ProviderGitLab, Kind: KindPAT, SealedPayload: blob, AccessTokenExpiresAt: &exp})

	w := &RefreshWorker{Connections: connStore, Secrets: secrets.NewMemoryGenericSecretStore(), Sealer: sealer, Now: func() time.Time { return now }}
	n, err := w.RunOnce(context.Background())
	if err != nil || n != 0 {
		t.Errorf("PAT must be skipped: n=%d err=%v", n, err)
	}
}
