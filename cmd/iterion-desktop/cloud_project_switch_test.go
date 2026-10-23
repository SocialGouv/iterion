package main

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/identity"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	iserver "github.com/SocialGouv/iterion/pkg/server"
)

// newCloudWithReuseGrace is newRealCloud with the server's refresh-reuse
// grace shrunk, for drives that must reach PAST a rotation's grace window:
// the shipped default is 30 s and a test must not sleep that long.
func newCloudWithReuseGrace(t *testing.T, grace time.Duration) *realCloud {
	t.Helper()
	signer, err := auth.NewJWTSigner(randomJWTSecret(t), 15*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	sessions := &failingSessions{MemorySessionStore: auth.NewMemorySessionStore()}
	svc, err := auth.NewService(auth.Config{
		Store: identity.NewMemoryStore(), Sessions: sessions, Signer: signer,
		SignupMode: auth.SignupOpen, RefreshTTL: time.Hour,
		RefreshReuseGrace: &grace,
	})
	if err != nil {
		t.Fatal(err)
	}
	s := iserver.New(iserver.Config{AuthService: svc, AuthSigner: signer}, iterlog.New(iterlog.LevelError, nil))
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return &realCloud{url: srv.URL, svc: svc, sessions: sessions}
}

// registerJar registers an account, gives the jar over store its session (the
// mint path's product, as seedJar does for its jar), and leaves the jar as
// the connection's live jar — the one the connection registry holds.
func registerJar(t *testing.T, c *realCloud, email, connID string, store refreshStore) *cloudTokenJar {
	t.Helper()
	ctx := context.Background()
	desk, err := c.svc.Register(ctx, email, "password-forever-9", "", "", "desktop", "ip")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	jar := newCloudTokenJar(connID, c.url, store)
	if err := jar.seed(cloudAuthResult{AccessToken: desk.AccessToken, AccessExpiry: desk.AccessExpires, RefreshToken: desk.RefreshToken}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return jar
}

// TestTheProjectSwitchRenewsTheJarThePaneTunnelHolds (one jar per
// connection): switching the current project to a connection a pane already
// holds open resolves the switch's jar through the registry's — the pane
// tunnel's — and renews the session through it. A switch that built its own
// jar would rotate the session beneath the registry jar: that jar's next
// presentation lands outside the rotation's reuse grace, which revokes the
// switched-to lineage and deletes the keychain entry the live jar owns, so a
// restart adopts nothing.
func TestTheProjectSwitchRenewsTheJarThePaneTunnelHolds(t *testing.T) {
	ctx := context.Background()
	c := newCloudWithReuseGrace(t, 10*time.Millisecond)
	store := newFakeStore()
	key := cloudRefreshKeyPrefix + "conn-1"
	registryJar := registerJar(t, c, "desk@example.org", "conn-1", store)

	token := registryJar.storedRefresh()
	got := activateConnectionJar(ctx, registryJar, "conn-1", c.url, store)
	if got != registryJar {
		t.Fatal("the switch built a second jar over the connection's keychain entry: one jar per connection means the registry jar — the pane tunnel's — is the switch's")
	}
	if got.storedRefresh() == token {
		t.Fatal("the activation did not renew the session through the registry jar")
	}
	if !got.LoggedIn() || got.AccessToken() == "" {
		t.Fatalf("the activation left the jar unauthenticated (logged in %t, access %q)", got.LoggedIn(), got.AccessToken())
	}
	rotated := got.storedRefresh()
	if entry := store.m[key]; entry != rotated {
		t.Fatalf("the entry holds %q, want the rotation's successor %q, written by the same jar the registry holds", entry, rotated)
	}
	if !c.lineageLive(t, rotated) {
		t.Fatal("precondition: the switched-to session must be live after the activation")
	}

	// The registry jar was never replaced: its next tick presents the token
	// IT rotated, and nothing is refused or revoked — there is no second jar
	// whose stale presentation could land outside the reuse grace.
	time.Sleep(25 * time.Millisecond) // past the rotation's grace window
	if err := got.refreshNow(ctx); err != nil {
		t.Fatalf("the registry jar's next tick was refused (%v): the switch left a second jar rotating beneath it", err)
	}
	live := got.storedRefresh()
	if !c.lineageLive(t, live) {
		t.Fatal("the registry jar's tick revoked the session the operator was switched to")
	}
	if entry := store.m[key]; entry != live {
		t.Fatalf("the entry holds %q after the tick, want the jar's own successor %q: a stale jar deleted it", entry, live)
	}

	// Control: with no registry jar the activation builds one and hydrates
	// it — the restart shape still works.
	seed := registerJar(t, c, "second@example.org", "conn-2", store)
	fresh := activateConnectionJar(ctx, nil, "conn-2", c.url, store)
	if fresh == nil || fresh == seed {
		t.Fatalf("the activation without a registry jar built nothing or reused a foreign jar (%v)", fresh)
	}
	if fresh.storedRefresh() == "" {
		t.Fatal("the activation of a connection no jar holds did not hydrate its entry")
	}
	if err := fresh.refreshNow(ctx); err != nil {
		t.Fatalf("the fresh jar's renewal: %v", err)
	}
	if fresh.storedRefresh() == seed.storedRefresh() {
		t.Fatal("the fresh jar did not renew the session it hydrated")
	}
}
