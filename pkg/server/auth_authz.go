package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/identity"
)

// ---- Authorization helpers ----

func (s *Server) canViewTeam(ctx context.Context, id auth.Identity, teamID string) bool {
	// Synthetic principals (webhook / config-share) never authorize an
	// operator team action, even if a future bug set a role on one.
	if id.IsSynthetic() {
		return false
	}
	if id.IsSuperAdmin {
		return true
	}
	// AtLeast(RoleViewer) is equivalent to the old Valid() for the four ladder
	// roles, but excludes the orthogonal config_editor capability (rank 0) — a
	// config editor is not a team viewer (ADR-078); it reaches only its own
	// endpoints via canEditConfigShares.
	if id.TeamID == teamID && id.Role.AtLeast(identity.RoleViewer) {
		return true
	}
	if mb, err := s.authStore().GetMembership(ctx, id.UserID, teamID); err == nil && mb.Role.AtLeast(identity.RoleViewer) {
		return true
	}
	// An org admin/owner can view every team in their org.
	return s.orgAdminOfTeam(ctx, id, teamID)
}

// canEditConfigShares reports whether the principal may read/write this team's
// config-shares through the AUTHENTICATED (session) editor endpoints: the
// orthogonal config_editor capability on this team, plus anyone who already
// manages it (admin/owner, org admin, super-admin). Deliberately NOT
// canViewTeam — a plain viewer/member was never delegated config editing. The
// public iws_ token path has its own middleware; synthetic principals are
// rejected here. See ADR-078.
func (s *Server) canEditConfigShares(ctx context.Context, id auth.Identity, teamID string) bool {
	if id.IsSynthetic() {
		return false
	}
	if id.IsSuperAdmin {
		return true
	}
	if id.TeamID == teamID && id.Role == identity.RoleConfigEditor {
		return true
	}
	if mb, err := s.authStore().GetMembership(ctx, id.UserID, teamID); err == nil && mb.Role == identity.RoleConfigEditor {
		return true
	}
	return s.canManageTeam(ctx, id, teamID)
}

// canEditBots reports whether the principal may create/edit/delete this team's
// authored bot bundles. Editing a team bot is authoring team automation that
// runs in every member's context, so it takes the same orthogonal
// config_editor capability (ADR-078) as config-share editing, plus anyone who
// already manages the team (admin/owner, org admin, super-admin). Deliberately
// NOT canViewTeam — a plain viewer/member was never delegated bot authoring.
func (s *Server) canEditBots(ctx context.Context, id auth.Identity, teamID string) bool {
	return s.canEditConfigShares(ctx, id, teamID)
}

func (s *Server) canManageTeam(ctx context.Context, id auth.Identity, teamID string) bool {
	if id.IsSynthetic() {
		return false
	}
	if id.IsSuperAdmin {
		return true
	}
	if mb, err := s.authStore().GetMembership(ctx, id.UserID, teamID); err == nil && mb.Role.AtLeast(identity.RoleAdmin) {
		return true
	}
	// An org admin/owner can manage every team in their org.
	return s.orgAdminOfTeam(ctx, id, teamID)
}

// orgAdminOfTeam reports whether the principal is an admin/owner of the
// team's parent org (so org admins implicitly manage every team in it).
func (s *Server) orgAdminOfTeam(ctx context.Context, id auth.Identity, teamID string) bool {
	t, err := s.authStore().GetTeam(ctx, teamID)
	if err != nil || t.OrgID == "" {
		return false
	}
	return s.canManageOrg(ctx, id, t.OrgID)
}

// canViewOrg reports whether the principal may read an org's settings /
// roster / usage. Super-admins and any org member pass.
func (s *Server) canViewOrg(ctx context.Context, id auth.Identity, orgID string) bool {
	if id.IsSynthetic() {
		return false
	}
	if id.IsSuperAdmin {
		return true
	}
	if id.OrgID == orgID && id.OrgRole.Valid() {
		return true
	}
	om, err := s.authStore().GetOrgMembership(ctx, id.UserID, orgID)
	if err != nil {
		return false
	}
	return om.Role.Valid()
}

// canManageOrg reports whether the principal may mutate an org (members,
// SSO, settings, teams). Super-admins and org admins/owners pass.
func (s *Server) canManageOrg(ctx context.Context, id auth.Identity, orgID string) bool {
	if id.IsSynthetic() {
		return false
	}
	if id.IsSuperAdmin {
		return true
	}
	om, err := s.authStore().GetOrgMembership(ctx, id.UserID, orgID)
	if err != nil {
		return false
	}
	return om.Role.AtLeast(identity.OrgRoleAdmin)
}

