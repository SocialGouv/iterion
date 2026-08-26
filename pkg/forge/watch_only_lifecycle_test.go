package forge

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/secrets"
)

type lifecycleHarness struct {
	sealer  secrets.Sealer
	conns   *MemoryConnectionStore
	secrets secrets.GenericSecretStore
	now     time.Time
	worker  *RefreshWorker
}

func newLifecycleHarness(t *testing.T, optedIn bool, minter func(context.Context, Connection) (string, time.Time, error)) *lifecycleHarness {
	t.Helper()
	sealer, _ := secrets.NewAESGCMSealer(make([]byte, 32))
	h := &lifecycleHarness{
		sealer:  sealer,
		conns:   NewMemoryConnectionStore(),
		secrets: secrets.NewMemoryGenericSecretStore(),
		now:     time.Unix(1700000000, 0).UTC(),
	}
	seedWatchOnlyConn(t, sealer, h.conns, h.now.Add(2*time.Minute), optedIn)
	h.worker = &RefreshWorker{
		Connections:    h.conns,
		Secrets:        h.secrets,
		Sealer:         sealer,
		Now:            func() time.Time { return h.now },
		RefresherFor:   func(Connection) TokenRefresher { return fakeRefresher{} },
		SecurityMinter: minter,
	}
	return h
}

