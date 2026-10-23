package auth

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/identity"
)

// The password flows mint a session after a commit — the new password, a
// consumed reset link — and every read that session needs runs BEFORE that
// commit. A read failing there fails the flow with nothing changed: answered
// after the commit instead, it would report a failure for a password that
// did change (and sessions that were revoked), or burn a reset link.

// TestChangePassword_ReadFailureChangesNothing drives the authenticated
// change with each read failing: the error comes back, the old password
// still signs in, and the session the change ran in still refreshes.
func TestChangePassword_ReadFailureChangesNothing(t *testing.T) {
	for _, read := range []string{"GetUser", "ListMembershipsByUser", "ListOrgMembershipsByUser"} {
		t.Run(read, func(t *testing.T) {
			ctx := context.Background()
			f := newRotationFixture(t, errStoreUnreachable)
			f.ids.fail(read, f.ids.count(read)+1)
			_, err := f.svc.ChangePassword(ctx, f.userID, "correct-horse-9", "another-horse-7", "ua", "ip")
			if !errors.Is(err, errStoreUnreachable) {
				t.Fatalf("a password change whose %s fails: got %v, want that read's error", read, err)
			}
			f.ids.disarm()
			if _, err := f.svc.Login(ctx, "rotation@example.org", "correct-horse-9", "ua", "ip"); err != nil {
				t.Fatalf("the password changed under an error answer: the old one no longer signs in (%v)", err)
			}
			if s := f.session(t, f.token); s.RevokedAt != nil {
				t.Fatal("the session the failed change ran in was revoked under an error answer")
			}
		})
	}
}

// TestConfirmPasswordReset_ReadFailureKeepsTheLink drives the reset confirm
// with each read failing: the error comes back as the failure it is — not
// "invalid credentials" — and the same link still resets once the store
// answers.
func TestConfirmPasswordReset_ReadFailureKeepsTheLink(t *testing.T) {
	for _, read := range []string{"GetUser", "ListMembershipsByUser", "GetTeam"} {
		t.Run(read, func(t *testing.T) {
			ctx := context.Background()
			svc, mailer, store := newResetTestService(t)
			if err := svc.RequestPasswordReset(ctx, "user@example.org"); err != nil {
				t.Fatalf("RequestPasswordReset: %v", err)
			}
			msg, ok := mailer.last()
			if !ok {
				t.Fatal("no reset email sent")
			}
			token := extractResetToken(t, msg)
			personalTeam(t, svc, store, "u1")
			gate := newNthCallGate(store, context.Canceled).fail(read, 1)
			svc.SetStoreForTest(gate)
			if _, err := svc.ConfirmPasswordReset(ctx, token, "brand-new-password-1", "ua", "ip"); !errors.Is(err, context.Canceled) {
				t.Fatalf("a reset confirm whose %s fails: got %v, want that read's error", read, err)
			}
			gate.disarm()
			if _, err := svc.ConfirmPasswordReset(ctx, token, "brand-new-password-1", "ua", "ip"); err != nil {
				t.Fatalf("the link the failed confirm was given no longer resets: %v", err)
			}
		})
	}
}

// TestChangePasswordPending_ReadFailureChangesNothing drives the forced
// rotation with a session read failing: the error comes back, and the
// account is still pending on its temporary password.
func TestChangePasswordPending_ReadFailureChangesNothing(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t, SignupOpen)
	hash, err := HashPassword("temporary-pw-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.store.CreateUser(ctx, identity.User{
		ID: "pending", Email: "pending@example.org", PasswordHash: hash, Status: identity.UserStatusPendingPasswordChange,
	}); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	gate := newNthCallGate(svc.store, errStoreUnreachable).fail("ListMembershipsByUser", 1)
	svc.SetStoreForTest(gate)
	if _, err := svc.ChangePasswordPending(ctx, "pending@example.org", "temporary-pw-1", "chosen-password-2", "ua", "ip"); !errors.Is(err, errStoreUnreachable) {
		t.Fatalf("a forced rotation whose memberships read fails: got %v, want that read's error", err)
	}
	gate.disarm()
	u, err := svc.store.GetUser(ctx, "pending")
	if err != nil {
		t.Fatalf("GetUser: %v", err)
	}
	if u.Status != identity.UserStatusPendingPasswordChange {
		t.Fatalf("a failed forced rotation moved the account to %q", u.Status)
	}
	if _, err := svc.ChangePasswordPending(ctx, "pending@example.org", "temporary-pw-1", "chosen-password-2", "ua", "ip"); err != nil {
		t.Fatalf("the temporary password no longer completes the rotation after the failed one: %v", err)
	}
}

// personalTeam gives userID a team with a parent org, so a session's org
// resolution reads the team and the org membership.
func personalTeam(t *testing.T, svc *Service, store identity.Store, userID string) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	if _, err := store.CreateOrg(ctx, identity.Org{ID: "org-" + userID, Name: "Org", Slug: "org-" + userID, CreatedAt: now}); err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	if _, err := store.CreateTeam(ctx, identity.Team{ID: "team-" + userID, OrgID: "org-" + userID, Name: "Team", Slug: "team-" + userID, CreatedAt: now}); err != nil {
		t.Fatalf("CreateTeam: %v", err)
	}
	if err := store.UpsertMembership(ctx, identity.Membership{UserID: userID, TeamID: "team-" + userID, Role: identity.RoleOwner, JoinedAt: now}); err != nil {
		t.Fatalf("UpsertMembership: %v", err)
	}
	if err := store.UpsertOrgMembership(ctx, identity.OrgMembership{UserID: userID, OrgID: "org-" + userID, Role: identity.OrgRoleOwner, JoinedAt: now}); err != nil {
		t.Fatalf("UpsertOrgMembership: %v", err)
	}
}