// requireOrgStandingToInvite refuses to mint a team invitation whose role
// would be mirrored up to an org role its creator does not hold. Accepting an
// invitation writes the team grant AND, for a role on the org ladder, an org
// membership at identity.OrgRoleForTeamRole (pkg/auth
// grantOrgMembershipForTeam), so minting one hands out ORG standing: capped by
// the team-management check alone, a team admin who is a plain org member
// mints an admin/owner invitation for their own address and comes back an org
// admin, and the off-boarded holder of an orphan admin grant re-enters the org
// on that grant alone. The cap is the minter's own standing, read from the
// store rather than from their token — a claim can predate a removal — and
// read again here on the org route, whose gate read it before the body named
// the team.
//
// A role mirrored up to NOTHING (config_editor, rank 0 — ADR-078 decision 4,
// the same predicate as grantNeedsOrgBoundary) carries no org standing to
// exceed and is never refused here; neither is an invitation into a team with
// no parent org, which has no boundary to read, as in
// requireOrgMembershipForGrant. Platform admins pass: IsSuperAdmin is the
// escape hatch, and they hold no org membership for a cap to read.
//
// This read is taken BEFORE the invitation row lands, so it is half of a
// pair: confirmOrgStandingAfterMint re-reads the standing once the row is
// there, since a write that lowers it sweeps only the rows its own listing
// reaches. Both minting routes owe both halves.
//
// It answers the request and returns false when the invitation must not be
// minted. orgID is the org of the team the invitation joins, which both
// minting routes resolve before they call: POST /api/teams/{id}/invitations
// from the team it names, POST /api/orgs/{id}/invitations from the team its
// body names, once that team is confirmed to be in the org of the path.
func (s *Server) requireOrgStandingToInvite(w http.ResponseWriter, r *http.Request, id auth.Identity, orgID string, role identity.Role) bool {
	want := identity.OrgRoleForTeamRole(role)
	if orgID == "" || want == "" || id.IsSuperAdmin {
		return true
	}
	have, answered := s.ownOrgStanding(w, r, id, orgID)
	switch {
	case answered:
		return false
	case have.AtLeast(want):
		return true
	}
	httpError(w, http.StatusForbidden,
		"a team invitation cannot carry an org role above its creator's: an invitation at role %s is mirrored up to an org %s membership when it is accepted, and your own org role in this team's organization is %s",
		role, want, orgStandingName(have))
	return false
}

