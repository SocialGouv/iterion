package server

import (
	"context"
	"errors"
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
	om, err := s.authStore().GetOrgMembership(r.Context(), id.UserID, orgID)
	switch {
	case err == nil && om.Role.AtLeast(want):
		return true
	case err != nil && !errors.Is(err, identity.ErrNotFound):
		httpError(w, http.StatusInternalServerError, "resolve your org membership: %v", err)
		return false
	}
	have := "none"
	if err == nil && om.Role != "" {
		have = string(om.Role)
	}
	httpError(w, http.StatusForbidden,
		"a team invitation cannot carry an org role above its creator's: an invitation at role %s is mirrored up to an org %s membership when it is accepted, and your own org role in this team's organization is %s",
		role, want, have)
	return false
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
