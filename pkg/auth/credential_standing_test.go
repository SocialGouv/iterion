package auth

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth/oidc"
	"github.com/SocialGouv/iterion/pkg/identity"
)

// TestCheckDerivation_APersonalAccessTokenStandsOnItsRow: a personal access
// token — presented, or as the minter of a WS ticket — stands only while its
// row does: revoked or gone, every purpose it may serve is refused, and the
// ticket it minted with it; a PAT credential that names no row is refused; a
// row that cannot be read is a failure, not a refusal.
func TestCheckDerivation_APersonalAccessTokenStandsOnItsRow(t *testing.T) {
	ctx := context.Background()
	f := newRotationFixture(t, errStoreUnreachable)
	pat := Identity{UserID: f.userID, Credential: CredentialPAT, CredentialID: "pat-1"}
	ticket := pat
	ticket.MintedBy, ticket.Credential = CredentialPAT, CredentialWSTicket

	usablePATs(f.svc, "pat-1")
	for mint := range patMayDo {
		if _, err := f.svc.CheckDerivation(ctx, pat, mint); err != nil {
			t.Fatalf("a usable personal access token asking for %s: %v", mint, err)
		}
	}
	if _, err := f.svc.CheckStanding(ctx, ticket); err != nil {
		t.Fatalf("the WS ticket of a usable personal access token: %v", err)
	}

	usablePATs(f.svc) // every row revoked
	for mint := range patMayDo {
		if _, err := f.svc.CheckDerivation(ctx, pat, mint); !errors.Is(err, ErrTokenRevoked) {
			t.Fatalf("a revoked personal access token asking for %s: got %v, want ErrTokenRevoked", mint, err)
		}
	}
	if _, err := f.svc.CheckStanding(ctx, ticket); !errors.Is(err, ErrTokenRevoked) {
		t.Fatalf("the WS ticket of a revoked personal access token: got %v, want ErrTokenRevoked", err)
	}

	unnamed := pat
	unnamed.CredentialID = ""
	if _, err := f.svc.CheckDerivation(ctx, unnamed, MintWSTicket); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("a personal access token credential naming no row: got %v, want ErrTokenInvalid", err)
	}

	readFailed := errors.New("pat store down")
	f.svc.SetPATState(func(context.Context, string, time.Time) (bool, error) { return false, readFailed })
	if _, err := f.svc.CheckDerivation(ctx, pat, MintWSTicket); !errors.Is(err, readFailed) || errors.Is(err, ErrTokenRevoked) {
		t.Fatalf("a personal access token whose row cannot be read: got %v, want the read's failure", err)
	}
}

// TestCheckDerivation_AnAccessTokenNamingNoSessionLinksNoSSOIdentity: an
// access token minted before access tokens named their session is checked
// for its account alone and bounded by its own expiry — but an SSO link has
// no expiry, so it links none; everything else it may still do.
func TestCheckDerivation_AnAccessTokenNamingNoSessionLinksNoSSOIdentity(t *testing.T) {
	ctx := context.Background()
	f := newRotationFixture(t, errStoreUnreachable)
	login, err := f.svc.Login(ctx, "rotation@example.org", "correct-horse-9", "ua", "ip")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	access := presentedAccess(t, f.svc, login.AccessToken)
	access.SessionLineage, access.SessionGeneration = "", 0
	for _, mint := range everyMint {
		_, err := f.svc.CheckDerivation(ctx, access, mint)
		if mint == MintSSOLink {
			if !errors.Is(err, ErrNotDerivable) {
				t.Fatalf("an access token naming no session linking an SSO identity: got %v, want ErrNotDerivable", err)
			}
			continue
		}
		if err != nil {
			t.Fatalf("an access token naming no session asking for %s: %v", mint, err)
		}
	}
}

