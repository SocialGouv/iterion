package auth

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/identity"
)

// Every route that mints a credential from the one presented to it asks
// CheckDerivation first: what the presented kind may mint at all, then
// whether its account and its session still stand.

var everyMint = []Mint{MintAccessToken, MintPAT, MintWSTicket, MintSSOLink, MintPassword, ChangeCredential, ChangeSignIn, ExercisePlatformAuthority}

// patMayDo is what a personal access token may do: mint a WS ticket, change
// or remove a credential of its account, change how an org signs in, and
// exercise platform authority — never mint a credential of its own account.
var patMayDo = map[Mint]bool{MintWSTicket: true, ChangeCredential: true, ChangeSignIn: true, ExercisePlatformAuthority: true}

// usablePATs registers on svc a PAT reader that answers the ids given as
// usable and every other id as revoked.
func usablePATs(svc *Service, ids ...string) {
	usable := map[string]bool{}
	for _, id := range ids {
		usable[id] = true
	}
	svc.SetPATState(func(_ context.Context, id string, _ time.Time) (bool, error) { return usable[id], nil })
}

// TestCheckDerivation_WhatEachCredentialMayMint: an access token mints
// anything; a personal access token mints a WS ticket and nothing a session
// mints; a WS ticket, a synthetic principal and a machine principal mint
// nothing; an identity no credential reader built is a failure, not a
// refusal; the dev-mode principal derives without a store.
func TestCheckDerivation_WhatEachCredentialMayMint(t *testing.T) {
	ctx := context.Background()
	f := newRotationFixture(t, errStoreUnreachable)
	login, err := f.svc.Login(ctx, "rotation@example.org", "correct-horse-9", "ua", "ip")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	access := presentedAccess(t, f.svc, login.AccessToken)
	patExpiry := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	pat := Identity{UserID: f.userID, Credential: CredentialPAT, CredentialID: "pat-1", ExpiresAt: patExpiry}
	usablePATs(f.svc, "pat-1")
	ticket := access
	ticket.Credential = CredentialWSTicket
	for _, mint := range everyMint {
		d, err := f.svc.CheckDerivation(ctx, access, mint)
		if err != nil {
			t.Fatalf("a live session's access token minting %s: %v", mint, err)
		}
		if d.User.ID != f.userID || !d.NotAfter.Equal(access.ExpiresAt) {
			t.Fatalf("the derivation of %s answered %+v / %v, want the account and the token's expiry %v", mint, d.User.ID, d.NotAfter, access.ExpiresAt)
		}
		d, err = f.svc.CheckDerivation(ctx, pat, mint)
		if patMayDo[mint] {
			if err != nil || !d.NotAfter.Equal(patExpiry) {
				t.Fatalf("a personal access token asking for %s: %v (not after %v), want allowed until %v", mint, err, d.NotAfter, patExpiry)
			}
		} else if !errors.Is(err, ErrNotDerivable) {
			t.Fatalf("a personal access token minting %s: got %v, want ErrNotDerivable", mint, err)
		}
		if _, err := f.svc.CheckDerivation(ctx, ticket, mint); !errors.Is(err, ErrNotDerivable) {
			t.Fatalf("a WS ticket minting %s: got %v, want ErrNotDerivable", mint, err)
		}
		for _, kind := range []IdentityKind{KindWebhook, KindShare, KindInternal} {
			if _, err := f.svc.CheckDerivation(ctx, Identity{UserID: f.userID, Kind: kind}, mint); !errors.Is(err, ErrNotDerivable) {
				t.Fatalf("a %s principal minting %s: got %v, want ErrNotDerivable", kind, mint, err)
			}
		}
		if _, err := f.svc.CheckDerivation(ctx, Identity{UserID: f.userID}, mint); err == nil || errors.Is(err, ErrNotDerivable) {
			t.Fatalf("an identity no reader built minting %s: got %v, want a failure", mint, err)
		}
		if _, err := (*Service)(nil).CheckDerivation(ctx, Identity{UserID: "dev", Credential: CredentialDevMode}, mint); err != nil {
			t.Fatalf("the dev-mode principal minting %s: %v", mint, err)
		}
	}
}

