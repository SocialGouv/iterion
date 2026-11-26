package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/audit"
	"github.com/SocialGouv/iterion/pkg/identity"
	"github.com/SocialGouv/iterion/pkg/pat"
)

// patIDOf reads the id of the personal access token whose plaintext is token.
func patIDOf(t *testing.T, f *derivationFixture, token string) string {
	t.Helper()
	row, err := f.pats.GetByTokenHash(context.Background(), pat.HashToken(token))
	if err != nil {
		t.Fatalf("read the PAT row: %v", err)
	}
	return row.ID
}

func revokePAT(t *testing.T, f *derivationFixture, id string) {
	t.Helper()
	if err := f.pats.Revoke(context.Background(), id, time.Now().UTC()); err != nil {
		t.Errorf("revoke PAT %s inside the race: %v", id, err)
	}
}

// interleavingStore runs a hook once, at a chosen point of an invitation
// write, to land a concurrent write there.
type interleavingStore struct {
	identity.Store
	once sync.Once
	// afterCreate runs once an invitation row has been inserted.
	afterCreate func()
	// beforeUpdate runs before an invitation row is updated (the claim).
	beforeUpdate func()
	// listFails refuses the listing a token revoke sweeps by.
	listFails bool
	// userReadsFail, once set, refuses every account read.
	userReadsFail atomic.Bool
}

func (s *interleavingStore) GetUser(ctx context.Context, id string) (identity.User, error) {
	if s.userReadsFail.Load() {
		return identity.User{}, errors.New("store unreachable")
	}
	return s.Store.GetUser(ctx, id)
}

func (s *interleavingStore) CreateInvitation(ctx context.Context, inv identity.Invitation) error {
	if err := s.Store.CreateInvitation(ctx, inv); err != nil {
		return err
	}
	if s.afterCreate != nil {
		s.once.Do(s.afterCreate)
	}
	return nil
}

func (s *interleavingStore) UpdateInvitation(ctx context.Context, inv identity.Invitation) error {
	if s.beforeUpdate != nil {
		s.once.Do(s.beforeUpdate)
	}
	return s.Store.UpdateInvitation(ctx, inv)
}

func (s *interleavingStore) ListPendingInvitationsMintedWith(ctx context.Context, credentialID string) ([]identity.Invitation, error) {
	if s.listFails {
		return nil, errors.New("store unreachable")
	}
	return s.Store.ListPendingInvitationsMintedWith(ctx, credentialID)
}

// TestInvitations_ATokenRevokedWhileItsInvitationLandsSendsNothing: the
// revoke of a personal access token lands after its invitation row and before
// the email. The revoke's sweep may have listed the pending rows before the
// row landed, so the mint reads the token again before delivery: no email
// leaves, the row is taken back, and the minter hears its token no longer
// stands.
func TestInvitations_ATokenRevokedWhileItsInvitationLandsSendsNothing(t *testing.T) {
	mailer := &recordingMailer{}
	f, patToken := invitationMailFixture(t, mailer)
	f.s.auditStore = audit.NewMemoryStore()
	id := patIDOf(t, f, patToken)
	real := f.s.authStore()
	f.s.authSvc.SetStoreForTest(&interleavingStore{Store: real, afterCreate: func() { revokePAT(t, f, id) }})
	code, _, body := f.call(t, http.MethodPost, "/api/teams/ta/invitations", patToken, `{"email":"a@example.com","role":"member"}`)
	f.s.authSvc.SetStoreForTest(real)
	if code != http.StatusUnauthorized || !strings.Contains(body, "nothing was sent") {
		t.Fatalf("a mint whose token was revoked before delivery: %d %s, want 401 saying nothing was sent", code, body)
	}
	if n := len(mailer.messages()); n != 0 {
		t.Fatalf("%d emails left for an invitation whose token was revoked before delivery", n)
	}
	if n := len(invitationsOfTeam(t, f, "ta")); n != 0 {
		t.Fatalf("%d invitation rows stand after the take-back, want 0", n)
	}
	deleted := waitForTenantAuditRows(t, f.s.auditStore, "ta", "invitation.deleted", 1)
	if deleted[0].Meta["reason"] != "minting_pat_gone" {
		t.Fatalf("the take-back's audit line names %v, want reason minting_pat_gone", deleted[0].Meta)
	}
}

