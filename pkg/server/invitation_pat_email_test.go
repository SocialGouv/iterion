package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/audit"
	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/identity"
	"github.com/SocialGouv/iterion/pkg/mail"
)

// recordingMailer records what it is asked to send, and refuses every send
// with fail when set. Sends arrive from the request goroutine and from the
// detached email of a session mint, hence the lock.
type recordingMailer struct {
	mu   sync.Mutex
	sent []mail.Message
	fail error
}

func (m *recordingMailer) Enabled() bool { return true }

func (m *recordingMailer) Send(_ context.Context, msg mail.Message) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail != nil {
		return m.fail
	}
	m.sent = append(m.sent, msg)
	return nil
}

func (m *recordingMailer) messages() []mail.Message {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]mail.Message(nil), m.sent...)
}

// invitationMailFixture is the derivation fixture with mailer wired into the
// auth service (nil: an instance that sends no email), and a personal access
// token pinned to team ta, which its account manages through its org-admin
// standing in o1.
func invitationMailFixture(t *testing.T, mailer mail.Mailer) (*derivationFixture, string) {
	t.Helper()
	f := newDerivationFixtureWithAuth(t, func(c *auth.Config) {
		c.Mailer = mailer
		c.PublicURL = "https://iterion.example"
	}, func(*Config) {})
	code, _, body := f.call(t, http.MethodPost, "/api/me/tokens", f.user.AccessToken, `{"name":"cli","team_id":"ta"}`)
	var v struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal([]byte(body), &v); err != nil || code != http.StatusCreated || v.Token == "" {
		t.Fatalf("mint a PAT pinned to ta: %d %s", code, body)
	}
	return f, v.Token
}

func invitationsOfTeam(t *testing.T, f *derivationFixture, teamID string) []identity.Invitation {
	t.Helper()
	invs, err := f.s.authStore().ListInvitationsByTeam(context.Background(), teamID)
	if err != nil {
		t.Fatalf("list the invitations of %s: %v", teamID, err)
	}
	return invs
}

// acceptTokenOf reads the invitation token out of the accept link an
// invitation email carries.
func acceptTokenOf(t *testing.T, msg mail.Message) string {
	t.Helper()
	i := strings.Index(msg.TextBody, "https://iterion.example/invitations/accept?token=")
	if i < 0 {
		t.Fatalf("the email to %s carries no accept link:\n%s", msg.To, msg.TextBody)
	}
	u, err := url.Parse(strings.Fields(msg.TextBody[i:])[0])
	if err != nil {
		t.Fatalf("parse the accept link: %v", err)
	}
	return u.Query().Get("token")
}

