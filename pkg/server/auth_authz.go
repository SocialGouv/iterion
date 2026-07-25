package server

import (
	"context"
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