func (h *lifecycleHarness) conn(t *testing.T) Connection {
	t.Helper()
	c, err := h.conns.Get(context.Background(), "conn-gh")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func (h *lifecycleHarness) mapHas(t *testing.T, org string) bool {
	t.Helper()
	gs, ok, err := findSecurityReadSecret(context.Background(), h.secrets, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		return false
	}
	plain, err := secrets.OpenGenericSecret(h.sealer, gs.ID, gs.SealedSecret)
	if err != nil {
		t.Fatal(err)
	}
	m, err := securityReadTokens(plain)
	if err != nil {
		t.Fatal(err)
	}
	return m[org] != ""
}

// The runtime lane calls markRevoked on a 401 (rotated App key, suspended
// installation) and withdraws the entry. Without the same classification the
// watch-only connection keeps reading "active" while the map holds a token
// that dies within the hour — a 401 in the bot with nothing to explain it.
func TestWatchOnly_UnauthorizedMintRevokesAndWithdraws(t *testing.T) {
	h := newLifecycleHarness(t, true, func(context.Context, Connection) (string, time.Time, error) {
		return "", time.Time{}, fmt.Errorf("mint: %w", ErrUnauthorized)
	})
	conn := h.conn(t)
	if err := UpsertSecurityReadToken(context.Background(), h.secrets, h.sealer, &conn, "live-token", h.now); err != nil {
		t.Fatal(err)
	}
	if _, err := h.worker.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if got := h.conn(t).Status; got != StatusNeedsReauth {
		t.Fatalf("status = %q, want needs_reauth", got)
	}
	if h.mapHas(t, "socialgouv") {
		t.Fatal("a revoked connection left its dead token in the map")
	}
}

// A malformed operator-written map can never be fixed by retrying. Left
// unclassified it re-mints against GitHub on every tick, forever, and the
// connection never leaves the sweep because it is dated only on success.
func TestWatchOnly_PermanentUpsertFailureDegradesInsteadOfLooping(t *testing.T) {
	mints := 0
	h := newLifecycleHarness(t, true, nil)
	h.worker.SecurityMinter = func(context.Context, Connection) (string, time.Time, error) {
		mints++
		return "tok", h.now.Add(time.Hour), nil
	}
	// A hand-set bare PAT where the contract wants a JSON map.
	id := secrets.NewGenericSecretID()
	sealed, _ := secrets.SealGenericSecret(h.sealer, id, []byte("ghp_not_a_map"))
	if err := h.secrets.Create(context.Background(), secrets.GenericSecret{
		ID: id, TenantID: "t1", ScopeTeamID: "t1", Name: SecurityReadSecretName,
		SealedSecret: sealed, CreatedAt: h.now,
	}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 6; i++ {
		_, _ = h.worker.RunOnce(context.Background())
		h.now = h.now.Add(10 * time.Minute)
	}
	if mints > 1 {
		t.Fatalf("mints = %d over six ticks — a permanent upsert failure is re-minting forever", mints)
	}
	if got := h.conn(t); got.SecurityReadEnabled {
		t.Fatalf("opt-in still on after a permanent upsert failure (reason %q)", got.StatusReason)
	}
}

// The opt-out branch documents withdrawing the entry it still owns. Guarding
// that withdrawal on the very flag whose absence got us here made the whole
// branch a no-op.
func TestWatchOnly_OptedOutWithdrawsAndLeavesTheSweep(t *testing.T) {
	h := newLifecycleHarness(t, false, func(context.Context, Connection) (string, time.Time, error) {
		t.Fatal("the minter must not run for an opted-out connection")
		return "", time.Time{}, nil
	})
	// An entry survives from when the opt-in was on.
	conn := h.conn(t)
	conn.SecurityReadEnabled = true
	if err := UpsertSecurityReadToken(context.Background(), h.secrets, h.sealer, &conn, "stale-token", h.now); err != nil {
		t.Fatal(err)
	}
	if _, err := h.worker.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if h.mapHas(t, "socialgouv") {
		t.Fatal("the opt-out path did NOT withdraw the entry it documents withdrawing")
	}
	if exp := h.conn(t).AccessTokenExpiresAt; exp != nil {
		t.Fatalf("AccessTokenExpiresAt = %v, want nil — an idle connection must leave the sweep", exp)
	}
	due, err := h.conns.ExpiringBefore(context.Background(), h.now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 0 {
		t.Fatalf("still due after parking: %d connection(s)", len(due))
	}
}

// A transient mint failure keeps retrying, but the map carries no expiry of
// its own: once the expiry we recorded has passed, the token in it is dead and
// must stop being advertised as fresh.
func TestWatchOnly_TransientFailureWithdrawsAnExpiredEntry(t *testing.T) {
	h := newLifecycleHarness(t, true, func(context.Context, Connection) (string, time.Time, error) {
		return "", time.Time{}, errors.New("github: 502 bad gateway")
	})
	conn := h.conn(t)
	if err := UpsertSecurityReadToken(context.Background(), h.secrets, h.sealer, &conn, "live-token", h.now); err != nil {
		t.Fatal(err)
	}
	h.now = h.now.Add(10 * time.Minute) // past the recorded expiry
	if _, err := h.worker.RunOnce(context.Background()); err == nil {
		t.Fatal("a transient mint failure must surface as an error")
	}
	if h.mapHas(t, "socialgouv") {
		t.Fatal("an expired token is still advertised in the map")
	}
}

// The worker reads the connection at the top of the sweep and writes it back
// after the mint. An operator disabling the opt-in in that window would see
// their choice resurrected — and the token they asked to withdraw re-uploaded.
func TestWatchOnly_ConcurrentDisableIsNotResurrected(t *testing.T) {
	h := newLifecycleHarness(t, true, nil)
	h.worker.SecurityMinter = func(context.Context, Connection) (string, time.Time, error) {
		// The operator's PATCH lands while the mint is in flight.
		c := h.conn(t)
		c.SecurityReadEnabled = false
		if err := h.conns.Update(context.Background(), c); err != nil {
			t.Fatal(err)
		}
		return "tok", h.now.Add(time.Hour), nil
	}
	if _, err := h.worker.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if h.conn(t).SecurityReadEnabled {
		t.Fatal("the worker resurrected an operator's disable (lost update)")
	}
	if h.mapHas(t, "socialgouv") {
		t.Fatal("the worker re-uploaded a token the operator had just withdrawn")
	}
}

// The health probe rewrites InstallationAccount from live truth, so a GitHub
// org rename moves the derived key. Withdrawal must reach the key the token
// was actually FILED under, or the entry is stranded forever: the map never
// empties, the secret is never deleted, and a disconnect leaves a live token
// readable by every bot of the team.
func TestWatchOnly_WithdrawalReachesTheRecordedKeyAfterAnOrgRename(t *testing.T) {
	h := newLifecycleHarness(t, true, func(context.Context, Connection) (string, time.Time, error) {
		return "tok", time.Time{}, nil
	})
	conn := h.conn(t)
	if err := UpsertSecurityReadToken(context.Background(), h.secrets, h.sealer, &conn, "tok", h.now); err != nil {
		t.Fatal(err)
	}
	if conn.SecurityReadOrgKey != "socialgouv" {
		t.Fatalf("SecurityReadOrgKey = %q, want the key it filed under", conn.SecurityReadOrgKey)
	}
	conn.InstallationAccount = "SocialGouv-Renamed" // renamed on GitHub
	if err := RemoveSecurityReadToken(context.Background(), h.secrets, h.sealer, &conn); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if h.mapHas(t, "socialgouv") {
		t.Fatal("the pre-rename entry was stranded — no withdrawal path can reach it")
	}
}

// A minter that reports no expiry must not leave the connection undated: an
// undated connection reads as "never expires" to the sweep on one side, or is
// re-minted every tick on the other.
func TestWatchOnly_ZeroMintExpiryStillDatesTheConnection(t *testing.T) {
	h := newLifecycleHarness(t, true, func(context.Context, Connection) (string, time.Time, error) {
		return "tok", time.Time{}, nil
	})
	if _, err := h.worker.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	// Asserting merely "in the future" would be satisfied by the expiry the
	// connection was SEEDED with (now+2m) — the test would pass against its
	// own defect. Assert the default was actually applied.
	exp := h.conn(t).AccessTokenExpiresAt
	if exp == nil {
		t.Fatal("AccessTokenExpiresAt = nil — an undated connection reads as never-expiring")
	}
	if want := h.now.Add(time.Hour); !exp.Equal(want) {
		t.Fatalf("AccessTokenExpiresAt = %v, want the one-hour default %v", exp, want)
	}
}