// TestInvitations_APersonalAccessTokenInvitesByEmailOnly: from a personal
// access token both invitation routes mint, email the token to the invited
// address alone, and answer with no token; the team route's invitation
// writes an org member, the org route's the org role it names.
func TestInvitations_APersonalAccessTokenInvitesByEmailOnly(t *testing.T) {
	mailer := &recordingMailer{}
	f, patToken := invitationMailFixture(t, mailer)
	for _, tc := range []struct {
		path, body, email string
		wantOrg           identity.OrgRole
	}{
		{"/api/teams/ta/invitations", `{"email":"lucas@example.com","role":"admin"}`, "lucas@example.com", identity.OrgRoleMember},
		{"/api/orgs/o1/invitations", `{"email":"victor@example.com","role":"member","team_id":"ta","org_role":"admin"}`, "victor@example.com", identity.OrgRoleAdmin},
	} {
		before := len(mailer.messages())
		code, _, body := f.call(t, http.MethodPost, tc.path, patToken, tc.body)
		if code != http.StatusOK {
			t.Fatalf("POST %s with a personal access token: %d %s, want 200", tc.path, code, body)
		}
		var resp map[string]any
		if err := json.Unmarshal([]byte(body), &resp); err != nil {
			t.Fatalf("decode %s: %v", body, err)
		}
		if _, has := resp["token"]; has {
			t.Fatalf("POST %s handed the token back to a personal access token: %s", tc.path, body)
		}
		if resp["delivery"] != "email" || resp["org_role"] != string(tc.wantOrg) {
			t.Fatalf("POST %s answered %s, want delivery email and org_role %s", tc.path, body, tc.wantOrg)
		}
		sent := mailer.messages()
		if len(sent) != before+1 || sent[len(sent)-1].To != tc.email {
			t.Fatalf("POST %s sent %d emails (last to %v), want one to %s", tc.path, len(sent)-before, sent, tc.email)
		}
		// The email tells the invitee an org rank above member, and only then.
		orgLine := strings.Contains(sent[len(sent)-1].TextBody, "Accepting also makes you admin of its organization")
		if orgLine != (tc.wantOrg == identity.OrgRoleAdmin) {
			t.Fatalf("POST %s: the email names the org-admin rank = %v, want %v:\n%s", tc.path, orgLine, tc.wantOrg == identity.OrgRoleAdmin, sent[len(sent)-1].TextBody)
		}
		tok := acceptTokenOf(t, sent[len(sent)-1])
		code, _, look := f.call(t, http.MethodGet, "/api/auth/invitations/lookup?token="+url.QueryEscape(tok), "", "")
		if code != http.StatusOK || !strings.Contains(look, `"org_role":"`+string(tc.wantOrg)+`"`) {
			t.Fatalf("lookup of the invitation for %s: %d %s, want org_role %s", tc.email, code, look, tc.wantOrg)
		}
		var row *identity.Invitation
		for _, inv := range invitationsOfTeam(t, f, "ta") {
			if inv.Email == tc.email {
				inv := inv
				row = &inv
			}
		}
		if row == nil {
			t.Fatalf("POST %s left no invitation row for %s", tc.path, tc.email)
		}
		if auth.HashRefreshToken(tok) != row.TokenHash {
			t.Fatalf("the emailed token for %s is not the one the row hashes", tc.email)
		}
		if row.OrgRoleOnAccept() != tc.wantOrg {
			t.Fatalf("the row for %s writes org %q, want %q", tc.email, row.OrgRoleOnAccept(), tc.wantOrg)
		}
	}
}

