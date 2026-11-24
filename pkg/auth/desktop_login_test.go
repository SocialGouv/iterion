package auth

import (
	"context"
	"errors"
	"testing"

	"github.com/SocialGouv/iterion/pkg/auth/oidc"
	"github.com/SocialGouv/iterion/pkg/identity"
)

// The desktop SSO login, split at the ticket (#2635): the callback DEFERS —
// no session row, no token — and the redemption re-asks the account's
// standing against the lineage and generation the callback named, and mints.

// TestDesktopLogin_TheCallbackWritesNothingLive: a deferred desktop login
// resolves the account and names the lineage its session will be born under,
// but writes NO session and mints NO token — an unredeemed ticket leaves
// nothing live (there is no session to revoke at its expiry), and the store
// the ticket waits in never holds a credential. The redemption then mints
// the session under the lineage the callback named, and it refreshes.
func TestDesktopLogin_TheCallbackWritesNothingLive(t *testing.T) {
	ctx := context.Background()
	f := newRotationFixture(t, errStoreUnreachable)
	ext := oidc.ExternalUser{Provider: "testsso", Subject: "desk-1", Email: "desktop@example.org", Name: "Desk"}

	d, err := f.svc.LoginWithExternalDesktop(ctx, ext, "browser", "ip")
	if err != nil {
		t.Fatalf("LoginWithExternalDesktop: %v", err)
	}
	if d.UserID == "" || d.Root.ID == "" || d.PreferredTeamID == "" {
		t.Fatalf("the deferred login is incomplete: %+v", d)
	}
	// The callback wrote no session: the account holds none at all (it was
	// just created), and the lineage the redemption will mint under is not
	// live anywhere.
	if got := sessionsOf(t, f.sessions.MemorySessionStore, d.UserID); len(got) != 0 {
		t.Fatalf("the callback wrote %d session(s): a deferred login mints nothing", len(got))
	}
	if _, found, err := f.sessions.LiveLineageSession(ctx, d.UserID, d.Root.ID); err != nil || found {
		t.Fatalf("the lineage the ticket names is live before redemption (found=%v, err=%v)", found, err)
	}

	res, err := f.svc.RedeemDesktopLogin(ctx, d, "desktop", "ip-desktop")
	if err != nil {
		t.Fatalf("RedeemDesktopLogin: %v", err)
	}
	if res.AccessToken == "" || res.RefreshToken == "" {
		t.Fatal("the redemption delivered no tokens")
	}
	if res.User.ID != d.UserID || res.ActiveTeamID != d.PreferredTeamID {
		t.Errorf("the redemption landed elsewhere: user %q team %q, want %q / %q", res.User.ID, res.ActiveTeamID, d.UserID, d.PreferredTeamID)
	}
	// The session is the root the callback named — one lineage, born under
	// the generation the callback read.
	sess := f.session(t, res.RefreshToken)
	if sess.ID != d.Root.ID || sess.LineageID != d.Root.ID || sess.Generation != d.Root.Generation {
		t.Errorf("the session = %+v, want the lineage %q under generation %d", sess, d.Root.ID, d.Root.Generation)
	}
	id := presentedAccess(t, f.svc, res.AccessToken)
	if id.SessionLineage != d.Root.ID || id.SessionGeneration != d.Root.Generation {
		t.Errorf("the access token names %+v, want the ticket's lineage", id)
	}
	// And it is an ordinary session: it refreshes.
	if _, err := f.svc.Refresh(ctx, res.RefreshToken, "desktop", "ip-desktop"); err != nil {
		t.Fatalf("the minted session does not refresh: %v", err)
	}
}