// confirmOrgStandingAfterMint re-reads the minter's own org standing once the
// invitation row has LANDED, and revokes it unless that standing still covers
// the role it carries. requireOrgStandingToInvite is a check taken BEFORE the
// act, and the two share no transaction: a write that lowers the minter's own
// standing — the roster's PATCH, the placement, the off-boarding cascade —
// sweeps the invitations they minted above what they are left with, but only
// the rows its listing can see, so a mint whose read preceded that write and
// whose INSERT follows the sweep outlives it and still hands out the rank its
// creator has lost. This is the writer side of that pair, the way
// confirmOrgMembershipForGrant is for a team grant.
//
// It answers the same predicate those writes answer
// (standingTakesInvitation), on the rows as they stand once the invitation is
// there: the MINTED half takes what the standing re-read no longer covers,
// and the ADDRESSED-TO half takes, whatever its role, an invitation addressed
// to the minter's own address once their membership row is GONE — the
// re-entry ticket the member a removal is taking out would otherwise mint for
// themselves.
//
// One predicate is not one selection, and the two sides do NOT leave the same
// rows on every state: this one asks the addressed-to half only where the
// minter holds no grant in the team either, a precondition the removal's
// cascade does not carry because it REVOKES that grant in the same pass. So
// for the one state where that differs — a self-addressed row from a minter
// who still holds the grant, the orphan grant the console reports — the mint
// keeps the row and a removal takes it, by design and with no race. What
// closes the race is the removal's own order: its last write is the
// invitation sweep, so a row this confirmation kept on a grant that pass was
// about to revoke does not outlive it (handleRemoveOrgMember).
//
// It exempts what the cap exempts and no more: a team with no parent org and
// a platform admin. A role mirrored up to NOTHING (config_editor, ADR-078)
// carries no standing for the minted half to measure, so a standing that
// drops hands such an invitation OVER rather than withdrawing it — but the
// addressed-to half still reaches it, which is why every mint into an org
// owes the standing read. The two further reads are the addressed-to half's
// own, taken only where that row is gone: the minter's grant in the team the
// invitation joins, which separates the member a removal took out from the
// holder of an orphan grant minting for themselves, and then the address
// their account answers at.
//
// The re-read and the revoke run on contexts DETACHED from the request, each
// on its own budget: a client that goes away is the most ordinary reason a
// standing cannot be re-confirmed, and a revoke that died with it would leave
// exactly the invitation this guard exists to remove. A state that cannot be
// READ — the standing, the grant or the address — takes the invitation back
// too and answers 500 naming THAT read, under its own audit cause
// (mintConfirmationRead): a cap that cannot be confirmed is not one, and a
// body that blamed the standing for a grant read would send an operator to a
// row that was read fine. A revoke that fails anyway is logged and named in
// the refusal, which then asks for the invitation to be revoked by hand.
//
// It answers the request and returns false when the invitation did not
// survive.
func (s *Server) confirmOrgStandingAfterMint(w http.ResponseWriter, r *http.Request, id auth.Identity, orgID string, inv identity.Invitation) bool {
	if orgID == "" || id.IsSuperAdmin {
		return true
	}
	rctx, rcancel := context.WithTimeout(context.WithoutCancel(r.Context()), grantCompensationTimeout)
	defer rcancel()
	taken, have, read, err := s.standingAfterMintTakesInvitation(rctx, id.UserID, orgID, inv)
	if err == nil && !taken {
		return true
	}
	// The audit line of a revoke carries the exit that asked for it: a
	// standing READ as below the role minted is a standing that dropped, one
	// that could not be read at all is not — and which of the three reads
	// could not be made is its own cause (mintConfirmationRead), since the
	// trail is where an operator looks for whose standing dropped and what
	// it took, while the honest account of an unreadable one (the 500 body,
	// the log) goes to a client that may be gone.
	reason := "creator_standing_dropped"
	if err != nil {
		reason = read.reason
	}
	fate := s.revokeUnconfirmedInvitation(r, inv, reason)
	if err != nil {
		if s.logger != nil {
			s.logger.Error("team %s: confirming %s of %s in org %s after invitation %s was minted failed: %v — %s",
				inv.TeamID, read.subject, id.UserID, orgID, inv.ID, err, fate)
		}
		httpError(w, http.StatusInternalServerError,
			"confirming %s after the invitation was minted failed: %v — a cap that cannot be confirmed is not one, so %s",
			read.what, err, fate)
		return false
	}
	// Which half took the row, in its own words. A role that HAS an org
	// mirror can only have been taken by the minted half: the addressed-to
	// half fires at a standing that is gone, which covers no mirror at all,
	// so a standing still covering the role leaves both halves false
	// (standingTakesInvitation).
	if want := identity.OrgRoleForTeamRole(inv.Role); want != "" {
		httpError(w, http.StatusForbidden,
			"a team invitation cannot carry an org role above its creator's: an invitation at role %s is mirrored up to an org %s membership when it is accepted, and your own org role in this team's organization is %s by the time it was minted — %s",
			inv.Role, want, orgStandingName(have), fate)
		return false
	}
	httpError(w, http.StatusForbidden,
		"a team invitation addressed to its creator's own address does not outlive their membership of the team's organization: your own org role in this team's organization is %s by the time it was minted, and accepting the invitation would seat you back in a team of it — %s",
		orgStandingName(have), fate)
	return false
}

