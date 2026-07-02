package server

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/auth/orgsso"
	"github.com/SocialGouv/iterion/pkg/forge"
	forgegithub "github.com/SocialGouv/iterion/pkg/forge/github"
	"github.com/SocialGouv/iterion/pkg/identity"
	"github.com/SocialGouv/iterion/pkg/store"
)

// registerOrgSSORoutes wires the per-tenant SSO provider CRUD. An org admin
// self-serves their org's Keycloak (and, from Phase 2, a GitHub team
// allow-list), mirroring the forge OAuth-app routes: tenant-scoped, sealed
// secrets, admin-gated, audited.
func (s *Server) registerOrgSSORoutes() {
	s.mux.Handle("GET /api/orgs/{id}/sso/providers", s.requireAuth(http.HandlerFunc(s.handleListOrgSSOProviders)))
	s.mux.Handle("POST /api/orgs/{id}/sso/providers", s.requireAuth(http.HandlerFunc(s.handleCreateOrgSSOProvider)))
	s.mux.Handle("PATCH /api/orgs/{id}/sso/providers/{provider_id}", s.requireAuth(http.HandlerFunc(s.handleUpdateOrgSSOProvider)))
	s.mux.Handle("DELETE /api/orgs/{id}/sso/providers/{provider_id}", s.requireAuth(http.HandlerFunc(s.handleDeleteOrgSSOProvider)))
	s.mux.Handle("POST /api/orgs/{id}/sso/providers/{provider_id}/test", s.requireAuth(http.HandlerFunc(s.handleTestOrgSSOProvider)))
}

// orgSSOProviderReq is the create/update body. client_secret is write-only; an
// empty value on PATCH means "don't rotate the stored secret".
type orgSSOProviderReq struct {
	Kind            string   `json:"kind"`
	DisplayName     string   `json:"display_name"`
	Enabled         bool     `json:"enabled"`
	IssuerURL       string   `json:"issuer_url"`
	ClientID        string   `json:"client_id"`
	ClientSecret    string   `json:"client_secret"`
	Scopes          []string `json:"scopes"`
	DefaultRole     string   `json:"default_role"`
	AutoLinkOnEmail bool     `json:"auto_link_on_email"`
	// GitHub fields (wired in Phase 2).
	AutoProvision bool                     `json:"auto_provision"`
	Grants        []orgsso.GitHubTeamGrant `json:"grants"`
}

// orgSSOProviderView is the response shape: the stored row (SealedSecret is
// json:"-") plus the redirect URI an admin registers at their IdP.
type orgSSOProviderView struct {
	orgsso.OrgSSOProvider
	RedirectURI string `json:"redirect_uri,omitempty"`
}

func (s *Server) viewOrgSSOProvider(p orgsso.OrgSSOProvider) orgSSOProviderView {
	p.SealedSecret = nil // defensive — also json:"-"
	v := orgSSOProviderView{OrgSSOProvider: p}
	if p.Kind == orgsso.KindOIDC {
		v.RedirectURI = s.oidcRedirectURI(p.OIDCSlug())
	}
	return v
}

func (s *Server) handleListOrgSSOProviders(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	orgID := r.PathValue("id")
	if !s.canViewOrg(r.Context(), id, orgID) {
		httpError(w, http.StatusForbidden, "not a member")
		return
	}
	teamID := s.firstTeamInOrg(r.Context(), orgID)
	if teamID == "" {
		httpError(w, http.StatusNotFound, "org has no team")
		return
	}
	rows, err := s.orgSSO.ListByTenant(store.WithTenant(r.Context(), teamID), teamID)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "list sso providers: %v", err)
		return
	}
	views := make([]orgSSOProviderView, 0, len(rows))
	for _, p := range rows {
		views = append(views, s.viewOrgSSOProvider(p))
	}
	writeJSON(w, map[string]any{"providers": views})
}

