package auth

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/identity"
)

// The tests in this file pin one rule across the service: a read that FAILED
// is not a fact about the user. Taken for "not found" — no org, not an admin,
// not a member — it mints a session or answers a refusal the store never
// stated. Each gate fails a read the way a real store does (a cancelled
// context, an unreachable cluster), often on a call other than the first.

// TestLogin_TeamOrgReadFailureIsNotAnOrglessSession: the active team's org is
// resolved by two reads. Either failing must fail the login, never mint an
// access token whose org context is empty while the account has one.
func TestLogin_TeamOrgReadFailureIsNotAnOrglessSession(t *testing.T) {
	for _, read := range []string{"GetTeam", "GetOrgMembership"} {
		t.Run(read, func(t *testing.T) {
			ctx := context.Background()
			svc := newTestService(t, SignupOpen)
			if _, err := svc.Register(ctx, "orgless@example.org", "correct-horse-9", "", "", "ua", "ip"); err != nil {
				t.Fatalf("Register: %v", err)
			}
			gate := newNthCallGate(svc.store, context.Canceled).fail(read, 1)
			svc.SetStoreForTest(gate)
			res, err := svc.Login(ctx, "orgless@example.org", "correct-horse-9", "ua", "ip")
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("a login whose %s fails: got err=%v active_org=%q, want that read's error", read, err, res.ActiveOrgID)
			}
		})
	}
}

// TestSwitchTeam_ReadFailureIsNotARefusalNorAnOrglessToken drives the step-in
// of an org admin into a team it holds no grant on — the switch the studio
// offers every org admin. Each of its reads failing must answer that failure:
// not "not a member" (a refusal the switcher shows as a dead click), not a
// token with no org — and the user's default must not move.
func TestSwitchTeam_ReadFailureIsNotARefusalNorAnOrglessToken(t *testing.T) {
	for _, tc := range []struct {
		name, read string
		call       int
	}{
		{"the team read of the step-in", "GetTeam", 1},
		{"the org-admin read of the step-in", "GetOrgMembership", 1},
		{"the team read of the org resolution", "GetTeam", 2},
		{"the org-membership read of the org resolution", "GetOrgMembership", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			svc := newTestService(t, SignupOpen)
			uid, _, teamID := orgAdminFixture(t, svc, "stepin@example.com", identity.OrgRoleAdmin)
			before, err := svc.store.GetUser(ctx, uid)
			if err != nil {
				t.Fatalf("GetUser: %v", err)
			}
			svc.SetStoreForTest(newNthCallGate(svc.store, errStoreUnreachable).fail(tc.read, tc.call))
			sw, err := svc.SwitchTeam(ctx, Identity{Credential: CredentialAccessToken, UserID: uid}, teamID)
			if err == nil || errors.Is(err, ErrNotAMember) || !errors.Is(err, errStoreUnreachable) {
				t.Fatalf("a team switch whose %s fails: got err=%v org=%q, want that read's error", tc.name, err, sw.Identity.OrgID)
			}
			after, err := svc.store.GetUser(ctx, uid)
			if err != nil {
				t.Fatalf("GetUser: %v", err)
			}
			if after.DefaultTeamID != before.DefaultTeamID {
				t.Fatalf("a switch that failed moved the user's default team to %q", after.DefaultTeamID)
			}
		})
	}
}

// TestSwitchOrg_AdminLandingReadsTheOrgRoleOnce pins the org admin's landing
// on the org's first team: the switch already read the caller's org role, so
// a later failed read cannot turn the admin into "no team here" — an empty
// workspace the store never described.
func TestSwitchOrg_AdminLandingReadsTheOrgRoleOnce(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t, SignupOpen)
	uid, orgID, teamID := orgAdminFixture(t, svc, "landing-once@example.com", identity.OrgRoleAdmin)
	// The switch's own role read (call 1) answers; any later one fails.
	svc.SetStoreForTest(newNthCallGate(svc.store, errStoreUnreachable).fail("GetOrgMembership", 2, 3, 4, 5))
	sw, err := svc.SwitchOrg(ctx, Identity{Credential: CredentialAccessToken, UserID: uid}, orgID)
	if err != nil {
		t.Fatalf("SwitchOrg: %v", err)
	}
	if sw.Identity.TeamID != teamID || sw.Identity.Role != identity.RoleAdmin {
		t.Fatalf("org admin landed on team=%q role=%q, want the org's team %q as admin", sw.Identity.TeamID, sw.Identity.Role, teamID)
	}
}

