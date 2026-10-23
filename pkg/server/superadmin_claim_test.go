package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/auth/orgsso"
	"github.com/SocialGouv/iterion/pkg/identity"
	"github.com/SocialGouv/iterion/pkg/marketplace"
)

// The super-admin claim is re-derived where the identity is established
// (revokeSuperAdminClaim): a token that claims the flag carries it only while
// the credential stands AND the account still is a super-admin, so a revoked
// session's or a demoted super-admin's token exercises no platform authority
// anywhere — the admin routes, the team/org RBAC escape hatches, the
// org-member placement, the marketplace moderation of public entries, the
// global memory space, another account's PAT, the org-SSO proof bypass.

// testRevokedSuperAdmin answers a super-admin whose session is signed out
// everywhere from another device: the access token is within its lifetime,
// its session revoked. The control (401 at an admin route) is asserted.
func testRevokedSuperAdmin(t *testing.T, f *derivationFixture, email string) auth.LoginResult {
	t.Helper()
	ctx := context.Background()
	reg, err := f.svc.Register(ctx, email, fixturePassword, "", "", "ua", "ip")
	if err != nil {
		t.Fatal(err)
	}
	yes := true
	if _, err := f.s.authStore().PatchUser(ctx, reg.User.ID, identity.UserPatch{IsSuperAdmin: &yes}); err != nil {
		t.Fatal(err)
	}
	admin, err := f.svc.Login(ctx, email, fixturePassword, "ua", "ip")
	if err != nil {
		t.Fatal(err)
	}
	other, err := f.svc.Login(ctx, email, fixturePassword, "other-device", "ip2")
	if err != nil {
		t.Fatal(err)
	}
	if code, _, body := f.call(t, http.MethodPost, "/api/me/sessions/revoke-all", other.AccessToken, ""); code != http.StatusNoContent {
		t.Fatalf("sign out everywhere: %d %s", code, body)
	}
	if code, _, body := f.call(t, http.MethodGet, "/api/admin/users", admin.AccessToken, ""); code != http.StatusUnauthorized {
		t.Fatalf("control: the revoked session's token was not refused at /api/admin/users: %d %s", code, body)
	}
	return admin
}

// testSuperAdminNamed registers a platform super-admin and answers its live
// session, under the email given (the fixtures share one identity store).
func testSuperAdminNamed(t *testing.T, f *derivationFixture, email string) auth.LoginResult {
	t.Helper()
	ctx := context.Background()
	reg, err := f.svc.Register(ctx, email, fixturePassword, "", "", "ua", "ip")
	if err != nil {
		t.Fatal(err)
	}
	yes := true
	if _, err := f.s.authStore().PatchUser(ctx, reg.User.ID, identity.UserPatch{IsSuperAdmin: &yes}); err != nil {
		t.Fatal(err)
	}
	live, err := f.svc.Login(ctx, email, fixturePassword, "ua", "ip")
	if err != nil {
		t.Fatal(err)
	}
	return live
}

// testDemotedSuperAdmin answers a live super-admin session whose account is
// then demoted: no session is revoked by a demotion, so the refusal at an
// admin route is 403.
func testDemotedSuperAdmin(t *testing.T, f *derivationFixture, email string) auth.LoginResult {
	t.Helper()
	ctx := context.Background()
	reg, err := f.svc.Register(ctx, email, fixturePassword, "", "", "ua", "ip")
	if err != nil {
		t.Fatal(err)
	}
	yes := true
	if _, err := f.s.authStore().PatchUser(ctx, reg.User.ID, identity.UserPatch{IsSuperAdmin: &yes}); err != nil {
		t.Fatal(err)
	}
	live, err := f.svc.Login(ctx, email, fixturePassword, "ua", "ip")
	if err != nil {
		t.Fatal(err)
	}
	no := false
	if _, err := f.s.authStore().PatchUser(ctx, live.User.ID, identity.UserPatch{IsSuperAdmin: &no}); err != nil {
		t.Fatal(err)
	}
	if code, _, body := f.call(t, http.MethodGet, "/api/admin/users", live.AccessToken, ""); code != http.StatusForbidden {
		t.Fatalf("control: the demoted account's live token was not refused at /api/admin/users: %d %s", code, body)
	}
	return live
}