// standingAfterMintTakesInvitation answers, from the rows as they stand once
// a mint's own row has landed, whether the write that mint may have
// straddled takes it (standingTakesInvitation) — asking each half in the
// order of what it has to read:
//
//   - the MINTED half measures a RANK and needs only the standing, read from
//     the membership ROW, so it is asked first and decides every invitation
//     carrying an org mirror;
//   - the ADDRESSED-TO half is asked only where that standing is GONE (the
//     one a removal leaves) and the minted half left the row — and it owes
//     the precondition the cascade has by construction: that the minter can
//     no longer REACH the team the invitation joins. The cascade revokes
//     their grants in the same pass, before the membership row this reads is
//     gone; here a minter who still HOLDS the grant in that team gains
//     nothing AT THIS MOMENT by accepting their own invitation — it is the
//     access canManageTeam admitted the mint on, the orphan grant the console
//     reports (orphan_grant, off_ladder), and withdrawing it would refuse a
//     state the platform sanctions. A minter whose grant is gone too is the
//     member a removal took out, for whom the row is a re-entry ticket.
//
// What makes "at this moment" enough is that the premise travels with the
// grant: whoever withdraws it takes the rows kept on it. A removal's order
// does so for the grants it revokes — its last write is the invitation
// sweep, so a row kept on a grant the cascade was about to revoke does not
// outlive it (handleRemoveOrgMember) — and THREE writes revoke a team grant
// and leave the team standing with no removal in flight, each sweeping that
// row itself through the one selection they share
// (takeInvitationsKeptOnRevokedTeamGrant): DELETE
// /api/teams/{id}/members/{user_id}, the grant compensation of the
// team-member PUT/PATCH — on its exit where the org membership row is GONE;
// the one where that row could not be READ takes no invitation and names the
// residue — and the GitHub SSO reconciliation of a login the allow-list no
// longer grants the team (pkg/auth reconcileGitHubGrants, through the sweep
// the server registers on the service). A team or org deletion revokes such
// a grant too and owes no sweep: the org cascade takes the team's
// invitations with it, and a deleted team leaves rows no accept can resolve
// (handleDeleteTeam, auth DeleteOrgCascade). The selection takes nothing
// where it has no premise to withdraw: a holder whose org membership row
// answers the read, a team with no parent org or that no longer exists, an
// account with no known address. So a grant written BACK after a removal's
// last sweep, which re-opens the window, carries the row with it when it
// goes.
//
// It answers the standing it read, which the refusal names, and the error of
// a state it could not read at all — with the READ that failed, since the
// three fail for different reasons and name different rows: a cap that cannot
// be confirmed is not one, so its caller takes the invitation back on it
// rather than leaving a row it cannot measure, and the body and the audit
// cause it does that under have to name the read it could not make. read is
// meaningful on the error path only.
func (s *Server) standingAfterMintTakesInvitation(ctx context.Context, userID, orgID string, inv identity.Invitation) (taken bool, have identity.OrgRole, read mintConfirmationRead, err error) {
	have, hasRow, err := s.readOwnOrgStanding(ctx, userID, orgID)
	if err != nil {
		return false, "", mintStandingRead, err
	}
	if standingTakesInvitation(userID, have, "", false, inv) {
		return true, have, mintConfirmationRead{}, nil
	}
	// The row is THERE: this is a demotion's state, not a removal's, and the
	// addressed-to half fires at the row's absence rather than at a rank
	// (readOwnOrgStanding) — which is also what spares the two reads below.
	if hasRow {
		return false, have, mintConfirmationRead{}, nil
	}
	gone, err := s.ownTeamGrantGone(ctx, userID, inv.TeamID)
	if err != nil {
		return false, have, mintGrantRead, err
	}
	if !gone {
		return false, have, mintConfirmationRead{}, nil
	}
	addr, addrKnown, err := s.readOwnInvitationAddress(ctx, userID)
	if err != nil {
		return false, have, mintAddressRead, err
	}
	// The standing is GONE on every path reaching here — the read above
	// found no row (hasRow returned) — which is the sentinel the sweeps pass
	// and the gate the addressed-to half fires at.
	return standingTakesInvitation(userID, orgStandingAfterRemoval, addr, addrKnown, inv), have, mintConfirmationRead{}, nil
}

// mintConfirmationRead names one of the three state reads the confirmation
// after a mint makes, for the 500 that reports a read it could not make and
// for the audit cause the revoke records. A body that blamed the STANDING for
// the grant read sends an operator to a row that was read fine, and one audit
// cause for three reads cannot be queried for either.
type mintConfirmationRead struct {
	// subject names the read in the LOG line, which says who it was about:
	// "confirming <subject> of <user> in org <org> after invitation …".
	subject string
	// what names it in the 500 body, addressed to the caller: "confirming
	// <what> after the invitation was minted failed".
	what string
	// reason is the cause the invitation.deleted audit line carries.
	reason string
}