// TestSwitchOrg_ReadFailureMovesNothing covers the org switch's reads: each
// failing answers the failure and leaves the user's defaults where they were.
func TestSwitchOrg_ReadFailureMovesNothing(t *testing.T) {
	for _, read := range []string{"GetOrgMembership", "ListMembershipsByUser", "ListTeamsByOrg"} {
		t.Run(read, func(t *testing.T) {
			ctx := context.Background()
			svc := newTestService(t, SignupOpen)
			uid, orgID, _ := orgAdminFixture(t, svc, "orgswitch@example.com", identity.OrgRoleAdmin)
			before, err := svc.store.GetUser(ctx, uid)
			if err != nil {
				t.Fatalf("GetUser: %v", err)
			}
			svc.SetStoreForTest(newNthCallGate(svc.store, context.Canceled).fail(read, 1))
			if _, err := svc.SwitchOrg(ctx, Identity{Credential: CredentialAccessToken, UserID: uid}, orgID); !errors.Is(err, context.Canceled) {
				t.Fatalf("an org switch whose %s fails: got %v, want that read's error", read, err)
			}
			after, err := svc.store.GetUser(ctx, uid)
			if err != nil {
				t.Fatalf("GetUser: %v", err)
			}
			if after.DefaultOrgID != before.DefaultOrgID || after.DefaultTeamID != before.DefaultTeamID {
				t.Fatalf("a failed org switch moved the defaults to org=%q team=%q", after.DefaultOrgID, after.DefaultTeamID)
			}
		})
	}
}

// TestSwitchOrg_SuperAdminOrgReadFailureIsNotOrgNotFound: a super-admin steps
// into an org it is not a member of once the org's existence is read. That
// read failing is not "no such org".
func TestSwitchOrg_SuperAdminOrgReadFailureIsNotOrgNotFound(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t, SignupOpen)
	res, err := svc.Register(ctx, "root@example.org", "correct-horse-9", "", "", "ua", "ip")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	yes := true
	if _, err := svc.store.PatchUser(ctx, res.User.ID, identity.UserPatch{IsSuperAdmin: &yes}); err != nil {
		t.Fatalf("grant super-admin: %v", err)
	}
	org, err := svc.store.CreateOrg(ctx, identity.Org{ID: "org-foreign", Name: "Foreign", Slug: "foreign"})
	if err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	svc.SetStoreForTest(newNthCallGate(svc.store, errStoreUnreachable).fail("GetOrg", 1))
	if _, err := svc.SwitchOrg(ctx, Identity{Credential: CredentialAccessToken, UserID: res.User.ID}, org.ID); errors.Is(err, ErrOrgNotFound) || !errors.Is(err, errStoreUnreachable) {
		t.Fatalf("a super-admin's org switch whose org read fails: got %v, want that read's error", err)
	}
}

// TestSwitchTeam_AnswersWithTheRowItWrote pins where a switch's user view
// comes from: the row the switch persisted its defaults to — nothing is read
// after the switch commits.
func TestSwitchTeam_AnswersWithTheRowItWrote(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t, SignupOpen)
	uid, _, teamID := orgAdminFixture(t, svc, "row@example.com", identity.OrgRoleAdmin)
	sw, err := svc.SwitchTeam(ctx, Identity{Credential: CredentialAccessToken, UserID: uid}, teamID)
	if err != nil {
		t.Fatalf("SwitchTeam: %v", err)
	}
	if sw.User.ID != uid || sw.User.DefaultTeamID != teamID {
		t.Fatalf("the switch answered with user %q default team %q, want %q on %q (the row it wrote)", sw.User.ID, sw.User.DefaultTeamID, uid, teamID)
	}
	if sw.AccessToken == "" || sw.AccessExpires.IsZero() {
		t.Fatalf("the switch delivered no access token: %+v", sw)
	}
}

// TestRefresh_ReuseGraceConfig pins the window's override: zero disables it
// (every presentation of a retired token is reuse, as before the window
// existed), a negative value is refused.
func TestRefresh_ReuseGraceConfig(t *testing.T) {
	ctx := context.Background()
	off := time.Duration(0)
	svc, err := NewService(Config{
		Store: identity.NewMemoryStore(), Sessions: NewMemorySessionStore(), Signer: newTestSigner(t),
		SignupMode: SignupOpen, RefreshReuseGrace: &off,
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	reg, err := svc.Register(ctx, "nograce@example.org", "correct-horse-9", "", "", "ua", "ip")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if _, err := svc.Refresh(ctx, reg.RefreshToken, "ua", "ip"); err != nil {
		t.Fatalf("first refresh: %v", err)
	}
	if _, err := svc.Refresh(ctx, reg.RefreshToken, "ua", "ip"); !errors.Is(err, ErrSessionRevoked) {
		t.Fatalf("with the window disabled, a retired token presented at once: got %v, want ErrSessionRevoked", err)
	}

	neg := -time.Second
	if _, err := NewService(Config{
		Store: identity.NewMemoryStore(), Sessions: NewMemorySessionStore(), Signer: newTestSigner(t),
		RefreshReuseGrace: &neg,
	}); err == nil {
		t.Fatal("a negative reuse grace was accepted")
	}
}