// TestPlatformAuthority_ARevokedSuperAdminsTokenPlacesNoOrgMember: placing an
// existing account in an org (PUT /api/orgs/{id}/members/{user_id}) is a
// platform act — a revoked session's super-admin token places no one, a
// demoted one neither, and a live super-admin still can.
func TestPlatformAuthority_ARevokedSuperAdminsTokenPlacesNoOrgMember(t *testing.T) {
	f := newDerivationFixture(t)
	ctx := context.Background()
	seedOrg(t, f.s, "victim", "victim-org")
	seedOrg(t, f.s, "victim2", "victim2-org")

	admin := testRevokedSuperAdmin(t, f, "sa1@example.com")
	code, _, body := f.call(t, http.MethodPut, "/api/orgs/victim/members/"+admin.User.ID, admin.AccessToken, `{"role":"owner"}`)
	if code != http.StatusUnauthorized {
		t.Fatalf("a revoked session's super-admin token placed an org member: %d %s", code, body)
	}
	if _, err := f.s.authStore().GetOrgMembership(ctx, admin.User.ID, "victim"); err == nil {
		t.Fatal("the revoked session's token wrote an org membership")
	}

	demoted := testDemotedSuperAdmin(t, f, "admin2@example.com")
	code, _, body = f.call(t, http.MethodPut, "/api/orgs/victim2/members/"+demoted.User.ID, demoted.AccessToken, `{"role":"owner"}`)
	if code != http.StatusForbidden {
		t.Fatalf("a demoted super-admin's live token placed an org member: %d %s", code, body)
	}
	if _, err := f.s.authStore().GetOrgMembership(ctx, demoted.User.ID, "victim2"); err == nil {
		t.Fatal("the demoted super-admin's token wrote an org membership")
	}

	// Control: a live super-admin session still places an account.
	live := testSuperAdminNamed(t, f, "sa-live@example.com")
	if code, _, body := f.call(t, http.MethodPut, "/api/orgs/victim/members/"+live.User.ID, live.AccessToken, `{"role":"member"}`); code != http.StatusOK {
		t.Fatalf("control: a live super-admin could not place an org member: %d %s", code, body)
	}
}

// TestPlatformAuthority_ARevokedSuperAdminsTokenApprovesNoPublicMarketplaceEntry:
// moderating a PUBLIC marketplace entry is platform authority: a revoked
// session's super-admin token is refused 401, a demoted one 403, and a plain
// member stays 403; the entry stays pending until a live super-admin acts.
// The same arms run against an INSTANCE-WIDE entry — the docs' sentence
// names "a public or instance-wide entry", and both scopes ride the one
// gate.
func TestPlatformAuthority_ARevokedSuperAdminsTokenApprovesNoPublicMarketplaceEntry(t *testing.T) {
	for _, tc := range []struct {
		name  string
		scope marketplace.Scope
	}{
		{"public", marketplace.ScopePublic},
		{"instance", marketplace.ScopeInstance},
	} {
		t.Run(tc.name, func(t *testing.T) {
			registry, err := marketplace.NewJSONStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			f := newDerivationFixtureWith(t, func(c *Config) {
				c.Mode = "cloud"
				c.Marketplace = registry
			})
			ctx := context.Background()
			seed := func(slug string, scope marketplace.Scope) {
				if err := registry.Upsert(ctx, marketplace.Entry{
					Slug: slug, Name: slug, RepoURL: "https://example.invalid/" + slug,
					Scope: scope, Status: marketplace.StatusPending, SubmittedBy: f.user.User.ID,
				}); err != nil {
					t.Fatal(err)
				}
			}
			statusOf := func(slug string) marketplace.Status {
				e, ok, err := registry.Get(ctx, slug)
				if err != nil || !ok {
					t.Fatalf("get %s: %v %v", slug, ok, err)
				}
				return marketplace.EffectiveStatus(*e)
			}
			mustStatus := func(action, slug string, want marketplace.Status) {
				t.Helper()
				if got := statusOf(slug); got != want {
					t.Fatalf("%s: entry %s is %s, want %s", action, slug, got, want)
				}
			}

			seed("member-try-"+tc.name, tc.scope)
			if code, _, _ := f.call(t, http.MethodPost, "/api/v1/marketplace/moderation/member-try-"+tc.name+"/approve", f.user.AccessToken, ""); code != http.StatusForbidden {
				t.Fatalf("control: a plain member approved a %s entry: %d", tc.name, code)
			}
			mustStatus("a plain member's approval", "member-try-"+tc.name, marketplace.StatusPending)

			seed("revoked-try-"+tc.name, tc.scope)
			admin := testRevokedSuperAdmin(t, f, "sa1@example.com")
			if code, _, body := f.call(t, http.MethodPost, "/api/v1/marketplace/moderation/revoked-try-"+tc.name+"/approve", admin.AccessToken, ""); code != http.StatusUnauthorized {
				t.Fatalf("a revoked session's super-admin token approved a %s entry: %d %s", tc.name, code, body)
			}
			mustStatus("the revoked session's token", "revoked-try-"+tc.name, marketplace.StatusPending)

			seed("demoted-try-"+tc.name, tc.scope)
			demoted := testDemotedSuperAdmin(t, f, "admin2@example.com")
			if code, _, body := f.call(t, http.MethodPost, "/api/v1/marketplace/moderation/demoted-try-"+tc.name+"/approve", demoted.AccessToken, ""); code != http.StatusForbidden {
				t.Fatalf("a demoted super-admin's live token approved a %s entry: %d %s", tc.name, code, body)
			}
			mustStatus("the demoted super-admin's token", "demoted-try-"+tc.name, marketplace.StatusPending)

			seed("live-ok-"+tc.name, tc.scope)
			live := testSuperAdminNamed(t, f, "sa-live@example.com")
			if code, _, body := f.call(t, http.MethodPost, "/api/v1/marketplace/moderation/live-ok-"+tc.name+"/approve", live.AccessToken, ""); code != http.StatusOK {
				t.Fatalf("control: a live super-admin could not approve a %s entry: %d %s", tc.name, code, body)
			}
			mustStatus("the live super-admin's approval", "live-ok-"+tc.name, marketplace.StatusApproved)
		})
	}
}

