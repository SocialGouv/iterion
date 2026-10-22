package forge

import (
	"bytes"
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

// UpdateIfFingerprint is the method guardedUpdate PREFERS whenever the store
// implements it — which the embedded memory store now does. Without this
// override the promoted method bypasses the very tenant assertion this
// double exists to make.
func (s tenantScopedSecretStore) UpdateIfFingerprint(ctx context.Context, rec secrets.GenericSecret, expected string) error {
	if t, ok := store.TenantFromContext(ctx); !ok || t == "" {
		return errNoTenant
	}
	return s.MemoryGenericSecretStore.UpdateIfFingerprint(ctx, rec, expected)
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

// A refresh token the forge rotates is consumed by whoever refreshes first.
// A second sweep that loaded the connection before the winner wrote refreshes
// with the consumed token and is answered invalid_grant — ErrUnauthorized
// here. It must leave the row as the winner left it: active, with the
// winner's tokens and expiry. Without the re-read the loser marked the
// renewed connection needs_reauth and wrote its stale copy over the fresh
// tokens (#2682, PIC 2026-10-22).
func TestRefreshWorker_TheLoserOfARotatingRefreshRaceWritesNothing(t *testing.T) {
	sealer, _ := secrets.NewAESGCMSealer(make([]byte, 32))
	connStore := NewMemoryConnectionStore()
	secStore := secrets.NewMemoryGenericSecretStore()
	now := time.Unix(1700000000, 0).UTC()
	loaded, _ := seedOAuthConn(t, sealer, connStore, secStore, now.Add(time.Minute))

	// The winner's write lands first: new tokens, a later expiry, a refresh stamp.
	winnerBlob, err := sealConnectionSecret(sealer, loaded.ID, connectionSecret{AccessToken: "new-access", RefreshToken: "refresh-2", ExpiresAt: now.Add(2 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	won := loaded
	won.SealedPayload = winnerBlob
	exp := now.Add(2 * time.Hour)
	won.AccessTokenExpiresAt = &exp
	refreshedAt := now.Add(time.Second)
	won.LastRefreshedAt = &refreshedAt
	if err := connStore.Update(context.Background(), won); err != nil {
		t.Fatal(err)
	}

	w := &RefreshWorker{
		Connections: connStore, Secrets: secStore, Sealer: sealer,
		Now:          func() time.Time { return now.Add(2 * time.Second) },
		RefresherFor: func(Connection) TokenRefresher { return fakeRefresher{err: ErrUnauthorized} },
	}
	// The loser still holds the copy it loaded before the winner wrote.
	if err := w.refreshOne(context.Background(), loaded); err != nil {
		t.Fatalf("the loser must end quietly, got %v", err)
	}
	got, err := connStore.Get(context.Background(), loaded.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusActive {
		t.Fatalf("status = %q, want active: the loser marked revoked a connection the winner had just renewed", got.Status)
	}
	if !bytes.Equal(got.SealedPayload, winnerBlob) {
		t.Fatal("the loser overwrote the winner's tokens with its stale copy")
	}
	if got.AccessTokenExpiresAt == nil || !got.AccessTokenExpiresAt.Equal(exp) {
		t.Fatalf("expiry = %v, want the winner's %v", got.AccessTokenExpiresAt, exp)
	}
}

// An operator's write that leaves the credential alone — a webhook base pin,
// a display name — and lands between the sweep loading the connection and
// its revocation stays: the revoke sets only the fields it owns.
func TestRefreshWorker_ARevocationKeepsAnOperatorWriteThatLandedMeanwhile(t *testing.T) {
	sealer, _ := secrets.NewAESGCMSealer(make([]byte, 32))
	connStore := NewMemoryConnectionStore()
	secStore := secrets.NewMemoryGenericSecretStore()
	now := time.Unix(1700000000, 0).UTC()
	loaded, _ := seedOAuthConn(t, sealer, connStore, secStore, now.Add(time.Minute))

	pinned := loaded
	pinned.WebhookBaseURL = "https://iterion.fabrique.example"
	pinned.DisplayName = "jo"
	if err := connStore.Update(context.Background(), pinned); err != nil {
		t.Fatal(err)
	}
	w := &RefreshWorker{
		Connections: connStore, Secrets: secStore, Sealer: sealer,
		Now:          func() time.Time { return now.Add(time.Second) },
		RefresherFor: func(Connection) TokenRefresher { return fakeRefresher{err: ErrUnauthorized} },
	}
	if err := w.refreshOne(context.Background(), loaded); err != nil {
		t.Fatal(err)
	}
	got, _ := connStore.Get(context.Background(), loaded.ID)
	if got.Status != StatusNeedsReauth {
		t.Fatalf("status = %q, want needs_reauth: the forge really revoked this one", got.Status)
	}
	if got.WebhookBaseURL != "https://iterion.fabrique.example" || got.DisplayName != "jo" {
		t.Fatalf("the revoke reverted the operator's write: %+v", got)
	}
}

// ctxFailingStore fails its writes on a done context, as the Mongo driver does.
type ctxFailingStore struct {
	*MemoryConnectionStore
}

func (s ctxFailingStore) Update(ctx context.Context, c Connection) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.MemoryConnectionStore.Update(ctx, c)
}

// hookRefresher runs fn on Refresh — the hook a test uses to end the sweep's
// term at the instant the forge has already rotated.
type hookRefresher struct {
	fn func() (RefreshedToken, error)
}

func (h hookRefresher) Refresh(context.Context, Connection, string) (RefreshedToken, error) {
	return h.fn()
}

// The forge rotated and answered; the sweep's term ends before the row is
// written (step-down, lost lease, shutdown). The renewal still lands: the
// write runs on its own bounded context, so the forge and the row agree on
// the new refresh token — otherwise the next pass refreshed with the consumed
// one and marked a healthy connection revoked with nobody racing it.
func TestRefreshWorker_ATermEndingAfterTheForgeAnsweredStillWritesTheRotatedToken(t *testing.T) {
	sealer, _ := secrets.NewAESGCMSealer(make([]byte, 32))
	connStore := ctxFailingStore{NewMemoryConnectionStore()}
	secStore := secrets.NewMemoryGenericSecretStore()
	now := time.Unix(1700000000, 0).UTC()
	loaded, secID := seedOAuthConn(t, sealer, connStore, secStore, now.Add(time.Minute))

	term, endTerm := context.WithCancel(context.Background())
	defer endTerm()
	w := &RefreshWorker{
		Connections: connStore, Secrets: secStore, Sealer: sealer,
		Now: func() time.Time { return now },
		RefresherFor: func(Connection) TokenRefresher {
			return hookRefresher{fn: func() (RefreshedToken, error) {
				endTerm() // the forge has rotated; the term ends right now
				return RefreshedToken{AccessToken: "new-access", RefreshToken: "refresh-2", ExpiresAt: now.Add(2 * time.Hour)}, nil
			}}
		},
	}
	if err := w.refreshOne(term, loaded); err != nil {
		t.Fatalf("the renewal must finish on its own context, got %v", err)
	}
	got, _ := connStore.Get(context.Background(), loaded.ID)
	cur, err := openConnectionSecret(sealer, got.ID, got.SealedPayload)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusActive || cur.RefreshToken != "refresh-2" {
		t.Fatalf("row: status %q refresh token %q, want active with the rotated token", got.Status, cur.RefreshToken)
	}
	gs, _ := secStore.Get(store.WithTenant(context.Background(), "t1"), secID)
	if pt, _ := secrets.OpenGenericSecret(sealer, secID, gs.SealedSecret); string(pt) != "new-access" {
		t.Fatalf("managed secret = %q, want the new access token", string(pt))
	}
}

// A sweep whose term ended starts no further renewal: the second due
// connection is the successor's, whose term opens with a pass.
func TestRefreshWorker_ADoneTermStartsNoFurtherRenewal(t *testing.T) {
	sealer, _ := secrets.NewAESGCMSealer(make([]byte, 32))
	connStore := NewMemoryConnectionStore()
	secStore := secrets.NewMemoryGenericSecretStore()
	now := time.Unix(1700000000, 0).UTC()
	seedOAuthConn(t, sealer, connStore, secStore, now.Add(time.Minute))
	blob, err := sealConnectionSecret(sealer, "conn-2", connectionSecret{AccessToken: "a2", RefreshToken: "r2", ExpiresAt: now.Add(time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	exp := now.Add(time.Minute)
	if err := connStore.Create(context.Background(), Connection{ID: "conn-2", TenantID: "t1", Provider: ProviderGitLab, Kind: KindOAuthApp, Status: StatusActive, SealedPayload: blob, AccessTokenExpiresAt: &exp}); err != nil {
		t.Fatal(err)
	}

	term, endTerm := context.WithCancel(context.Background())
	defer endTerm()
	calls := 0
	w := &RefreshWorker{
		Connections: connStore, Secrets: secStore, Sealer: sealer,
		Now: func() time.Time { return now },
		RefresherFor: func(Connection) TokenRefresher {
			return hookRefresher{fn: func() (RefreshedToken, error) {
				calls++
				endTerm()
				return RefreshedToken{AccessToken: "fresh", ExpiresAt: now.Add(2 * time.Hour)}, nil
			}}
		},
	}
	refreshed, err := w.RunOnce(term)
	if err != nil {
		t.Fatalf("a term ending between connections is not an error, got %v", err)
	}
	if refreshed != 1 || calls != 1 {
		t.Fatalf("refreshed=%d forge calls=%d, want 1 and 1: the first renewal finishes, the second waits for the successor", refreshed, calls)
	}
}

// racingStore lands the winner's write immediately before the loser's FIRST
// write, whatever the method: the interleaving a re-read cannot see (the
// loser's Get, if any, already happened). Only a write that carries its own
// condition survives it.
type racingStore struct {
	*MemoryConnectionStore
	winner func()
	once   bool
}

func (s *racingStore) beforeWrite() {
	if !s.once && s.winner != nil {
		s.once = true
		s.winner()
	}
}

func (s *racingStore) Update(ctx context.Context, c Connection) error {
	s.beforeWrite()
	return s.MemoryConnectionStore.Update(ctx, c)
}

func (s *racingStore) RevokeIfPayload(ctx context.Context, id string, expected []byte, at time.Time, clearExpiry bool) (bool, error) {
	s.beforeWrite()
	return s.MemoryConnectionStore.RevokeIfPayload(ctx, id, expected, at, clearExpiry)
}

// The winner's renewal lands between the loser's last read and its write.
// A loser that re-reads then writes blindly marks the renewed connection
// revoked and overwrites the fresh tokens; the conditional write matches
// nothing and the row stays the winner's.
func TestRefreshWorker_AWinnerLandingRightBeforeTheLosersWriteStillWins(t *testing.T) {
	sealer, _ := secrets.NewAESGCMSealer(make([]byte, 32))
	inner := NewMemoryConnectionStore()
	secStore := secrets.NewMemoryGenericSecretStore()
	now := time.Unix(1700000000, 0).UTC()
	loaded, _ := seedOAuthConn(t, sealer, inner, secStore, now.Add(time.Minute))
	winnerBlob, err := sealConnectionSecret(sealer, loaded.ID, connectionSecret{AccessToken: "new-access", RefreshToken: "refresh-2", ExpiresAt: now.Add(2 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	st := &racingStore{MemoryConnectionStore: inner}
	st.winner = func() {
		won := loaded
		won.SealedPayload = winnerBlob
		exp := now.Add(2 * time.Hour)
		won.AccessTokenExpiresAt = &exp
		at := now.Add(time.Second)
		won.LastRefreshedAt = &at
		if err := inner.Update(context.Background(), won); err != nil {
			t.Error(err)
		}
	}
	w := &RefreshWorker{
		Connections: st, Secrets: secStore, Sealer: sealer,
		Now:          func() time.Time { return now.Add(2 * time.Second) },
		RefresherFor: func(Connection) TokenRefresher { return fakeRefresher{err: ErrUnauthorized} },
	}
	if err := w.refreshOne(context.Background(), loaded); err != nil {
		t.Fatalf("the loser must end quietly, got %v", err)
	}
	got, _ := inner.Get(context.Background(), loaded.ID)
	cur, err := openConnectionSecret(sealer, got.ID, got.SealedPayload)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusActive || cur.RefreshToken != "refresh-2" {
		t.Fatalf("row: status %q refresh token %q, want the winner's active row with refresh-2 — the loser's write must carry its own condition, a re-read cannot see a winner landing after it", got.Status, cur.RefreshToken)
	}
}