// TestInvitations_ASessionStillGetsTheLink: the studio's own path is
// unchanged — the token is in the response, for the minter to hand over.
func TestInvitations_ASessionStillGetsTheLink(t *testing.T) {
	f, _ := invitationMailFixture(t, nil)
	code, _, body := f.call(t, http.MethodPost, "/api/teams/ta/invitations", f.user.AccessToken, `{"email":"a@example.com","role":"member"}`)
	if code != http.StatusOK {
		t.Fatalf("a session's invitation: %d %s", code, body)
	}
	var resp struct {
		Token    string `json:"token"`
		Delivery string `json:"delivery"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil || resp.Token == "" || resp.Delivery != "link" {
		t.Fatalf("a session's invitation answered %s, want a token and delivery link", body)
	}
}

// TestInvitations_APersonalAccessTokenOnAnInstanceWithNoMailerMintsNone: the
// token would reach nobody, so nothing is minted and the refusal names where
// the deed can be done.
func TestInvitations_APersonalAccessTokenOnAnInstanceWithNoMailerMintsNone(t *testing.T) {
	f, patToken := invitationMailFixture(t, nil)
	code, _, body := f.call(t, http.MethodPost, "/api/teams/ta/invitations", patToken, `{"email":"a@example.com","role":"member"}`)
	if code != http.StatusForbidden || !strings.Contains(body, "sends no email") || !strings.Contains(body, "studio") {
		t.Fatalf("a PAT invitation with no mailer: %d %s, want 403 naming the missing mailer and the studio", code, body)
	}
	if n := len(invitationsOfTeam(t, f, "ta")); n != 0 {
		t.Fatalf("a refused PAT invitation left %d rows", n)
	}
}

// TestInvitations_AnEmailTheRelayRefusesTakesTheInvitationBack: the email is
// the only way the token leaves, so a relay that refuses it leaves no
// invitation behind.
func TestInvitations_AnEmailTheRelayRefusesTakesTheInvitationBack(t *testing.T) {
	mailer := &recordingMailer{fail: errors.New("relay down")}
	f, patToken := invitationMailFixture(t, mailer)
	f.s.auditStore = audit.NewMemoryStore()
	code, _, body := f.call(t, http.MethodPost, "/api/teams/ta/invitations", patToken, `{"email":"a@example.com","role":"member"}`)
	if code != http.StatusBadGateway || !strings.Contains(body, `"error_code":"`+invitationEmailFailedCode+`"`) {
		t.Fatalf("a refused email: %d %s, want 502 with error_code %s", code, body, invitationEmailFailedCode)
	}
	if n := len(invitationsOfTeam(t, f, "ta")); n != 0 {
		t.Fatalf("an invitation whose email was refused outlived the request (%d rows)", n)
	}
	deleted := waitForTenantAuditRows(t, f.s.auditStore, "ta", "invitation.deleted", 1)
	if deleted[0].Meta["reason"] != "email_delivery_failed" {
		t.Fatalf("the take-back's audit line names %v, want reason email_delivery_failed", deleted[0].Meta)
	}
}

// TestInvitations_TheTeamRouteNamesNoOrgRankAboveMember: a team invitation
// asking for an org admin is refused by name, pointing at the org route,
// rather than minted as a member.
func TestInvitations_TheTeamRouteNamesNoOrgRankAboveMember(t *testing.T) {
	f, _ := invitationMailFixture(t, nil)
	code, _, body := f.call(t, http.MethodPost, "/api/teams/ta/invitations", f.user.AccessToken, `{"email":"a@example.com","role":"admin","org_role":"admin"}`)
	if code != http.StatusBadRequest || !strings.Contains(body, "/api/orgs/{id}/invitations") {
		t.Fatalf("a team invitation naming org admin: %d %s, want 400 pointing at the org route", code, body)
	}
	if n := len(invitationsOfTeam(t, f, "ta")); n != 0 {
		t.Fatalf("a refused team invitation left %d rows", n)
	}
	code, _, body = f.call(t, http.MethodPost, "/api/teams/ta/invitations", f.user.AccessToken, `{"email":"a@example.com","role":"admin","org_role":"member"}`)
	if code != http.StatusOK {
		t.Fatalf("a team invitation naming org member: %d %s, want 200", code, body)
	}
}

// TestInvitations_APinnedTokenInvitesIntoItsOwnTeamOnly: a personal access
// token pinned to team ta reaches org o1 through the org route, and invites
// there into ta alone — never into a sibling team it cannot read, whether
// named or reached as the org's default team.
func TestInvitations_APinnedTokenInvitesIntoItsOwnTeamOnly(t *testing.T) {
	mailer := &recordingMailer{}
	f, patToken := invitationMailFixture(t, mailer)
	if _, err := f.s.authStore().CreateTeam(context.Background(), identity.Team{ID: "tb", Name: "TB", Slug: "tb", OrgID: "o1"}); err != nil {
		t.Fatalf("create sibling team tb: %v", err)
	}
	code, _, body := f.call(t, http.MethodPost, "/api/orgs/o1/invitations", patToken,
		`{"email":"y@example.com","role":"owner","team_id":"tb","org_role":"admin"}`)
	if code != http.StatusForbidden || !strings.Contains(body, "pinned to team ta") {
		t.Fatalf("a ta-pinned PAT inviting into tb: %d %s, want 403 naming the pin", code, body)
	}
	if n := len(invitationsOfTeam(t, f, "tb")); n != 0 {
		t.Fatalf("a refused pinned invitation left %d rows in tb", n)
	}
	if n := len(mailer.messages()); n != 0 {
		t.Fatalf("a refused pinned invitation sent %d emails", n)
	}
	code, _, body = f.call(t, http.MethodPost, "/api/orgs/o1/invitations", patToken,
		`{"email":"y@example.com","role":"member","team_id":"ta","org_role":"admin"}`)
	if code != http.StatusOK {
		t.Fatalf("a ta-pinned PAT inviting into ta through the org route: %d %s, want 200", code, body)
	}
}

// TestInvitations_APinnedTokenReachesNoSiblingByTheOrgsDefaultTeam: an org
// invitation that names no team joins the org's primary team, and a token
// pinned to another team is refused there too.
func TestInvitations_APinnedTokenReachesNoSiblingByTheOrgsDefaultTeam(t *testing.T) {
	mailer := &recordingMailer{}
	f, patToken := invitationMailFixture(t, mailer)
	if _, err := f.s.authStore().CreateTeam(context.Background(), identity.Team{ID: "t0", Name: "T0", Slug: "t0", OrgID: "o1", CreatedAt: time.Unix(0, 0).UTC()}); err != nil {
		t.Fatalf("create the org's primary team t0: %v", err)
	}
	if primary, err := f.s.firstTeamInOrg(context.Background(), "o1"); err != nil || primary != "t0" {
		t.Fatalf("the org's primary team is %q (%v), want t0 for this scenario", primary, err)
	}
	code, _, body := f.call(t, http.MethodPost, "/api/orgs/o1/invitations", patToken, `{"email":"y@example.com","role":"member"}`)
	if code != http.StatusForbidden || !strings.Contains(body, "pinned to team ta") {
		t.Fatalf("a ta-pinned PAT inviting into the org's default team t0: %d %s, want 403", code, body)
	}
	if n := len(invitationsOfTeam(t, f, "t0")); n != 0 || len(mailer.messages()) != 0 {
		t.Fatalf("a refused default-team invitation left %d rows and %d emails", n, len(mailer.messages()))
	}
}

// TestInvitations_APinnedTokenListsAndRevokesItsOwnTeamOnly: through the org
// routes, a token pinned to ta reads and revokes ta's invitations only — a
// sibling team's are ones the team route refuses it already.
func TestInvitations_APinnedTokenListsAndRevokesItsOwnTeamOnly(t *testing.T) {
	f, patToken := invitationMailFixture(t, nil)
	if _, err := f.s.authStore().CreateTeam(context.Background(), identity.Team{ID: "tb", Name: "TB", Slug: "tb", OrgID: "o1"}); err != nil {
		t.Fatalf("create sibling team tb: %v", err)
	}
	for _, team := range []string{"ta", "tb"} {
		code, _, body := f.call(t, http.MethodPost, "/api/orgs/o1/invitations", f.user.AccessToken,
			`{"email":"`+team+`@example.com","role":"member","team_id":"`+team+`"}`)
		if code != http.StatusOK {
			t.Fatalf("a session's invitation into %s: %d %s", team, code, body)
		}
	}
	code, _, body := f.call(t, http.MethodGet, "/api/orgs/o1/invitations", patToken, "")
	if code != http.StatusOK || !strings.Contains(body, "ta@example.com") || strings.Contains(body, "tb@example.com") {
		t.Fatalf("a ta-pinned PAT listing the org's invitations: %d %s, want ta's only", code, body)
	}
	sibling := invitationsOfTeam(t, f, "tb")[0]
	if code, _, body := f.call(t, http.MethodDelete, "/api/orgs/o1/invitations/"+sibling.ID, patToken, ""); code != http.StatusNotFound {
		t.Fatalf("a ta-pinned PAT revoking tb's invitation: %d %s, want 404", code, body)
	}
	if n := len(invitationsOfTeam(t, f, "tb")); n != 1 {
		t.Fatalf("tb holds %d invitations after the refused revoke, want 1", n)
	}
	own := invitationsOfTeam(t, f, "ta")[0]
	if code, _, body := f.call(t, http.MethodDelete, "/api/orgs/o1/invitations/"+own.ID, patToken, ""); code != http.StatusNoContent {
		t.Fatalf("a ta-pinned PAT revoking ta's invitation: %d %s, want 204", code, body)
	}
}

// TestInvitations_ATeamWithNoOrgNamesNoOrgRole: accepting an invitation into
// a team with no parent org writes no org membership, so neither the mint's
// answer nor the lookup names one.
func TestInvitations_ATeamWithNoOrgNamesNoOrgRole(t *testing.T) {
	f, _ := invitationMailFixture(t, nil)
	admin := f.superAdmin(t)
	if _, err := f.s.authStore().CreateTeam(context.Background(), identity.Team{ID: "lone", Name: "Lone", Slug: "lone"}); err != nil {
		t.Fatalf("create team lone: %v", err)
	}
	code, _, body := f.call(t, http.MethodPost, "/api/teams/lone/invitations", admin.AccessToken, `{"email":"n@example.com","role":"admin"}`)
	if code != http.StatusOK || strings.Contains(body, "org_role") {
		t.Fatalf("a mint on a team with no org: %d %s, want 200 naming no org_role", code, body)
	}
	var resp struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil || resp.Token == "" {
		t.Fatalf("decode %s: %v", body, err)
	}
	code, _, look := f.call(t, http.MethodGet, "/api/auth/invitations/lookup?token="+url.QueryEscape(resp.Token), "", "")
	if code != http.StatusOK || strings.Contains(look, "org_role") {
		t.Fatalf("lookup on a team with no org: %d %s, want 200 naming no org_role", code, look)
	}
	code, _, list := f.call(t, http.MethodGet, "/api/teams/lone/invitations", admin.AccessToken, "")
	if code != http.StatusOK || !strings.Contains(list, "n@example.com") || strings.Contains(list, "org_role") {
		t.Fatalf("the invitations of a team with no org: %d %s, want the row naming no org_role", code, list)
	}
}

// TestInvitations_TokensTurnedOffAfterTheMintEndWhatTheySent: with personal
// access tokens off, none stands, so an invitation one emailed is refused
// (410) rather than failing every read of it.
func TestInvitations_TokensTurnedOffAfterTheMintEndWhatTheySent(t *testing.T) {
	mailer := &recordingMailer{}
	f, patToken := invitationMailFixture(t, mailer)
	code, _, body := f.call(t, http.MethodPost, "/api/teams/ta/invitations", patToken, `{"email":"a@example.com","role":"member"}`)
	if code != http.StatusOK {
		t.Fatalf("the PAT's invitation: %d %s", code, body)
	}
	tok := acceptTokenOf(t, mailer.messages()[0])
	f.s.pats = nil
	code, _, look := f.call(t, http.MethodGet, "/api/auth/invitations/lookup?token="+url.QueryEscape(tok), "", "")
	if code != http.StatusGone {
		t.Fatalf("lookup once personal access tokens are off: %d %s, want 410", code, look)
	}
}

// TestInvitations_RevokingThePATEndsWhatItSent: the response to a leaked or
// rotated token is to revoke it, and that ends the invitations it emailed —
// never in silence. Revoking that token takes its pending invitations out and
// names them; a revocation that sweeps nothing (sign out everywhere) leaves
// them listed, and the lookup and the accept refuse them (410) since the
// token no longer stands (auth.ErrInvitationCredentialGone).
func TestInvitations_RevokingThePATEndsWhatItSent(t *testing.T) {
	mint := func(t *testing.T) (*derivationFixture, string, string) {
		mailer := &recordingMailer{}
		f, patToken := invitationMailFixture(t, mailer)
		code, _, body := f.call(t, http.MethodPost, "/api/orgs/o1/invitations", patToken,
			`{"email":"thief@example.com","role":"owner","team_id":"ta","org_role":"admin"}`)
		if code != http.StatusOK {
			t.Fatalf("the PAT's invitation: %d %s", code, body)
		}
		rows := invitationsOfTeam(t, f, "ta")
		if len(rows) != 1 || rows[0].MintedWith == "" {
			t.Fatalf("the PAT's invitation row does not name its minting token: %+v", rows)
		}
		return f, acceptTokenOf(t, mailer.messages()[0]), rows[0].MintedWith
	}
	acceptAs := func(t *testing.T, f *derivationFixture, tok string) (int, string) {
		thief, err := f.svc.Register(context.Background(), "thief@example.com", fixturePassword, "Thief", "", "ua", "ip")
		if err != nil {
			t.Fatalf("register the thief: %v", err)
		}
		code, _, body := f.call(t, http.MethodPost, "/api/auth/invitations/accept", thief.AccessToken, `{"token":"`+tok+`"}`)
		if _, err := f.s.authStore().GetOrgMembership(context.Background(), thief.User.ID, "o1"); err == nil {
			t.Fatal("the refused accept wrote an org membership")
		}
		return code, body
	}

	t.Run("revoking the token takes its invitations out and names them", func(t *testing.T) {
		f, tok, patID := mint(t)
		code, _, body := f.call(t, http.MethodDelete, "/api/me/tokens/"+patID, f.user.AccessToken, "")
		if code != http.StatusOK || !strings.Contains(body, `"invitations_revoked":[{`) || !strings.Contains(body, "thief@example.com") {
			t.Fatalf("revoke the PAT: %d %s, want 200 naming the invitation it ended", code, body)
		}
		if n := len(invitationsOfTeam(t, f, "ta")); n != 0 {
			t.Fatalf("%d invitations still pending after their token was revoked", n)
		}
		if code, body := acceptAs(t, f, tok); code != http.StatusNotFound {
			t.Fatalf("accepting a revoked token's invitation: %d %s, want 404", code, body)
		}
	})

	t.Run("a revocation that sweeps nothing is refused at lookup and accept", func(t *testing.T) {
		f, tok, _ := mint(t)
		if code, _, body := f.call(t, http.MethodPost, "/api/me/sessions/revoke-all", f.user.AccessToken, ""); code != http.StatusNoContent {
			t.Fatalf("sign out everywhere: %d %s", code, body)
		}
		f.s.authSvc.WaitRevocationPasses()
		code, _, body := f.call(t, http.MethodGet, "/api/auth/invitations/lookup?token="+url.QueryEscape(tok), "", "")
		if code != http.StatusGone || !strings.Contains(body, "does not stand") || strings.Contains(body, "disabled") {
			t.Fatalf("lookup after the token was revoked: %d %s, want 410 naming no account state", code, body)
		}
		if code, body := acceptAs(t, f, tok); code != http.StatusGone || !strings.Contains(body, "does not stand") {
			t.Fatalf("accepting the revoked token's invitation: %d %s, want 410", code, body)
		}
	})

	t.Run("revoking a token that sent nothing answers 204", func(t *testing.T) {
		f, _ := invitationMailFixture(t, &recordingMailer{})
		code, _, body := f.call(t, http.MethodPost, "/api/me/tokens", f.user.AccessToken, `{"name":"idle","team_id":"ta"}`)
		var v struct {
			PAT struct {
				ID string `json:"id"`
			} `json:"pat"`
		}
		if err := json.Unmarshal([]byte(body), &v); err != nil || code != http.StatusCreated || v.PAT.ID == "" {
			t.Fatalf("mint an idle PAT: %d", code)
		}
		if code, _, body := f.call(t, http.MethodDelete, "/api/me/tokens/"+v.PAT.ID, f.user.AccessToken, ""); code != http.StatusNoContent {
			t.Fatalf("revoke an idle PAT: %d %s, want 204", code, body)
		}
	})
}

// deleteRefusingStore refuses every invitation delete, so a take-back fails.
type deleteRefusingStore struct{ identity.Store }

func (deleteRefusingStore) DeleteInvitation(context.Context, string) error {
	return errors.New("store unreachable")
}

// TestInvitations_ATakeBackThatFailsSaysTheInvitationStands: when the relay
// refuses the email AND the row cannot be taken back, the 502's code says the
// invitation stands — the only part of a 5xx body a caller reads.
func TestInvitations_ATakeBackThatFailsSaysTheInvitationStands(t *testing.T) {
	mailer := &recordingMailer{fail: errors.New("relay down")}
	f, patToken := invitationMailFixture(t, mailer)
	real := f.s.authStore()
	f.s.authSvc.SetStoreForTest(deleteRefusingStore{Store: real})
	code, _, body := f.call(t, http.MethodPost, "/api/teams/ta/invitations", patToken, `{"email":"a@example.com","role":"member"}`)
	f.s.authSvc.SetStoreForTest(real)
	if code != http.StatusBadGateway || !strings.Contains(body, `"error_code":"`+invitationEmailFailedStandsCode+`"`) {
		t.Fatalf("a refused email whose take-back failed: %d %s, want 502 with error_code %s", code, body, invitationEmailFailedStandsCode)
	}
	if n := len(invitationsOfTeam(t, f, "ta")); n != 1 {
		t.Fatalf("%d invitation rows, want the 1 the failed take-back left", n)
	}
}

// TestInvitations_TheAuditNamesTheMintingToken: someone answering a leak
// reads the trail for what a token sent and what its revocation took — the
// mint's line names the token, the sweep's names the token and its reason.
func TestInvitations_TheAuditNamesTheMintingToken(t *testing.T) {
	mailer := &recordingMailer{}
	f, patToken := invitationMailFixture(t, mailer)
	f.s.auditStore = audit.NewMemoryStore()
	if code, _, body := f.call(t, http.MethodPost, "/api/teams/ta/invitations", patToken, `{"email":"a@example.com","role":"member"}`); code != http.StatusOK {
		t.Fatalf("the PAT's invitation: %d %s", code, body)
	}
	patID := invitationsOfTeam(t, f, "ta")[0].MintedWith
	created := waitForTenantAuditRows(t, f.s.auditStore, "ta", "invitation.created", 1)
	if got := created[0].Meta["minted_with"]; got != patID {
		t.Fatalf("the mint's audit line names minted_with %v, want the token %s", got, patID)
	}
	if code, _, body := f.call(t, http.MethodDelete, "/api/me/tokens/"+patID, f.user.AccessToken, ""); code != http.StatusOK {
		t.Fatalf("revoke the PAT: %d %s", code, body)
	}
	deleted := waitForTenantAuditRows(t, f.s.auditStore, "ta", "invitation.deleted", 1)
	if deleted[0].Meta["reason"] != "minting_pat_revoked" || deleted[0].Meta["minted_with"] != patID {
		t.Fatalf("the sweep's audit line names %v, want reason minting_pat_revoked and the token", deleted[0].Meta)
	}
}

// TestInvitations_TheOrgListNamesWhatALegacyRowWrites: a row minted before
// invitations named their org role writes an org member, which the org's
// list says rather than leaving the field out.
func TestInvitations_TheOrgListNamesWhatALegacyRowWrites(t *testing.T) {
	f, _ := invitationMailFixture(t, nil)
	now := time.Now().UTC()
	if err := f.s.authStore().CreateInvitation(context.Background(), identity.Invitation{ID: "legacy", TeamID: "ta", Email: "legacy@example.com", Role: identity.RoleAdmin, TokenHash: "h-legacy", InvitedBy: f.user.User.ID, CreatedAt: now, ExpiresAt: now.Add(time.Hour)}); err != nil {
		t.Fatalf("write a legacy row: %v", err)
	}
	code, _, body := f.call(t, http.MethodGet, "/api/orgs/o1/invitations", f.user.AccessToken, "")
	if code != http.StatusOK || !strings.Contains(body, `"email":"legacy@example.com","role":"admin","org_role":"member"`) {
		t.Fatalf("the org list of a legacy admin row: %d %s, want org_role member", code, body)
	}
}