// TestPlatformAuthority_ARevokedSuperAdminsTokenWritesNoGlobalMemoryDoc: the
// global memory space is instance-wide; a revoked session's super-admin token
// writes no doc into it and another tenant's member reads nothing.
func TestPlatformAuthority_ARevokedSuperAdminsTokenWritesNoGlobalMemoryDoc(t *testing.T) {
	t.Setenv("ITERION_HOME", t.TempDir())
	f := newDerivationFixture(t)
	q := "/api/memory/doc?visibility=global&name=catalog&path=pwn.md"
	if code, _, body := f.call(t, http.MethodPut, q, f.user.AccessToken, "# member try"); code != http.StatusForbidden {
		t.Fatalf("control: a plain member wrote the global memory space: %d %s", code, body)
	}

	admin := testRevokedSuperAdmin(t, f, "sa1@example.com")
	code, _, body := f.call(t, http.MethodPut, q, admin.AccessToken, "# injected")
	if code != http.StatusUnauthorized {
		t.Fatalf("a revoked session's super-admin token wrote the global memory space: %d %s", code, body)
	}
	if code, _, body := f.call(t, http.MethodGet, q, f.user.AccessToken, ""); code == http.StatusOK && strings.Contains(body, "injected") {
		t.Fatal("the injected global doc is readable by another tenant")
	}

	demoted := testDemotedSuperAdmin(t, f, "admin2@example.com")
	if code, _, body := f.call(t, http.MethodPut, q, demoted.AccessToken, "# injected"); code != http.StatusForbidden {
		t.Fatalf("a demoted super-admin's live token wrote the global memory space: %d %s", code, body)
	}
}

