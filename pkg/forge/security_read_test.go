package forge

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/secrets"
)

func securityReadMap(t *testing.T, sealer secrets.Sealer, st secrets.GenericSecretStore, tenant string) map[string]string {
	t.Helper()
	gs, ok, err := findSecurityReadSecret(context.Background(), st, tenant)
	if err != nil {
		t.Fatalf("find secret: %v", err)
	}
	if !ok {
		t.Fatalf("no %s secret for tenant %s", SecurityReadSecretName, tenant)
	}
	plain, err := secrets.OpenGenericSecret(sealer, gs.ID, gs.SealedSecret)
	if err != nil {
		t.Fatalf("open secret: %v", err)
	}
	m := map[string]string{}
	if err := json.Unmarshal(plain, &m); err != nil {
		t.Fatalf("secret is not a JSON map: %v", err)
	}
	return m
}

func TestUpsertSecurityReadToken_CreatesThenMergesAcrossConnections(t *testing.T) {
	sealer, _ := secrets.NewAESGCMSealer(make([]byte, 32))
	st := secrets.NewMemoryGenericSecretStore()
	ctx := context.Background()
	now := time.Unix(1700000000, 0).UTC()

	connA := &Connection{ID: "c1", TenantID: "t1", Provider: ProviderGitHub, Kind: KindGitHubApp, AccountLogin: "SocialGouv"}
	connB := &Connection{ID: "c2", TenantID: "t1", Provider: ProviderGitHub, Kind: KindGitHubApp, AccountLogin: "DNUM-SocialGouv"}

	if err := UpsertSecurityReadToken(ctx, st, sealer, connA, "tok-a", now); err != nil {
		t.Fatalf("upsert A: %v", err)
	}
	if err := UpsertSecurityReadToken(ctx, st, sealer, connB, "tok-b", now); err != nil {
		t.Fatalf("upsert B: %v", err)
	}
	m := securityReadMap(t, sealer, st, "t1")
	// Keys are the org logins, lowercased so a bot config matches
	// case-insensitively.
	if m["socialgouv"] != "tok-a" || m["dnum-socialgouv"] != "tok-b" {
		t.Fatalf("map = %v, want both orgs", m)
	}

	// Rotation: a re-upsert for the same org replaces its token in place.
	if err := UpsertSecurityReadToken(ctx, st, sealer, connA, "tok-a2", now); err != nil {
		t.Fatalf("re-upsert A: %v", err)
	}
	if m := securityReadMap(t, sealer, st, "t1"); m["socialgouv"] != "tok-a2" || len(m) != 2 {
		t.Fatalf("after rotation map = %v", m)
	}

	// Egress lock: the created secret is pinned to the forge host.
	gs, _, _ := findSecurityReadSecret(ctx, st, "t1")
	if len(gs.AllowedHosts) != 1 || gs.AllowedHosts[0] != "github.com" {
		t.Fatalf("AllowedHosts = %v, want [github.com]", gs.AllowedHosts)
	}
	if gs.ScopeUserID != "" || gs.ScopeTeamID != "t1" {
		t.Fatalf("secret must be team-scoped: %+v", gs)
	}
}

func TestUpsertSecurityReadToken_RefusesNonJSONHandSetSecret(t *testing.T) {
	sealer, _ := secrets.NewAESGCMSealer(make([]byte, 32))
	st := secrets.NewMemoryGenericSecretStore()
	ctx := context.Background()

	// An operator hand-set the secret with a bare token instead of the JSON
	// map. The upsert must surface that explicitly — silently replacing an
	// operator's value is the forbidden move.
	id := secrets.NewGenericSecretID()
	sealed, _ := secrets.SealGenericSecret(sealer, id, []byte("ghp_not_a_json_map"))
	if err := st.Create(ctx, secrets.GenericSecret{
		ID: id, TenantID: "t1", ScopeTeamID: "t1", Name: SecurityReadSecretName, SealedSecret: sealed,
	}); err != nil {
		t.Fatal(err)
	}
	conn := &Connection{ID: "c1", TenantID: "t1", Provider: ProviderGitHub, Kind: KindGitHubApp, AccountLogin: "SocialGouv"}
	err := UpsertSecurityReadToken(ctx, st, sealer, conn, "tok", time.Now().UTC())
	if err == nil || !strings.Contains(err.Error(), "JSON map") {
		t.Fatalf("err = %v, want explicit JSON-map error", err)
	}
}