var (
	mintStandingRead = mintConfirmationRead{
		subject: "the org standing",
		what:    "your own org standing in this team's organization",
		reason:  "creator_standing_unconfirmed",
	}
	mintGrantRead = mintConfirmationRead{
		subject: "the team grant",
		what:    "whether you still hold a grant in the team this invitation joins",
		reason:  "creator_team_grant_unconfirmed",
	}
	mintAddressRead = mintConfirmationRead{
		subject: "the invitation address",
		what:    "the address your own pending invitations are addressed to",
		reason:  "creator_address_unconfirmed",
	}
)

// revokeUnconfirmedInvitation deletes an invitation whose cap the mint could
// not confirm and answers the clause a refusal names its fate with. reason is
// the cause its audit line records — the caller knows which of its two exits
// asked for the revoke, and this one cannot tell them apart. A row another
// caller revoked in between — the sweep of the very write that lowered the
// standing — is already in the state this wants, and both stores say so
// definitively, so its ErrNotFound carries no audit line. A delete that fails
// leaves the row standing at a rank its creator no longer holds, which the
// refusal has to say.
//
// It runs on a context detached from the request and on its own budget: the
// re-read may have spent the one it would otherwise share, and a repair that
// still has to happen is exactly the call a spent budget must not kill.
func (s *Server) revokeUnconfirmedInvitation(r *http.Request, inv identity.Invitation, reason string) string {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), grantCompensationTimeout)
	defer cancel()
	err := s.authStore().DeleteInvitation(ctx, inv.ID)
	switch {
	case err == nil:
		s.auditTenant(r, inv.TeamID, "invitation.deleted", "invitation", inv.ID,
			map[string]any{"email": inv.Email, "reason": reason})
		return "the invitation this request minted was revoked"
	case errors.Is(err, identity.ErrNotFound):
		return "the invitation this request minted is already revoked"
	default:
		if s.logger != nil {
			s.logger.Error("team %s: revoking invitation %s, whose creator's org standing could not be confirmed, failed: %v — it stands at a rank its creator does not hold",
				inv.TeamID, inv.ID, err)
		}
		return fmt.Sprintf("revoking the invitation this request minted failed too (%v), so invitation %s stands at a rank its creator does not hold: revoke it by hand", err, inv.ID)
	}
}

// requireOrgStandingToWriteOrgRole refuses to write an org membership at a
// role above the writer's own standing in that org. The roster's write is
// gated on canManageOrg, which admits AtLeast(OrgRoleAdmin) and bounds no
// rank: this cap is what bounds it, and it is the same shape as the
// invitation one — the writer's standing, read from the membership ROW rather
// than from their token, which a removal since does not rewrite.
//
// Platform admins pass (IsSuperAdmin is the escape hatch, and they hold no
// org membership for a cap to read), and a standing that cannot be READ
// refuses the write with the store's error instead of writing uncapped. The
// rule is on the rank WRITTEN, so lowering a role is never refused BY THE
// RANK, and an owner still appoints an owner — a writer whose own row went
// away in between holds no standing at all and is refused whatever they
// write, lowering included
// (TestOrgMembers_aStandingThatVanishesAfterTheGateRefusesTheRoleWrite).
//
// The refusal names who can write the rank it refused, since the cap leaves
// an org whose roster holds no such rank — what the teams→orgs backfill
// creates, where identity.OrgRoleForTeamRole tops out at org admin — with no
// member able to appoint one.
//
// It answers the request and returns false when the role must not be written.
func (s *Server) requireOrgStandingToWriteOrgRole(w http.ResponseWriter, r *http.Request, id auth.Identity, orgID string, role identity.OrgRole) bool {
	if id.IsSuperAdmin {
		return true
	}
	have, answered := s.ownOrgStanding(w, r, id, orgID)
	switch {
	case answered:
		return false
	case have.AtLeast(role):
		return true
	}
	httpError(w, http.StatusForbidden,
		"a member's org role cannot be raised above the writer's own: writing the org role %s takes that standing, and your own org role in this org is %s — a member of this org who already holds %s writes it, or a platform admin",
		role, orgStandingName(have), role)
	return false
}

// ownOrgStanding reads the caller's own org role in orgID for a guard that
// caps what they may hand out there, on the REQUEST's context. A standing
// that cannot be READ answers the request 500 and reports answered: a cap
// that cannot be applied refuses the act rather than letting it through
// uncapped.
func (s *Server) ownOrgStanding(w http.ResponseWriter, r *http.Request, id auth.Identity, orgID string) (identity.OrgRole, bool) {
	have, _, err := s.readOwnOrgStanding(r.Context(), id.UserID, orgID)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "resolve your org membership: %v", err)
		return "", true
	}
	return have, false
}

