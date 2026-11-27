package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/identity"
)

// The invitation link's two answers the studio builds its pages on: the
// anonymous lookup names who invites and into which org, and an accept of an
// invitation its caller already accepted names the team it led to — to that
// caller alone.

// seedLinkInvitation writes a pending invitation of t2 (a team of org o1,
// "Acme Org") for outsider@x, minted by admin, and answers its token.
func seedLinkInvitation(t *testing.T, i *inviteE2E) string {
	t.Helper()
	ctx := context.Background()
	if _, err := i.s.authStore().CreateOrg(ctx, identity.Org{ID: "o1", Name: "Acme Org", Slug: "acme-org", CreatedAt: time.Now()}); err != nil {
		t.Fatalf("seed org: %v", err)
	}
	if _, err := i.s.authStore().CreateTeam(ctx, identity.Team{ID: "t2", Name: "Two", Slug: "two", OrgID: "o1", CreatedAt: time.Now()}); err != nil {
		t.Fatalf("seed team: %v", err)
	}
	const token = "link-flow-token"
	if err := i.s.authStore().CreateInvitation(ctx, identity.Invitation{
		ID: "inv-link", TeamID: "t2", Email: "outsider@x", Role: identity.RoleMember,
		TokenHash: auth.HashRefreshToken(token), InvitedBy: "admin",
		CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatalf("seed invitation: %v", err)
	}
	return token
}

func TestInvitationLookup_NamesTheOrgAndTheInviter(t *testing.T) {
	i := newInviteE2E(t)
	token := seedLinkInvitation(t, i)

	status, body := i.call(t, http.MethodGet, "/api/auth/invitations/lookup?token="+url.QueryEscape(token), "", "")
	if status != http.StatusOK {
		t.Fatalf("lookup = %d body=%s", status, body)
	}
	var view invitationLookupView
	if err := json.Unmarshal(body, &view); err != nil {
		t.Fatalf("decode lookup: %v (%s)", err, body)
	}
	if view.OrgName != "Acme Org" {
		t.Errorf("org_name = %q, want the name of the team's org", view.OrgName)
	}
	if view.InvitedBy != "admin@x" {
		t.Errorf("invited_by = %q, want the inviter's address the email shows", view.InvitedBy)
	}
	if view.TeamID != "t2" || view.Email != "outsider@x" || view.OrgRole != string(identity.OrgRoleMember) {
		t.Errorf("lookup = %+v, want the invitation as minted", view)
	}
}

// decodeAcceptRefusal reads a refused accept's body.
func decodeAcceptRefusal(t *testing.T, body []byte) invitationAcceptedByYou {
	t.Helper()
	var got invitationAcceptedByYou
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode refusal: %v (%s)", err, body)
	}
	return got
}