func TestRemoveSecurityReadToken_DropsEntryThenDeletesEmptiedSecret(t *testing.T) {
	sealer, _ := secrets.NewAESGCMSealer(make([]byte, 32))
	st := secrets.NewMemoryGenericSecretStore()
	ctx := context.Background()
	now := time.Now().UTC()

	connA := &Connection{ID: "c1", TenantID: "t1", Provider: ProviderGitHub, Kind: KindGitHubApp, AccountLogin: "OrgA"}
	connB := &Connection{ID: "c2", TenantID: "t1", Provider: ProviderGitHub, Kind: KindGitHubApp, AccountLogin: "OrgB"}
	if err := UpsertSecurityReadToken(ctx, st, sealer, connA, "tok-a", now); err != nil {
		t.Fatal(err)
	}
	if err := UpsertSecurityReadToken(ctx, st, sealer, connB, "tok-b", now); err != nil {
		t.Fatal(err)
	}

	if err := RemoveSecurityReadToken(ctx, st, sealer, connA); err != nil {
		t.Fatalf("remove A: %v", err)
	}
	m := securityReadMap(t, sealer, st, "t1")
	if _, ok := m["orga"]; ok || m["orgb"] != "tok-b" {
		t.Fatalf("after remove A map = %v", m)
	}

	// Removing the last entry deletes the secret outright: a leftover empty
	// map would read as "configured" to the bot's explicit-error gate.
	if err := RemoveSecurityReadToken(ctx, st, sealer, connB); err != nil {
		t.Fatalf("remove B: %v", err)
	}
	if _, ok, _ := findSecurityReadSecret(ctx, st, "t1"); ok {
		t.Fatal("secret should be deleted once the map empties")
	}

	// Idempotent on absence.
	if err := RemoveSecurityReadToken(ctx, st, sealer, connB); err != nil {
		t.Fatalf("remove on absent secret: %v", err)
	}
}