// TestCheckStanding_AnOpenSocketIsAskedAboutItsSessionNotItsExpiry: an open
// WebSocket outlives the access token it was opened with; asked about with
// its expiry left out, it stands while its session does and falls with it —
// opened with the token itself or with a ticket the token minted.
func TestCheckStanding_AnOpenSocketIsAskedAboutItsSessionNotItsExpiry(t *testing.T) {
	ctx := context.Background()
	f := newRotationFixture(t, errStoreUnreachable)
	login, err := f.svc.Login(ctx, "rotation@example.org", "correct-horse-9", "ua", "ip")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	access := presentedAccess(t, f.svc, login.AccessToken)
	ticket := access
	ticket.MintedBy, ticket.Credential = CredentialAccessToken, CredentialWSTicket
	f.clock.Advance(access.ExpiresAt.Sub(f.clock.Now()) + time.Minute)
	if _, err := f.svc.CheckStanding(ctx, access); !errors.Is(err, ErrTokenExpired) {
		t.Fatalf("an expired access token asked with its expiry: got %v, want ErrTokenExpired", err)
	}
	for name, id := range map[string]Identity{"the access token": access, "its ticket": ticket} {
		if _, err := f.svc.CheckStanding(ctx, StandingOfOpenSocket(id)); err != nil {
			t.Fatalf("a socket opened with %s of a live session, past the token's expiry: %v", name, err)
		}
	}
	if err := f.svc.Logout(ctx, login.RefreshToken); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	for name, id := range map[string]Identity{"the access token": access, "its ticket": ticket} {
		if _, err := f.svc.CheckStanding(ctx, StandingOfOpenSocket(id)); !errors.Is(err, ErrSessionRevoked) {
			t.Fatalf("a socket opened with %s of a session signed out: got %v, want ErrSessionRevoked", name, err)
		}
	}
}

// TestSSOLinks_WhatASessionWroteEndsWithTheCompromiseResponse: a link written
// from the account settings records the session that wrote it; "sign out
// everywhere" (RemoveSessionSSOLinks) removes every such link and keeps the
// one an SSO sign-in wrote itself; a reuse revocation of a lineage removes
// the links that lineage wrote, and only those.
func TestSSOLinks_WhatASessionWroteEndsWithTheCompromiseResponse(t *testing.T) {
	ctx := context.Background()
	f := newRotationFixture(t, errStoreUnreachable)
	lineage := f.session(t, f.token).LineageID
	if lineage == "" {
		t.Fatal("the registered session names no lineage")
	}
	signUp := identity.OIDCLink{Provider: "google", ProviderUserID: "signup-sub", UserID: f.userID, CreatedAt: time.Now().UTC()}
	if err := f.ids.UpsertOIDCLink(ctx, signUp); err != nil {
		t.Fatalf("seed the sign-up link: %v", err)
	}
	link := func(subject, lineage string) {
		t.Helper()
		created, err := f.svc.LinkExternalToUser(ctx, oidc.ExternalUser{Provider: "github", Subject: subject, Email: "rotation@example.org"}, f.userID, lineage)
		if err != nil || !created {
			t.Fatalf("link %s from lineage %s: created=%v err=%v", subject, lineage, created, err)
		}
	}
	subjects := func() map[string]bool {
		t.Helper()
		links, err := f.svc.ListSSOLinks(ctx, f.userID)
		if err != nil {
			t.Fatalf("list links: %v", err)
		}
		out := map[string]bool{}
		for _, l := range links {
			out[l.ProviderUserID] = true
		}
		return out
	}

	link("from-this-lineage", lineage)
	link("from-another-lineage", "another-lineage")
	if _, err := f.svc.Refresh(ctx, f.token, "ua", "ip"); err != nil {
		t.Fatalf("rotate the session: %v", err)
	}
	f.clock.Advance(DefaultRefreshReuseGrace + 30*time.Second)
	if _, err := f.svc.Refresh(ctx, f.token, "ua", "ip"); !errors.Is(err, ErrSessionRevoked) {
		t.Fatalf("the retired token presented past its window: got %v, want ErrSessionRevoked (reuse)", err)
	}
	if got := subjects(); got["from-this-lineage"] || !got["from-another-lineage"] || !got["signup-sub"] {
		t.Fatalf("after the reuse revocation of lineage %s the account links %v, want that lineage's link removed and the others kept", lineage, got)
	}

	link("from-a-third-lineage", "third-lineage")
	n, err := f.svc.RemoveSessionSSOLinks(ctx, f.userID)
	if err != nil || n != 2 {
		t.Fatalf("sign out everywhere removed %d links (err %v), want the 2 a session wrote", n, err)
	}
	if got := subjects(); len(got) != 1 || !got["signup-sub"] {
		t.Fatalf("after sign out everywhere the account links %v, want only the sign-up link", got)
	}
}
