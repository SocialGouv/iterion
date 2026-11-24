package auth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/SocialGouv/iterion/pkg/identity"
)

// ErrLastOrgOwner refuses an erasure that would leave an organization with
// no owner: ownership is transferred first, or the organization deleted.
// The message names the organization.
var ErrLastOrgOwner = errors.New("auth: the account is the last owner of an organization")

// ErrGrantRefused refuses a grant to an account no session opens on (a
// disabled or an erased one, identity.UserStatus.SignInRefused): such an
// account owns no new organization. The message names the account and its
// status.
var ErrGrantRefused = errors.New("auth: the account may be granted nothing")

// EraseResult is what an erasure removed, for the audit line and the
// console: every count is of this call, so a repeated erasure reads zeros.
type EraseResult struct {
	// User is the row as it stands after the erasure (identity.Store.EraseUser).
	User identity.User
	// AlreadyErased reports that the row was anonymized before this call:
	// the call swept what an earlier, interrupted erasure may have left and
	// changed the row itself not at all.
	AlreadyErased bool
	// Marked reports that the row carries the erased status — the erasure's
	// first write landed, by this call or an earlier one. EraseUser answers
	// it with an error too: an erasure that fails past it is an act begun,
	// which the caller records as incomplete before answering the failure.
	Marked                 bool
	SessionsDeleted        int64
	SSOLinksRemoved        int64
	MembershipsRemoved     int64
	OrgMembershipsRemoved  int64
	InvitationsRemoved     int64
	PersonalOrgsMarkedGone []string
}

// EraseOptions parameterize EraseUser.
type EraseOptions struct {
	// PersonalOrgGrace is the grace MarkOrgForDeletion gives the account's
	// personal organizations before the nightly purge removes them.
	PersonalOrgGrace time.Duration
	// BeforeAnonymize ends what the account holds in stores this service
	// does not own (personal access tokens, credentials, push
	// subscriptions, notes, audit rows). It runs after every refusal, once
	// the row carries the erased status and before it loses its address,
	// and receives the row with that address — so an erasure a failure
	// interrupted is completed, on the next call, with the address still
	// at hand. Its error aborts the erasure before the anonymization; it
	// must be idempotent. Nil runs nothing.
	BeforeAnonymize func(ctx context.Context, u identity.User) error
}

