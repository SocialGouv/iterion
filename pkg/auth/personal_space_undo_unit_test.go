package auth

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth/oidc"
	"github.com/SocialGouv/iterion/pkg/identity"
)

// createPersonalTeam used to leave behind whatever its four writes had
// committed: a signup whose owner grant failed left a personal ORG and a
// personal TEAM holding the slugs derived from the email, under an account
// that could reach neither. It is the defect CreateOrgFor killed one level up
// (cca8ed4a0), at the three callers that provision a personal space —
// registerOpen, the SSO first login and the bootstrap. The compensation is
// the one CreateOrgFor uses: provisionUndo, each step bounded to an id this
// call minted.
//
// What it does NOT undo is the ACCOUNT row its callers wrote first: the
// identity store has no account delete (#2655 R1, blocked), so the price is
// asserted as a price, not as a pass.

// seedPersonalProvisioner answers a service whose store is the recording
// org-provision gate, and the account row the personal space is provisioned
// for. The account is written BEHIND the gate, so the writes the gate counts
// are the space's own four.
func seedPersonalProvisioner(t *testing.T) (*Service, *orgProvisionStore, identity.User) {
	t.Helper()
	svc := newTestService(t, SignupOpen)
	u, err := svc.store.CreateUser(context.Background(), identity.User{
		ID: "u-alice", Email: "alice@example.com", Name: "Alice",
		Status: identity.UserStatusActive, CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("seed the account: %v", err)
	}
	gate := newOrgProvisionStore(svc.store)
	svc.SetStoreForTest(gate)
	gate.reset()
	return svc, gate, u
}

// personalSpaceWrites names the four writes in the order they commit, so a
// failing sub-test says which one it armed.
var personalSpaceWrites = []string{
	"first (the personal org)",
	"second (the owner org membership)",
	"third (the personal team)",
	"fourth (the owner team grant)",
}

func TestCreatePersonalTeam_aFailedWriteLeavesNoSpaceAndTheRetryKeepsTheSlug(t *testing.T) {
	ctx := context.Background()
	for k := 1; k <= 4; k++ {
		t.Run(fmt.Sprintf("the %s write fails", personalSpaceWrites[k-1]), func(t *testing.T) {
			svc, gate, u := seedPersonalProvisioner(t)
			gate.failNthWrite(k)

			if _, err := svc.createPersonalTeam(ctx, u); err == nil {
				t.Fatalf("write %d failed and the provisioning answered success", k)
			} else if !errors.Is(err, errStoreUnreachable) {
				t.Fatalf("err = %v, want the store failure of write %d", err, k)
			}
			base := identity.SlugifyTeamName(u.Email)
			assertNoPersonalSpace(t, svc, u, base, fmt.Sprintf("write %d", k))

			// The retry — the next signup attempt, or the next SSO login of
			// the same person — gets the slugs derived from their email, not
			// <base>-2 pushed by the orphans of the attempt that failed.
			gate.disarm()
			space, err := svc.createPersonalTeam(ctx, u)
			if err != nil {
				t.Fatalf("the retry after write %d failed: %v", k, err)
			}
			if space.team.Slug != base {
				t.Fatalf("the retry after write %d answered team slug %q, want %q: the first attempt left a team row holding it", k, space.team.Slug, base)
			}
			org, err := svc.store.GetOrg(ctx, space.orgID)
			if err != nil {
				t.Fatalf("read the org the retry created: %v", err)
			}
			if org.Slug != base {
				t.Fatalf("the retry after write %d answered org slug %q, want %q: the first attempt left an org row holding it", k, org.Slug, base)
			}
		})
	}
}

// TestCreatePersonalTeam_aProvisioningThatSucceedsCompensatesNothing is the
// mirror: the compensation must fire on the failing direction only, or a
// green suite would also fit a signup that undoes the space it just wrote.
func TestCreatePersonalTeam_aProvisioningThatSucceedsCompensatesNothing(t *testing.T) {
	ctx := context.Background()
	svc, gate, u := seedPersonalProvisioner(t)

	space, err := svc.createPersonalTeam(ctx, u)
	if err != nil {
		t.Fatalf("createPersonalTeam: %v", err)
	}
	seq := gate.sequence()
	for _, c := range seq {
		if strings.HasPrefix(c, "undo ") {
			t.Fatalf("a successful provisioning ran a compensation: %v", seq)
		}
	}
	if _, err := svc.store.GetTeam(ctx, space.team.ID); err != nil {
		t.Fatalf("the personal team it answered is not in the store: %v", err)
	}
	if _, err := svc.store.GetMembership(ctx, u.ID, space.team.ID); err != nil {
		t.Fatalf("the owner grant is not in the store: %v", err)
	}
	if _, err := svc.store.GetOrgMembership(ctx, u.ID, space.orgID); err != nil {
		t.Fatalf("the owner org membership is not in the store: %v", err)
	}
}

// TestRegisterOpen_aFailedPersonalSpaceLeavesNoOrphanOrgOrTeam is the public
// caller's own isolate — two callers, two scenarios, or neither is proven —
// and it states the residue the compensation cannot reach.
func TestRegisterOpen_aFailedPersonalSpaceLeavesNoOrphanOrgOrTeam(t *testing.T) {
	ctx := context.Background()
	const email = "alice@example.com"
	for k := 1; k <= 4; k++ {
		t.Run(fmt.Sprintf("the %s write fails", personalSpaceWrites[k-1]), func(t *testing.T) {
			svc := newTestService(t, SignupOpen)
			gate := newOrgProvisionStore(svc.store)
			svc.SetStoreForTest(gate)
			gate.reset()
			gate.failNthWrite(k)

			if _, err := svc.Register(ctx, email, "correcthorse", "Alice", "", "ua", "ip"); err == nil {
				t.Fatalf("write %d failed and the registration answered success", k)
			} else if !errors.Is(err, errStoreUnreachable) {
				t.Fatalf("err = %v, want the store failure of write %d", err, k)
			}
			u, err := svc.store.GetUserByEmail(ctx, email)
			if err != nil {
				t.Fatalf("read the account the registration wrote: %v", err)
			}
			assertNoPersonalSpace(t, svc, u, identity.SlugifyTeamName(email), fmt.Sprintf("write %d", k))

			// The price this change does NOT pay: the account row stands —
			// no account delete exists in the identity store (#2655 R1) — so
			// the email is taken and the retry answers the permanent 409.
			// Asserted as the price it is, so a future R1 has to update it.
			gate.disarm()
			if _, err := svc.Register(ctx, email, "correcthorse", "Alice", "", "ua", "ip"); !errors.Is(err, identity.ErrEmailAlreadyTaken) {
				t.Fatalf("the retry after write %d answered %v, want ErrEmailAlreadyTaken: the account row of the failed attempt stands until an account delete exists", k, err)
			}
			// The whole price, which the 409 alone understates: that account
			// SIGNS IN, with the password it registered, and lands nowhere —
			// no active team, no active org, and no path provisions a space
			// for it afterwards (createPersonalTeam's three callers all need
			// the account or the SSO link to be absent). An operator has to
			// invite it to a team. The lifecycle page states this; it is
			// asserted so a change of either has to move both.
			res, err := svc.Login(ctx, email, "correcthorse", "ua", "ip")
			if err != nil {
				t.Fatalf("after write %d the compensated account could not sign in: %v — the lifecycle page says it can", k, err)
			}
			if res.ActiveTeamID != "" || res.ActiveOrgID != "" {
				t.Fatalf("after write %d the compensated account signed in with team %q org %q, want neither: the compensation left a row the login resolved", k, res.ActiveTeamID, res.ActiveOrgID)
			}
		})
	}
}

// TestLoginWithExternal_aFailedPersonalSpaceLeavesNoOrphanAndTheRetrySignsIn
// is the third caller: the SSO first login of a provider nothing gates. Its
// retry really does succeed — the SSO link was written BEFORE the space, so
// the second attempt is a returning user, and it must not find the slugs of
// the attempt that failed.
func TestLoginWithExternal_aFailedPersonalSpaceLeavesNoOrphanAndTheRetrySignsIn(t *testing.T) {
	ctx := context.Background()
	const email = "alice@example.com"
	for k := 1; k <= 4; k++ {
		t.Run(fmt.Sprintf("the %s write fails", personalSpaceWrites[k-1]), func(t *testing.T) {
			svc := newTestService(t, SignupOpen)
			gate := newOrgProvisionStore(svc.store)
			svc.SetStoreForTest(gate)
			gate.reset()
			gate.failNthWrite(k)
			ext := oidc.ExternalUser{Provider: "sso", Subject: "kc-alice", Email: email, Name: "Alice"}

			if _, err := svc.LoginWithExternal(ctx, ext, "ua", "ip"); err == nil {
				t.Fatalf("write %d failed and the first login answered success", k)
			} else if !errors.Is(err, errStoreUnreachable) {
				t.Fatalf("err = %v, want the store failure of write %d", err, k)
			}
			u, err := svc.store.GetUserByEmail(ctx, email)
			if err != nil {
				t.Fatalf("read the account the first login wrote: %v", err)
			}
			assertNoPersonalSpace(t, svc, u, identity.SlugifyTeamName(email), fmt.Sprintf("write %d", k))

			gate.disarm()
			res, err := svc.LoginWithExternal(ctx, ext, "ua", "ip")
			if err != nil {
				t.Fatalf("the retry after write %d could not sign in: %v", k, err)
			}
			if res.User.ID != u.ID {
				t.Fatalf("the retry after write %d signed in as %q, want the account of the first attempt %q", k, res.User.ID, u.ID)
			}
		})
	}
}

// assertNoPersonalSpace is the invariant the compensation owes: no org, no
// team and no owner row of a personal space that did not finish survives.
func assertNoPersonalSpace(t *testing.T, svc *Service, u identity.User, base, what string) {
	t.Helper()
	ctx := context.Background()
	if o, err := svc.store.GetOrgBySlug(ctx, base); !errors.Is(err, identity.ErrNotFound) {
		t.Fatalf("%s left the personal org %+v standing (read err %v): its slug is taken and the retry answers %s-2", what, o, err, base)
	}
	if tm, err := svc.store.GetTeamBySlug(ctx, base); !errors.Is(err, identity.ErrNotFound) {
		t.Fatalf("%s left the personal team %+v standing (read err %v): its slug is taken and the retry answers %s-2", what, tm, err, base)
	}
	ms, err := svc.store.ListMembershipsByUser(ctx, u.ID)
	if err != nil {
		t.Fatalf("read the memberships of %s: %v", u.ID, err)
	}
	if len(ms) != 0 {
		t.Fatalf("%s left the team grant(s) %+v on an account with no team", what, ms)
	}
	oms, err := svc.store.ListOrgMembershipsByUser(ctx, u.ID)
	if err != nil {
		t.Fatalf("read the org memberships of %s: %v", u.ID, err)
	}
	if len(oms) != 0 {
		t.Fatalf("%s left the org membership(s) %+v on an account with no org", what, oms)
	}
}

// TestCreatePersonalSpaceIn_everyCommittedWriteHasItsUndo: the write phase
// pushes the undo of all FOUR writes, including the last one — the owner
// grant, whose undo no failure of this unit can reach, since nothing is
// written after it. The stack is what makes appending a write safe (#2655 R1
// will append an account delete under it), so it is asserted directly:
// compensating a COMPLETED space removes every row it wrote.
func TestCreatePersonalSpaceIn_everyCommittedWriteHasItsUndo(t *testing.T) {
	ctx := context.Background()
	svc, _, u := seedPersonalProvisioner(t)
	undo := &provisionUndo{}

	space, err := svc.createPersonalSpaceIn(ctx, u, time.Now().UTC(), undo)
	if err != nil {
		t.Fatalf("createPersonalSpaceIn: %v", err)
	}
	if len(undo.steps) != 4 {
		t.Fatalf("the undo stack holds %d steps, want one per committed write: %+v", len(undo.steps), undo.steps)
	}
	cause := errors.New("a write the CALLER added after the space failed")
	if err := undo.compensate(ctx, cause); !errors.Is(err, cause) {
		t.Fatalf("the compensation lost the cause: %v", err)
	} else if strings.Contains(err.Error(), "could not remove") {
		t.Fatalf("the compensation of a completed space could not remove a row it wrote: %v", err)
	}
	if _, gerr := svc.store.GetTeam(ctx, space.team.ID); !errors.Is(gerr, identity.ErrNotFound) {
		t.Errorf("the personal team survived the compensation (read err %v)", gerr)
	}
	assertNoPersonalSpace(t, svc, u, identity.SlugifyTeamName(u.Email), "the compensation of a completed space")
}
