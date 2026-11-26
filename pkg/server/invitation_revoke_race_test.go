package server

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

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
