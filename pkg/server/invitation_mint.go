package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/identity"
)

// Invitation deliveries a mint answers with (invitationMinted.Delivery).
const (
	// invitationDeliveryLink: the token is in the response, for the minter to
	// hand over, and is emailed too when the instance has a mailer.
	invitationDeliveryLink = "link"
	// invitationDeliveryEmail: the token went to the invited address only;
	// the response carries none.
	invitationDeliveryEmail = "email"
)

// invitationEmailFailedCode is the error_code of a 502 that took back an
// invitation whose email the relay refused.
const invitationEmailFailedCode = "invitation_email_failed"

// invitationEmailFailedStandsCode is that 502 when taking the invitation back
// failed too: the row stands, listed by GET /api/teams/{id}/invitations, and
// is revoked by hand.
const invitationEmailFailedStandsCode = "invitation_email_failed_invitation_stands"

// invitationMinted is what both invitation routes answer once an invitation
// has landed and survived its confirmations.
type invitationMinted struct {
	ID string `json:"id"`
	// Token is the invitation's plaintext token, present only when Delivery
	// is "link". It is never recoverable afterwards.
	Token     string    `json:"token,omitempty"`
	Delivery  string    `json:"delivery"`
	Email     string    `json:"email"`
	Role      string    `json:"role"`
	OrgRole   string    `json:"org_role,omitempty"`
	TeamID    string    `json:"team_id"`
	ExpiresAt time.Time `json:"expires_at"`
}

// invitationMint is the mint an invitation route asks the derivation
// chokepoint for, by the kind of credential presented: a personal access
// token mints an invitation delivered by email only, whose token never comes
// back to it (auth.MintEmailedInvitation); any other credential the team
// token the studio shows as a link (auth.MintTeamToken).
func invitationMint(id auth.Identity) auth.Mint {
	if id.Credential == auth.CredentialPAT {
		return auth.MintEmailedInvitation
	}
	return auth.MintTeamToken
}

// invitationMintCall is one invitation a route mints, once its caps passed:
// the request, the org the team belongs to ("" for a team with no parent
// org), whether the team cap measured the minter's grant (requireRankToInvite),
// the names its email shows, and the route's own audit line.
type invitationMintCall struct {
	req         auth.InvitationRequest
	orgID       string
	grantCapped bool
	mail        auth.InvitationMail
	audit       func(inv identity.Invitation)
	emailTask   string
}

// mintInvitation mints the invitation c names by the mint the credential
// presented may make (invitationMint), confirms both caps once the row has
// landed (confirmOrgStandingAfterMint, confirmTeamGrantAfterMint) and only
// then lets the token leave: in the response — and by a detached email when
// the instance has a mailer — for a studio session; by a synchronous email
// alone for a personal access token, whose invitation is taken back when the
// relay refuses it, since it would reach nobody. Both deliveries run behind
// the one pair of confirmations below. It answers the request itself.
func (s *Server) mintInvitation(w http.ResponseWriter, r *http.Request, id auth.Identity, c invitationMintCall) {
	var (
		inv     identity.Invitation
		deliver func()
	)
	if invitationMint(id) == auth.MintEmailedInvitation {
		sealed, err := s.authSvc.CreateEmailedInvitation(r.Context(), c.req)
		if err != nil {
			httpError(w, mapAuthErrorStatus(err), "%s", err.Error())
			return
		}
		inv = sealed.Invitation
		deliver = func() { s.deliverSealedInvitation(w, r, sealed, c.mail, c.orgID) }
	} else {
		tok, minted, err := s.authSvc.CreateInvitation(r.Context(), c.req)
		if err != nil {
			httpError(w, mapAuthErrorStatus(err), "%s", err.Error())
			return
		}
		inv = minted
		deliver = func() { s.deliverInvitationLink(w, inv, tok, c) }
	}
	c.audit(inv)
	// The caps were read BEFORE the row landed, and the sweep that follows a
	// write lowering the minter's own standing only sees the rows its listing
	// reaches: the confirmations are what cover a mint that straddles such a
	// write. They run before the token leaves — by email or in the response —
	// so an invitation that did not survive them reaches nobody.
	if !s.confirmOrgStandingAfterMint(w, r, id, c.orgID, inv) {
		return
	}
	if !s.confirmTeamGrantAfterMint(w, r, id, c.orgID, c.grantCapped, inv) {
		return
	}
	if !s.confirmMintingTokenAfterMint(w, r, inv) {
		return
	}
	deliver()
}

// confirmMintingTokenAfterMint reads again, once the row has landed, the
// standing of the personal access token that minted inv (nothing to read for
// a session's mint). The request was authenticated before the row landed,
// and the revoke of that token sweeps the pending rows it lists only AFTER
// the revoke is written: a row inserted behind that listing would be emailed
// and then refused at accept, with nobody told. Read here, the revoke is
// either seen — the row is taken back before any email leaves, and the
// minter hears that its token no longer stands — or it lands later, and its
// sweep lists this row and names it.
func (s *Server) confirmMintingTokenAfterMint(w http.ResponseWriter, r *http.Request, inv identity.Invitation) bool {
	err := s.authSvc.InvitationCredentialStands(r.Context(), inv)
	if err == nil {
		return true
	}
	fate, _ := s.revokeUndeliveredInvitation(r, inv, "minting_pat_gone", "")
	if errors.Is(err, auth.ErrInvitationCredentialGone) {
		httpError(w, http.StatusUnauthorized,
			"this personal access token was revoked, or stopped standing, while the invitation for %s was being minted: nothing was sent, and %s",
			inv.Email, fate)
		return false
	}
	httpError(w, http.StatusInternalServerError,
		"confirming the personal access token that minted the invitation for %s failed (%v): nothing was sent, and %s", inv.Email, err, fate)
	return false
}