// readOwnOrgStanding reads one account's org role in orgID — the membership
// ROW, never a token's claim — and answers whether that row is THERE at all.
// An account with no row stands at no org role, which AtLeast refuses for
// every rung, so ErrNotFound is a standing (orgStandingAfterRemoval) and not
// an error; any other failure is returned, for a caller whose policy on an
// unreadable standing is its own.
//
// The two facts are answered apart because a role this build does not rank —
// a rung added to identity.OrgRole before OrgRole.rank — is "no standing"
// held by a member who still HAS a row, and the guard that fires at a
// standing that is GONE needs the row's absence, not a rank of zero
// (standingTakesInvitation).
func (s *Server) readOwnOrgStanding(ctx context.Context, userID, orgID string) (identity.OrgRole, bool, error) {
	om, err := s.authStore().GetOrgMembership(ctx, userID, orgID)
	switch {
	case errors.Is(err, identity.ErrNotFound):
		return orgStandingAfterRemoval, false, nil
	case err != nil:
		return "", false, err
	}
	return om.Role, true, nil
}

// readOwnInvitationAddress reads the address one account's pending
// invitations would be addressed to: its own row, read exactly as both
// accept paths read it (identity.Invitation.AddressedTo), or a selection
// made with it leaves behind rows they would honour. An account that
// definitively has NO row (ErrNotFound) has no address an invitation could be
// addressed to, which is an answer rather than an error — known is false and
// the half selecting by the address selects nothing. Any other failure is
// returned: a row that MAY exist and could not be read is a half that cannot
// be applied, and what to do about that is each caller's own policy.
func (s *Server) readOwnInvitationAddress(ctx context.Context, userID string) (addr string, known bool, err error) {
	u, err := s.authStore().GetUser(ctx, userID)
	switch {
	case err == nil:
		return u.Email, true, nil
	case errors.Is(err, identity.ErrNotFound):
		return "", false, nil
	}
	return "", false, fmt.Errorf("resolve the address invitations of %s are addressed to: %w", userID, err)
}

// ownTeamGrantGone reports whether userID holds NO grant in teamID — their
// own access to the team an invitation joins. A definitive ErrNotFound is the
// answer this asks for; any other failure is returned, since a grant that MAY
// be there cannot tell an off-boarded member from the holder of an orphan
// grant, which is the whole question its caller puts.
func (s *Server) ownTeamGrantGone(ctx context.Context, userID, teamID string) (bool, error) {
	_, err := s.authStore().GetMembership(ctx, userID, teamID)
	switch {
	case err == nil:
		return false, nil
	case errors.Is(err, identity.ErrNotFound):
		return true, nil
	}
	return false, fmt.Errorf("resolve the team %s grant of %s: %w", teamID, userID, err)
}

// orgStandingName names a standing in a refusal, including the one an account
// with no org membership row holds.
func orgStandingName(role identity.OrgRole) string {
	if role == "" {
		return "none"
	}
	return string(role)
}

// clientIP picks the audit IP for an inbound request. The default
// is r.RemoteAddr — the only field a client can't forge. We only
// consult X-Forwarded-For / X-Real-IP when the immediate peer is in
// s.cfg.TrustedProxyCIDRs, which the operator has explicitly
// configured. Without this guard a client could spoof its audit IP
// (and undermine any future IP-based throttling) just by sending an
// X-Forwarded-For header.
func (s *Server) clientIP(r *http.Request) string {
	if !s.peerIsTrusted(r) {
		return r.RemoteAddr
	}
	if h := r.Header.Get("X-Forwarded-For"); h != "" {
		if i := strings.Index(h, ","); i > 0 {
			return strings.TrimSpace(h[:i])
		}
		return strings.TrimSpace(h)
	}
	if h := r.Header.Get("X-Real-IP"); h != "" {
		return h
	}
	return r.RemoteAddr
}

func (s *Server) peerIsTrusted(r *http.Request) bool {
	if s == nil || len(s.cfg.TrustedProxyCIDRs) == 0 {
		return false
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	for _, cidr := range s.cfg.TrustedProxyCIDRs {
		_, network, perr := net.ParseCIDR(cidr)
		if perr != nil {
			continue
		}
		if network.Contains(ip) {
			return true
		}
	}
	return false
}