// TestPlatformAuthority_ADemotedSuperAdminsTokenRevokesNoOtherAccountsPAT:
// revoking ANOTHER account's personal access token is a platform act; a
// demoted super-admin's live token reads the flag from the account, so the
// victim's PAT keeps working.
func TestPlatformAuthority_ADemotedSuperAdminsTokenRevokesNoOtherAccountsPAT(t *testing.T) {
	f := newDerivationFixture(t)
	ctx := context.Background()
	code, victimPAT := f.mintPAT(t, f.user.AccessToken, "victim-ci")
	if code != http.StatusCreated {
		t.Fatalf("mint victim PAT: %d", code)
	}
	toks, err := f.pats.ListByUser(ctx, f.user.User.ID)
	if err != nil || len(toks) != 1 {
		t.Fatalf("list: %v %d", err, len(toks))
	}
	demoted := testDemotedSuperAdmin(t, f, "admin@example.com")
	if code, _, body := f.call(t, http.MethodDelete, "/api/me/tokens/"+toks[0].ID, demoted.AccessToken, ""); code != http.StatusForbidden {
		t.Fatalf("a demoted super-admin's token revoked another account's PAT: %d %s", code, body)
	}
	if me := f.meStatus(t, victimPAT); me != http.StatusOK {
		t.Fatalf("the victim's PAT stopped working after the refused revoke: %d", me)
	}

	revoked := testRevokedSuperAdmin(t, f, "sa3@example.com")
	if code, _, body := f.call(t, http.MethodDelete, "/api/me/tokens/"+toks[0].ID, revoked.AccessToken, ""); code != http.StatusUnauthorized {
		t.Fatalf("a revoked session's super-admin token revoked another account's PAT: %d %s", code, body)
	}
	if me := f.meStatus(t, victimPAT); me != http.StatusOK {
		t.Fatalf("the victim's PAT stopped working after the refused revoke: %d", me)
	}
}

// TestPlatformAuthority_ADemotedSuperAdminsTokenGetsNoOrgSSOProofBypass:
// an org's GitHub SSO grants are honoured only once the org proves control
// of the GitHub org; the proof bypass was read from the token's claim, so a
// demoted super-admin's live token marked its own grants verified on an org
// it does not belong to. It reads the account now: verified=false.
func TestPlatformAuthority_ADemotedSuperAdminsTokenGetsNoOrgSSOProofBypass(t *testing.T) {
	f := newDerivationFixtureWithOrgSSO(t)
	demoted := testDemotedSuperAdmin(t, f, "admin@example.com")
	if _, err := f.s.authStore().GetOrgMembership(context.Background(), demoted.User.ID, "o1"); err == nil {
		t.Fatal("fixture: the demoted admin is a member of o1")
	}
	body := `{"kind":"github","display_name":"gh","enabled":true,"auto_provision":true,"grants":[{"github_org":"attacker-gh-org","role":"member"}]}`
	// The demoted admin is NOT a member of o1 and its escape hatch is gone:
	// the org-SSO write is refused outright.
	if code, _, resp := f.call(t, http.MethodPost, "/api/orgs/o1/sso/providers", demoted.AccessToken, body); code != http.StatusForbidden {
		t.Fatalf("a demoted super-admin's token wrote an org-SSO provider on an org it does not manage: %d %s", code, resp)
	}
	// Control: the org's OWN admin still stores the provider, inert until a
	// forge proof (verified=false).
	code, _, resp := f.call(t, http.MethodPost, "/api/orgs/o1/sso/providers", f.user.AccessToken, body)
	if code != http.StatusOK {
		t.Fatalf("control: the org's own admin could not store the provider: %d %s", code, resp)
	}
	var v struct {
		Grants []orgsso.GitHubTeamGrant `json:"grants"`
	}
	if err := json.Unmarshal([]byte(resp), &v); err != nil {
		t.Fatal(err)
	}
	for _, g := range v.Grants {
		if g.Verified {
			t.Fatal("the org's own admin stored a verified grant without proof of control")
		}
	}
}