// EraseUser erases the account userID by anonymization — the identity half of
// a GDPR art. 17 erasure, docs/adr: "identity data lifecycle". What it does,
// in this order, every step idempotent so an erasure a failure interrupted
// completes on the next call:
//
//  1. refuses, before any write, when the account is the last owner of an
//     organization that is not personal and not already pending deletion
//     (ErrLastOrgOwner names it);
//  2. releases its ownership of every such organization, and recounts the
//     owners once its membership is gone: an erasure or a demotion of the
//     other owner racing this one leaves none, so the membership is put
//     back and the erasure refused (ErrLastOrgOwner) — never an
//     organization without an owner;
//  3. marks the row erased (identity.Store.MarkUserErased), its address
//     kept: no sign-in of any kind, no refresh and no patch opens on it
//     from here, so nothing the steps below remove is re-created by the
//     account itself;
//  4. marks the account's personal organizations for deletion, under
//     opts.PersonalOrgGrace, for the nightly purge to remove with everything
//     in them;
//  5. records the account's session revocation, which refuses every access
//     token minted before it (revokeAccount);
//  6. removes the invitations addressed to the row's address, then every
//     team and organization membership — an ownership of a shared org
//     granted since step 1 is released with step 2's recount, and refused
//     (ErrLastOrgOwner) when no other owner remains — then every SSO link;
//  7. runs opts.BeforeAnonymize, with the row's address still on it;
//  8. anonymizes the row (identity.Store.EraseUser): email replaced with its
//     marker, name, password, GitHub orgs cleared;
//  9. sweeps step 6 again, and DELETES the account's session rows, which
//     carry the IP and user agent of each sign-in: a sign-in that read the
//     account before step 3 and wrote after step 6 — a link, a membership,
//     a session — is removed here.
//
// The id stays: every owner_id, actor_id and created_by that names it keeps
// naming a pseudonym. On an error, the result answers how far the erasure
// went (Marked and the counts so far).
func (s *Service) EraseUser(ctx context.Context, userID string, opts EraseOptions) (EraseResult, error) {
	if userID == "" {
		return EraseResult{}, fmt.Errorf("auth: erase user: user id required")
	}
	u, err := s.store.GetUser(ctx, userID)
	if err != nil {
		return EraseResult{}, err
	}
	res := EraseResult{
		AlreadyErased: u.Status == identity.UserStatusErased && identity.IsErasedEmail(u.Email),
		Marked:        u.Status == identity.UserStatusErased,
	}

	orgMems, err := s.store.ListOrgMembershipsByUser(ctx, userID)
	if err != nil {
		return res, fmt.Errorf("auth: erase user %s: list its org memberships: %w", userID, err)
	}
	// Every refusal before any write: an erasure refused half-way would
	// leave an account that can sign in with fewer memberships than it
	// had, and no answer naming why.
	var personal []identity.Org
	var owned []identity.OrgMembership
	for _, m := range orgMems {
		o, kind, err := s.classifyOwnership(ctx, m)
		if err != nil {
			return res, fmt.Errorf("auth: erase user %s: %w", userID, err)
		}
		if kind == personalOwnership {
			personal = append(personal, o)
			continue
		}
		if kind != sharedOwnership {
			continue
		}
		others, err := s.otherOrgOwners(ctx, o.ID, userID)
		if err != nil {
			return res, fmt.Errorf("auth: erase user %s: %w", userID, err)
		}
		if others == 0 {
			return res, lastOwnerRefusal(o)
		}
		owned = append(owned, m)
	}
	released, err := s.releaseOwnerships(ctx, userID, owned, &res)
	if err != nil {
		return res, err
	}

	if !res.Marked {
		if _, err := s.store.MarkUserErased(ctx, userID); err != nil {
			// The account stays active: what step 2 released is put back,
			// so the failed erasure leaves it as it was.
			return res, errors.Join(
				fmt.Errorf("auth: erase user %s: mark the row erased: %w", userID, err),
				s.restoreOwnerships(ctx, released, &res))
		}
		res.Marked = true
	}

	for _, o := range personal {
		if o.Status == identity.TeamStatusPendingDeletion {
			continue
		}
		if _, err := s.MarkOrgForDeletion(ctx, o.ID, opts.PersonalOrgGrace); err != nil {
			return res, fmt.Errorf("auth: erase user %s: mark its personal org %s for deletion: %w", userID, o.ID, err)
		}
		res.PersonalOrgsMarkedGone = append(res.PersonalOrgsMarkedGone, o.ID)
	}

	if _, err := s.revokeAccount(ctx, userID); err != nil {
		return res, fmt.Errorf("auth: erase user %s: %w", userID, err)
	}

	// The address the row carries, kept for the second sweep once the row
	// lost it: an anonymized row carries its marker, to which nothing was
	// ever addressed.
	address := ""
	if !identity.IsErasedEmail(u.Email) {
		address = u.Email
	}
	if err := s.sweepAccountGrants(ctx, userID, address, &res); err != nil {
		return res, err
	}

	if opts.BeforeAnonymize != nil {
		if err := opts.BeforeAnonymize(ctx, u); err != nil {
			return res, fmt.Errorf("auth: erase user %s: %w", userID, err)
		}
	}

	res.User, err = s.store.EraseUser(ctx, userID)
	if err != nil {
		return res, fmt.Errorf("auth: erase user %s: anonymize the row: %w", userID, err)
	}

	if err := s.sweepAccountGrants(ctx, userID, address, &res); err != nil {
		return res, fmt.Errorf("%w — the row is anonymized: repeat the erasure", err)
	}
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sessionOpTimeout)
	defer cancel()
	n, err := s.sessions.DeleteAccountSessions(dctx, userID)
	if err != nil {
		return res, fmt.Errorf("auth: erase user %s: the row is anonymized, its sessions are not deleted yet — repeat the erasure: %w", userID, err)
	}
	res.SessionsDeleted = n
	return res, nil
}

