package auth

import (
	"context"
	"errors"
	"fmt"

	"github.com/SocialGouv/iterion/pkg/identity"
)

// grantGuard is the identity store the Service writes through: no team or
// organization membership lands on an account under erasure. Every grant —
// an org or a team created with its owner, an invitation accepted, a member
// added by an admin, an SSO group sync — ends in UpsertMembership or
// UpsertOrgMembership, so the guard sits on those two writes rather than at
// each of their callers.
//
// A request that read the account before the erasure marked it can write
// after: the guard refuses before the write when the account already
// carries the erased status, and reads the status again after it. A grant
// that finds the mark then is taken back and refused, so the caller's own
// undo compensates what it provisioned around it (CreateOrgFor deletes the
// org it just created). A grant whose second read lands before the mark is
// the erasure's to find: its sweep releases an ownership with the recount
// of the last-owner safeguard (sweepAccountGrants). One of the two reads
// always sees the other write.
type grantGuard struct {
	identity.Store
}

// erasureCompensationKey marks the context of the erasure's own
// compensation: putting back an ownership it released is the one membership
// write an account under erasure receives.
type erasureCompensationKey struct{}

func withErasureCompensation(ctx context.Context) context.Context {
	return context.WithValue(ctx, erasureCompensationKey{}, true)
}

func isErasureCompensation(ctx context.Context) bool {
	v, _ := ctx.Value(erasureCompensationKey{}).(bool)
	return v
}

// grantTakeBackTimeout bounds the delete of a refused grant once it is
// detached from the request: a client that went away must not leave the
// grant standing.
const grantTakeBackTimeout = provisionUndoTimeout

func (g grantGuard) UpsertMembership(ctx context.Context, m identity.Membership) error {
	what := "a membership of team " + m.TeamID
	if err := g.refuseErased(ctx, m.UserID, what); err != nil {
		return err
	}
	if err := g.Store.UpsertMembership(ctx, m); err != nil {
		return err
	}
	return g.takeBackIfErased(ctx, m.UserID, what, func(c context.Context) error {
		return g.DeleteMembership(c, m.UserID, m.TeamID)
	})
}

func (g grantGuard) UpsertOrgMembership(ctx context.Context, m identity.OrgMembership) error {
	what := "a membership of org " + m.OrgID
	if err := g.refuseErased(ctx, m.UserID, what); err != nil {
		return err
	}
	if err := g.Store.UpsertOrgMembership(ctx, m); err != nil {
		return err
	}
	return g.takeBackIfErased(ctx, m.UserID, what, func(c context.Context) error {
		return g.DeleteOrgMembership(c, m.UserID, m.OrgID)
	})
}

// refuseErased answers ErrGrantRefused when userID's account carries the
// erased status. An id with no account row holds nothing to erase.
func (g grantGuard) refuseErased(ctx context.Context, userID, what string) error {
	if isErasureCompensation(ctx) {
		return nil
	}
	erased, err := g.erased(ctx, userID)
	if err != nil {
		return fmt.Errorf("auth: grant %s to user %s: %w", what, userID, err)
	}
	if erased {
		return fmt.Errorf("%w: user %s is being erased — %s is not granted", ErrGrantRefused, userID, what)
	}
	return nil
}

// takeBackIfErased reads userID's status again once the grant is written,
// and deletes the grant when the account carries the erased status or the
// read fails: a grant nobody could clear stays out.
func (g grantGuard) takeBackIfErased(ctx context.Context, userID, what string, del func(context.Context) error) error {
	if isErasureCompensation(ctx) {
		return nil
	}
	erased, err := g.erased(ctx, userID)
	if err == nil && !erased {
		return nil
	}
	cause := fmt.Errorf("%w: user %s is being erased — %s is taken back", ErrGrantRefused, userID, what)
	if err != nil {
		cause = fmt.Errorf("auth: re-read user %s after granting %s, so the grant is taken back: %w", userID, what, err)
	}
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), grantTakeBackTimeout)
	defer cancel()
	if derr := del(dctx); derr != nil && !errors.Is(derr, identity.ErrNotFound) {
		return errors.Join(cause, fmt.Errorf("auth: take back %s of user %s: %w", what, userID, derr))
	}
	return cause
}

func (g grantGuard) erased(ctx context.Context, userID string) (bool, error) {
	u, err := g.GetUser(ctx, userID)
	if errors.Is(err, identity.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read the account: %w", err)
	}
	return u.Status == identity.UserStatusErased, nil
}