// TestCheckDerivation_ARevokedSessionsAccessTokenMintsNothing: once its
// session is signed out, its account signed out everywhere, its password
// changed elsewhere, or its account disabled or set pending, an access token
// still within its lifetime mints nothing — no switch, no personal access
// token, no WS ticket, no SSO link — and one past its expiry neither.
func TestCheckDerivation_ARevokedSessionsAccessTokenMintsNothing(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name   string
		revoke func(t *testing.T, f *rotationFixture, login LoginResult)
		want   error
	}{
		{"its session signed out", func(t *testing.T, f *rotationFixture, login LoginResult) {
			if err := f.svc.Logout(ctx, login.RefreshToken); err != nil {
				t.Fatalf("Logout: %v", err)
			}
		}, ErrSessionRevoked},
		{"its account signed out everywhere", func(t *testing.T, f *rotationFixture, _ LoginResult) {
			if err := f.svc.RevokeUserSessions(ctx, f.userID); err != nil {
				t.Fatalf("RevokeUserSessions: %v", err)
			}
		}, ErrSessionRevoked},
		{"its password changed on another device", func(t *testing.T, f *rotationFixture, _ LoginResult) {
			if _, err := f.svc.ChangePassword(ctx, f.userID, "correct-horse-9", "another-horse-7", "other", "ip"); err != nil {
				t.Fatalf("ChangePassword: %v", err)
			}
		}, ErrSessionRevoked},
		{"its account disabled", func(t *testing.T, f *rotationFixture, _ LoginResult) {
			setStatus(t, f, identity.UserStatusDisabled)
		}, ErrAccountDisabled},
		{"its account set pending a password change", func(t *testing.T, f *rotationFixture, _ LoginResult) {
			setStatus(t, f, identity.UserStatusPendingPasswordChange)
		}, ErrSessionRevoked},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRotationFixture(t, errStoreUnreachable)
			login, err := f.svc.Login(ctx, "rotation@example.org", "correct-horse-9", "ua", "ip")
			if err != nil {
				t.Fatalf("Login: %v", err)
			}
			access := presentedAccess(t, f.svc, login.AccessToken)
			tc.revoke(t, f, login)
			for _, mint := range everyMint {
				if _, err := f.svc.CheckDerivation(ctx, access, mint); !errors.Is(err, tc.want) {
					t.Fatalf("the access token minting %s after %s: got %v, want %v", mint, tc.name, err, tc.want)
				}
			}
		})
	}
	t.Run("past its own expiry", func(t *testing.T) {
		f := newRotationFixture(t, errStoreUnreachable)
		login, err := f.svc.Login(ctx, "rotation@example.org", "correct-horse-9", "ua", "ip")
		if err != nil {
			t.Fatalf("Login: %v", err)
		}
		access := presentedAccess(t, f.svc, login.AccessToken)
		f.clock.Advance(access.ExpiresAt.Sub(f.clock.Now()) + time.Minute)
		for _, mint := range everyMint {
			if _, err := f.svc.CheckDerivation(ctx, access, mint); !errors.Is(err, ErrTokenExpired) {
				t.Fatalf("an expired access token minting %s: got %v, want ErrTokenExpired", mint, err)
			}
		}
	})
}

// TestSwitch_APersonalAccessTokenIsRefused: a switch answers ErrNotDerivable
// to a personal access token — it is pinned to its team and its expiry — and
// mints nothing.
func TestSwitch_APersonalAccessTokenIsRefused(t *testing.T) {
	ctx := context.Background()
	f := newRotationFixture(t, errStoreUnreachable)
	login, err := f.svc.Login(ctx, "rotation@example.org", "correct-horse-9", "ua", "ip")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	pat := Identity{UserID: f.userID, TeamID: login.ActiveTeamID, Credential: CredentialPAT}
	if sw, err := f.svc.SwitchTeam(ctx, pat, login.ActiveTeamID); !errors.Is(err, ErrNotDerivable) || sw.AccessToken != "" {
		t.Fatalf("a team switch with a personal access token: got %v (token minted: %t), want ErrNotDerivable", err, sw.AccessToken != "")
	}
	if sw, err := f.svc.SwitchOrg(ctx, pat, login.ActiveOrgID); !errors.Is(err, ErrNotDerivable) || sw.AccessToken != "" {
		t.Fatalf("an org switch with a personal access token: got %v (token minted: %t), want ErrNotDerivable", err, sw.AccessToken != "")
	}
}