func TestInvitationAccept_AnAlreadyAcceptedInvitationNamesItsTeamToItsAccepteeOnly(t *testing.T) {
	i := newInviteE2E(t)
	token := seedLinkInvitation(t, i)
	outsiderJWT := i.jwt(t, "outsider", "", "")
	reqBody := `{"token":"` + token + `"}`

	if status, body := i.call(t, http.MethodPost, "/api/auth/invitations/accept", outsiderJWT, reqBody); status != http.StatusOK {
		t.Fatalf("first accept = %d body=%s", status, body)
	}

	t.Run("the acceptee opening the link again is told the team it joined", func(t *testing.T) {
		status, body := i.call(t, http.MethodPost, "/api/auth/invitations/accept", outsiderJWT, reqBody)
		if status != http.StatusConflict {
			t.Fatalf("replay by the acceptee = %d body=%s, want 409: the invitation stays single-use", status, body)
		}
		got := decodeAcceptRefusal(t, body)
		if got.ErrorCode != errorCodeInvitationAcceptedByYou || got.TeamID != "t2" {
			t.Fatalf("replay by the acceptee answered %+v, want error_code %q naming t2", got, errorCodeInvitationAcceptedByYou)
		}
	})

	// The other account is a member of t2 itself, so only the acceptee check
	// — not the grant read after it — tells it apart from the acceptee.
	t.Run("another account holding the link learns nothing of where it led", func(t *testing.T) {
		if err := i.s.authStore().UpsertMembership(context.Background(), identity.Membership{UserID: "interloper", TeamID: "t2", Role: identity.RoleMember}); err != nil {
			t.Fatalf("grant interloper: %v", err)
		}
		interloperJWT := i.jwt(t, "interloper", "", "")
		status, body := i.call(t, http.MethodPost, "/api/auth/invitations/accept", interloperJWT, reqBody)
		if status != http.StatusConflict {
			t.Fatalf("replay by another account = %d body=%s, want 409", status, body)
		}
		if got := decodeAcceptRefusal(t, body); got.ErrorCode != "" || got.TeamID != "" {
			t.Fatalf("replay by another account answered %+v: the team of someone else's invitation leaked", got)
		}
	})

	t.Run("an acceptee whose grant was removed since reads the plain refusal", func(t *testing.T) {
		if err := i.s.authStore().DeleteMembership(context.Background(), "outsider", "t2"); err != nil {
			t.Fatalf("remove grant: %v", err)
		}
		status, body := i.call(t, http.MethodPost, "/api/auth/invitations/accept", outsiderJWT, reqBody)
		if status != http.StatusConflict {
			t.Fatalf("replay after removal = %d body=%s, want 409", status, body)
		}
		if got := decodeAcceptRefusal(t, body); got.ErrorCode != "" || got.TeamID != "" {
			t.Fatalf("replay after removal answered %+v: it points at a team the account no longer holds", got)
		}
		if _, err := i.s.authStore().GetMembership(context.Background(), "outsider", "t2"); err == nil {
			t.Fatal("the replay wrote the removed grant back: the spent invitation granted twice")
		}
	})
}

// namesUnreadableStore fails the org and/or user reads the lookup makes for
// its display names, as an unreachable store would.
type namesUnreadableStore struct {
	identity.Store
	failOrg, failUser bool
}

var errNamesUnreadable = errors.New("store unreachable")

func (s *namesUnreadableStore) GetOrg(ctx context.Context, id string) (identity.Org, error) {
	if s.failOrg {
		return identity.Org{}, errNamesUnreadable
	}
	return s.Store.GetOrg(ctx, id)
}

func (s *namesUnreadableStore) GetUser(ctx context.Context, id string) (identity.User, error) {
	if s.failUser {
		return identity.User{}, errNamesUnreadable
	}
	return s.Store.GetUser(ctx, id)
}

// The org name and the inviter are display only: a read of either that
// fails leaves it out, and the invitation is still described — the accept
// does not need them, so neither does the form that leads to it.
func TestInvitationLookup_AnUnreadableNameIsLeftOutNotRefused(t *testing.T) {
	for _, tc := range []struct {
		name              string
		failOrg, failUser bool
	}{
		{name: "the org", failOrg: true},
		{name: "the inviter", failUser: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			i := newInviteE2E(t)
			token := seedLinkInvitation(t, i)
			real := i.s.authSvc.Store()
			i.s.authSvc.SetStoreForTest(&namesUnreadableStore{Store: real, failOrg: tc.failOrg, failUser: tc.failUser})
			t.Cleanup(func() { i.s.authSvc.SetStoreForTest(real) })

			status, body := i.call(t, http.MethodGet, "/api/auth/invitations/lookup?token="+url.QueryEscape(token), "", "")
			if status != http.StatusOK {
				t.Fatalf("lookup with %s unreadable = %d body=%s, want 200: a display name refused the invitation", tc.name, status, body)
			}
			var view invitationLookupView
			if err := json.Unmarshal(body, &view); err != nil {
				t.Fatalf("decode lookup: %v (%s)", err, body)
			}
			if tc.failOrg && view.OrgName != "" {
				t.Errorf("org_name = %q from a failed read", view.OrgName)
			}
			if tc.failUser && view.InvitedBy != "" {
				t.Errorf("invited_by = %q from a failed read", view.InvitedBy)
			}
			if view.TeamID != "t2" || view.Email != "outsider@x" {
				t.Errorf("lookup = %+v, want the invitation still described", view)
			}
		})
	}
}
