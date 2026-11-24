package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"slices"
	"strings"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/identity"
)

// ---- Authorization helpers ----

// canViewTeam is canViewTeamOrFail's fail-closed form: a read it could not
// complete grants nothing, and the caller is told nothing about it. It is
// what a GATE wants — no privilege from a read nobody made, nothing written
// — and it is what every caller but one takes.
func (s *Server) canViewTeam(ctx context.Context, id auth.Identity, teamID string) bool {
	allowed, _ := s.canViewTeamOrFail(ctx, id, teamID)
	return allowed
}

// canViewTeamOrFail resolves whether id may read teamID, and reports the
// read that stopped a "no". Only a REFUSAL carries the error: a read that
// failed beside a row which GRANTS access changes no verdict, so a "yes"
// never carries one. The route whose published contract says a store failure
// answers the generic 500 and is logged (GET /api/teams/{id}/members,
// docs/cloud-rest-api.md) takes this form; the fail-closed canViewTeam above
// is what every other caller takes, and a "not a member" that rests on a
// read the store could not complete is the state this exists to keep out of
// that one route's answers.
func (s *Server) canViewTeamOrFail(ctx context.Context, id auth.Identity, teamID string) (bool, error) {
	// The token arm below reads no store, so it answers the credential
	// first, with the refusals teamRoleOrFail opens with: a synthetic
	// principal (webhook / config-share) never authorizes an operator team
	// action, even if a future bug set a role on one, and a pin bounds the
	// credential whatever role it claims.
	if id.IsSynthetic() || id.PinRefusesTeam(teamID) {
		return false, nil
	}
	// AtLeast(RoleViewer) is equivalent to the old Valid() for the four ladder
	// roles, but excludes the orthogonal config_editor capability (rank 0) — a
	// config editor is not a team viewer (ADR-078); it reaches only its own
	// endpoints via canEditConfigShares.
	if id.TeamID == teamID && id.Role.AtLeast(identity.RoleViewer) {
		return true, nil
	}
	return s.teamRoleOrFail(ctx, id, teamID, identity.RoleViewer)
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
	if id.PinRefusesTeam(teamID) {
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

// canManageTeam is canManageTeamOrFail's fail-closed form: a read it could
// not complete grants nothing, and the caller is told nothing about it — what
// a gate wants, and what every caller takes but the two that must tell an
// outage from a refusal: the pin heal and the trigger routes' credential gate.
func (s *Server) canManageTeam(ctx context.Context, id auth.Identity, teamID string) bool {
	allowed, _ := s.canManageTeamOrFail(ctx, id, teamID)
	return allowed
}

// canManageTeamOrFail resolves whether id may administer teamID (admin/owner
// of the team, admin/owner of its org, super-admin) and reports the read that
// stopped a "no", the way canViewTeamOrFail does: only a refusal carries the
// error, a "yes" never does. A caller that acts on the refusal itself takes
// this form: the delete retry's pin heal, which would otherwise answer 404
// over pins still standing because a Mongo blip read as "no right", and the
// trigger routes' credential gate (authorizeTriggerCredentials), which would
// otherwise tell an admin they are not one. It has no token arm: the right
// is read from the grant row, never from the role a token claims.
func (s *Server) canManageTeamOrFail(ctx context.Context, id auth.Identity, teamID string) (bool, error) {
	return s.teamRoleOrFail(ctx, id, teamID, identity.RoleAdmin)
}

// teamRoleOrFail is the one body of the team ladder's (bool, error) gates:
// whether id holds at least rung on teamID — as a super-admin, through a
// grant row of that rank, or as an admin/owner of the team's org, who
// reaches every team of it on every rung — and, on a "no", the read that
// stopped it. Only a refusal carries an error, since a failed read beside an
// arm that grants changes no verdict: the grant read's failure first, else
// the org lineage's (orgAdminOfTeamOrFail). A grant the store says is NOT
// THERE is a fact, not a failure, so beside it a failed org read is the
// whole reason the gate could not say yes — a refusal resting on it is that
// failure, never a "no". A gate on another rung delegates here rather than
// copying the merge.
//
// A caller can reach it directly, so it answers the credential itself (the
// pin census of pat_pin_gate_test.go holds it to that), whatever its callers
// already asked.
func (s *Server) teamRoleOrFail(ctx context.Context, id auth.Identity, teamID string, rung identity.Role) (bool, error) {
	if id.IsSynthetic() || id.PinRefusesTeam(teamID) {
		return false, nil
	}
	if id.IsSuperAdmin {
		return true, nil
	}
	var unread error
	role, _, err := s.readTeamGrantRole(ctx, id.UserID, teamID)
	switch {
	case err != nil:
		unread = fmt.Errorf("read the team grant of %s in %s: %w", id.UserID, teamID, err)
	case role.AtLeast(rung):
		return true, nil
	}
	allowed, oerr := s.orgAdminOfTeamOrFail(ctx, id, teamID)
	if allowed {
		return true, nil
	}
	if unread == nil {
		unread = oerr
	}
	return false, unread
}

// orgAdminOfTeam reports whether the principal is an admin/owner of the
// team's parent org (so org admins implicitly manage every team in it).
// Fail-closed form of orgAdminOfTeamOrFail.
func (s *Server) orgAdminOfTeam(ctx context.Context, id auth.Identity, teamID string) bool {
	allowed, _ := s.orgAdminOfTeamOrFail(ctx, id, teamID)
	return allowed
}

// orgAdminOfTeamOrFail is orgAdminOfTeam reporting the read that stopped a
// "no": a team the store says is NOT THERE, or one with no parent org, is a
// fact and no failure; a team row it could not read is.
func (s *Server) orgAdminOfTeamOrFail(ctx context.Context, id auth.Identity, teamID string) (bool, error) {
	// A team-addressed gate in its own right (team_lifecycle_routes.go takes
	// it directly), so it owes the pin its own refusal and not the one its
	// callers happen to have made.
	if id.PinRefusesTeam(teamID) {
		return false, nil
	}
	t, err := s.authStore().GetTeam(ctx, teamID)
	if err != nil {
		if errors.Is(err, identity.ErrNotFound) {
			return false, nil
		}
		return false, fmt.Errorf("read team %s: %w", teamID, err)
	}
	if t.OrgID == "" {
		return false, nil
	}
	return s.canManageOrgOrFail(ctx, id, t.OrgID)
}

// canViewOrg reports whether the principal may read an org's settings /
// roster / usage. Super-admins and any org member pass.
func (s *Server) canViewOrg(ctx context.Context, id auth.Identity, orgID string) bool {
	if id.IsSynthetic() {
		return false
	}
	if id.PinRefusesOrg(orgID) {
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
// Fail-closed form of canManageOrgOrFail.
func (s *Server) canManageOrg(ctx context.Context, id auth.Identity, orgID string) bool {
	allowed, _ := s.canManageOrgOrFail(ctx, id, orgID)
	return allowed
}

// canManageOrgOrFail is canManageOrg reporting the read that stopped a "no":
// a standing the store says is NOT THERE, or one below admin, is a fact and
// no failure; a membership row it could not read is.
func (s *Server) canManageOrgOrFail(ctx context.Context, id auth.Identity, orgID string) (bool, error) {
	if id.IsSynthetic() {
		return false, nil
	}
	if id.PinRefusesOrg(orgID) {
		return false, nil
	}
	if id.IsSuperAdmin {
		return true, nil
	}
	om, err := s.authStore().GetOrgMembership(ctx, id.UserID, orgID)
	if err != nil {
		if errors.Is(err, identity.ErrNotFound) {
			return false, nil
		}
		return false, fmt.Errorf("read the org standing of %s in %s: %w", id.UserID, orgID, err)
	}
	return om.Role.AtLeast(identity.OrgRoleAdmin), nil
}

// requireRankToInvite applies the two caps a mint owes on the role it hands
// out, on ONE read of the minter's own org standing, which both measure.
// Accepting an invitation writes the team GRANT it names and, for a role on
// the org ladder, an org membership at identity.OrgRoleForTeamRole (pkg/auth
// grantOrgMembershipForTeam), so minting one hands out both ranks: capped by
// the team-management check alone, a team admin who is a plain org member
// mints an admin/owner invitation for their own address and comes back an org
// admin, the off-boarded holder of an orphan admin grant re-enters the org on
// that grant alone, and on a team with NO parent org — where no org rank is
// mirrored at all — a team admin mints `owner` for their own address and
// reaches every grant of the team on the next request.
//
//   - the ORG cap measures the role's org mirror against the minter's own
//     standing in the team's org, and is the whole rule for a minter who
//     holds that org through an admin/owner standing: they manage every team
//     in it (canManageTeam, orgAdminOfTeam), so no grant of theirs bounds
//     what they hand out inside it.
//   - the TEAM cap measures the minted TEAM role against the minter's own
//     grant, where their authority over the team IS that grant: a team with
//     no parent org, or an org standing below admin. It is the mint's twin of
//     the cap requireRankOverMember puts on the role a roster write puts on a
//     row, and the two together are what makes a team rank a boundary rather
//     than a delay.
//
// Both caps read the minter's own rows rather than their token — a claim can
// predate a removal — and the standing is read once here, on the org route
// too, whose gate read it before the body named the team.
//
// A role mirrored up to NOTHING (config_editor, rank 0 — ADR-078 decision 4,
// the same predicate as grantNeedsOrgBoundary) carries no org standing to
// exceed and sits on no team ladder, so neither cap measures it. Platform
// admins pass: IsSuperAdmin is the escape hatch, and they hold no membership
// row for a cap to read.
//
// Each cap is read BEFORE the invitation row lands, so each is half of a
// pair: confirmOrgStandingAfterMint re-reads the standing once the row is
// there, and confirmTeamGrantAfterMint the grant, since a write that lowers
// either sweeps only the rows its own listing reaches. Both minting routes
// owe every half, which is why this guard is the one call they make.
//
// It answers the request and returns false when the invitation must not be
// minted. grantCapped reports that the team cap measured the minter's grant,
// which is the premise confirmTeamGrantAfterMint re-reads. orgID is the org
// of the team the invitation joins, which both minting routes resolve before
// they call: POST /api/teams/{id}/invitations from the team it names, POST
// /api/orgs/{id}/invitations from the team its body names, once that team is
// confirmed to be in the org of the path.
func (s *Server) requireRankToInvite(w http.ResponseWriter, r *http.Request, id auth.Identity, orgID, teamID string, role identity.Role) (ok bool, grantCapped bool) {
	want := identity.OrgRoleForTeamRole(role)
	if want == "" || id.IsSuperAdmin {
		return true, false
	}
	if orgID != "" {
		have, answered := s.ownOrgStanding(w, r, id, orgID)
		if answered {
			return false, false
		}
		if !have.AtLeast(want) {
			httpError(w, http.StatusForbidden,
				"a team invitation cannot carry an org role above its creator's: an invitation at role %s is mirrored up to an org %s membership when it is accepted, and your own org role in this team's organization is %s",
				role, want, orgStandingName(have))
			return false, false
		}
		if have.AtLeast(identity.OrgRoleAdmin) {
			return true, false
		}
	}
	mine, _, err := s.readTeamGrantRole(r.Context(), id.UserID, teamID)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "resolve your own grant in team %s: %v", teamID, err)
		return false, false
	}
	if !mine.AtLeast(role) {
		httpError(w, http.StatusForbidden,
			"a team invitation cannot carry a team role above its creator's grant: accepting an invitation at role %s writes that grant in this team, and your own is %s — %s",
			role, teamGrantName(mine), teamRankRemedy(role, orgID != ""))
		return false, false
	}
	return true, true
}

// confirmTeamGrantAfterMint re-reads the minter's own GRANT in the team the
// invitation joins once the row has LANDED, and revokes the invitation unless
// that grant still covers the team role it carries. The cap is a check taken
// BEFORE the act, and the two share no transaction: a write that lowers that
// grant — the team-member PATCH, its DELETE, the org off-boarding cascade,
// the SSO reconciliation — reaches only the rows its own listing sees, so a
// mint whose read preceded it and whose INSERT follows it outlives it and
// still hands out a rank its creator has lost. This is the team twin of
// confirmOrgStandingAfterMint.
//
// capped is the cap's own report that it measured the grant
// (requireRankToInvite), rather than a second read of the standing asking
// whether it would have: where the mint rested on an org standing of admin
// or above, no grant was measured and none is re-read here. This
// confirmation covers the mint that straddled a lowering WHERE the mint
// measured the grant; the lowerings themselves are covered by their own
// writes, which re-measure the cap that would bind the creator NOW against
// the rows they left (teamRankTakesInvitation): the sweep of a standing that
// falls (revokeInvitationsAboveOrgStanding) reads the grant too where the
// new standing is below admin, and the writes that lower a GRANT — the team
// PATCH and PUT, the DELETE, the grant compensation, the GitHub SSO
// reconciliation, the logged-in accept that writes a lower team role over an
// existing grant — run the team sweep (reconcileTeamRankInvitations, and the
// seam the service's own writes call: SetTeamRankReconcile). What the two
// pairs together close is the boundary this guard once stated, short of one
// stated straddle: no team invitation stands at a rank the authority that
// minted it no longer covers, whichever write lowered that authority —
// except a mint that rested on an org standing of admin or above
// (capped=false) whose INSERT lands after a lowering write's listing: the
// sweep's listing missed the row, and this confirmation, told no grant was
// measured, returns early, so the row stands at a rank that authority no
// longer covers until the next lowering of it re-lists the team. The
// standing half has no such gap — its confirmation re-reads the standing
// unconditionally. The one state the writes still hand OVER is the role
// mirrored up to NOTHING (config_editor, ADR-078), which no cap has ever
// measured.
//
// The re-read runs on a context DETACHED from the request and on its own
// budget, as the standing's does: a client that goes away is the most
// ordinary reason a cap cannot be re-confirmed, and a revoke that died with
// it would leave exactly the invitation this guard exists to remove. A grant
// that cannot be READ takes the invitation back too and answers 500 naming
// that read, under the audit cause of the row it could not read
// (mintGrantRead): a cap that cannot be confirmed is not one.
//
// It answers the request and returns false when the invitation did not
// survive.
func (s *Server) confirmTeamGrantAfterMint(w http.ResponseWriter, r *http.Request, id auth.Identity, capped bool, inv identity.Invitation) bool {
	if !capped {
		return true
	}
	rctx, rcancel := context.WithTimeout(context.WithoutCancel(r.Context()), grantCompensationTimeout)
	defer rcancel()
	mine, _, err := s.readTeamGrantRole(rctx, id.UserID, inv.TeamID)
	if err == nil && mine.AtLeast(inv.Role) {
		return true
	}
	reason := "creator_team_rank_dropped"
	if err != nil {
		reason = mintGrantRead.reason
	}
	fate := s.revokeUnconfirmedInvitation(r, inv, reason)
	if err != nil {
		if s.logger != nil {
			s.logger.Error("team %s: confirming the team grant of %s after invitation %s was minted failed: %v — %s",
				inv.TeamID, id.UserID, inv.ID, err, fate)
		}
		httpError(w, http.StatusInternalServerError,
			"confirming your own grant in the team this invitation joins after it was minted failed: %v — a cap that cannot be confirmed is not one, so %s",
			err, fate)
		return false
	}
	httpError(w, http.StatusForbidden,
		"a team invitation cannot carry a team role above its creator's grant: accepting an invitation at role %s writes that grant in this team, and your own is %s by the time it was minted — %s",
		inv.Role, teamGrantName(mine), fate)
	return false
}

// confirmOrgStandingAfterMint re-reads the minter's own org standing once the
// invitation row has LANDED, and revokes it unless that standing still covers
// the role it carries. requireRankToInvite's org cap is a check taken BEFORE
// the act, and the two share no transaction: a write that lowers the minter's own
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
			s.logger.Error("team %s: revoking invitation %s, whose cap the mint could not confirm, failed: %v — it stands at a rank its creator does not hold",
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

// requireRankOverMember refuses a roster write that names a member ranked
// above its author: no demotion, removal or placement reaches a row above the
// writer's own rank, read from the store on both sides. Every write in that
// domain calls THIS guard — the roster's PATCH and DELETE on an org row, and
// the PATCH, DELETE and PUT on a team grant — so the rule lives at one place
// rather than as a copy per route.
//
// Two ladders, each read from the ROW rather than from a token, which a write
// since does not rewrite:
//
//   - the ORG ladder (member < admin < owner) binds every write whose org is
//     known, team-scope ones included: the target's current org role there
//     against the writer's own standing (readOwnOrgStanding, the read the
//     role cap takes). A team admin whose org standing is `member` therefore
//     does not write over the grant of an org admin.
//   - the TEAM ladder (viewer < member < admin < owner) binds a team-scope
//     write whose authority comes from the writer's own GRANT alone, on BOTH
//     sides of that write: the rank the target holds, and the rank being
//     written (teamRole). An admin or owner of the team's parent org manages
//     every team in it (canManageTeam, orgAdminOfTeam), so for them the org
//     ladder above is the whole rule; config_editor is orthogonal to the
//     ladder (rank 0, ADR-078), so it outranks no grant and is capped by
//     none.
//
// The cap on the rank WRITTEN is the team twin of
// requireOrgStandingToWriteOrgRole, and it is what makes the target half a
// boundary rather than a delay: without it a team admin writes `owner` on
// their OWN grant and reaches every grant of the team on the next request.
// It binds that own row like any other.
//
// Equal rank passes, which is what leaves an owner writing an owner, an admin
// an admin, and any member their own row. A target with no row on a ladder
// stands below every rung of it. A rank this build cannot PLACE on its ladder
// is not below any of them, so it is refused rather than written over, and a
// rank that cannot be READ refuses the write with the store's error instead of
// writing unranked. Platform admins pass: IsSuperAdmin is the escape hatch,
// and they hold no membership row for a ladder to place them on — which is
// also the remedy every refusal here names, beside the standings that can
// make the write on the route that refused it (orgRankRemedy, teamRankRemedy).
//
// An author who holds no org row at all — an orphan team grant, which
// GET /api/admin/users/{id} reports as drift — stands below every rung of the
// org ladder, so the org ladder refuses them every target that HAS an org row
// (docs/cloud-rest-api.md states it). Their refusal names no rank of the
// target's: the org roster that rank is read from answers them 403.
//
// The ranks are read ONCE, as the last check before the write: a promotion of
// the target that lands between that read and the write is not seen. Closing
// that window takes a conditional membership write — a pkg/identity seam both
// stores would owe — rather than a second read, which would only move it.
//
// removesGrant marks the one write whose target may hold no row to rank at
// all: a team-member DELETE takes a grant away, and one that is not THERE
// takes nothing, so no ladder is read and the route keeps its own answer. That
// existence test is the SAME read the team ladder measures, taken first and
// once — a second read of the row could answer differently, and the earlier
// one deciding whether a ladder is read at all is the stale one. A grant that
// cannot be READ is not one that is absent: it refuses the removal with the
// store's error. A grant that appears AFTER that read is the boundary below.
//
// teamID is empty for an org-scope write; a team-scope one passes the team and
// the guard resolves its parent org itself, since the org ladder binds it too.
// A team that no longer EXISTS has no parent org to read, so the team ladder
// is the whole rule there, as for a team that never had one: a write that
// answered before this guard — clearing the dangling grant a deleted team
// leaves — is not turned into a 404 by it. teamRole is the team role the write
// puts on the row, empty when it removes a grant and on an org-scope write,
// whose own cap is requireOrgStandingToWriteOrgRole.
//
// It answers the request and returns false when the write must not happen.
func (s *Server) requireRankOverMember(w http.ResponseWriter, r *http.Request, id auth.Identity, orgID, teamID, memberID string, teamRole identity.Role, removesGrant bool) bool {
	if id.IsSuperAdmin {
		return true
	}
	ctx := r.Context()
	// The removal's own read, which the team ladder below measures rather
	// than re-reading: a grant that is not there leaves no rank to compare,
	// on either ladder.
	var theirs identity.Role
	var hasGrant, grantRead bool
	if removesGrant && teamID != "" {
		var err error
		theirs, hasGrant, err = s.readTeamGrantRole(ctx, memberID, teamID)
		if err != nil {
			httpError(w, http.StatusInternalServerError, "resolve the grant of %s in team %s: %v", memberID, teamID, err)
			return false
		}
		if !hasGrant {
			return true
		}
		grantRead = true
	}
	if teamID != "" {
		t, err := s.authStore().GetTeam(ctx, teamID)
		switch {
		case errors.Is(err, identity.ErrNotFound):
			orgID = ""
		case err != nil:
			httpError(w, http.StatusInternalServerError, "resolve the organization of team %s: %s", teamID, err.Error())
			return false
		default:
			orgID = t.OrgID
		}
	}
	standing := orgStandingAfterRemoval
	if orgID != "" {
		have, answered := s.ownOrgStanding(w, r, id, orgID)
		if answered {
			return false
		}
		standing = have
		// readOwnOrgStanding reads the row of the account it is GIVEN, so the
		// target's rank comes from the same read, with the same policy: a row
		// that is not there is a standing below every rung, and a row that
		// cannot be read is an error this guard must not compare against.
		theirs, hasRow, err := s.readOwnOrgStanding(ctx, memberID, orgID)
		if err != nil {
			httpError(w, http.StatusInternalServerError, "resolve the org membership of %s: %v", memberID, err)
			return false
		}
		if hasRow && !have.AtLeast(theirs) {
			httpError(w, http.StatusForbidden,
				"a roster write does not reach a member whose org rank the writer's own does not cover: %s — %s",
				orgRankRefused(memberID, theirs, have), orgRankRemedy(theirs, have, teamID != ""))
			return false
		}
	}
	// The team ladder measures the writer's own grant against the target's
	// and against the rank it writes. An admin or owner of the team's parent
	// org holds the team through the ORG — the same row canManageOrg reads,
	// so the standing above answers it — and is bound by the org ladder
	// instead.
	if teamID == "" || standing.AtLeast(identity.OrgRoleAdmin) {
		return true
	}
	mine, _, err := s.readTeamGrantRole(ctx, id.UserID, teamID)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "resolve your own grant in team %s: %v", teamID, err)
		return false
	}
	// config_editor carries no rank to raise (ADR-078), so the cap leaves the
	// capability alone and measures the ladder roles only.
	if teamRole != "" && teamRole != identity.RoleConfigEditor && !mine.AtLeast(teamRole) {
		httpError(w, http.StatusForbidden,
			"a team role is not written above the writer's own grant: writing the role %s in this team takes that grant, and your own is %s — %s",
			teamRole, teamGrantName(mine), teamRankRemedy(teamRole, orgID != ""))
		return false
	}
	if !grantRead {
		theirs, hasGrant, err = s.readTeamGrantRole(ctx, memberID, teamID)
		if err != nil {
			httpError(w, http.StatusInternalServerError, "resolve the grant of %s in team %s: %v", memberID, teamID, err)
			return false
		}
	}
	if !hasGrant || theirs == identity.RoleConfigEditor || mine.AtLeast(theirs) {
		return true
	}
	httpError(w, http.StatusForbidden,
		"a team grant is not written over a member whose team rank the writer's own does not cover: the role of %s in this team is %s, and your own is %s — %s",
		memberID, teamGrantName(theirs), teamGrantName(mine), teamRankRemedy(theirs, orgID != ""))
	return false
}

// requireRankOverTeamMembers refuses a team DELETION whose roster holds a
// member ranked above its author on the ORG ladder. Deleting a team clears
// every grant it holds, so it is a roster write over each of them at once —
// the rule is requireRankOverMember's org ladder applied to every row the
// deletion clears, at the one place that clears them.
//
// The TEAM ladder does not apply here: it governs a write whose author's
// authority over the team is their own GRANT, and this route admits none —
// its gate is canManageOrg (an org admin or owner standing, read from the
// row) or a platform admin, so the org standing above is the author's whole
// authority over the team, on the target's rank and on their own. A team
// with no parent org answers at the gate: the route's explicit OrgID
// disjunct refuses EVERYONE there — a platform admin included, whose
// exemption lives only where the gate can read an org — before canManageOrg
// runs, so no ladder is read for a deletion nothing authorizes.
//
// The author's standing and each member's rank are read from the ROWS, the
// same reads requireRankOverMember takes (ownOrgStanding,
// readOwnOrgStanding): a row that is not there stands below every rung, a
// rank that cannot be READ refuses the deletion with the store's error, and
// the author's own row having gone away in between leaves them a standing of
// none, below every member that holds a row.
//
// members is the team's roster the caller has already read for the deletion
// itself — one read serves the rule and the act, and the rule runs BEFORE the
// first revoke, so a refused deletion leaves the team and every grant it
// holds as they were. Where the refusal sits in the route's own order: after
// the 404 (no team), the 403 gate, the 422 (personal team), the 409s (the
// author's active team, the residue) and the 500 of that members read, and
// before any side effect. The residue 409 precedes it, so a rank-refused
// author naming a non-empty team learns the tenant holds data first, and
// only once the team is empty does this guard answer them — refusing before
// the residue is looked at is the gate's own 403, higher up.
//
// Equal rank passes, which is what leaves an org admin deleting a team whose
// members stand at or below admin; an empty roster passes with nothing to
// compare. Platform admins pass: IsSuperAdmin is the escape hatch, and they
// hold no membership row for a ladder to place them on — which is also the
// remedy every refusal here names, beside the owner standing that tops the
// ladder and so is never outranked; a rank this build cannot place is
// covered by no standing at all, and there the remedy is the platform admin
// alone (deleteTeamRemedy).
//
// It answers the request and returns false when the deletion must not happen.
func (s *Server) requireRankOverTeamMembers(w http.ResponseWriter, r *http.Request, id auth.Identity, orgID string, members []identity.Membership) bool {
	if id.IsSuperAdmin {
		return true
	}
	have, answered := s.ownOrgStanding(w, r, id, orgID)
	if answered {
		return false
	}
	for _, m := range members {
		theirs, hasRow, err := s.readOwnOrgStanding(r.Context(), m.UserID, orgID)
		if err != nil {
			httpError(w, http.StatusInternalServerError, "resolve the org membership of %s: %v", m.UserID, err)
			return false
		}
		if hasRow && !have.AtLeast(theirs) {
			httpError(w, http.StatusForbidden,
				"a team is not deleted over a member whose org rank the author's own does not cover: %s — %s",
				orgRankRefused(m.UserID, theirs, have), deleteTeamRemedy(theirs))
			return false
		}
	}
	return true
}

// orgRankRefused states the two org ranks a refusal of the org ladder is
// about. A writer who holds NO org row reads no org roster — canViewOrg
// refuses them — so their refusal names the target's rank as being above
// their own rather than naming the rank itself.
func orgRankRefused(memberID string, theirs, have identity.OrgRole) string {
	if have == orgStandingAfterRemoval {
		return fmt.Sprintf("the org role of %s in this org stands above your own, which is none", memberID)
	}
	return fmt.Sprintf("the org role of %s in this org is %s, and your own is %s",
		memberID, orgStandingName(theirs), orgStandingName(have))
}

// orgRankRemedy names who writes the org rank the org ladder refused, FOR THE
// ROUTE that refused it: the org roster's own writes are gated on
// canManageOrg, which any member holding the target's rank passes, while a
// team-scope write is gated on canManageTeam, which inside an org only its
// admins and owners pass — a plain member of the org holding the target's
// rank reaches no team of it. A rank this build cannot PLACE is covered by no
// standing at all (OrgRole.AtLeast is false on both sides of rank zero), so
// there a platform admin is the only remedy; and a writer with no org row is
// named a remedy that does not disclose the rank theirs is.
func orgRankRemedy(theirs, have identity.OrgRole, teamScope bool) string {
	switch {
	case !theirs.Valid():
		return "a platform admin writes it"
	case !teamScope && have != orgStandingAfterRemoval:
		return fmt.Sprintf("a member of this org who already holds %s writes it, or a platform admin", theirs)
	case theirs == identity.OrgRoleOwner || have == orgStandingAfterRemoval:
		return "an owner of this organization writes it, or a platform admin"
	}
	return "an admin or owner of this organization writes it, or a platform admin"
}

// deleteTeamRemedy names who deletes the team the container rule refused, so
// the refusal offers only a remedy that works on this route: an owner tops
// the org ladder the rule measures and is never outranked by it, while a rank
// this build cannot PLACE is covered by no standing at all (OrgRole.AtLeast
// is false on both sides of rank zero) — there an owner would execute the
// remedy and be refused again, and a platform admin is the only one who
// reaches the row.
func deleteTeamRemedy(theirs identity.OrgRole) string {
	if !theirs.Valid() {
		return "a platform admin deletes this team"
	}
	return "an owner of this organization, or a platform admin, deletes this team"
}

// teamRankRemedy names who writes the team rank the team ladder refused,
// whether it refused the target's or the one being written: a member of the
// team holding that rank — named only where canManageTeam admits them at all,
// which is admin and above — an admin or owner of the parent org where the
// team has one, and a platform admin.
func teamRankRemedy(role identity.Role, hasOrg bool) string {
	var remedies []string
	if role.AtLeast(identity.RoleAdmin) {
		remedies = append(remedies, fmt.Sprintf("a member of this team who already holds %s writes it", role))
	}
	if hasOrg {
		remedies = append(remedies, "an admin or owner of the team's organization")
	}
	return joinRemedies(remedies, "a platform admin")
}

// joinRemedies reads a list of remedies as a sentence does, with last after an
// "or" — the remedy every refusal here ends on, which is why it is named
// apart rather than taken from the end of the list.
func joinRemedies(remedies []string, last string) string {
	if len(remedies) == 0 {
		return last
	}
	return strings.Join(remedies, ", ") + ", or " + last
}

// readTeamGrantRole reads one account's role in teamID — the grant ROW, never
// a token's claim — and answers whether that row is THERE at all. An account
// with no grant holds no team role, which the ladder places below every rung,
// so ErrNotFound is an answer and not an error; any other failure is returned,
// for a guard that must not compare a rank it could not read.
func (s *Server) readTeamGrantRole(ctx context.Context, userID, teamID string) (identity.Role, bool, error) {
	mb, err := s.authStore().GetMembership(ctx, userID, teamID)
	switch {
	case errors.Is(err, identity.ErrNotFound):
		return "", false, nil
	case err != nil:
		return "", false, err
	}
	return mb.Role, true, nil
}

// teamGrantName names a team role in a refusal, including the one an account
// holding no grant in the team has (orgStandingName's twin on the team
// ladder).
func teamGrantName(role identity.Role) string {
	if role == "" {
		return "none"
	}
	return string(role)
}

// clientIP names the client of an inbound request: the address audit
// rows carry and the auth rate limiter keys on. Three rules make it an
// identity rather than a connection:
//
//   - No port. r.RemoteAddr carries the ephemeral source port, so an
//     identity built from it changes on every TCP connection and a fresh
//     rate-limit bucket comes free with each one (#2673).
//   - Forwarded addresses only from a trusted peer. X-Forwarded-For,
//     Forwarded and X-Real-IP are all client-writable, so they are read
//     only when the immediate peer is inside s.cfg.TrustedProxyCIDRs,
//     which the operator configures for the proxy actually in front.
//     Without that guard a client spoofs its audit IP, and buys a fresh
//     bucket per forged value, just by sending a header.
//   - One spelling per host, and nothing that is not an address.
//     "2001:DB8::17", "2001:0db8::17" and "::ffff:203.0.113.5" are one
//     host each however they are written, so cycling spellings buys no
//     buckets; a value that is not an address at all never becomes the
//     audit IP (see canonicalIP).
//
// This is the identity, which is ONE address: the rate limiter charges
// the GROUP an IPv6 address belongs to instead, because a client holding
// a prefix is not one address (rateBucketIP). An audit row names the
// client that acted, a bucket groups the clients one of them can speak
// for.
//
// Behind a proxy whose range is NOT configured every request carries the
// proxy's own address, so the whole instance shares one bucket: the
// ranges are what makes the limiter per-client, and
// docs/environment-variables.md says so next to the variable.
func (s *Server) clientIP(r *http.Request) string {
	ip, named := s.forwardedClientIP(r)
	if ip != "" {
		s.warnIdentityIsOurs(r, ip)
		return ip
	}
	s.warnForwardedNotRead(r, named)
	host := addrHost(r.RemoteAddr)
	if ip := canonicalIP(host); ip != "" {
		s.warnIdentityIsOurs(r, ip)
		return ip
	}
	if host == "" {
		// No peer address at all. net/http's listener always sets one, so
		// this is an embedder's own transport or a handler called
		// directly — and the empty string would be a key every such
		// caller shares, one bucket and one blank audit row for all of
		// them. A name that is not an address says what happened and
		// collides with no client.
		return unknownPeer
	}
	// A peer address that is not an IP — a unix socket, a test's own
	// spelling — is what the kernel reported rather than what a header
	// claims, so it stays the identity. It is not per-client, and cannot
	// be: a unix listener spells every peer that bound no name as "@",
	// and an ordinary client binds none, so everything behind such a
	// listener shares the one bucket "@" by construction. No server path
	// listens on one today; an embedder that does gets the limiter a
	// unix socket can have.
	return host
}

// unknownPeer is the identity of a request whose transport reported no
// peer address: a name, so the rows and buckets of such requests are
// legible instead of blank, and one no address can be confused with.
const unknownPeer = "unknown-peer"

// forwardedClientIP returns the client a TRUSTED peer named, or "" when
// none did and the caller must fall back to the socket — and beside it,
// how many ADDRESSES the walk read before it stopped. The second answer
// is the diagnosis's, not the identity's: "" is the answer both when the
// hops we account for named no address at all and when they named only
// hops of our own, fewer than the declared bound — a chain SHORTER than
// the count with every entry in-range, or one whose front end named no
// client — and the two want different lines (warnForwardedNotRead).
//
// The chain is read RIGHT TO LEFT and the answer is the entry the
// OUTERMOST hop we can account for wrote — trustedProxyHops entries in
// from the right. A proxy that appends rather than replaces
// (ingress-nginx with `use-forwarded-headers` AND
// `compute-full-forwarded-for` on — the first alone makes it resolve
// the client and write that one address — writes "<whatever the client
// sent>, <the address it saw>") leaves the client's own forgeries on
// the LEFT, so everything left of that entry is attacker-controlled in
// exactly the deployment the ranges are configured for, and a client
// can only make the chain LONGER — never move the hop its proxy
// appended.
//
// The COUNT is what bounds the walk, not the trust of what it reads.
// Skipping every entry that happens to be inside the ranges would walk
// straight back into the client's half of the chain whenever the client
// itself holds an in-range address: with the chart default — all of
// private space — that is every LAN client of a self-hosted instance and
// every in-cluster pod the NetworkPolicy admits, and such a client would
// buy a fresh bucket per forged value again by writing one
// OUT-of-range entry of its own (#2673's own defect, through this
// header). An entry inside the ranges is therefore skipped only while
// fewer than trustedProxyHops of them have been, and the entry that
// reaches the bound is the answer whether it is in-range or not.
//
// What that entry is worth depends on who wrote it. Behind a trusted
// proxy that APPENDED, with trustedProxyHops set to the number of
// appending proxies actually in front, it is an address one of them
// really saw and no header moves it. A peer that reaches the port
// DIRECTLY wrote the whole chain itself, so its rightmost entry is
// client-chosen too: it names any address it likes, including an
// in-range victim's, and rotating the value moves the key. That is the
// same residual as the sentence below — trusting a range means trusting
// what every address inside it says — and the answer to both is to keep
// the ranges to the proxy's own addresses, which values.yaml and
// docs/environment-variables.md both say, not to second-guess a trusted
// peer here. `networkPolicy.enabled` is what keeps the port out of
// reach of everything but the ingress (#2804).
//
// What stays by construction, whatever the walk does: a peer inside the
// ranges is a proxy the operator vouches for, so it can still name an
// address OUTSIDE them and be believed.
func (s *Server) forwardedClientIP(r *http.Request) (string, int) {
	if !s.peerIsTrusted(r) {
		return "", 0
	}
	chain := s.forwardedChain(r)
	hops := s.trustedProxyHops()
	// One right-to-left pass. An entry that names no address is still a
	// hop, and the one nothing beyond is accountable to: the proxy that
	// wrote it — an RFC 7239 for=unknown or obfuscated identifier, an
	// element with no for= at all, an X-Forwarded-For "unknown" — named
	// no client, so every entry left of it is whatever the client wrote.
	// Stepping over it would read the client's own forgery as its
	// identity; the walk stops there and the caller falls back to the
	// socket peer, the proxy's own address.
	ours, named := 0, 0
	for i := len(chain) - 1; i >= 0; i-- {
		ip := canonicalIP(chain[i])
		if ip == "" {
			return "", named
		}
		named++
		if !s.ipIsTrusted(ip) {
			// No proxy of ours wrote this one, so nothing further left
			// is accountable either: this is the furthest hop we can
			// name. The bound did not arbitrate, so whether it COULD
			// have is the operator's business.
			s.warnHopsUnreachable(r, ours, hops)
			return ip, named
		}
		ours++
		if ours >= hops {
			// The bound: the address the outermost proxy we are told to
			// account for actually saw. Everything left of it is what
			// whoever that proxy was talking to chose to write.
			if i > 0 {
				s.warnUndeclaredHop(r, chain[i-1], hops)
			}
			return ip, named
		}
	}
	// "" hands the caller back to the socket peer, which is itself a
	// trusted proxy here: an identity the kernel reports, not one a header
	// claims. Reached when the chain names no address at all, when a hop
	// within the bound names none (above), and when it is SHORTER than
	// the declared bound and every entry is ours — an
	// operator who declares more appending hops than are really in front
	// gets the proxy's own address, which is less reach than the
	// over-declaration asked for and never a client-written entry. The
	// count tells the caller which of the two it was.
	return "", named
}

// defaultTrustedProxyHops is how many APPENDING trusted proxies sit in
// front when the operator says nothing: one, the shape the chart ships
// (a single ingress controller) and the shape every reverse-proxy
// quickstart describes.
const defaultTrustedProxyHops = 1

// trustedProxyHops is how many of the chain's rightmost entries our own
// infrastructure wrote — the number of APPENDING trusted proxies between
// a client and this server. It is the bound the right-to-left walk stops
// at, so it is also how far a client's own forgeries can be pushed:
// declaring MORE hops than are really in front walks the answer into the
// client's half of the chain, which is why the default is the shipped
// shape and raising it is the operator's explicit act.
//
// A chained front end needs it: a CDN edge that appends and an ingress
// that appends behind it write TWO entries of their own, and the client
// the operator wants is the second from the right. Set via
// ITERION_AUTH_TRUSTED_PROXY_HOPS; pkg/config refuses a negative value
// at boot. The walk is bounded by maxForwardedHops whatever the value:
// a count larger than the chain reaches its leftmost address, or — when
// every entry is inside the ranges — no entry at all, and the caller
// falls back to the socket peer.
func (s *Server) trustedProxyHops() int {
	if s == nil || s.cfg.TrustedProxyHops < defaultTrustedProxyHops {
		return defaultTrustedProxyHops
	}
	return s.cfg.TrustedProxyHops
}

// defaultForwardedHeader is the mechanism read when the operator names
// none: the one every ingress controller and reverse proxy writes.
const defaultForwardedHeader = "X-Forwarded-For"

// forwardedHeaderCandidates are the mechanisms a proxy in front might be
// writing instead of the declared one. They feed the diagnosis ONLY —
// what is read is what the operator declared, never what a request
// happens to carry.
var forwardedHeaderCandidates = []string{"X-Forwarded-For", "Forwarded", "X-Real-IP"}

// forwardedHeader is the ONE header a forwarded client address is read
// from: the mechanism the OPERATOR declares, never the one the request
// picks. Reading "the first mechanism that carries any" hands the choice
// to the client: a proxy that computes the client into X-Real-IP (or
// RFC 7239 Forwarded) and passes X-Forwarded-For through untouched — the
// canonical hand-written nginx snippet, any HAProxy or CDN writing only
// Forwarded — is then beaten by a header the client wrote, which names
// its own identity, buys a fresh rate-limit bucket per value and lands a
// chosen address in the audit row. The right-to-left walk cannot help
// there: the whole chain is the client's.
//
// Any field name is accepted, not a closed set of three: a deployment
// behind a CDN reads CF-Connecting-IP or True-Client-IP, and refusing
// those would be a bound with no override. pkg/config refuses a value
// that is not a single field name at boot, naming it. "Forwarded" is
// read with the RFC 7239 grammar; anything else is a comma-separated
// address list, which is the one entry of an X-Real-IP as much as the
// chain of an X-Forwarded-For.
func (s *Server) forwardedHeader() string {
	if s == nil {
		return defaultForwardedHeader
	}
	if h := strings.TrimSpace(s.cfg.ForwardedHeader); h != "" {
		return h
	}
	return defaultForwardedHeader
}

// maxForwardedHops bounds the entries the reader KEEPS — the hops an
// identity can come from. A real chain is a handful of them, and the far
// end of a chain longer than this is not accountable anyway; the ones
// kept are those nearest us, so a client that pads its way past the
// bound lands on its proxy's own bucket, which is less reach and not
// more.
//
// It is NOT the cost bound. The header is client-writable up to the
// server's MaxHeaderBytes (1 MiB by default) and this runs BEFORE the
// limiter has a key to refuse the request with, so the work is bounded
// on its own terms — maxForwardedLooks for the positions visited,
// maxForwardedScans for the values opened, maxForwardedBytes for how far
// a separator is hunted. Spending the hop budget on the cost would be
// the conflation that lets a run of separators hide a hop.
const maxForwardedHops = 32

// maxAddrLen bounds how LONG an entry may be, the way maxForwardedHops
// bounds how MANY there are. Both halves are needed, and the count alone
// is not the cost: a client-writable header reaches MaxHeaderBytes
// (1 MiB by default) as ONE entry as easily as a million, and
// net.ParseIP walks every byte of whatever it is handed — measured on
// the pre-limiter path, a 1 MiB single entry cost 1.8 ms of CPU per
// request against 7 us for a full 32-hop chain.
//
// 53 bytes is the longest form canonicalIP can accept: an IPv4-mapped
// IPv6 address written out, bracketed and ported
// ("[0000:0000:0000:0000:0000:ffff:255.255.255.255]:65535"). 64 leaves
// slack for a spelling net.ParseIP may learn while staying far under
// anything a chain of real hops reaches.
const maxAddrLen = 64

// maxForwardedBytes is how much of a header value the reader LOOKS at:
// the bytes nearest us, sized to hold maxForwardedHops entries of
// maxAddrLen each with their separators. Nothing further away can be an
// entry this reader would keep, so a separator further away is one it
// never needs to find — and finding one is the last cost that grows
// with a value the client chooses: measured on the pre-limiter path,
// one memchr over a 1 MiB entry cost 315 us per request and the three
// an RFC 7239 element needs cost 1.2 ms, against 4 us for a full chain.
//
// It bounds the WALK, as maxForwardedHops bounds the entries and
// maxAddrLen their length; and like them it can only cost a client
// reach: padding its way past the window pushes its own forgeries out
// of it, leaving the hops nearest us — its proxy's own address, one
// bucket for everything behind that proxy. The hop an appending proxy
// writes is the LAST byte of the value, so no padding moves it out. An
// RFC 7239 element the proxy itself wrote longer than the window — a
// client-typed host="…" — is not one the reader can delimit, so it is
// not read and the identity is the proxy's (lastEntries).
const maxForwardedBytes = maxForwardedHops * (maxAddrLen + 1)

// maxForwardedLooks bounds the entry POSITIONS one request's header is
// worth visiting — a hop and an empty entry alike. It is the COST bound,
// and it is deliberately slack against maxForwardedHops because an empty
// entry is a malformed chain and not a hop: "203.0.113.5,,,,…" with
// thirty-two commas after it would otherwise spend the hop budget on
// nothing and bury the only address in the value. The slack also covers
// the parameters an RFC 7239 element carries beside its for=, which are
// looked at from the same purse.
const maxForwardedLooks = 8 * maxForwardedHops

// maxForwardedScans bounds the VALUES one request's header is worth
// opening — its lines, and the RFC 7239 elements whose parameters are
// read the same way. Each costs one scan of its window to find where an
// entry begins, including a window with no separator anywhere in it,
// which yields no entry at all: unbounded, five hundred such lines cost
// five hundred scans before the limiter has a key to refuse the request
// with. Twice the hop bound covers the lines of a value plus the
// elements they yield, both already capped by the hop bound itself.
//
// The CPU ceiling is these constants times the window, never the
// header: at most this many windows of maxForwardedBytes per pass, each
// a few memory scans (IndexByte, LastIndexByte, TrimSpace), and the RFC
// 7239 reader makes two passes from two purses (forwardedHeaderChain) —
// a few hundred kilobytes of byte scanning per request at the very
// worst, tens of microseconds, against under a microsecond for a real
// chain. Bounded and paid before the limiter has a key, which is why it
// is written down: the allocation witnesses cannot see it, a memory
// scan allocates nothing.
const maxForwardedScans = 2 * maxForwardedHops

// forwardedBudget is what ONE read of a client-writable header is worth.
// The bounds are held APART because conflating them lets a client spend
// the one that matters on entries that are no hops at all — a run of
// separators, an entry too long to be an address — and the hop a proxy
// appended falls out of reach:
//
//   - keep bounds the entries a cut MATERIALISES: the hops an identity
//     can come from.
//   - looks and scans bound the WORK, and they are spent across every
//     line, element and parameter of the request rather than per call:
//     splitting a value must not multiply what reading it costs.
//
// A request that spends its work budget keeps the hops already
// collected and reads no further. The entries nearest us are collected
// FIRST, so what a padding client loses is its own forgeries: it lands
// on its proxy's bucket, which is less reach and not more.
type forwardedBudget struct {
	keep  int
	looks int
	scans int
}

// newForwardedBudget is the budget of one request's header read, handed
// to every cut that read makes.
func newForwardedBudget() *forwardedBudget {
	return &forwardedBudget{keep: maxForwardedHops, looks: maxForwardedLooks, scans: maxForwardedScans}
}

// forwardedChain returns the forwarded addresses as the proxies wrote
// them — the client first, each hop appended — from the one header
// forwardedHeader names, bounded to the maxForwardedHops entries nearest
// this server and to the entries that could BE an address. Header.Values,
// not Get: a client can send its own line and the proxy adds a second
// one, and only the whole list is the chain.
//
// Every bound lives here, on the one path every forwarded entry takes to
// an identity, so the readers below are handed entries whose number and
// length nothing client-controlled decides. ONE budget serves the whole
// read — its lines, its RFC 7239 elements and their parameters — so a
// client cannot multiply the work by splitting its value across them.
// What remains linear in the header is the cutting itself —
// LastIndexByte and Cut are a memory scan, which no reader of a header
// can be cheaper than.
func (s *Server) forwardedChain(r *http.Request) []string {
	name := s.forwardedHeader()
	values := r.Header.Values(name)
	budget := newForwardedBudget()
	var entries []string
	if strings.EqualFold(name, "Forwarded") {
		entries = forwardedHeaderChain(values, budget)
	} else {
		entries = lastEntries(values, ',', maxAddrLen, plainCut, budget)
	}
	// An entry longer than any address names none. Measuring it here
	// rather than letting canonicalIP answer "" on it is the difference
	// between a memory scan and net.ParseIP's per-byte walk over a
	// megabyte, on a path that runs BEFORE the limiter has a key to
	// refuse the request with. The address list refuses one against the
	// hop budget itself — no proxy writes such an entry, so it is the
	// client's and never a hop; an RFC 7239 for= parameter cannot be
	// refused that way — a parameter is legitimately longer than an
	// address, and an obfuscated identifier has no length bound — so its
	// value is measured here and its element stays a hop that names no
	// address (forwardedClientIP stops on it). Both producers return a
	// fresh slice of at most maxForwardedHops entries, so it is rewritten
	// in place.
	for i, entry := range entries {
		if len(entry) > maxAddrLen {
			entries[i] = unnamedHop
		}
	}
	return entries
}

// unnamedHop stands in the chain for a hop that names no address — an
// RFC 7239 element with no for=, an empty one, one too long to be an
// address. It is the RFC's own spelling of "this hop names no client",
// which canonicalIP refuses like every non-address.
const unnamedHop = "unknown"

// unreadableElement is what an RFC 7239 cut leaves in place of an
// element its byte window cannot delimit: the RFC's own element for a
// hop that names no client, so both cuts read it as one — the elements
// cut as an element, the parameters cut as its for= — and the walk
// stops on it (lastEntries).
const unreadableElement = "for=" + unnamedHop

// lastEntries returns the last entries of values cut on sep, in the
// order the header spells them, trimmed and without the empty ones —
// at most b.keep of them, and at most keepLen bytes each when keepLen is
// not 0. The grammar says whether a sep inside a quoted-string cuts:
// under quotedCut it is part of the entry (lastSep).
//
// It cuts from the RIGHT and stops, so the entries it MATERIALISES are
// the ones kept and not every entry of the header: strings.Split cost
// 10.8 MB of garbage and 3.5 ms of CPU per request on a 1 MiB
// X-Forwarded-For, on the pre-auth path, enough for one unauthenticated
// caller to saturate a 1-core pod before the limiter could refuse it.
//
// What stops it is the budget, and WHICH bound stops it is the point: an
// empty entry and an entry too long to be an address are positions
// LOOKED at, never hops KEPT, so no run of either can push the hop a
// proxy appended out of a value. A line whose window holds no separator
// is a scan spent and no entry read, which is what keeps five hundred of
// them from costing five hundred scans.
func lastEntries(values []string, sep byte, keepLen int, grammar cutGrammar, b *forwardedBudget) []string {
	out := []string{}
	for i := len(values) - 1; i >= 0 && len(out) < b.keep && b.looks > 0 && b.scans > 0; i-- {
		b.scans--
		v := values[i]
		// The window opens wherever the arithmetic landed, which may be
		// INSIDE an entry — half of "203.0.113.5" is "3.0.113.5", an
		// address no proxy ever wrote — or inside a quoted-string, where
		// a separator is part of a value and delimits nothing. Only the
		// separators the cut below finds from OUR end, the one edge whose
		// quoted state is known, delimit an entry; so the piece left of
		// the last of them is a fragment no writer of the header
		// delimited, and it is never read. A window with no such
		// separator is the middle of one enormous entry: no hop of this
		// line is reachable, and none is read.
		//
		// What the fragment WAS decides what comes after it. In an
		// address list it is a piece of an entry the client wrote: every
		// entry a proxy writes is one short address at the end of the
		// value, read before any fragment, so the earlier lines are read
		// on. Under quotedCut it may be the proxy's own RFC 7239 element,
		// one whose client-typed host="…" outgrew the window — and every
		// line before it is the client's. That fragment is a hop the
		// reader cannot read, so it stands in the chain as one that
		// names no client (unreadableElement) and the read ends there:
		// the identity falls back to the socket peer, never to bytes
		// inside the quotes nor to an earlier line.
		truncated := len(v) > maxForwardedBytes
		if truncated {
			v = v[len(v)-maxForwardedBytes:]
		}
		for len(out) < b.keep && b.looks > 0 {
			b.looks--
			cut := lastSep(v, sep, grammar)
			if cut < 0 && truncated {
				if grammar == quotedCut {
					out = append(out, unreadableElement)
					slices.Reverse(out)
					return out
				}
				break
			}
			if entry := strings.TrimSpace(v[cut+1:]); entry != "" && (keepLen == 0 || len(entry) <= keepLen) {
				out = append(out, entry)
			}
			if cut < 0 {
				break
			}
			v = v[:cut]
		}
	}
	slices.Reverse(out)
	return out
}

// cutGrammar says whether a separator inside a quoted-string delimits.
// An address list has no quoting in its grammar, so every separator
// cuts. RFC 7239's two cuts read values that may be quoted-strings —
// for="[2001:db8::1]:443" is the RFC's own spelling of an IPv6 address
// — and a proxy that writes the client's Host into host="…" hands the
// reader whatever the client typed there, nginx's Host accepting ';',
// ',' and '='. There a separator inside the quotes is part of the value.
type cutGrammar bool

const (
	plainCut  cutGrammar = false
	quotedCut cutGrammar = true
)

// lastSep is the index of the last sep of v that cuts, or -1. Under
// quotedCut it reads from the RIGHT, a DQUOTE toggling the quoted state
// unless an odd run of backslashes escapes it (an RFC 7230 quoted-pair),
// so the value nearest us — the element a proxy appended, the parameter
// it wrote last — is parsed from a known state whatever an unbalanced
// quote further left does to what a client wrote there.
//
// It honours the quotes a writer balanced, and no reader can do more. A
// proxy that interpolates client bytes into a quoted value without
// escaping DQUOTE and backslash lets the client close the string and
// write parameters or a whole element of its own, well-formed ones every
// reader must believe, or leave the quotes unbalanced, which misreads
// the rest of that element from this end. nginx's $host in a
// hand-written Forwarded template is such a value on releases before
// 1.29.4, which accept both characters in a Host; 1.29.4 validates the
// Host as RFC 3986 host syntax and refuses them, leaving a client the
// sub-delims (';', ',', '=') this cut honours. Where the Host still
// carries a DQUOTE, the deployment is open whatever the reader does and
// its remedy is the template: no client bytes in Forwarded, or
// X-Forwarded-For or X-Real-IP from $remote_addr.
func lastSep(v string, sep byte, grammar cutGrammar) int {
	if grammar == plainCut {
		return strings.LastIndexByte(v, sep)
	}
	quoted := false
	for i := len(v) - 1; i >= 0; i-- {
		switch v[i] {
		case sep:
			if !quoted {
				return i
			}
		case '"':
			if !escapedAt(v, i) {
				quoted = !quoted
			}
		}
	}
	return -1
}

// escapedAt reports whether an odd run of backslashes precedes v[i]:
// inside a quoted-string that is a quoted-pair, and the DQUOTE it
// escapes neither opens nor closes the string.
func escapedAt(v string, i int) bool {
	run := 0
	for j := i - 1; j >= 0 && v[j] == '\\'; j-- {
		run++
	}
	return run%2 == 1
}

// forwardedHeaderChain reads the for= parameter of every element of an
// RFC 7239 Forwarded header, in order. A quoted value carries a port, an
// IPv6 address or both, and the quotes come off. The obfuscated forms the
// RFC allows in place of an address ("unknown", "_hidden") name no
// address, and neither does an element with no for= or an empty one —
// but each is still the hop its proxy wrote, so it stays in the chain as
// one that names no client (unnamedHop) and the walk stops on it.
// Dropping it would hand the walk the element left of it, which is the
// client's: `for=9.9.9.9, for=unknown` behind one appending proxy would
// resolve to 9.9.9.9.
//
// Bounds: the elements nearest us, their last parameters, and the hops
// nearest us of what that yields — the header is as client-writable as
// X-Forwarded-For, and one element carrying a hundred thousand
// parameters must cost no more than a chain of thirty-two.
//
// Two cuts, two purses. The grammar makes this reader two-pass —
// elements on the comma, then each element's parameters on the
// semicolon — and the passes spending ONE purse let a client starve the
// second with the first: 255 empty elements left of the proxy's spent
// every look, 63 empty lines spent every scan, and the proxy's element,
// collected intact at the very first look, then yielded no for= at all.
// The chain was empty, the identity fell to the socket peer, and the
// socket peer is the trusted proxy — one bucket for everyone behind it
// and the proxy's address in the audit row, at the client's choosing,
// which is the collapse the chokepoint exists to refuse. So the
// parameters are read from a purse of their own, and from the element
// nearest us OUTWARD: the work stays bounded at twice the constants, and
// what exhaustion costs is the client's far elements, never the one its
// proxy appended. X-Forwarded-For is single-pass and was never exposed.
//
// Both cuts honour the quoted-string (quotedCut): a ';' or a ',' inside
// one is part of the value and never cuts. A proxy that writes the
// client's Host into host="…" puts client-typed bytes inside its OWN
// element, and under plain cuts a ';' there ends that element with a
// for= the client wrote — its chosen identity, a fresh bucket per value
// — while a quoted run of parameters spends the keep bound before the
// proxy's own for= is reached, so the element yields nothing and the
// identity collapses onto the socket peer; a ',' does the same through
// the elements cut. With the quotes honoured, pushing the proxy's for=
// past the keep bound takes as many real parameters, which a client
// cannot write inside a value its proxy escapes; a proxy that does not
// escape is open to any reader (lastSep).
func forwardedHeaderChain(values []string, b *forwardedBudget) []string {
	out := []string{}
	// No length bound on either cut: an element and a parameter are
	// legitimately longer than an address, and forwardedChain measures
	// what a for= finally names.
	elements := lastEntries(values, ',', 0, quotedCut, b)
	params := newForwardedBudget()
	for i := len(elements) - 1; i >= 0 && len(out) < maxForwardedHops && params.looks > 0 && params.scans > 0; i-- {
		// Nearest us first, so out fills from our end; reversed below
		// into the order the header spells.
		fors := lastEntries([]string{elements[i]}, ';', 0, quotedCut, params)
		named := false
		for j := len(fors) - 1; j >= 0; j-- {
			key, value, found := strings.Cut(fors[j], "=")
			if !found || !strings.EqualFold(strings.TrimSpace(key), "for") {
				continue
			}
			named = true
			value = strings.Trim(strings.TrimSpace(value), `"`)
			if value == "" {
				value = unnamedHop
			}
			out = append(out, value)
		}
		if !named {
			out = append(out, unnamedHop)
		}
	}
	slices.Reverse(out)
	if len(out) > maxForwardedHops {
		return out[len(out)-maxForwardedHops:]
	}
	return out
}

// canonicalIP turns a forwarded or socket address into one canonical
// spelling of one IP, or "" when it is not an IP at all. Both halves
// matter for an identity: "2001:DB8::17", "2001:0db8::17" and
// "::ffff:203.0.113.5" are one host each however they are spelled, so
// cycling spellings must not buy fresh rate-limit buckets; and a value
// that is not an address at all ("unknown", or a string carrying CRLF)
// must never become the audit IP or reach a log line.
func canonicalIP(addr string) string {
	ip := net.ParseIP(addrHost(addr))
	if ip == nil {
		return ""
	}
	if v4 := ip.To4(); v4 != nil {
		return v4.String()
	}
	return ip.String()
}

// addrHost drops the port from an address, the brackets from a bare
// IPv6 literal and the zone from a scoped one. SplitHostPort refuses a
// portless host and a bracketless IPv6 address, which is what a
// forwarded entry and a portless RemoteAddr both are, so its failure is
// the common case and not an error — and it allocates an error value for
// it, which only a value that COULD carry a port is worth paying: one
// colon, or a bracketed literal. Everything else goes straight to the
// brackets.
//
// The zone comes off because the kernel puts it on: net's listener
// spells a link-local peer "[fe80::1%eth0]:443", net.ParseIP refuses
// the "%eth0", and an identity that is not an address is its own bucket
// — so every address of fe80::/64, which any neighbour on the link can
// mint from, was a fresh bucket: the class the /64 widening closes for
// every other IPv6 client, open on the socket path. Stripped, the
// address canonicalises and buckets like any other; the interface it
// came in on is not what a bucket or an audit row is keyed on.
func addrHost(addr string) string {
	addr = strings.TrimSpace(addr)
	host := addr
	if strings.HasPrefix(addr, "[") || strings.Count(addr, ":") == 1 {
		if h, _, err := net.SplitHostPort(addr); err == nil {
			host = h
		}
	}
	host = strings.TrimSpace(strings.Trim(host, "[]"))
	if strings.Contains(host, ":") {
		host, _, _ = strings.Cut(host, "%")
	}
	return host
}

// warnForwardedNotRead says ONCE per cause why a deployment that IS
// configured to read a forwarded client address did not read one. Its
// premise is its one caller's branch: clientIP calls it only when
// forwardedClientIP answered nothing, so NO forwarded address was read
// from this request, whatever it carries; named is how many addresses
// the declared header named on the way to that nothing. The
// boot warning covers only an EMPTY list, and the outcome it exists to
// announce — every request keyed on the proxy's own address, so the auth
// rate limiter holds one bucket for the whole instance — is the same when
// the list is non-empty but misses the real peer (a Service CIDR instead
// of a pod CIDR, a node-SNAT shape, an Istio-style sidecar whose inbound
// peer is 127.0.0.6), when the declared header is not the one the proxy
// writes, and when the chain is every bit ours and shorter than the
// hops declared. Those are the likelier operator errors now that the
// chart ships a default to narrow, and they are the ones nothing in the
// logs shows.
//
// Once per cause, naming the request that carried it: clientIP runs on
// every audited action, every webhook and every rate-limited route, and
// one line is a diagnosis while one per request is a flood.
func (s *Server) warnForwardedNotRead(r *http.Request, named int) {
	if s == nil || s.logger == nil || len(s.cfg.TrustedProxyCIDRs) == 0 {
		return
	}
	declared := s.forwardedHeader()
	if !s.peerIsTrusted(r) {
		if !s.anyForwardedHeader(r) {
			return
		}
		s.untrustedPeerOnce.Do(func() {
			s.sharedBucketSaid.Store(true)
			s.logger.Warn("server: forwarded addresses from %s are ignored — it is outside ITERION_AUTH_TRUSTED_PROXY_CIDRS (%s), so the client of a request is the socket address and the auth rate limiter is instance-wide for everything behind that peer. Expected when a client sends its own header; if that address IS the proxy in front, name its range", addrHost(r.RemoteAddr), strings.Join(s.cfg.TrustedProxyCIDRs, ","))
		})
		return
	}
	// READ, and every address it named was one of ours, fewer than the
	// hops declared: the walk never reached a client and the identity is
	// the peer's own. The shape of an in-cluster caller reaching the
	// ingress directly on a chained deployment, of every request of a
	// deployment whose front end does not append after all, and of a
	// front end that named no client (an RFC 7239 for=unknown ahead of
	// its own entry). It is NOT
	// a header that names no address, which the branch below diagnoses:
	// that line quotes a valid address and prescribes the one variable
	// that cannot help here, and it stood the generic line down, so
	// nothing in the process ever named the count.
	if named > 0 {
		s.chainShortOnce.Do(func() {
			s.sharedBucketSaid.Store(true)
			s.logger.Warn("server: the %s from %s names %d address(es) (%q), every one inside ITERION_AUTH_TRUSTED_PROXY_CIDRS (%s) and fewer than the %d hops ITERION_AUTH_TRUSTED_PROXY_HOPS declares — on this path the count exceeds the appending proxies really in front, or the front end named no client, so the client of a request is the peer's own address and every client behind it shares one auth rate-limit bucket and one audit IP. Expected when the caller really is in-cluster or on the LAN and reaches the proxy directly, and when the front end wrote no client address (an RFC 7239 for=unknown); otherwise set the count to the number of APPENDING proxies in front, never higher (docs/environment-variables.md)", declared, addrHost(r.RemoteAddr), named, logSample(strings.Join(r.Header.Values(declared), ", ")), strings.Join(s.cfg.TrustedProxyCIDRs, ","), s.trustedProxyHops())
		})
		return
	}
	// PRESENT is not READ. The declared header being there while this
	// function runs means it named no address at all — the boot
	// validation accepts any field name on purpose, so a non-"Forwarded"
	// RFC 7239 name, a proxy writing a hostname, or a mistyped
	// neighbour like X-Forwarded-Proto all pass it — and the outcome is
	// the instance-wide bucket this function exists to announce, on
	// EVERY request rather than on the rare forged one.
	if values := r.Header.Values(declared); len(values) > 0 {
		s.unusableHeaderOnce.Do(func() {
			s.sharedBucketSaid.Store(true)
			s.logger.Warn("server: the request from %s carries %s but it names no address (%q) — no client address is read, so every request is keyed on the proxy's own address and the auth rate limiter shares one bucket across the whole instance; ITERION_AUTH_FORWARDED_HEADER must name a header whose value is an address list (\"Forwarded\" is read with the RFC 7239 grammar, any other name as a comma-separated list — docs/environment-variables.md)", addrHost(r.RemoteAddr), declared, logSample(values[0]))
		})
		return
	}
	// Every other mechanism the request DOES carry, not the first of
	// them: with two present, naming one would be a guess, and the
	// operator needs the list to pick the header their proxy writes.
	others := []string{}
	for _, candidate := range forwardedHeaderCandidates {
		if !strings.EqualFold(candidate, declared) && len(r.Header.Values(candidate)) > 0 {
			others = append(others, candidate)
		}
	}
	if len(others) == 0 {
		return
	}
	s.undeclaredHeaderOnce.Do(func() {
		s.sharedBucketSaid.Store(true)
		s.logger.Warn("server: the request from %s carries %s but no %s, the header this server reads — only that one is read, so the client of a request is the proxy's own address and the auth rate limiter is instance-wide; set ITERION_AUTH_FORWARDED_HEADER to the header the proxy in front writes on every request (docs/environment-variables.md)", addrHost(r.RemoteAddr), strings.Join(others, " and "), declared)
	})
}

// warnUndeclaredHop says ONCE when the entry just beyond the hop bound
// is itself inside the trusted ranges — the shape of an APPENDING proxy
// of the operator's own that the declared count does not reach.
//
// The bound exists because nothing in a chain's CONTENT tells a proxy's
// entry from a client's: the two shapes are identical, so the count is
// the only discriminator and the default is the one hop the chart ships.
// A deployment with a second appending hop (a service mesh sidecar, an
// internal L7 proxy, a CDN edge whose ranges the operator trusts) then
// keys every client behind that hop on the hop's own address, and
// nothing would say so.
//
// Ambiguous on purpose, and the line says both causes: a client that
// writes an in-range address of its own produces the same shape, which
// is precisely why it is not believed.
func (s *Server) warnUndeclaredHop(r *http.Request, beyond string, hops int) {
	if s == nil || s.logger == nil {
		return
	}
	ip := canonicalIP(beyond)
	if ip == "" || !s.ipIsTrusted(ip) {
		return
	}
	s.undeclaredHopOnce.Do(func() {
		s.sharedBucketSaid.Store(true)
		s.logger.Warn("server: the forwarded chain from %s carries %s past the %d hop(s) ITERION_AUTH_TRUSTED_PROXY_HOPS declares, and that address is inside ITERION_AUTH_TRUSTED_PROXY_CIDRS (%s) — either another appending proxy of yours sits in front and is not declared, in which case every client behind it shares one rate-limit bucket and one audit IP, or a client is writing in-range addresses of its own, which is why they are not believed. Raise the count to the number of APPENDING proxies really in front, never higher (docs/environment-variables.md)", addrHost(r.RemoteAddr), ip, hops, strings.Join(s.cfg.TrustedProxyCIDRs, ","))
	})
}

// warnIdentityIsOurs says ONCE when the identity a request finally
// resolves to is itself inside the trusted ranges. It keys on the
// ANSWER, not on the shape of the walk that found it, so every way of
// keying an instance on its own infrastructure ends here: a chain whose
// hop bound stops on a proxy of ours, a trusted peer that forwards
// nothing, and the shape the chart's own default lands in — with the
// Kubernetes-default `externalTrafficPolicy: Cluster` the Service SNATs
// the client address to the node, so an ingress controller that
// REPLACES the chain writes the node's address as the client, and one
// that appends leaves what arrived one entry beyond the declared
// bound. The whole internet then shares one bucket per node and one
// audit IP.
//
// Ambiguous on purpose, and the line names both causes: a LAN client of
// a self-hosted instance and an in-cluster pod the NetworkPolicy admits
// are inside the ranges too, and for them the identity is the right one.
// Nothing in a request tells the two apart — the chain, the peer and the
// answer are identical — so the diagnosis is a question put to the
// operator, once, with the variable and the remedy in it.
//
// It is the net UNDER the specific causes, not a second line beside
// them: the four in warnForwardedNotRead and warnUndeclaredHop report
// the same outcome with a cause and a remedy attached, and each of them
// runs before this one on the request that shows it. The stand-down and
// the announcement are one atomic step (CompareAndSwap inside the Once),
// and what that step guarantees is two things: this line is written at
// most once per process, and once a specific cause has stored the flag
// no request that reaches the swap after it writes this line. It does
// not order the log: two FIRST requests racing — one carrying a specific
// cause, one healthy and keyed on our own address — may each get their
// line, and the generic one may land after the specific one, since each
// cause's Store and its Warn are two steps. At most once each is the
// whole guarantee.
func (s *Server) warnIdentityIsOurs(r *http.Request, identity string) {
	if s == nil || s.logger == nil || s.sharedBucketSaid.Load() || !s.ipIsTrusted(identity) {
		return
	}
	s.identityIsOursOnce.Do(func() {
		if !s.sharedBucketSaid.CompareAndSwap(false, true) {
			return
		}
		s.logger.Warn("server: the client of a request from %s resolves to %s, which is inside ITERION_AUTH_TRUSTED_PROXY_CIDRS (%s) — this instance is keyed on an address of its own infrastructure, so every client behind that address shares one auth rate-limit bucket and one audit IP. Expected when the caller really is in-cluster or on the LAN; otherwise the client's address never reaches this server — a Service with externalTrafficPolicy: Cluster replaces it with the node's, and the client is then either written as that node or left beyond the %d hop(s) ITERION_AUTH_TRUSTED_PROXY_HOPS declares (docs/cloud-deployment.md names the shapes that carry it through)", addrHost(r.RemoteAddr), identity, strings.Join(s.cfg.TrustedProxyCIDRs, ","), s.trustedProxyHops())
	})
}

// warnHopsUnreachable says ONCE when the walk ran out of our own entries
// before the declared bound could arbitrate: the chain cannot supply
// trustedProxyHops hops, so the identity is read from wherever the first
// entry outside the ranges happens to sit — a client's own forgery as
// readily as a proxy's address.
//
// The test is hops-1 and not hops because of the designed shape's
// arithmetic: N appending proxies write N entries, of which the leftmost
// is the CLIENT's own address, so a correctly declared chain whose client
// sits outside the ranges crosses exactly N-1 entries of ours before the
// answer. Warning at `ours < hops` would fire on every legitimate
// chained front end — the very configuration docs/cloud-deployment.md
// asks for — and a line that fires on the healthy shape is one operators
// learn to skip.
//
// What stays invisible is documented rather than guessed: an
// over-declaration of exactly one hop whose client is itself in-range
// crosses hops-1 entries too. Nothing in a chain's CONTENT tells a
// proxy's entry from a client's, which is the premise of the bound
// itself.
func (s *Server) warnHopsUnreachable(r *http.Request, ours, hops int) {
	if s == nil || s.logger == nil || ours >= hops-1 {
		return
	}
	s.hopsUnreachableOnce.Do(func() {
		s.logger.Warn("server: the forwarded chain from %s carries %d entries inside ITERION_AUTH_TRUSTED_PROXY_CIDRS (%s) where the %d hop(s) ITERION_AUTH_TRUSTED_PROXY_HOPS declares need at least %d — the bound never arbitrates, so the client address is read from the first entry outside the ranges, which is in the client's own half of the chain and moves with what it writes. Set the count to the number of APPENDING proxies really in front, never higher (docs/environment-variables.md)", addrHost(r.RemoteAddr), ours, strings.Join(s.cfg.TrustedProxyCIDRs, ","), hops, hops-1)
	})
}

// logSample bounds and escapes a client-controlled value on its way to
// a log line: the caller's %q turns the CRLF a header can carry into an
// escape, and the cut keeps a megabyte of header out of the line. A
// header value is diagnosed by its shape — a hostname, a scheme, an
// RFC 7239 element — which the first bytes carry.
func logSample(v string) string {
	const max = 64
	if len(v) > max {
		return v[:max] + "..."
	}
	return v
}

// anyForwardedHeader answers whether the request carries a forwarded
// mechanism at all — the premise of the untrusted-peer diagnosis, which
// must stay silent for the requests that name no client.
//
// The DECLARED header counts first, before the three candidates: it is
// the mechanism this server actually reads, and a deployment behind a
// CDN declares one that is on no candidate list (CF-Connecting-IP,
// True-Client-IP). Testing the candidates alone leaves the diagnosis
// silent for exactly the operator who had to declare a header — the same
// misconfiguration, the same instance-wide bucket, no line in the log.
func (s *Server) anyForwardedHeader(r *http.Request) bool {
	if len(r.Header.Values(s.forwardedHeader())) > 0 {
		return true
	}
	for _, candidate := range forwardedHeaderCandidates {
		if len(r.Header.Values(candidate)) > 0 {
			return true
		}
	}
	return false
}

func (s *Server) peerIsTrusted(r *http.Request) bool {
	if s == nil {
		return false
	}
	return s.ipIsTrusted(addrHost(r.RemoteAddr))
}

// ipIsTrusted answers whether an address is one of the operator's
// proxies — the immediate peer, or a hop inside a forwarded chain.
func (s *Server) ipIsTrusted(addr string) bool {
	if s == nil {
		return false
	}
	ip := net.ParseIP(addr)
	if ip == nil {
		return false
	}
	for _, network := range s.trustedProxyNets() {
		if network.Contains(ip) {
			return true
		}
	}
	return false
}

// trustedProxyNets parses the configured ranges once per server: they are
// immutable for the process's life, and clientIP runs on every audited
// action, every webhook and every rate-limited route.
//
// An entry that parses nowhere is skipped — it trusts no peer — and said
// once in the log. pkg/config refuses it at boot, naming it; a Config
// assembled by another embedder reaches here instead, and a range that
// matches nothing must not pass for a configured one in silence.
func (s *Server) trustedProxyNets() []*net.IPNet {
	s.trustedProxyOnce.Do(func() {
		for _, cidr := range s.cfg.TrustedProxyCIDRs {
			_, network, err := net.ParseCIDR(cidr)
			if err != nil {
				if s.logger != nil {
					s.logger.Warn("server: ITERION_AUTH_TRUSTED_PROXY_CIDRS entry %q is not a CIDR range (%v) — it is ignored, and no peer is trusted by it", cidr, err)
				}
				continue
			}
			s.trustedProxyParsed = append(s.trustedProxyParsed, network)
		}
	})
	return s.trustedProxyParsed
}