func seedGitHubAppConn(t *testing.T, sealer secrets.Sealer, connStore ConnectionStore, expiresAt time.Time, securityRead bool) Connection {
	t.Helper()
	blob, err := sealConnectionSecret(sealer, "conn-gh", connectionSecret{AccessToken: "old-install-token", ExpiresAt: expiresAt})
	if err != nil {
		t.Fatal(err)
	}
	exp := expiresAt
	c := Connection{
		ID: "conn-gh", TenantID: "t1", Provider: ProviderGitHub, Kind: KindGitHubApp,
		AccountLogin: "SocialGouv", InstallationID: 42,
		Status: StatusActive, SealedPayload: blob, AccessTokenExpiresAt: &exp,
		SecurityReadEnabled: securityRead,
	}
	if err := connStore.Create(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestRefreshWorker_MintsSecurityReadTokenAlongsideRefresh(t *testing.T) {
	sealer, _ := secrets.NewAESGCMSealer(make([]byte, 32))
	connStore := NewMemoryConnectionStore()
	secStore := secrets.NewMemoryGenericSecretStore()
	now := time.Unix(1700000000, 0).UTC()
	seedGitHubAppConn(t, sealer, connStore, now.Add(2*time.Minute), true)

	minted := 0
	w := &RefreshWorker{
		Connections: connStore,
		Secrets:     secStore,
		Sealer:      sealer,
		Now:         func() time.Time { return now },
		RefresherFor: func(Connection) TokenRefresher {
			return fakeRefresher{newAccess: "fresh-install-token", expiresAt: now.Add(time.Hour)}
		},
		SecurityMinter: func(_ context.Context, conn Connection) (string, error) {
			minted++
			if conn.ID != "conn-gh" {
				t.Fatalf("minter called for %s", conn.ID)
			}
			return "sec-read-token", nil
		},
	}
	if _, err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if minted != 1 {
		t.Fatalf("security minter calls = %d, want 1", minted)
	}
	m := securityReadMap(t, sealer, secStore, "t1")
	if m["socialgouv"] != "sec-read-token" {
		t.Fatalf("map = %v, want socialgouv → sec-read-token", m)
	}
}

func TestRefreshWorker_SecurityMinterSkippedWhenNotOptedIn(t *testing.T) {
	sealer, _ := secrets.NewAESGCMSealer(make([]byte, 32))
	connStore := NewMemoryConnectionStore()
	secStore := secrets.NewMemoryGenericSecretStore()
	now := time.Unix(1700000000, 0).UTC()
	seedGitHubAppConn(t, sealer, connStore, now.Add(2*time.Minute), false)

	w := &RefreshWorker{
		Connections: connStore,
		Secrets:     secStore,
		Sealer:      sealer,
		Now:         func() time.Time { return now },
		RefresherFor: func(Connection) TokenRefresher {
			return fakeRefresher{newAccess: "fresh", expiresAt: now.Add(time.Hour)}
		},
		SecurityMinter: func(context.Context, Connection) (string, error) {
			t.Fatal("security minter must not run for a connection that did not opt in")
			return "", nil
		},
	}
	if _, err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if _, ok, _ := findSecurityReadSecret(context.Background(), secStore, "t1"); ok {
		t.Fatal("no dependabot_tokens secret expected")
	}
}

func TestRefreshWorker_SecurityMintFailureSurfacesButKeepsRefresh(t *testing.T) {
	sealer, _ := secrets.NewAESGCMSealer(make([]byte, 32))
	connStore := NewMemoryConnectionStore()
	secStore := secrets.NewMemoryGenericSecretStore()
	now := time.Unix(1700000000, 0).UTC()
	seedGitHubAppConn(t, sealer, connStore, now.Add(2*time.Minute), true)

	w := &RefreshWorker{
		Connections: connStore,
		Secrets:     secStore,
		Sealer:      sealer,
		Now:         func() time.Time { return now },
		RefresherFor: func(Connection) TokenRefresher {
			return fakeRefresher{newAccess: "fresh-install-token", expiresAt: now.Add(time.Hour)}
		},
		SecurityMinter: func(context.Context, Connection) (string, error) {
			return "", ErrPermissionsNotGranted
		},
	}
	_, err := w.RunOnce(context.Background())
	// The failure must be VISIBLE (returned → warn-logged), never swallowed:
	// an hourly vuln-watch reading a dead map would otherwise fail with no
	// server-side trail.
	if err == nil || !strings.Contains(err.Error(), "security-read mint") {
		t.Fatalf("err = %v, want surfaced security-read mint error", err)
	}
	// … but the connection's own refresh already landed (canonical blob +
	// status), so the forge token is NOT held hostage by the security lane.
	conn, _ := connStore.Get(context.Background(), "conn-gh")
	if conn.Status != StatusActive {
		t.Fatalf("connection status = %s, want active", conn.Status)
	}
	sec, err2 := openConnectionSecret(sealer, conn.ID, conn.SealedPayload)
	if err2 != nil || sec.AccessToken != "fresh-install-token" {
		t.Fatalf("connection token = %q (err %v), want fresh-install-token", sec.AccessToken, err2)
	}
}

// ── Regressions from the adversarial review ──────────────────────────

func TestUpsertSecurityReadToken_PinsEgressOnAPreexistingSecret(t *testing.T) {
	sealer, _ := secrets.NewAESGCMSealer(make([]byte, 32))
	st := secrets.NewMemoryGenericSecretStore()
	ctx := context.Background()

	// The documented hand-set path: an operator creates the map with PATs
	// and no egress pin. The mint must not inherit "unrestricted" — an
	// unpinned credential map can be exfiltrated to any host.
	id := secrets.NewGenericSecretID()
	sealed, _ := secrets.SealGenericSecret(sealer, id, []byte(`{"other":"ghp_hand"}`))
	if err := st.Create(ctx, secrets.GenericSecret{
		ID: id, TenantID: "t1", ScopeTeamID: "t1", Name: SecurityReadSecretName,
		SealedSecret: sealed, Fingerprint: secrets.FingerprintSHA256(`{"other":"ghp_hand"}`),
	}); err != nil {
		t.Fatal(err)
	}
	conn := &Connection{ID: "c1", TenantID: "t1", Provider: ProviderGitHub, Kind: KindGitHubApp, AccountLogin: "acme"}
	if err := UpsertSecurityReadToken(ctx, st, sealer, conn, "ghs_tok", time.Now().UTC()); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	gs, _, _ := findSecurityReadSecret(ctx, st, "t1")
	if len(gs.AllowedHosts) != 1 || gs.AllowedHosts[0] != "github.com" {
		t.Fatalf("AllowedHosts = %v, want the forge host pinned on update too", gs.AllowedHosts)
	}
	// A second connection on ANOTHER host adds its own host rather than
	// being filed under a pin that does not cover it.
	ghes := &Connection{ID: "c2", TenantID: "t1", Provider: ProviderGitHub, Kind: KindGitHubApp,
		AccountLogin: "internal", ForgeBaseURL: "https://ghe.corp.example"}
	if err := UpsertSecurityReadToken(ctx, st, sealer, ghes, "ghs_ghes", time.Now().UTC()); err != nil {
		t.Fatalf("upsert ghes: %v", err)
	}
	gs, _, _ = findSecurityReadSecret(ctx, st, "t1")
	if len(gs.AllowedHosts) != 2 || gs.AllowedHosts[0] != "ghe.corp.example" || gs.AllowedHosts[1] != "github.com" {
		t.Fatalf("AllowedHosts = %v, want both forge hosts", gs.AllowedHosts)
	}
}

func TestRemoveSecurityReadToken_KeepsAnOperatorsHandSetSecret(t *testing.T) {
	sealer, _ := secrets.NewAESGCMSealer(make([]byte, 32))
	st := secrets.NewMemoryGenericSecretStore()
	ctx := context.Background()

	// Hand-set (CreatedBy is the operator, not iterion): emptying the map
	// must NOT destroy their secret — that would silently replace an
	// explicit choice, and they may be about to refill it.
	id := secrets.NewGenericSecretID()
	body := `{"acme":"ghp_hand"}`
	sealed, _ := secrets.SealGenericSecret(sealer, id, []byte(body))
	if err := st.Create(ctx, secrets.GenericSecret{
		ID: id, TenantID: "t1", ScopeTeamID: "t1", Name: SecurityReadSecretName,
		SealedSecret: sealed, Fingerprint: secrets.FingerprintSHA256(body), CreatedBy: "u-operator",
	}); err != nil {
		t.Fatal(err)
	}
	conn := &Connection{ID: "c1", TenantID: "t1", Provider: ProviderGitHub, Kind: KindGitHubApp, AccountLogin: "acme"}
	if err := RemoveSecurityReadToken(ctx, st, sealer, conn); err != nil {
		t.Fatalf("remove: %v", err)
	}
	gs, ok, _ := findSecurityReadSecret(ctx, st, "t1")
	if !ok {
		t.Fatal("an operator's hand-set secret must survive an emptied map")
	}
	plain, _ := secrets.OpenGenericSecret(sealer, gs.ID, gs.SealedSecret)
	if string(plain) != "{}" {
		t.Fatalf("plaintext = %s, want an explicit empty map", plain)
	}
}

// casConflictStore lets one write land between the caller's read and its
// write — the multi-replica interleaving (every replica runs a refresh
// worker) that a plain last-writer-wins Update turns into a lost update.
type casConflictStore struct {
	*secrets.MemoryGenericSecretStore
	inject func()
	fired  bool
}

func (s *casConflictStore) UpdateIfFingerprint(ctx context.Context, rec secrets.GenericSecret, expected string) error {
	if !s.fired && s.inject != nil {
		s.fired = true
		s.inject()
	}
	return s.MemoryGenericSecretStore.UpdateIfFingerprint(ctx, rec, expected)
}

func TestUpsertSecurityReadToken_ConcurrentWriteIsNotLost(t *testing.T) {
	sealer, _ := secrets.NewAESGCMSealer(make([]byte, 32))
	mem := secrets.NewMemoryGenericSecretStore()
	ctx := context.Background()
	now := time.Now().UTC()

	connA := &Connection{ID: "c1", TenantID: "t1", Provider: ProviderGitHub, Kind: KindGitHubApp, AccountLogin: "orgA"}
	connB := &Connection{ID: "c2", TenantID: "t1", Provider: ProviderGitHub, Kind: KindGitHubApp, AccountLogin: "orgB"}
	if err := UpsertSecurityReadToken(ctx, mem, sealer, connA, "tok-a", now); err != nil {
		t.Fatal(err)
	}

	st := &casConflictStore{MemoryGenericSecretStore: mem}
	// While this upsert is in flight, another replica files orgB.
	st.inject = func() {
		if err := UpsertSecurityReadToken(ctx, mem, sealer, connB, "tok-b", now); err != nil {
			t.Fatalf("concurrent writer: %v", err)
		}
	}
	if err := UpsertSecurityReadToken(ctx, st, sealer, connA, "tok-a2", now); err != nil {
		t.Fatalf("upsert under conflict: %v", err)
	}
	m := securityReadMap(t, sealer, mem, "t1")
	if m["orga"] != "tok-a2" || m["orgb"] != "tok-b" {
		t.Fatalf("map = %v — the concurrent write was lost (last-writer-wins)", m)
	}
}

func TestRefreshWorker_WithdrawsEntryWhenConnectionCannotRefresh(t *testing.T) {
	sealer, _ := secrets.NewAESGCMSealer(make([]byte, 32))
	connStore := NewMemoryConnectionStore()
	secStore := secrets.NewMemoryGenericSecretStore()
	now := time.Unix(1700000000, 0).UTC()
	conn := seedGitHubAppConn(t, sealer, connStore, now.Add(2*time.Minute), true)
	if err := UpsertSecurityReadToken(context.Background(), secStore, sealer, &conn, "ghs_live", now); err != nil {
		t.Fatal(err)
	}
	// The worker must scope the ctx itself: RunOnce sweeps every tenant with
	// a tenant-less ctx, and the Mongo store REFUSES a tenant-less read.
	// A plain memory store would hide that (it ignores the tenant), so the
	// production contract is what this test runs against.
	tenantScoped := tenantScopedSecretStore{secStore}
	// The connection degrades (a permanent permission mismatch): it can no
	// longer mint, and the map carries no expiry — leaving the entry means
	// the hourly bot reads a dying token with no server-side trace.
	conn.Status = StatusDegraded
	if err := connStore.Update(context.Background(), conn); err != nil {
		t.Fatal(err)
	}
	w := &RefreshWorker{
		Connections: connStore, Secrets: tenantScoped, Sealer: sealer,
		Now:          func() time.Time { return now },
		RefresherFor: func(Connection) TokenRefresher { return fakeRefresher{newAccess: "x", expiresAt: now.Add(time.Hour)} },
		SecurityMinter: func(context.Context, Connection) (string, error) {
			t.Fatal("a degraded connection must not be re-minted")
			return "", nil
		},
	}
	if _, err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if _, ok, _ := findSecurityReadSecret(context.Background(), secStore, "t1"); ok {
		t.Fatal("the stalled connection's entry must be withdrawn (map emptied → secret gone)")
	}
}
