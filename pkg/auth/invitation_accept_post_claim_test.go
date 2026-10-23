package auth

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/identity"
)

// acceptFixture is a registered account with no standing in org o1 and an
// invitation to its team tb, on a gated identity store: the accept path's
// own shape, where every row the accept writes is one it does not yet hold.
type acceptFixture struct {
	svc  *Service
	ids  *nthCallGate
	user identity.User
}

// raw is the identity store BEHIND the gate: a test seeds and inspects rows
// through it, so those calls are neither counted nor failed.
func (f *acceptFixture) raw() identity.Store { return f.ids.Store }

func newAcceptFixture(t *testing.T) *acceptFixture {
	t.Helper()
	ctx := context.Background()
	ids := newNthCallGate(identity.NewMemoryStore(), errStoreUnreachable)
	svc := newTestServiceOn(t, ids, NewMemorySessionStore())
	seed := ids.Store
	if _, err := seed.CreateOrg(ctx, identity.Org{ID: "o1", Name: "Org One", Slug: "org-one", CreatedAt: time.Now()}); err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	if _, err := seed.CreateTeam(ctx, identity.Team{ID: "tb", OrgID: "o1", Name: "Team B", Slug: "team-b"}); err != nil {
		t.Fatalf("CreateTeam: %v", err)
	}
	reg, err := svc.Register(ctx, "invitee@example.org", "correct-horse-9", "", "", "ua", "ip")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	return &acceptFixture{svc: svc, ids: ids, user: reg.User}
}

// invite mints an invitation to tb for the fixture's account and returns its
// token.
func (f *acceptFixture) invite(t *testing.T) string {
	t.Helper()
	token, _, err := f.svc.CreateInvitation(context.Background(), "tb", f.user.Email, identity.RoleMember, "someone-else")
	if err != nil {
		t.Fatalf("CreateInvitation: %v", err)
	}
	return token
}

// TestAcceptInvitationForExistingUser_AFailedReadAfterTheClaimBurnsTheLink
// drives the one failure claimInvitation's order deliberately pays for: the
// accept re-reads the org boundary AFTER it has claimed the invitation — so
// that an off-boarding landing in between finds the membership written back
// with the grant — and a read that fails there answers an error with the
// invitation already spent.
//
// The state it leaves is this test's subject: the row says "accepted by this
// account", the account holds nothing, the same link is refused as already
// accepted, and only a NEW invitation gets the user in. Un-claiming is not
// the answer (see claimInvitation): re-opening a row an off-boarding sweep
// skipped as accepted would hand a removed account a live re-entry ticket.
func TestAcceptInvitationForExistingUser_AFailedReadAfterTheClaimBurnsTheLink(t *testing.T) {
	for _, read := range []string{"GetTeam", "GetOrgMembership"} {
		t.Run(read, func(t *testing.T) {
			ctx := context.Background()
			f := newAcceptFixture(t)
			token := f.invite(t)
			// The accept plans the grant twice: once before the claim, once
			// after it. Counting from where the mint left the gate makes
			// "the read after the claim" the second of the accept's own.
			base := f.ids.count(read)
			f.ids.fail(read, base+2)
			_, err := f.svc.AcceptInvitationForExistingUser(ctx, f.user.ID, token)
			if !errors.Is(err, errStoreUnreachable) {
				t.Fatalf("the accept whose %s after the claim failed: got %v, want the read's failure", read, err)
			}
			f.ids.disarm()
			inv, err := f.raw().GetInvitationByTokenHash(ctx, hashInvitationToken(token))
			if err != nil {
				t.Fatalf("read the invitation back: %v", err)
			}
			if inv.AcceptedAt == nil || inv.AcceptedBy != f.user.ID {
				t.Fatalf("the failed accept left the invitation accepted_at=%v accepted_by=%q, want it SPENT by %s (the claim landed)",
					inv.AcceptedAt, inv.AcceptedBy, f.user.ID)
			}
			if _, err := f.raw().GetMembership(ctx, f.user.ID, "tb"); !errors.Is(err, identity.ErrNotFound) {
				t.Fatalf("the failed accept wrote a team grant: %v", err)
			}
			if _, err := f.raw().GetOrgMembership(ctx, f.user.ID, "o1"); !errors.Is(err, identity.ErrNotFound) {
				t.Fatalf("the failed accept wrote an org membership: %v", err)
			}
			// The same link, with the store answering, is refused: the row IS
			// the claim, so the invitee cannot retry their way in.
			if _, err := f.svc.AcceptInvitationForExistingUser(ctx, f.user.ID, token); !errors.Is(err, identity.ErrInvitationUsed) {
				t.Fatalf("the same link retried: got %v, want ErrInvitationUsed", err)
			}
			// A NEW invitation is the recovery, and it works.
			mb, err := f.svc.AcceptInvitationForExistingUser(ctx, f.user.ID, f.invite(t))
			if err != nil {
				t.Fatalf("a new invitation accepted: %v", err)
			}
			if mb.TeamID != "tb" || mb.Role != identity.RoleMember {
				t.Fatalf("the new invitation granted team=%q role=%q, want tb/member", mb.TeamID, mb.Role)
			}
			if om, err := f.raw().GetOrgMembership(ctx, f.user.ID, "o1"); err != nil || om.Role != identity.OrgRoleMember {
				t.Fatalf("the new invitation's org mirror: role=%q err=%v, want member", om.Role, err)
			}
		})
	}
}