func (s *Server) handleCreateOrgSSOProvider(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	orgID := r.PathValue("id")
	if !s.canManageOrg(r.Context(), id, orgID) {
		httpError(w, http.StatusForbidden, "admin or owner required")
		return
	}
	teamID := s.firstTeamInOrg(r.Context(), orgID)
	if teamID == "" {
		httpError(w, http.StatusNotFound, "org has no team")
		return
	}
	var req orgSSOProviderReq
	if !decodeJSON(w, r, &req) {
		return
	}
	now := time.Now().UTC()
	row := orgsso.OrgSSOProvider{
		ID:        uuid.NewString(),
		TenantID:  teamID,
		Kind:      orgsso.Kind(strings.TrimSpace(req.Kind)),
		Enabled:   req.Enabled,
		CreatedBy: id.UserID,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := s.applyOrgSSOReq(r.Context(), &row, req, true, id.IsSuperAdmin); err != nil {
		httpError(w, http.StatusBadRequest, "%v", err)
		return
	}
	if err := s.orgSSO.Create(store.WithTenant(r.Context(), teamID), row); err != nil {
		s.writeOrgSSOError(w, err)
		return
	}
	s.auditOrg(r, orgID, "org_sso.created", "org_sso_provider", row.ID, map[string]any{
		"kind": string(row.Kind), "enabled": row.Enabled,
	})
	writeJSON(w, s.viewOrgSSOProvider(row))
}

func (s *Server) handleUpdateOrgSSOProvider(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	orgID := r.PathValue("id")
	if !s.canManageOrg(r.Context(), id, orgID) {
		httpError(w, http.StatusForbidden, "admin or owner required")
		return
	}
	teamID := s.firstTeamInOrg(r.Context(), orgID)
	if teamID == "" {
		httpError(w, http.StatusNotFound, "org has no team")
		return
	}
	providerID := r.PathValue("provider_id")
	ctx := store.WithTenant(r.Context(), teamID)
	row, ok := s.loadTenantOrgSSORow(ctx, w, teamID, providerID)
	if !ok {
		return
	}
	var req orgSSOProviderReq
	if !decodeJSON(w, r, &req) {
		return
	}
	// Kind is immutable; ignore any kind in the body.
	req.Kind = string(row.Kind)
	row.Enabled = req.Enabled
	row.UpdatedAt = time.Now().UTC()
	if err := s.applyOrgSSOReq(r.Context(), &row, req, false, id.IsSuperAdmin); err != nil {
		httpError(w, http.StatusBadRequest, "%v", err)
		return
	}
	if err := s.orgSSO.Update(ctx, row); err != nil {
		s.writeOrgSSOError(w, err)
		return
	}
	s.auditOrg(r, orgID, "org_sso.updated", "org_sso_provider", row.ID, map[string]any{
		"kind": string(row.Kind), "enabled": row.Enabled,
	})
	writeJSON(w, s.viewOrgSSOProvider(row))
}

func (s *Server) handleDeleteOrgSSOProvider(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	orgID := r.PathValue("id")
	if !s.canManageOrg(r.Context(), id, orgID) {
		httpError(w, http.StatusForbidden, "admin or owner required")
		return
	}
	teamID := s.firstTeamInOrg(r.Context(), orgID)
	if teamID == "" {
		httpError(w, http.StatusNotFound, "org has no team")
		return
	}
	providerID := r.PathValue("provider_id")
	ctx := store.WithTenant(r.Context(), teamID)
	row, ok := s.loadTenantOrgSSORow(ctx, w, teamID, providerID)
	if !ok {
		return
	}
	if err := s.orgSSO.Delete(ctx, providerID); err != nil {
		httpError(w, http.StatusInternalServerError, "delete provider: %v", err)
		return
	}
	s.auditOrg(r, orgID, "org_sso.deleted", "org_sso_provider", providerID, map[string]any{"kind": string(row.Kind)})
	w.WriteHeader(http.StatusNoContent)
}

// handleTestOrgSSOProvider runs a discovery smoke test against an OIDC row's
// issuer (through the SSRF guard) so an admin can validate config before
// enabling it. Never returns the client secret or the upstream body.
func (s *Server) handleTestOrgSSOProvider(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	orgID := r.PathValue("id")
	if !s.canManageOrg(r.Context(), id, orgID) {
		httpError(w, http.StatusForbidden, "admin or owner required")
		return
	}
	teamID := s.firstTeamInOrg(r.Context(), orgID)
	if teamID == "" {
		httpError(w, http.StatusNotFound, "org has no team")
		return
	}
	providerID := r.PathValue("provider_id")
	row, ok := s.loadTenantOrgSSORow(store.WithTenant(r.Context(), teamID), w, teamID, providerID)
	if !ok {
		return
	}
	if row.Kind != orgsso.KindOIDC {
		httpError(w, http.StatusBadRequest, "test is only supported for oidc providers")
		return
	}
	conn, err := s.buildOrgOIDCConnector(row)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "open secret")
		return
	}
	// AuthorizeURL triggers discovery; a build success means the issuer is
	// reachable, advertises matching endpoints, and (strict) is https.
	if _, derr := conn.AuthorizeURL(r.Context(), s.oidcRedirectURI(row.OIDCSlug()), "test-state", "test-verifier"); derr != nil {
		writeJSON(w, map[string]any{"ok": false, "error": derr.Error()})
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// applyOrgSSOReq validates a request against the row's kind and applies the
// mutable fields. On create, the secret is required for OIDC; on update an
// empty client_secret preserves the stored one. The role ceiling is enforced
// by orgsso.Validate (which rejects owner) — anyone reaching here passed
// canManageTeam (admin/owner), whose SSO grant ceiling is exactly "admin".
//
// bypassControlProof skips the GitHub proof-of-control check (forge connection
// proving the configurer admins the GitHub org). It is set for platform
// super-admins: the proof gate (H-8) exists to stop a NON-super org admin from
// allow-listing a GitHub org they don't control; a super-admin is the trusted
// platform operator, and login still gates on each user's real GitHub-team
// membership (read from their own login token, not the forge app's).
func (s *Server) applyOrgSSOReq(ctx context.Context, row *orgsso.OrgSSOProvider, req orgSSOProviderReq, create, bypassControlProof bool) error {
	row.DisplayName = req.DisplayName
	switch orgsso.Kind(req.Kind) {
	case orgsso.KindOIDC:
		row.Kind = orgsso.KindOIDC
		row.IssuerURL = req.IssuerURL
		row.ClientID = req.ClientID
		row.Scopes = req.Scopes
		row.DefaultRole = identity.Role(strings.TrimSpace(req.DefaultRole))
		row.AutoLinkOnEmail = req.AutoLinkOnEmail
		if req.ClientSecret != "" {
			sealed, err := orgsso.SealClientSecret(s.sealer, row.ID, req.ClientSecret)
			if err != nil {
				return err
			}
			row.SealedSecret = sealed
		} else if create {
			return orgsso.ErrInvalid // client_secret required on create
		}
		// (update with empty secret keeps row.SealedSecret unchanged)
	case orgsso.KindGitHub:
		row.Kind = orgsso.KindGitHub
		row.AutoProvision = req.AutoProvision
		row.Grants = req.Grants
		// Proof-of-control: a grant is honoured at login only once the team has
		// proven it controls (admins) the GitHub org via a verified forge
		// connection. Mark each grant; unverified grants are stored but inert
		// (FlattenGitHubTeamKeys / RoleForGroups skip them).
		for i := range row.Grants {
			row.Grants[i].Verified = bypassControlProof || s.teamControlsGitHubOrg(ctx, row.TenantID, row.Grants[i].GitHubOrg)
		}
	default:
		return orgsso.ErrInvalid
	}
	row.Normalize()
	return row.Validate()
}

// loadTenantOrgSSORow fetches a provider and asserts it belongs to teamID,
// writing a 404 (and returning ok=false) otherwise. Shared by the
// update/delete/test handlers.
func (s *Server) loadTenantOrgSSORow(ctx context.Context, w http.ResponseWriter, teamID, providerID string) (orgsso.OrgSSOProvider, bool) {
	row, err := s.orgSSO.Get(ctx, providerID)
	if err != nil || row.TenantID != teamID {
		httpError(w, http.StatusNotFound, "provider not found")
		return orgsso.OrgSSOProvider{}, false
	}
	return row, true
}

// teamControlsGitHubOrg reports whether the team has proven control of a GitHub
// org — an active forge GitHub connection whose token is an org admin. This is
// the proof gate for GitHub SSO grants (an admin may only allow-list teams of a
// GitHub org they actually administer, closing H-8). Returns false when forge
// isn't wired or no connection proves control.
func (s *Server) teamControlsGitHubOrg(ctx context.Context, teamID, org string) bool {
	if s.forgeConnections == nil || s.sealer == nil || org == "" {
		return false
	}
	conns, err := s.forgeConnections.ListByTenant(store.WithTenant(ctx, teamID), teamID)
	if err != nil {
		return false
	}
	for _, conn := range conns {
		if conn.Provider != forge.ProviderGitHub || conn.Status != forge.StatusActive {
			continue
		}
		token, err := forge.AdminTokenFor(s.sealer, conn)
		if err != nil {
			continue
		}
		role, active, err := forgegithub.New(s.forgeHTTPClient(), conn.BaseURL(), token).OrgMembershipRole(ctx, org)
		if err == nil && active && role == "admin" {
			return true
		}
	}
	return false
}

func (s *Server) writeOrgSSOError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, orgsso.ErrExists):
		httpError(w, http.StatusConflict, "a github provider already exists for this org")
	case errors.Is(err, orgsso.ErrInvalid) || errors.Is(err, orgsso.ErrOwnerNotGrant):
		httpError(w, http.StatusBadRequest, "%v", err)
	default:
		httpError(w, http.StatusInternalServerError, "%v", err)
	}
}