// TestDesktopLogin_ARevocationSinceTheCallbackRefusesTheRedemption: the
// account's standing is asked at REDEMPTION as the lineage and generation
// the callback named — a "sign out everywhere", an admin disable or a
// forced password change landing between the two leaves the ticket nothing
// to mint.
func TestDesktopLogin_ARevocationSinceTheCallbackRefusesTheRedemption(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name   string
		revoke func(t *testing.T, f *rotationFixture, d DesktopLogin)
		want   error
	}{
		{"nothing happened since", func(*testing.T, *rotationFixture, DesktopLogin) {}, nil},
		{"a sign out everywhere", func(t *testing.T, f *rotationFixture, d DesktopLogin) {
			if err := f.svc.RevokeUserSessions(ctx, d.UserID); err != nil {
				t.Fatal(err)
			}
		}, ErrSessionRevoked},
		{"an admin disable", func(t *testing.T, f *rotationFixture, d DesktopLogin) {
			disabled := identity.UserStatusDisabled
			if _, err := f.svc.store.PatchUser(ctx, d.UserID, identity.UserPatch{Status: &disabled}); err != nil {
				t.Fatal(err)
			}
			if err := f.svc.RevokeUserSessions(ctx, d.UserID); err != nil {
				t.Fatal(err)
			}
		}, ErrAccountDisabled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRotationFixture(t, errStoreUnreachable)
			d, err := f.svc.LoginWithExternalDesktop(ctx, oidc.ExternalUser{
				Provider: "testsso", Subject: "desk-" + tc.name, Email: "desk@example.org",
			}, "browser", "ip")
			if err != nil {
				t.Fatalf("LoginWithExternalDesktop: %v", err)
			}
			tc.revoke(t, f, d)
			res, err := f.svc.RedeemDesktopLogin(ctx, d, "desktop", "ip-desktop")
			if tc.want == nil {
				if err != nil || res.AccessToken == "" {
					t.Fatalf("the redemption after %s: %v (delivered=%t), want a session", tc.name, err, res.AccessToken != "")
				}
				return
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("the redemption after %s: %v, want %v", tc.name, err, tc.want)
			}
			if res.AccessToken != "" || res.RefreshToken != "" {
				t.Fatalf("the refused redemption after %s delivered tokens", tc.name)
			}
			if got := sessionsOf(t, f.sessions.MemorySessionStore, d.UserID); len(got) != 0 {
				t.Fatalf("the refused redemption after %s still wrote %d session(s)", tc.name, len(got))
			}
		})
	}
}

// TestDesktopLogin_OrgFlowDefersTheSameWay: the per-org SSO variant defers
// identically — the landing team is the org's — and the redemption lands in
// it.
func TestDesktopLogin_OrgFlowDefersTheSameWay(t *testing.T) {
	ctx := context.Background()
	svc, teamID := orgServiceFixture(t, identity.RoleMember)
	ext := oidc.ExternalUser{Provider: "oidc-org-prov1", Subject: "kc-desk", Email: "desk@acme.example", Name: "Desk"}
	d, err := svc.LoginWithExternalForOrgDesktop(ctx, ext, teamID, "prov1", "browser", "ip")
	if err != nil {
		t.Fatalf("LoginWithExternalForOrgDesktop: %v", err)
	}
	if d.PreferredTeamID != teamID {
		t.Fatalf("the landing team = %q, want the org's %q", d.PreferredTeamID, teamID)
	}
	res, err := svc.RedeemDesktopLogin(ctx, d, "desktop", "ip-desktop")
	if err != nil {
		t.Fatalf("RedeemDesktopLogin: %v", err)
	}
	if res.ActiveTeamID != teamID || res.ActiveRole != identity.RoleMember {
		t.Errorf("the redemption landed in %q as %q, want %q as member", res.ActiveTeamID, res.ActiveRole, teamID)
	}
}

// TestRedeemDesktopLogin_RefusesAnEmptyLogin: a ticket whose payload names no
// account or no lineage is a refusal, never a mint.
func TestRedeemDesktopLogin_RefusesAnEmptyLogin(t *testing.T) {
	f := newRotationFixture(t, errStoreUnreachable)
	for _, d := range []DesktopLogin{
		{},
		{UserID: "u1"},
		{Root: SessionRoot{ID: "lineage-1"}},
	} {
		if _, err := f.svc.RedeemDesktopLogin(context.Background(), d, "ua", "ip"); err == nil {
			t.Errorf("RedeemDesktopLogin(%+v) minted a session", d)
		}
	}
}

// TestRedeemDesktopLogin_RefusesAnUnknownAccount: a ticket naming an account
// that is gone (deleted since the callback) mints nothing.
func TestRedeemDesktopLogin_RefusesAnUnknownAccount(t *testing.T) {
	f := newRotationFixture(t, errStoreUnreachable)
	root, err := NewSessionRoot(context.Background(), f.sessions, "no-such-user")
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.svc.RedeemDesktopLogin(context.Background(), DesktopLogin{UserID: "no-such-user", Root: root}, "ua", "ip")
	if !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("the redemption of a ticket naming a gone account: %v, want ErrSessionNotFound", err)
	}
}