// TestInvitations_AMintThatCannotReadItsTokenSendsNothing: the second read of
// the minting token fails rather than refuses. Nothing is sent, the row is
// taken back, and the trail says the standing could not be read — not that
// the token is gone.
func TestInvitations_AMintThatCannotReadItsTokenSendsNothing(t *testing.T) {
	mailer := &recordingMailer{}
	f, patToken := invitationMailFixture(t, mailer)
	f.s.auditStore = audit.NewMemoryStore()
	real := f.s.authStore()
	st := &interleavingStore{Store: real}
	st.afterCreate = func() { st.userReadsFail.Store(true) }
	f.s.authSvc.SetStoreForTest(st)
	code, _, body := f.call(t, http.MethodPost, "/api/teams/ta/invitations", patToken, `{"email":"a@example.com","role":"member"}`)
	f.s.authSvc.SetStoreForTest(real)
	if code != http.StatusInternalServerError {
		t.Fatalf("a mint whose token could not be read again: %d %s, want 500", code, body)
	}
	if n := len(mailer.messages()); n != 0 {
		t.Fatalf("%d emails left for an invitation whose token could not be confirmed", n)
	}
	if n := len(invitationsOfTeam(t, f, "ta")); n != 0 {
		t.Fatalf("%d invitation rows stand after the take-back, want 0", n)
	}
	deleted := waitForTenantAuditRows(t, f.s.auditStore, "ta", "invitation.deleted", 1)
	if deleted[0].Meta["reason"] != "minting_pat_unconfirmed" {
		t.Fatalf("the take-back's audit line names %v, want reason minting_pat_unconfirmed", deleted[0].Meta)
	}
}

// TestInvitations_AnAcceptOvertakenByTheTokensRevokeWritesNoAccess: the
// revoke of the token that emailed an invitation lands between the accept's
// check of that token and its claim. The claim reads the token again once
// written, so the accept writes no access and answers 410.
func TestInvitations_AnAcceptOvertakenByTheTokensRevokeWritesNoAccess(t *testing.T) {
	mailer := &recordingMailer{}
	f, patToken := invitationMailFixture(t, mailer)
	code, _, body := f.call(t, http.MethodPost, "/api/orgs/o1/invitations", patToken,
		`{"email":"late@example.com","role":"owner","team_id":"ta","org_role":"admin"}`)
	if code != http.StatusOK {
		t.Fatalf("the PAT's invitation: %d %s", code, body)
	}
	tok := acceptTokenOf(t, mailer.messages()[0])
	invitee, err := f.svc.Register(context.Background(), "late@example.com", fixturePassword, "Late", "", "ua", "ip")
	if err != nil {
		t.Fatalf("register the invitee: %v", err)
	}
	id := patIDOf(t, f, patToken)
	real := f.s.authStore()
	f.s.authSvc.SetStoreForTest(&interleavingStore{Store: real, beforeUpdate: func() { revokePAT(t, f, id) }})
	code, _, body = f.call(t, http.MethodPost, "/api/auth/invitations/accept", invitee.AccessToken, `{"token":"`+tok+`"}`)
	f.s.authSvc.SetStoreForTest(real)
	if code != http.StatusGone || !strings.Contains(body, "does not stand") {
		t.Fatalf("an accept overtaken by the token's revoke: %d %s, want 410", code, body)
	}
	ctx := context.Background()
	if _, err := real.GetOrgMembership(ctx, invitee.User.ID, "o1"); err == nil {
		t.Fatal("the overtaken accept wrote an org membership")
	}
	if _, err := real.GetMembership(ctx, invitee.User.ID, "ta"); err == nil {
		t.Fatal("the overtaken accept wrote a team grant")
	}
}