// TestPlatformAuthority_ARevokedSuperAdminsTokenKeepsNoTeamReach: the
// team/org RBAC escape hatches (canViewTeam & co.) read the same flag; a
// demoted super-admin's live token and a revoked session's token keep no
// reach over a team they do not belong to.
func TestPlatformAuthority_ARevokedSuperAdminsTokenKeepsNoTeamReach(t *testing.T) {
	f := newDerivationFixture(t)
	// A team nobody in the fixture belongs to.
	seedTeam(t, f.s, "oteam", "other-team")

	demoted := testDemotedSuperAdmin(t, f, "admin2@example.com")
	if code, _, body := f.call(t, http.MethodGet, "/api/teams/oteam/members", demoted.AccessToken, ""); code != http.StatusForbidden {
		t.Fatalf("a demoted super-admin's live token kept the team reach: %d %s", code, body)
	}
	revoked := testRevokedSuperAdmin(t, f, "sa2@example.com")
	// The route is not a credential gate: the stripped flag sends the token
	// down the ordinary membership path, which answers 403 (not a member).
	if code, _, body := f.call(t, http.MethodGet, "/api/teams/oteam/members", revoked.AccessToken, ""); code != http.StatusForbidden {
		t.Fatalf("a revoked session's super-admin token kept the team reach: %d %s", code, body)
	}
	// Control: a live super-admin keeps the reach.
	live := testSuperAdminNamed(t, f, "sa-live@example.com")
	if code, _, body := f.call(t, http.MethodGet, "/api/teams/oteam/members", live.AccessToken, ""); code != http.StatusOK {
		t.Fatalf("control: a live super-admin lost the team reach: %d %s", code, body)
	}
}

// TestPlatformAuthority_AStoreReadFailureAnswersFiveXX: the re-derivation
// that cannot read the account answers an explicit 5xx — never a silent
// strip that would let the request go on judged by a claim nobody checked.
func TestPlatformAuthority_AStoreReadFailureAnswersFiveXX(t *testing.T) {
	f := newDerivationFixture(t)
	live := f.superAdmin(t)
	f.ids.fail("GetUser", 0)
	defer f.ids.disarm()
	if code, _, body := f.call(t, http.MethodGet, "/api/admin/users", live.AccessToken, ""); code < 500 {
		t.Fatalf("a failing store read answered %d on a super-admin request: %s", code, body)
	}
	if code, _, body := f.call(t, http.MethodGet, "/api/teams", live.AccessToken, ""); code < 500 && code != http.StatusForbidden {
		t.Fatalf("a failing store read answered %d on the team list of a super-admin claim: %s", code, body)
	}
}

// TestPlatformAuthority_ATicketCarriesNoStaleSuperAdminClaim: a WS ticket
// serializes the identity it was minted from; its redemption re-reads the
// account (wsTicketIdentity), so a demotion between the mint and the dial
// does not ride the ticket.
func TestPlatformAuthority_ATicketCarriesNoStaleSuperAdminClaim(t *testing.T) {
	f := newDerivationFixture(t)
	admin := f.superAdmin(t)
	code, _, body := f.call(t, http.MethodPost, "/api/ws/ticket", admin.AccessToken, "")
	if code != http.StatusOK {
		t.Fatalf("mint a ws ticket: %d %s", code, body)
	}
	var v struct {
		Ticket string `json:"ticket"`
	}
	if err := json.Unmarshal([]byte(body), &v); err != nil || v.Ticket == "" {
		t.Fatalf("the ticket: %v %s", err, body)
	}
	redeem := func() auth.Identity {
		t.Helper()
		r := httptest.NewRequest(http.MethodGet, "/api/ws?ticket="+url.QueryEscape(v.Ticket), nil)
		id, presented, err := f.s.wsTicketIdentity(r)
		if !presented || err != nil {
			t.Fatalf("redeem: presented=%v err=%v", presented, err)
		}
		return id
	}
	if !redeem().IsSuperAdmin {
		t.Fatal("control: the live super-admin's ticket redeemed without the flag")
	}
	code, _, body = f.call(t, http.MethodPost, "/api/ws/ticket", admin.AccessToken, "")
	if code != http.StatusOK {
		t.Fatalf("mint a second ws ticket: %d %s", code, body)
	}
	if err := json.Unmarshal([]byte(body), &v); err != nil || v.Ticket == "" {
		t.Fatalf("the second ticket: %v %s", err, body)
	}
	no := false
	if _, err := f.s.authStore().PatchUser(context.Background(), admin.User.ID, identity.UserPatch{IsSuperAdmin: &no}); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodGet, "/api/ws?ticket="+url.QueryEscape(v.Ticket), nil)
	id, presented, err := f.s.wsTicketIdentity(r)
	if !presented || err != nil {
		t.Fatalf("redeem: presented=%v err=%v", presented, err)
	}
	if id.IsSuperAdmin {
		t.Fatal("a ws ticket redeemed after its account's demotion carries the super-admin flag")
	}
}