// lastOwnerRefusal is ErrLastOrgOwner naming o.
func lastOwnerRefusal(o identity.Org) error {
	return fmt.Errorf("%w: %s (%s) — transfer its ownership or delete it first", ErrLastOrgOwner, o.Slug, o.ID)
}

// ownershipKind is what an org membership weighs for the last-owner
// safeguard.
type ownershipKind int

const (
	// notGuarded: not the owner role, or an org that is gone or pending
	// deletion — its membership goes with the account.
	notGuarded ownershipKind = iota
	// personalOwnership: the account's personal org, marked for deletion
	// with it.
	personalOwnership
	// sharedOwnership: a shared org that stays — released only while
	// another owner remains.
	sharedOwnership
)

// classifyOwnership reads m's org and answers what m weighs for the
// last-owner safeguard, with the org it read.
func (s *Service) classifyOwnership(ctx context.Context, m identity.OrgMembership) (identity.Org, ownershipKind, error) {
	if m.Role != identity.OrgRoleOwner {
		return identity.Org{}, notGuarded, nil
	}
	o, err := s.store.GetOrg(ctx, m.OrgID)
	if errors.Is(err, identity.ErrNotFound) {
		// A membership whose org row is gone owns nothing.
		return identity.Org{}, notGuarded, nil
	}
	if err != nil {
		return identity.Org{}, notGuarded, fmt.Errorf("read org %s: %w", m.OrgID, err)
	}
	switch {
	case o.Personal:
		return o, personalOwnership, nil
	case o.Status == identity.TeamStatusPendingDeletion:
		return o, notGuarded, nil
	}
	return o, sharedOwnership, nil
}

// otherOrgOwners counts the owners of orgID other than userID.
func (s *Service) otherOrgOwners(ctx context.Context, orgID, userID string) (int, error) {
	members, err := s.store.ListOrgMembershipsByOrg(ctx, orgID)
	if err != nil {
		return 0, fmt.Errorf("list the members of org %s: %w", orgID, err)
	}
	n := 0
	for _, m := range members {
		if m.Role == identity.OrgRoleOwner && m.UserID != userID {
			n++
		}
	}
	return n, nil
}

// releaseOwnerships removes userID's owner membership of each shared org in
// owned, then recounts that org's owners: a concurrent erasure or demotion
// of the other owner, which read this one as still there, leaves none. The
// recount after the delete is what makes the refusal hold under that race
// — whichever of the two writes lands second sees the other — so on zero
// every membership released so far is put back and the erasure is refused.
// It answers the memberships it removed.
func (s *Service) releaseOwnerships(ctx context.Context, userID string, owned []identity.OrgMembership, res *EraseResult) ([]identity.OrgMembership, error) {
	var released []identity.OrgMembership
	for _, m := range owned {
		err := s.store.DeleteOrgMembership(ctx, userID, m.OrgID)
		if errors.Is(err, identity.ErrNotFound) {
			// Removed under this erasure by another writer: nothing of it
			// is this call's to release or to put back.
			continue
		}
		if err != nil {
			return nil, errors.Join(
				fmt.Errorf("auth: erase user %s: release its ownership of org %s: %w", userID, m.OrgID, err),
				s.restoreOwnerships(ctx, released, res))
		}
		released = append(released, m)
		res.OrgMembershipsRemoved++
		others, err := s.otherOrgOwners(ctx, m.OrgID, userID)
		if err != nil {
			return nil, errors.Join(
				fmt.Errorf("auth: erase user %s: recount the owners of org %s: %w", userID, m.OrgID, err),
				s.restoreOwnerships(ctx, released, res))
		}
		if others > 0 {
			continue
		}
		o, err := s.store.GetOrg(ctx, m.OrgID)
		if err != nil {
			o = identity.Org{ID: m.OrgID, Slug: m.OrgID}
		}
		return nil, errors.Join(lastOwnerRefusal(o), s.restoreOwnerships(ctx, released, res))
	}
	return released, nil
}

// ownershipCompensationTimeout bounds restoreOwnerships once it is detached
// from the request.
const ownershipCompensationTimeout = 5 * time.Second