// deliverInvitationLink answers a studio session's mint with the token, and
// emails it too when a real mailer is wired (detached — a relay blip must not
// fail the create; CLI/SDK flows and operators without SMTP copy the token).
func (s *Server) deliverInvitationLink(w http.ResponseWriter, inv identity.Invitation, tok string, c invitationMintCall) {
	if s.authSvc.EmailEnabled() {
		msg := s.authSvc.InvitationMessage(inv, tok, c.mail)
		s.goSafe(c.emailTask, func() {
			bg, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			if err := s.authSvc.Mailer().Send(bg, msg); err != nil && s.logger != nil {
				s.logger.Warn("auth: invitation email to %s: %v", msg.To, err)
			}
		})
	}
	writeJSON(w, invitationMintedOf(inv, tok, invitationDeliveryLink, c.orgID))
}

// deliverSealedInvitation sends a personal access token's invitation by
// email, synchronously — the token stays sealed in pkg/auth — and answers
// with the delivery instead of the token. An email the relay refuses takes
// the invitation back.
func (s *Server) deliverSealedInvitation(w http.ResponseWriter, r *http.Request, sealed auth.SealedInvitation, d auth.InvitationMail, orgID string) {
	inv := sealed.Invitation
	dctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 30*time.Second)
	defer cancel()
	if err := s.authSvc.DeliverInvitation(dctx, sealed, d); err != nil {
		fate, stands := s.revokeUndeliveredInvitation(r, inv, "email_delivery_failed", ": mint it again once the mail relay answers")
		// A 5xx body keeps its error_code alone (the rest reaches the log
		// under the request id): the code is what tells the caller whether
		// the invitation went with its email or still stands, to revoke by
		// hand.
		code := invitationEmailFailedCode
		if stands {
			code = invitationEmailFailedStandsCode
		}
		s.httpErrorCode(w, r, http.StatusBadGateway, code,
			"the invitation for %s could not be emailed: %v — an invitation minted from a personal access token reaches its address by email alone, so %s",
			inv.Email, err, fate)
		return
	}
	writeJSON(w, invitationMintedOf(inv, "", invitationDeliveryEmail, orgID))
}

// revokeUndeliveredInvitation takes back an invitation that will not be
// delivered — its email refused by the relay, or its minting token gone
// before any email left — on a context detached from the request, and
// answers its fate in words for the refusal, and whether the row still
// stands. reason is the audit line's; next follows the fate of a row taken
// back, as the advice for what to do then.
func (s *Server) revokeUndeliveredInvitation(r *http.Request, inv identity.Invitation, reason, next string) (string, bool) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), grantCompensationTimeout)
	defer cancel()
	err := s.authStore().DeleteInvitation(ctx, inv.ID)
	switch {
	case err == nil:
		s.auditTenant(r, inv.TeamID, "invitation.deleted", "invitation", inv.ID,
			map[string]any{"email": inv.Email, "reason": reason})
		return "it was revoked" + next, false
	case errors.Is(err, identity.ErrNotFound):
		return "it is already revoked", false
	default:
		if s.logger != nil {
			s.logger.Error("team %s: revoking undelivered invitation %s (%s) failed: %v", inv.TeamID, inv.ID, reason, err)
		}
		return fmt.Sprintf("revoking it failed too (%v): invitation %s stands undelivered — revoke it by hand (DELETE /api/teams/%s/invitations/%s)", err, inv.ID, inv.TeamID, inv.ID), true
	}
}

// invitationMintedOf answers a minted invitation. org_role is the org
// membership accepting it writes, so a team with no parent org (orgID "")
// names none: nothing is written there.
func invitationMintedOf(inv identity.Invitation, token, delivery, orgID string) invitationMinted {
	return invitationMinted{
		ID:        inv.ID,
		Token:     token,
		Delivery:  delivery,
		Email:     inv.Email,
		Role:      string(inv.Role),
		OrgRole:   string(invitationOrgRoleIn(inv, orgID)),
		TeamID:    inv.TeamID,
		ExpiresAt: inv.ExpiresAt,
	}
}

// inviteMailNames reads the display names an invitation email shows for
// teamID and its parent org. A name that cannot be read shows the id: the
// email is a courtesy around the token, never a reason to refuse the mint.
func (s *Server) inviteMailNames(ctx context.Context, t identity.Team, inviter string) auth.InvitationMail {
	d := auth.InvitationMail{TeamName: t.Name, InvitedBy: inviter}
	if d.TeamName == "" {
		d.TeamName = t.ID
	}
	if t.OrgID != "" {
		d.OrgName = t.OrgID
		if org, err := s.authStore().GetOrg(ctx, t.OrgID); err == nil && org.Name != "" {
			d.OrgName = org.Name
		}
	}
	return d
}

// invitationOrgRoleIn is the org membership accepting inv writes in a team
// whose parent org is orgID: none where there is no org to write it in.
func invitationOrgRoleIn(inv identity.Invitation, orgID string) identity.OrgRole {
	if orgID == "" {
		return ""
	}
	return inv.OrgRoleOnAccept()
}

// invitationAuditMeta is what the audit line of a minted invitation names:
// its address, its team role, the org role accepting writes, and — for one a
// personal access token minted — that token, which is what someone answering
// a leak looks for.
func invitationAuditMeta(inv identity.Invitation, orgID string) map[string]any {
	meta := map[string]any{"email": inv.Email, "role": string(inv.Role), "org_role": string(invitationOrgRoleIn(inv, orgID))}
	if inv.MintedWith != "" {
		meta["minted_with"] = inv.MintedWith
	}
	return meta
}