// TestRevokePAT_ASweepWhoseListingFailedAnswersAnEmptyListAndARetry: the
// token is revoked, the listing of what it sent fails, and the answer says
// so with an empty list (never null) and the way out; re-issuing the revoke
// takes the invitation out and names it.
func TestRevokePAT_ASweepWhoseListingFailedAnswersAnEmptyListAndARetry(t *testing.T) {
	mailer := &recordingMailer{}
	f, patToken := invitationMailFixture(t, mailer)
	if code, _, body := f.call(t, http.MethodPost, "/api/teams/ta/invitations", patToken, `{"email":"a@example.com","role":"member"}`); code != http.StatusOK {
		t.Fatalf("the PAT's invitation: %d %s", code, body)
	}
	id := patIDOf(t, f, patToken)
	real := f.s.authStore()
	f.s.authSvc.SetStoreForTest(&interleavingStore{Store: real, listFails: true})
	code, _, body := f.call(t, http.MethodDelete, "/api/me/tokens/"+id, f.user.AccessToken, "")
	f.s.authSvc.SetStoreForTest(real)
	if code != http.StatusOK || !strings.Contains(body, `"invitations_revoked":[]`) || !strings.Contains(body, "re-issuing this revoke") {
		t.Fatalf("a revoke whose sweep could not list: %d %s, want 200 with an empty list and the retry", code, body)
	}
	code, _, body = f.call(t, http.MethodDelete, "/api/me/tokens/"+id, f.user.AccessToken, "")
	if code != http.StatusOK || !strings.Contains(body, "a@example.com") {
		t.Fatalf("the re-issued revoke: %d %s, want 200 naming the invitation it took out", code, body)
	}
	if n := len(invitationsOfTeam(t, f, "ta")); n != 0 {
		t.Fatalf("%d invitations pending after the re-issued revoke", n)
	}
}

// TestRevokePAT_NamesOnlyTheInvitationsTheRevokingPinReaches: the revoke
// reaches any token of the account, but a token pinned to ta reads no
// invitation of team tb. Revoking the account's tb-pinned token from the
// ta-pinned one ends tb's invitation, counts it, and names nothing; the
// tb-pinned token revoking itself, or the session, names it.
func TestRevokePAT_NamesOnlyTheInvitationsTheRevokingPinReaches(t *testing.T) {
	mintTB := func(t *testing.T) (*derivationFixture, string, string, string) {
		f, taPAT := invitationMailFixture(t, &recordingMailer{})
		if _, err := f.s.authStore().CreateTeam(context.Background(), identity.Team{ID: "tb", Name: "TB", Slug: "tb", OrgID: "o1"}); err != nil {
			t.Fatalf("create sibling team tb: %v", err)
		}
		if err := f.s.authStore().UpsertMembership(context.Background(), identity.Membership{UserID: f.user.User.ID, TeamID: "tb", Role: identity.RoleAdmin}); err != nil {
			t.Fatal(err)
		}
		code, _, body := f.call(t, http.MethodPost, "/api/me/tokens", f.user.AccessToken, `{"name":"tb-bot","team_id":"tb"}`)
		var v struct {
			Token string `json:"token"`
			PAT   struct {
				ID string `json:"id"`
			} `json:"pat"`
		}
		if err := json.Unmarshal([]byte(body), &v); err != nil || code != http.StatusCreated {
			t.Fatalf("mint a tb-pinned PAT: %d %s", code, body)
		}
		if code, _, body := f.call(t, http.MethodPost, "/api/teams/tb/invitations", v.Token, `{"email":"tb-invitee@example.com","role":"admin"}`); code != http.StatusOK {
			t.Fatalf("the tb-pinned PAT invites into tb: %d %s", code, body)
		}
		return f, taPAT, v.Token, v.PAT.ID
	}

	t.Run("a token pinned elsewhere counts it", func(t *testing.T) {
		f, taPAT, _, tbID := mintTB(t)
		code, _, body := f.call(t, http.MethodDelete, "/api/me/tokens/"+tbID, taPAT, "")
		if code != http.StatusOK || strings.Contains(body, "tb-invitee@example.com") || !strings.Contains(body, `"invitations_revoked_elsewhere":1`) {
			t.Fatalf("the ta-pinned PAT revoking the tb-pinned one: %d %s, want 200 counting tb's invitation without naming it", code, body)
		}
		if n := len(invitationsOfTeam(t, f, "tb")); n != 0 {
			t.Fatalf("%d invitations pending in tb after their token was revoked", n)
		}
	})
	for _, by := range []string{"itself", "the session"} {
		t.Run("revoked by "+by+", it is named", func(t *testing.T) {
			f, _, tbPAT, tbID := mintTB(t)
			bearer := tbPAT
			if by == "the session" {
				bearer = f.user.AccessToken
			}
			code, _, body := f.call(t, http.MethodDelete, "/api/me/tokens/"+tbID, bearer, "")
			if code != http.StatusOK || !strings.Contains(body, "tb-invitee@example.com") || strings.Contains(body, "invitations_revoked_elsewhere") {
				t.Fatalf("the tb-pinned PAT revoked by %s: %d %s, want 200 naming tb's invitation", by, code, body)
			}
		})
	}
}