// restoreOwnerships puts back the owner memberships releaseOwnerships
// removed, uncounting each; its error names every membership it could not
// restore, for the operator to re-grant.
func (s *Service) restoreOwnerships(ctx context.Context, released []identity.OrgMembership, res *EraseResult) error {
	// The compensation outlives the request: a client that goes away
	// between the release and the recount must not leave the account
	// without the ownerships it still holds. It is the one membership write
	// the grant guard lets through on an account already marked erased: the
	// sweep's recount runs after the mark.
	ctx, cancel := context.WithTimeout(withErasureCompensation(context.WithoutCancel(ctx)), ownershipCompensationTimeout)
	defer cancel()
	var errs []error
	for _, m := range released {
		if err := s.store.UpsertOrgMembership(ctx, m); err != nil {
			errs = append(errs, fmt.Errorf("auth: restore the ownership of org %s by user %s: %w", m.OrgID, m.UserID, err))
			continue
		}
		res.OrgMembershipsRemoved--
	}
	return errors.Join(errs...)
}

// sweepAccountGrants removes what grants the account access or names its
// address in the identity store: the invitations addressed to address (none
// when it is empty) and the ones the account created — accepting one reads
// nothing of its creator, so an erased account's pending invitation would
// still seat its invitee, which an org offboarding revokes too — every team
// and org membership, every SSO link. It runs twice in an erasure — before
// the hook and after the anonymization — and adds what each pass removed to
// res.
func (s *Service) sweepAccountGrants(ctx context.Context, userID, address string, res *EraseResult) error {
	if address != "" {
		n, err := s.store.DeleteInvitationsByEmail(ctx, address)
		if err != nil {
			return fmt.Errorf("auth: erase user %s: %w", userID, err)
		}
		res.InvitationsRemoved += n
	}
	n, err := s.store.DeleteInvitationsByInviter(ctx, userID)
	if err != nil {
		return fmt.Errorf("auth: erase user %s: %w", userID, err)
	}
	res.InvitationsRemoved += n
	mems, err := s.store.ListMembershipsByUser(ctx, userID)
	if err != nil {
		return fmt.Errorf("auth: erase user %s: list its memberships: %w", userID, err)
	}
	for _, m := range mems {
		if err := s.store.DeleteMembership(ctx, userID, m.TeamID); err != nil && !errors.Is(err, identity.ErrNotFound) {
			return fmt.Errorf("auth: erase user %s: delete its membership in team %s: %w", userID, m.TeamID, err)
		}
		res.MembershipsRemoved++
	}
	orgMems, err := s.store.ListOrgMembershipsByUser(ctx, userID)
	if err != nil {
		return fmt.Errorf("auth: erase user %s: list its org memberships: %w", userID, err)
	}
	for _, m := range orgMems {
		// An ownership granted after step 1 read the memberships — an org
		// created by a request that read the account before the mark — is
		// released like step 2's: a recount that finds no other owner puts
		// it back and refuses, never an organization without an owner.
		_, kind, err := s.classifyOwnership(ctx, m)
		if err != nil {
			return fmt.Errorf("auth: erase user %s: %w", userID, err)
		}
		if kind == sharedOwnership {
			if _, err := s.releaseOwnerships(ctx, userID, []identity.OrgMembership{m}, res); err != nil {
				return fmt.Errorf("auth: erase user %s: an ownership granted during the erasure: %w", userID, err)
			}
			continue
		}
		if err := s.store.DeleteOrgMembership(ctx, userID, m.OrgID); err != nil && !errors.Is(err, identity.ErrNotFound) {
			return fmt.Errorf("auth: erase user %s: delete its membership in org %s: %w", userID, m.OrgID, err)
		}
		res.OrgMembershipsRemoved++
	}
	links, err := s.store.ListOIDCLinksByUser(ctx, userID)
	if err != nil {
		return fmt.Errorf("auth: erase user %s: list its SSO links: %w", userID, err)
	}
	for _, l := range links {
		if err := s.store.DeleteOIDCLink(ctx, l.Provider, l.ProviderUserID); err != nil && !errors.Is(err, identity.ErrNotFound) {
			return fmt.Errorf("auth: erase user %s: delete its SSO link %s/%s: %w", userID, l.Provider, l.ProviderUserID, err)
		}
		res.SSOLinksRemoved++
	}
	return nil
}
